package vmm

import "github.com/gocracker/gocracker/internal/kvm"

// X86VCPUState stores the amd64-specific vCPU state needed for
// snapshot/restore and migration.
//
// The full set mirrors Firecracker's VcpuState (src/vstate/vcpu/src/x86_64.rs):
// Sregs/Regs establish the CPU mode; MPState/LAPIC drive the APIC and halted
// bookkeeping; MSRs carry syscall entry points and kvmclock; XSAVE/XCRs carry
// SSE/AVX state; VCPUEvents carries pending interrupts so a guest captured
// mid-`hlt` wakes on resume instead of sitting idle forever; DebugRegs
// preserves hardware breakpoints; TSCKHz pins the virtual clock frequency.
// Without the last five, a live guest restore looks fine but timekeeping
// stalls and the exec-agent channel never dials back.
type X86VCPUState struct {
	Regs             kvm.Regs        `json:"regs"`
	Sregs            kvm.Sregs       `json:"sregs"`
	MPState          kvm.MPState     `json:"mp_state"`
	LAPIC            *kvm.LAPICState `json:"lapic,omitempty"`
	MSRs             []kvm.MSREntry  `json:"msrs,omitempty"`
	// TSCDeadline is MSR_IA32_TSC_DEADLINE captured separately so the
	// restore path can write it AFTER the main MSR chunk + LAPIC (KVM
	// rejects the write otherwise). If the guest captured this MSR as 0
	// (no armed timer at snapshot time), the capture path rewrites it to
	// the current value of MSR_IA32_TSC so post-restore the first
	// LAPIC-timer comparison "deadline < TSC" is true and the interrupt
	// fires immediately — this is Firecracker's `fix_zero_tsc_deadline_msr`
	// trick and is what actually wakes a HALTED guest from post-restore HLT.
	TSCDeadline uint64          `json:"tsc_deadline,omitempty"`
	FPU         *kvm.FPUState   `json:"fpu,omitempty"`
	XSAVE       *kvm.XSaveState `json:"xsave,omitempty"`
	XCRs        *kvm.XCRsState  `json:"xcrs,omitempty"`
	VCPUEvents  *kvm.VCPUEvents `json:"vcpu_events,omitempty"`
	DebugRegs   *kvm.DebugRegs  `json:"debug_regs,omitempty"`
	TSCKHz      uint32          `json:"tsc_khz,omitempty"`
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

func newX86VCPUState(id int, x86 X86VCPUState) VCPUState {
	return VCPUState{
		ID:      id,
		X86:     &x86,
		Regs:    x86.Regs,
		Sregs:   x86.Sregs,
		MPState: x86.MPState,
		LAPIC:   x86.LAPIC,
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
