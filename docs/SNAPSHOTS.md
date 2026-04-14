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

Typical sequence:

1. Boot a template VM with `network_mode=auto` and `exec_enabled=true`.
2. Install the toolbox (`apk add …`, `pip install …`, stage binaries). The
   disk is ext4; changes stay in the VM until clone.
3. Optionally `POST /vms/{id}/pause` while you wait for a clone request;
   pause freezes vCPUs without tearing down state.
4. For each sandbox: `POST /vms/{templateID}/clone` with per-instance
   overrides (`tap_name`, `mounts` for virtiofs toolbox injection,
   `metadata`). The clone:
   - Gets a fresh ID and its own tap (`tclone-<N>` auto-minted when the
     caller does not override) so source and clone run concurrently.
   - Inherits the source's disk via snapshot restore — no re-install needed.
   - Reports `restored_from_snapshot=true`; metadata `cloned_from` points
     back at the source.
5. Stop the source when you're done provisioning or keep it as a live
   template for future clones.

### Virtiofs toolbox injection on clone

When the template boots with a `backend=virtiofs` mount pointing at a
placeholder directory, `POST /vms/{id}/clone` accepts `mounts` with the same
guest `target` and a different host `source`. The server rewrites the
snapshot's shared-FS export by matching guest target, keeps the tag (so the
guest kernel's frozen mount continues to find its device), and spawns a
fresh virtiofsd against the new source.

**Convention**: the template must snapshot with a matching virtiofs mount
point. Restoring with a target the template never mounted returns 400
(`snapshot has no virtiofs slot for target …`).

**Known limitation**: the snapshot restore fast path uses MAP_PRIVATE COW on
the memory file, which is incompatible with virtio-fs's memfd requirement.
The rebind plumbing + validation is in place (covered by unit tests in
`pkg/vmm/sharedfs_rebind_test.go`), but end-to-end guest I/O through the
rebound export needs the restore path to materialize guest memory in a memfd
when any virtio-fs device is present. Tracked as a follow-up gap separate
from the rest of the sandbox API.

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

