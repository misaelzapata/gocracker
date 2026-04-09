//go:build arm64

package kvm

func (s *System) initVMArch(vm *VM) error {
	// ARM64 VM init is handled by the architecture-specific backend once the
	// in-kernel GIC/PSCI wiring lands. Keeping this hook separate removes the
	// hard x86 dependency from CreateVM in the meantime.
	return nil
}
