//go:build windows

// Package whp wraps the Windows Hypervisor Platform (WHP) C API exposed
// by WinHvPlatform.dll. The bindings are loaded dynamically via
// syscall.NewLazyDLL so a build of gocracker can include this package
// even on a host where the Hypervisor Platform feature is disabled — the
// errors only surface when callers try to use the partition APIs.
//
// Reference: Microsoft's WinHvPlatform.h, plus node-vmm's
// native/whp/api.h (https://github.com/misaelzapata/node-vmm) which
// proved the same surface in C++.
//
// All exported function names mirror the underlying header symbols so
// cross-referencing docs is mechanical:
//
//	WHvGetCapability, WHvCreatePartition, WHvSetupPartition,
//	WHvDeletePartition, WHvSetPartitionProperty,
//	WHvMapGpaRange, WHvUnmapGpaRange, WHvQueryGpaRangeDirtyBitmap,
//	WHvCreateVirtualProcessor, WHvDeleteVirtualProcessor,
//	WHvRunVirtualProcessor, WHvCancelRunVirtualProcessor,
//	WHvGetVirtualProcessorRegisters, WHvSetVirtualProcessorRegisters,
//	WHvRequestInterrupt
//
// CGO is not used — Go's syscall package can drive Windows DLLs natively.
package whp

