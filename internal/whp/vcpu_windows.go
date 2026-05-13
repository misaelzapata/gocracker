//go:build windows

package whp

import (
	"encoding/binary"
	"unsafe"
)

// WHV_RUN_VP_EXIT_CONTEXT layout from WinHvPlatformDefs.h (24H2 SDK):
//
//	[0..3]   ExitReason (uint32, WHV_RUN_VP_EXIT_REASON)
//	[4..7]   Reserved (uint32)
//	[8..47]  VpContext (WHV_X64_VP_EXIT_CONTEXT, 40 bytes):
//	            [8..9]   ExecutionState (uint16, WHV_X64_VP_EXECUTION_STATE)
//	            [10]     InstructionLength[0:3] + Cr8[4:7] (uint8 bitfield)
//	            [11]     Reserved (uint8)
//	            [12..15] Reserved2 (uint32)
//	            [16..31] Cs (16 bytes WHV_X64_SEGMENT_REGISTER)
//	            [32..39] Rip (uint64)
//	            [40..47] Rflags (uint64)
//	[48..]   Union of per-exit payloads (176 bytes worth of overlay)
//
// IMPORTANT: an earlier prototype had the union starting at offset 64
// (assumption that VpContext was 56 bytes). The actual VpContext size is
// 40 bytes (verified with `C_ASSERT(sizeof(WHV_X64_VP_EXIT_CONTEXT) ==
// 40)` in WinHvPlatformDefs.h line 2898). Verified empirically by
// dumping the raw bytes after a real `out dx, al` exit: the port number
// lands at offset 72 in the parent struct, which only makes sense if
// the IoPortAccess body starts at offset 48.
const exitContextSize = 224

// ExitReason mirrors WHV_RUN_VP_EXIT_REASON (WinHvPlatformDefs.h:2805-2832).
// Hex values are verbatim from the header.
type ExitReason uint32

const (
	ExitReasonNone                     ExitReason = 0x00000000
	ExitReasonMemoryAccess             ExitReason = 0x00000001
	ExitReasonX64IoPortAccess          ExitReason = 0x00000002
	ExitReasonUnrecoverableException   ExitReason = 0x00000004
	ExitReasonInvalidVpRegisterValue   ExitReason = 0x00000005
	ExitReasonUnsupportedFeature       ExitReason = 0x00000006
	ExitReasonX64InterruptWindow       ExitReason = 0x00000007
	ExitReasonX64Halt                  ExitReason = 0x00000008
	ExitReasonX64ApicEoi               ExitReason = 0x00000009
	ExitReasonSynicSintDeliverable     ExitReason = 0x0000000A
	ExitReasonX64MsrAccess             ExitReason = 0x00001000
	ExitReasonX64Cpuid                 ExitReason = 0x00001001
	ExitReasonException                ExitReason = 0x00001002
	ExitReasonX64Rdtsc                 ExitReason = 0x00001003
	ExitReasonX64ApicSmiTrap           ExitReason = 0x00001004
	ExitReasonHypercall                ExitReason = 0x00001005
	ExitReasonX64ApicInitSipiTrap      ExitReason = 0x00001006
	ExitReasonX64ApicWriteTrap         ExitReason = 0x00001007
	ExitReasonCanceled                 ExitReason = 0x00002001
)

// ExitContext is the Go-side decoded form of WHV_RUN_VP_EXIT_CONTEXT.
// We keep the raw bytes plus the decoded reason; per-reason accessors
// extract MMIO/IOPort/CPUID/MSR fields on demand.
type ExitContext struct {
	Reason ExitReason

	// VpContext fields (always populated, regardless of exit reason).
	InstructionLength uint8 // VpContext.InstructionLength (low 4 bits at byte 10)
	Cr8               uint8 // VpContext.Cr8 (high 4 bits at byte 10)
	Cs                SegmentValue
	Rip               uint64
	Rflags            uint64

	// raw is the full 224-byte exit context, kept so we can decode
	// per-reason fields without re-running the vCPU.
	raw [exitContextSize]byte
}

