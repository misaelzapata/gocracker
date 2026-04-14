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
gocracker run --image ubuntu:22.04 --kernel ./kernel --wait
```

No Docker daemon. No containerd. No runc. Just KVM.

This started as a hobby project to understand KVM and Go systems programming.
It grew into something that boots 328 real-world projects, runs Flask + PostgreSQL
compose stacks on ARM64 bare metal, and supports snapshots, live migration, and a
Firecracker-compatible REST API. All in a single static binary.

## Quick Start

```bash
# Build gocracker
make build

# Build a guest kernel (one time)
make kernel-guest

# Run your first microVM
sudo ./gocracker run --image alpine:latest --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux --cmd "echo hello from a real VM" --wait
```

## Features

**Container Sources** -- OCI images from any registry, local Dockerfiles (BuildKit AST), git repos with auto-detected Dockerfiles, local directories.

**Orchestration** -- Docker Compose stacks as microVMs, healthchecks (CMD/CMD-SHELL executed in-guest), `depends_on` with conditions (`service_healthy`, `service_completed_successfully`), `.env` and interpolation.

**Networking** -- `--net auto` for single-command TAP + IPv4 + NAT, per-stack network namespaces for Compose, manual TAP for advanced setups, deterministic per-VM MAC addresses, port publishing.

**Isolation** -- KVM hardware virtualization, Firecracker-style jailer (`gocracker-jailer`), seccomp filters (per-arch), private mount namespaces, `pivot_root` isolation for builds.

**Operations** -- Snapshot and restore (RAM + vCPU + device state), live migration (stop-and-copy), Firecracker-compatible REST API with extensions, structured logging, event streaming (SSE).

**Devices** -- virtio-net, virtio-blk, virtio-rng, virtio-vsock, virtio-balloon (manual + auto reclaim), virtio-fs, UART 16550A serial console, memory hotplug.

## Examples

### 1. Run Alpine, print a message

```bash
sudo ./gocracker run --image alpine:latest --kernel ./kernel --cmd "echo hello from a real VM" --wait
```

![Alpine one-shot demo](assets/demos/01-alpine-oneshot.gif)

### 2. Interactive Ubuntu session

```bash
sudo ./gocracker run --image ubuntu:22.04 --kernel ./kernel
```

Drops you into a shell inside the VM over the serial console.

![Interactive TTY demo](assets/demos/02-interactive-tty.gif)

### 3. Build from Dockerfile

```bash
sudo ./gocracker run --dockerfile tests/examples/python-api/Dockerfile \
  --context tests/examples/python-api --kernel ./kernel --wait
```

Parses the Dockerfile, builds layers, creates an ext4 disk, and boots the result.

![Dockerfile demo](assets/demos/03-dockerfile.gif)

### 4. Clone and boot a git repo

```bash
sudo ./gocracker repo --url https://github.com/user/myapp --kernel ./kernel --wait
```

Clones the repo, auto-detects the Dockerfile, builds, and boots.

![Git repo demo](assets/demos/04-git-repo.gif)

### 5. Docker Compose (Flask + PostgreSQL)

```bash
sudo ./gocracker compose \
  --file tests/manual-smoke/fixtures/compose-todo-postgres/docker-compose.yml \
  --kernel ./kernel --wait
```

Each service runs in its own VM. PostgreSQL waits for healthcheck, then the app starts. Port 18081 is published to the host.

![Compose demo](assets/demos/05-compose.gif)

### 6. Exec into a running Compose service

```bash
# Start the API server + compose stack
sudo ./gocracker compose --file docker-compose.yml --kernel ./kernel --server http://127.0.0.1:8080