import (
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// PartitionHandle is the opaque WHV_PARTITION_HANDLE returned by
// WHvCreatePartition. It is a HANDLE (= uintptr) on Windows.
type PartitionHandle uintptr

// CapabilityCode mirrors WHV_CAPABILITY_CODE. Only the values gocracker
// queries appear here; the full enum is in WinHvPlatform.h.
type CapabilityCode uint32

const (
	CapHypervisorPresent CapabilityCode = 0x00000000
	CapFeatures          CapabilityCode = 0x00000001
	CapExtendedVMExits   CapabilityCode = 0x00000002
	CapProcessorVendor   CapabilityCode = 0x00001000
	CapProcessorFeatures CapabilityCode = 0x00001001
)

// PartitionPropertyCode mirrors WHV_PARTITION_PROPERTY_CODE. We expose
// only the properties gocracker actually sets at creation time.
type PartitionPropertyCode uint32

const (
	PropExtendedVMExits   PartitionPropertyCode = 0x00000001
	PropProcessorFeatures PartitionPropertyCode = 0x00001001
	PropCpuidExitList     PartitionPropertyCode = 0x00000002

	// PropLocalApicEmulationMode controls whether WHP emulates the
	// local APIC internally (so guest reads/writes at GPA 0xFEE00000
	// don't escape as MMIO exits) or leaves it to the VMM. Values:
	//   0 = None    — every APIC access is an MMIO exit
	//   1 = XApic   — WHP emulates a legacy MMIO-mapped xAPIC
	//   2 = X2Apic  — WHP emulates an MSR-based x2APIC
	// Phase 2f sets this to XApic so the kernel's early init runs to
	// completion without us writing a full APIC emulator.
	PropLocalApicEmulationMode PartitionPropertyCode = 0x00001005

	// PropProcessorCount: the value in WinHvPlatformDefs.h on Windows 11
	// 24H2 SDK (10.0.26100) is 0x00001fff, NOT 0x00001e00 as widely
	// repeated in older docs and pre-24H2 references. Hardcoding 0x1e00
	// returns WHV_E_UNKNOWN_PROPERTY (0x80370302) on this build. We pin
	// 0x1fff because it matches the SDK header and is what the kernel
	// actually accepts on every Win10/Win11 release tested. If a future
	// Windows revision moves this again, the lifecycle smoke test will
	// catch it.
	PropProcessorCount PartitionPropertyCode = 0x00001fff
)

// Local APIC emulation modes for PropLocalApicEmulationMode.
const (
	ApicEmuNone   uint32 = 0
	ApicEmuXApic  uint32 = 1
	ApicEmuX2Apic uint32 = 2
)

// MapGpaRangeFlags mirrors WHV_MAP_GPA_RANGE_FLAGS. Bitmask.
type MapGpaRangeFlags uint32

const (
	MapGpaNone    MapGpaRangeFlags = 0x00000000
	MapGpaRead    MapGpaRangeFlags = 0x00000001
	MapGpaWrite   MapGpaRangeFlags = 0x00000002
	MapGpaExecute MapGpaRangeFlags = 0x00000004
	MapGpaTrack   MapGpaRangeFlags = 0x00000008 // dirty-page tracking
)

// HResult is a native HRESULT wrapped as a Go error. WHP functions return
// HRESULTs; success is S_OK (0). Non-zero values correspond to
// platform-defined error codes (see WinError.h).
type HResult uint32

const (
	sOK   HResult = 0
	eFail HResult = 0x80004005 // generic E_FAIL — returned from callbacks on internal error
)

func (h HResult) Error() string {
	return fmt.Sprintf("WHP HRESULT 0x%08x", uint32(h))
}

// Available reports whether WinHvPlatform.dll is present on this host
// AND every function we need is exported. It does NOT check whether the
// Hypervisor Platform feature is enabled in the host config — that is
// what HypervisorPresent does, by calling WHvGetCapability.
func Available() bool { return loadDLL() == nil }

// HypervisorPresent queries the hypervisor for its presence flag
// (WHvCapabilityCodeHypervisorPresent). Returns (true, nil) when WHP is
// fully usable, (false, nil) when the DLL is loadable but the feature
// is disabled, and (false, err) on a hard failure (DLL missing,
// capability call rejected).
func HypervisorPresent() (bool, error) {
	if err := loadDLL(); err != nil {
		return false, err
	}
	var present uint32
	var written uint32
	hr, _, _ := procGetCapability.Call(
		uintptr(CapHypervisorPresent),
		uintptr(unsafe.Pointer(&present)),
		uintptr(unsafe.Sizeof(present)),
		uintptr(unsafe.Pointer(&written)),
	)
	if HResult(hr) != sOK {
		return false, HResult(hr)
	}
	return present != 0, nil
}

// CreatePartition allocates a new WHV partition. Caller must follow
// up with property sets (e.g. ProcessorCount via SetPartitionPropertyU32)
// and SetupPartition before mapping memory or creating vCPUs.
func CreatePartition() (PartitionHandle, error) {
	if err := loadDLL(); err != nil {
		return 0, err
	}
	var h PartitionHandle
	hr, _, _ := procCreatePartition.Call(uintptr(unsafe.Pointer(&h)))
	if HResult(hr) != sOK {
		return 0, HResult(hr)
	}
	return h, nil
}

// DeletePartition releases a previously-created partition. Idempotent
// on a zero handle. After Delete, the handle is invalid; do not call
// any further whp.* functions with it.
func DeletePartition(h PartitionHandle) error {
	if h == 0 {
		return nil
	}
	if err := loadDLL(); err != nil {
		return err
	}
	hr, _, _ := procDeletePartition.Call(uintptr(h))
	if HResult(hr) != sOK {
		return HResult(hr)
	}
	return nil
}

// SetupPartition commits the partition configuration. Must be called
// after every SetPartitionProperty call relevant to the boot configuration
// and BEFORE any MapGpaRange or CreateVirtualProcessor.
func SetupPartition(h PartitionHandle) error {
	if err := loadDLL(); err != nil {
		return err
	}
	hr, _, _ := procSetupPartition.Call(uintptr(h))
	if HResult(hr) != sOK {
		return HResult(hr)
	}
	return nil
}

// whvPartitionPropertySize is the in-memory size of the
// WHV_PARTITION_PROPERTY union from WinHvPlatform.h. It must be at least
// as large as the largest union member (WHV_X64_CPUID_RESULT2 in newer
// SDKs, which sits around 40 bytes); we round up to 64 to absorb future
// additions. The kernel ignores trailing bytes, but rejects buffers
// that are smaller than the field-size for the requested property —
// which is why a bare sizeof(uint32) for ProcessorCount errors with
// WHV_E_UNKNOWN_PROPERTY despite the value being correct.
//
// Reference: node-vmm passes sizeof(WHV_PARTITION_PROPERTY) for every
// SetPartitionProperty call ([native/whp/backend.cc] line ~3011).
const whvPartitionPropertySize = 64

// SetPartitionPropertyU32 is a convenience for properties whose value is
// a single 32-bit integer (e.g. ProcessorCount). The wire payload is an
// 8-byte-aligned WHV_PARTITION_PROPERTY-shaped buffer (the Win32 union is
// declared DECLSPEC_ALIGN(8); we mirror that with a uint64 backing array)
// with the U32 written at offset 0. Trailing bytes are zero (matches
// union semantics).
func SetPartitionPropertyU32(h PartitionHandle, code PartitionPropertyCode, value uint32) error {
	if err := loadDLL(); err != nil {
		return err
	}
	// uint64 array gives us guaranteed 8-byte alignment, which the C
	// declaration DECLSPEC_ALIGN(8) requires.
	var buf [whvPartitionPropertySize / 8]uint64
	*(*uint32)(unsafe.Pointer(&buf[0])) = value
	hr, _, _ := procSetPartitionProperty.Call(
		uintptr(h),
		uintptr(code),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(whvPartitionPropertySize),
	)
	if HResult(hr) != sOK {
		return HResult(hr)
	}
	return nil
}

// MapGpaRange maps a region of host memory at hostMem into the guest at
// gpa with the given access flags. The host memory must remain pinned
// (caller's responsibility — typically VirtualAlloc with PAGE_READWRITE)
// for the lifetime of the mapping.
func MapGpaRange(h PartitionHandle, hostMem []byte, gpa uint64, flags MapGpaRangeFlags) error {
	if err := loadDLL(); err != nil {
		return err
	}
	if len(hostMem) == 0 {
		return fmt.Errorf("whp.MapGpaRange: hostMem is empty")
	}
	hr, _, _ := procMapGpaRange.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(&hostMem[0])),
		uintptr(gpa),
		uintptr(uint64(len(hostMem))),
		uintptr(flags),
	)
	if HResult(hr) != sOK {
		return HResult(hr)
	}
	return nil
}