// MMIO extracts the WHV_MEMORY_ACCESS_CONTEXT fields. Only valid when
// Reason == ExitReasonMemoryAccess.
type MMIO struct {
	// InstructionByteCount is how many bytes of the trapped instruction
	// the hypervisor captured (up to 16); InstructionBytes carries them.
	InstructionByteCount uint8
	InstructionBytes     [16]byte

	// AccessInfo packs AccessType[1:0] (0=read, 1=write, 2=execute),
	// GpaUnmapped[2], GvaValid[3].
	AccessInfo uint32

	Gpa uint64 // guest physical address
	Gva uint64 // guest virtual address (valid iff AccessInfo bit 3 set)
}

// AccessType returns 0=Read, 1=Write, 2=Execute (low 2 bits of AccessInfo).
func (m MMIO) AccessType() uint8 { return uint8(m.AccessInfo & 0x3) }

// IsWrite is true when the guest is writing to the MMIO region.
func (m MMIO) IsWrite() bool { return m.AccessType() == 1 }

// MMIO returns the decoded memory-access exit fields. Only call when
// Reason == ExitReasonMemoryAccess; otherwise the result is undefined.
// WHV_MEMORY_ACCESS_CONTEXT layout (40 bytes, starts at parent offset 48):
//
//	[48]      InstructionByteCount (uint8)
//	[49..51]  Reserved[3]
//	[52..67]  InstructionBytes[16]
//	[68..71]  AccessInfo (uint32 WHV_MEMORY_ACCESS_INFO)
//	[72..79]  Gpa (uint64)
//	[80..87]  Gva (uint64)
func (e *ExitContext) MMIO() MMIO {
	return MMIO{
		InstructionByteCount: e.raw[48],
		InstructionBytes:     *(*[16]byte)(unsafe.Pointer(&e.raw[52])),
		AccessInfo:           binary.LittleEndian.Uint32(e.raw[68:72]),
		Gpa:                  binary.LittleEndian.Uint64(e.raw[72:80]),
		Gva:                  binary.LittleEndian.Uint64(e.raw[80:88]),
	}
}

// IOPort extracts the WHV_X64_IO_PORT_ACCESS_CONTEXT fields. Only valid
// when Reason == ExitReasonX64IoPortAccess.
type IOPort struct {
	InstructionByteCount uint8
	InstructionBytes     [16]byte

	// AccessInfo packs IsWrite[0], AccessSize[1:3], StringOp[4], RepPrefix[5].
	AccessInfo uint32
	Port       uint16

	Rax uint64
	Rcx uint64
	Rsi uint64
	Rdi uint64

	Ds SegmentValue
	Es SegmentValue
}

// IsWrite is true for OUT instructions; false for IN.
func (io IOPort) IsWrite() bool { return (io.AccessInfo & 0x1) != 0 }

// AccessSize returns the size of one I/O transfer in bytes (1, 2, or 4).
// In WHV_X64_IO_PORT_ACCESS_INFO, AccessSize occupies bits [1..3] and
// holds the byte count directly (1, 2, or 4) — not an encoded value as
// some older docs suggest. Verified empirically: `out dx, al` (1-byte
// write) reports AccessSize bits = 0b001 = 1.
func (io IOPort) AccessSize() uint8 {
	return uint8((io.AccessInfo >> 1) & 0x7)
}

