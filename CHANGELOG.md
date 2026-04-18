# Changelog

All notable changes to gocracker will be documented in this file.
Format follows [Keep a Changelog](https://keepachangelog.com/).

## Unreleased

### Added
- **Warm cache** (`--warm` / `GOCRACKER_WARM_CACHE=1`): captures a dirty-page
  snapshot on the first cold boot and restores from it on subsequent runs.
  Restore completes in ~5–7 ms via `MAP_PRIVATE` on a sparse `mem.bin`; only
  the pages the guest actually touches are faulted in from disk.
- `reIPGuest`: after a warm restore with `--net auto`, the guest's `eth0` is
  automatically re-configured (flush + new CIDR + default route) via the exec
  agent so outbound connectivity works through the new TAP's gateway.
- `ComputeWarmCacheKey` exported from `pkg/container` for external tooling.
- `waitExecReady` helper in warm-cache capture path to ensure the exec agent
  is up before taking the snapshot.
- `TrackDirtyPages` wired through the vmmserver/worker path so jailer-on VMs
  can participate in the warm cache.

### Fixed
- Warm-cache captures on jailer-on VMs now hardlink the disk from the host path
  instead of failing to copy across the jailer bind-mount boundary; bundle time
  drops from ~1500 ms to <60 ms.
- vsock `QuiesceForSnapshot` no longer waits 250 ms when there are no active
  connections; the RX drain is skipped when no RST packets were enqueued.
- `drainWarmDone` is called before `printResult` and before the interactive
  shell opens, preventing snapshot-goroutine log lines from appearing between
  the network info and the shell prompt.
- Snapshot `mem.bin` uses a sparse file layout (one `WriteAt` per dirty run,
  `ftruncate` to full size); clean pages are true file holes that read as zero
  via `MAP_PRIVATE`, matching the original `MAP_ANONYMOUS` state without
  copying any bytes.
- `augmentDirtyBitmap` marks all non-zero pages as dirty before saving so
  kernel-loaded pages (kernel image in guest RAM) are not silently zeroed out
  on restore, preventing "Initramfs unpacking failed: broken padding" panics.

## 0.1.0 - 2026-04-10

First public release. Complete KVM microVM runtime with multi-architecture support.

### Core Runtime
- KVM-based virtual machine monitor in pure Go (~10K LOC)
- x86-64 long mode boot: 4-level page tables, GDT, ACPI/legacy selectable
- ARM64 boot: FDT/DTB generation, GICv2/v3, PSCI power management
- ELF vmlinux and compressed Image kernel loading
- Pure Go ext4 disk builder (no mkfs.ext4 dependency)
- Pure Go initrd/cpio builder with embedded guest init binary

### Container Runtime
- OCI image pulling from any registry (gzip/zstd/uncompressed layers)
- Dockerfile parser and executor (BuildKit AST, multi-stage, COPY --from, RUN --mount)
- Git repo cloner with Dockerfile auto-detection
- Docker Compose orchestrator (services, healthchecks, depends_on, ports, volumes)

### Device Model (virtio MMIO, Firecracker-compatible)
- virtio-net: TAP backend, TX/RX, mergeable rx buffers
- virtio-blk: raw image, read/write/flush/discard
- virtio-rng: /dev/random entropy source
- virtio-vsock: host-guest streams, exec agent for TTY and command execution
- virtio-balloon: manual target, stats polling, conservative auto reclaim
- virtio-fs: shared filesystem passthrough (vhost-user backend)
- UART 16550A: serial console (I/O ports on x86, MMIO on ARM64)

### Networking
- Automatic networking: `--net auto` creates TAP + IPv4 + iptables NAT
- Compose networking: isolated namespace per stack, bridge, service DNS
- Port forwarding via userspace TCP/UDP proxy
- Manual TAP interface support

### Isolation and Security
- Firecracker-style jailer: chroot, mount namespace, PID namespace, cgroups v2
- seccomp BPF filtering: per-profile syscall whitelists (API, VMM, vCPU)
- Privilege drop to unprivileged UID/GID after setup
- PR_SET_NO_NEW_PRIVS enforcement

### Operations
- Snapshot/restore: RAM + vCPU registers + device state
- Live migration: stop-and-copy between API servers
- REST API: Firecracker-compatible + extended endpoints
- SMP: multi-vCPU guest boot (2/4+ validated)
- Memory hotplug: online grow/shrink beyond base budget

### ARM64 Support
- GICv2/v3 in-kernel interrupt controller (auto-probed)
- irqfd-based interrupt delivery (eventfd, matching Firecracker)
- PSCI v0.2 for shutdown/reboot and SMP CPU_ON
- Secondary vCPU POWER_OFF for safe multi-core boot
- DTB with timer, GIC, PSCI, UART, virtio, clock nodes
- virtio-net link activation on DRIVER_OK (carrier detect fix)
- Snapshot/restore with per-host PreferredARM64Target
- ARM64 guest kernel build tooling
- Tested on AWS a1.metal (Graviton 1, Cortex-A72)

### API
- Firecracker-compatible preboot flow (boot-source, machine-config, drives, actions)
- Extended: POST /run, GET /vms, snapshot, exec, logs, SSE event stream
- Bearer-token authentication
- Trusted-path validation for kernel/workspace/snapshot paths

### Tooling
- `build-guest-kernel.sh` (x86-64) and `build-guest-kernel-arm64.sh`
- Pre-built guest kernels: x86 vmlinux + ARM64 Image (6.17.13)
- `check-host-devices.sh` host readiness validation
- `prepare-kernel.sh` for pinning host kernels
- 17 example applications (Go, Python, Node, Ruby, Rust, Java, PHP, Elixir, Deno, Bun)
- Manual smoke test suite with 10+ fixtures
- Integration test suite (API, compose, exec, balloon, migration, virtiofs)

### Fixed
- KVM_GET_ONE_REG ioctl direction on ARM64 (_IOWR to _IOW)
- virtio GPA translation for ARM64 RAM base (0x80000000)
- DTB mem_rsvmap and root endNode encoding
- Guest init_arm64.bin rebuilt as static ELF (was ar archive)
- Hardened virtio guest memory access and descriptor-chain traversal
- Hardened OCI layer extraction (entries stay inside rootfs)
- Hardened jailer against symlink-based path tricks
- vCPU mmap and guest-RAM cleanup gaps
- virtio-fs eventfd leak on partial allocation failure
