# Snapshots and Live Migration

gocracker can snapshot a running VM, persist its full state to disk, and restore
it later -- on the same host or a different one.

## How It Works

1. The VM is paused (vCPUs stop executing).
2. vCPU registers, special registers, and MP state are captured via KVM ioctls.
3. Device state is serialized: UART registers, virtio transport/queue state,
   balloon configuration.
4. Guest RAM is written to `mem.bin`.
5. Static artifacts (kernel, initrd, disk image) are copied into the bundle
   under `artifacts/`.
6. A `snapshot.json` manifest ties everything together (version 3 format).
7. The VM is left paused so the caller can resume or stop it.

Bundle layout:

```
snapshot-dir/
  snapshot.json          # manifest with vCPU state, device state, config
  mem.bin                # full guest RAM dump
  artifacts/
    kernel               # kernel image
    initrd               # initrd (if used)
    disk.ext4            # root disk image
```

## Via Compose (save on Ctrl-C)

```bash
sudo gocracker compose \
  --file docker-compose.yml \
  --kernel ./vmlinux \
  --wait \
  --save-snapshot \
  --snapshot ./snap-dir \
  --jailer off
```

When you press Ctrl-C, gocracker snapshots every service VM into `./snap-dir`
before shutting down. Restore the whole stack by passing `--snapshot ./snap-dir`
on the next `compose` invocation.

## Via API

```bash
curl -s -X POST http://localhost:8080/vms/vm-abc123/snapshot \
  -d '{"dest_dir": "/tmp/my-snapshot"}'
```

The response confirms the snapshot directory. The VM remains paused after
snapshot; issue `PUT /actions {"action_type":"InstanceResume"}` or stop it.

## Restore from Snapshot

### CLI

```bash
sudo gocracker restore \
  --snapshot ./snap-dir \
  --jailer off \
  --wait
```

Flags:

| Flag | Default | Description |
|------|---------|-------------|
| `--snapshot` | (required) | Snapshot directory |
| `--wait` | `false` | Block until VM stops |
| `--tty` | `auto` | Console mode: `auto`, `off`, or `force` |
| `--cpus` | `0` (from snapshot) | Override expected vCPU count |
| `--x86-boot` | (from snapshot) | Override x86 boot mode |
| `--jailer` | `on` | Privilege model: `on` or `off` |

### Via API

Use `POST /run` with `"snapshot_dir": "/path/to/snap-dir"` to restore a VM
through the API server.

## Live Migration

gocracker supports pre-copy live migration between two API servers using dirty
page tracking.

### CLI

```bash
gocracker migrate \
  --source http://host-a:8080 \
  --id vm-abc123 \
  --dest http://host-b:8080
```

Flags:

| Flag | Description |
|------|-------------|
| `--source` | Source API server URL (required) |
| `--id` | VM identifier on the source (required) |
| `--dest` | Destination API server URL (required) |
| `--target-id` | Override VM identifier on destination |
| `--tap` | Override TAP interface on destination |
| `--no-resume` | Leave VM paused on destination after migration |

### How pre-copy migration works

1. `PrepareMigrationBundle` -- copies kernel, initrd, disk, and a full RAM
   snapshot while the VM keeps running. Enables KVM dirty logging.
2. The bundle is transferred to the destination host.
3. `FinalizeMigrationBundle` -- pauses the VM, captures vCPU/device state,
   collects dirty memory and disk pages since step 1, writes them as a compact
   patch set (`patches.json` + `patches.bin`).
4. The patch set is shipped to the destination.
5. `ApplyMigrationPatches` merges the deltas into the base bundle.
6. `RestoreMigrationBundle` restores the VM on the destination.

### Orchestration endpoints (used by `migrate` CLI)

| Endpoint | Role |
|----------|------|
| `POST /migrations/prepare` | Start pre-copy on source |
| `POST /migrations/load` | Load base bundle on destination |
| `POST /migrations/finalize` | Pause, capture delta on source |
| `POST /migrations/abort` | Cancel and resume source VM |

## Sandbox template flow

For warm-pool / sandbox-per-template patterns, a second `gocracker serve`
is not needed — the `POST /vms/{id}/clone` endpoint snapshots a running
source and restores it as a new VM on the same server in one atomic call.

```
  boot template (run)  →  install tools (exec)  →  clone per sandbox
    ▲                                                    │
    └─── source keeps running ───────────────────────────┘
```

Full walkthrough (network works end-to-end, package manager works in the
clone, source stays usable):

```bash
# 1. Publish the template with network_mode=auto + exec (the two flags
#    /clone needs later to re-IP the guest).
TEMPLATE=$(curl -sS http://127.0.0.1:8080/run -X POST \
  -H 'Content-Type: application/json' \
  -d '{"image":"alpine:3.20","kernel_path":"/k/vmlinux","mem_mb":256,
       "network_mode":"auto","exec_enabled":true,"wait":true,
       "cmd":["/bin/sh","-lc","sleep infinity"]}' | jq -r .id)

# 2. Install the toolbox on the template. Changes land on the ext4 disk
#    and survive snapshot/restore.
curl -sS http://127.0.0.1:8080/vms/$TEMPLATE/exec -X POST \
  -d '{"command":["/bin/sh","-lc","apk add --no-cache bc && echo TEMPLATE-READY > /tmp/marker"]}'

# 3. Clone. Source keeps running unchanged; the clone gets its own tap +
#    fresh /30 subnet, eth0 is re-IP'd on the fly, and it inherits the
#    template's disk including the pre-installed toolbox.
CLONE=$(curl -sS http://127.0.0.1:8080/vms/$TEMPLATE/clone -X POST \
  -d '{"exec_enabled":true,"network_mode":"auto"}' | jq -r .id)

# 4. The clone has bc (from disk) AND working network (from re-IP).
curl -sS http://127.0.0.1:8080/vms/$CLONE/exec -X POST \
  -d '{"command":["/bin/sh","-lc","cat /tmp/marker && echo 2*21 | bc && apk add --no-cache file"]}'
# → stdout: "TEMPLATE-READY\n42\n...ok-install"

# 5. Optional: pause the template while idle, resume when another clone
#    request arrives. Both /pause and /resume return 204.
curl -sS -X POST http://127.0.0.1:8080/vms/$TEMPLATE/pause -d '{}'
curl -sS -X POST http://127.0.0.1:8080/vms/$TEMPLATE/resume -d '{}'
```