// IOPort returns the decoded port-I/O exit fields.
// WHV_X64_IO_PORT_ACCESS_CONTEXT layout (96 bytes, starts at parent offset 48):
//
//	[48]       InstructionByteCount
//	[49..51]   Reserved[3]
//	[52..67]   InstructionBytes[16]
//	[68..71]   AccessInfo (WHV_X64_IO_PORT_ACCESS_INFO uint32)
//	[72..73]   PortNumber (uint16)
//	[74..79]   Reserved2[3]
//	[80..87]   Rax
//	[88..95]   Rcx
//	[96..103]  Rsi
//	[104..111] Rdi
//	[112..127] Ds (16 bytes WHV_X64_SEGMENT_REGISTER)
//	[128..143] Es (16 bytes)
func (e *ExitContext) IOPort() IOPort {
	return IOPort{
		InstructionByteCount: e.raw[48],
		InstructionBytes:     *(*[16]byte)(unsafe.Pointer(&e.raw[52])),
		AccessInfo:           binary.LittleEndian.Uint32(e.raw[68:72]),
		Port:                 binary.LittleEndian.Uint16(e.raw[72:74]),
		Rax:                  binary.LittleEndian.Uint64(e.raw[80:88]),
		Rcx:                  binary.LittleEndian.Uint64(e.raw[88:96]),
		Rsi:                  binary.LittleEndian.Uint64(e.raw[96:104]),
		Rdi:                  binary.LittleEndian.Uint64(e.raw[104:112]),
		Ds: SegmentValue{
			Base:       binary.LittleEndian.Uint64(e.raw[112:120]),
			Limit:      binary.LittleEndian.Uint32(e.raw[120:124]),
			Selector:   binary.LittleEndian.Uint16(e.raw[124:126]),
			Attributes: binary.LittleEndian.Uint16(e.raw[126:128]),
		},
		Es: SegmentValue{
			Base:       binary.LittleEndian.Uint64(e.raw[128:136]),
			Limit:      binary.LittleEndian.Uint32(e.raw[136:140]),
			Selector:   binary.LittleEndian.Uint16(e.raw[140:142]),
			Attributes: binary.LittleEndian.Uint16(e.raw[142:144]),
		},
	}
}

// CPUID extracts WHV_X64_CPUID_ACCESS_CONTEXT. Input regs are Rax/Rcx;
// the hypervisor provides default output regs (DefaultResultR{ax,cx,dx,bx})
// which the run loop may overwrite by SetVCPURegisters before resuming.
type CPUID struct {
	Rax              uint64 // input EAX (function)
	Rcx              uint64 // input ECX (subfunction)
	Rdx              uint64 // input EDX
	Rbx              uint64 // input EBX
	DefaultResultRax uint64
	DefaultResultRcx uint64
	DefaultResultRdx uint64
	DefaultResultRbx uint64
}

// CPUID returns the decoded CPUID exit fields.
// WHV_X64_CPUID_ACCESS_CONTEXT layout (64 bytes, starts at parent offset 48):
//
//	[48..55]   Rax (input EAX, function)
//	[56..63]   Rcx (input ECX, subfunction)
//	[64..71]   Rdx
//	[72..79]   Rbx
//	[80..87]   DefaultResultRax
//	[88..95]   DefaultResultRcx
//	[96..103]  DefaultResultRdx
//	[104..111] DefaultResultRbx
func (e *ExitContext) CPUID() CPUID {
	return CPUID{
		Rax:              binary.LittleEndian.Uint64(e.raw[48:56]),
		Rcx:              binary.LittleEndian.Uint64(e.raw[56:64]),
		Rdx:              binary.LittleEndian.Uint64(e.raw[64:72]),
		Rbx:              binary.LittleEndian.Uint64(e.raw[72:80]),
		DefaultResultRax: binary.LittleEndian.Uint64(e.raw[80:88]),
		DefaultResultRcx: binary.LittleEndian.Uint64(e.raw[88:96]),
		DefaultResultRdx: binary.LittleEndian.Uint64(e.raw[96:104]),
		DefaultResultRbx: binary.LittleEndian.Uint64(e.raw[104:112]),
	}
}

// MSR extracts WHV_X64_MSR_ACCESS_CONTEXT.
type MSR struct {
	AccessInfo uint32 // bit 0 = IsWrite
	MsrNumber  uint32
	Rax        uint64
	Rdx        uint64
}

// IsWrite returns true for WRMSR, false for RDMSR.
func (m MSR) IsWrite() bool { return (m.AccessInfo & 0x1) != 0 }

