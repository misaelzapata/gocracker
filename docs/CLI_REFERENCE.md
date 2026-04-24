# CLI Reference

```
gocracker <command> [flags]
```

## Commands Overview

| Command | Description |
|---------|-------------|
| `run` | Build and boot a microVM from an OCI image, Dockerfile, or local path |
| `repo` | Clone a git repo and boot its Dockerfile |
| `compose` | Boot a docker-compose.yml stack as microVMs |
| `build` | Build a disk image without booting |
| `restore` | Restore and boot a VM from a snapshot |
| `migrate` | Live-migrate a VM between API servers |
| `serve` | Start the REST API server |

Internal commands (not typically invoked directly):

| Command | Description |
|---------|-------------|
| `vmm` | Start a single-VM Firecracker-compatible API worker |
| `build-worker` | Start the jailed build worker |
| `jailer` | Start a Firecracker-style jailer for a worker/VMM |

---

## run

Build and boot a microVM from an OCI image or Dockerfile.

```bash
sudo gocracker run --image ubuntu:22.04 --kernel ./vmlinux --wait --jailer off
sudo gocracker run --dockerfile ./Dockerfile --context . --kernel ./vmlinux --mem 512 --wait
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--image` | string | | OCI image ref (e.g. `ubuntu:22.04`) |
| `--dockerfile` | string | | Path to Dockerfile |
| `--context` | string | `.` | Build context directory |
| `--kernel` | string | (required) | Kernel image path |
| `--mem` | uint64 | `256` | RAM in MiB |
| `--arch` | string | host arch | Guest architecture: `amd64` or `arm64` |
| `--cpus` | int | `1` | vCPU count |
| `--x86-boot` | string | `auto` | x86 boot mode: `auto`, `acpi`, or `legacy` |
| `--net` | string | `none` | Network mode: `none` or `auto` |
| `--tap` | string | | TAP interface (e.g. `tap0`) |
| `--disk` | int | `2048` | Disk size in MiB |
| `--snapshot` | string | | Restore from snapshot directory |
| `--cache-dir` | string | `/tmp/gocracker/cache` | Persistent cache directory |
| `--env` | string | | Comma-separated `KEY=VALUE` env vars |
| `--cmd` | string | | Override CMD |
| `--entrypoint` | string | | Override ENTRYPOINT |
| `--workdir` | string | | Override working directory |
| `--id` | string | (auto) | VM identifier |
| `--wait` | bool | `false` | Block until VM stops |
| `--tty` | string | `auto` | Console mode: `auto`, `off`, or `force` |
| `--jailer` | string | `on` | Privilege model: `on` or `off` |
| `--build-arg` | string | | Build arg `KEY=VALUE` (repeatable) |
| `--balloon-target-mib` | uint64 | `0` | Balloon target in MiB |
| `--balloon-deflate-on-oom` | bool | `false` | Allow balloon deflate on guest OOM |
| `--balloon-stats-interval-s` | int | `0` | Balloon statistics polling interval (seconds) |
| `--balloon-auto` | string | `off` | Balloon auto policy: `off` or `conservative` |
| `--hotplug-total-mib` | uint64 | `0` | Hotpluggable memory region total size in MiB |
| `--hotplug-slot-mib` | uint64 | `0` | Hotpluggable memory slot size in MiB |
| `--hotplug-block-mib` | uint64 | `0` | Hotpluggable memory block size in MiB |
| `--warm` | bool | `false` | Enable warm-cache: capture a snapshot on first boot, restore from it on subsequent runs with the same image/kernel/config (see [Warm Cache](SNAPSHOTS.md#warm-cache)) |

---

## repo

Clone a git repo and boot the Dockerfile found inside.

```bash
sudo gocracker repo --url https://github.com/user/myapp --kernel ./vmlinux --wait
sudo gocracker repo --url ./myapp --kernel ./vmlinux --wait --jailer off
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--url` | string | (required) | Git repo URL or local path |
| `--ref` | string | | Branch or tag to checkout |
| `--subdir` | string | | Subdirectory inside repo |
| `--kernel` | string | (required) | Kernel image path |
| `--mem` | uint64 | `256` | RAM in MiB |
| `--arch` | string | host arch | Guest architecture: `amd64` or `arm64` |
| `--cpus` | int | `1` | vCPU count |
| `--x86-boot` | string | `auto` | x86 boot mode |
| `--net` | string | `none` | Network mode: `none` or `auto` |
| `--tap` | string | | TAP interface |
| `--disk` | int | `2048` | Disk size in MiB |
| `--snapshot` | string | | Restore from snapshot directory |
| `--cache-dir` | string | `/tmp/gocracker/cache` | Persistent cache directory |
| `--env` | string | | Comma-separated env vars |
| `--cmd` | string | | Override CMD |
| `--entrypoint` | string | | Override ENTRYPOINT |
| `--workdir` | string | | Override working directory |
| `--wait` | bool | `false` | Block until VM stops |
| `--tty` | string | `auto` | Console mode |
| `--jailer` | string | `on` | Privilege model |
| `--build-arg` | string | | Build arg `KEY=VALUE` (repeatable) |
| `--balloon-target-mib` | uint64 | `0` | Balloon target in MiB |
| `--balloon-deflate-on-oom` | bool | `false` | Deflate on guest OOM |
| `--balloon-stats-interval-s` | int | `0` | Stats polling interval |
| `--balloon-auto` | string | `off` | Auto balloon policy |
| `--hotplug-total-mib` | uint64 | `0` | Hotpluggable memory total |
| `--hotplug-slot-mib` | uint64 | `0` | Hotpluggable slot size |
| `--hotplug-block-mib` | uint64 | `0` | Hotpluggable block size |

---

## compose

Boot a `docker-compose.yml` stack as microVMs.

```bash
sudo gocracker compose --file docker-compose.yml --kernel ./vmlinux --wait --jailer off
```

Subcommands: `compose down`, `compose exec`.

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--file` | string | `docker-compose.yml` | Path to docker-compose.yml |
| `--server` | string | | Optional API server URL for compose-managed VMs |
| `--kernel` | string | (required) | Kernel image path |
| `--cache-dir` | string | `/tmp/gocracker/cache` | Persistent cache directory |
| `--mem` | uint64 | `256` | Default RAM per service (MiB) |
| `--arch` | string | host arch | Guest architecture |
| `--disk` | int | `4096` | Default disk size per service (MiB) |
| `--x86-boot` | string | `auto` | x86 boot mode |
| `--tap-prefix` | string | `gc` | TAP interface name prefix |
| `--snapshot` | string | | Snapshot directory to restore from / save to |
| `--wait` | bool | `false` | Block until all VMs stop |
| `--save-snapshot` | bool | `false` | Take snapshots on Ctrl-C / stop |
| `--jailer` | string | `on` | Privilege model |

### compose down

```bash
gocracker compose down --server http://localhost:8080 --file docker-compose.yml
```

### compose exec

```bash
gocracker compose exec --server http://localhost:8080 myservice -- ls /app
gocracker compose exec --server http://localhost:8080 myservice   # interactive shell
```

---

## build

Build a disk image without booting a VM.

```bash
sudo gocracker build --image python:3.12-slim --output ./disk.ext4
sudo gocracker build --dockerfile ./Dockerfile --context . --output ./disk.ext4
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--image` | string | | OCI image ref |
| `--dockerfile` | string | | Path to Dockerfile |
| `--context` | string | `.` | Build context directory |
| `--repo` | string | | Git repo URL or local path |
| `--ref` | string | | Branch or tag |
| `--subdir` | string | | Subdirectory inside repo |
| `--output` | string | (required) | Output ext4 image path |
| `--disk` | int | `2048` | Disk size in MiB |
| `--cache-dir` | string | `/tmp/gocracker/cache` | Cache directory |
| `--jailer` | string | `on` | Privilege model |
| `--build-arg` | string | | Build arg `KEY=VALUE` (repeatable) |

---

## restore

Restore and boot a VM from a snapshot directory.

```bash
sudo gocracker restore --snapshot ./snap-dir --jailer off --wait
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--snapshot` | string | (required) | Snapshot directory |
| `--wait` | bool | `false` | Block until VM stops |
| `--tty` | string | `auto` | Console mode |
| `--cpus` | int | `0` | Override vCPU count (0 = from snapshot) |
| `--x86-boot` | string | | Override x86 boot mode |
| `--jailer` | string | `on` | Privilege model |

---

## migrate

Live-migrate a VM between two gocracker API servers.

```bash
gocracker migrate --source http://host-a:8080 --id vm-123 --dest http://host-b:8080
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--source` | string | (required) | Source API base URL |
| `--id` | string | (required) | VM identifier on source |
| `--dest` | string | (required) | Destination API base URL |
| `--target-id` | string | | Override VM identifier on destination |
| `--tap` | string | | Override TAP interface on destination |
| `--no-resume` | bool | `false` | Leave VM paused on destination |

---

## serve

Start the REST API server. See [API.md](API.md) for endpoint documentation.

```bash
sudo gocracker serve --addr :8080 --jailer off
sudo gocracker serve --sock /tmp/gocracker.sock
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--addr` | string | | TCP listen address (e.g. `:8080`) |
| `--sock` | string | `/tmp/gocracker.sock` | Unix socket path |
| `--auth-token` | string | `$GOCRACKER_API_TOKEN` | Bearer token for authentication |
| `--x86-boot` | string | `auto` | Default x86 boot mode |
| `--jailer` | string | `on` | Privilege model |
| `--jailer-binary` | string | `gocracker-jailer` | Path to jailer binary |
| `--vmm-binary` | string | `gocracker-vmm` | Path to VMM binary |
| `--state-dir` | string | `/tmp/gocracker-serve-state` | Supervisor state directory |
| `--cache-dir` | string | `/tmp/gocracker/cache` | Build/OCI cache directory |
| `--chroot-base-dir` | string | (auto) | Base directory for jail roots |
| `--uid` | int | caller UID | UID for jailed workers |
| `--gid` | int | caller GID | GID for jailed workers |
| `--trusted-kernel-dir` | string | (auto) | Trusted kernel directory (repeatable) |
| `--trusted-work-dir` | string | (auto) | Trusted workspace directory (repeatable) |
| `--trusted-snapshot-dir` | string | (auto) | Trusted snapshot directory (repeatable) |

---

## More Documentation

- [Getting Started](GETTING_STARTED.md) | [Networking](NETWORKING.md) | [Architecture](ARCHITECTURE.md) | [Compose](COMPOSE.md)
- [API Reference](API.md) | [CLI Reference](CLI_REFERENCE.md) | [Snapshots](SNAPSHOTS.md)
- [Examples](EXAMPLES.md) | [Validated Projects](VALIDATED_PROJECTS.md) | [Troubleshooting](TROUBLESHOOTING.md)
- [Security Policy](../SECURITY.md)

