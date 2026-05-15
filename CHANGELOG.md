# Changelog

All notable changes to gocracker will be documented in this file.
Format follows [Keep a Changelog](https://keepachangelog.com/).

## Unreleased

### Added — Windows / WHP port (2026-05-15)
- **HPET emulation** (`pkg/vmm/hpet_windows.go`) — 10 MHz 64-bit
  counter at 0xFED00000 with 3 timers and a monotonic counter read
  path that always strictly increases between successive reads
  (works around Linux's "counter not counting" check when the udelay
  TSC is unstable during early boot). ACPI HPET table added to
  `internal/acpi/tables_x86.go` so the kernel discovers it via the
  RSDT. Default cmdline now reads `console=ttyS0 earlyprintk=ttyS0
  reboot=k panic=1` — the `tsc_early_khz / tsc=reliable / lpj /
  no_timer_check` workarounds are gone. Linux dmesg confirms:
  `clocksource: hpet: ...` and `tsc: using HPET reference calibration`.
- **Slirp TCP via gVisor netstack** (`internal/slirp/tcp_gvisor.go`,
  build tag `slirp_gvisor`) — gVisor stack with one NIC backed by a
  channel.Endpoint; `Slirp.handleTCP` injects guest frames, drains
  outbound, dials real host sockets for guest-initiated connections.
  `tcp_stub.go` (default tag) keeps the drop-with-metric behaviour
  so Linux builds without `slirp_gvisor` stay lean. Windows builds
  enable the tag by default.
- **Host→guest port-forwarding registry**
  (`internal/slirp/portfwd.go`) — `PortFwdRegistry.Add/Lookup/Listen`
  with a goroutine-per-rule accept loop and `ctx`-driven shutdown.
  Foundation for `--publish HOST_IP:PORT:GUEST_PORT/{tcp,udp}`.
- **UDP NAT idle-flow janitor** (`internal/slirp/udp_nat.go`) —
  per-flow `lastActivity` atomic; 5 s sweep evicts flows idle ≥ 30 s.

### Changed — Windows / WHP port (2026-05-15)
- **`machineArchBackend` interface** (`pkg/vmm/arch.go`) now takes
  `HVVCPU`/`Hypervisor`/`HVVM` instead of `*kvm.VCPU`/`*kvm.System`/
  `*kvm.VM`. The x86/arm64 adapters type-assert to the concrete
  `*kvmVCPU`/`*kvmVM` to access KVM-specific extensions
  (MSR/FPU/XSAVE/LAPIC/GIC/PSCI) — `HVVCPU` stays narrow.
- **`VM` struct** drops `kvmSys`/`kvmVM`/`vcpus` fields; uses
  `hv`/`hvVM`/`hvVCPUs` exclusively. Constructor + cleanup +
  `runLoop`/`handleIO`/`handleMMIO` all updated to take `HVVCPU`.
  `pkg/vmm` now cross-compiles cleanly on Windows.
- **Interactive shell** —
  - UART RDI is now level-triggered: every `PushRX` while `IER.RDI`
    is set latches `rdiPending` and raises IRQ4 (was edge-triggered
    on empty→non-empty; dropped IRQs when host typed multi-byte
    commands faster than the guest drained RBR).
  - `WHPBootSession.raiseIRQ` queues IRQs that arrive before the PIC
    is initialised or while a line is masked; the new
    `pic8259.OnStateChange` hook drains the queue on every ICW
    completion / OCW1 mask update.
  - `cmd/gocracker-guest-shell/main.go` opens `/dev/kmsg` once and
    reuses the fd for every klog call (avoids reopening per call
    which the kernel printk subsystem sometimes drops).
  - Small `time.Sleep(50 ms)` after console wiring so the 8250
    driver finishes its UART probe before we hit the port.

### Added — Windows / WHP port (2026-05-13)
- 16550A UART (`pkg/vmm/uart_windows.go`) — full COM1 with DLAB-gated
  divisor latches, IER, MCR loopback, RX FIFO, IRQ4 on RBR/THRE transitions.
- PCI config-space dummy (`pkg/vmm/pci_windows.go`) — ports 0xCF8/0xCFC
  return the no-device sentinel so Linux's bus enumeration exits cleanly.
- 8254 PIT real mode-3 (`pkg/vmm/pit_windows.go` rewrite) — internal IRQ0
  via `SetIRQ0Callback`; lets us drop the `tsc=reliable / lpj=10000000
  / no_timer_check` cmdline workarounds.
- MC146818 CMOS / RTC (`pkg/vmm/cmos_windows.go`) — BCD time fields backed
  by `time.Now().UTC()`.
- ACPI tables (`internal/acpi/tables_x86.go`) — RSDP + RSDT + MADT
  enumerating LAPIC at 0xFEE00000, IOAPIC at 0xFEC00000, IRQ0 + IRQ4
  source overrides.
- virtio-rng-mmio (`pkg/vmm/virtio_rng_windows.go`) — always-on entropy
  from `crypto/rand` so userspace doesn't block on initial seeding.
- virtio-blk-mmio (`pkg/vmm/virtio_blk_windows.go`) — rootfs.ext4 attach
  with T_IN / T_OUT / T_FLUSH / T_GET_ID. Port of node-vmm/virtio/blk.cc.
- WHP MMIO emulator binding (`internal/whp/emulator_windows.go`) — wraps
  `WHvEmulatorTryMmioEmulation` with 5 Go callbacks routed via a per-vCPU
  registry. Plus `WHvTranslateGva` binding and `eFail` HRESULT.
- Pure-Go ext4 image builder (`internal/ext4/builder.go`) — wraps
  `github.com/diskfs/go-diskfs`.
- `WHPBootConfig.RootfsPath` + `-rootfs` / `-rootfs-ro` flags on
  `gocracker-whp.exe`.
- `HVVMConfig.EnableXAPIC` opt-in (`BootLinuxOnWHP` sets true, bare-metal
  smoke tests leave false). Default `false` keeps `WHvCancelRunVirtualProcessor`
  semantics clean for HLT-only test partitions.
- `make test-smoke` Makefile target (≤60 s sub-suite).

### Changed — Windows / WHP port
- virtio IRQ delivery unified: the per-device eventfd + `KVM_IRQFD` hot
  path is gone. All 13 sites (UART + virtio devices in `vmm.go` and
  `arch_arm64.go`) now use `makeIRQLine` → `HVVM.InjectInterrupt`. The
  KVM adapter forwards to `kvm.VM.IRQLine` (one extra ioctl per IRQ);
  the WHP adapter calls `WHvRequestInterrupt`. No per-arch divergence,
  `irqEventFds` field gone, net −88 LoC.
- `cmd/gocracker` Windows stub now points users at `gocracker-whp.exe`
  instead of just suggesting WSL2.

### Fixed — Windows / WHP port
- `TestWHPHypervisorEndToEnd` no longer times out: enabling
  `PropLocalApicEmulationMode` unconditionally on every partition caused
  the bare-metal HLT-at-GPA-0 smoke test to never see its halt exit
  (background timer IRQs kept the vCPU live). Gated behind
  `HVVMConfig.EnableXAPIC`.
- Initrd no longer corrupts the kernel image: placed near the top of
  RAM (`MemoryBytes - initrdSize` aligned down) instead of at a fixed
  16 MiB that collided with a 40 MiB vmlinux loaded at 1 MiB.
- Initramfs `/init` execve no longer fails with EACCES on Windows host
  builds: cpio writer stamps 0755 on regular files and directories
  when `runtime.GOOS == "windows"` since the staged-temp files don't
  carry POSIX mode bits there.
- ACPI MADT no longer advertises an I/O APIC: previously the kernel
  trusted the (unemulated) IOAPIC and masked the 8259 PIC, so timer
  and COM1 IRQs never reached the guest. With LAPIC-only, the kernel
  falls back to PIC for legacy IRQs, which is what
  `WHvRequestInterrupt` delivers.

### Verified end-to-end (Win11 24H2 + WHP)
`gocracker-whp.exe -mem 256 -initrd <initrd.cpio.gz> vmlinux` runs the
Linux kernel through every subsystem-init phase (memory, ACPI,
filesystems, PCI probe, 8250/16550 driver, virtio, networking,
crypto) and hands off to `/init` in userspace. Tested kernels: the
shipped `gocracker-guest-standard-vmlinux` (Linux 6.1.102). The
default cmdline still ships with `tsc=reliable / lpj=10000000 /
no_timer_check` because our software PIT counter readbacks don't
converge against Linux's TSC calibration probe (HPET emulation or
KV-clock/HV-clock paravirtual TSC are the long-term fix).

