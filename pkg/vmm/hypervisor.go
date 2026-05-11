package vmm

import "io"

// Hypervisor abstracts the host-kernel hypervisor (KVM on Linux, WHP on
// Windows, HVF on macOS in a future port). The current code base is
// written against KVM concretely; this interface is introduced in Phase 1
// of the Windows port plan so a second backend (WHP) can land in Phase 2
// without touching device emulation code.
//
// Naming: HV* prefix identifies hypervisor-abstracted handles (HVVM,
// HVVCPU). The portable Registers / SegmentRegisters / ExitContext shapes
// are kept separate from kvm-package types so a snapshot taken on Linux
// does not embed Linux UAPI structs in the on-disk format (snapshot v2,
// Phase 3).
//
// Both the KVM adapter ([pkg/vmm/hypervisor_kvm.go]) and the future WHP
// adapter ([pkg/vmm/hypervisor_whp.go]) implement this interface. The
// concrete type lives behind a build tag so non-Linux/non-Windows targets
// fall through to a third stub that errors out at runtime.
type Hypervisor interface {
	// Capabilities returns runtime feature flags (dirty-page tracking,
	// in-kernel IRQ chip, vCPU pause/resume, etc.). Callers gate features
	// at construction time rather than failing later in the run loop.
	Capabilities() HVCapabilities

	// CreateVM opens a new partition. Memory regions are added afterwards
	// via HVVM.MapMemory. Calling Close on the returned HVVM tears the
	// partition down and releases all vCPUs.
	CreateVM(cfg HVVMConfig) (HVVM, error)

	// Close releases the system handle (Linux: closes /dev/kvm; Windows:
	// frees the dynamic WinHvPlatform.dll handle).
	Close() error
}

// HVVMConfig is the minimal shape the hypervisor needs to size a partition.
// Device-emulation parameters (NIC backends, block devices, etc.) live in
// pkg/vmm.Config and stay above this layer.
type HVVMConfig struct {
	NumVCPUs int

	// MemoryBytes is the total guest RAM. Backends may map it in one or
	// more contiguous regions (KVM: typically one slot; WHP: one or more
	// WHvMapGpaRange calls) — that is the backend's choice.
	MemoryBytes uint64

	// EnableDirtyTracking turns on per-page dirty-bitmap collection. Used
	// by snapshot/migrate. Adds non-trivial cost; off by default.
	EnableDirtyTracking bool
}

// HVCapabilities exposes whether optional features are available. Each is a
// bool because the absence/presence model maps cleanly across KVM
// (KVM_CHECK_EXTENSION) and WHP (WHvGetCapability).
type HVCapabilities struct {
	DirtyPageTracking bool // KVM_CAP_DIRTY_LOG_RING / WHV_PARTITION_PROPERTY_CODE_DIRTY_PAGE_TRACKING
	InKernelIRQChip   bool // KVM_CAP_IRQCHIP / always-on synthetic interrupt chip on WHP
	PauseResume       bool // KVM_CAP_VCPU_PAUSE / WHvCancelRunVirtualProcessor + suspend property
	XSAVE             bool // KVM_CAP_XSAVE / WHV_X64_REGISTER_XSTATE family
	MaxVCPUs          int  // upper bound enforced by the backend
}

