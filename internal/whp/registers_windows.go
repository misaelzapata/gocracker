//go:build windows

package whp

import (
	"encoding/binary"
	"fmt"
	"unsafe"
)

// RegisterName mirrors WHV_REGISTER_NAME from the Windows 11 24H2 SDK
// (`WinHvPlatformDefs.h:32-323`). Constants are verbatim from the header
// rather than from online docs — we already learned the hard way that
// MSFT moves these values between Windows builds (see
// PropProcessorCount = 0x1fff lesson).
type RegisterName uint32

// x86_64 GPR register names. Each value is the corresponding
// WHvX64Register* enum entry.
const (
	RegRax    RegisterName = 0x00000000
	RegRcx    RegisterName = 0x00000001
	RegRdx    RegisterName = 0x00000002
	RegRbx    RegisterName = 0x00000003
	RegRsp    RegisterName = 0x00000004
	RegRbp    RegisterName = 0x00000005
	RegRsi    RegisterName = 0x00000006
	RegRdi    RegisterName = 0x00000007
	RegR8     RegisterName = 0x00000008
	RegR9     RegisterName = 0x00000009
	RegR10    RegisterName = 0x0000000A
	RegR11    RegisterName = 0x0000000B
	RegR12    RegisterName = 0x0000000C
	RegR13    RegisterName = 0x0000000D
	RegR14    RegisterName = 0x0000000E
	RegR15    RegisterName = 0x0000000F
	RegRip    RegisterName = 0x00000010
	RegRflags RegisterName = 0x00000011
)

// Segment register names.
const (
	RegEs   RegisterName = 0x00000012
	RegCs   RegisterName = 0x00000013
	RegSs   RegisterName = 0x00000014
	RegDs   RegisterName = 0x00000015
	RegFs   RegisterName = 0x00000016
	RegGs   RegisterName = 0x00000017
	RegLdtr RegisterName = 0x00000018
	RegTr   RegisterName = 0x00000019
)

// Table register names (GDT, IDT).
const (
	RegIdtr RegisterName = 0x0000001A
	RegGdtr RegisterName = 0x0000001B
)

// Control register names.
const (
	RegCr0 RegisterName = 0x0000001C
	RegCr2 RegisterName = 0x0000001D
	RegCr3 RegisterName = 0x0000001E
	RegCr4 RegisterName = 0x0000001F
	RegCr8 RegisterName = 0x00000020
)

// MSR-mapped register names.
const (
	RegTsc           RegisterName = 0x00002000
	RegEfer          RegisterName = 0x00002001
	RegKernelGsBase  RegisterName = 0x00002002
	RegApicBase      RegisterName = 0x00002003
)

// Event / interrupt-state register names.
const (
	RegPendingInterruption RegisterName = 0x80000000
	RegInterruptState      RegisterName = 0x80000001
)

// RegisterValue mirrors WHV_REGISTER_VALUE — a 16-byte union. Different
// register classes use different layouts (scalar at offset 0, segment
// {base, limit, selector, attributes}, table {pad, limit, base}). We
// keep the raw bytes and provide typed accessors so each register's
// encoding is in one place.
//
// The struct is exactly 16 bytes (WHV_REGISTER_VALUE is 16 bytes per the
// SDK header `C_ASSERT(sizeof(WHV_REGISTER_VALUE) == 16)`).
type RegisterValue [16]byte

// SetUint64 writes a 64-bit scalar register (most general-purpose,
// control, and MSR-mapped registers). Bytes 8..15 are zeroed.
func (v *RegisterValue) SetUint64(x uint64) {
	binary.LittleEndian.PutUint64(v[0:8], x)
	for i := 8; i < 16; i++ {
		v[i] = 0
	}
}

// Uint64 reads a 64-bit scalar register from the value union.
func (v *RegisterValue) Uint64() uint64 {
	return binary.LittleEndian.Uint64(v[0:8])
}

// SegmentValue is the WHV_X64_SEGMENT_REGISTER form of WHV_REGISTER_VALUE
// (used for CS/DS/ES/FS/GS/SS/TR/LDTR). The 16 bytes split as:
//
//	[0..7]   Base   (uint64)
//	[8..11]  Limit  (uint32)
//	[12..13] Selector (uint16)
//	[14..15] Attributes (uint16; see SegmentAttrs for bit layout)
//
// Source: WinHvPlatformDefs.h:924-947.
type SegmentValue struct {
	Base       uint64
	Limit      uint32
	Selector   uint16
	Attributes uint16
}

// SetSegment writes the 16-byte segment encoding into the value union.
func (v *RegisterValue) SetSegment(s SegmentValue) {
	binary.LittleEndian.PutUint64(v[0:8], s.Base)
	binary.LittleEndian.PutUint32(v[8:12], s.Limit)
	binary.LittleEndian.PutUint16(v[12:14], s.Selector)
	binary.LittleEndian.PutUint16(v[14:16], s.Attributes)
}