### Added — boot-to-shell scaffolding
- `WHV_INTERRUPT_CONTROL` struct size fixed: 32 → 16 bytes. The wrong
  size caused `WHvRequestInterrupt` to fail with `WHV_E_INVALID_PARAMETER`
  (0xc0350005) on every PIT / UART IRQ injection — the kernel never
  saw a timer tick, so jiffies never advanced and userspace never got
  scheduled.
- `WHPBootSession.PushUARTInput` exposes the 16550A RX path so a host
  stdin reader can feed keystrokes into the guest's COM1.
- `cmd/gocracker-guest-shell` — minimal Linux pid 1 that wires fd 0/1/2
  to `/dev/ttyS0`, prints a banner, and echoes stdin. Used by
  `tools/mkshellinitrd` to produce a `shell-initrd.cpio.gz` for the
  WHP smoke path before the full vsock exec-agent lands on Windows.
- `Run` loop treats `ExitReasonHalt` as idle (just continues) instead
  of returning — HLT is the vCPU's wait-for-interrupt state, not a
  shutdown signal.
- Linux kernel cmdline now includes `tsc_early_khz=2400000` —
  Linux's PIT-based TSC calibration loop doesn't converge against our
  software-only PIT, but `tsc_early_khz` lets the kernel skip it and
  trust the host's TSC rate directly. Once HPET emulation lands, this
  workaround can come off.
- cpio packer (internal/guest/initrd.go) converts Windows backslash
  separators to forward slashes via `filepath.ToSlash`. The Linux
  kernel cpio reader treats `dev\pts` as a literal filename, not as
  `dev/pts`; every directory hierarchy in a Windows-built initramfs
  was broken until this commit.

### Verified end-to-end (Win11 24H2 + WHP)

`gocracker-whp.exe -initrd <shell-initrd.cpio.gz> vmlinux` boots Linux
to **userspace `/init` running**, with the embedded
`gocracker-guest-shell` printing its banner to the host console:

```
[    0.221587] === gocracker-guest-shell — Linux on WHP — alive as PID 1 ===
```

The init binary mounts `devtmpfs / proc / sysfs`, opens `/dev/ttyS0`
read-write, and loops echoing host stdin into the guest's COM1 RX
path via `WHPBootSession.PushUARTInput`. End-to-end keystroke echo
needs more polish (subsequent kmsg lines after the first don't
always make it through within the 6 s timeout — likely a printk
rate-limiting / TSC-tick-source artefact), but the boot-to-shell
plumbing is in place.

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
