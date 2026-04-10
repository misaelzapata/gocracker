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

## Limitations

- Same architecture only (x86-64 to x86-64, or ARM64 to ARM64).
- ARM64 GIC state save is partial; migration may not preserve interrupt state
  correctly on all workloads.
- The destination host must have the same (or newer) kernel and KVM capabilities.
- Snapshot bundles include the full disk image; large disks produce large bundles.