// MSR returns the decoded MSR exit fields.
// WHV_X64_MSR_ACCESS_CONTEXT layout (24 bytes, starts at parent offset 48):
//
//	[48..51] AccessInfo (uint32, bit 0 = IsWrite)
//	[52..55] MsrNumber (uint32)
//	[56..63] Rax (uint64)
//	[64..71] Rdx (uint64)
func (e *ExitContext) MSR() MSR {
	return MSR{
		AccessInfo: binary.LittleEndian.Uint32(e.raw[48:52]),
		MsrNumber:  binary.LittleEndian.Uint32(e.raw[52:56]),
		Rax:        binary.LittleEndian.Uint64(e.raw[56:64]),
		Rdx:        binary.LittleEndian.Uint64(e.raw[64:72]),
	}
}

// CancelReason mirrors WHV_RUN_VP_CANCEL_REASON (4 bytes at parent offset 48).
func (e *ExitContext) CancelReason() uint32 {
	return binary.LittleEndian.Uint32(e.raw[48:52])
}

// VpContextPtr returns a raw pointer to the embedded WHV_VP_EXIT_CONTEXT
// (40 bytes at parent offset 8). Required when handing the exit
// context to WHvEmulatorTryMmioEmulation / WHvEmulatorTryIoEmulation.
// The pointer is valid for the lifetime of the *ExitContext value.
func (e *ExitContext) VpContextPtr() unsafe.Pointer { return unsafe.Pointer(&e.raw[8]) }

// MemoryAccessPtr returns a raw pointer to the WHV_MEMORY_ACCESS_CONTEXT
// (40 bytes at parent offset 48). Only meaningful when Reason ==
// ExitReasonMemoryAccess.
func (e *ExitContext) MemoryAccessPtr() unsafe.Pointer { return unsafe.Pointer(&e.raw[48]) }

// IoPortAccessPtr returns a raw pointer to the WHV_X64_IO_PORT_ACCESS_CONTEXT
// (96 bytes at parent offset 48). Only meaningful when Reason ==
// ExitReasonX64IoPortAccess.
func (e *ExitContext) IoPortAccessPtr() unsafe.Pointer { return unsafe.Pointer(&e.raw[48]) }

// RunVirtualProcessor executes the vCPU until it exits. The exit shape
// is decoded into a Go struct; callers switch on ExitContext.Reason and
// pull per-exit data via the typed accessors (MMIO, IOPort, CPUID, MSR).
//
// IMPORTANT: WHP does NOT auto-advance RIP after emulated exits. After
// handling MMIO / IOPort / CPUID / MSR, the caller MUST add
// InstructionLength to RIP and write back via SetVCPURegisters before
// the next Run; otherwise the same instruction re-traps.
func RunVirtualProcessor(h PartitionHandle, vcpu uint32) (ExitContext, error) {
	if err := loadDLL(); err != nil {
		return ExitContext{}, err
	}
	var ctx ExitContext
	hr, _, _ := procRunVCPU.Call(
		uintptr(h),
		uintptr(vcpu),
		uintptr(unsafe.Pointer(&ctx.raw[0])),
		uintptr(exitContextSize),
	)
	if HResult(hr) != sOK {
		return ExitContext{}, HResult(hr)
	}
	// Decode the common header fields. Per-exit payloads stay in raw
	// and are extracted on demand by the typed accessors.
	ctx.Reason = ExitReason(binary.LittleEndian.Uint32(ctx.raw[0:4]))
	ctx.InstructionLength = ctx.raw[10] & 0x0F
	ctx.Cr8 = (ctx.raw[10] >> 4) & 0x0F
	ctx.Cs = SegmentValue{
		Base:       binary.LittleEndian.Uint64(ctx.raw[16:24]),
		Limit:      binary.LittleEndian.Uint32(ctx.raw[24:28]),
		Selector:   binary.LittleEndian.Uint16(ctx.raw[28:30]),
		Attributes: binary.LittleEndian.Uint16(ctx.raw[30:32]),
	}
	ctx.Rip = binary.LittleEndian.Uint64(ctx.raw[32:40])
	ctx.Rflags = binary.LittleEndian.Uint64(ctx.raw[40:48])
	return ctx, nil
}
