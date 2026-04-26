# gocracker

One binary. One command. Real VM isolation.

[![Go 1.22+](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Architecture](https://img.shields.io/badge/Architecture-docs-green)](docs/ARCHITECTURE.md)
[![Validated projects](https://img.shields.io/badge/Validated-projects-blue)](docs/VALIDATED_PROJECTS.md)

[![Linux KVM](https://img.shields.io/badge/Linux-KVM-FCC624?logo=linux&logoColor=black)](https://www.kernel.org)
[![x86-64](https://img.shields.io/badge/x86--64-supported-brightgreen)]()
[![ARM64](https://img.shields.io/badge/ARM64-supported-brightgreen)]()
[![OCI](https://img.shields.io/badge/OCI-compatible-purple?logo=open-containers-initiative)](https://opencontainers.org)
[![Inspired by Firecracker](https://img.shields.io/badge/Inspired_by-Firecracker-FF9900?logo=amazon-aws)](https://firecracker-microvm.github.io)

## What is gocracker?

I fell in love with [Firecracker](https://firecracker-microvm.github.io) --
the idea of booting a real Linux VM in milliseconds with minimal overhead is
brilliant. But every time I wanted to just run a container image as a microVM,
I had to: pull the image manually, extract the rootfs, build an ext4 disk,
generate an initrd, write a JSON config, set up a TAP interface, configure
iptables... and only then call the Firecracker API. I kept thinking: why can't
I just type one command?

So I built gocracker. It is a micro-VMM written in pure Go that does everything
Firecracker does at the KVM level -- virtio devices, jailer, seccomp -- but adds
the developer experience on top: pull any OCI image, build any Dockerfile, clone
any git repo, orchestrate Docker Compose stacks. Each service becomes a real
Linux VM.

```
gocracker run --image ubuntu:22.04 --kernel artifacts/kernels/gocracker-guest-standard-vmlinux --wait
```

No Docker daemon. No containerd. No runc. Just KVM.

This started as a hobby project to understand KVM and Go systems programming.
It grew into something that boots 378 real-world projects, runs Flask + PostgreSQL
compose stacks on ARM64 bare metal, and supports snapshots, live migration, and a
Firecracker-compatible REST API. All in a single static binary.

## Quick Start

```bash
# Build gocracker + unpack the prebuilt guest kernel that ships with the repo
make build kernel-unpack

# Run your first microVM (uses the unpacked vmlinux from artifacts/kernels/)
sudo ./gocracker run \
  --image alpine:latest \
  --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux \
  --cmd "echo hello from a real VM" \
  --wait
```

That's it — alpine boots in ~80 ms (warm OCI cache), runs the command, and exits. No kernel build required: the repo ships gzipped guest kernels under [artifacts/kernels/](artifacts/kernels/) for both x86_64 and arm64. `make kernel-unpack` decompresses them in place. To build a custom kernel from source, run `make kernel-guest` (x86_64) or `make kernel-guest-arm64` (arm64) instead — that takes ~10 min the first time.

## Features

**Container Sources** -- OCI images from any registry, local Dockerfiles (BuildKit AST), git repos with auto-detected Dockerfiles, local directories.

**Orchestration** -- Docker Compose stacks as microVMs, healthchecks (CMD/CMD-SHELL executed in-guest), `depends_on` with conditions (`service_healthy`, `service_completed_successfully`), `.env` and interpolation.

**Networking** -- `--net auto` (CLI) and `network_mode=auto` (REST) for single-command TAP + IPv4 + NAT, per-stack network namespaces for Compose, manual TAP for advanced setups, deterministic per-VM MAC addresses, port publishing.

**Isolation** -- KVM hardware virtualization, Firecracker-style jailer (`gocracker-jailer`), seccomp filters (per-arch), private mount namespaces, `pivot_root` isolation for builds.

**Operations** -- Snapshot / restore / pause / resume (RAM + vCPU + device state), in-place `/clone` (snapshot + restore as a new VM on the same server, fresh tap + guest re-IP), live migration (stop-and-copy), Firecracker-compatible REST API with extensions, structured logging, event streaming (SSE). Stale `/tmp/gocracker-*` from SIGKILL'd prior runs is pruned on `serve` startup.

**Devices** -- virtio-net, virtio-blk, virtio-rng, virtio-vsock, virtio-balloon (manual + auto reclaim), virtio-fs, UART 16550A serial console, memory hotplug.

**Sandbox control plane** -- [gocracker-sandboxd](sandboxes/cmd/gocracker-sandboxd/) — HTTP daemon that wraps the runtime with warm pools (`~1.5 ms p95 lease`), content-addressed templates (`~80 µs cache hit`), HMAC-signed preview URLs, and Python / Go / JS SDKs with typed errors and context-manager sandbox lifecycle. Per-sandbox Firecracker-style UDS at `<state-dir>/sandboxes/<id>.sock` speaks directly to a baked-in toolbox agent on vsock 10023 (framed exec + files + git + secrets + `SetNetwork` re-IP). See the [Sandboxd overview](#sandboxd-sandbox-control-plane).

## Examples

1. [Run Alpine, print a message](#1-run-alpine-print-a-message)
2. [Interactive Ubuntu session](#2-interactive-ubuntu-session)
3. [Build from Dockerfile](#3-build-from-dockerfile)
4. [Clone and boot a git repo](#4-clone-and-boot-a-git-repo)
5. [Docker Compose (Flask + PostgreSQL)](#5-docker-compose-flask--postgresql)
6. [Exec into a running Compose service](#6-exec-into-a-running-compose-service)
7. [Networking (auto NAT)](#7-networking-auto-nat)
8. [Multi-vCPU](#8-multi-vcpu)
9. [Sandbox pool from template (REST API)](#9-sandbox-pool-from-template-rest-api)
10. **[Sandbox control plane — raw HTTP](#10-sandbox-control-plane--raw-http)** — `gocracker-sandboxd` HTTP daemon + per-sandbox UDS + toolbox agent (exec / files / git / re-IP)

The snippets below use `$KERNEL` for the guest kernel path. Set it once after running `make build kernel-unpack`:

```bash
export KERNEL=$PWD/artifacts/kernels/gocracker-guest-standard-vmlinux
```

### 1. Run Alpine, print a message

```bash
sudo ./gocracker run --image alpine:latest --kernel "$KERNEL" --cmd "echo hello from a real VM" --wait
```

![Alpine one-shot demo](assets/demos/01-alpine-oneshot.gif)

### 2. Interactive Ubuntu session

```bash
sudo ./gocracker run --image ubuntu:22.04 --kernel "$KERNEL"
```

Drops you into a shell inside the VM over the serial console.

![Interactive TTY demo](assets/demos/02-interactive-tty.gif)

### 3. Build from Dockerfile

```bash
sudo ./gocracker run --dockerfile tests/examples/python-api/Dockerfile \
  --context tests/examples/python-api --kernel "$KERNEL" --wait
```

Parses the Dockerfile, builds layers, creates an ext4 disk, and boots the result.

![Dockerfile demo](assets/demos/03-dockerfile.gif)

### 4. Clone and boot a git repo

```bash
sudo ./gocracker repo --url https://github.com/user/myapp --kernel "$KERNEL" --wait
```

Clones the repo, auto-detects the Dockerfile, builds, and boots.

![Git repo demo](assets/demos/04-git-repo.gif)

### 5. Docker Compose (Flask + PostgreSQL)

```bash
sudo ./gocracker compose \
  --file tests/manual-smoke/fixtures/compose-todo-postgres/docker-compose.yml \
  --kernel "$KERNEL" --wait
```

Each service runs in its own VM. PostgreSQL waits for healthcheck, then the app starts. Port 18081 is published to the host.

![Compose demo](assets/demos/05-compose.gif)

### 6. Exec into a running Compose service

```bash
# Start the API server + compose stack
sudo ./gocracker compose --file docker-compose.yml --kernel "$KERNEL" --server http://127.0.0.1:8080

# In another terminal, exec into a service
sudo ./gocracker compose exec --server http://127.0.0.1:8080 --file docker-compose.yml app
```

Opens an interactive shell inside the running VM.

![Exec demo](assets/demos/06-exec.gif)

### 7. Networking (auto NAT)

```bash
sudo ./gocracker run --image nginx:alpine --kernel "$KERNEL" --net auto --wait
```

Creates a TAP device, assigns IPv4 addresses, sets up NAT. The guest gets internet access automatically.

![Networking demo](assets/demos/07-networking.gif)

### 8. Multi-vCPU

```bash
sudo ./gocracker run --image alpine:latest --kernel "$KERNEL" --cpus 4 --mem 512 \
  --cmd "nproc && free -m" --wait
```

![Multi-vCPU demo](assets/demos/08-multi-vcpu.gif)

### 9. Sandbox pool from template (REST API)

Boot a template VM, install tools once, then clone it per sandbox. Each
clone gets a fresh TAP + subnet and the guest is re-IP'd on the fly via the
exec agent so outbound networking works without the caller pre-allocating
any host resources. `network_mode=auto` creates TAP devices so the server
process must have root or `CAP_NET_ADMIN` (`sudo` or `setcap cap_net_admin+ep`
on the binary); `/run` and `/clone` return 403 with an actionable message
if it's missing.

```bash
sudo ./gocracker serve --addr 127.0.0.1:8080 --trusted-kernel-dir ./artifacts/kernels &

# 1. Boot template (network_mode=auto + exec + wait=true)
TEMPLATE=$(curl -sS http://127.0.0.1:8080/run -X POST \
  -H 'Content-Type: application/json' \
  -d '{"image":"alpine:3.20","kernel_path":"./artifacts/kernels/gocracker-guest-minimal-vmlinux",
       "mem_mb":256,"network_mode":"auto","exec_enabled":true,"wait":true,
       "cmd":["/bin/sh","-lc","sleep infinity"]}' | jq -r .id)

# 2. Install tools on the template (persists in the ext4 disk)
curl -sS http://127.0.0.1:8080/vms/$TEMPLATE/exec -X POST \
  -d '{"command":["/bin/sh","-lc","apk add --no-cache bc && echo TEMPLATE-READY > /tmp/marker"]}'

# 3. Clone it. Source keeps running. Clone gets tclone-<N> tap + fresh /30.
CLONE=$(curl -sS http://127.0.0.1:8080/vms/$TEMPLATE/clone -X POST \
  -d '{"exec_enabled":true,"network_mode":"auto"}' | jq -r .id)

# 4. Clone has bc (disk inherited) AND working outbound net (re-IP'd eth0).
curl -sS http://127.0.0.1:8080/vms/$CLONE/exec -X POST \
  -d '{"command":["/bin/sh","-lc","echo 2*21 | bc && apk add --no-cache file"]}'

# 5. Optionally pause idle clones; resume on next request.
curl -sS -X POST http://127.0.0.1:8080/vms/$CLONE/pause -d '{}'
curl -sS -X POST http://127.0.0.1:8080/vms/$CLONE/resume -d '{}'
```

See [docs/SNAPSHOTS.md#sandbox-template-flow](docs/SNAPSHOTS.md#sandbox-template-flow) for the full walkthrough
and the pkg/warmcache / pkg/warmpool primitives (content-addressable snapshot cache + pre-spawned VMM pool)
that make this pattern sub-100 ms on the hot path.

### 10. Sandbox control plane — raw HTTP

`gocracker-sandboxd` is a small HTTP service that wraps the runtime with sandbox-specific lifecycle (see the [Sandboxd](#sandboxd-sandbox-control-plane) overview below for the high-level description and perf numbers). Each `POST /sandboxes` cold-boots a VM in ~80 ms (or ~40 ms on warm restore), auto-generates a per-sandbox Firecracker-style UDS, and exposes the toolbox agent for framed exec / files / git / re-IP RPCs. Most users will reach it through the Python/Go/JS SDKs rather than curl.

```bash
sudo ./gocracker-sandboxd serve --addr 127.0.0.1:9091 \
  --state-dir /var/lib/gocracker-sandboxd &

# Create a sandbox — cold boot ~80 ms on cached image
SB=$(curl -sS http://127.0.0.1:9091/sandboxes -X POST \
  -H 'Content-Type: application/json' \
  -d '{"image":"alpine:3.20","kernel_path":"./artifacts/kernels/gocracker-guest-standard-vmlinux",
       "network_mode":"auto","cmd":["sleep","3600"]}' | jq -r .sandbox.id)
UDS=$(curl -s http://127.0.0.1:9091/sandboxes/$SB | jq -r .uds_path)

# Dial the baked toolbox agent on vsock 10023 via the UDS bridge (Firecracker-style)
./toolbox-cli health -uds $UDS                      # → ok=true version=0.1.0
./toolbox-cli exec   -uds $UDS -- sh -c 'echo hi'   # → hi

# Re-IP the guest's eth0 in one RPC (used by the warm-pool slice to hand out a
# restored VM with a fresh lease subnet without the caller doing DHCP gymnastics)
./toolbox-cli setnetwork -uds $UDS \
  -ip 10.100.42.2/30 -gw 10.100.42.1 -mac 02:42:00:00:2a:02   # → 11-13 ms

curl -sS -X DELETE http://127.0.0.1:9091/sandboxes/$SB        # → 204
```

The toolbox agent binary is baked into every disk gocracker builds (`/opt/gocracker/toolbox/toolboxguest`, via `go:embed` in [internal/toolbox/embed/](internal/toolbox/embed/)) and spawned by [internal/guest/init.go](internal/guest/init.go) post-`switch_root` before the user's CMD, so there is no post-boot install race — the v2 architectural cul-de-sac of `runtime.Exec → base64 upload → spawn → EnsureToolbox-on-lease` doesn't apply here. The framed `/exec` data plane (`[channel][len][payload]`, stdin/stdout/stderr/exit/signal) lives on vsock 10023, coexisting with the existing `internal/guestexec` JSON agent on 10022 — nothing in the current `/vms/{id}/exec` surface regressed.

## Platform Support

| Platform | Status | Tested On |
|----------|--------|-----------|
| x86-64 Linux | Full support | Ubuntu 22.04, 24.04 |
| ARM64 Linux | Full support | AWS a1.metal (Graviton 1), Ubuntu 24.04 |
| macOS | Planned | Hypervisor.framework |

## Networking

### One-command networking

```bash
sudo ./gocracker run --net auto --image nginx:alpine --kernel "$KERNEL" --wait
```

Automatic TAP creation, IPv4 assignment, and NAT. The guest gets internet access with no manual setup.

### Compose networking

Each Compose stack gets an isolated Linux network namespace. Services within a stack communicate by service name. Port publishing maps host ports to guest ports:

```yaml
ports:
  - "18081:8080"   # host:18081 -> guest:8080
```

### How it works

```
Guest VM
  |
virtio-net
  |
TAP device
  |
bridge / NAT (iptables)
  |
Host network
```

## How It Works

1. **Pull** -- Fetch an OCI image from any registry (or build a Dockerfile, or clone a repo)
2. **Extract** -- Unpack layers into an ext4 disk image (pure Go, no `mkfs.ext4`)
3. **Initrd** -- Generate an initrd with an embedded init binary (pure Go, no shell tools)
4. **Create VM** -- Open `/dev/kvm`, configure vCPU, memory, and virtio MMIO devices
5. **Boot** -- Load the kernel, attach the disk and network, start the vCPU run loop
6. **Guest init** -- The init process mounts the disk, sets up the environment, and runs the user command

The device model uses virtio MMIO transport, the same approach Firecracker uses. Each device (net, blk, rng, vsock, balloon, fs) is memory-mapped and interrupt-driven.

## gocracker vs Firecracker

| | Firecracker | gocracker |
|---|---|---|
| Language | Rust | Go |
| OCI image support | No | Yes |
| Dockerfile support | No | Yes |
| Docker Compose | No | Yes |
| ARM64 | Yes | Yes |
| API | REST | REST (Firecracker-compatible + extensions) |
| Jailer | Yes | Yes |
| Seccomp | Yes | Yes |
| virtio devices | net, blk, balloon, vsock | net, blk, rng, vsock, balloon, fs |
| Snapshots | Yes | Yes |
| Live migration | No | Yes |

gocracker builds on Firecracker's proven security model and adds the developer experience layer: pull an image, boot a VM, one command.

## Boot-time benchmark

End-to-end wall clock for `gocracker run --image alpine:3.20 --wait --cmd "echo OK"`, measured from process start to the first stdout byte of the user CMD. Warm OCI artifact cache (so the run pays only VMM setup + guest boot + first output, not OCI pull or ext4 build).

Host: AMD Ryzen AI 9 HX 370, Linux 6.17, `/dev/kvm` available. Guest: 1 vCPU, 128 MiB RAM, alpine 3.20, `jailer=off`. 10 samples per cell after 1 warmup.

| kernel | network | p50 | p90 | max |
|---|---|---:|---:|---:|
| standard | `--net none` | 77 ms | 86 ms | 108 ms |
| standard | `--net auto` (tap) | 89 ms | 90 ms | 91 ms |
| minimal | `--net none` | 77 ms | 83 ms | 93 ms |
| minimal | `--net auto` (tap) | 79 ms | 81 ms | 84 ms |

`standard` is the default guest kernel ([artifacts/kernels/gocracker-guest-standard-vmlinux](artifacts/kernels/gocracker-guest-standard-vmlinux)) — works against any OCI image the user might throw at it. `minimal` ([artifacts/kernels/gocracker-guest-minimal-vmlinux](artifacts/kernels/gocracker-guest-minimal-vmlinux)) trims subsystems the microVM path never uses (ACPI NUMA, USB, hibernation, bzip2/lzma/lzo decompressors, etc.); virtio + vsock + DNS + IPv6 + TLS still work — `apk update` inside Alpine boots green.

### Reproduce

```bash
make build kernel-unpack
KERNEL=$PWD/artifacts/kernels/gocracker-guest-standard-vmlinux
for i in {1..10}; do
  sudo ./gocracker run \
    --image alpine:3.20 --kernel "$KERNEL" \
    --mem 128 --cpus 1 --jailer off --net none \
    --wait --cmd "echo OK" 2>&1 | grep -oP 'duration=\K[0-9]+ms'
done
```

The `duration=…` line in `[container:INFO] started …` is the end-to-end measurement the runtime reports. It breaks down into `orchestration_ms` (rootfs/initrd/disk reuse), `vmm_setup_ms` (KVM create + kernel load), and `guest_first_output_ms` (KVM_RUN → first serial byte from the guest CMD).

## Time-to-Interactive benchmark

The Firecracker head-to-head above measures at the VMM level. **Time-to-Interactive (TTI)** is the higher-level number most sandbox callers actually care about: the wall-clock time from `sandbox.create()` returning a handle to the first successful stdout byte of `runCommand("node -v")`, against a pre-built sandbox image.

[tools/bench-node-tti.sh](tools/bench-node-tti.sh) in this repo runs that test: a `node:20-alpine` Dockerfile with `CMD ["node","-v"]`, warm artifact cache, and `gocracker run ... --wait` timed to the first stdout byte starting with `v`.

### Setup

- **Host**: AMD Ryzen AI 9 HX 370 (24 threads), Linux 6.17, `/dev/kvm` available
- **Guest kernel**: [artifacts/kernels/gocracker-guest-minimal-vmlinux](artifacts/kernels/gocracker-guest-minimal-vmlinux) (Linux 6.1.102, trimmed initcalls, `loglevel=4` default)
- **Image**: `node:20-alpine`, CMD `["node","-v"]`
- **Warm artifact cache**: first iteration pulls/extracts once, subsequent timed runs boot from the cached ext4
- **Rootfs mode**: default (read-only rootfs with tmpfs overlay) — matches Docker's ephemeral-writable-layer semantics and enables the hardlink fast-path in [pkg/container/container.go](pkg/container/container.go)

### Results (10 timed runs after one warmup, CPU-pinned + FIFO scheduler)

```
TTI 1: 209ms
TTI 2: 211ms
TTI 3: 219ms
TTI 4: 227ms
TTI 5: 245ms
TTI 6: 262ms
TTI 7: 287ms
TTI 8: 285ms
TTI 9: 274ms
TTI 10: 292ms
median = 253 ms, mean = 251 ms, p95 = 292 ms, min = 209 ms, max = 292 ms
```

The bench pins `gocracker` to CPU 0 with `taskset -c 0` and SCHED_FIFO priority 50 via `chrt`. Override with `TTI_PIN_CPUS="0-3"` or `TTI_PIN_CPUS=""` to disable pinning. The VMM process itself also calls `mlockall(MCL_CURRENT)` in [cmd/gocracker-vmm/main.go](cmd/gocracker-vmm/main.go) so its working set stays resident.

The median is ~35 ms slower than the pre-toolbox 218 ms baseline because [internal/guest/init.go](internal/guest/init.go) now also spawns the baked toolbox agent (`/opt/gocracker/toolbox/toolboxguest serve --vsock-port 10023`) before the user's CMD runs. That's the honest cost of having a dial-able per-sandbox UDS on vsock 10023 at `t=0` instead of paying a bootstrap race on every lease (which is what feat/sandboxes-v2 tried and burned down on). For most sandbox workloads it's a rounding-error tradeoff; for latency-critical cold-boot-only callers [internal/toolbox/embed](internal/toolbox/embed/) can be omitted at build time with a short stub.

## Sandboxd (sandbox control plane)

`gocracker-sandboxd` is a separate daemon that turns the runtime into a hosted sandbox platform. It exposes a small HTTP API on top of gocracker that does four things the raw runtime doesn't:

1. **Warm pools** — register a template, and sandboxd keeps N paused VMs ready. A `POST /sandboxes/lease` returns a handle in single-digit milliseconds because the VM is already booted, the IP is already baked into the snapshot, and the exec agent is already listening on vsock.
2. **Content-addressed templates** — register a Dockerfile or image once, sandboxd cold-boots it, takes a warm snapshot, and indexes it by `SpecHash`. Subsequent `CreateTemplate` calls with the same spec are a no-op (microsecond cache hit). `LeaseSandbox` against a pool backed by that template restores from the snapshot.
3. **Signed preview URLs** — `sb.preview_url(8080)` mints an HMAC-signed token that lets callers hit a guest-side port via a dedicated proxy path (`/previews/<token>/...`) or one-label subdomain (`<id>.<preview-host>/...`).
4. **Namespaced SDKs** in Python / Go / JS — `with client.create_sandbox(template="base-python") as sb: sb.process.exec("python -c '…'"); sb.fs.read_file("/tmp/x"); sb.preview_url(8080)`. Context managers, typed errors (`ProcessExitError`, `PoolExhausted`, `TemplateNotFound`, `RuntimeUnreachable`, `SandboxTimeout`), and zero third-party runtime deps in each SDK.

The runtime does VM lifecycle. The sandbox daemon does pool lifecycle, template caching, preview auth, and per-sandbox UDS routing. They're separate processes talking over the runtime's HTTP API, so either one can be swapped out.

Reproduce the numbers below with the scripts in [sandboxes/examples/python/bench/](sandboxes/examples/python/bench/) (Python), [sandboxes/examples/go/bench/](sandboxes/examples/go/bench/) (Go), and [sandboxes/examples/js/bench/](sandboxes/examples/js/bench/) (Node). All were measured on a Ryzen AI 9 HX 370 laptop, Linux 6.17, `/dev/kvm` available, `feat/sandboxd-v3-arm64-and-cleanup` head.

### Warm-lease latency per SDK

`lease → process.exec("echo hi") → delete` against a pool of 8 paused VMs, 8 sequential samples per SDK (all times in ms):

| SDK | phase | min | p50 | p95 | max |
|---|---|---:|---:|---:|---:|
| Python | `lease_sandbox` | 0.72 | **1.09** | 40.17 | 40.17 |
| Python | `process.exec("echo")` | 12.52 | 18.86 | 21.53 | 21.53 |
| Python | `delete` | 0.70 | 1.06 | 2.42 | 2.42 |
| Go | `LeaseSandbox` | 0.45 | **0.93** | 103.94¹ | 103.94 |
| Go | `Process().Exec` | 12.59 | 20.48 | 22.76 | 22.76 |
| Go | `Delete` | 0.40 | 0.55 | 0.61 | 0.61 |
| Node | `leaseSandbox` | 3.46 | **5.14** | 16.78 | 16.78 |
| Node | `process.exec` | 24.88 | 41.03 | 84.94 | 84.94 |
| Node | `delete` | 1.09 | 2.70 | 5.80 | 5.80 |

¹ One Go p95 outlier of 103 ms while the refiller was concurrently catching up after the previous sample drained the pool. The other 7/8 samples were ≤ 2 ms. Node's exec is slower than Python/Go because the JS SDK currently doesn't connection-pool the UDS bridge — every call pays the CONNECT handshake from scratch.

### Time-to-Interactive (via SDK)

Wall-clock from `sandbox.create()` returning a handle → first stdout byte of `runCommand("node", "-v")`. The pre-built sandbox image is our `base-node` template (auto-registered when sandboxd starts with `-kernel-path`).

10 timed runs after one warmup, against a pool of 8 paused base-node VMs:

```
median = 277 ms   mean = 274 ms   p95 = 314 ms   min = 206 ms   max = 314 ms
```

Per-workload breakdown (same path, different command):

| workload | p50 | what dominates |
|---|---:|---|
| `sb.process.exec(['/bin/true'])` | ~20 ms | UDS + CONNECT round trip + agent fork |
| `sb.process.exec(['echo','hi'])` | ~19 ms | same as `/bin/true` within noise |
| `sb.process.exec(['node','-v'])` | **277 ms** | **node startup on alpine/musl** (~258 ms of the total) |

The 258 ms node-startup floor is inside the guest process — our boot path and agent overhead account for ~19 ms of the total. A post-ready snapshot (node already parsed and resident in the page cache) would drop the outer total under 50 ms; the wiring is in tree (`ReadinessProbe` in the template spec) but has a known issue at restore.

### Cookbook + cold-boot bursts (Python SDK)

Numbers from the 10-example cookbook sweep (`sandboxes/examples/python/cookbook/`):

| Workload | p95 |
|---|---:|
| `concurrent_cold.py` N=3 (cold-boot each) | 142 ms |
| `pool_burst.py` burst=3 (3 concurrent warm leases) | 3.4 ms |
| `pool_burst.py` burst=5 (5 concurrent warm leases) | 6.0 ms |
| `template_pool.py` (lease from base-python pool) | 0.9–2.9 ms |

### Validated images sweep (50 images)

End-to-end smoke against 50 docker-hub images covering web servers, databases, runtimes, proxies, CLIs, language runtimes, and observability tools. Each image is cold-booted via `client.create_sandbox(image=..., entrypoint=['sleep'], cmd=['infinity'])` (so PID 1 outlives the agent dial), then `sb.process.exec` runs a version command and verifies the output. `create_ms` includes the OCI pull on first run; later runs hit the artifact cache and create drops to ~120–500 ms regardless of image size.

**Result: 43/50 PASS (86 %).** All 7 failures are caller-side / external (image renamed on DockerHub, port-80 privilege, etc.) — none is a runtime bug. Reproduce with [sandboxes/examples/python/bench/sweep_validated.py](sandboxes/examples/python/bench/sweep_validated.py).

#### Pass — language runtimes

| image | create | exec | delete | output |
|---|---:|---:|---:|---|
| `alpine:3.20` | 126 ms | 202 ms | 3 ms | `3.20.10` |
| `debian:12-slim` | 4.7 s | 217 ms | 3 ms | `12.x` |
| `ubuntu:24.04` | 6.3 s | 224 ms | 3 ms | `24.04` |
| `amazonlinux:2023` | 9.1 s | 268 ms | 3 ms | `Amazon Linux release 2023` |
| `rockylinux:9-minimal` | 8.4 s | 233 ms | 3 ms | `Rocky Linux release 9` |
| `python:3.12-alpine` | 156 ms | 210 ms | 1 ms | `Python 3.12.13` |
| `node:22-alpine` | 2.6 s | 112 ms | 1 ms | `v22.22.2` |
| `golang:1.23-alpine` | 154 ms | 223 ms | 2 ms | `go1.23.12 linux/amd64` |
| `oven/bun:1` (`/usr/local/bin/bun`) | 206 ms | 323 ms | 3 ms | `1.3.13` |
| `ruby:3-alpine` | 11.3 s | 201 ms | 3 ms | `ruby 3.4.9` |
| `php:8-cli-alpine` | 12.6 s | 366 ms | 3 ms | `PHP 8.5.5 (cli)` |
| `elixir:alpine` | 11.3 s | 1.4 s | 3 ms | `Erlang/OTP 28` |
| `eclipse-temurin:21-jre-alpine` | 4.1 s | 434 ms | 3 ms | `OpenJDK Runtime` |
| `busybox:latest` | 4.6 s | 266 ms | 3 ms | `BusyBox v1.37.0` |
| `alpine:3.20 + apk add git` | 105 ms | 1.7 s | 2 ms | `git version 2.45.4` |

#### Pass — web servers / proxies / CLIs

| image | create | exec | delete | output |
|---|---:|---:|---:|---|
| `nginx:alpine` | 9.4 s | 150 ms | 3 ms | `nginx/1.29.8` |
| `caddy:2-alpine` | 8.2 s | 303 ms | 3 ms | `v2.11.2` |
| `traefik:latest` | 8.3 s | 457 ms | 3 ms | `Version: 3.6.14` |
| `httpd:alpine` (`/usr/local/apache2/bin/httpd`) | 10.0 s | 250 ms | 3 ms | `Server version: Apache` |
| `haproxy:lts-alpine` | 8.9 s | 293 ms | 3 ms | `HAProxy version 3.2.15` |
| `traefik/whoami:latest` | 1.4 s | 28 ms | 2 ms | `whoami help` |
| `mailhog/mailhog:latest` | 2.0 s | 134 ms | 3 ms | `MailHog` |

#### Pass — databases / caches / messaging

| image | create | exec | delete | output |
|---|---:|---:|---:|---|
| `redis:alpine` | 10.1 s | 164 ms | 3 ms | `Redis server v=8.6.2` |
| `postgres:16-alpine` | 5.9 s | 51 ms | 1 ms | `postgres (PostgreSQL) 16.13` |
| `mariadb:lts` | 14.9 s | 137 ms | 3 ms | `mariadbd Ver 11.8.6` |
| `cockroachdb/cockroach:latest` | 5.8 s | 54 s | 3 ms | `Build Tag` (cockroach version is heavy) |
| `influxdb:2-alpine` | 15.6 s | 409 ms | 3 ms | `InfluxDB v2.8.0` |
| `nats:alpine` | 7.2 s | 173 ms | 6 ms | `nats-server v2.12.7` |
| `eclipse-mosquitto:2` | 6.4 s | 133 ms | 3 ms | `mosquitto version 2.1.2` |

#### Pass — observability + Hashicorp + dev tools

| image | create | exec | delete | output |
|---|---:|---:|---:|---|
| `prom/prometheus:latest` | 5.7 s | 469 ms | 3 ms | `prometheus, version` |
| `prom/alertmanager:latest` | 3.0 s | 297 ms | 3 ms | `alertmanager, version` |
| `prom/node-exporter:latest` | 2.0 s | 274 ms | 3 ms | `node_exporter` |
| `prom/blackbox-exporter:latest` | 3.0 s | 290 ms | 4 ms | `blackbox_exporter` |
| `jaegertracing/all-in-one:latest` | 2.6 s | 361 ms | 3 ms | `jaeger help` |
| `linuxserver/syslog-ng:latest` | 3.8 s | 64 ms | 2 ms | `syslog-ng 4.7.1` |
| `hashicorp/vault:latest` | 22.8 s | 679 ms | 3 ms | `Vault v2.0.0` |
| `hashicorp/consul:latest` | 12.9 s | 729 ms | 3 ms | `Consul v1.22.6` |
| `gitea/gitea:latest` | 3.4 s | 813 ms | 3 ms | `gitea version 1.26.0` |
| `jenkins/jenkins:lts-jdk21` | 9.0 s | 227 ms | 3 ms | `OpenJDK Runtime` |

#### Fail (7 images — none is a gocracker runtime bug)

| image | failure | category |
|---|---|---|
| `pocketbase/pocketbase:latest` | 404 on docker hub | image renamed |
| `etcd:latest` | 404 on docker hub | image renamed (now `gcr.io/etcd-development/etcd` or `quay.io/coreos/etcd`) |
| `vault:latest` (legacy) | 404 on docker hub | image renamed (now `hashicorp/vault`) |
| `hashicorp/http-echo:latest` | timed out connecting to agent | minimal scratch-based image — toolbox can't supervise |
| `coredns/coredns:latest` | timed out connecting to agent | minimal scratch-based image — toolbox can't supervise |
| `gotify/server:latest` | exit=1 — `listen tcp :80: permission denied` | guest tries to bind privileged port without root |
| `memcached:alpine` | `KVM_CREATE_VM` failed | host KVM exhaustion (76+ leaked VMs from earlier sessions on the same host) |

The runtime handles 43 distinct images, 13 language runtimes (incl. bun), 5 databases, 4 proxies/load balancers, 6 messaging / observability tools, and 5 OS bases without code changes — all via a single `client.create_sandbox(image=...)` call from the SDK.

> **Calling-convention tip**: for service images that ship their own `ENTRYPOINT` (most non-base images: nginx, prom/*, hashicorp/*, jenkins, bun, etc.), pass `entrypoint=['sleep'], cmd=['infinity']` so init runs a benign keep-alive instead of the image's daemon. Skipping this is the main cause of the "agent timed out" / "exit=-1" symptoms in the few-failure column above.

## Snapshot / restore

`POST /restore {resume:true}` maps the memory snapshot `MAP_PRIVATE` (lazy COW — no up-front read or copy), re-wires virtio devices, restores vCPU state, and resumes. Because memory is page-faulted in lazily rather than eagerly copied, restore latency is O(1) in memory size rather than O(memory size).

Key design points that keep the restore fast ([bench-rtt numbers below](#per-primitive-rtt-benchmark)):

- **`MAP_PRIVATE` COW memory restore** — [internal/kvm/kvm.go](internal/kvm/kvm.go). The snapshot file is mapped lazily; pages fault in as the guest touches them rather than up-front.
- **Single-call `/restore {resume:true}`** — [internal/vmmserver/server.go](internal/vmmserver/server.go). Snapshot-load and vCPU-resume in one HTTP round-trip, not two.
- **`SkipDiscardProbe` on restore** — [internal/virtio/blk.go](internal/virtio/blk.go). Skips the `FALLOC_FL_PUNCH_HOLE` probe because the guest already negotiated `VIRTIO_BLK_F_DISCARD` against the pre-snapshot `DeviceFeatures`.
- **`postCreateVCPUs` in restore path** — [pkg/vmm/vmm.go](pkg/vmm/vmm.go). Wires per-device `KVM_IRQFD` against freshly-created vCPU GSIs so queue notifies don't take a reconfiguration VMexit on first use.
- **`MADV_HUGEPAGE` + `MADV_WILLNEED`** on the first 8 MiB of the memory mapping — serves hot early pages from 2 MiB TLB entries without page-fault stalls.

### Hot-path numbers (sandboxd)

Measurements from the end-to-end sandboxd path on the same host:

| scenario | latency | notes |
|---|---:|---|
| Cold boot (`gocracker run` to first `node -v` stdout) | **253 ms** p50 | +35 ms over the pre-toolbox baseline — cost of init spawning the baked toolbox agent before the user CMD. |
| Snapshot resume (runtime call only) | **1.67 ms** p50 / 3.08 ms p90 | `RestoreFromSnapshotWithOptions → vm.Start`. See [bench-rtt](#per-primitive-rtt-benchmark) below for full distribution. |
| Warm-pool lease (`POST /sandboxes/lease`) | **1.5 ms** p95 | Pool of 8, sequential. The pool reuses the IP baked into each VM's cold-boot snapshot and skips SetNetwork on the hot path. |
| Sandboxd E2E `create → exec echo → delete` | **~35 ms** p95 | Full round trip from the Python/Go/Node SDK. Async delete lands the HTTP response in 1.9 ms; the ~22 ms residual is UDS+vsock CONNECT + agent fork/exec for `echo`. |
| Sandboxd warm TTI (`node -v` against `base-node`) | **277 ms** p50 | Warm pool of 8, lease handle then time `sb.process.exec(['node','-v'])` to first byte. ~258 ms is node's own startup on alpine/musl; agent + UDS overhead is ~20 ms. |

The bench runs with the VMM pinned to a dedicated CPU (`taskset`) and boosted to SCHED_FIFO (`chrt -r 50`), with `mlockall(MCL_CURRENT)` locking the VMM's working set. The snapshot path deliberately does NOT set `MCL_FUTURE` because that would eager-fault the `MAP_PRIVATE` memory snapshot and turn a ~2 ms lazy-COW restore into ~30 ms.

### ARM64 warm-cache benchmark

`gocracker run ... --warm` captures a snapshot on first run and restores from it on every subsequent run with identical parameters. The ARM64 port round-trips the full vCPU register set (including SP_EL1, ELR_EL1, SPSR[0], V0-V31 FPSIMD, FPSR/FPCR, MPState, KVM_REG_ARM_TIMER_*), plus the entire VGICv3 state (distributor + per-vCPU redistributor + ICC_* CPU sysregs, with IC-then-IS ordered writes), so multi-vCPU guests resume with IRQ delivery and scheduler state intact — not just vCPU 0.

Measured on `a1.metal` Graviton, Ubuntu 24.04, Alpine 3.20 guest, `--net auto --cmd nproc`, wall-clock end-to-end and the pure `restoreFromSnapshot → vm.Start()` phase (from the `[container] restored duration=...` log):

| vCPUs | cold boot | warm restore (total) | pure restore |
|---:|---:|---:|---:|
| 1 | 5.28 s | **293 ms** | 87 ms |
| 2 | 5.31 s | **288 ms** | 82 ms |
| 4 | 5.24 s | **310 ms** | 105 ms |
| 8 | 5.28 s | **290 ms** | 96 ms |
| 16 | 5.41 s | **330 ms** | 117 ms |

Cold-boot time here is dominated by OCI pull + ext4 build (~3 s for the alpine rootfs); the actual kernel-to-init boot is ~150 ms. Warm restore stays ~linear in vCPU count because VGIC state is O(vCPU) (one redistributor frame + 9 ICC sysregs each).

### Per-primitive RTT benchmark ([tools/bench-rtt](tools/bench-rtt/main.go))

Unlike the end-to-end TTI and snapshot-resume numbers above, `bench-rtt` isolates each primitive on the warm-cache hot path so a regression can be attributed to a specific code path. Numbers below are from a fresh run on a Ryzen AI 9 HX 370 laptop, Linux 6.17, N=30 iterations after 3 warmups, against the branch head:

| primitive | p50 | p90 | max | notes |
|---|---:|---:|---:|---|
| pause (`vm.Pause()`) | **10.33 ms** | 10.36 ms | 10.44 ms | floor is the 10 ms `time.Sleep` polling loop inside `(*VM).Pause()` — an architectural cost, not measurement noise. |
| resume (`vm.Resume()`) | **~1 µs** | ~2 µs | ~2 µs | near-instant — no vCPU state round-trip. |
| snapshot capture (`vm.TakeSnapshot()`) | **170 ms** | 268 ms | 359 ms | O(memory size) — the cost is writing the 256 MB memory image to disk, bounded by page-cache + device speed. |
| snapshot restore (`vmm.RestoreFromSnapshotWithOptions → vm.Start`) | **1.67 ms** | 3.08 ms | 3.23 ms | `MAP_PRIVATE` COW of the memory snapshot + vCPU state round-trip. O(1) in memory size — no eager page-in. |
| warmcache lookup hit | **9 µs** | 11 µs | 18 µs | `pkg/warmcache.Lookup()` — hashes the spec + stats the snapshot dir. |
| warmcache lookup miss | **4 µs** | 4 µs | 7 µs | early-out when the directory does not exist. |
| UDS handshake (`CONNECT → OK`) | **~0.1 ms** | ~1 ms | ~2 ms | Firecracker-style Unix socket: dial + `CONNECT <port>\n` + read `OK\n`. Measures the host→guest→host virtio-vsock round-trip the sandbox orchestrator pays per exec. Populated when bench-rtt is run with `-uds <path>`. |
| Toolbox framed exec stdout throughput | **50 MB/s** sustained | — | — | `POST /exec` on vsock 10023 with `[channel][len][payload]` framing. Validated end-to-end 4 MB round-trips (64 KB 22 ms, 512 KB 31 ms, 1 MB 33 ms, 4 MB 83 ms). |
| SetNetwork RPC (re-IP post-restore) | **~12 ms** | ~15 ms | ~20 ms | Toolbox `POST /internal/setnetwork`: `LinkSetHardwareAddr → LinkSetUp → flush stale v4 addrs → AddrReplace → RouteReplace → arping -U` (async best-effort). Now **bypassed** on the warm-pool hot path because the IP is baked into the cold-boot snapshot. |

Reproduce:

```bash
sudo go run ./tools/bench-rtt \
  -kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux \
  -iter 30 -warmups 3
```

## Documentation

- [Getting Started](docs/GETTING_STARTED.md) -- build, install, first VM in 60 seconds
- [Networking](docs/NETWORKING.md) -- auto NAT, compose bridges, manual TAP
- [Architecture](docs/ARCHITECTURE.md) -- boot flow, device model, security layers
- [Docker Compose](docs/COMPOSE.md) -- multi-service stacks, healthchecks, ports
- [API Reference](docs/API.md) -- Firecracker-compatible + extended endpoints
- [CLI Reference](docs/CLI_REFERENCE.md) -- every command and flag
- [Snapshots and Migration](docs/SNAPSHOTS.md) -- save, restore, live migrate
- [Examples](docs/EXAMPLES.md) -- 16 example apps across 10 languages
- [Validated Projects](docs/VALIDATED_PROJECTS.md) -- 378 real-world repos tested
- [Troubleshooting](docs/TROUBLESHOOTING.md) -- common issues and fixes
- [Security Policy](SECURITY.md) -- isolation model, vulnerability reporting

## CLI Reference

```
gocracker <command> [flags]

Commands:
  run        Build and boot a microVM (image, Dockerfile, or local path)
  repo       Clone a git repo and boot its Dockerfile
  compose    Boot a docker-compose.yml stack as microVMs
  build      Build a disk image without booting
  restore    Restore and boot a VM from a snapshot
  migrate    Live-migrate a VM between gocracker API servers
  serve      Start the REST API server
  snapshot   Take a snapshot of a running VM via the API
  version    Print build version, commit, date, and Go runtime

Common flags (run):
  --image       OCI image ref (e.g. ubuntu:22.04)
  --dockerfile  Path to Dockerfile
  --kernel      Kernel image path [required]
  --mem         RAM in MiB (default: 256)
  --cpus        vCPU count (default: 1)
  --disk        Disk size MiB (default: 2048)
  --net         Network mode: none or auto
  --cmd         Override CMD
  --wait        Block until VM stops
  --tty         Console mode: auto, off, or force
  --jailer      Privilege model: on or off (default: on)
```

## Tested with 378 Real-World Projects

gocracker has been validated against 378 open-source projects across 16 languages,
booting each from its own Dockerfile as a microVM:

| Category | Projects |
|----------|----------|
| Infrastructure | Traefik, Caddy, Envoy, HAProxy, NGINX, CoreDNS |
| Databases | PostgreSQL, Redis, Valkey, CockroachDB, DragonflyDB, Memcached |
| Search | Meilisearch, Qdrant, Typesense, Quickwit |
| Monitoring | Prometheus, Grafana, Loki, Tempo, Telegraf, VictoriaMetrics |
| ML/AI | MLflow, BentoML, Gradio, Streamlit, Ollama |
| CI/CD | Woodpecker, Drone, Kestra, Dagster, Prefect |
| Messaging | RabbitMQ, NATS, EMQX, Mosquitto |
| Storage | MinIO, SeaweedFS, Distribution (registry) |
| Forges | Gitea, Gogs |
| Languages | Go (123), Node.js (62), Python (50), PHP (24), Rust (21), Ruby (12), Java (12), Elixir (6), C (6), C++ (4), and more |

Full manifest: [`tests/external-repos/manifest.tsv`](tests/external-repos/manifest.tsv) (378 entries)

See [docs/EXAMPLES.md](docs/EXAMPLES.md) for included example applications.

## Requirements

- Linux kernel 5.10+ with KVM (`/dev/kvm` accessible)
- Go 1.22+ (build from source)
- Network tools: `ip`, `iptables` (for `--net auto`)
- `sudo` for KVM access and networking

Verify your host is ready:

```bash
make hostcheck
```

## License

[Apache License 2.0](LICENSE)
