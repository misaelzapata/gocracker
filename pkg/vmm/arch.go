package vmm

import (
	"fmt"

	"github.com/gocracker/gocracker/internal/kvm"
	"github.com/gocracker/gocracker/internal/loader"
)

type machineArchBackend interface {
	setupDevices(*VM) error
	setupIRQs(*VM) error
	loadKernel(*VM) (*loader.KernelInfo, error)
	postCreateVCPUs(*VM) error
	setupVCPUsInParallel() bool
	setupVCPU(*VM, *kvm.VCPU, int, *loader.KernelInfo) error
	captureVCPU(*kvm.VCPU) (VCPUState, error)
	restoreVCPU(*kvm.System, *kvm.VM, *kvm.VCPU, VCPUState) error
	captureVMState(*VM) (*SnapshotArchState, error)
	restoreVMState(*kvm.VM, *SnapshotArchState) error
	// restoreVMStatePostIRQ runs AFTER postCreateVCPUs, so the interrupt
	// controller (x86 IRQCHIP already up, arm64 VGIC just created) exists.
	// Needed on ARM64 because the VGIC cannot be restored before it is
	// created; no-op on x86.
	restoreVMStatePostIRQ(*VM, *SnapshotArchState) error
	handleExit(*VM, *kvm.VCPU) (handled bool, stop bool, err error)
	deviceList(*VM) []DeviceInfo
	consoleOutput(*VM) []byte
}

// arm64BackendFactory is set by arch_arm64.go init() when compiled for arm64.
var arm64BackendFactory func() machineArchBackend

func newMachineArchBackend(arch MachineArch) (machineArchBackend, error) {
	switch arch {
	case ArchAMD64:
		return x86MachineBackend{}, nil
	case ArchARM64:
		if arm64BackendFactory != nil {
			return arm64BackendFactory(), nil
		}
		return nil, fmt.Errorf("arch %q backend is not available in this build", arch)
	default:
		return nil, fmt.Errorf("invalid arch %q", arch)
	}
}