// UnmapGpaRange removes a previously-mapped range starting at gpa with
// the given size in bytes.
func UnmapGpaRange(h PartitionHandle, gpa uint64, size uint64) error {
	if err := loadDLL(); err != nil {
		return err
	}
	hr, _, _ := procUnmapGpaRange.Call(
		uintptr(h),
		uintptr(gpa),
		uintptr(size),
	)
	if HResult(hr) != sOK {
		return HResult(hr)
	}
	return nil
}

// CreateVirtualProcessor adds a vCPU at the given index. flags is
// reserved and must be 0 in the public API.
func CreateVirtualProcessor(h PartitionHandle, idx uint32) error {
	if err := loadDLL(); err != nil {
		return err
	}
	hr, _, _ := procCreateVCPU.Call(uintptr(h), uintptr(idx), 0)
	if HResult(hr) != sOK {
		return HResult(hr)
	}
	return nil
}

// DeleteVirtualProcessor releases the vCPU at idx.
func DeleteVirtualProcessor(h PartitionHandle, idx uint32) error {
	if err := loadDLL(); err != nil {
		return err
	}
	hr, _, _ := procDeleteVCPU.Call(uintptr(h), uintptr(idx))
	if HResult(hr) != sOK {
		return HResult(hr)
	}
	return nil
}

// CancelRunVirtualProcessor asks the hypervisor to interrupt an in-flight
// RunVirtualProcessor on the given vCPU. The Run call returns with an
// exit reason indicating cancellation.
func CancelRunVirtualProcessor(h PartitionHandle, idx uint32) error {
	if err := loadDLL(); err != nil {
		return err
	}
	hr, _, _ := procCancelRunVCPU.Call(uintptr(h), uintptr(idx), 0)
	if HResult(hr) != sOK {
		return HResult(hr)
	}
	return nil
}

// whvInterruptControl mirrors WHV_INTERRUPT_CONTROL (32 bytes).
//
//	bits  0–3 : Type (0=Fixed, 1=LowestPriority, 2=Nmi, 3=Init, 4=Sipi, 5=LocalInt1)
//	bits  4–7 : DestinationMode (0=Physical, 1=Logical)
//	bits  8–11: TriggerMode (0=Edge, 1=Level)
//	bits 12–63: reserved
//	[8..11]   : reserved
//	[12..15]  : Destination (vp index or APIC ID)
//	[16..19]  : Vector
//	[20..31]  : reserved
type whvInterruptControl struct {
	TypeDestModeTrigger uint64 // packed bitfield (Type/DestMode/Trigger)
	_                   uint32 // reserved
	Destination         uint32 // vp index (Physical) or APIC ID set (Logical)
	Vector              uint32 // interrupt vector 0..0xFF
	_                   uint32 // reserved
	_                   uint64 // reserved tail to keep 32-byte size
}

// Interrupt type constants for whvInterruptControl.Type field.
const (
	IntTypeFixed           uint64 = 0
	IntTypeLowestPriority  uint64 = 1
	IntTypeNmi             uint64 = 2
	IntTypeInit            uint64 = 3
	IntTypeSipi            uint64 = 4
	IntDestModePhysical    uint64 = 0
	IntDestModeLogical     uint64 = 1 << 4
	IntTriggerModeEdge     uint64 = 0
	IntTriggerModeLevel    uint64 = 1 << 8
)

