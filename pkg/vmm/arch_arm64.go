//go:build arm64

package vmm

import (
	"errors"
	"fmt"
	"os"
	"time"
	"unsafe"

	"github.com/gocracker/gocracker/internal/arm64layout"
	"github.com/gocracker/gocracker/internal/fdt"
	"github.com/gocracker/gocracker/internal/kvm"
	"github.com/gocracker/gocracker/internal/loader"
	gclog "github.com/gocracker/gocracker/internal/log"
	"github.com/gocracker/gocracker/internal/rtc"
	"github.com/gocracker/gocracker/internal/runtimecfg"
	"github.com/gocracker/gocracker/internal/uart"
	"github.com/gocracker/gocracker/internal/virtio"
	"github.com/gocracker/gocracker/internal/vsock"
	"golang.org/x/sys/unix"
)

// Firecracker's AArch64 MMIO layout reserves:
//
//	0x40000000 = BOOT_DEVICE (RTC)
//	0x40001000 = RTC
//	0x40002000 = serial slot
//	0x40003000 = first virtio-mmio device
//
// gocracker currently reuses the serial slot at 0x40002000 but exposes an
// ns16550a-compatible UART there, so the guest console stays on ttyS0. The
// Firecracker benchmark path still uses its own PL011/ttyAMA0 console.
const (
	arm64VirtioBase    = 0x40003000 // Firecracker: MEM_32BIT_DEVICES_START
	arm64VirtioStride  = 0x1000     // Firecracker: MMIO_LEN
	arm64VirtioIRQBase = 2          // SPI 2 → INTID 34 (SPI 0=RTC, SPI 1=serial)
	arm64PL011SPI      = 1          // SPI 1 → INTID 33
)

// ARM64 KVM ONE_REG encoding constants (mirrors internal/kvm/cpu_arm64.go
// unexported values so the vmm package can compute register IDs).
const (
	kvmRegArm64   = 0x6000000000000000
	kvmRegSizeU64 = 0x0030000000000000
	kvmRegArmCore = 0x0010 << 16

	// Extended core reg IDs — offsets into KVM's struct kvm_regs (which
	// extends struct user_pt_regs). SP_EL1 sits at byte 272 (0x110) in
	// kvm_regs on aarch64, immediately after pstate (0x108).
	//   struct kvm_regs {
	//     struct user_pt_regs regs;  // ends at byte 0x110 (incl. 8-byte pstate)
	//     __u64 sp_el1;              // 0x110
	//     __u64 elr_el1;             // 0x118
	//     __u64 spsr[5];             // 0x120..0x140
	//     struct user_fpsimd_state fp_regs;
	//   };
	// ID encoding: offset/4 in the low bits, with ARM_CORE coproc.
	arm64ExtraRegSPEL1  = kvmRegArm64 | kvmRegSizeU64 | kvmRegArmCore | (0x110 / 4)
	arm64ExtraRegELREL1 = kvmRegArm64 | kvmRegSizeU64 | kvmRegArmCore | (0x118 / 4)
	arm64ExtraRegSPSR0  = kvmRegArm64 | kvmRegSizeU64 | kvmRegArmCore | (0x120 / 4)
	arm64ExtraRegSPSR1  = kvmRegArm64 | kvmRegSizeU64 | kvmRegArmCore | (0x128 / 4)
	arm64ExtraRegSPSR2  = kvmRegArm64 | kvmRegSizeU64 | kvmRegArmCore | (0x130 / 4)
	arm64ExtraRegSPSR3  = kvmRegArm64 | kvmRegSizeU64 | kvmRegArmCore | (0x138 / 4)
	arm64ExtraRegSPSR4  = kvmRegArm64 | kvmRegSizeU64 | kvmRegArmCore | (0x140 / 4)
)

// isKVMSkippable returns true for errors that mean "this register is not
// settable/gettable in this kernel or this vCPU configuration" — EINVAL
// (rejected by KVM), ENOENT (not in the register table), and ENOTTY
// (ioctl/attr not supported). Non-skippable errors (EFAULT, EPERM,
// EBUSY, etc.) must propagate.
func isKVMSkippable(err error) bool {
	return errors.Is(err, unix.EINVAL) ||
		errors.Is(err, unix.ENOENT) ||
		errors.Is(err, unix.ENOTTY)
}

// arm64CoreRegIDs pre-computes the KVM ONE_REG IDs for X0..X30, SP, PC and
// PSTATE using the same layout offsets that the kernel's struct kvm_regs
// (which wraps struct user_pt_regs) defines.
var arm64CoreRegIDs struct {
	X  [31]uint64 // X0..X30
	SP uint64
	PC uint64
	PS uint64 // PSTATE
}

func init() {
	arm64BackendFactory = func() machineArchBackend { return arm64MachineBackend{} }

	// user_pt_regs layout: regs[31] uint64, sp uint64, pc uint64, pstate uint64
	// The outer kvm_regs struct starts at offset 0 with an embedded user_pt_regs.
	type userPtRegs struct {
		Regs   [31]uint64
		SP     uint64
		PC     uint64
		PState uint64
	}
	type kvmRegsLayout struct {
		Regs userPtRegs
	}
	var layout kvmRegsLayout
	regsArrayOff := unsafe.Offsetof(layout.Regs.Regs)
	for i := 0; i < 31; i++ {
		off := regsArrayOff + uintptr(i)*8
		arm64CoreRegIDs.X[i] = kvmRegArm64 | kvmRegSizeU64 | kvmRegArmCore | uint64(off/4)
	}
	arm64CoreRegIDs.SP = kvmRegArm64 | kvmRegSizeU64 | kvmRegArmCore | uint64(unsafe.Offsetof(layout.Regs.SP)/4)
	arm64CoreRegIDs.PC = kvmRegArm64 | kvmRegSizeU64 | kvmRegArmCore | uint64(unsafe.Offsetof(layout.Regs.PC)/4)
	arm64CoreRegIDs.PS = kvmRegArm64 | kvmRegSizeU64 | kvmRegArmCore | uint64(unsafe.Offsetof(layout.Regs.PState)/4)
}

type arm64MachineBackend struct{}

func (vm *VM) ensureARM64GICLayout() (arm64layout.GICLayout, error) {
	if vm.arm64GICLayout.Valid() {
		return vm.arm64GICLayout, nil
	}
	// 128 IRQs matches Firecracker: 32 private (SGI+PPI) + 96 SPIs.
	const nrIRQs = 128
	layout, err := vm.kvmVM.ProbeGICLayout(vm.cfg.VCPUs, nrIRQs)
	if err != nil {
		return arm64layout.GICLayout{}, err
	}
	vm.arm64GICLayout = layout
	return layout, nil
}

// ---------------------------------------------------------------------------
// setupDevices
// ---------------------------------------------------------------------------

