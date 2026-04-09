<h1 align="center">gocracker</h1>

<p align="center">
  Run containers as microVMs. No Docker daemon. Just KVM.
</p>

<p align="center">
  gocracker takes an OCI image, a Dockerfile, a git repo, or a full Compose stack and boots each service as its own Linux microVM using KVM — in milliseconds, with its own kernel, disk, and network.
</p>

> [!WARNING]
> gocracker is **alpha-quality** software. The API surface is evolving and breaking changes may occur. Do not use in production without understanding the risks.

---

## Start in 60 seconds

### 1) Build

```bash
go build -o gocracker ./cmd/gocracker
go build -o gocracker-vmm ./cmd/gocracker-vmm
go build -o gocracker-jailer ./cmd/gocracker-jailer
```

### 2) Build a guest kernel

```bash
./tools/build-guest-kernel.sh
```

This creates `./artifacts/kernels/gocracker-guest-standard-vmlinux`.

### 3) Run your first VM

<p align="center">
  <img src="assets/demos/01-alpine-oneshot.gif" alt="gocracker: boot Alpine as a microVM" width="960" />
</p>

<p align="center">
  <a href="assets/demos/01-alpine-oneshot.mp4"><strong>Watch MP4</strong></a>
</p>

```bash
sudo ./gocracker run \
  --image alpine:3.20 \
  --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux \
  --cmd 'echo hello-from-vm' \
  --wait --tty off
```

That is the core loop: pick a source, pick a kernel, boot a VM.

Architecture note:
- `--arch` defaults to the host architecture and is persisted in VM/snapshot metadata.
- The shipped runtime backend is currently `amd64`; `--arch arm64` is rejected early with a clear error until the ARM64 backend lands.
- Snapshot, restore, and migration are `same-arch only`.
- The embedded guest init and seccomp profiles now exist for both `amd64` and `arm64`, and `make build` follows the host architecture by default.

---

## From a Dockerfile

Point gocracker at a Dockerfile and it builds + boots in one step.

<p align="center">
  <img src="assets/demos/03-dockerfile.gif" alt="gocracker: build and boot a Dockerfile" width="960" />
</p>

<p align="center">
  <a href="assets/demos/03-dockerfile.mp4"><strong>Watch MP4</strong></a>
</p>

```bash
sudo ./gocracker run \
  --dockerfile ./Dockerfile \
  --context . \
  --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux \
  --net auto --mem 512 --wait --tty off
```

Supports `RUN --mount=type=bind`, `--mount=type=cache`, `--mount=type=tmpfs`, `--mount=type=secret`, `COPY --from=<registry/image>`, heredoc syntax, multi-stage builds, and `USER` with proper home/env setup.

---

## From a git repo

Clone any repo and gocracker auto-detects its Dockerfile, builds, and boots.

<p align="center">
  <img src="assets/demos/04-git-repo.gif" alt="gocracker: clone a git repo and boot its Dockerfile" width="960" />
</p>

<p align="center">
  <a href="assets/demos/04-git-repo.mp4"><strong>Watch MP4</strong></a>
</p>

```bash
sudo ./gocracker repo \
  --url https://github.com/traefik/whoami \
  --ref 3c6fc4814630 \
  --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux \
  --net auto --wait --tty off
```

---

## Docker Compose

Boot a full stack. Each service gets its own microVM, its own disk, and its own IP inside an isolated network namespace.

<p align="center">
  <img src="assets/demos/05-compose.gif" alt="gocracker: Docker Compose multi-VM stack" width="960" />
</p>

<p align="center">
  <a href="assets/demos/05-compose.mp4"><strong>Watch MP4</strong></a>
</p>

```bash
sudo ./gocracker compose \
  --file docker-compose.yml \
  --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux \
  --wait
```

Example `docker-compose.yml`:

```yaml
services:
  db:
    image: postgres:16
    environment:
      POSTGRES_PASSWORD: secret
      POSTGRES_DB: app
    ports:
      - "5432:5432"

  app:
    build: .
    ports:
      - "8000:8000"
    depends_on:
      db:
        condition: service_healthy
    environment:
      DATABASE_URL: postgres://postgres:secret@db:5432/app
```

