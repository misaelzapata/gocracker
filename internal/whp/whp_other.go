//go:build !windows

// Package whp on non-Windows platforms exposes the same surface but
// every entry point reports the platform doesn't support WHP. Lets
// callers (pkg/vmm hypervisor selection) compile across the matrix
// without per-platform import guards.
package whp

import "errors"

type PartitionHandle uintptr
type CapabilityCode uint32
type PartitionPropertyCode uint32
type MapGpaRangeFlags uint32
type HResult uint32

const (
	CapHypervisorPresent CapabilityCode = 0
	CapFeatures          CapabilityCode = 1
	CapExtendedVMExits   CapabilityCode = 2
	CapProcessorVendor   CapabilityCode = 0x1000
	CapProcessorFeatures CapabilityCode = 0x1001

	PropExtendedVMExits        PartitionPropertyCode = 0x1
	PropProcessorFeatures      PartitionPropertyCode = 0x1001
	PropCpuidExitList          PartitionPropertyCode = 0x2
	PropLocalApicEmulationMode PartitionPropertyCode = 0x1005
	PropProcessorCount         PartitionPropertyCode = 0x1fff

	ApicEmuNone   uint32 = 0
	ApicEmuXApic  uint32 = 1
	ApicEmuX2Apic uint32 = 2

	MapGpaNone    MapGpaRangeFlags = 0
	MapGpaRead    MapGpaRangeFlags = 1
	MapGpaWrite   MapGpaRangeFlags = 2
	MapGpaExecute MapGpaRangeFlags = 4
	MapGpaTrack   MapGpaRangeFlags = 8
)

var errNotWindows = errors.New("whp: WinHvPlatform.dll is only available on Windows")

func (h HResult) Error() string { return "whp: not a Windows host" }

func Available() bool                                                                         { return false }
func HypervisorPresent() (bool, error)                                                        { return false, errNotWindows }
func CreatePartition() (PartitionHandle, error)                                               { return 0, errNotWindows }
func DeletePartition(PartitionHandle) error                                                   { return errNotWindows }
func SetupPartition(PartitionHandle) error                                                    { return errNotWindows }
func SetPartitionPropertyU32(PartitionHandle, PartitionPropertyCode, uint32) error            { return errNotWindows }
func MapGpaRange(PartitionHandle, []byte, uint64, MapGpaRangeFlags) error                     { return errNotWindows }
func UnmapGpaRange(PartitionHandle, uint64, uint64) error                                     { return errNotWindows }
func CreateVirtualProcessor(PartitionHandle, uint32) error                                    { return errNotWindows }
func DeleteVirtualProcessor(PartitionHandle, uint32) error                                    { return errNotWindows }
func CancelRunVirtualProcessor(PartitionHandle, uint32) error                                 { return errNotWindows }
func RequestFixedInterrupt(PartitionHandle, uint32) error                                     { return errNotWindows }

// Register helpers — stubs so the public surface matches Windows. None
// of these can do anything useful without WinHvPlatform.dll.

type RegisterName uint32
type RegisterValue [16]byte
type SegmentValue struct {
	Base       uint64
	Limit      uint32
	Selector   uint16
	Attributes uint16
}
type TableValue struct {
	Base  uint64
	Limit uint16
}
type SegmentAttrs struct {
	Type, S, DPL, Present, AVL, L, DB, G uint8
}

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
	RegEs     RegisterName = 0x00000012
	RegCs     RegisterName = 0x00000013
	RegSs     RegisterName = 0x00000014
	RegDs     RegisterName = 0x00000015
	RegFs     RegisterName = 0x00000016
	RegGs     RegisterName = 0x00000017
	RegLdtr   RegisterName = 0x00000018
	RegTr     RegisterName = 0x00000019
	RegIdtr   RegisterName = 0x0000001A
	RegGdtr   RegisterName = 0x0000001B
	RegCr0    RegisterName = 0x0000001C
	RegCr2    RegisterName = 0x0000001D
	RegCr3    RegisterName = 0x0000001E
	RegCr4    RegisterName = 0x0000001F
	RegCr8    RegisterName = 0x00000020
	RegTsc          RegisterName = 0x00002000
	RegEfer         RegisterName = 0x00002001
	RegKernelGsBase RegisterName = 0x00002002
	RegApicBase     RegisterName = 0x00002003

	RegPendingInterruption RegisterName = 0x80000000
	RegInterruptState      RegisterName = 0x80000001
)

func (v *RegisterValue) SetUint64(x uint64)            {}
func (v *RegisterValue) Uint64() uint64                { return 0 }
func (v *RegisterValue) SetSegment(s SegmentValue)     {}
func (v *RegisterValue) Segment() SegmentValue         { return SegmentValue{} }
func (v *RegisterValue) SetTable(t TableValue)         {}
func (v *RegisterValue) Table() TableValue             { return TableValue{} }
func (a SegmentAttrs) Pack() uint16                    { return 0 }
func UnpackSegmentAttrs(uint16) SegmentAttrs           { return SegmentAttrs{} }
func GetVCPURegisters(PartitionHandle, uint32, []RegisterName, []RegisterValue) error {
	return errNotWindows
}
func SetVCPURegisters(PartitionHandle, uint32, []RegisterName, []RegisterValue) error {
	return errNotWindows
}

// Run-loop stubs.

type ExitReason uint32

const (
	ExitReasonNone                     ExitReason = 0
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

type ExitContext struct {
	Reason            ExitReason
	InstructionLength uint8
	Cr8               uint8
	Cs                SegmentValue
	Rip               uint64
	Rflags            uint64
}

type MMIO struct {
	InstructionByteCount uint8
	InstructionBytes     [16]byte
	AccessInfo           uint32
	Gpa                  uint64
	Gva                  uint64
}

func (m MMIO) AccessType() uint8 { return 0 }
func (m MMIO) IsWrite() bool     { return false }

type IOPort struct {
	InstructionByteCount uint8
	InstructionBytes     [16]byte
	AccessInfo           uint32
	Port                 uint16
	Rax, Rcx, Rsi, Rdi   uint64
	Ds, Es               SegmentValue
}

func (io IOPort) IsWrite() bool    { return false }
func (io IOPort) AccessSize() uint8 { return 1 }

type CPUID struct {
	Rax, Rcx, Rdx, Rbx                                                   uint64
	DefaultResultRax, DefaultResultRcx, DefaultResultRdx, DefaultResultRbx uint64
}

type MSR struct {
	AccessInfo uint32
	MsrNumber  uint32
	Rax, Rdx   uint64
}

func (m MSR) IsWrite() bool { return false }

func (e *ExitContext) MMIO() MMIO         { return MMIO{} }
func (e *ExitContext) IOPort() IOPort     { return IOPort{} }
func (e *ExitContext) CPUID() CPUID       { return CPUID{} }
func (e *ExitContext) MSR() MSR           { return MSR{} }
func (e *ExitContext) CancelReason() uint32 { return 0 }

func RunVirtualProcessor(PartitionHandle, uint32) (ExitContext, error) {
	return ExitContext{}, errNotWindows
}