Properties the clone guarantees:

- Fresh VM ID (`gc-<random>`) and unique `tclone-<N>` tap so source +
  siblings coexist without TUNSETIFF EBUSY.
- Inherits the full source disk — no re-install cost per clone.
- `network_mode=auto` on `/clone` re-IPs eth0 + the default route inside
  the guest over exec, so outbound reaches the new gateway. Requires
  `exec_enabled=true`; the endpoint returns 400 otherwise.
- Metadata `cloned_from` points back at the source for observability.

### Virtio-fs mounts on the source (not clonable today)

`POST /vms/{id}/clone` returns 400 if the source VM holds live virtio-fs
mounts. The Linux virtio-fs driver stores in-flight FUSE request IDs in its
virtqueue; snapshot freezes that state while a fresh virtiofsd on the
restored side starts at queue index 0, tripping the kernel assertion
"requests.0:id 0 is not a head!" on first FUSE op. The endpoint refuses
rather than silently hang.

Workarounds for per-sandbox data:

- **umount before snapshot**: take the snapshot with no active virtio-fs
  mount (umount inside the guest first), then re-mount by hand after
  restore. `TestE2ECloneRejectsActiveVirtiofs` guards the live-mount case.
- **virtio-blk** (`drives` field on `/run`): attach a per-sandbox block
  device. virtio-blk queues do not have the FUSE-session problem.

The rebind *contract* (host-side Source rewrite + SocketPath clear + memfd-
backed restore) is implemented and covered by unit tests in
`pkg/vmm/sharedfs_rebind_test.go`; what is not yet supported is migrating
the guest-side FUSE session across the snapshot boundary.

## Warm Cache

The warm-cache feature captures a snapshot automatically on the first cold boot and
restores from it on every subsequent `run` call with the same parameters — without
any extra flags from the caller.

### Enabling it

```bash
# Per-invocation:
sudo gocracker run --image oven/bun:alpine --kernel ./vmlinux --warm

# Process-wide (all `run` calls in the process use the cache):
export GOCRACKER_WARM_CACHE=1
sudo gocracker run --image oven/bun:alpine --kernel ./vmlinux
```

### How it works

1. On the **first run** (`--warm` or `GOCRACKER_WARM_CACHE=1`), the VM boots
   normally (cold boot, ~200 ms).  A background goroutine waits for the exec
   agent to be ready (~150 ms after first console output), pauses the VM briefly,
   captures dirty pages as a sparse `mem.bin`, then resumes the VM.  The snapshot
   is stored under `~/.cache/gocracker/snapshots/<key>/`.
2. On **subsequent runs**, the runtime detects the cache key match and restores
   via `MAP_PRIVATE` on the sparse file — page-faults load only the pages the
   current command actually touches, so the restore itself completes in **~5–7 ms**.
3. If **`--net auto`** is used, the guest's `eth0` is automatically re-IP'd after
   restore via the exec agent so it routes through the new TAP's gateway.

### Cache key

The key is a SHA-256 over: OCI image digest · kernel binary hash · kernel
cmdline · memory (MiB) · vCPU count · architecture · network mode.  Any change
to these fields produces a new key and a new cold boot.

### Cache location

```
~/.cache/gocracker/snapshots/<key>/
  snapshot.json   # vCPU + device state
  mem.bin         # sparse guest RAM (only dirty pages occupy disk)
  artifacts/
    disk.ext4     # root disk (hardlink to the build cache — no copy cost)
    kernel
    initrd
```

Override with `XDG_CACHE_HOME`.

### Limitations

- Only OCI-image sources are cached.  Dockerfile and git-repo builds are skipped
  because their rootfs is non-deterministic across rebuilds.
- Block devices passed via `--drives` bypass the cache (drive content is not part
  of the cache key).
- Requires `--exec` / `ExecEnabled: true` — the exec agent provides the
  "guest is ready" signal used to time the snapshot capture.

## Limitations

- Same architecture only (x86-64 to x86-64, or ARM64 to ARM64).
- ARM64 GIC state save is partial; migration may not preserve interrupt state
  correctly on all workloads.
- The destination host must have the same (or newer) kernel and KVM capabilities.
- Snapshot bundles include the full disk image; large disks produce large bundles.
- Virtio-fs devices cannot be active through the MAP_PRIVATE restore path;
  snapshots taken while a virtiofs export was mounted cannot be restored
  today. Take the snapshot without virtiofs or materialize the mount if the
  template needs to survive restore.

---

## More Documentation

- [Getting Started](GETTING_STARTED.md) | [Networking](NETWORKING.md) | [Architecture](ARCHITECTURE.md) | [Compose](COMPOSE.md)
- [API Reference](API.md) | [CLI Reference](CLI_REFERENCE.md) | [Snapshots](SNAPSHOTS.md)
- [Examples](EXAMPLES.md) | [Validated Projects](VALIDATED_PROJECTS.md) | [Troubleshooting](TROUBLESHOOTING.md)
- [How gocracker Fits In](COMPETITIVE_ANALYSIS.md) | [Security Policy](../SECURITY.md)