// RequestFixedInterrupt fires a Fixed/Physical/Edge interrupt with the
// given vector at vCPU 0. Used by the PIC to deliver IRQs to the guest.
// Mirrors node-vmm's `RequestFixedInterrupt` helper.
func RequestFixedInterrupt(h PartitionHandle, vector uint32) error {
	if err := loadDLL(); err != nil {
		return err
	}
	ctl := whvInterruptControl{
		TypeDestModeTrigger: IntTypeFixed | IntDestModePhysical | IntTriggerModeEdge,
		Destination:         0,
		Vector:              vector,
	}
	hr, _, _ := procRequestInterrupt.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(&ctl)),
		uintptr(unsafe.Sizeof(ctl)),
	)
	if HResult(hr) != sOK {
		return HResult(hr)
	}
	return nil
}

// TranslateGva translates a guest-virtual address to a guest-physical
// address by walking the guest's page tables. flags is a
// WHV_TRANSLATE_GVA_FLAGS bitmask (e.g. PrivilegeExempt=1,
// SetPageTableBits=2, EnforceUserMode=4). Returns the GPA + the
// translation result code (WHV_TRANSLATE_GVA_RESULT_CODE: 0=Success).
//
// The WHP MMIO emulator invokes this through our TranslateGvaPage
// callback when the trapped instruction referenced a memory operand
// via a non-trivial segment / RIP-relative addressing mode.
func TranslateGva(h PartitionHandle, vcpu uint32, gva uint64, flags uint32) (gpa uint64, result uint32, err error) {
	if err := loadDLL(); err != nil {
		return 0, 0, err
	}
	// WHV_TRANSLATE_GVA_RESULT is a 64-bit struct: { uint32 ResultCode;
	// uint32 Reserved; }. We pass its address; only the low 32 bits
	// (ResultCode) are meaningful.
	var resultBuf [2]uint32
	hr, _, _ := procTranslateGva.Call(
		uintptr(h),
		uintptr(vcpu),
		uintptr(gva),
		uintptr(flags),
		uintptr(unsafe.Pointer(&resultBuf[0])),
		uintptr(unsafe.Pointer(&gpa)),
	)
	if HResult(hr) != sOK {
		return 0, 0, HResult(hr)
	}
	return gpa, resultBuf[0], nil
}

// loadDLL caches DLL load + symbol resolution. The symbol resolutions
// happen up-front so callers fail fast at process startup rather than
// mid-run with a confusing "proc not found" error.
func loadDLL() error {
	loadOnce.Do(func() {
		loadErr = dll.Load()
		if loadErr != nil {
			return
		}
		for _, p := range allProcs {
			if err := p.Find(); err != nil {
				loadErr = fmt.Errorf("WinHvPlatform.dll missing %s: %w", p.Name, err)
				return
			}
		}
	})
	return loadErr
}

var (
	dll = windows.NewLazyDLL("WinHvPlatform.dll")

	procGetCapability        = dll.NewProc("WHvGetCapability")
	procCreatePartition      = dll.NewProc("WHvCreatePartition")
	procSetupPartition       = dll.NewProc("WHvSetupPartition")
	procDeletePartition      = dll.NewProc("WHvDeletePartition")
	procSetPartitionProperty = dll.NewProc("WHvSetPartitionProperty")
	procMapGpaRange          = dll.NewProc("WHvMapGpaRange")
	procUnmapGpaRange        = dll.NewProc("WHvUnmapGpaRange")
	procQueryDirtyBitmap     = dll.NewProc("WHvQueryGpaRangeDirtyBitmap")
	procCreateVCPU           = dll.NewProc("WHvCreateVirtualProcessor")
	procDeleteVCPU           = dll.NewProc("WHvDeleteVirtualProcessor")
	procRunVCPU              = dll.NewProc("WHvRunVirtualProcessor")
	procCancelRunVCPU        = dll.NewProc("WHvCancelRunVirtualProcessor")
	procGetVCPURegisters     = dll.NewProc("WHvGetVirtualProcessorRegisters")
	procSetVCPURegisters     = dll.NewProc("WHvSetVirtualProcessorRegisters")
	procRequestInterrupt     = dll.NewProc("WHvRequestInterrupt")
	procTranslateGva         = dll.NewProc("WHvTranslateGva")

	allProcs = []*windows.LazyProc{
		procGetCapability,
		procCreatePartition,
		procSetupPartition,
		procDeletePartition,
		procSetPartitionProperty,
		procMapGpaRange,
		procUnmapGpaRange,
		procQueryDirtyBitmap,
		procCreateVCPU,
		procDeleteVCPU,
		procRunVCPU,
		procCancelRunVCPU,
		procGetVCPURegisters,
		procSetVCPURegisters,
		procRequestInterrupt,
		procTranslateGva,
	}

	loadOnce sync.Once
	loadErr  error
)
