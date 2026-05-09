# Code-disk attach — design + status

Status:
- **Phase 1 shipped** (cold-boot CLI + REST `--code-disk` flag).
- **Phase 2 shipped** as plumbing — `RestoreOptions.AdditionalDrives`
  end-to-end through vmm + vmmserver + worker + container.Run, with
  a host-side post-restore mount via `toolbox.Exec`. Smoke binary
  (`tests/manual-smoke/cmd/codedisksnapshot`) exercises the path
  end-to-end; it surfaces a known toolbox-agent /exec interaction
  with snapshot-restore (the agent closes the conn before the EXIT
  frame on the first request after restore — Health works fine, so
  the vsock channel is up). Bug is logged at the bottom of this doc.
- **Phase 3 shipped** as wire-shape — `LeaseSandboxRequest.CodeDisks`
  threads through sandboxd into `pool.LeaseSpec.CodeDisks`, and the
  Python / Go / JS SDKs accept the field. Runtime application is
  still gated on a pool-side restore-on-demand mode (see "Phase 3
  next steps" below).

## The shape

Today, every gocracker microVM is built end-to-end from an OCI image (or
Dockerfile/repo): pull the layers, materialize an overlay, build the
ext4 root, boot. When you have one base image and only the *application
code* changes between launches, you still pay the rootfs build cost —
even though the OS bits are bit-identical.

The code-disk-attach shape splits that:

```
host                                    guest
─────────────────────────────────────   ─────────────────────────────
sudo gocracker run \                    [init] kernel cmdline:
  --image alpine:3.20 \                   gc.code_disk=/dev/vdb:/app:ext4
  --kernel ./vmlinux \                  [init] mountFS /dev/vdb /app ext4
  --code-disk app.ext4:/app \           [init] switch_root, exec /app/main
  --cmd '/app/main'                     [guest running app from /app]
```

Build the rootfs once, attach a tiny ext4 with the user's code per
launch. Four different code disks over the same Alpine template launch
in roughly the same time as a single launch — only the code disk
differs.

## Phase 1 — what shipped

A minimal cold-boot variant of the shape, usable today via the CLI and
the REST API.

### Surface

```
--code-disk HOST_PATH:GUEST_MOUNT[:FS[:ro|rw]]   # repeatable
```

- `HOST_PATH`: absolute path to a disk image on the host.
- `GUEST_MOUNT`: absolute mountpoint inside the guest.
- `FS`: filesystem type (default `ext4`).
- `ro` / `rw`: access mode (default `rw`).

Each `--code-disk` becomes one extra virtio-blk drive plus one segment
of the `gc.code_disk=` kernel cmdline param. Drives are appended after
any explicit `--drive` entries; the first code disk is `/dev/vdb`, the
second `/dev/vdc`, etc. The guest's init parses `gc.code_disk=` after
`switch_root` and mounts each entry in order.

### Pieces

| Layer | Change | File |
| --- | --- | --- |
| Type | New `container.CodeDisk{HostPath, Mount, FSType, ReadOnly}` | [pkg/container/container.go](../../pkg/container/container.go) |
| RunOptions | New `CodeDisks []CodeDisk` field | [pkg/container/container.go](../../pkg/container/container.go) |
| Cmdline builder | Emits `gc.code_disk=<segs>` from `opts.CodeDisks` | [pkg/container/container.go buildCmdlineWithPlan](../../pkg/container/container.go) |
| Drive builder | Appends one `vmm.DriveConfig` per code disk after explicit `Drives` | [pkg/container/container.go runtimeDrives](../../pkg/container/container.go) |
| Guest init | New `mountExtraCodeDisks(cmdline)` invoked after `switch_root` | [internal/guest/init.go](../../internal/guest/init.go) |
| CLI | New `--code-disk` flag, parsed via `mustParseCodeDisks` | [cmd/gocracker/main.go](../../cmd/gocracker/main.go) |
| REST API | Already exposed via `RunRequest.Drives[]`; callers can reproduce the shape by passing drives + explicitly setting the kernel cmdline | [internal/api/api.go](../../internal/api/api.go) |
| Smoke | `tests/manual-smoke/code-disk/run.sh` — builds two disks and runs both over the same Alpine template | [tests/manual-smoke/code-disk/](../../tests/manual-smoke/code-disk/) |

### What Phase 1 does *not* cover

- **Snapshot/restore + per-launch code disk.** This is the step that
  makes 4 versions launch *quasi-instantly*. Today, restoring from a
  snapshot uses the snapshot's frozen drive list — there is no
  mechanism to attach a fresh, snapshot-absent drive at restore time.
  Phase 2 work below.
- **Sandboxd lease-time drive injection.** `LeaseSandboxRequest` does
  not yet take a `Drives` field. Today you create the sandbox at the
  REST layer, not the sandboxd layer.
- **Hot-plug.** virtio-blk in gocracker requires the drive to be
  declared at VMM start. Hot-plug would let an already-running VM
  pick up a new code disk without reboot, but neither QEMU's hot-plug
  pathway nor Firecracker's are implemented in gocracker today.

## Phase 2 — snapshot + per-launch code disk (shipped)

Implemented on `feat/slirp-net-and-atomic-disk-meta` (commit
`764a262`). The full plumbing in ~25 LoC of core change:

1. `pkg/vmm/vmm.go`: `RestoreOptions.AdditionalDrives []DriveConfig`.
   `applyAdditionalDrives` merges them into `snap.Config.Drives`
   before `setupDevices` runs, with collision/root/empty guards.
2. `internal/vmmserver/server.go`: `RestoreRequest.AdditionalDrives`
   forwarded to `vmm.RestoreOptions`.
3. `internal/worker/vmm.go`: plumbs the field through `client.Restore`
   in `LaunchRestoredVMM` so jailed restores get the same shape.
4. `pkg/container/container.go`: both restore paths (`runLocal` and
   `runViaWorker`) translate `opts.CodeDisks` into AdditionalDrives
   via `codeDisksAsDriveConfigs`, and after Resume invoke
   `MountAdditionalCodeDisks` (in `pkg/container/codedisk_mount.go`)
   which drives the in-guest mount via `toolbox.Exec(["mount", …])`.
   The script polls for `/dev/vdb` in devtmpfs (50 ms backoff up to
   1 s) before calling `mount(2)` — covers the kernel-publish race
   that Agent C #4 surfaced.

The snapshot capture point doesn't need a new "post-init, pre-app"
boundary — reusing the existing exec endpoint works because the
toolbox agent is already running by the time the snapshot is taken
(template VMs boot with `ExecEnabled: true, InteractiveExec: true`).

### Known limitation

`tests/manual-smoke/cmd/codedisksnapshot` ran end-to-end except for
one thing: the toolbox agent closes the /exec connection before the
EXIT frame on the FIRST request after a snapshot-restore. The Health
probe answers, so the vsock channel + UDS bridge work; the bug is
inside the agent's exec handler post-restore. Tracking as a separate
issue — the Phase 2 plumbing here is correct and exercised by the
unit tests in `pkg/vmm/restore_drives_test.go` and
`pkg/container/codedisk_mount_test.go`.

### Remaining gaps (deferred)

| Gap | Where | Why deferred |
| --- | --- | --- |
| Toolbox /exec breaks on first call after restore | `internal/toolbox/agent` | Pre-existing interaction with snapshot/resume — needs its own debug session, not in scope of this PR |
| Dirty-tracking is root-only | [pkg/vmm/migration.go](../../pkg/vmm/migration.go) | Blocks live-migration with code-disks; OK for static restore. Iterate `m.blkDevs[]` in patch loop when needed. |

## Phase 3 — sandboxd lease-time injection (wire shipped)

Implemented as wire-shape on the same branch:

- `sandboxes/internal/pool/pool.go`: `LeaseSpec.CodeDisks
  []container.CodeDisk` field.
- `sandboxes/internal/sandboxd/pool.go`: `LeaseSandboxRequest.CodeDisks`
  forwarded into `pool.LeaseSpec.CodeDisks` in `Manager.LeaseSandbox`.
- `sandboxes/internal/sandboxd/server.go`: handler unchanged — JSON
  decoder picks up the new field automatically; covered by
  `lease_codedisk_test.go`.
- SDKs:
  - `sandboxes/sdk/python/gocracker/client.py`:
    `lease_sandbox(template_id, …, code_disks=[{host_path, mount, …}])`
  - `sandboxes/sdk/go/client.go`: `LeaseSandboxRequest.CodeDisks
    []CodeDisk`.
  - `sandboxes/sdk/js/src/index.js`: `leaseSandbox({codeDisks: [...]})`.

### Phase 3 next steps (functional application)

The wire is in; the runtime **does not yet attach** the disks at lease
time. The pool's existing model gives out already-restored VMs (warm
resume), and gocracker has no virtio-blk hot-plug, so attach-at-lease
needs one of:

1. **Restore-on-demand pool entries** (recommended). Keep entries as
   "snapshot ready to restore" rather than "running VM ready to
   resume". `Acquire` becomes
   `vmm.RestoreFromSnapshotWithOptions(snapDir, RestoreOptions{
   AdditionalDrives: lease.CodeDisks})` followed by `Start`. Cost:
   ~30 ms restore vs ~1 ms resume — still way under cold boot but no
   longer "free".
2. **Hot-plug virtio-blk**. Substantial change to the VMM device
   subsystem; not currently scoped.

Until one of these lands, sandboxd accepts the field, the SDK round-
trip works, but the disks are silently *not* mounted. The smoke binary
tests/manual-smoke/cmd/codedisksnapshot demonstrates the cold-boot
flavor of the attach (which DOES work via Phase 2's restore path with
a fresh container.Run).

### Expected TTI once functional

`lease cost + restore (30 ms) + drive attach (~ms) + post-restore
mount via toolbox.Exec (1-3 ms)` ≈ 35-40 ms per lease. Cold-boot of
the same template is ~280 ms, so the speedup is ~7×.

## Verification

Phase 1 (cold boot, fully functional):

```bash
make build kernel-unpack
sudo bash tests/manual-smoke/code-disk/run.sh
# Expect:
#   OK [v1]: code-disk version v1: alpha
#   OK [v2]: code-disk version v2: bravo
```

