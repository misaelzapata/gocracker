package vmm

import "github.com/gocracker/gocracker/internal/kvm"

// X86VCPUState stores the amd64-specific vCPU state needed for
// snapshot/restore and migration.
type X86VCPUState struct {
	Regs    kvm.Regs        `json:"regs"`
	Sregs   kvm.Sregs       `json:"sregs"`
	MPState kvm.MPState     `json:"mp_state"`
	LAPIC   *kvm.LAPICState `json:"lapic,omitempty"`
}

// ARM64VCPUState stores the aarch64-specific vCPU register state needed for
// snapshot/restore. CoreRegs maps KVM ONE_REG IDs to 64-bit values covering
// X0-X30, SP, PC, and PSTATE.
type ARM64VCPUState struct {
	CoreRegs map[uint64]uint64 `json:"core_regs,omitempty"` // X0-X30, SP, PC, PSTATE
	SysRegs  map[uint64]uint64 `json:"sys_regs,omitempty"`  // SCTLR_EL1, TTBRx, TCR, etc.
}

type VCPUState struct {
	ID      int             `json:"id"`
	X86     *X86VCPUState   `json:"x86,omitempty"`
	ARM64   *ARM64VCPUState `json:"arm64,omitempty"`
	Regs    kvm.Regs        `json:"regs,omitempty"`
	Sregs   kvm.Sregs       `json:"sregs,omitempty"`
	MPState kvm.MPState     `json:"mp_state,omitempty"`
	LAPIC   *kvm.LAPICState `json:"lapic,omitempty"`
}

func newX86VCPUState(id int, regs kvm.Regs, sregs kvm.Sregs, mpState kvm.MPState, lapic *kvm.LAPICState) VCPUState {
	return VCPUState{
		ID:      id,
		X86:     &X86VCPUState{Regs: regs, Sregs: sregs, MPState: mpState, LAPIC: lapic},
		Regs:    regs,
		Sregs:   sregs,
		MPState: mpState,
		LAPIC:   lapic,
	}
}

func (s VCPUState) normalizedX86() X86VCPUState {
	if s.X86 != nil {
		return *s.X86
	}
	return X86VCPUState{
		Regs:    s.Regs,
		Sregs:   s.Sregs,
		MPState: s.MPState,
		LAPIC:   s.LAPIC,
	}
}

func newARM64VCPUState(id int, coreRegs, sysRegs map[uint64]uint64) VCPUState {
	return VCPUState{
		ID:    id,
		ARM64: &ARM64VCPUState{CoreRegs: coreRegs, SysRegs: sysRegs},
	}
}

// X86MachineState stores the amd64-specific in-kernel VM state that must be
// restored alongside RAM and vCPU registers.
type X86MachineState struct {
	Clock    kvm.ClockData `json:"clock"`
	PIT2     kvm.PITState2 `json:"pit2"`
	IRQChips []kvm.IRQChip `json:"irqchips,omitempty"`
}

// ARM64MachineState is reserved for future VGIC/timer/one-reg snapshot state.
type ARM64MachineState struct{}

type SnapshotArchState struct {
	X86   *X86MachineState   `json:"x86,omitempty"`
	ARM64 *ARM64MachineState `json:"arm64,omitempty"`

	// Legacy flat x86 fields kept for backward compatibility with previously
	// generated snapshots and older readers.
	Clock    kvm.ClockData `json:"clock,omitempty"`
	PIT2     kvm.PITState2 `json:"pit2,omitempty"`
	IRQChips []kvm.IRQChip `json:"irqchips,omitempty"`
}

func newX86SnapshotArchState(state *X86MachineState) *SnapshotArchState {
	if state == nil {
		return nil
	}
	return &SnapshotArchState{
		X86:      state,
		Clock:    state.Clock,
		PIT2:     state.PIT2,
		IRQChips: append([]kvm.IRQChip(nil), state.IRQChips...),
	}
}

func newARM64SnapshotArchState() *SnapshotArchState {
	return &SnapshotArchState{ARM64: &ARM64MachineState{}}
}

func (s *SnapshotArchState) normalizedX86() *X86MachineState {
	if s == nil {
		return nil
	}
	if s.X86 != nil {
		return s.X86
	}
	return &X86MachineState{
		Clock:    s.Clock,
		PIT2:     s.PIT2,
		IRQChips: append([]kvm.IRQChip(nil), s.IRQChips...),
	}
}
