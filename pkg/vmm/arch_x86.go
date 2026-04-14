package vmm

import (
	"fmt"

	"github.com/gocracker/gocracker/internal/kvm"
	"github.com/gocracker/gocracker/internal/loader"
	"github.com/gocracker/gocracker/internal/virtio"
)

// x86MachineBackend is a compatibility wrapper around the existing amd64
// runtime path. It keeps the current behavior intact while giving the core
// VMM explicit arch-specific dispatch points.
type x86MachineBackend struct{}

func (x86MachineBackend) setupDevices(vm *VM) error {
	return vm.setupDevices()
}

func (x86MachineBackend) setupIRQs(vm *VM) error {
	return vm.setupIRQs()
}

func (x86MachineBackend) loadKernel(vm *VM) (*loader.KernelInfo, error) {
	return vm.loadKernel()
}

func (x86MachineBackend) postCreateVCPUs(vm *VM) error {
	// Register the per-device eventfds collected in setupDevices against the
	// GSIs set up by setupIRQs. From this point on, writing a uint64(1) into
	// the eventfd injects the interrupt via KVM_IRQFD with no
	// ioctl(KVM_IRQ_LINE) on our side — the same model Firecracker and our
	// arm64 backend use. Order must match setupIRQs: COM1 first, then each
	// transport in append order.
	gsis := make([]uint32, 0, 1+len(vm.transports))
	gsis = append(gsis, COM1IRQ)
	for _, t := range vm.transports {
		gsis = append(gsis, uint32(t.IRQLine()))
	}
	if len(vm.irqEventFds) != len(gsis) {
		return fmt.Errorf("x86 irqfd count mismatch: %d eventfds vs %d GSIs", len(vm.irqEventFds), len(gsis))
	}
	for i, efd := range vm.irqEventFds {
		if err := vm.kvmVM.RegisterIRQFD(efd, gsis[i]); err != nil {
			return fmt.Errorf("register irqfd gsi=%d: %w", gsis[i], err)
		}
	}
	return nil
}

func (x86MachineBackend) setupVCPUsInParallel() bool {
	return true
}

func (x86MachineBackend) setupVCPU(vm *VM, vcpu *kvm.VCPU, index int, kernelInfo *loader.KernelInfo) error {
	if err := kvm.SetupCPUID(vm.kvmSys, vcpu); err != nil {
		return fmt.Errorf("cpuid setup vcpu %d: %w", index, err)
	}
	if err := kvm.SetupMSRs(vcpu); err != nil {
		return fmt.Errorf("msr setup vcpu %d: %w", index, err)
	}
	if err := kvm.SetupFPU(vcpu); err != nil {
		return fmt.Errorf("fpu setup vcpu %d: %w", index, err)
	}
	if err := kvm.SetupLongMode(vcpu, vm.kvmVM.Memory(), kernelInfo.EntryPoint, PageTableBase, BootParamsAddr); err != nil {
		return fmt.Errorf("cpu setup vcpu %d: %w", index, err)
	}
	if err := kvm.SetupLAPIC(vcpu); err != nil {
		return fmt.Errorf("lapic setup vcpu %d: %w", index, err)
	}
	return nil
}

func (x86MachineBackend) captureVCPU(vcpu *kvm.VCPU) (VCPUState, error) {
	return captureVCPUState(vcpu)
}