Phase 2 (snapshot+restore plumbing):

```bash
go build -o bin/codedisksnapshot ./tests/manual-smoke/cmd/codedisksnapshot
sudo ./bin/codedisksnapshot
# Currently fails at the toolbox.Exec post-restore step (see "known
# limitation" above). The plumbing itself is unit-tested:
go test ./pkg/vmm -run TestApplyAdditionalDrives
go test ./pkg/container -run "TestCodeDisksAsDriveConfigs|TestCacheLookupAndStore|TestHashSourceDir"
```

Phase 3 (wire shape):

```bash
go test ./sandboxes/internal/sandboxd -run TestHandleLeaseSandbox_
```

## Real-app examples

Three working examples live in [examples/code-disk/](../../examples/code-disk/).
Each ships a real application, a bundled data file, and a `build.sh` that
produces the ext4 disk image.

| Example | Image | What it does |
| --- | --- | --- |
| [node-word-count](../../examples/code-disk/node-word-count/) | `node:20-alpine` | Node.js reads `/app/text.txt` from the code disk; emits a JSON word-frequency report (top-10, totals) |
| [python-stats](../../examples/code-disk/python-stats/) | `python:3.12-alpine` | Python reads `/app/cities.csv`; emits JSON population statistics (mean, top-5, histogram buckets) |
| [go-serve](../../examples/code-disk/go-serve/) | `alpine:3.20` | Statically-compiled Go binary reads `/app/config.json`; HTTP server with a `--print` mode for non-networked testing |

### Quick start (individual examples)

```bash
make build kernel-unpack

# node word-count
bash examples/code-disk/node-word-count/build.sh
sudo bin/gocracker run \
  --image node:20-alpine \
  --kernel artifacts/kernels/gocracker-guest-standard-vmlinux \
  --code-disk examples/code-disk/node-word-count/node-word-count.ext4:/app:ext4:ro \
  --net none --jailer off --wait \
  --cmd 'node /app/word-count.js'

# python population stats
bash examples/code-disk/python-stats/build.sh
sudo bin/gocracker run \
  --image python:3.12-alpine \
  --kernel artifacts/kernels/gocracker-guest-standard-vmlinux \
  --code-disk examples/code-disk/python-stats/python-stats.ext4:/app:ext4:ro \
  --net none --jailer off --wait \
  --cmd 'python3 /app/stats.py'

# go HTTP server (print mode)
bash examples/code-disk/go-serve/build.sh
sudo bin/gocracker run \
  --image alpine:3.20 \
  --kernel artifacts/kernels/gocracker-guest-standard-vmlinux \
  --code-disk examples/code-disk/go-serve/go-serve.ext4:/app:ext4:ro \
  --net none --jailer off --wait \
  --cmd '/app/go-serve --print'
```

### Full automated smoke test (all three apps)

```bash
make build kernel-unpack
sudo bash tests/manual-smoke/code-disk-apps/run.sh
# Expected:
#   Results: 10 passed, 0 failed
```