// HVVM is one virtualization partition. Memory regions and vCPUs hang off
// it. Implementations are NOT goroutine-safe for mutation operations
// (MapMemory, CreateVCPU, Close) — callers must serialise those — but
// concurrent reads (Capabilities, QueryDirtyBitmap on disjoint ranges)
// are allowed.
type HVVM interface {
	// AllocateGuestRAM returns a pinned host-memory slice the caller can
	// write into (e.g. to load a kernel image) and later MapMemory into
	// the guest. On Linux this is mmap-backed; on Windows it's backed by
	// windows.VirtualAlloc. The HVVM owns the underlying allocation —
	// closing the HVVM releases it. Use this rather than passing a
	// Go-managed slice to MapMemory, which can be moved by the GC.
	AllocateGuestRAM(size uint64) ([]byte, error)

	// MapMemory makes hostMem visible to the guest at gpa. The slice's
	// underlying pages must remain pinned for the lifetime of the
	// mapping. Use AllocateGuestRAM to obtain a guaranteed-pinned slice.
	// Slot allocation is internal; backends pick one.
	MapMemory(gpa uint64, hostMem []byte, flags MemFlags) error

	// UnmapMemory removes a previously-mapped range. gpa+size must match
	// a prior MapMemory call; partial unmaps are not supported.
	UnmapMemory(gpa uint64, size uint64) error

	// QueryDirtyBitmap fills bitmap with 1-bit-per-page dirty flags for
	// the given range. Pages are 4 KiB. Backends clear the dirty state
	// after the query (matches KVM_GET_DIRTY_LOG semantics).
	QueryDirtyBitmap(gpa uint64, size uint64, bitmap []byte) error

	// CreateVCPU returns a new virtual processor handle indexed by idx
	// (0..NumVCPUs-1). On WHP this calls WHvCreateVirtualProcessor; on
	// KVM, KVM_CREATE_VCPU.
	CreateVCPU(idx int) (HVVCPU, error)

	// InjectInterrupt is the unified interrupt injection path. Callers
	// no longer plumb eventfds + KVM_IRQFD; instead, virtio devices are
	// constructed with an IRQLine callback that ultimately routes here.
	// On KVM the implementation triggers an irqfd; on WHP it calls
	// WHvRequestInterrupt.
	InjectInterrupt(req InterruptRequest) error

	// Close tears down the partition, the vCPU mmaps, and the kernel-
	// side state. After Close, all HVVCPUs from this VM are invalid.
	Close() error
}

// HVVCPU is a virtual processor. Run loops drive it, and register state is
// captured/restored through Get/SetRegisters in portable form.
//
// A minimal lifecycle: CreateVCPU → SetRegisters (boot state) → Run loop
// (handle exits, possibly InjectInterrupt) → Close.
type HVVCPU interface {
	// ID is the vCPU index, matches the idx passed to CreateVCPU.
	ID() int

	// Run executes the vCPU until it exits. Returns the exit context
	// describing why. Implementations must be cancellable from another
	// goroutine — KVM uses a signal + KVM_RUN's immediate-exit flag,
	// WHP uses WHvCancelRunVirtualProcessor.
	Run() (ExitContext, error)

	// Cancel asks an in-flight Run to return promptly. Idempotent and
	// safe to call from any goroutine. The Run returns with
	// ExitReasonCancelled — the backend never blocks indefinitely.
	Cancel() error

	// GetRegisters / SetRegisters copy general-purpose register state
	// in the portable Registers shape. Backends translate to/from native
	// register layouts (KVM kvm_regs, WHP WHV_REGISTER_VALUE arrays).
	GetRegisters() (Registers, error)
	SetRegisters(Registers) error

	// GetSegmentRegisters / SetSegmentRegisters cover x86 segment +
	// control registers (CRn, EFER, GDT/IDT, segment selectors). On
	// ARM64 hosts these methods return ErrUnsupportedArch.
	GetSegmentRegisters() (SegmentRegisters, error)
	SetSegmentRegisters(SegmentRegisters) error

	// HandleMMIORead / HandleMMIOWrite are convenience wrappers for the
	// most common exit paths. Implementations decode the exit context's
	// data field and dispatch to the device tree the caller maintains.
	// (Provided so virtio device code stays hypervisor-agnostic.)

	// Close releases backend-side vCPU resources. Cannot be called while
	// Run is in flight; cancel first.
	Close() error
}

// Registers is the portable general-purpose register set for x86_64. ARM64
// hosts use a separate Aarch64Registers struct (defined in
// hypervisor_arm64.go); since ARM64 ports are deferred (see plan), only
// x86_64 is fleshed out here.
//
// Layout deliberately mirrors kvm.Regs for cheap round-trip on Linux. On
// Windows the WHP backend builds a WHV_REGISTER_VALUE table by reading
// these fields in a fixed order — see hypervisor_whp.go for the mapping.
type Registers struct {
	RAX, RBX, RCX, RDX uint64
	RSI, RDI, RSP, RBP uint64
	R8, R9, R10, R11   uint64
	R12, R13, R14, R15 uint64
	RIP, RFLAGS        uint64
}

