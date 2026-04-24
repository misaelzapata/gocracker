# Architecture

gocracker is a KVM-based microVM runtime written in Go. It boots OCI
containers as real Linux virtual machines with hardware isolation.

## High-Level Overview

```
                CLI (cmd/gocracker)
                      |
            +---------v----------+
            | Container Runtime  |   pkg/container
            | (source -> rootfs  |   - OCI pull / Dockerfile build
            |  -> ext4 -> boot)  |   - initrd generation
            +---------+----------+
                      |
            +---------v----------+
            |       VMM          |   pkg/vmm
            | (VM lifecycle,     |   - KVM VM + vCPU creation
            |  device model,     |   - virtio MMIO transport
            |  run loop)         |   - snapshot / restore
            +---------+----------+
                      |
            +---------v----------+
            |     KVM (kernel)   |   internal/kvm
            |  /dev/kvm ioctls   |   - ioctl wrappers
            +--------------------+
```

### Package Map

| Package | Path | Role |
|---------|------|------|
| CLI | `cmd/gocracker/` | Command-line interface, flag parsing |
| Container | `pkg/container/` | High-level runtime: source to running VM |
| VMM | `pkg/vmm/` | Core VM monitor: KVM, devices, run loop |
| KVM | `internal/kvm/` | Low-level KVM ioctl bindings |
| Virtio | `internal/virtio/` | virtio MMIO transport + device backends |
| UART | `internal/uart/` | 16550A serial console emulation |
| Loader | `internal/loader/` | Kernel loader (bzImage, ELF, multi-format decompression) |
| Vsock | `internal/vsock/` | virtio-vsock (host-guest communication) |
| Guest | `internal/guest/` | Guest init binary (PID 1 inside the VM) |
| Compose | `internal/compose/` | docker-compose.yml parser, multi-VM orchestration |
| API | `internal/api/` | REST API server (Firecracker-compatible + extensions) |
| Hostnet | `internal/hostnet/` | Automatic TAP + NAT networking |
| Stacknet | `internal/stacknet/` | Compose bridge networking (netns, veth, bridge) |
| OCI | `internal/oci/` | OCI image pulling and layer extraction |
| Seccomp | `internal/seccomp/` | seccomp-BPF filter profiles |
| Jailer | `internal/jailer/` | Firecracker-style jailer (chroot, namespaces, cgroups) |
| ACPI | `internal/acpi/` | ACPI table generation (x86) |
| FDT | `internal/fdt/` | Flattened Device Tree generation (ARM64) |
| Log | `internal/log/` | Structured logging (slog + colored CLI output) |

## Boot Flow

When you run `gocracker run --image alpine:3.20 --kernel ./kernel`, the
following steps execute:

1. **Source resolution** -- The container runtime (`pkg/container`) determines
   the source type: OCI image ref, Dockerfile path, or git repo URL.

2. **OCI pull / build** -- For images, layers are pulled via
   `go-containerregistry`. For Dockerfiles, a built-in builder produces the
   image. Layer decompression (gzip, zstd, none) is handled automatically
   via `layer.Uncompressed()`.

3. **Rootfs extraction** -- Layers are unpacked into a temporary directory to
   form the root filesystem.

4. **ext4 disk image** -- The rootfs is packed into an ext4 disk image
   (default 2 GiB, configurable with `--disk`).

5. **Initrd generation** -- A minimal initrd is built in pure Go
   (`cavaliergopher/cpio`) containing the guest init binary, runtime config,
   and host alias entries.

6. **Kernel loading** -- The kernel is loaded into guest memory by
   `internal/loader`. Supported formats:
   - bzImage (x86): uses `payload_offset` from the setup header (protocol >= 2.08).
     Payloads are decompressed by detecting magic bytes (gzip, bzip2, xz,
     lzma, lz4, zstd).
   - ELF: standard ELF loading at the specified physical address.
   - ARM64 Image: raw kernel image loaded at the DRAM base.

7. **KVM VM creation** -- `internal/kvm` opens `/dev/kvm`, creates a VM fd,
   sets up memory regions, creates the in-kernel IRQ chip (IOAPIC on x86,
   GIC on ARM64), and creates the PIT (x86 only). The ordering is:
   TSS address, then IRQ chip, then PIT2.

8. **vCPU setup** -- CPUID is passed through from the host. Boot MSRs are
   configured (11 MSRs including EFER, STAR, LSTAR, PAT, etc.). FPU is
   initialized. The LAPIC is configured with LVT0=ExtINT and LVT1=NMI.
   GDT/IDT are written into guest memory. CR0 and EFER use `|=` (not
   hardcoded) to preserve KVM defaults.

9. **Device registration** -- virtio MMIO devices are placed starting at
   physical address `0xD0000000` with a stride of `0x1000`. IRQs start at 5.
   The UART is at I/O port `0x3F8` (COM1), IRQ 4.

10. **Boot** -- The vCPU starts executing at the kernel entry point. The
    kernel mounts the initrd, runs the guest init, which mounts the ext4
    rootfs, configures networking, and exec's the user process.

## Device Model

