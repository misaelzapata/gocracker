//go:build amd64

package kvm

import (
	"fmt"
	"unsafe"
)

func (s *System) initVMArch(vm *VM) error {
	// TSS must be set first on Intel (before IRQCHIP).
	if _, err := vmIoctl(vm.fd, kvmSetTSSAddr, 0xFFFBD000); err != nil {
		return fmt.Errorf("KVM_SET_TSS_ADDR: %w", err)
	}

	// In-kernel APIC / IRQ chip.
	if _, err := vmIoctl(vm.fd, uintptr(kvmCreateIRQChip), 0); err != nil {
		return fmt.Errorf("KVM_CREATE_IRQCHIP: %w", err)
	}

	// PIT2 provides timer interrupts. KVM_PIT_SPEAKER_DUMMY (flags=1) makes
	// KVM handle port 0x61 internally.
	pit := PITConfig{Flags: 1}
	if _, err := vmIoctl(vm.fd, kvmCreatePIT2, uintptr(unsafe.Pointer(&pit))); err != nil {
		return fmt.Errorf("KVM_CREATE_PIT2: %w", err)
	}

	return nil
}
