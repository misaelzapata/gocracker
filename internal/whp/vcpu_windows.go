//go:build windows

package whp

import (
	"encoding/binary"
	"unsafe"
)

// WHV_RUN_VP_EXIT_CONTEXT is 224 bytes:
//
//	[0..3]   ExitReason (uint32, WHV_RUN_VP_EXIT_REASON)
//	[4..7]   Reserved (uint32)
//	[8..63]  VpContext (56 bytes; ExecutionState@8, InstructionLength@10[3:0]+Cr8@10[7:4],
//	                    Cs@16-31, Rip@32, Rflags@40)
//	[64..]   Union of per-exit payloads (160 bytes worth of overlay)
//
// Offsets sourced from `WinHvPlatformDefs.h` (Win11 24H2 SDK).
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
func (e *ExitContext) MMIO() MMIO {
	return MMIO{
		InstructionByteCount: e.raw[64],
		InstructionBytes:     *(*[16]byte)(unsafe.Pointer(&e.raw[68])),
		AccessInfo:           binary.LittleEndian.Uint32(e.raw[84:88]),
		Gpa:                  binary.LittleEndian.Uint64(e.raw[88:96]),
		Gva:                  binary.LittleEndian.Uint64(e.raw[96:104]),
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
// Encoded as bits [3:1] of AccessInfo where 0→1, 1→2, 2→4.
func (io IOPort) AccessSize() uint8 {
	switch (io.AccessInfo >> 1) & 0x7 {
	case 0:
		return 1
	case 1:
		return 2
	case 2:
		return 4
	default:
		return 0
	}
}

// IOPort returns the decoded port-I/O exit fields.
func (e *ExitContext) IOPort() IOPort {
	return IOPort{
		InstructionByteCount: e.raw[64],
		InstructionBytes:     *(*[16]byte)(unsafe.Pointer(&e.raw[68])),
		AccessInfo:           binary.LittleEndian.Uint32(e.raw[84:88]),
		Port:                 binary.LittleEndian.Uint16(e.raw[88:90]),
		Rax:                  binary.LittleEndian.Uint64(e.raw[96:104]),
		Rcx:                  binary.LittleEndian.Uint64(e.raw[104:112]),
		Rsi:                  binary.LittleEndian.Uint64(e.raw[112:120]),
		Rdi:                  binary.LittleEndian.Uint64(e.raw[120:128]),
		Ds: SegmentValue{
			Base:       binary.LittleEndian.Uint64(e.raw[128:136]),
			Limit:      binary.LittleEndian.Uint32(e.raw[136:140]),
			Selector:   binary.LittleEndian.Uint16(e.raw[140:142]),
			Attributes: binary.LittleEndian.Uint16(e.raw[142:144]),
		},
		Es: SegmentValue{
			Base:       binary.LittleEndian.Uint64(e.raw[144:152]),
			Limit:      binary.LittleEndian.Uint32(e.raw[152:156]),
			Selector:   binary.LittleEndian.Uint16(e.raw[156:158]),
			Attributes: binary.LittleEndian.Uint16(e.raw[158:160]),
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
func (e *ExitContext) CPUID() CPUID {
	return CPUID{
		Rax:              binary.LittleEndian.Uint64(e.raw[64:72]),
		Rcx:              binary.LittleEndian.Uint64(e.raw[72:80]),
		Rdx:              binary.LittleEndian.Uint64(e.raw[80:88]),
		Rbx:              binary.LittleEndian.Uint64(e.raw[88:96]),
		DefaultResultRax: binary.LittleEndian.Uint64(e.raw[96:104]),
		DefaultResultRcx: binary.LittleEndian.Uint64(e.raw[104:112]),
		DefaultResultRdx: binary.LittleEndian.Uint64(e.raw[112:120]),
		DefaultResultRbx: binary.LittleEndian.Uint64(e.raw[120:128]),
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
func (e *ExitContext) MSR() MSR {
	return MSR{
		AccessInfo: binary.LittleEndian.Uint32(e.raw[64:68]),
		MsrNumber:  binary.LittleEndian.Uint32(e.raw[68:72]),
		Rax:        binary.LittleEndian.Uint64(e.raw[72:80]),
		Rdx:        binary.LittleEndian.Uint64(e.raw[80:88]),
	}
}

// CancelReason mirrors WHV_RUN_VP_CANCEL_REASON for ExitReasonCanceled.
func (e *ExitContext) CancelReason() uint32 {
	return binary.LittleEndian.Uint32(e.raw[64:68])
}

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