func (arm64MachineBackend) setupDevices(vm *VM) error {
	mem := vm.kvmVM.Memory()
	slot := 0

	// Serial console — gocracker currently exposes an ns16550a-compatible UART
	// in Firecracker's serial MMIO slot at 0x40002000 instead of using PL011.
	// IRQ delivery still goes through irqfd (eventfd -> KVM -> GIC).
	consoleOut := vm.cfg.ConsoleOut
	if consoleOut == nil {
		consoleOut = os.Stdout
	}
	consoleIn := vm.cfg.ConsoleIn
	_, serialIRQFn, err := vm.makeEventFDIRQFn()
	if err != nil {
		return fmt.Errorf("serial eventfd: %w", err)
	}
	vm.uart0 = uart.New(consoleOut, consoleIn, serialIRQFn)

	// PL031 RTC — provides wall clock to the guest kernel at boot.
	vm.rtcDev = rtc.New()

	// virtio-rng
	{
		base := uint64(arm64VirtioBase) + uint64(slot)*arm64VirtioStride
		irq := uint8(arm64VirtioIRQBase + slot)
		_, irqFn, err := vm.makeEventFDIRQFn()
		if err != nil {
			return fmt.Errorf("virtio-rng eventfd: %w", err)
		}
		rng := virtio.NewRNGDevice(mem, base, irq, vm.memDirty, irqFn)
		rng.SetRateLimiter(buildRateLimiter(vm.cfg.RNGRateLimiter))
		vm.rngDev = rng
		vm.transports = append(vm.transports, rng.Transport)
		slot++
	}

	// virtio-balloon
	if vm.cfg.Balloon != nil {
		base := uint64(arm64VirtioBase) + uint64(slot)*arm64VirtioStride
		irq := uint8(arm64VirtioIRQBase + slot)
		_, irqFn, err := vm.makeEventFDIRQFn()
		if err != nil {
			return fmt.Errorf("virtio-balloon eventfd: %w", err)
		}
		balloon := virtio.NewBalloonDevice(mem, base, irq, virtio.BalloonDeviceConfig{
			AmountMiB:            vm.cfg.Balloon.AmountMiB,
			DeflateOnOOM:         vm.cfg.Balloon.DeflateOnOOM,
			StatsPollingInterval: time.Duration(vm.cfg.Balloon.StatsPollingIntervalS) * time.Second,
			SnapshotPages:        append([]uint32(nil), vm.cfg.Balloon.SnapshotPages...),
		}, vm.memDirty, irqFn)
		vm.balloonDev = balloon
		vm.transports = append(vm.transports, balloon.Transport)
		slot++
	}

	// virtio-net
	if vm.cfg.TapName != "" {
		mac := vm.cfg.MACAddr
		if mac == nil {
			mac = defaultGuestMAC(vm.cfg.ID, vm.cfg.TapName)
		}
		base := uint64(arm64VirtioBase) + uint64(slot)*arm64VirtioStride
		irq := uint8(arm64VirtioIRQBase + slot)
		_, irqFn, err := vm.makeEventFDIRQFn()
		if err != nil {
			return fmt.Errorf("virtio-net eventfd: %w", err)
		}
		nd, err := virtio.NewNetDevice(mem, base, irq, mac, vm.cfg.TapName, vm.memDirty, irqFn)
		if err != nil {
			return fmt.Errorf("virtio-net: %w", err)
		}
		nd.SetRateLimiter(buildRateLimiter(vm.cfg.NetRateLimiter))
		vm.netDev = nd
		vm.transports = append(vm.transports, nd.Transport)
		slot++
	}

	// virtio-vsock
	if vm.cfg.Vsock != nil && vm.cfg.Vsock.Enabled {
		base := uint64(arm64VirtioBase) + uint64(slot)*arm64VirtioStride
		irq := uint8(arm64VirtioIRQBase + slot)
		_, irqFn, err := vm.makeEventFDIRQFn()
		if err != nil {
			return fmt.Errorf("virtio-vsock eventfd: %w", err)
		}
		vsockDev := vsock.NewDevice(mem, base, irq, nil, vm.memDirty, irqFn)
		vsockDev.Label = vm.cfg.ID
		vm.vsockDev = vsockDev
		vm.transports = append(vm.transports, vsockDev.Transport)
		slot++

		if err := attachVsockUDSListener(vm); err != nil {
			return err
		}
	}

	// virtio-blk
	for _, drive := range vm.cfg.DriveList() {
		base := uint64(arm64VirtioBase) + uint64(slot)*arm64VirtioStride
		irq := uint8(arm64VirtioIRQBase + slot)
		_, irqFn, err := vm.makeEventFDIRQFn()
		if err != nil {
			return fmt.Errorf("virtio-blk eventfd: %w", err)
		}
		bd, err := virtio.NewBlockDevice(mem, base, irq, drive.Path, drive.ReadOnly, vm.memDirty, irqFn)
		if err != nil {
			return fmt.Errorf("virtio-blk %s: %w", drive.ID, err)
		}
		bd.SetRateLimiter(buildRateLimiter(drive.RateLimiter))
		if drive.Root && vm.blkDev == nil {
			vm.blkDev = bd
		}
		vm.blkDevs = append(vm.blkDevs, bd)
		vm.transports = append(vm.transports, bd.Transport)
		slot++
	}

	// virtio-fs
	for _, fsCfg := range vm.cfg.SharedFS {
		base := uint64(arm64VirtioBase) + uint64(slot)*arm64VirtioStride
		irq := uint8(arm64VirtioIRQBase + slot)
		_, irqFn, err := vm.makeEventFDIRQFn()
		if err != nil {
			return fmt.Errorf("virtio-fs eventfd: %w", err)
		}
		fsDev, err := virtio.NewFSDevice(mem, vm.kvmVM.MemoryFD(), base, irq, fsCfg.Source, fsCfg.Tag, fsCfg.SocketPath, vm.memDirty, irqFn)
		if err != nil {
			return fmt.Errorf("virtio-fs %s: %w", fsCfg.Tag, err)
		}
		vm.fsDevs = append(vm.fsDevs, fsDev)
		vm.transports = append(vm.transports, fsDev.Transport)
		slot++
	}

	// ARM64 RAM starts at a non-zero GPA; tell virtio queues so they can
	// translate guest physical addresses to mem[] offsets.
	base := vm.kvmVM.GuestPhysBase()
	for _, t := range vm.transports {
		t.SetGuestPhysBase(base)
	}

	return nil
}

// ---------------------------------------------------------------------------
// setupIRQs
// ---------------------------------------------------------------------------

func (arm64MachineBackend) setupIRQs(_ *VM) error {
	// ARM64 GSI routing is set up after all vCPU fds exist and the GIC is created,
	// because KVM_SET_GSI_ROUTING requires the in-kernel irqchip to exist.
	return nil
}

