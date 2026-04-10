// This file contains pure data type definitions used for snapshot serialization.
// These types have no platform-specific dependencies and compile on all targets.
// The KVM ioctl bindings that operate on these types live in kvm.go (linux-only).
package kvm

// Regs holds general-purpose x86_64 registers.
type Regs struct {
	RAX, RBX, RCX, RDX uint64
	RSI, RDI, RSP, RBP uint64
	R8, R9, R10, R11   uint64
	R12, R13, R14, R15 uint64
	RIP, RFLAGS        uint64
}

// Segment describes an x86 segment descriptor.
type Segment struct {
	Base                           uint64
	Limit                          uint32
	Selector                       uint16
	Type                           uint8
	Present, DPL, DB, S, L, G, AVL uint8
	Unusable                       uint8
	_                              uint8
}

// DTTR describes a descriptor table register (GDT/IDT).
// Must match struct kvm_dtable: { __u64 base; __u16 limit; __u16 padding[3]; }
type DTTR struct {
	Base    uint64
	Limit   uint16
	Padding [3]uint16
}

// Sregs holds special x86_64 registers (segments, control regs).
type Sregs struct {
	CS, DS, ES, FS, GS, SS  Segment
	TR, LDT                 Segment
	GDT, IDT                DTTR
	CR0, CR2, CR3, CR4, CR8 uint64
	EFER                    uint64
	ApicBase                uint64
	InterruptBitmap         [4]uint64
}

// ClockData matches struct kvm_clock_data.
type ClockData struct {
	Clock    uint64
	Flags    uint32
	Pad0     uint32
	Realtime uint64
	HostTSC  uint64
	Pad      [4]uint32
}

// IRQChip matches struct kvm_irqchip and stores the raw per-chip state.
type IRQChip struct {
	ChipID uint32
	Pad    uint32
	Chip   [512]byte
}

// PITState2 matches struct kvm_pit_state2 as an opaque round-tripped blob.
type PITState2 struct {
	Data [112]byte
}

// LAPICState matches struct kvm_lapic_state.
type LAPICState struct {
	Regs [1024]byte
}

// MPState matches struct kvm_mp_state.
type MPState struct {
	State uint32
}

// Public constants for snapshot and IRQ chip identification.
const (
	IRQChipPicMaster = 0
	IRQChipPicSlave  = 1
	IRQChipIOAPIC    = 2

	ClockTSCStable = 1 << 1
)
