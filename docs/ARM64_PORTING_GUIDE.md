# gocracker aarch64 Porting Guide

## Scope

Add ARM64 (aarch64) support alongside x86-64, without breaking anything that exists today. Same approach as Firecracker: two architecture directories, shared core, build-tag split.

## Current state

Every file below is **x86-64 only** today. None have build tags; all compile unconditionally.

| File | What's x86-specific |
|------|---------------------|
| `internal/kvm/kvm.go` | IOCTL constants, Sregs/Segment structs, CPUID, MSRs, FPU, LAPIC, GSI routing, TSS addr |
| `internal/kvm/cpu.go` | GDT/IDT, 4-level page tables, CR0/CR3/CR4/EFER, long mode setup |
| `internal/acpi/` | RSDP, FADT, MADT, DSDT, AML — all x86 ACPI boot tables |
| `internal/mptable/` | MP table, APIC/IOAPIC addresses, ISA IRQ routing |
| `internal/loader/loader.go` | bzImage header, E820 memory map, boot_params zero-page |
| `internal/uart/uart.go` | 16550A registers (0x3F8), I/O port offsets |
| `internal/i8042/i8042.go` | i8042 keyboard controller (port 0x60), CPU reset via 0xFE |
| `pkg/vmm/vmm.go` | Memory layout (BootParams, GDT, cmdline, kernel @ 1 MiB), vCPU setup calls, device wiring, IRQ setup |
| `internal/guest/init.go` | Compiled with `GOARCH=amd64` |

Already architecture-neutral (no changes needed):

| File | Why |
|------|-----|
| `internal/virtio/` | Pure MMIO transport — just change base addresses |
| `internal/fdt/fdt.go` | DTB generator already exists (ARM needs DTB instead of ACPI) |
| `internal/vsock/` | Virtio-vsock over MMIO — address-agnostic |
| `internal/console/` | PTY handling — no arch dependency |
| `internal/oci/` | OCI/image — no arch dependency (just pull `linux/arm64` manifests) |
| `internal/dockerfile/` | Build engine — no arch dependency |
| `internal/compose/` | Compose orchestrator — no arch dependency |
| `internal/api/` | REST API — no arch dependency |
| `pkg/container/` | High-level runtime — no arch dependency |

---

## Phase 0 — Rename existing files (zero behavior change)

Rename x86-specific files to `_amd64.go` so they only compile on x86. Run `go test ./...` on x86 after each rename to confirm nothing breaks.

```
internal/kvm/kvm.go          → split into:
  internal/kvm/kvm.go              (shared: System, VM, VCPU types, fd management)
  internal/kvm/kvm_amd64.go        (x86: ioctl constants, Sregs, Segment, CPUID, MSR, FPU, LAPIC, GSI)

internal/kvm/cpu.go           → internal/kvm/cpu_amd64.go

internal/acpi/acpi.go         → internal/acpi/acpi_amd64.go
internal/acpi/aml.go          → internal/acpi/aml_amd64.go

internal/mptable/mptable.go   → internal/mptable/mptable_amd64.go

internal/loader/loader.go     → split into:
  internal/loader/loader.go        (shared: ELF loader, decompression)
  internal/loader/bzimage_amd64.go (x86: bzImage header, E820, boot_params)

internal/uart/uart.go         → internal/uart/uart.go  (keep as-is, also needed on ARM for PL011 to coexist)

internal/i8042/i8042.go       → internal/i8042/i8042_amd64.go

pkg/vmm/vmm.go                → split into:
  pkg/vmm/vmm.go                   (shared: Config, state machine, pause/resume, snapshot, run loop)
  pkg/vmm/boot_amd64.go            (x86: memory layout, vCPU setup, device wiring, IRQ routing)
```

Guest init:
```
internal/guest/init.go         (keep shared — pure Linux userspace)
internal/guest/initrd.go       (keep shared)
```
Just add a second build target: `GOARCH=arm64` → `init_arm64.bin`.

**Verification:** `go build ./...` and `go test ./...` on x86 must produce identical binaries and results.

---

## Phase 1 — Define architecture interface

Create a clean interface that both x86 and ARM implement:

```go
// pkg/vmm/arch.go (shared)
package vmm

type ArchBoot interface {
    // Memory layout
    KernelLoadAddr() uint64
    InitrdAddr() uint64
    CmdlineAddr() uint64
    VirtioBase() uint64
    VirtioStride() uint64

    // vCPU setup
    SetupVCPU(vcpu *kvm.VCPU, index int, entry uint64, mem []byte) error

    // Device creation
    CreateSerialDevice(irqFn func(bool)) SerialDevice
    CreateRebootDevice() RebootDevice

    // Boot tables (ACPI/MPtable for x86, DTB for ARM)
    WriteBootTables(mem []byte, cfg BootTableConfig) error

    // IRQ routing
    SetupIRQRouting(vm *kvm.VM, devices []IRQDevice) error

    // Kernel loading
    LoadKernel(mem []byte, kernelPath string, cmdline string, initrdPath string) (entry uint64, err error)
}

type SerialDevice interface {
    Read(offset uint64) byte
    Write(offset uint64, val byte)
    InjectBytes(data []byte)
    SetOutput(w io.Writer)
}

type RebootDevice interface {
    Read(offset uint64) byte
    Write(offset uint64, val byte)
    RebootRequested() bool
}
```

Then in the VMM core:
```go
// pkg/vmm/vmm.go (shared run loop)
func (m *VM) boot() error {
    arch := m.arch  // ArchBoot — set at creation time based on runtime.GOARCH
    entry, err := arch.LoadKernel(m.mem, m.cfg.KernelPath, cmdline, initrdPath)
    // ...
    arch.WriteBootTables(m.mem, tablesCfg)
    // ...
    for i := 0; i < vcpuCount; i++ {
        arch.SetupVCPU(m.vcpus[i], i, entry, m.mem)
    }
    arch.SetupIRQRouting(m.vm, irqDevices)
    // ...
}
```

---

## Phase 2 — x86 implementation (extract, don't rewrite)

Move existing code into the interface — this is mechanical extraction, not new logic:

```go
// pkg/vmm/boot_amd64.go
package vmm

type x86Boot struct{}

func newArchBoot() ArchBoot { return &x86Boot{} }

func (x *x86Boot) KernelLoadAddr() uint64  { return 0x100000 }        // 1 MiB
func (x *x86Boot) InitrdAddr() uint64      { return 0x1000000 }       // 16 MiB
func (x *x86Boot) CmdlineAddr() uint64     { return 0x20000 }
func (x *x86Boot) VirtioBase() uint64      { return 0xD0000000 }
func (x *x86Boot) VirtioStride() uint64    { return 0x1000 }

func (x *x86Boot) SetupVCPU(vcpu *kvm.VCPU, index int, entry uint64, mem []byte) error {
    // existing code from vmm.go lines 304-327:
    // SetupCPUID, SetupMSRs, SetupFPU, SetupLongMode, SetupLAPIC
}

func (x *x86Boot) CreateSerialDevice(irqFn func(bool)) SerialDevice {
    return uart.New(irqFn)  // 16550A
}

func (x *x86Boot) CreateRebootDevice() RebootDevice {
    return i8042.New()
}

func (x *x86Boot) WriteBootTables(mem []byte, cfg BootTableConfig) error {
    // existing ACPI + MP table code
}

func (x *x86Boot) SetupIRQRouting(vm *kvm.VM, devices []IRQDevice) error {
    // existing GSI routing code from setupIRQs()
}

func (x *x86Boot) LoadKernel(mem []byte, kernelPath, cmdline, initrdPath string) (uint64, error) {
    // existing loader.Load() + E820 + boot_params
}
```

**Verification:** identical behavior to current code. `go test ./...` green.

---

## Phase 3 — ARM64 KVM bindings

New file: `internal/kvm/kvm_arm64.go`

```go
//go:build arm64

package kvm

// ARM64 KVM ioctl constants
const (
    kvmArmVCPUInit    = 0xAE   // KVM_ARM_VCPU_INIT
    kvmArmPreferredTarget = 0xAF  // KVM_ARM_PREFERRED_TARGET
    kvmSetOneFeg      = ...     // KVM_SET_ONE_REG (ARM uses one-reg interface, not bulk regs)
    kvmGetOneFeg      = ...
)

// ARM64 vCPU init target
type ARMVCPUInit struct {
    Target   uint32
    Features [7]uint32
}

// ARM uses KVM_SET_ONE_REG / KVM_GET_ONE_REG with register IDs
// instead of x86's bulk GetRegs/SetRegs
type ARMOneReg struct {
    ID   uint64
    Addr uint64  // pointer to value
}

// ARM register IDs (from Linux arch/arm64/include/uapi/asm/kvm.h)
const (
    // Core registers
    KVM_REG_ARM64_CORE_BASE = 0x6030000000100000
    RegPC    = KVM_REG_ARM64_CORE_BASE + 0x100  // Program Counter
    RegPSTATE = KVM_REG_ARM64_CORE_BASE + 0x104  // Process State
    RegSP_EL1 = KVM_REG_ARM64_CORE_BASE + ...

    // System registers
    RegSCTLR_EL1 = ...  // System Control Register
    RegTCR_EL1   = ...  // Translation Control Register
    RegMPIDR_EL1 = ...  // Multiprocessor Affinity Register
)
```