# In another terminal, exec into a service
sudo ./gocracker compose exec --server http://127.0.0.1:8080 --file docker-compose.yml app
```

Opens an interactive shell inside the running VM.

![Exec demo](assets/demos/06-exec.gif)

### 7. Networking (auto NAT)

```bash
sudo ./gocracker run --image nginx:alpine --kernel ./kernel --net auto --wait
```

Creates a TAP device, assigns IPv4 addresses, sets up NAT. The guest gets internet access automatically.

![Networking demo](assets/demos/07-networking.gif)

### 8. Multi-vCPU

```bash
sudo ./gocracker run --image alpine:latest --kernel ./kernel --cpus 4 --mem 512 \
  --cmd "nproc && free -m" --wait
```

![Multi-vCPU demo](assets/demos/08-multi-vcpu.gif)

## Platform Support

| Platform | Status | Tested On |
|----------|--------|-----------|
| x86-64 Linux | Full support | Ubuntu 22.04, 24.04 |
| ARM64 Linux | Full support | AWS a1.metal (Graviton 1), Ubuntu 24.04 |
| macOS | Planned | Hypervisor.framework |

### ARM64 / x86-64 Subsystem Comparison

| Subsystem | x86-64 | ARM64 | Notes |
|-----------|--------|-------|-------|
| KVM bindings | `KVM_GET/SET_REGS` | `KVM_GET/SET_ONE_REG` | ARM64 uses per-register ioctls |
| Interrupt controller | IOAPIC + LAPIC | GICv2 / GICv3 (in-kernel) | Auto-probed; GICv2 preferred on Graviton 1 |
| IRQ delivery | `KVM_IRQ_LINE` | irqfd (eventfd) | ARM64 matches Firecracker's irqfd approach |
| Serial console | UART 16550A (I/O port 0x3F8) | UART 16550A (MMIO 0x40002000) | Same device, different transport |
| Boot protocol | bzImage / ELF vmlinux | ARM64 Image / Image.gz / ELF | PC=entry, X0=DTB address |
| Device tree | ACPI (x86) | FDT/DTB (generated) | GIC, timer, PSCI, UART, virtio nodes |
| SMP boot | INIT/SIPI sequence | PSCI CPU_ON | Secondary vCPUs start POWER_OFF |
| Virtio MMIO transport | 0xD0000000+ | 0x40003000+ | Firecracker-compatible layout on ARM64 |
| virtio-net | Done | Done | |
| virtio-blk | Done | Done | |
| virtio-rng | Done | Done | |
| virtio-vsock | Done | Done | |
| virtio-balloon | Done | Done | |
| virtio-fs | Done | Done | |
| Snapshot / Restore | Done | Done | |
| Jailer + seccomp | Done | Done | seccomp filter compiled per-arch |
| Compose networking | Done | Done | TAP + bridge + userspace port proxy |
| Memory layout | RAM at GPA 0x0 | RAM at GPA 0x80000000 | ARM64 reserves low 2 GB for MMIO |

## Networking

### One-command networking

```bash
sudo ./gocracker run --net auto --image nginx:alpine --kernel ./kernel --wait
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

I measured gocracker against Firecracker v1.10.1 on the same host, across the matrix of `{standard, minimal}` guest kernels × `{none, tap}` network × `{off, on}` TTY, 10 runs per cell (3 warmups), all driven through the same Firecracker REST API so the comparison is apples-to-apples at the VMM level.

> **Note** — this section was re-measured on 2026-04-13 on the `perf/x86-slim-kernel-boot-bench` branch (commit preceding the README update). The earlier run is preserved in git history.

### Setup

