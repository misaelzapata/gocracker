# Code-disk attach — design + Phase 1

Status: **Phase 1 shipped** on `feat/slirp-net-and-atomic-disk-meta`.
Phase 2/3 are deferred.

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

## Phase 2 — snapshot + per-launch code disk

The win the user asked for: attach a code disk to a *restored* VM, so
each launch is a snapshot-restore (~30 ms) plus the disk attach
(~ms), not a cold boot (~280 ms).

### Required changes

1. **`vmm.RestoreOptions.AdditionalDrives []DriveConfig`** — a list of
   drives present at restore time but absent in the snapshot. The
   restore loop must merge these into `cfg.Drives` *before*
   `setupDevices` runs, and assign them MMIO slots that are
   guaranteed not to collide with the snapshot's slots.
2. **Guest-side persistence** — the snapshot was taken with no code
   disk mounted; the restored guest's init runs *only* the resume
   continuation, not a full boot. We need a hook to re-run
   `mountExtraCodeDisks` after restore. Two options:
   - (a) Defer the mount to a userspace agent over the toolbox
     vsock — `gocracker-toolbox mount-code-disk` invoked by the
     restore handler.
   - (b) Snapshot at a "post-init, pre-app" boundary so the user
     binary launch is the resume work and the mount can run before
     app start.
   (b) is cleaner but requires a new boot mode; (a) is incremental.
3. **REST API** — `CloneRequest.CodeDisks []CodeDisk` so a clone of a
   warm template can pick a code disk per launch.

### Critical blockers

| Blocker | Where | Fix |
| --- | --- | --- |
| Restore can't accept new drives | [pkg/vmm/migration.go restoreFromSnapshot](../../pkg/vmm/migration.go) | Add `AdditionalDrives` plumbing through `RestoreOptions` |
| Guest mount happens at boot | [internal/guest/init.go](../../internal/guest/init.go) | Either toolbox-driven post-restore mount, or rebuild snapshot capture point |
| Dirty-tracking is root-only | [pkg/vmm/migration.go writeMigrationPatches](../../pkg/vmm/migration.go) | Iterate `m.blkDevs[]` in dirty patch loop |

## Phase 3 — sandboxd integration

Once Phase 2 lands, sandboxd grows a `LeaseSandboxRequest.CodeDisks`
field. Pool entries become "warm template + per-lease code disk". The
expected lease+exec TTI for a code-disk lease becomes roughly
`lease cost (~1.5 ms) + disk attach + boot continuation (~5–10 ms) +
app first-byte`. For a `node app.js` shape this should land in the
20–40 ms range — comparable to "warm container start" but with full
KVM isolation per launch.

## Verification (Phase 1)

```bash
make build kernel-unpack
sudo bash tests/manual-smoke/code-disk/run.sh
# Expect:
#   OK [v1]: code-disk version v1: alpha
#   OK [v2]: code-disk version v2: bravo
#   code-disk smoke OK — same alpine template, two distinct code disks.
```

The same disk built once and run twice over the same Alpine template
demonstrates that the rootfs really is shared and the code is in the
attached disk.