New file: `internal/kvm/cpu_arm64.go`

```go
//go:build arm64

package kvm

func (vcpu *VCPU) SetupARM64Boot(entry uint64, dtbAddr uint64) error {
    // 1. KVM_ARM_VCPU_INIT with preferred target
    // 2. Set PC = kernel entry point
    // 3. Set X0 = DTB physical address (ARM Linux boot protocol)
    // 4. Set PSTATE = EL1h (Exception Level 1, handler mode)
    // 5. No page tables needed — kernel sets them up itself
}
```

Key difference from x86: ARM Linux kernel sets up its own page tables. The VMM just needs to:
- Point PC at the kernel entry
- Point X0 at the DTB address
- Set PSTATE to EL1h
- That's it — no GDT, no page tables, no EFER, no long mode

---

## Phase 4 — ARM64 serial: PL011

New package: `internal/pl011/pl011.go`

```go
package pl011

// PL011 UART registers (ARM PrimeCell UART)
const (
    RegDR   = 0x000  // Data Register
    RegFR   = 0x018  // Flag Register
    RegIBRD = 0x024  // Integer Baud Rate
    RegFBRD = 0x028  // Fractional Baud Rate
    RegLCR  = 0x02C  // Line Control
    RegCR   = 0x030  // Control Register
    RegIFLS = 0x034  // Interrupt FIFO Level Select
    RegIMSC = 0x038  // Interrupt Mask Set/Clear
    RegRIS  = 0x03C  // Raw Interrupt Status
    RegMIS  = 0x040  // Masked Interrupt Status
    RegICR  = 0x044  // Interrupt Clear Register
)

// PL011 Flag Register bits
const (
    FRTxFull  = 1 << 5
    FRRxEmpty = 1 << 4
    FRBusy    = 1 << 3
)

type PL011 struct {
    // Same pattern as uart.UART: mutex, rxBuf, irqFn, reader, writer
}

func (p *PL011) Read(offset uint64) byte  { /* MMIO read handler */ }
func (p *PL011) Write(offset uint64, val byte) { /* MMIO write handler */ }
func (p *PL011) InjectBytes(data []byte) { /* same as UART.InjectBytes */ }
```

Key differences from 16550A:
- MMIO instead of I/O ports (both are memory-mapped in gocracker, so minimal change)
- Different register layout and widths
- Interrupt model: level-triggered via GIC instead of edge-triggered via PIC/IOAPIC

---

## Phase 5 — ARM64 interrupt controller: GIC

ARM uses GIC (Generic Interrupt Controller) instead of PIC/APIC/IOAPIC.

