//go:build arm64

package vmm

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"time"
	"unsafe"

	"github.com/gocracker/gocracker/internal/arm64layout"
	"github.com/gocracker/gocracker/internal/fdt"
	"github.com/gocracker/gocracker/internal/kvm"
	"github.com/gocracker/gocracker/internal/loader"
	gclog "github.com/gocracker/gocracker/internal/log"
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
)

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

// makeEventFDIRQFn creates an eventfd and returns an IRQ callback that writes
// to it. Firecracker uses irqfd exclusively — writing a uint64(1) to the
// eventfd causes KVM to inject the interrupt into the GIC without a VMexit.
func (vm *VM) makeEventFDIRQFn() (int, func(bool), error) {
	efd, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		return -1, nil, fmt.Errorf("eventfd: %w", err)
	}
	vm.irqEventFds = append(vm.irqEventFds, efd)
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], 1)
	fn := func(assert bool) {
		if !assert {
			return
		}
		_, _ = unix.Write(efd, buf[:])
	}
	return efd, fn, nil
}

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
		var listenFn func(uint32) (net.Conn, error)
		if vm.cfg.Exec != nil && vm.cfg.Exec.Enabled {
			vm.execBroker = newExecAgentBroker(vm.cfg.Exec.VsockPort)
			listenFn = vm.execBroker.listen
		}
		vsockDev := vsock.NewDevice(mem, base, irq, listenFn, vm.memDirty, irqFn)
		vm.vsockDev = vsockDev
		vm.transports = append(vm.transports, vsockDev.Transport)
		slot++
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

	state := ARM64VCPUState{CoreRegs: regs}
	return VCPUState{
		ID:    vcpu.ID,
		ARM64: &state,
	}, nil
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

	for id, val := range state.ARM64.CoreRegs {
		if err := vcpu.SetOneReg64(id, val); err != nil {
			return fmt.Errorf("restore reg %#x vcpu %d: %w", id, vcpu.ID, err)
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// captureVMState / restoreVMState — phase 1: GIC state deferred
// ---------------------------------------------------------------------------

func (arm64MachineBackend) captureVMState(_ *VM) (*SnapshotArchState, error) {
	return &SnapshotArchState{ARM64: &ARM64MachineState{}}, nil
}

func (arm64MachineBackend) restoreVMState(_ *kvm.VM, _ *SnapshotArchState) error {
	return nil
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