// SegmentRegisters mirrors kvm.Sregs in shape. Each Segment captures the
// hidden state cached by the CPU after a load; backends translate them to
// their hypervisor-native layout.
type SegmentRegisters struct {
	CS, DS, ES, FS, GS, SS Segment
	TR, LDT                Segment
	GDT, IDT               DescriptorTable
	CR0, CR2, CR3, CR4, CR8 uint64
	EFER                   uint64
	ApicBase               uint64
	InterruptBitmap        [4]uint64
}

// Segment is a single x86 segment descriptor in unpacked form.
type Segment struct {
	Base     uint64
	Limit    uint32
	Selector uint16
	Type     uint8
	Present  uint8
	DPL      uint8
	DB       uint8
	S        uint8
	L        uint8
	G        uint8
	AVL      uint8
	Unusable uint8
}

// DescriptorTable is the GDT or IDT pseudo-descriptor (base + limit).
type DescriptorTable struct {
	Base  uint64
	Limit uint16
}

// MemFlags enumerates GPA range protection flags. Mirrors WHvMapAccess /
// the (W|R|X) bits in KVM_MEM_*.
type MemFlags uint32

const (
	MemRead    MemFlags = 1 << 0
	MemWrite   MemFlags = 1 << 1
	MemExecute MemFlags = 1 << 2
	// MemTrackDirty enables per-page dirty bitmap collection. The
	// partition must have been created with EnableDirtyTracking.
	MemTrackDirty MemFlags = 1 << 3

	// MemRWX is shorthand for the common case (full RAM mapping).
	MemRWX = MemRead | MemWrite | MemExecute
)

// InterruptRequest is the unified shape for hypervisor-side interrupt
// injection. KVM's KVM_IRQ_LINE (gsi-keyed) and WHP's
// WHvRequestInterrupt (vector+vCPU+destination) both fit.
type InterruptRequest struct {
	// VCPU is the destination vCPU index. -1 means "any" (round-robin
	// destination handled by the hypervisor's IOAPIC/synthetic chip).
	VCPU int

	// Vector is the x86 interrupt vector (0–255) for level-triggered
	// or edge-triggered MSI/MSI-X. Ignored on ARM64 (uses IRQNumber).
	Vector uint8

	// IRQNumber is the legacy GSI / ARM SPI number. On x86, used by
	// virtio-mmio devices that hang off the IOAPIC; on ARM64, used by
	// the GIC.
	IRQNumber uint32

	// Level controls assertion direction. 1 = assert, 0 = deassert.
	// MSI-style edge interrupts only need level=1.
	Level uint32

	// Edge if true marks the interrupt as edge-triggered (MSI-style).
	// Level-triggered interrupts (legacy IOAPIC) must be deasserted by
	// the device emulation when the guest acknowledges.
	Edge bool
}

// IRQLine is the per-virtio-device callback that replaces the eventfd +
// KVM_IRQFD plumbing. virtio devices in pkg/vmm and internal/virtio are
// being refactored (Phase 1 part 2) to receive an IRQLine at construction
// rather than an eventfd. KVM impl bridges this to an internal IRQFD
// behind the scenes; WHP impl calls WHvRequestInterrupt directly.
//
// level=1 asserts; level=0 deasserts. For edge-triggered devices, the
// caller pulses 1 then 0 (or just 1 for MSI).
type IRQLine func(level int) error