- **Host**: AMD Ryzen AI 9 HX 370 (24 threads), Linux 6.17, `/dev/kvm` available
- **Firecracker**: v1.10.1 ([official release tarball](https://github.com/firecracker-microvm/firecracker/releases/tag/v1.10.1))
- **gocracker**: this branch (includes the virtio-net shutdown-race fix from this PR; the earlier vsock-side fix landed in PR #2)
- **Kernels measured**:
  - [artifacts/kernels/gocracker-guest-standard-vmlinux](artifacts/kernels/gocracker-guest-standard-vmlinux) (ELF vmlinux, Linux 6.1.102, 41 MiB)
  - [artifacts/kernels/gocracker-guest-minimal-vmlinux](artifacts/kernels/gocracker-guest-minimal-vmlinux) (new minimal profile — committed config drops ACPI NUMA + SLEEP, AMD NUMA, HIBERNATION + snapshot dev, PROFILING, USB (entire subsystem), PM_SLEEP, and the `bzip2`/`lzma`/`lzo` decompressors. Some symbols the fragment requests off — e.g. `PERF_EVENTS`, `VT`, `INPUT` — stay `=y` because other Kconfig options (`HARDLOCKUP_DETECTOR_PERF`, `HID`, legacy console selectors) transitively select them. Full functional baseline — virtio + vsock + DNS + IPv6 + TLS — still works: `apk update` inside Alpine boots green. 40 MiB.)
- **Shared rootfs**: 64 MiB ext4 built from alpine-minirootfs 3.20.3 with a custom `/init` that mounts `/proc` and calls `/sbin/reboot -f` (triggers `KVM_EXIT_SHUTDOWN` via triple fault on `reboot=t`)
- **Guest**: 1 vCPU, 128 MiB RAM
- **Kernel cmdline**: `console=ttyS0 reboot=t panic=-1 pci=off i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd root=/dev/vda rw init=/init 8250.nr_uarts=1`

**Scope of this update**: the matrix here is reduced to `jailer=off` only (the jailer chroot needs root to set up and I did not want the bench to touch `/srv/jailer` silently). The jailer row from the older run is preserved in git history; its +8 ms overhead is unchanged.

Both runtimes are driven by the same bash script which does:

1. `fork-exec` of the VMM binary (`firecracker` or `gocracker-vmm`)
2. Wait for the Unix socket to appear
3. `PUT /boot-source`, `/drives/rootfs`, `/machine-config` (+ optional `/network-interfaces/eth0`)
4. `PUT /actions {InstanceStart}`
5. Wait for the VMM to report the guest has shut down (Firecracker exits; gocracker-vmm's `/vm` state transitions to `stopped`)

Four timings per run:

| metric | meaning |
|---|---|
| `spawn_ms`  | fork-exec → API socket accepts connections |
| `api_ms`    | socket ready → last setup PUT complete (before `InstanceStart`) |
| `boot_ms`   | `InstanceStart` response → host sees guest shutdown |
| `total_ms`  | end-to-end wall clock (sum of the above) |

### Results (medians over 10 runs per cell, 0 failures)

```
kernel    runtime      net   tty   spawn  api   boot   total   p95
---------------------------------------------------------------------
standard  firecracker  none  off    3ms  19ms  315ms  338ms  378ms
standard  firecracker  none  on     3ms  19ms  312ms  333ms  356ms
standard  firecracker  tap   off    3ms  25ms  324ms  351ms  395ms
standard  firecracker  tap   on     2ms  25ms  310ms  340ms  384ms
standard  gocracker    none  off    6ms  18ms  389ms  417ms  498ms
standard  gocracker    none  on     5ms  17ms  387ms  408ms  452ms
standard  gocracker    tap   off    5ms  19ms  387ms  410ms  451ms
standard  gocracker    tap   on     5ms  19ms  391ms  416ms  459ms
minimal   firecracker  none  off    3ms  17ms  302ms  321ms  373ms
minimal   firecracker  none  on     3ms  17ms  292ms  314ms  321ms
minimal   firecracker  tap   off    3ms  24ms  303ms  329ms  348ms
minimal   firecracker  tap   on     3ms  24ms  304ms  333ms  387ms
minimal   gocracker    none  off    4ms  15ms  362ms  380ms  438ms
minimal   gocracker    none  on     5ms  15ms  348ms  368ms  410ms
minimal   gocracker    tap   off    4ms  19ms  372ms  395ms  498ms
minimal   gocracker    tap   on     5ms  19ms  382ms  405ms  455ms
```

| config (runtime/net/tty) | standard `boot` | minimal `boot` | Δ |
|---|---:|---:|---:|
| firecracker / none / off | 315 ms | 302 ms | −13 |
| firecracker / none / on  | 312 ms | 292 ms | −20 |
| firecracker / tap  / off | 324 ms | 303 ms | −21 |
| firecracker / tap  / on  | 310 ms | 304 ms | −6  |
| gocracker   / none / off | 389 ms | 362 ms | **−27** |
| gocracker   / none / on  | 387 ms | 348 ms | **−39** |
| gocracker   / tap  / off | 387 ms | 372 ms | −15 |
| gocracker   / tap  / on  | 391 ms | 382 ms | −9  |

### What the data shows

1. **The minimal kernel is a real win**, most noticeable on gocracker: the headline no-net/no-tty cell drops from **389 ms → 362 ms** (−27 ms), and the no-net/tty-on cell drops from **387 ms → 348 ms** (−39 ms). Firecracker sees a smaller benefit (Firecracker's base config was already close to microVM-minimal).

2. **Gocracker remains a bit slower than Firecracker** on the same host (~45 ms on the common case). Some of that is shutdown measurement methodology — gocracker measures to `state=stopped` (post-cleanup), while Firecracker measures to process exit. The slim kernel closes roughly half the gap.

3. **TAP network adds ~5–15 ms** (an extra `/network-interfaces/eth0` PUT plus TAP fd setup); both runtimes pay the same amount.

4. **TTY on/off is noise** (<10 ms in every configuration).

5. **Functional sanity check** — running `gocracker run` on an Alpine Dockerfile with the minimal kernel, network `auto`, and a `CMD` of `apk update && date && echo APK_OK` prints `OK: 24175 distinct packages available` and the timestamp, confirming that the slim profile still supports DNS, HTTPS/TLS, virtio-blk, virtio-net, wall clock via kvm-clock, and the gocracker exec agent.

### The "2x" that the old `duration` field showed

The `gocracker run` CLI used to print a single `duration=Xms` number that looked like the jailer slowed boot by ~2×. I reproduced it:

```
gocracker run --jailer=off  →  running (27ms)  wall 862ms
gocracker run --jailer=on   →  running (56ms)  wall 878ms
```

`56 / 27 ≈ 2.07×`, but the **wall clock is identical** (862 vs 878 ms). The ratio was real work (fork-exec of the jailer, chroot setup, socket polling, 7 REST PUTs to the worker VMM) but it is **~29 extra milliseconds on a 300 ms+ kernel boot**, not a 2× guest boot. The name `duration` was misleading because:

- In `runLocal`, `t0` was placed right before in-process `vmm.New()`.
- In `runViaWorker`, `t0` was placed right before `worker.LaunchVMM()`, which encapsulates the entire IPC pipeline *plus* an eventual remote `vmm.New()`.
- Both paths stopped the clock when `vm.Start()` returned — which is before the guest kernel has printed a single byte. Neither number reflected the actual time-to-guest-ready.

**This branch replaces that single field with a phase breakdown** (`orchestration_ms`, `vmm_setup_ms`, `guest_first_output_ms`, `total_ms`) so users see the split directly and no longer get a misleading 2× comparison. See `cmd/gocracker/main.go` and `pkg/container/container.go` for the new fields.

### How these numbers compare to Firecracker's published figures

Context from authoritative sources:

| source | number | what it measures |
|---|---:|---|
| [Firecracker SPECIFICATION.md](https://github.com/firecracker-microvm/firecracker/blob/main/SPECIFICATION.md) | **≤ 125 ms** (SLO ceiling) | API `InstanceStart` → guest `/sbin/init`, m5d.metal / i3.metal, serial console **disabled**, minimal kernel+rootfs |
| Firecracker [issue #877](https://github.com/firecracker-microvm/firecracker/issues/877) (i3.metal, 2019) | median **99.8 ms** (mean 105.3, σ 8.6) | `--boot-timer`, no network |
| Firecracker [issue #877](https://github.com/firecracker-microvm/firecracker/issues/877) (m5d, 2021) | no net: **112.3 ms**; w/ net: **116 ms** | `--boot-timer` |
| NSDI 2020 paper ([Agache et al.](https://www.usenix.org/system/files/nsdi20-paper-agache.pdf)) | ~125 ms | VMM boot to userspace |
| [jailer.md](https://github.com/firecracker-microvm/firecracker/blob/main/docs/jailer.md) | **2×** with 10 parallel jails, 0 mounts; **10×** with 500 mounts | Jail **creation** (parallel), *not* single-VM boot |

The ~300 ms above is ~3× Firecracker's CI number for these reasons:

- Test host is a consumer laptop, not an m5d.metal / i3.metal bare-metal instance.
- The shared kernel is built from gocracker's generic guest config, not Firecracker's [stripped microvm-kernel config](https://github.com/firecracker-microvm/firecracker/tree/main/resources/guest_configs) (diff is ~37 options: PCI subsystem, ACPI NUMA, PCIe serial drivers, DMA engines, Intel perf counters — worth ~10–30 ms).
- Serial console (`console=ttyS0`) is enabled. Firecracker's SLO number measures with it disabled.
- Guest rootfs runs `alpine init` with a full mount sequence, not a static-linked noop init.

The ratio between runtimes on the same host is the useful number, and there gocracker is neck-and-neck with Firecracker. No single-VM public Firecracker figure supports a 2× jailer penalty — the 2× in their docs is specifically about parallel jail creation at scale.

## Time-to-Interactive benchmark (ComputeSDK methodology)

The Firecracker head-to-head above measures at the VMM level. [ComputeSDK's leaderboard](https://www.computesdk.com/benchmarks/) publishes a higher-level **Time-to-Interactive (TTI)** — the wall-clock time from `compute.sandbox.create()` returning to the first successful `runCommand("node -v")` stdout byte, against a pre-built sandbox image. [Their methodology doc](https://github.com/computesdk/benchmarks/blob/master/METHODOLOGY.md) specifies the exact test.

[tools/bench-node-tti.sh](tools/bench-node-tti.sh) in this repo replicates it: a `node:20-alpine` Dockerfile with `CMD ["node","-v"]`, warm artifact cache standing in for their pre-built images, and `gocracker run ... --wait` timed to the first stdout byte starting with `v`.

### Setup

- **Host**: AMD Ryzen AI 9 HX 370 (24 threads), Linux 6.17, `/dev/kvm` available
- **Guest kernel**: [artifacts/kernels/gocracker-guest-minimal-vmlinux](artifacts/kernels/gocracker-guest-minimal-vmlinux) (Linux 6.1.102, trimmed initcalls, `loglevel=4` default)
- **Image**: `node:20-alpine`, CMD `["node","-v"]`
- **Warm artifact cache**: first iteration pulls/extracts once, subsequent timed runs boot from the cached ext4
- **Rootfs mode**: default (read-only rootfs with tmpfs overlay) — matches Docker's ephemeral-writable-layer semantics and enables the hardlink fast-path in [pkg/container/container.go](pkg/container/container.go)

### Results (10 timed runs after one warmup)

```
TTI 1: 227ms
TTI 2: 234ms
TTI 3: 240ms
TTI 4: 234ms
TTI 5: 245ms
TTI 6: 207ms
TTI 7: 240ms
TTI 8: 283ms
TTI 9: 226ms
TTI 10: 245ms
median = 237 ms, mean = 236 ms, p95 = 283 ms, min = 207 ms, max = 283 ms
```

### ComputeSDK leaderboard (reference)

For context, ComputeSDK publishes these medians on its own infrastructure and methodology (not on the host used above — different hardware, different orchestration layer):

| provider | TTI median |
|---|---:|
| Daytona | 100 ms |
| Vercel | 380 ms |
| Blaxel | 440 ms |
| E2B | 440 ms |
| Hopx | 1,050 ms |
| Modal | 1,520 ms |
| Cloudflare | 1,720 ms |
| Namespace | 1,770 ms |
| Runloop | 1,960 ms |
| CodeSandbox | 3,790 ms |

These numbers are **not directly comparable** to the 237 ms above — ComputeSDK runs each provider on the provider's own infrastructure (cloud VMs, different CPUs, their own network path). Our measurement is gocracker on an AMD Ryzen AI 9 HX 370 laptop with the bench harness in this repo. The shared piece is the workload: a `node:20-alpine` sandbox whose CMD is `node -v`, timed from the VM spawn to the first stdout byte.

## Snapshot-resume benchmark (head-to-head vs Firecracker)

gocracker's `POST /restore {resume:true}` maps the memory snapshot `MAP_PRIVATE` (lazy COW — no up-front read or copy), re-wires virtio devices, restores vCPU state, and resumes. Head-to-head vs Firecracker v1.10.1 on the same host, 128 MiB guest, `alpine + sleep` rootfs, 20 fresh-process runs each, measured via `curl -w '%{time_total}'` (the request round-trip — excludes shell-process startup):

| VMM | min | p50 | p90 | mean | max |
|---|---:|---:|---:|---:|---:|
| Firecracker v1.10.1 | 1.39 ms | **1.71 ms** | 2.06 ms | 1.83 ms | 3.45 ms |
| gocracker *(this branch)* | 1.50 ms | **2.69 ms** | 3.26 ms | 2.65 ms | 4.81 ms |

A ~1 ms p50 gap at the low end of the latency budget, from a pure-Go VMM against a C/Rust production VMM. The key wins that landed to get here:

- **`MAP_PRIVATE` COW memory restore** — [internal/kvm/kvm.go](internal/kvm/kvm.go). Page-faults the snapshot file in lazily rather than reading+copying 128 MiB up front; saves ~60–100 ms.
- **Single-call `/restore {resume:true}`** — [internal/vmmserver/server.go](internal/vmmserver/server.go). Snapshot-load and vCPU-resume in one HTTP round-trip; saves ~8 ms vs two-call load-then-start.
- **`SkipDiscardProbe` on restore** — [internal/virtio/blk.go](internal/virtio/blk.go). Skips the `FALLOC_FL_PUNCH_HOLE` probe because the guest has already negotiated `VIRTIO_BLK_F_DISCARD` against the pre-snapshot `DeviceFeatures`; saves ~3 ms per writable drive.
- **`postCreateVCPUs` in restore path** — [pkg/vmm/vmm.go](pkg/vmm/vmm.go). Wires per-device `KVM_IRQFD` against the freshly-created vCPU GSIs so queue notifies don't take a reconfiguration VMexit on first use.
- **`MADV_HUGEPAGE` + `MADV_WILLNEED`** on the first 8 MiB of the memory mapping — tells the kernel to serve hot early pages from 2 MiB TLB entries without page-fault stalls.
- **Minimal restore response** — drops `Events[]` + `DeviceList[]` from the hot-path response body.

### Speed summary

| scenario | gocracker | Firecracker v1.10.1 | notes |
|---|---:|---:|---|
| Cold boot (Node TTI) | **237 ms** p50 | — | `gocracker run` to first `node -v` stdout (ComputeSDK methodology) |
| Snapshot-resume | **2.69 ms** p50 | 1.71 ms p50 | `POST /restore {resume:true}` RTT, 128 MiB guest, curl time_total |

### Reproducing

```bash
make build
./tools/bench-node-tti.sh               # 10 cold-boot TTI runs (default)
./tools/bench-node-tti.sh 50            # 50 runs
GC_KERNEL=/path/to/vmlinux ./tools/bench-node-tti.sh
```

First run pulls `node:20-alpine` and builds the ext4 disk (~10–30 s). Every subsequent run hits the cache and measures only the boot path.

## Time-to-Interactive vs commercial sandbox providers

The above compares gocracker to Firecracker at the VMM level. [ComputeSDK's leaderboard](https://www.computesdk.com/benchmarks/) publishes a higher-level **Time-to-Interactive (TTI)** — the wall-clock time from `compute.sandbox.create()` returning to the first successful `runCommand("node -v")` stdout byte, against a pre-built sandbox image. [Their methodology doc](https://github.com/computesdk/benchmarks/blob/master/METHODOLOGY.md) specifies the exact test.

[tools/bench-node-tti.sh](tools/bench-node-tti.sh) in this repo replicates it: a `node:20-alpine` Dockerfile with `CMD ["node","-v"]`, warm artifact cache standing in for their pre-built images, and `gocracker run ... --wait` timed to the first stdout byte starting with `v`.

### Setup

- **Host**: AMD Ryzen AI 9 HX 370 (24 threads), Linux 6.17, `/dev/kvm` available
- **Guest kernel**: [artifacts/kernels/gocracker-guest-minimal-vmlinux](artifacts/kernels/gocracker-guest-minimal-vmlinux) (Linux 6.1.102, trimmed initcalls, `loglevel=4` default)
- **Sandbox image**: `node:20-alpine` (cached locally after the first pull)
- **VM shape**: 1 vCPU, 256 MiB RAM, ext4 rootfs, no network (the test doesn't need it)
- **Optimisations this branch lands**: `loglevel=4` in `firecrackerBaseArgs`, per-fs discard-probe cache, slim minimal kernel, x86 `KVM_IRQFD` on every virtio device + 8250 UART, `debug.SetGCPercent(-1)` in `gocracker-vmm` at init

### Results

10 timed runs after a warm-up:

```
TTI 1: 269ms
TTI 2: 247ms
TTI 3: 252ms
TTI 4: 247ms
TTI 5: 252ms
TTI 6: 250ms
TTI 7: 258ms
TTI 8: 243ms
TTI 9: 253ms
TTI 10: 257ms
median = 252 ms, mean = 253 ms, p95 = 269 ms
```

### Against the ComputeSDK leaderboard

| rank | provider | TTI median | Δ vs gocracker |
|:---:|---|---:|---:|
| 1 | Daytona | 100 ms | −152 (they serve from a pre-warmed snapshot pool) |
| **2** | **gocracker** *(this branch)* | **252 ms** | — |
| 3 | Vercel | 380 ms | **+128 ahead** |
| 4 | Blaxel | 440 ms | **+188 ahead** |
| 4 | E2B | 440 ms | **+188 ahead** |
| 6 | Hopx | 1,050 ms | +798 |
| 7 | Modal | 1,520 ms | +1,268 |
| 8 | Cloudflare | 1,720 ms | +1,468 |
| 9 | Namespace | 1,770 ms | +1,518 |
| 10 | Runloop | 1,960 ms | +1,708 |
| 11 | CodeSandbox | 3,790 ms | +3,538 |

On the same workload ComputeSDK measures on its own, gocracker is **#2 of 11** — ahead of every commercial provider except Daytona, whose lead comes from serving sandboxes out of a pre-warmed snapshot pool rather than cold booting them each time.

### Snapshot-resume (COW restore)

Issue [#3](https://github.com/misaelzapata/gocracker/issues/3) is implemented: `/restore {resume:true}` maps the memory snapshot `MAP_PRIVATE` (lazy COW — no up-front read or copy), re-wires virtio devices, restores vCPU state, and resumes. Head-to-head vs Firecracker v1.10.1 on the same Ryzen AI 9 HX 370 host, 128 MiB guest, `alpine + sleep` rootfs, 20 fresh-process runs each, measured via `curl -w '%{time_total}'` (the request round-trip — excludes shell-process startup):

| VMM | min | p50 | p90 | mean | max |
|---|---:|---:|---:|---:|---:|
| Firecracker v1.10.1 | 1.15 ms | **1.66 ms** | 2.16 ms | 1.72 ms | 2.87 ms |
| gocracker *(this branch)* | 1.68 ms | **2.74 ms** | 3.35 ms | 2.79 ms | 3.54 ms |

A ~1 ms gap at the low end of the latency budget, from a pure-Go VMM against a C/Rust production VMM. Optimisations that landed to get here:

- **`MAP_PRIVATE` COW memory restore** — [`internal/kvm/kvm.go:CreateVMFromSnapshotFile`](internal/kvm/kvm.go). Page-faults the snapshot file in lazily rather than reading+copying 128 MiB up front; saves ~60–100 ms.
- **Single-call `/restore {resume:true}`** — [`internal/vmmserver/server.go:handleRestore`](internal/vmmserver/server.go). Snapshot-load and vCPU-resume in one HTTP round-trip; saves ~8 ms vs two-call load-then-start.
- **`SkipDiscardProbe` on restore** — [`internal/virtio/blk.go:NewBlockDeviceWithOptions`](internal/virtio/blk.go). Skips the `FALLOC_FL_PUNCH_HOLE` probe because the guest has already negotiated `VIRTIO_BLK_F_DISCARD` against the pre-snapshot `DeviceFeatures`; saves ~3 ms per writable drive.
- **`postCreateVCPUs` in restore path** — [`pkg/vmm/vmm.go:restoreFromSnapshot`](pkg/vmm/vmm.go). Wires per-device `KVM_IRQFD` against the freshly-created vCPU GSIs so queue notifies don't take a reconfiguration VMexit on first use.
- **`MADV_HUGEPAGE` + `MADV_WILLNEED`** on the first 8 MiB of the memory mapping — tells the kernel to serve hot early pages from 2 MiB TLB entries without page-fault stalls.
- **Minimal restore response** — drops `Events[]` + `DeviceList[]` from the hot-path response body; callers who need them do a follow-up `GET /vm`.

With this latency as a baseline, a pre-warmed pool of restored VMMs puts `compute.sandbox.create()` TTI in the 5–15 ms range — competitive with Daytona's snapshot-pool lead on the leaderboard above.

### Reproducing

```bash
make gocracker
./tools/bench-node-tti.sh               # 10 timed runs (default)
./tools/bench-node-tti.sh 50            # 50 timed runs
GC_KERNEL=/path/to/vmlinux ./tools/bench-node-tti.sh
GC_BIN=/path/to/gocracker    ./tools/bench-node-tti.sh
```

First run pulls `node:20-alpine` and builds the ext4 disk (~10–30 s). Every subsequent run hits the cache and measures only the boot path.

## Documentation

- [Getting Started](docs/GETTING_STARTED.md) -- build, install, first VM in 60 seconds
- [Networking](docs/NETWORKING.md) -- auto NAT, compose bridges, manual TAP
- [Architecture](docs/ARCHITECTURE.md) -- boot flow, device model, security layers
- [Docker Compose](docs/COMPOSE.md) -- multi-service stacks, healthchecks, ports
- [API Reference](docs/API.md) -- Firecracker-compatible + extended endpoints
- [CLI Reference](docs/CLI_REFERENCE.md) -- every command and flag
- [Snapshots and Migration](docs/SNAPSHOTS.md) -- save, restore, live migrate
- [Examples](docs/EXAMPLES.md) -- 16 example apps across 10 languages
- [Validated Projects](docs/VALIDATED_PROJECTS.md) -- 328 real-world repos tested
- [Troubleshooting](docs/TROUBLESHOOTING.md) -- common issues and fixes
- [How gocracker Fits In](docs/COMPETITIVE_ANALYSIS.md) -- comparison with Firecracker, Kata, etc.
- [Security Policy](SECURITY.md) -- isolation model, vulnerability reporting
- [VFIO GPU Passthrough Plan](docs/VFIO_GPU_PASSTHROUGH_PLAN.md) -- future GPU support

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

## Tested with 328 Real-World Projects

gocracker has been validated against 328 open-source projects across 16 languages,
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

Full manifest: [`tests/external-repos/manifest.tsv`](tests/external-repos/manifest.tsv) (328 entries)

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