func (arm64MachineBackend) postCreateVCPUs(vm *VM) error {
	// Match Firecracker's ordering on aarch64: create all vCPU fds first, then
	// create/configure the irqchip, and only afterwards initialize each vCPU.
	const nrIRQs = 128
	gicLayout, err := vm.ensureARM64GICLayout()
	if err != nil {
		return fmt.Errorf("select arm64 gic layout: %w", err)
	}
	gic, err := vm.kvmVM.CreateGIC(gicLayout, nrIRQs)
	if err != nil {
		return fmt.Errorf("create GIC: %w", err)
	}
	vm.gicDev = gic

	// Set up GSI routing table — maps each GSI to the in-kernel GIC (irqchip=0).
	var gsis []uint32
	gsis = append(gsis, uint32(fdt.DefaultARM64PL011IRQ))
	for i := range vm.transports {
		gsis = append(gsis, uint32(arm64VirtioIRQBase+i))
	}
	if err := vm.kvmVM.SetGSIRoutingGIC(gsis); err != nil {
		return fmt.Errorf("arm64 GSI routing: %w", err)
	}

	// Wire all device eventfds into KVM after the routing table exists.
	if len(vm.irqEventFds) != len(gsis) {
		return fmt.Errorf("irqfd count mismatch: %d eventfds vs %d GSIs", len(vm.irqEventFds), len(gsis))
	}
	for i, efd := range vm.irqEventFds {
		if err := vm.kvmVM.RegisterIRQFD(efd, gsis[i]); err != nil {
			return fmt.Errorf("register irqfd gsi=%d: %w", gsis[i], err)
		}
	}
	gclog.VMM.Info("arm64 irqfd registered", "id", vm.cfg.ID, "count", len(gsis))
	return nil
}

func (arm64MachineBackend) setupVCPUsInParallel() bool {
	return arm64SetupVCPUsInParallel()
}

// ---------------------------------------------------------------------------
// loadKernel
// ---------------------------------------------------------------------------

func (arm64MachineBackend) loadKernel(vm *VM) (*loader.KernelInfo, error) {
	mem := vm.kvmVM.Memory()
	memBytes := uint64(len(mem))
	memBase := uint64(fdt.DefaultARM64MemoryBase)
	memTop := memBase + memBytes

	// Firecracker memory layout (from layout.rs):
	//   DRAM_MEM_START = 0x80000000
	//   SYSTEM_MEM_SIZE = 0x200000 (2 MiB reserved)
	//   kernel_start = DRAM + SYSTEM_MEM_SIZE = 0x80200000
	//   FDT at end of DRAM (top - FDT_MAX_SIZE)
	//   initrd just before FDT

	info, err := loader.LoadArm64Kernel(mem, vm.cfg.KernelPath, memBase)
	if err != nil {
		return nil, err
	}

	cmdline := normalizeARM64KernelCmdline(vm.cfg.Cmdline)

	if len(cmdline)+1 > runtimecfg.KernelCmdlineMax {
		return nil, fmt.Errorf("kernel cmdline too long: %d bytes exceeds limit %d", len(cmdline)+1, runtimecfg.KernelCmdlineMax)
	}

	// DTB goes at end of DRAM (Firecracker: FDT_MAX_SIZE = 2 MiB).
	const fdtMaxSize = 0x200000
	dtbGuestAddr := memTop - fdtMaxSize
	if dtbGuestAddr < info.KernelEnd {
		dtbGuestAddr = (info.KernelEnd + 0xFFF) &^ 0xFFF
	}

	// Initrd goes just before DTB (Firecracker: initrd_load_addr).
	var initrdAddr, initrdSize uint64
	if vm.cfg.InitrdPath != "" {
		initrd, err := os.ReadFile(vm.cfg.InitrdPath)
		if err != nil {
			return nil, fmt.Errorf("read initrd: %w", err)
		}
		initrdSize = uint64(len(initrd))
		alignedSize := (initrdSize + 0xFFF) &^ 0xFFF
		initrdAddr = dtbGuestAddr - alignedSize
		if initrdAddr < info.KernelEnd {
			return nil, fmt.Errorf("initrd (%d bytes) does not fit between kernel end %#x and DTB %#x", initrdSize, info.KernelEnd, dtbGuestAddr)
		}
		if err := copyGuestPayload(mem, memBase, initrdAddr, initrd); err != nil {
			return nil, fmt.Errorf("copy initrd: %w", err)
		}
	}

	// Build list of virtio devices for the DTB.
	var virtioDevs []fdt.VirtioDevice
	for i, t := range vm.transports {
		virtioDevs = append(virtioDevs, fdt.VirtioDevice{
			BaseAddr: t.BasePA(),
			Size:     arm64VirtioStride,
			IRQ:      uint32(arm64VirtioIRQBase + i),
		})
	}

	// Generate the flattened device tree.
	// Firecracker reserves the first 2 MiB of DRAM (SYSTEM_MEM_SIZE) and starts
	// the DTB memory node at DRAM + 0x200000, with size reduced by 0x200000.
	const systemReserve = fdt.DefaultARM64SystemSize // 2 MiB
	gicLayout, err := vm.ensureARM64GICLayout()
	if err != nil {
		return nil, fmt.Errorf("select arm64 gic layout: %w", err)
	}
	dtb, err := fdt.GenerateARM64(fdt.ARM64Config{
		MemBase:       memBase + systemReserve,
		MemBytes:      memBytes - systemReserve,
		CPUs:          vm.cfg.VCPUs,
		Cmdline:       cmdline,
		InitrdAddr:    initrdAddr,
		InitrdSize:    initrdSize,
		GIC:           gicLayout,
		VirtioDevices: virtioDevs,
	})
	if err != nil {
		return nil, fmt.Errorf("generate arm64 dtb: %w", err)
	}

	dtbOffset := dtbGuestAddr - memBase
	if dtbOffset+uint64(len(dtb)) > memBytes {
		return nil, fmt.Errorf("dtb at %#x (%d bytes) exceeds guest RAM", dtbGuestAddr, len(dtb))
	}
	copy(mem[dtbOffset:], dtb)

	info.SetupBase = dtbGuestAddr
	return info, nil
}

// ---------------------------------------------------------------------------
// setupVCPU
// ---------------------------------------------------------------------------