Each Compose service is a real VM: separate kernel, separate memory, separate disk. Services talk to each other through a stack-private bridge. Published ports are forwarded to the host.

---

## Interactive console (TTY)

Open an interactive shell inside a VM, just like `docker run -it`.

<p align="center">
  <img src="assets/demos/02-interactive-tty.gif" alt="gocracker: interactive shell inside a microVM" width="960" />
</p>

<p align="center">
  <a href="assets/demos/02-interactive-tty.mp4"><strong>Watch MP4</strong></a>
</p>

```bash
sudo ./gocracker run \
  --image alpine:3.20 \
  --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux \
  --wait --tty force
```

- `Ctrl-C` sends SIGINT to the guest (kills `top`, `sleep`, etc.)
- `Ctrl-]` detaches from the console without stopping the VM
- Backspace, arrow keys, tab completion all work

---

## Exec into a running VM

Execute commands inside a running guest via the API — no SSH keys, no guest IP lookup, no port forwarding. The exec agent runs over virtio-vsock directly between host and guest.

<p align="center">
  <img src="assets/demos/06-exec.gif" alt="gocracker: exec into a running VM via the API" width="960" />
</p>

<p align="center">
  <a href="assets/demos/06-exec.mp4"><strong>Watch MP4</strong></a>
</p>

### Via the API

```bash
# Start the API server
./gocracker serve --addr :8080

# Boot a VM
curl -sS -X POST http://localhost:8080/run \
  -H 'Content-Type: application/json' \
  -d '{"image":"alpine:3.20","kernel_path":"./artifacts/kernels/gocracker-guest-standard-vmlinux","mem_mb":256,"net":"auto","wait":true}'

# Exec into it
curl -sS -X POST http://localhost:8080/vms/{id}/exec \
  -H 'Content-Type: application/json' \
  -d '{"command":["cat","/etc/os-release"]}'
```

### Via Compose exec

```bash
./gocracker compose exec \
  --server http://127.0.0.1:8080 \
  --file docker-compose.yml \
  db -- psql -U postgres -c 'SELECT version()'
```

---

## Networking

<p align="center">
  <img src="assets/demos/07-networking.gif" alt="gocracker: automatic networking with --net auto" width="960" />
</p>

<p align="center">
  <a href="assets/demos/07-networking.mp4"><strong>Watch MP4</strong></a>
</p>

### Automatic (recommended)

```bash
sudo ./gocracker run \
  --image nginx:alpine \
  --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux \
  --net auto --wait --tty off
```

`--net auto` creates a TAP device, assigns a `/30` subnet, sets up IPv4 forwarding and NAT on the host. The guest gets internet access automatically.

### Manual TAP

```bash
sudo ./gocracker run \
  --image alpine:3.20 \
  --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux \
  --tap tap0 --wait --tty off
```

You manage the TAP device, IP addressing, and routing yourself.

### Compose networking

Compose creates an isolated network namespace per stack with a bridge, one TAP per service, and static IPs. Services resolve each other by name. Published ports are forwarded to the host.

```text
Host
 |
 +-- netns: compose-mystack
      |
      +-- bridge br0
      |    |
      |    +-- tap0 (db)     172.20.0.2
      |    +-- tap1 (app)    172.20.0.3
      |    +-- tap2 (redis)  172.20.0.4
      |
      +-- veth pair -> host (for published ports)
```

---

## Multi-vCPU guests

<p align="center">
  <img src="assets/demos/08-multi-vcpu.gif" alt="gocracker: SMP boot with 4 vCPUs" width="960" />
</p>

<p align="center">
  <a href="assets/demos/08-multi-vcpu.mp4"><strong>Watch MP4</strong></a>
</p>

```bash
sudo ./gocracker run \
  --image alpine:3.20 \
  --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux \
  --cpus 4 --mem 512 \
  --cmd 'echo CPUs: $(nproc) && free -m | head -2' \
  --wait --tty off
```

