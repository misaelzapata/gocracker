# Getting Started with gocracker

gocracker boots OCI containers as real Linux microVMs using KVM.
No Docker daemon required. Inspired by Firecracker.

## System Requirements

- **Linux** (x86_64 or aarch64). No macOS or Windows support.
- **KVM** enabled in your kernel.
- **Go 1.22+** only if building from source (pre-built binaries available).
- **ip** (iproute2) and **iptables** (or iptables-nft) for networking.
- Root privileges (or appropriate `/dev/kvm` permissions).

## 1. Check KVM Support

```bash
ls /dev/kvm
```

If the file does not exist, enable KVM in your BIOS/UEFI (Intel VT-x / AMD-V)
or load the kernel module:

```bash
sudo modprobe kvm_intel   # Intel
sudo modprobe kvm_amd     # AMD
```

Verify your user can access it:

```bash
[ -r /dev/kvm ] && [ -w /dev/kvm ] && echo "KVM OK" || echo "no access"
```

## 2. Install gocracker

### Option A: Download pre-built binaries (fastest)

Download the latest release from GitHub:

```bash
# x86-64
curl -L -o gocracker https://github.com/misaelzapata/gocracker/releases/latest/download/gocracker-linux-amd64
chmod +x gocracker

# ARM64
curl -L -o gocracker https://github.com/misaelzapata/gocracker/releases/latest/download/gocracker-linux-arm64
chmod +x gocracker
```

### Option B: Build from source

```bash
git clone https://github.com/misaelzapata/gocracker.git
cd gocracker
make build
```

This produces a fully static `gocracker` binary (CGO_ENABLED=0).

## 3. Get a Guest Kernel

Pre-built guest kernels are included in the repository (gzip-compressed):

```bash
# x86-64
gunzip -k artifacts/kernels/gocracker-guest-standard-vmlinux.gz

# ARM64
gunzip -k artifacts/kernels/gocracker-guest-standard-arm64-Image.gz
```

Or build a minimal guest kernel from source:

```bash
make kernel-guest
```

This runs `tools/build-guest-kernel.sh --profile standard` and places the
kernel at `artifacts/kernels/gocracker-guest-standard-vmlinux`.

Alternatively, use a pre-built kernel if available, or your distribution's
vmlinuz (must have virtio drivers and 9P/ext4 built in).

## 4. Run Your First VM

Boot an Alpine container as a microVM:

```bash
sudo ./gocracker run \
  --image alpine:3.20 \
  --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux \
  --wait \
  --tty off \
  --cmd 'echo hello from gocracker'
```

The `--wait` flag blocks until the VM exits. Without it, gocracker prints the
VM ID and returns immediately.

## 5. Interactive Shell

Open an interactive session inside an Ubuntu VM:

```bash
sudo ./gocracker run \
  --image ubuntu:22.04 \
  --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux \
  --wait \
  --tty force
```

This allocates a PTY over virtio-vsock and drops you into a shell. Press
`Ctrl-]` (or let the process exit) to stop the VM.

## 6. From a Dockerfile

Build and boot a Dockerfile directly:

```bash
sudo ./gocracker run \
  --dockerfile ./tests/examples/python-api/Dockerfile \
  --context ./tests/examples/python-api \
  --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux \
  --wait \
  --cmd 'python3 app.py'
```

The `--context` flag sets the build context directory (where `COPY` and `ADD`
instructions look for files). It works exactly like the path argument in
`docker build -f Dockerfile <context>`. gocracker builds the image using a
built-in OCI builder (no Docker daemon), creates an ext4 disk, and boots the
result as a VM.

## 7. From a Git Repo

Clone a repo and auto-detect its Dockerfile:

```bash
sudo ./gocracker repo \
  --url https://github.com/user/myapp \
  --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux \
  --wait
```

Use `--ref` and `--subdir` to pick a branch/tag and subdirectory.

## 8. Compose (Multi-VM Stacks)

Boot a `docker-compose.yml` where each service becomes its own VM:

```bash
sudo ./gocracker compose \
  --file ./tests/manual-smoke/fixtures/compose-todo-postgres/docker-compose.yml \
  --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux \
  --wait
```

The included example runs a Flask app backed by PostgreSQL. The app service
waits for the Postgres healthcheck before starting. Published ports are
forwarded to the host automatically.

See [COMPOSE.md](COMPOSE.md) for full details.

## 9. REST API Server

Start the Firecracker-compatible API server:

```bash
sudo ./gocracker serve --addr :8080
```

Then create VMs via the HTTP API:

```bash
# Configure boot source
curl -s -X PUT http://localhost:8080/boot-source \
  -H 'Content-Type: application/json' \
  -d '{"kernel_image_path":"./artifacts/kernels/gocracker-guest-standard-vmlinux",
       "boot_args":"console=ttyS0 reboot=k panic=1"}'

# Set machine config
curl -s -X PUT http://localhost:8080/machine-config \
  -H 'Content-Type: application/json' \
  -d '{"vcpu_count":1,"mem_size_mib":256}'
```

## Common Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--kernel` | (required) | Path to the guest kernel image |
| `--image` | | OCI image reference (e.g. `alpine:3.20`) |
| `--dockerfile` | | Path to a Dockerfile |
| `--context` | `.` | Build context directory |
| `--mem` | `256` | RAM in MiB |
| `--cpus` | `1` | Number of vCPUs |
| `--disk` | `2048` | Disk image size in MiB |
| `--net` | `none` | Network mode: `none` or `auto` |
| `--tap` | | Explicit TAP interface name |
| `--tty` | `auto` | Console mode: `auto`, `off`, or `force` |
| `--wait` | `false` | Block until the VM exits |
| `--env` | | Comma-separated `KEY=VALUE` pairs |
| `--cmd` | | Override the image CMD |
| `--entrypoint` | | Override the image ENTRYPOINT |
| `--workdir` | | Override the working directory |
| `--cache-dir` | `/tmp/gocracker/cache` | Persistent artifact cache |
| `--jailer` | `on` | Privilege model: `on` or `off` |
| `--snapshot` | | Restore from a snapshot directory |

## Networking

By default, VMs have no network (`--net none`). Pass `--net auto` to get a
TAP interface with NAT to the internet. See [NETWORKING.md](NETWORKING.md).

## Next Steps

- [ARCHITECTURE.md](ARCHITECTURE.md) -- internals and boot flow
- [NETWORKING.md](NETWORKING.md) -- networking modes in detail
- [COMPOSE.md](COMPOSE.md) -- multi-service stacks

---

## More Documentation

- [Getting Started](GETTING_STARTED.md) | [Networking](NETWORKING.md) | [Architecture](ARCHITECTURE.md) | [Compose](COMPOSE.md)
- [API Reference](API.md) | [CLI Reference](CLI_REFERENCE.md) | [Snapshots](SNAPSHOTS.md)
- [Examples](EXAMPLES.md) | [Validated Projects](VALIDATED_PROJECTS.md) | [Troubleshooting](TROUBLESHOOTING.md)
- [Security Policy](../SECURITY.md)