func (arm64MachineBackend) setupVCPU(vm *VM, vcpu *kvm.VCPU, index int, kernelInfo *loader.KernelInfo) error {
	// Get the preferred target type for this host.
	init, err := vm.kvmVM.PreferredARM64Target()
	if err != nil {
		return fmt.Errorf("preferred arm64 target: %w", err)
	}

	// Enable PSCI v0.2 (feature bit 2).
	init.Features[0] |= 1 << kvm.KVMArmVCPUPSCI02

	// Secondary vCPUs (index > 0) must start powered off. The kernel brings
	// them up via PSCI CPU_ON. Without this, all vCPUs start executing at the
	// same PC simultaneously, which crashes the kernel.
	// Matches Firecracker: kvi.features[0] |= 1 << KVM_ARM_VCPU_POWER_OFF
	if index > 0 {
		init.Features[0] |= 1 << kvm.KVMArmVCPUPowerOff
	}

	if err := vcpu.InitARM64(init); err != nil {
		return fmt.Errorf("init arm64 vcpu %d: %w", index, err)
	}

	// Only the boot vCPU (index 0) gets boot registers set.
	// Secondary vCPUs are halted and will be brought up by the kernel.
	if index == 0 {
		if err := vcpu.SetupARM64Boot(kernelInfo.EntryPoint, kernelInfo.SetupBase); err != nil {
			return fmt.Errorf("setup arm64 boot vcpu %d: %w", index, err)
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// handleExit
// ---------------------------------------------------------------------------

func (arm64MachineBackend) handleExit(vm *VM, vcpu *kvm.VCPU) (handled bool, stop bool, err error) {
	// On ARM64, KVM_EXIT_SYSTEM_EVENT is generated by PSCI calls
	// (SYSTEM_OFF / SYSTEM_RESET). Treat any system event as a guest
	// shutdown/reboot request.
	if vcpu.RunData.ExitReason == kvm.ExitSystemEvent {
		gclog.VMM.Info("arm64 system event", "id", vm.cfg.ID, "vcpu", vcpu.ID)
		vm.events.Emit(EventShutdown, "arm64 system event (PSCI)")
		return true, true, nil
	}
	return false, false, nil
}

// ---------------------------------------------------------------------------
// captureVCPU
// ---------------------------------------------------------------------------

func (arm64MachineBackend) captureVCPU(vcpu *kvm.VCPU) (VCPUState, error) {
	regs := make(map[uint64]uint64, 34)

	// X0..X30
	for i := 0; i < 31; i++ {
		val, err := vcpu.GetOneReg64(arm64CoreRegIDs.X[i])
		if err != nil {
			return VCPUState{}, fmt.Errorf("get X%d vcpu %d: %w", i, vcpu.ID, err)
		}
		regs[arm64CoreRegIDs.X[i]] = val
	}
	// SP
	sp, err := vcpu.GetOneReg64(arm64CoreRegIDs.SP)
	if err != nil {
		return VCPUState{}, fmt.Errorf("get SP vcpu %d: %w", vcpu.ID, err)
	}
	regs[arm64CoreRegIDs.SP] = sp

	// PC
	pc, err := vcpu.GetOneReg64(arm64CoreRegIDs.PC)
	if err != nil {
		return VCPUState{}, fmt.Errorf("get PC vcpu %d: %w", vcpu.ID, err)
	}
	regs[arm64CoreRegIDs.PC] = pc

	// PSTATE
	ps, err := vcpu.GetOneReg64(arm64CoreRegIDs.PS)
	if err != nil {
		return VCPUState{}, fmt.Errorf("get PSTATE vcpu %d: %w", vcpu.ID, err)
	}
	regs[arm64CoreRegIDs.PS] = ps

	// Extended core regs beyond struct user_pt_regs — still in ARM_CORE
	// encoding (not SYSREG). KVM's struct kvm_regs contains:
	//   user_pt_regs regs;   (X0-X30, SP=SP_EL0, PC, PState)
	//   u64 sp_el1;
	//   u64 elr_el1;
	//   u64 spsr[5];
	// Missing SP_EL1 is the classic "SP=0 in kernel" symptom: the guest
	// was in EL1h (kernel mode, uses SP_EL1) but we only saved SP_EL0.
	// On restore, SP_EL1 resets to 0 → first kernel push faults.
	// Note: SPSR[1..4] are AArch32 legacy saved-state slots. Restoring them
	// on a pure AArch64 kernel has observed to cause post-restore hangs
	// (vCPU stuck in exception loop); only SPSR[0] (SPSR_EL1, the AArch64
	// EL1 exception context) is load-bearing.
	for _, extra := range []uint64{
		arm64ExtraRegSPEL1,
		arm64ExtraRegELREL1,
		arm64ExtraRegSPSR0,
	} {
		val, err := vcpu.GetOneReg64(extra)
		if err != nil {
			// Some SPSR slots may not be meaningful on all hardware;
			// EINVAL is fine. Harder errors propagate.
			if isKVMSkippable(err) {
				continue
			}
			return VCPUState{}, fmt.Errorf("get extra core reg %#x vcpu %d: %w", extra, vcpu.ID, err)
		}
		regs[extra] = val
	}

	// Enumerate via KVM_GET_REG_LIST and capture EVERYTHING the kernel
	// exposes: sysregs (SCTLR_EL1, TTBR0/1, VBAR, MAIR, TCR, CPACR, timer
	// sysregs…), FPSIMD (V0-V31 128-bit + FPSR/FPCR 32-bit), and timer
	// state (KVM_REG_ARM_TIMER_CNT / CTL / CVAL — not sysregs, a separate
	// coproc). Skipping FPSIMD corrupts in-flight memcpy/memset; skipping
	// TIMER_CNT warps the virtual clock on resume and wedges the scheduler.
	sysRegs, fpsimd, fpStatus, otherRegs, err := captureARM64ExtendedRegs(vcpu)
	if err != nil {
		return VCPUState{}, fmt.Errorf("capture extended regs vcpu %d: %w", vcpu.ID, err)
	}

	// MPState tells KVM whether this vCPU is runnable, halted, or stopped
	// (PSCI OFF for aarch64). Essential for multi-vCPU snapshots: secondary
	// vCPUs in POWER_OFF at capture must restore as STOPPED, otherwise they
	// execute at whatever stale regs left their PC and trap-loop the host.
	mp, err := vcpu.GetMPState()
	if err != nil {
		return VCPUState{}, fmt.Errorf("get mp_state vcpu %d: %w", vcpu.ID, err)
	}

	state := ARM64VCPUState{
		CoreRegs:     regs,
		FPSIMDRegs:   fpsimd,
		FPStatusRegs: fpStatus,
		SysRegs:      sysRegs,
		OtherRegs:    otherRegs,
		MPState:      mp.State,
	}
	return VCPUState{
		ID:    vcpu.ID,
		ARM64: &state,
	}, nil
}

// captureARM64ExtendedRegs enumerates every KVM ONE_REG id this vCPU exposes
// and partitions them into four buckets by size and coproc field:
//
//   - sysRegs    (SYSREG coproc, 64-bit): SCTLR_EL1, TTBR0/1_EL1, VBAR_EL1,
//     TCR_EL1, MAIR_EL1, CPACR_EL1, ESR_EL1, FAR_EL1, PAR_EL1, MIDR_EL1,
//     MPIDR_EL1, CNTV_* timer sysregs, ID_* feature regs, etc.
//   - fpsimd     (ARM_CORE coproc, 128-bit): V0-V31 (the aarch64 SIMD/FP
//     register file, 32 × 128-bit).
//   - fpStatus   (ARM_CORE coproc, 32-bit): FPSR, FPCR.
//   - otherRegs  (any other coproc, 64-bit): KVM_REG_ARM_TIMER_* — the
//     virtual-timer counter, control and compare ids. Not sysregs; they
//     live under the TIMER coproc (0x0011) and are therefore rejected
//     by IsARM64Sysreg but still critical for time-of-day continuity.
//
// Core registers (X0-X30, SP, PC, PSTATE, SP_EL1, ELR_EL1, SPSR[0]) are
// captured separately by the caller and deliberately skipped here. Read-only
// feature registers are captured so the restore path can round-trip them
// (KVM silently ignores writes to read-only regs, reported as EINVAL which
// isKVMSkippable filters).
func captureARM64ExtendedRegs(vcpu *kvm.VCPU) (
	sysRegs map[uint64]uint64,
	fpsimd map[uint64][16]byte,
	fpStatus map[uint64]uint32,
	otherRegs map[uint64]uint64,
	err error,
) {
	ids, err := vcpu.GetRegList()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("KVM_GET_REG_LIST: %w", err)
	}
	sysRegs = make(map[uint64]uint64, len(ids)/2)
	fpsimd = make(map[uint64][16]byte)
	fpStatus = make(map[uint64]uint32)
	otherRegs = make(map[uint64]uint64)
	for _, id := range ids {
		// Core regs (X0-X30, SP, PC, PSTATE, SP_EL1, ELR_EL1, SPSR[0-4])
		// are 64-bit CORE regs captured by the caller; skip here.
		// FPSIMD regs are also ARM_CORE but 128-bit — don't skip those,
		// handle them in the size switch below.
		if kvm.IsARM64CoreReg(id) && kvm.ARM64RegSize(id) == kvm.ARM64RegSizeU64 {
			continue
		}

		switch kvm.ARM64RegSize(id) {
		case kvm.ARM64RegSizeU64:
			val, gerr := vcpu.GetOneReg64(id)
			if gerr != nil {
				if isKVMSkippable(gerr) {
					continue
				}
				return nil, nil, nil, nil, fmt.Errorf("get reg64 %#x: %w", id, gerr)
			}
			if kvm.IsARM64Sysreg(id) {
				sysRegs[id] = val
			} else {
				otherRegs[id] = val
			}
		case kvm.ARM64RegSizeU128:
			// Only ARM_CORE 128-bit regs are the FPSIMD V0-V31 vregs.
			if !kvm.IsARM64CoreReg(id) {
				continue
			}
			val, gerr := vcpu.GetOneReg128(id)
			if gerr != nil {
				if isKVMSkippable(gerr) {
					continue
				}
				return nil, nil, nil, nil, fmt.Errorf("get reg128 %#x: %w", id, gerr)
			}
			fpsimd[id] = val
		case kvm.ARM64RegSizeU32:
			// FPSR/FPCR are ARM_CORE 32-bit. Other 32-bit regs (if KVM
			// ever adds them) land here too; harmless to round-trip.
			val, gerr := vcpu.GetOneReg32(id)
			if gerr != nil {
				if isKVMSkippable(gerr) {
					continue
				}
				return nil, nil, nil, nil, fmt.Errorf("get reg32 %#x: %w", id, gerr)
			}
			fpStatus[id] = val
		default:
			// Unknown size — skip.
		}
	}
	return sysRegs, fpsimd, fpStatus, otherRegs, nil
}

// ---------------------------------------------------------------------------
// restoreVCPU
// ---------------------------------------------------------------------------

func (arm64MachineBackend) restoreVCPU(_ *kvm.System, vm *kvm.VM, vcpu *kvm.VCPU, state VCPUState) error {
	if state.ARM64 == nil {
		return fmt.Errorf("restore arm64 vcpu %d: no ARM64 state in snapshot", state.ID)
	}

	// Use the host's preferred target (e.g. Graviton 1 = target 5).
	// DefaultARM64VCPUInit uses target=0 which fails on some hardware.
	init, err := vm.PreferredARM64Target()
	if err != nil {
		return fmt.Errorf("preferred arm64 target on restore: %w", err)
	}
	init.Features[0] |= 1 << kvm.KVMArmVCPUPSCI02

	if err := vcpu.InitARM64(init); err != nil {
		return fmt.Errorf("init arm64 vcpu %d on restore: %w", vcpu.ID, err)
	}

	// System registers must be set BEFORE core registers: SCTLR_EL1 (MMU),
	// TTBR0/1_EL1 (page tables), VBAR_EL1 (exception vectors), TCR_EL1
	// (translation control), MAIR_EL1 (memory attrs), CPACR_EL1 (FP/SIMD
	// trapping) all need to match the snapshot moment, otherwise the first
	// instruction after PC+PSTATE land in CoreRegs triple-faults.
	//
	// Some sysregs (notably MIDR_EL1, MPIDR_EL1, REVIDR_EL1, ID_* feature
	// regs) are KVM-read-only; attempting SetOneReg64 returns EINVAL. They
	// are harmless to skip — the restored vCPU keeps the host's values,
	// which match what the original vCPU captured (same host, same kernel).
	// We log the skip count at Debug so regressions are visible but do not
	// fail the restore.
	readOnlySkipped := 0
	for id, val := range state.ARM64.SysRegs {
		if err := vcpu.SetOneReg64(id, val); err != nil {
			if isKVMSkippable(err) {
				readOnlySkipped++
				continue
			}
			return fmt.Errorf("restore sysreg %#x vcpu %d: %w", id, vcpu.ID, err)
		}
	}
	if readOnlySkipped > 0 {
		gclog.VMM.Debug("arm64 sysreg restore: skipped read-only regs", "vcpu", vcpu.ID, "count", readOnlySkipped)
	}

	// FPSIMD V0-V31 (128-bit) BEFORE core regs: the kernel may be mid-memcpy
	// at the PC we restore; without Vn restored, the copy writes garbage.
	for id, val := range state.ARM64.FPSIMDRegs {
		if err := vcpu.SetOneReg128(id, val); err != nil {
			if isKVMSkippable(err) {
				continue
			}
			return fmt.Errorf("restore fpsimd %#x vcpu %d: %w", id, vcpu.ID, err)
		}
	}
	for id, val := range state.ARM64.FPStatusRegs {
		if err := vcpu.SetOneReg32(id, val); err != nil {
			if isKVMSkippable(err) {
				continue
			}
			return fmt.Errorf("restore fp status %#x vcpu %d: %w", id, vcpu.ID, err)
		}
	}
	// TIMER_CNT / CTL / CVAL and any future non-sysreg 64-bit regs.
	// Before core regs so CNTV_CVAL comparisons work on first KVM_RUN.
	for id, val := range state.ARM64.OtherRegs {
		if err := vcpu.SetOneReg64(id, val); err != nil {
			if isKVMSkippable(err) {
				continue
			}
			return fmt.Errorf("restore other %#x vcpu %d: %w", id, vcpu.ID, err)
		}
	}

	for id, val := range state.ARM64.CoreRegs {
		if err := vcpu.SetOneReg64(id, val); err != nil {
			return fmt.Errorf("restore reg %#x vcpu %d: %w", id, vcpu.ID, err)
		}
	}

	// MPState must be set AFTER register state — KVM_SET_MP_STATE to
	// RUNNABLE makes KVM_RUN actually execute the vCPU; if regs are still
	// being written to a vcpu that just flipped RUNNABLE, we race the run
	// loop. Secondary vCPUs in STOPPED state at capture come back STOPPED
	// and only wake when the primary issues PSCI_CPU_ON.
	if state.ARM64.MPState != 0 || state.ID == 0 {
		// Skip "unset" (zero) only for secondaries, so fields not populated
		// by old snapshots default to RUNNABLE for the primary (the
		// behaviour before MPState was added). state.ARM64.MPState == 0 is
		// also the encoding for MPStateRunnable, which is the desired
		// default for primary.
		if err := vcpu.SetMPState(kvm.MPState{State: state.ARM64.MPState}); err != nil {
			if !isKVMSkippable(err) {
				return fmt.Errorf("set mp_state vcpu %d: %w", vcpu.ID, err)
			}
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// captureVMState / restoreVMState — VGICv3 state save/restore
// ---------------------------------------------------------------------------

func (arm64MachineBackend) captureVMState(vm *VM) (*SnapshotArchState, error) {
	gic, ok := vm.gicDev.(*kvm.GICDevice)
	if !ok || gic == nil {
		// No GIC yet (early capture?) — return an empty state so the
		// snapshot file structure is valid; restore will fall back to a
		// fresh GIC.
		return &SnapshotArchState{ARM64: &ARM64MachineState{}}, nil
	}
	vgic, err := captureVGICState(gic, len(vm.vcpus), arm64GICNrIRQs)
	if err != nil {
		return nil, fmt.Errorf("capture vgic state: %w", err)
	}
	return &SnapshotArchState{ARM64: &ARM64MachineState{VGIC: vgic}}, nil
}

// restoreVMState runs BEFORE vCPUs are created and before the GIC exists on
// ARM64 — nothing can be restored at this phase. The real work happens in
// restoreVMStatePostIRQ, called after postCreateVCPUs brings the GIC up.
func (arm64MachineBackend) restoreVMState(_ *kvm.VM, _ *SnapshotArchState) error {
	return nil
}

func (arm64MachineBackend) restoreVMStatePostIRQ(vm *VM, arch *SnapshotArchState) error {
	if arch == nil || arch.ARM64 == nil || arch.ARM64.VGIC == nil {
		// Pre-VGIC-snapshot snapshot — nothing to restore. The restored
		// VM will come up with a freshly-initialized GIC and is unlikely
		// to work, but we log and continue rather than fail outright.
		gclog.VMM.Warn("arm64 restore: snapshot has no VGIC state, interrupts may not work")
		return nil
	}
	gic, ok := vm.gicDev.(*kvm.GICDevice)
	if !ok || gic == nil {
		return fmt.Errorf("arm64 restore: VGIC snapshot present but no GIC device")
	}
	if err := restoreVGICState(gic, arch.ARM64.VGIC, len(vm.vcpus)); err != nil {
		return fmt.Errorf("restore vgic state: %w", err)
	}
	return nil
}

// arm64GICNrIRQs matches the value passed to kvm.CreateGIC in postCreateVCPUs.
const arm64GICNrIRQs = 128

// captureVGICState reads every GICv3 register we know how to round-trip via
// KVM_GET_DEVICE_ATTR. Must be called while the VM is paused to get a
// consistent snapshot.
//
// Coverage:
//   - SAVE_PENDING_TABLES: flushes LPI pending state from redistributor
//     caches into guest memory so the memory snapshot captures it.
//   - Distributor regs: CTLR, STATUSR, IGROUPR, ISENABLER, ISPENDR, ISACTIVER,
//     IPRIORITYR, ICFGR, IROUTER.
//   - Redistributor regs (per vCPU, SGI_BASE = 0x10000): CTLR, STATUSR, WAKER,
//     IGROUPR0, ISENABLER0, ISPENDR0, ISACTIVER0, IPRIORITYR (8 words), ICFGR0/1.
//   - CPU sysregs (per vCPU): ICC_SRE, ICC_CTLR, ICC_IGRPEN0/1, ICC_PMR, ICC_BPR0/1,
//     ICC_AP0R/AP1R.
//   - Level info: GICD_ISPENDR-equivalent per-IRQ line state.
func captureVGICState(gic *kvm.GICDevice, nrVCPUs, nrIRQs int) (*VGICSnapshot, error) {
	if gic.Version() != 3 {
		// GICv2 support could be added later; skip for now.
		return &VGICSnapshot{Version: gic.Version(), NrIRQs: uint32(nrIRQs)}, nil
	}

	// Flush LPI pending tables from redistributor caches to guest memory so
	// the memory snapshot captures them. Required by the GICv3 save contract.
	if err := gic.CallCtrl(kvm.VGICCtrlSavePendingTable); err != nil {
		// SavePendingTables may not be supported if LPIs are disabled;
		// older kernels return EINVAL. Log and continue — non-fatal for
		// the vsock use case which uses SPIs, not LPIs.
		gclog.VMM.Debug("vgic save_pending_tables skipped", "error", err)
	}

	snap := &VGICSnapshot{
		Version:    3,
		NrIRQs:     uint32(nrIRQs),
		DistRegs:   make(map[uint64]uint64),
		RedistRegs: make(map[uint64]uint64),
		CPUSysRegs: make(map[uint64]uint64),
		LevelInfo:  make(map[uint64]uint64),
	}

	// --- Distributor registers ---
	for _, off := range vgicDistOffsets(nrIRQs) {
		v, err := gic.GetU32Attr(kvm.VGICGrpDistRegs, uint64(off))
		if err != nil {
			// Some offsets are optional depending on kernel version; skip
			// EINVAL so we don't fail the capture for a single reg.
			if isKVMSkippable(err) {
				continue
			}
			return nil, fmt.Errorf("get dist reg %#x: %w", off, err)
		}
		snap.DistRegs[uint64(off)] = uint64(v)
	}
	for _, off := range vgicDistOffsets64(nrIRQs) {
		v, err := gic.GetU64Attr(kvm.VGICGrpDistRegs, uint64(off))
		if err != nil {
			if isKVMSkippable(err) {
				continue
			}
			return nil, fmt.Errorf("get dist reg64 %#x: %w", off, err)
		}
		// Store 64-bit values at the offset OR'd with a tag bit in the
		// high 32 bits so restore knows to use 64-bit write. We use
		// offset | (1<<63) as the key.
		snap.DistRegs[uint64(off)|vgicWidthBit64] = v
	}

	// --- Redistributor registers, per vCPU ---
	for vcpu := 0; vcpu < nrVCPUs; vcpu++ {
		mpidr := uint64(vcpu) << kvm.VGICV3MPIDRShift
		for _, off := range vgicRedistOffsets() {
			attr := mpidr | uint64(off)
			v, err := gic.GetU32Attr(kvm.VGICGrpRedistRegs, attr)
			if err != nil {
				if isKVMSkippable(err) {
					continue
				}
				return nil, fmt.Errorf("get redist reg vcpu=%d off=%#x: %w", vcpu, off, err)
			}
			snap.RedistRegs[attr] = uint64(v)
		}
	}

	// --- CPU sysregs (ICC_*), per vCPU ---
	for vcpu := 0; vcpu < nrVCPUs; vcpu++ {
		mpidr := uint64(vcpu) << kvm.VGICV3MPIDRShift
		for _, sysreg := range vgicCPUSysregs() {
			attr := mpidr | sysreg
			v, err := gic.GetU64Attr(kvm.VGICGrpCPUSysregs, attr)
			if err != nil {
				if isKVMSkippable(err) {
					continue
				}
				return nil, fmt.Errorf("get cpu sysreg vcpu=%d sysreg=%#x: %w", vcpu, sysreg, err)
			}
			snap.CPUSysRegs[attr] = v
		}
	}

	return snap, nil
}

// restoreVGICState writes back every register captured by captureVGICState.
// The GIC must already be created (typically by postCreateVCPUs).
//
// Iteration MUST follow the deterministic order published by the offset
// helpers (vgicDistOffsets / vgicRedistOffsets / vgicCPUSysregs). Iterating
// the capture maps with random Go map order would apply IS/IC writes in
// arbitrary sequence, producing incorrect end state.
func restoreVGICState(gic *kvm.GICDevice, snap *VGICSnapshot, nrVCPUs int) error {
	if snap == nil {
		return nil
	}
	if gic.Version() != snap.Version {
		return fmt.Errorf("vgic version mismatch: host=%d snapshot=%d", gic.Version(), snap.Version)
	}
	if gic.Version() != 3 {
		return nil
	}

	// Distributor 32-bit regs, in published order (clear-side before set-side).
	for _, off := range vgicDistOffsets(int(snap.NrIRQs)) {
		val, ok := snap.DistRegs[uint64(off)]
		if !ok {
			continue
		}
		if err := gic.SetU32Attr(kvm.VGICGrpDistRegs, uint64(off), uint32(val)); err != nil {
			if isKVMSkippable(err) {
				continue
			}
			return fmt.Errorf("set dist reg %#x: %w", off, err)
		}
	}

	// Distributor 64-bit regs (IROUTER for SPIs).
	for _, off := range vgicDistOffsets64(int(snap.NrIRQs)) {
		val, ok := snap.DistRegs[uint64(off)|vgicWidthBit64]
		if !ok {
			continue
		}
		if err := gic.SetU64Attr(kvm.VGICGrpDistRegs, uint64(off), val); err != nil {
			if isKVMSkippable(err) {
				continue
			}
			return fmt.Errorf("set dist reg64 %#x: %w", off, err)
		}
	}

	// Redistributor regs, per vCPU, in published order.
	for vcpu := 0; vcpu < nrVCPUs; vcpu++ {
		mpidr := uint64(vcpu) << kvm.VGICV3MPIDRShift
		for _, off := range vgicRedistOffsets() {
			attr := mpidr | uint64(off)
			val, ok := snap.RedistRegs[attr]
			if !ok {
				continue
			}
			if err := gic.SetU32Attr(kvm.VGICGrpRedistRegs, attr, uint32(val)); err != nil {
				if isKVMSkippable(err) {
					continue
				}
				return fmt.Errorf("set redist reg vcpu=%d off=%#x: %w", vcpu, off, err)
			}
		}
	}

	// CPU sysregs, per vCPU, in published order.
	for vcpu := 0; vcpu < nrVCPUs; vcpu++ {
		mpidr := uint64(vcpu) << kvm.VGICV3MPIDRShift
		for _, sysreg := range vgicCPUSysregs() {
			attr := mpidr | sysreg
			val, ok := snap.CPUSysRegs[attr]
			if !ok {
				continue
			}
			if err := gic.SetU64Attr(kvm.VGICGrpCPUSysregs, attr, val); err != nil {
				if isKVMSkippable(err) {
					continue
				}
				return fmt.Errorf("set cpu sysreg vcpu=%d sysreg=%#x: %w", vcpu, sysreg, err)
			}
		}
	}

	gclog.VMM.Info("arm64 vgic restored",
		"dist_regs", len(snap.DistRegs),
		"redist_regs", len(snap.RedistRegs),
		"cpu_sysregs", len(snap.CPUSysRegs),
		"vcpus", nrVCPUs,
	)
	return nil
}

// vgicWidthBit64 is OR'd with an offset in DistRegs map keys to tag the value
// as a 64-bit register (stored alongside 32-bit regs in the same map). Bit 63
// is chosen because distributor offsets fit in 20 bits; collisions impossible.
const vgicWidthBit64 = uint64(1) << 63

// vgicDistOffsets returns the 32-bit GICv3 distributor register offsets to
// round-trip, in the ORDER Firecracker uses (src/arch/aarch64/gic/gicv3/
// regs/dist_regs.rs). IS/IC pairs ARE both present and the order matters:
// writing ICENABLER first (with its captured "effective enable" bits) clears
// everything, then ISENABLER (same bits) re-enables the right ones. Random
// map iteration here would corrupt the end state; callers must iterate this
// slice in order.
func vgicDistOffsets(nrIRQs int) []uint32 {
	const (
		GICD_CTLR       = 0x0000
		GICD_STATUSR    = 0x0010
		GICD_IGROUPR    = 0x0080 // nr_IRQs/32 words
		GICD_ICENABLER  = 0x0180
		GICD_ISENABLER  = 0x0100
		GICD_ICPENDR    = 0x0280
		GICD_ISPENDR    = 0x0200
		GICD_ICACTIVER  = 0x0380
		GICD_ISACTIVER  = 0x0300
		GICD_IPRIORITYR = 0x0400 // nr_IRQs bytes (one byte per IRQ, packed 4/word)
		GICD_ICFGR      = 0x0C00 // nr_IRQs*2 bits (16 IRQs/word)
	)
	offs := []uint32{GICD_CTLR, GICD_STATUSR}
	nrWords := uint32(nrIRQs) / 32
	addBlock := func(base uint32, words uint32) {
		for i := uint32(0); i < words; i++ {
			offs = append(offs, base+i*4)
		}
	}
	addBlock(GICD_IGROUPR, nrWords)
	// Clear-side writes FIRST (reset to zero), then set-side writes apply
	// the captured pattern. Mirrors Firecracker order.
	addBlock(GICD_ICENABLER, nrWords)
	addBlock(GICD_ISENABLER, nrWords)
	addBlock(GICD_ICPENDR, nrWords)
	addBlock(GICD_ISPENDR, nrWords)
	addBlock(GICD_ICACTIVER, nrWords)
	addBlock(GICD_ISACTIVER, nrWords)
	addBlock(GICD_IPRIORITYR, uint32(nrIRQs)/4)
	addBlock(GICD_ICFGR, uint32(nrIRQs)/16)
	return offs
}

// vgicDistOffsets64 returns the 64-bit GICv3 distributor register offsets.
// GICD_IROUTER is the only 64-bit distributor reg we care about: it routes
// each SPI (id ≥ 32) to a specific target vCPU (affinity). Without these,
// every SPI (virtio-blk, virtio-net, virtio-vsock) lands on "no CPU" and
// is silently dropped after restore.
func vgicDistOffsets64(nrIRQs int) []uint32 {
	const GICD_IROUTER = 0x6000
	// Entries for IDs 0-31 (SGI/PPI) are per-redist, not here. SPIs are 32+.
	offs := make([]uint32, 0, nrIRQs-32)
	for id := 32; id < nrIRQs; id++ {
		offs = append(offs, GICD_IROUTER+uint32(id)*8)
	}
	return offs
}

// vgicRedistOffsets returns the per-vCPU redistributor offsets to round-trip,
// in the order Firecracker uses (src/arch/aarch64/gic/gicv3/regs/
// redist_regs.rs). IS/IC pairs are both restored, clear-side first.
func vgicRedistOffsets() []uint32 {
	const (
		GICR_CTLR       = 0x00000
		GICR_STATUSR    = 0x00010
		GICR_WAKER      = 0x00014
		SGI_BASE        = 0x10000
		GICR_IGROUPR0   = SGI_BASE + 0x080
		GICR_ICENABLER0 = SGI_BASE + 0x180
		GICR_ISENABLER0 = SGI_BASE + 0x100
		GICR_ICPENDR0   = SGI_BASE + 0x280
		GICR_ISPENDR0   = SGI_BASE + 0x200
		GICR_ICACTIVER0 = SGI_BASE + 0x380
		GICR_ISACTIVER0 = SGI_BASE + 0x300
		GICR_IPRIORITYR = SGI_BASE + 0x400 // 32 PPI+SGI → 8 words
		GICR_ICFGR0     = SGI_BASE + 0xC00
		GICR_ICFGR1     = SGI_BASE + 0xC04
	)
	offs := []uint32{
		GICR_CTLR, GICR_STATUSR, GICR_WAKER,
		GICR_IGROUPR0,
		GICR_ICENABLER0, GICR_ISENABLER0,
		GICR_ICPENDR0, GICR_ISPENDR0,
		GICR_ICACTIVER0, GICR_ISACTIVER0,
		GICR_ICFGR0, GICR_ICFGR1,
	}
	for i := uint32(0); i < 8; i++ {
		offs = append(offs, GICR_IPRIORITYR+i*4)
	}
	return offs
}

// vgicCPUSysregs returns the KVM_DEV_ARM_VGIC_GRP_CPU_SYSREGS "attr" values
// for the ICC_* system registers we save/restore.
//
// Encoding: sys_reg(op0, op1, crn, crm, op2) from Linux arch/arm64/include/
// asm/sysreg.h — bits Op0<<19, Op1<<16, CRn<<12, CRm<<8, Op2<<5. This is a
// DIFFERENT encoding from KVM_REG_ARM64_SYSREG's ONE_REG encoding (which
// uses Op0<<14 / Op1<<11 / CRn<<7 / CRm<<3 / Op2<<0); GRP_CPU_SYSREGS uses
// the sys_reg() encoding because KVM dispatches these attrs through the
// same table it uses for guest EL1 MSR/MRS trap emulation.
//
// ICC_SRE_EL1     = sys_reg(3,0,12,12,5)
// ICC_CTLR_EL1    = sys_reg(3,0,12,12,4)
// ICC_IGRPEN0_EL1 = sys_reg(3,0,12,12,6)
// ICC_IGRPEN1_EL1 = sys_reg(3,0,12,12,7)
// ICC_PMR_EL1     = sys_reg(3,0, 4, 6,0)
// ICC_BPR0_EL1    = sys_reg(3,0,12, 8,3)
// ICC_BPR1_EL1    = sys_reg(3,0,12,12,3)
// ICC_AP0Rn_EL1   = sys_reg(3,0,12, 8,4..7)  (n=0..3)
// ICC_AP1Rn_EL1   = sys_reg(3,0,12, 9,0..3)  (n=0..3)
func vgicCPUSysregs() []uint64 {
	// Correct encoding per arch/arm64/kvm/vgic/vgic-sys-reg-v3.c
	// vgic_v3_cpu_sysregs_uaccess:
	//   params.Op0 = (sysreg >> 14) & 0x3
	//   params.Op1 = (sysreg >> 11) & 0x7
	//   params.CRn = (sysreg >>  7) & 0xf
	//   params.CRm = (sysreg >>  3) & 0xf
	//   params.Op2 = (sysreg >>  0) & 0x7
	encode := func(op0, op1, crn, crm, op2 uint64) uint64 {
		return (op0 << 14) | (op1 << 11) | (crn << 7) | (crm << 3) | op2
	}
	var out []uint64
	// Primary control regs
	out = append(out, encode(3, 0, 12, 12, 5)) // ICC_SRE_EL1
	out = append(out, encode(3, 0, 12, 12, 4)) // ICC_CTLR_EL1
	out = append(out, encode(3, 0, 12, 12, 6)) // ICC_IGRPEN0_EL1
	out = append(out, encode(3, 0, 12, 12, 7)) // ICC_IGRPEN1_EL1
	out = append(out, encode(3, 0, 4, 6, 0))   // ICC_PMR_EL1
	out = append(out, encode(3, 0, 12, 8, 3))  // ICC_BPR0_EL1
	out = append(out, encode(3, 0, 12, 12, 3)) // ICC_BPR1_EL1
	// Active priority regs (4 banks each for group 0 and group 1)
	for i := uint64(0); i < 4; i++ {
		out = append(out, encode(3, 0, 12, 8, 4+i)) // ICC_AP0R0..3_EL1
		out = append(out, encode(3, 0, 12, 9, 0+i)) // ICC_AP1R0..3_EL1
	}
	return out
}

// ---------------------------------------------------------------------------
// deviceList
// ---------------------------------------------------------------------------

func (arm64MachineBackend) deviceList(vm *VM) []DeviceInfo {
	devs := []DeviceInfo{{Type: "uart", IRQ: int(fdt.DefaultARM64PL011IRQ)}}
	for i, t := range vm.transports {
		irq := arm64VirtioIRQBase + i
		typ := "virtio-unknown"
		switch {
		case vm.rngDev != nil && t == vm.rngDev.Transport:
			typ = "virtio-rng"
		case vm.balloonDev != nil && t == vm.balloonDev.Transport:
			typ = "virtio-balloon"
		case vm.netDev != nil && t == vm.netDev.Transport:
			typ = "virtio-net"
		case transportInBlockDevices(vm.blkDevs, t):
			typ = "virtio-blk"
		case vm.vsockDev != nil && t == vm.vsockDev.Transport:
			typ = "virtio-vsock"
		case transportInFSDevices(vm.fsDevs, t):
			typ = "virtio-fs"
		}
		devs = append(devs, DeviceInfo{Type: typ, IRQ: irq})
	}
	return devs
}

// ---------------------------------------------------------------------------
// consoleOutput
// ---------------------------------------------------------------------------

func (arm64MachineBackend) consoleOutput(vm *VM) []byte {
	if vm.uart0 != nil {
		return vm.uart0.OutputBytes()
	}
	return nil
}