---

## Disk and memory

```bash
# 8 GiB disk, 1 GiB RAM
sudo ./gocracker run \
  --image postgres:16 \
  --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux \
  --disk 8192 --mem 1024 \
  --net auto --wait --tty off
```

The `--disk` flag controls the ext4 root filesystem size (in MiB). The default is 2048 MiB for `run`/`repo` and 4096 MiB for `compose`.

---

## Shared volumes (virtio-fs)

Compose stacks with writable shared volumes use virtio-fs to mount host directories into multiple guest VMs:

```yaml
services:
  writer:
    image: alpine:3.20
    volumes:
      - shared-data:/data
    command: sh -c "echo hello > /data/greeting.txt && sleep 30"

  reader:
    image: alpine:3.20
    volumes:
      - shared-data:/data
    command: sh -c "sleep 5 && cat /data/greeting.txt"
    depends_on:
      - writer

volumes:
  shared-data:
```

```bash
sudo ./gocracker compose \
  --file docker-compose.yml \
  --kernel ./artifacts/kernels/gocracker-guest-virtiofs-vmlinux \
  --x86-boot acpi --wait
```

Both VMs see the same `/data` directory in real time through virtio-fs. The virtiofs kernel profile is required for shared volumes.

---

## Snapshots and restore

Take a snapshot of a running VM:

```bash
curl -sS -X POST http://localhost:8080/vms/{id}/snapshot \
  -H 'Content-Type: application/json' \
  -d '{"dest_dir": "/tmp/snap1"}'
```

Restore it later:

```bash
sudo ./gocracker restore \
  --snapshot /tmp/snap1 \
  --wait --tty off
```

The snapshot captures full VM state: RAM, vCPU registers, and device state (UART, virtio transport, queues). The restored VM continues from exactly where it was paused.

---

## Live migration

Move a running VM from one host to another:

```bash
# On host A
./gocracker serve --addr :8080

# On host B
./gocracker serve --addr :8080

# Migrate
./gocracker migrate \
  --source http://host-a:8080 \
  --id vm-123 \
  --dest http://host-b:8080
```

Uses stop-and-copy with a pre-copy control plane for minimal downtime.

---

## API server

gocracker exposes a Firecracker-compatible REST API plus extended endpoints.

```bash
./gocracker serve --addr :8080
```

### Build + boot in one call

```bash
curl -sS -X POST http://localhost:8080/run \
  -H 'Content-Type: application/json' \
  -d '{"image":"alpine:3.20","kernel_path":"./artifacts/kernels/gocracker-guest-standard-vmlinux","mem_mb":256,"net":"auto"}'
```

### List VMs

```bash
curl -sS http://localhost:8080/vms
```

### Stop a VM

```bash
curl -sS -X POST http://localhost:8080/vms/{id}/stop
```

### Firecracker-compatible step-by-step boot

```bash
curl -X PUT http://localhost:8080/boot-source \
  -d '{"kernel_image_path": "./artifacts/kernels/gocracker-guest-standard-vmlinux"}'

curl -X PUT http://localhost:8080/machine-config \
  -d '{"vcpu_count": 2, "mem_size_mib": 256}'

curl -X PUT http://localhost:8080/drives/rootfs \
  -d '{"drive_id": "rootfs", "path_on_host": "/path/to/disk.ext4", "is_root_device": true}'

curl -X PUT http://localhost:8080/actions \
  -d '{"action_type": "InstanceStart"}'
```

### Events stream (SSE)

```bash
curl -N http://localhost:8080/events/stream
```

---

## Build without booting

Create a disk image for later use:

```bash
sudo ./gocracker build \
  --image nginx:alpine \
  --output /tmp/nginx-disk.ext4 \
  --disk 2048
```

The output is a standard ext4 image that can be used with `--snapshot`, the Firecracker-compatible API, or any tool that reads ext4.

---

## Real-world examples

gocracker has been validated against 116+ real-world open-source projects from the wild. Here are some that boot and serve traffic:

| Project | Stack | What it does |
|---------|-------|-------------|
| traefik/whoami | Go | Tiny HTTP service |
| nginx | C | Web server |
| postgres:16 | C | Database |
| redis | C | Cache |
| grafana | Go | Dashboard |
| MLflow | Python | ML platform |
| Strapi | Node | Headless CMS |
| Chatwoot | Ruby | Support inbox |
| BookStack | PHP | Documentation wiki |
| Kestra | Java | Workflow orchestrator |
| Meilisearch | Rust | Search engine |
| RabbitMQ | Erlang | Message broker |
| Livebook | Elixir | Interactive notebooks |

Each one boots from its upstream Dockerfile or OCI image, unmodified.

---

## Requirements

| Requirement | Details |
|-------------|---------|
| OS | Linux (x86-64) |
| KVM | `/dev/kvm` accessible |
| Go | 1.22+ to build |
| Privileges | `sudo` for VM operations (KVM, TAP, mount namespaces) |
| Network tools | `ip` + `iptables` (for `--net auto`) |

Check host readiness:

```bash
./tools/check-host-devices.sh
```

---

## CLI reference

```
gocracker run        Boot a VM from an OCI image or Dockerfile
gocracker repo       Clone a git repo and boot its Dockerfile
gocracker compose    Boot a docker-compose.yml stack as microVMs
gocracker build      Build a disk image without booting
gocracker restore    Restore a VM from a snapshot
gocracker migrate    Live-migrate a VM between API servers
gocracker serve      Start the REST API server

gocracker compose exec   Execute a command in a Compose service
gocracker compose down   Stop a Compose stack
```

### Key flags

| Flag | Available in | Description |
|------|-------------|-------------|
| `--image` | run, build | OCI image reference |
| `--dockerfile` | run, build | Path to Dockerfile |
| `--kernel` | run, repo, compose, restore | Guest kernel path |
| `--mem` | run, repo, compose | RAM in MiB (default: 256) |
| `--cpus` | run, repo | vCPU count (default: 1) |
| `--disk` | run, repo, compose, build | Root disk size in MiB |
| `--net` | run, repo | Network mode: `none` or `auto` |
| `--tap` | run, repo | Manual TAP device name |
| `--tty` | run, repo, restore | Console: `auto`, `off`, or `force` |
| `--wait` | run, repo, compose, restore | Block until VM stops |
| `--x86-boot` | run, repo, compose | Boot mode: `auto`, `acpi`, `legacy` |
| `--cmd` | run, repo | Override container CMD |
| `--entrypoint` | run, repo | Override container ENTRYPOINT |
| `--env` | run, repo | Environment variables (`KEY=VALUE,...`) |
| `--build-arg` | run, repo, build | Dockerfile build args (repeatable) |
| `--cache-dir` | run, repo, compose, build, serve | Persistent OCI/artifact cache |
| `--jailer` | run, repo, compose, build, serve | Privilege model: `on` or `off` |
| `--server` | compose, compose exec, compose down | API server URL |
| `--snapshot` | restore | Snapshot directory to restore from |

---

## How it works

```
Source (OCI image / Dockerfile / git repo)
    |
    v
Build: OCI pull or Dockerfile execute
    |
    v
Rootfs: extract layers -> ext4 disk image
    |
    v
Initrd: pure Go cpio builder + embedded init binary
    |
    v
KVM: create VM + load kernel + attach devices
    |
    v
Boot: kernel -> initrd init -> mount ext4 root -> pivot_root -> exec workload
    |
    v
Devices: UART (console) + virtio-net (TAP) + virtio-blk (disk) + virtio-rng (entropy) + virtio-vsock (exec agent)
```

Each VM is a real Linux instance with its own:
- Kernel (loaded from ELF vmlinux)
- Root filesystem (ext4 built from OCI layers)
- Network interface (TAP with optional NAT)
- Console (16550A UART over PTY)
- Entropy source (virtio-rng from host `/dev/urandom`)
- Host-guest channel (virtio-vsock for exec without SSH)

No Docker daemon. No container runtime. Just KVM.
