//go:build windows

package whp

import (
	"fmt"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Bindings for WinHvEmulation.dll — the Windows-supplied x86 instruction
// emulator that decodes MMIO / IO-port trap instructions and dispatches
// reads/writes through Go callbacks.
//
// Why we need this: WHP raises a memory-access exit with the trapped
// instruction bytes attached, but the application is expected to decode
// the instruction itself (which register, what size, read or write,
// repeat prefix, etc). Re-implementing an x86 instruction decoder in Go
// would be hundreds of lines and a maintenance burden. The emulator API
// takes care of all that — we just supply five callbacks for the side
// effects (memory read/write, register get/set, address translation,
// port I/O).
//
// Reference: WinHvEmulation.h (10.0.26100 SDK header). node-vmm's
// native/whp/backend.cc routes every WHvRunVpExitReasonMemoryAccess
// exit through WHvEmulatorTryMmioEmulation; we do the same here.
//
// The five callbacks below are registered once globally (Go's
// syscall.NewCallback is expensive — each call burns one of a fixed
// pool of trampolines). Per-vCPU state hangs off the Context pointer
// the emulator forwards back to each callback, which we use to look up
// the active EmulatorVCPU in a registry.

// EmulatorHandle is the opaque WHP emulator handle (a void* pointer).
type EmulatorHandle uintptr

// EmulatorStatus mirrors WHV_EMULATOR_STATUS — a 32-bit bitfield.
type EmulatorStatus uint32

// EmulationSuccessful returns true when the emulator decoded and
// executed the trapped instruction without any callback failure.
func (s EmulatorStatus) EmulationSuccessful() bool { return s&0x1 != 0 }

// Failed returns a short human-readable description of the failure
// bits, suitable for error messages.
func (s EmulatorStatus) Failed() string {
	if s.EmulationSuccessful() {
		return ""
	}
	var bits []string
	if s&(1<<1) != 0 {
		bits = append(bits, "InternalEmulationFailure")
	}
	if s&(1<<2) != 0 {
		bits = append(bits, "IoPortCallbackFailed")
	}
	if s&(1<<3) != 0 {
		bits = append(bits, "MemoryCallbackFailed")
	}
	if s&(1<<4) != 0 {
		bits = append(bits, "TranslateGvaPageCallbackFailed")
	}
	if s&(1<<5) != 0 {
		bits = append(bits, "TranslateGvaPageGpaNotAligned")
	}
	if s&(1<<6) != 0 {
		bits = append(bits, "GetVirtualProcessorRegistersFailed")
	}
	if s&(1<<7) != 0 {
		bits = append(bits, "SetVirtualProcessorRegistersFailed")
	}
	if s&(1<<8) != 0 {
		bits = append(bits, "InterruptCausedIntercept")
	}
	if s&(1<<9) != 0 {
		bits = append(bits, "GuestCannotBeFaulted")
	}
	if len(bits) == 0 {
		return fmt.Sprintf("unknown status %#x", uint32(s))
	}
	out := bits[0]
	for _, b := range bits[1:] {
		out += "+" + b
	}
	return out
}

// emulatorMemoryAccessInfo mirrors WHV_EMULATOR_MEMORY_ACCESS_INFO
// from WinHvEmulation.h. 24 bytes total (8-byte aligned, the trailing
// padding rounds the 18-byte payload up).
type emulatorMemoryAccessInfo struct {
	GpaAddress uint64
	Direction  uint8 // 0=read, 1=write
	AccessSize uint8
	Data       [8]uint8
	_pad       [6]uint8
}

// emulatorIoAccessInfo mirrors WHV_EMULATOR_IO_ACCESS_INFO.
type emulatorIoAccessInfo struct {
	Direction  uint8
	_pad1      uint8
	Port       uint16
	AccessSize uint16
	_pad2      uint16
	Data       uint32
}

// emulatorCallbacks mirrors WHV_EMULATOR_CALLBACKS:
//
//	UINT32 Size; UINT32 Reserved; + 5 function pointers (8 bytes each)
//	= 8 + 40 = 48 bytes total.
type emulatorCallbacks struct {
	Size                                     uint32
	Reserved                                 uint32
	WHvEmulatorIoPortCallback                uintptr
	WHvEmulatorMemoryCallback                uintptr
	WHvEmulatorGetVirtualProcessorRegisters  uintptr
	WHvEmulatorSetVirtualProcessorRegisters  uintptr
	WHvEmulatorTranslateGvaPage              uintptr
}

// EmulatorVCPU is the per-vCPU context the emulator routes its
// callbacks against. The caller constructs one per vCPU and passes it
// to TryMmioEmulation / TryIoEmulation; the emulator forwards the
// pointer back to our callbacks as the opaque Context parameter.
//
// The five callbacks delegate to the VCPU's wired hooks: MMIORead /
// MMIOWrite for device dispatch, the partition handle for register
// Get/Set, and an optional IOPort handler.
type EmulatorVCPU struct {
	Partition PartitionHandle
	VCPUIndex uint32

	// Mem is the host-side view of guest RAM. RAM reads/writes by the
	// emulator (e.g. instruction operand fetch via translated GVA) hit
	// this slice. MMIO accesses to addresses outside Mem are routed
	// through MMIORead / MMIOWrite.
	Mem []byte

	// MMIORead is invoked when the emulator wants to read N bytes
	// from a guest address that isn't backed by Mem. Should return
	// the value the guest will observe (in little-endian byte order).
	// The returned slice's length must equal `length`.
	MMIORead func(addr uint64, length uint8) []byte

	// MMIOWrite is invoked for guest writes into device MMIO. data
	// is little-endian-encoded; its length equals AccessSize.
	MMIOWrite func(addr uint64, length uint8, data []byte)

	// IOPortIn / IOPortOut are optional — only called for the
	// TryIoEmulation entry point. Most virtio-mmio setups never need
	// them; the WHP run loop handles port I/O directly via its own
	// IOPort exit path.
	IOPortIn  func(port uint16, length uint16) uint32
	IOPortOut func(port uint16, length uint16, value uint32)
}

// vcpuRegistry tracks active EmulatorVCPUs by an opaque token (a Go
// uintptr) we hand to the emulator as Context. The token is just a
// pointer to a registry entry — we can't pass the *EmulatorVCPU
// directly because Go's runtime can move the struct, but registry
// entries are pinned by sync.Map semantics.
var (
	vcpuRegistry  sync.Map // map[uintptr]*EmulatorVCPU
	nextVCPUToken uintptr
	tokenMu       sync.Mutex
)

func registerVCPU(v *EmulatorVCPU) uintptr {
	tokenMu.Lock()
	nextVCPUToken++
	token := nextVCPUToken
	tokenMu.Unlock()
	vcpuRegistry.Store(token, v)
	return token
}

func unregisterVCPU(token uintptr) {
	vcpuRegistry.Delete(token)
}

func lookupVCPU(token uintptr) *EmulatorVCPU {
	v, ok := vcpuRegistry.Load(token)
	if !ok {
		return nil
	}
	return v.(*EmulatorVCPU)
}

// Emulator wraps a WHV_EMULATOR_HANDLE plus the registered callback
// trampolines so they don't get garbage-collected.
type Emulator struct {
	handle EmulatorHandle
	cbs    *emulatorCallbacks // kept alive
}

var (
	emulatorDLL  *windows.LazyDLL
	procEmCreate *windows.LazyProc
	procEmDelete *windows.LazyProc
	procEmMmio   *windows.LazyProc
	procEmIo     *windows.LazyProc

	emulatorLoadOnce sync.Once
	emulatorLoadErr  error

	// Global callback trampolines, registered on first use.
	cbIoPort       uintptr
	cbMemory       uintptr
	cbGetRegisters uintptr
	cbSetRegisters uintptr
	cbTranslateGva uintptr
)

func loadEmulatorDLL() error {
	emulatorLoadOnce.Do(func() {
		emulatorDLL = windows.NewLazyDLL("WinHvEmulation.dll")
		if err := emulatorDLL.Load(); err != nil {
			emulatorLoadErr = fmt.Errorf("LoadDLL WinHvEmulation.dll: %w", err)
			return
		}
		procEmCreate = emulatorDLL.NewProc("WHvEmulatorCreateEmulator")
		procEmDelete = emulatorDLL.NewProc("WHvEmulatorDestroyEmulator")
		procEmMmio = emulatorDLL.NewProc("WHvEmulatorTryMmioEmulation")
		procEmIo = emulatorDLL.NewProc("WHvEmulatorTryIoEmulation")
		for _, p := range []*windows.LazyProc{procEmCreate, procEmDelete, procEmMmio, procEmIo} {
			if err := p.Find(); err != nil {
				emulatorLoadErr = fmt.Errorf("resolve %s: %w", p.Name, err)
				return
			}
		}
		cbIoPort = syscall.NewCallback(ioPortCallback)
		cbMemory = syscall.NewCallback(memoryCallback)
		cbGetRegisters = syscall.NewCallback(getRegistersCallback)
		cbSetRegisters = syscall.NewCallback(setRegistersCallback)
		cbTranslateGva = syscall.NewCallback(translateGvaCallback)
	})
	return emulatorLoadErr
}

// CreateEmulator returns a fresh WHP emulator instance wired to our
// five package-level callbacks. The returned *Emulator can be shared
// across vCPUs (each TryMmio/TryIo call carries its own *EmulatorVCPU
// context), but in practice we create one per vCPU so destroying it
// doesn't trash an in-flight emulation on another thread.
func CreateEmulator() (*Emulator, error) {
	if err := loadEmulatorDLL(); err != nil {
		return nil, err
	}
	cbs := &emulatorCallbacks{
		Size:                                    uint32(unsafe.Sizeof(emulatorCallbacks{})),
		WHvEmulatorIoPortCallback:               cbIoPort,
		WHvEmulatorMemoryCallback:               cbMemory,
		WHvEmulatorGetVirtualProcessorRegisters: cbGetRegisters,
		WHvEmulatorSetVirtualProcessorRegisters: cbSetRegisters,
		WHvEmulatorTranslateGvaPage:             cbTranslateGva,
	}
	var h EmulatorHandle
	hr, _, _ := procEmCreate.Call(
		uintptr(unsafe.Pointer(cbs)),
		uintptr(unsafe.Pointer(&h)),
	)
	if HResult(hr) != sOK {
		return nil, fmt.Errorf("WHvEmulatorCreateEmulator: %w", HResult(hr))
	}
	return &Emulator{handle: h, cbs: cbs}, nil
}

// Destroy releases the emulator. Safe to call multiple times.
func (e *Emulator) Destroy() error {
	if e == nil || e.handle == 0 {
		return nil
	}
	hr, _, _ := procEmDelete.Call(uintptr(e.handle))
	e.handle = 0
	if HResult(hr) != sOK {
		return fmt.Errorf("WHvEmulatorDestroyEmulator: %w", HResult(hr))
	}
	return nil
}

// TryMmioEmulation decodes the trapped MMIO instruction and routes
// the access through v.MMIORead/MMIOWrite. vpContextPtr points at the
// 40-byte WHV_VP_EXIT_CONTEXT inside the original WHV_RUN_VP_EXIT_CONTEXT
// (parent offset 8); mmioContextPtr points at the WHV_MEMORY_ACCESS_CONTEXT
// (parent offset 48). The caller is responsible for keeping those bytes
// valid for the duration of the call.
func (e *Emulator) TryMmioEmulation(v *EmulatorVCPU, vpContextPtr, mmioContextPtr unsafe.Pointer) error {
	token := registerVCPU(v)
	defer unregisterVCPU(token)
	var status EmulatorStatus
	hr, _, _ := procEmMmio.Call(
		uintptr(e.handle),
		token,
		uintptr(vpContextPtr),
		uintptr(mmioContextPtr),
		uintptr(unsafe.Pointer(&status)),
	)
	if HResult(hr) != sOK {
		return fmt.Errorf("WHvEmulatorTryMmioEmulation: %w", HResult(hr))
	}
	if !status.EmulationSuccessful() {
		return fmt.Errorf("WHvEmulatorTryMmioEmulation status: %s", status.Failed())
	}
	return nil
}

// TryIoEmulation decodes the trapped port-I/O instruction. We expose it
// for completeness; the run loop's IOPort exit path is simpler and is
// preferred for ports we know we handle.
func (e *Emulator) TryIoEmulation(v *EmulatorVCPU, vpContextPtr, ioContextPtr unsafe.Pointer) error {
	token := registerVCPU(v)
	defer unregisterVCPU(token)
	var status EmulatorStatus
	hr, _, _ := procEmIo.Call(
		uintptr(e.handle),
		token,
		uintptr(vpContextPtr),
		uintptr(ioContextPtr),
		uintptr(unsafe.Pointer(&status)),
	)
	if HResult(hr) != sOK {
		return fmt.Errorf("WHvEmulatorTryIoEmulation: %w", HResult(hr))
	}
	if !status.EmulationSuccessful() {
		return fmt.Errorf("WHvEmulatorTryIoEmulation status: %s", status.Failed())
	}
	return nil
}

// --- Callback implementations -------------------------------------------

// memoryCallback handles all the emulator's RAM accesses — for MMIO
// (routed to MMIORead/MMIOWrite) AND for accesses to plain RAM (e.g.,
// when the emulator fetches the source operand from guest memory).
// We can't tell them apart from the API alone, so the rule is:
//   - if the GPA falls inside v.Mem, treat as RAM
//   - otherwise treat as MMIO and call the registered dispatcher
//
//go:nocheckptr
func memoryCallback(context uintptr, accessPtr uintptr) uintptr {
	v := lookupVCPU(context)
	if v == nil {
		return uintptr(eFail)
	}
	access := (*emulatorMemoryAccessInfo)(unsafe.Pointer(accessPtr))
	gpa := access.GpaAddress
	size := access.AccessSize
	isWrite := access.Direction == 1
	memLen := uint64(len(v.Mem))

	// RAM-backed range: read/write directly into the host-side slice.
	if gpa < memLen && gpa+uint64(size) <= memLen {
		if isWrite {
			copy(v.Mem[gpa:gpa+uint64(size)], access.Data[:size])
		} else {
			copy(access.Data[:size], v.Mem[gpa:gpa+uint64(size)])
		}
		return uintptr(sOK)
	}

	// MMIO: dispatch through the registered handlers.
	if isWrite {
		if v.MMIOWrite != nil {
			v.MMIOWrite(gpa, size, access.Data[:size])
		}
	} else {
		if v.MMIORead != nil {
			data := v.MMIORead(gpa, size)
			n := int(size)
			if len(data) < n {
				n = len(data)
			}
			// Clear the buffer first so a short read yields zeros.
			for i := range access.Data {
				access.Data[i] = 0
			}
			copy(access.Data[:n], data[:n])
		} else {
			// No handler: return 0xFF (treat as "no device present" —
			// matches what Linux drivers expect on a missing MMIO
			// region).
			for i := uint8(0); i < size; i++ {
				access.Data[i] = 0xFF
			}
		}
	}
	return uintptr(sOK)
}

// ioPortCallback dispatches port I/O for TryIoEmulation. Most code paths
// don't use TryIoEmulation, but if a future caller wants the emulator
// to decode IN/OUT for them, this is the hook.
//
//go:nocheckptr
func ioPortCallback(context uintptr, accessPtr uintptr) uintptr {
	v := lookupVCPU(context)
	if v == nil {
		return uintptr(eFail)
	}
	access := (*emulatorIoAccessInfo)(unsafe.Pointer(accessPtr))
	if access.Direction == 1 {
		if v.IOPortOut != nil {
			v.IOPortOut(access.Port, access.AccessSize, access.Data)
		}
	} else {
		if v.IOPortIn != nil {
			access.Data = v.IOPortIn(access.Port, access.AccessSize)
		} else {
			access.Data = 0xFFFFFFFF
		}
	}
	return uintptr(sOK)
}

// getRegistersCallback forwards register reads to the underlying
// partition via the existing GetVCPURegisters binding. The WHP emulator
// uses it to read the source register on a `mov [mem], reg` or to know
// the segment base on a `mov [es:rdi], al`.
//
//go:nocheckptr
func getRegistersCallback(context uintptr, namesPtr uintptr, count uint32, valuesPtr uintptr) uintptr {
	v := lookupVCPU(context)
	if v == nil {
		return uintptr(eFail)
	}
	names := unsafe.Slice((*RegisterName)(unsafe.Pointer(namesPtr)), count)
	values := unsafe.Slice((*RegisterValue)(unsafe.Pointer(valuesPtr)), count)
	if err := GetVCPURegisters(v.Partition, v.VCPUIndex, names, values); err != nil {
		return uintptr(eFail)
	}
	return uintptr(sOK)
}

// setRegistersCallback writes back the registers the emulator updated
// (e.g. the destination of a `mov reg, [mem]` after it returns the
// read value).
//
//go:nocheckptr
func setRegistersCallback(context uintptr, namesPtr uintptr, count uint32, valuesPtr uintptr) uintptr {
	v := lookupVCPU(context)
	if v == nil {
		return uintptr(eFail)
	}
	names := unsafe.Slice((*RegisterName)(unsafe.Pointer(namesPtr)), count)
	values := unsafe.Slice((*RegisterValue)(unsafe.Pointer(valuesPtr)), count)
	if err := SetVCPURegisters(v.Partition, v.VCPUIndex, names, values); err != nil {
		return uintptr(eFail)
	}
	return uintptr(sOK)
}

// translateGvaCallback resolves a guest-virtual address to a guest-
// physical address. The emulator uses this when the trapped instruction
// referenced a memory operand via a segment register / RIP-relative
// addressing.
//
//go:nocheckptr
func translateGvaCallback(context uintptr, gva uint64, flags uint32, resultPtr uintptr, gpaPtr uintptr) uintptr {
	v := lookupVCPU(context)
	if v == nil {
		return uintptr(eFail)
	}
	gpa, transRes, err := TranslateGva(v.Partition, v.VCPUIndex, gva, flags)
	if err != nil {
		return uintptr(eFail)
	}
	*(*uint32)(unsafe.Pointer(resultPtr)) = transRes
	*(*uint64)(unsafe.Pointer(gpaPtr)) = gpa
	return uintptr(sOK)
}