// ExitContext describes why a vCPU returned from Run. The shape unifies
// KVM exit reasons and WHP exit contexts. Backends decode their native
// representation into this shape before returning from Run.
type ExitContext struct {
	Reason ExitReason

	// RIP at the time of the exit. Useful for logging and for the WHP
	// path that needs to advance past emulated instructions.
	RIP uint64

	// InstructionLength is the size in bytes of the trapped
	// instruction. The WHP backend reports this; KVM does not (the
	// kernel auto-advances RIP after most exits). Used by the WHP path
	// to advance RIP after MMIO / IOPort / CPUID / MSR emulation. Zero
	// means the backend didn't report it; the caller must decode the
	// instruction or pick a sensible default.
	InstructionLength uint8

	// Populated when Reason == ExitReasonMMIO.
	MMIO MMIOExit

	// Populated when Reason == ExitReasonIOPort.
	IOPort IOPortExit

	// Populated when Reason == ExitReasonCPUID. Set by WHP only — KVM
	// handles CPUID via KVM_SET_CPUID2 ahead of time. The MMIO/IO codepath
	// is the same on both backends; this is here so the WHP run loop
	// has a place to stash decoded values.
	CPUID CPUIDExit

	// FailureMsg carries any backend-specific diagnostic when the exit
	// is fatal (Reason == ExitReasonInternal / ExitReasonTriple).
	FailureMsg string
}

// ExitReason enumerates the exits the higher-level run loop knows how to
// dispatch. New reasons can be added without breaking adapters; unknown
// reasons map to ExitReasonInternal with a descriptive FailureMsg.
type ExitReason int

const (
	ExitReasonUnknown ExitReason = iota
	ExitReasonMMIO
	ExitReasonIOPort
	ExitReasonHalt
	ExitReasonShutdown      // immediate guest shutdown (KVM_EXIT_SHUTDOWN, triple-fault on WHP)
	ExitReasonSystemEvent   // arch-specific (KVM_EXIT_SYSTEM_EVENT — reset/poweroff/s4); arch backend decides
	ExitReasonIRQWindowOpen // ready for queued interrupt injection
	ExitReasonCPUID         // WHP only; KVM hits this only for unhandled leaves
	ExitReasonTriple        // triple-fault, fatal
	ExitReasonInternal      // backend-internal failure; FailureMsg set
	ExitReasonCancelled     // returned from Cancel()
	ExitReasonFailEntry     // KVM_EXIT_FAIL_ENTRY: invalid guest state on entry
)

// MMIOExit describes a memory-mapped I/O access the device tree must
// service. Address is the guest physical address; Data is the value
// being written (Write=true) or the buffer the device fills (Write=false).
// Length is the access size in bytes (1, 2, 4, or 8).
type MMIOExit struct {
	Address uint64
	Data    [8]byte
	Length  uint32
	Write   bool
}

// IOPortExit describes an x86 port I/O access (in/out instructions).
type IOPortExit struct {
	Port      uint16
	Direction IOPortDir
	Size      uint8 // 1, 2, or 4
	Count     uint32
	Data      [8]byte // up to 8 bytes per access
}

// IOPortDir identifies whether the guest is reading or writing the port.
type IOPortDir uint8

const (
	IOPortIn  IOPortDir = 0
	IOPortOut IOPortDir = 1
)

// CPUIDExit holds decoded CPUID input/output from a WHP exit.
// Populated before the WHP backend asks the run loop for emulation;
// the higher-level handler fills the four output registers and the
// backend resumes the vCPU with WHvSetVirtualProcessorRegisters.
type CPUIDExit struct {
	Function    uint32
	Subfunction uint32
	OutEAX      uint32
	OutEBX      uint32
	OutECX      uint32
	OutEDX      uint32
}

// ErrUnsupportedHV is returned by hypervisor_unsupported.go on platforms
// that have no backend yet (today: anything other than linux/amd64,
// linux/arm64, and — once Phase 2 lands — windows/amd64).
type ErrUnsupportedHV struct{ Reason string }

func (e ErrUnsupportedHV) Error() string { return "hypervisor backend unavailable: " + e.Reason }

// Compile-time assertion: HVVCPU should not be embeddable into types that
// also need io.Closer; HVVCPU.Close has the same signature, so callers
// can assign HVVCPU to io.Closer where convenient.
var _ io.Closer = (HVVCPU)(nil)