KVM on ARM creates the GIC in-kernel (like x86's in-kernel IRQCHIP).

```go
// internal/kvm/gic_arm64.go
//go:build arm64

package kvm

const (
    kvmCreateDevice = 0xE0  // KVM_CREATE_DEVICE
    kvmDeviceARM_VGIC_V3 = 7
)

// GIC distributor and redistributor addresses
const (
    GICDBase = 0x08000000  // Distributor (same as QEMU virt machine)
    GICRBase = 0x080A0000  // Redistributor
)

func (vm *VM) CreateGICv3(vcpuCount int) error {
    // 1. KVM_CREATE_DEVICE with type=kvmDeviceARM_VGIC_V3
    // 2. Set distributor base address via KVM_DEV_ARM_VGIC_GRP_ADDR
    // 3. Set redistributor base address
    // 4. KVM_DEV_ARM_VGIC_GRP_CTRL / KVM_DEV_ARM_VGIC_CTRL_INIT
    // GIC replaces LAPIC + IOAPIC + PIC
}
```

IRQ injection on ARM:
```go
// Instead of KVM_IRQ_LINE with GSI routing, ARM uses:
// KVM_IRQ_LINE with SPI (Shared Peripheral Interrupt) numbers
// SPI 0 = GIC interrupt 32 (first 32 are SGI/PPI)
func (vm *VM) SetIRQLine(spi uint32, level bool) error {
    // Same KVM_IRQ_LINE ioctl, different encoding
}
```

---

## Phase 6 — ARM64 reboot: PSCI

ARM doesn't have i8042. Guest reboots via PSCI (Power State Coordination Interface):

```go
// internal/kvm/psci_arm64.go
//go:build arm64

package kvm

// PSCI function IDs (ARM DEN0022D specification)
const (
    PSCI_CPU_ON_64   = 0xC4000003
    PSCI_SYSTEM_OFF  = 0x84000008
    PSCI_SYSTEM_RESET = 0x84000009
)

// KVM handles PSCI calls in-kernel when the guest does HVC/SMC.
// The VMM sees KVM_EXIT_SYSTEM_EVENT instead of KVM_EXIT_IO.
// No separate device needed — just handle the exit code in the run loop.
```

In the run loop:
```go
case KVM_EXIT_SYSTEM_EVENT:
    // Guest called PSCI SYSTEM_RESET or SYSTEM_OFF
    eventType := runData.SystemEvent.Type
    if eventType == KVM_SYSTEM_EVENT_RESET {
        // same as i8042 reboot on x86
    }
```

---

## Phase 7 — ARM64 boot: Device Tree

ARM Linux boots from a DTB (Device Tree Blob), not ACPI/E820/boot_params.

The DTB describes: memory, CPUs, GIC, UART, virtio devices, chosen (cmdline + initrd).

```go
// pkg/vmm/boot_arm64.go
//go:build arm64

package vmm

type arm64Boot struct{}

func newArchBoot() ArchBoot { return &arm64Boot{} }

func (a *arm64Boot) KernelLoadAddr() uint64  { return 0x40080000 }  // QEMU virt standard
func (a *arm64Boot) InitrdAddr() uint64      { return 0x48000000 }
func (a *arm64Boot) CmdlineAddr() uint64     { return 0 } // embedded in DTB
func (a *arm64Boot) VirtioBase() uint64      { return 0x0A000000 }  // QEMU virt standard
func (a *arm64Boot) VirtioStride() uint64    { return 0x200 }

func (a *arm64Boot) WriteBootTables(mem []byte, cfg BootTableConfig) error {
    // Use internal/fdt/ to generate DTB:
    //
    // /chosen {
    //     bootargs = "console=ttyAMA0 ...";
    //     linux,initrd-start = <initrd_addr>;
    //     linux,initrd-end = <initrd_addr + initrd_size>;
    // };
    //
    // /memory@40000000 {
    //     device_type = "memory";
    //     reg = <0x0 0x40000000 0x0 mem_size>;
    // };
    //
    // /cpus { ... one cpu@ node per vCPU ... };
    //
    // /intc (GICv3) {
    //     compatible = "arm,gic-v3";
    //     reg = <GICD_base, GICD_size, GICR_base, GICR_size>;
    // };
    //
    // /pl011@9000000 {
    //     compatible = "arm,pl011";
    //     reg = <0x9000000 0x1000>;
    //     interrupts = <GIC_SPI 1 IRQ_TYPE_LEVEL_HIGH>;
    // };
    //
    // /virtio_mmio@a000000 { ... one per device ... };
    //
    // Write DTB at dtbAddr in guest memory
}

func (a *arm64Boot) SetupVCPU(vcpu *kvm.VCPU, index int, entry uint64, mem []byte) error {
    // KVM_ARM_VCPU_INIT
    // Set PC = entry
    // Set X0 = dtbAddr
    // Set PSTATE = EL1h
    // Secondary CPUs: PSCI CPU_ON (handled by KVM in-kernel)
}

func (a *arm64Boot) CreateSerialDevice(irqFn func(bool)) SerialDevice {
    return pl011.New(irqFn)  // PL011 instead of 16550A
}

func (a *arm64Boot) CreateRebootDevice() RebootDevice {
    return nil  // PSCI is handled via KVM_EXIT_SYSTEM_EVENT, no separate device
}

func (a *arm64Boot) LoadKernel(mem []byte, kernelPath, cmdline, initrdPath string) (uint64, error) {
    // ARM64 kernel = raw Image or ELF
    // Load at KernelLoadAddr
    // No bzImage, no boot_params, no E820
    // Entry point from ELF header or KernelLoadAddr for raw Image
}

func (a *arm64Boot) SetupIRQRouting(vm *kvm.VM, devices []IRQDevice) error {
    // GICv3 in-kernel handles routing
    // Just set SPI numbers for each device
}
```

---

## Phase 8 — ARM64 guest kernel

```bash
# Build ARM64 guest kernel
./tools/build-guest-kernel.sh --arch arm64

# Produces:
# ./artifacts/kernels/gocracker-guest-standard-arm64-Image
```

Kernel config differences from x86:
- `CONFIG_ARCH=arm64` (not x86_64)
- `CONFIG_PL011_SERIAL=y` (not 8250/16550)
- `CONFIG_ARM_GIC_V3=y` (not APIC)
- `CONFIG_VIRTIO_MMIO=y` (same)
- `CONFIG_EXT4_FS=y` (same)
- No `CONFIG_ACPI` needed (DTB-only boot)

---

## Phase 9 — ARM64 guest init

```bash
# Second init binary
cd internal/guest
GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w -extldflags "-static"' -o init_arm64.bin ./init.go

# Embed both in initrd builder
```

The init code is pure Go + Linux syscalls — it's architecture-neutral. Only the binary needs to be cross-compiled.

Update `internal/guest/initrd.go` to embed `init_arm64.bin` when `runtime.GOARCH == "arm64"`.

---

## Phase 10 — OCI multi-arch

Update OCI puller to request `linux/arm64` platform when running on ARM:

```go
// internal/oci/oci.go
import "runtime"

func defaultPlatform() v1.Platform {
    return v1.Platform{
        OS:           "linux",
        Architecture: runtime.GOARCH,  // "amd64" or "arm64"
    }
}
```

Most popular images (alpine, nginx, postgres, etc.) already ship `linux/arm64` manifests.

---

## Phase summary

| Phase | What | Files changed | Files added | Risk to x86 |
|-------|------|---------------|-------------|--------------|
| 0 | Rename `→ _amd64.go` | ~10 renames | 0 | **Zero** (build tag only) |
| 1 | Define `ArchBoot` interface | `pkg/vmm/vmm.go` | `pkg/vmm/arch.go` | **Zero** (additive) |
| 2 | Extract x86 into interface | `pkg/vmm/vmm.go` | `pkg/vmm/boot_amd64.go` | **Low** (mechanical move) |
| 3 | ARM64 KVM bindings | 0 | `internal/kvm/kvm_arm64.go`, `cpu_arm64.go` | **Zero** (new files) |
| 4 | PL011 serial | 0 | `internal/pl011/pl011.go` | **Zero** (new package) |
| 5 | GIC interrupt controller | 0 | `internal/kvm/gic_arm64.go` | **Zero** (new files) |
| 6 | PSCI reboot | `pkg/vmm/vmm.go` run loop | 0 | **Low** (new exit handler) |
| 7 | ARM64 boot (DTB + kernel) | 0 | `pkg/vmm/boot_arm64.go` | **Zero** (new file) |
| 8 | ARM64 guest kernel | `tools/build-guest-kernel.sh` | kernel config | **Zero** (new profile) |
| 9 | ARM64 guest init | `internal/guest/initrd.go` | `init_arm64.bin` | **Zero** (additive embed) |
| 10 | OCI multi-arch | `internal/oci/oci.go` | 0 | **Minimal** (1 line) |

**Total risk to existing x86 path: near zero.** Phases 0-2 are the only ones that touch existing files, and they're mechanical renames + extractions verified by `go test ./...`.

---

## Testing strategy

1. **x86 regression gate:** `go test ./...` must pass after every phase
2. **ARM64 unit tests:** new `_arm64_test.go` files for KVM bindings, PL011, DTB
3. **ARM64 integration:** same test suite as x86 but on an ARM host (Graviton, M-series Mac with Linux VM, or QEMU TCG emulation for CI)
4. **Cross-compile check:** `GOOS=linux GOARCH=arm64 go build ./...` must compile on x86 (won't run, but catches type errors)

---

## Effort estimate

| Phase | Complexity | Reference |
|-------|-----------|-----------|
| 0-2 (split + interface) | Medium | Mechanical refactor, ~2 days |
| 3 (KVM ARM64) | Medium | ~500 lines, follows Linux KVM docs |
| 4 (PL011) | Easy | ~200 lines, simpler than 16550A |
| 5 (GIC) | Medium | ~150 lines, KVM does the heavy lifting |
| 6 (PSCI) | Easy | ~20 lines in run loop |
| 7 (DTB boot) | Medium | ~300 lines, reuse `internal/fdt/` |
| 8-9 (kernel + init) | Easy | Config + cross-compile |
| 10 (OCI) | Trivial | 1 line change |

Firecracker's aarch64 port was ~3000 lines of Rust alongside ~8000 lines of x86. Expect similar ratio here.
