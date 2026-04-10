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

func (x86MachineBackend) restoreVCPU(_ *kvm.System, _ *kvm.VM, vcpu *kvm.VCPU, state VCPUState) error {
	x86State := state.normalizedX86()
	if err := vcpu.SetMPState(x86State.MPState); err != nil {
		return fmt.Errorf("restore mp_state vcpu %d: %w", vcpu.ID, err)
	}
	if err := vcpu.SetRegs(x86State.Regs); err != nil {
		return fmt.Errorf("restore regs vcpu %d: %w", vcpu.ID, err)
	}
	if err := vcpu.SetSregs(x86State.Sregs); err != nil {
		return fmt.Errorf("restore sregs vcpu %d: %w", vcpu.ID, err)
	}
	if x86State.LAPIC != nil {
		if err := vcpu.SetLAPIC(*x86State.LAPIC); err != nil {
			return fmt.Errorf("restore lapic vcpu %d: %w", vcpu.ID, err)
		}
	} else if err := kvm.SetupLAPIC(vcpu); err != nil {
		return fmt.Errorf("restore default lapic vcpu %d: %w", vcpu.ID, err)
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