| Device | Type | Transport | IRQ | Notes |
|--------|------|-----------|-----|-------|
| virtio-net | Network | MMIO | 5+ | TAP backend, MAC auto-generated |
| virtio-blk | Block storage | MMIO | 5+ | ext4 root disk + additional drives |
| virtio-rng | Entropy | MMIO | 5+ | `/dev/urandom` passthrough |
| virtio-vsock | Host-guest socket | MMIO | 5+ | CID auto-assigned, exec agent |
| virtio-balloon | Memory balloon | MMIO | 5+ | Optional, auto-deflate on OOM |
| virtio-fs | Shared filesystem | MMIO | 5+ | virtiofsd backend, DAX support |
| UART 16550A | Serial console | Port I/O (x86) / MMIO (ARM64) | 4 (x86) | 64KB output buffer for API capture |
| i8042 | Keyboard controller | Port I/O | -- | Reboot detection only (x86) |

All virtio devices use MMIO transport (no PCI). IRQ injection uses
`KVM_IRQ_LINE`. Devices share a common virtio transport layer
(`internal/virtio/`) with per-device backends.

## Security Layers

gocracker uses defense in depth:

1. **KVM hardware isolation** -- Each VM runs in its own KVM address space.
   Guest memory is isolated by the CPU's MMU virtualization (EPT on Intel,
   NPT on AMD).

2. **seccomp-BPF** -- Three profiles restrict system calls:
   - `api`: for the API server process.
   - `vmm`: for the VMM process (device emulation).
   - `vcpu`: for the vCPU thread (tightest, only KVM_RUN and signal handling).

3. **Jailer** -- A Firecracker-style jailer (`internal/jailer`) provides:
   - `chroot` into a per-VM directory (`/srv/jailer/<id>/`).
   - New PID, mount, and network namespaces.
   - cgroup limits (CPU, memory).
   - UID/GID drop to an unprivileged user.
   - Bind-mount only the required files (kernel, disk, sockets).

4. **Privilege drop** -- The jailer is enabled by default (`--jailer on`).
   The outer process (with root) sets up networking, then the jailed worker
   runs the VMM with minimal privileges.

## Platform Differences

| Feature | x86_64 (amd64) | aarch64 (arm64) |
|---------|-----------------|-----------------|
| Interrupt controller | IOAPIC + PIC | GICv2 or GICv3 |
| IRQ delivery | `KVM_IRQ_LINE` | `KVM_IRQ_LINE` + irqfd |
| Timer | PIT (i8254) | ARM arch timer (CNTV) |
| Kernel format | bzImage or ELF | Image (flat binary) |
| Hardware tables | ACPI (RSDP, DSDT, MADT, FADT) | Device Tree (FDT/DTB) |
| Console | UART 16550A at port 0x3F8 | PL011 UART at MMIO |
| RAM base | `0x00000000` | `0x80000000` |
| Boot protocol | Linux boot protocol (setup header) | Kernel Image header |
| CPU init | CPUID passthrough, MSRs, GDT/IDT | PSCI, target CPU features |

The architecture backend is selected at compile time via build tags. The VMM
core (`pkg/vmm/arch.go`) dispatches to `x86MachineBackend` or
`arm64MachineBackend` through the `machineArchBackend` interface.

## Guest Init

The guest init (`internal/guest/init.go`) is a static Go binary compiled for
the target architecture and embedded into the gocracker binary via
`go generate`. It runs as PID 1 inside the VM and performs:

1. Mounts `/proc`, `/sys`, `/dev` (devtmpfs), `/tmp`, `/run`, and `/dev/pts`.
2. Creates device nodes and symlinks (`/dev/fd`, `/dev/stdin`, etc.).
3. Applies guest sysctls.
4. Sets up the serial console.
5. Reads runtime config from the initrd.
6. Mounts the ext4 root disk.
7. Configures network interfaces (IP, gateway, DNS, `/etc/hosts`).
8. Loads requested kernel modules.
9. Changes to the specified working directory.
10. Optionally starts a vsock-based exec agent for interactive access.
11. Execs the user process (entrypoint + cmd) or drops to the exec agent idle loop.

## Event System

The VMM includes a ring-buffer event log (`pkg/vmm/events.go`) that records
VM lifecycle events (boot, stop, pause, snapshot, device attach, etc.). Events
are surfaced via:

- `GET /vms/{id}/events` -- JSON event list.
- `GET /events/stream` -- Server-Sent Events (SSE) for real-time monitoring.
- `GET /logs` -- UART console output (64KB buffer).

---

## More Documentation

- [Getting Started](GETTING_STARTED.md) | [Networking](NETWORKING.md) | [Architecture](ARCHITECTURE.md) | [Compose](COMPOSE.md)
- [API Reference](API.md) | [CLI Reference](CLI_REFERENCE.md) | [Snapshots](SNAPSHOTS.md)
- [Examples](EXAMPLES.md) | [Validated Projects](VALIDATED_PROJECTS.md) | [Troubleshooting](TROUBLESHOOTING.md)
- [Security Policy](../SECURITY.md)
| [Security Policy](../SECURITY.md)

