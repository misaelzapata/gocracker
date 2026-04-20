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
// snapshot/restore. Every register the guest can observe on resume is
// round-tripped here; skipping any one of them tends to produce silent
// corruption (wrong SIMD state, time warps, trap loops).
type ARM64VCPUState struct {
	// CoreRegs: X0-X30, SP (SP_EL0), PC, PSTATE, plus KVM extensions
	// SP_EL1, ELR_EL1, SPSR[0]. All 64-bit KVM_REG_ARM_CORE ids.
	CoreRegs map[uint64]uint64 `json:"core_regs,omitempty"`
	// FPSIMDRegs: V0-V31 — 128-bit KVM_REG_ARM_CORE ids for the aarch64
	// SIMD/FP register file. The kernel uses these (memcpy, memset,
	// crypto) so restoring zero-initialised vregs corrupts in-flight
	// operations immediately.
	FPSIMDRegs map[uint64][16]byte `json:"fpsimd_regs,omitempty"`
	// FPSR + FPCR: 32-bit FP status/control, also KVM_REG_ARM_CORE.
	FPStatusRegs map[uint64]uint32 `json:"fp_status_regs,omitempty"`
	// SysRegs: every KVM_REG_ARM64_SYSREG exposed via KVM_GET_REG_LIST
	// that is actually writable (SCTLR_EL1, TTBR0/1_EL1, VBAR_EL1, TCR,
	// MAIR, CPACR, timer sysregs, etc.). Read-only feature regs
	// (MIDR_EL1, ID_*) are written back too but skipped if KVM refuses.
	SysRegs map[uint64]uint64 `json:"sys_regs,omitempty"`
	// OtherRegs: anything else GetRegList returns that isn't CORE or
	// SYSREG — on aarch64 that's KVM_REG_ARM_TIMER (virtual timer
	// counter, ctl, cval), captured as 64-bit. Without TIMER_CNT the
	// guest's virtual clock jumps backwards on restore and the scheduler
	// tight-loops trying to make progress.
	OtherRegs map[uint64]uint64 `json:"other_regs,omitempty"`
	// MPState: KVM multiprocessor state (RUNNABLE, STOPPED, HALTED,
	// SUSPENDED). Secondary vCPUs that were in PSCI POWER_OFF at capture
	// must restore as STOPPED, not RUNNABLE, otherwise they execute at
	// whatever stale regs left their PC and trap-loop the host.
	MPState uint32 `json:"mp_state"`
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

// ARM64MachineState carries the in-kernel VGIC state that must be round-tripped
// across snapshot/restore for the guest to receive IRQs (virtio, vsock, timer)
// after resume. Without it, the restored guest's memory-view of the GIC is
// "fully programmed" but the KVM in-kernel GIC is freshly created with every
// IRQ masked — IRQs are delivered via eventfd but dropped by the GIC.
type ARM64MachineState struct {
	// VGIC is nil on pre-VGIC-snapshot snapshots; the restore path treats
	// nil as "no VGIC state to load" and falls back to the old behaviour
	// (fresh GIC). Populated for new snapshots.
	VGIC *VGICSnapshot `json:"vgic,omitempty"`
}

// VGICSnapshot captures a GICv3 in-kernel device's state via
// KVM_GET_DEVICE_ATTR. Layout mirrors Firecracker's aarch64 gic snapshot:
//
//   - DistRegs:   distributor-wide registers (ICFGRn, IPRIORITYRn, ITARGETSRn
//     equivalents on v3, ICENABLERn/ICACTIVERn/ISPENDRn/...).
//     Keyed by register offset into the distributor region.
//   - RedistRegs: per-vCPU redistributor state, keyed by
//     ((mpidr << 32) | offset). mpidr here is the KVM logical vCPU index
//     packed into the upper 32 bits of the device-attr value.
//   - CPUSysRegs: per-vCPU ICC_* system-register shadow state (CTLR, SRE,
//     IGRPEN0/1, PMR, BPR, AP*Rn, RPR). Keyed identically to RedistRegs.
//   - LevelInfo:  per-IRQ line level / latched edge state.
//
// Every value is a single u64 read/written via GET_DEVICE_ATTR on the GIC fd.
// The concrete offsets depend on GIC version and vCPU count, so we enumerate
// them explicitly at capture time and replay at restore time.
type VGICSnapshot struct {
	Version    int             `json:"version"`      // 2 or 3 — must match host
	NrIRQs     uint32          `json:"nr_irqs"`
	DistRegs   map[uint64]uint64 `json:"dist_regs,omitempty"`
	RedistRegs map[uint64]uint64 `json:"redist_regs,omitempty"`
	CPUSysRegs map[uint64]uint64 `json:"cpu_sysregs,omitempty"`
	LevelInfo  map[uint64]uint64 `json:"level_info,omitempty"`
}

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