func (x86MachineBackend) restoreVCPU(sys *kvm.System, _ *kvm.VM, vcpu *kvm.VCPU, state VCPUState) error {
	x86State := state.normalizedX86()
	// Mirror Firecracker's VcpuState::restore order (src/vstate/vcpu/src/x86_64.rs):
	//   CPUID → MSRs → XSAVE → XCRs → DEBUGREGS → LAPIC → MP_STATE → REGS
	//   → SREGS → VCPU_EVENTS → TSC_KHZ → KVMCLOCK_CTRL
	//
	// Why this order and not others:
	//  - CPUID first so later SREGS CR4-bit validation has features to check.
	//  - MSRs before SREGS so TSC/EFER-related MSRs don't race SREGS writes.
	//  - XSAVE before SREGS so CR4.OSXSAVE is valid when SREGS lands.
	//  - XCRs after XSAVE (XSAVE establishes the header, XCRs applies XCR0).
	//  - LAPIC after DEBUGREGS and before MP_STATE (APIC base state drives
	//    MP_STATE semantics for secondary vCPUs).
	//  - MP_STATE before REGS: Firecracker does this to preserve HALTED
	//    semantics; the wake path comes from VCPU_EVENTS + LAPIC timer.
	//  - REGS before SREGS: sets RIP to a canonical long-mode address, then
	//    SREGS confirms the mode.
	//  - VCPU_EVENTS late so any pending exception/IRQ is injected after
	//    all other state is in place — this is what wakes a HALTED vCPU.
	//  - TSC_KHZ + KVMCLOCK_CTRL last so time sync happens after the guest
	//    state is coherent.
	if err := kvm.SetupCPUID(sys, vcpu); err != nil {
		return fmt.Errorf("restore cpuid vcpu %d: %w", vcpu.ID, err)
	}
	if len(x86State.MSRs) > 0 {
		if err := vcpu.SetMSRs(x86State.MSRs); err != nil {
			return fmt.Errorf("restore msrs vcpu %d: %w", vcpu.ID, err)
		}
	}
	if x86State.XSAVE != nil {
		if err := vcpu.SetXSAVE(*x86State.XSAVE); err != nil {
			return fmt.Errorf("restore xsave vcpu %d: %w", vcpu.ID, err)
		}
	} else if x86State.FPU != nil {
		if err := vcpu.SetFPUState(*x86State.FPU); err != nil {
			return fmt.Errorf("restore fpu vcpu %d: %w", vcpu.ID, err)
		}
	}
	if x86State.XCRs != nil {
		if err := vcpu.SetXCRS(*x86State.XCRs); err != nil && !isIgnorableKVMClockCtrlError(err) {
			return fmt.Errorf("restore xcrs vcpu %d: %w", vcpu.ID, err)
		}
	}
	if x86State.DebugRegs != nil {
		if err := vcpu.SetDebugRegs(*x86State.DebugRegs); err != nil {
			return fmt.Errorf("restore debugregs vcpu %d: %w", vcpu.ID, err)
		}
	}
	if x86State.LAPIC != nil {
		if err := vcpu.SetLAPIC(*x86State.LAPIC); err != nil {
			return fmt.Errorf("restore lapic vcpu %d: %w", vcpu.ID, err)
		}
	} else if err := kvm.SetupLAPIC(vcpu); err != nil {
		return fmt.Errorf("restore default lapic vcpu %d: %w", vcpu.ID, err)
	}
	if err := vcpu.SetMPState(x86State.MPState); err != nil {
		return fmt.Errorf("restore mp_state vcpu %d: %w", vcpu.ID, err)
	}
	if err := vcpu.SetRegs(x86State.Regs); err != nil {
		return fmt.Errorf("restore regs vcpu %d: %w", vcpu.ID, err)
	}
	if err := vcpu.SetSregs(x86State.Sregs); err != nil {
		return fmt.Errorf("restore sregs vcpu %d: %w", vcpu.ID, err)
	}
	if x86State.VCPUEvents != nil {
		if err := vcpu.SetVCPUEvents(*x86State.VCPUEvents); err != nil {
			return fmt.Errorf("restore vcpu_events vcpu %d: %w", vcpu.ID, err)
		}
	}
	// Deferred TSC_DEADLINE write: last MSR write, AFTER LAPIC restore and
	// AFTER the main MSR chunk that carries IA32_TSC. Firecracker defers
	// this for the same reason (vcpu.rs:`DEFERRED_MSRS`): KVM validates
	// TSC_DEADLINE against TSC + LAPIC timer mode, so the order must be
	// TSC → LAPIC → TSC_DEADLINE. Without this a modern Linux guest
	// captured mid-`hlt` sits forever in HLT because its LAPIC timer has
	// no deadline programmed.
	if x86State.TSCDeadline != 0 {
		if err := vcpu.SetMSRs([]kvm.MSREntry{{Index: kvm.MSRIA32TSCDeadline, Data: x86State.TSCDeadline}}); err != nil {
			return fmt.Errorf("restore tsc_deadline vcpu %d: %w", vcpu.ID, err)
		}
	}
	if x86State.TSCKHz > 0 {
		if err := vcpu.SetTSCKHz(x86State.TSCKHz); err != nil && !isIgnorableKVMClockCtrlError(err) {
			return fmt.Errorf("restore tsc_khz vcpu %d: %w", vcpu.ID, err)
		}
	}
	if err := vcpu.KVMClockCtrl(); err != nil {
		if isIgnorableKVMClockCtrlError(err) {
			return nil
		}
		return fmt.Errorf("restore kvmclock ctrl vcpu %d: %w", vcpu.ID, err)
	}
	return nil
}

func (x86MachineBackend) captureVMState(vm *VM) (*SnapshotArchState, error) {
	return captureVMArchState(vm)
}

func (x86MachineBackend) restoreVMState(kvmVM *kvm.VM, arch *SnapshotArchState) error {
	return restoreVMArchState(kvmVM, arch)
}

func (x86MachineBackend) handleExit(_ *VM, _ *kvm.VCPU) (handled bool, stop bool, err error) {
	return false, false, nil
}

func (x86MachineBackend) deviceList(vm *VM) []DeviceInfo {
	devs := []DeviceInfo{{Type: "uart", IRQ: COM1IRQ}}
	for i, t := range vm.transports {
		irq := VirtioIRQBase + i
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

func (x86MachineBackend) consoleOutput(vm *VM) []byte {
	if vm.uart0 == nil {
		return nil
	}
	return vm.uart0.OutputBytes()
}

func transportInFSDevices(devices []*virtio.FSDevice, transport *virtio.Transport) bool {
	for _, fsDev := range devices {
		if fsDev != nil && fsDev.Transport == transport {
			return true
		}
	}
	return false
}