// Segment reads a segment register value.
func (v *RegisterValue) Segment() SegmentValue {
	return SegmentValue{
		Base:       binary.LittleEndian.Uint64(v[0:8]),
		Limit:      binary.LittleEndian.Uint32(v[8:12]),
		Selector:   binary.LittleEndian.Uint16(v[12:14]),
		Attributes: binary.LittleEndian.Uint16(v[14:16]),
	}
}

// TableValue is the WHV_X64_TABLE_REGISTER form (used for GDTR/IDTR):
//
//	[0..5]   Pad[3] (zeros)
//	[6..7]   Limit (uint16)
//	[8..15]  Base  (uint64)
//
// Source: WinHvPlatformDefs.h:951-957.
type TableValue struct {
	Base  uint64
	Limit uint16
}

// SetTable writes the 16-byte table encoding (GDT/IDT).
func (v *RegisterValue) SetTable(t TableValue) {
	for i := 0; i < 6; i++ {
		v[i] = 0
	}
	binary.LittleEndian.PutUint16(v[6:8], t.Limit)
	binary.LittleEndian.PutUint64(v[8:16], t.Base)
}

// Table reads a table register value.
func (v *RegisterValue) Table() TableValue {
	return TableValue{
		Limit: binary.LittleEndian.Uint16(v[6:8]),
		Base:  binary.LittleEndian.Uint64(v[8:16]),
	}
}

// Segment attribute bit layout (Win-native, little-endian 16-bit word).
// Mirrors the WHV_X64_SEGMENT_REGISTER bitfield in WinHvPlatformDefs.h:
//
//	[0..3]   Type        — segment type (code/data/TSS/gate variants)
//	[4]      S (NonSystem) — 1=user, 0=system
//	[5..6]   DPL         — privilege level 0–3
//	[7]      P (Present) — 1=present
//	[8..11]  Reserved
//	[12]     AVL         — available for software
//	[13]     L (Long)    — 1=64-bit code segment
//	[14]     DB (Default) — operand/address size
//	[15]     G (Granularity) — 1=4-KiB pages, 0=byte granule
type SegmentAttrs struct {
	Type    uint8 // 0–15
	S       uint8 // 0 or 1 (system vs user)
	DPL     uint8 // 0–3
	Present uint8 // 0 or 1
	AVL     uint8 // 0 or 1
	L       uint8 // 0 or 1 (long mode code segment)
	DB      uint8 // 0 or 1
	G       uint8 // 0 or 1
}

// Pack assembles the 16-bit Attributes word from individual bits.
func (a SegmentAttrs) Pack() uint16 {
	return uint16(a.Type&0xF) |
		uint16(a.S&0x1)<<4 |
		uint16(a.DPL&0x3)<<5 |
		uint16(a.Present&0x1)<<7 |
		uint16(a.AVL&0x1)<<12 |
		uint16(a.L&0x1)<<13 |
		uint16(a.DB&0x1)<<14 |
		uint16(a.G&0x1)<<15
}

// UnpackSegmentAttrs reverses Pack — extracts bits from a 16-bit word.
func UnpackSegmentAttrs(w uint16) SegmentAttrs {
	return SegmentAttrs{
		Type:    uint8(w & 0xF),
		S:       uint8((w >> 4) & 0x1),
		DPL:     uint8((w >> 5) & 0x3),
		Present: uint8((w >> 7) & 0x1),
		AVL:     uint8((w >> 12) & 0x1),
		L:       uint8((w >> 13) & 0x1),
		DB:      uint8((w >> 14) & 0x1),
		G:       uint8((w >> 15) & 0x1),
	}
}

// GetVCPURegisters reads up to len(names) registers from the vCPU. Values
// is filled in the same order — values[i] corresponds to names[i].
func GetVCPURegisters(h PartitionHandle, vcpu uint32, names []RegisterName, values []RegisterValue) error {
	if len(names) == 0 {
		return nil
	}
	if len(values) < len(names) {
		return fmt.Errorf("whp.GetVCPURegisters: values buffer too small (%d < %d)", len(values), len(names))
	}
	if err := loadDLL(); err != nil {
		return err
	}
	hr, _, _ := procGetVCPURegisters.Call(
		uintptr(h),
		uintptr(vcpu),
		uintptr(unsafe.Pointer(&names[0])),
		uintptr(len(names)),
		uintptr(unsafe.Pointer(&values[0])),
	)
	if HResult(hr) != sOK {
		return HResult(hr)
	}
	return nil
}

// SetVCPURegisters writes register values to the vCPU. The lengths of
// names and values must match; values[i] is written to names[i].
func SetVCPURegisters(h PartitionHandle, vcpu uint32, names []RegisterName, values []RegisterValue) error {
	if len(names) == 0 {
		return nil
	}
	if len(values) < len(names) {
		return fmt.Errorf("whp.SetVCPURegisters: values buffer too small (%d < %d)", len(values), len(names))
	}
	if err := loadDLL(); err != nil {
		return err
	}
	hr, _, _ := procSetVCPURegisters.Call(
		uintptr(h),
		uintptr(vcpu),
		uintptr(unsafe.Pointer(&names[0])),
		uintptr(len(names)),
		uintptr(unsafe.Pointer(&values[0])),
	)
	if HResult(hr) != sOK {
		return HResult(hr)
	}
	return nil
}
