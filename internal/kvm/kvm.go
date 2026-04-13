// Package kvm provides low-level KVM hypervisor bindings for Linux.
// It wraps /dev/kvm ioctls to create and manage virtual machines.
package kvm

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// KVM ioctl numbers (Linux kernel ABI)
const (
	kvmGetAPIVersion   = 0xAE00
	kvmCreateVM        = 0xAE01
	kvmCreateVCPU      = uintptr(0xAE41)
	kvmGetDirtyLog     = uintptr(0x4010AE42)
	kvmSetUserMemory   = uintptr(0x4020AE46)
	kvmRun             = 0xAE80
	kvmGetRegs         = uintptr(0x8090AE81)
	kvmSetRegs         = uintptr(0x4090AE82)
	kvmGetSregs        = uintptr(0x8138AE83)
	kvmSetSregs        = uintptr(0x4138AE84)
	kvmGetVCPUMmapSize = 0xAE04
	kvmSetTSSAddr      = uintptr(0xAE47)
	kvmSetIdentityMap  = uintptr(0x4008AE48)
	kvmCreateIRQChip   = 0xAE60
	kvmCreatePIT2      = uintptr(0x4040AE77)
	kvmSetCPUID2       = uintptr(0x4008AE90)
	kvmGetCPUID2       = uintptr(0xC008AE91)
	kvmX86SetupMCE     = uintptr(0x4008AE9C)
	kvmIRQLine         = uintptr(0x4008AE61)
	kvmIRQFD           = uintptr(0x4020AE76)
	kvmSetGSIRouting   = uintptr(0x4008AE6A)
	kvmGetIRQChip      = uintptr(0xC208AE62)
	kvmSetIRQChip      = uintptr(0x8208AE63)
	kvmSetClock        = uintptr(0x4030AE7B)
	kvmGetClock        = uintptr(0x8030AE7C)
	kvmGetPIT2         = uintptr(0x8070AE9F)
	kvmSetPIT2         = uintptr(0x4070AEA0)

	// Additional ioctls for full vCPU setup
	kvmGetSupportedCPUID = uintptr(0xC008AE05) // system
	kvmSetMSRs           = uintptr(0x4008AE89) // vcpu
	kvmSetFPU            = uintptr(0x41A0AE8D) // vcpu
	kvmGetLAPIC          = uintptr(0x8400AE8E) // vcpu
	kvmSetLAPIC          = uintptr(0x4400AE8F) // vcpu
	kvmGetMPState        = uintptr(0x8004AE98) // vcpu
	kvmSetMPState        = uintptr(0x4004AE99) // vcpu
	kvmKVMClockCtrl      = uintptr(0xAEAD) // vcpu
)

const (
	kvmIRQRoutingIRQChip = 1
	kvmIRQChipIOAPIC     = 2
	kvmMemLogDirtyPages  = 1 << 0

	IRQChipPicMaster = 0
	IRQChipPicSlave  = 1
	IRQChipIOAPIC    = 2

	ClockTSCStable = 1 << 1
)

// x86 MP states from <linux/kvm.h>.
const (
	MPStateRunnable      = 0
	MPStateUninitialized = 1
	MPStateInitReceived  = 2
	MPStateHalted        = 3
	MPStateSIPIReceived  = 4
	MPStateStopped       = 5
	MPStateCheckStop     = 6
	MPStateOperating     = 7
	MPStateLoad          = 8
	MPStateAPResetHold   = 9
	MPStateSuspended     = 10
)

// Exit reasons from KVM_RUN
const (
	ExitUnknown       = 0
	ExitIO            = 2
	ExitHyperCall     = 3
	ExitDebug         = 4
	ExitHLT           = 5
	ExitMMIO          = 6
	ExitIRQWindowOpen = 7
	ExitShutdown      = 8
	ExitFailEntry     = 9
	ExitInternalError = 17
	ExitSystemEvent   = 24
)

// MemoryRegion maps guest physical memory to host virtual memory.
type MemoryRegion struct {
	Slot          uint32
	Flags         uint32
	GuestPhysAddr uint64
	MemorySize    uint64
	UserspaceAddr uint64
}

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

// RunData is the shared memory region between kernel and userspace for KVM_RUN.
// Its layout must match struct kvm_run in the kernel.
type RunData struct {
	RequestInterruptWindow     uint8
	ImmediateExit              uint8
	_                          [6]uint8
	ExitReason                 uint32
	ReadyForInterruptInjection uint8
	IfFlag                     uint8
	Flags                      uint16
	CR8                        uint64
	ApicBase                   uint64
	// Union data - we use the largest member (MMIO) to size it.
	Data [256]byte
}

// IOData extracts IO exit information from RunData.
type IOData struct {
	Direction  uint8 // 0=in, 1=out
	Size       uint8
	Port       uint16
	Count      uint32
	DataOffset uint64
}

// MMIOData extracts MMIO exit information from RunData.
type MMIOData struct {
	PhysAddr uint64
	Data     [8]byte
	Len      uint32
	IsWrite  uint8
}

// System wraps the KVM system fd (/dev/kvm).
type System struct {
	fd int
}

// VM wraps a KVM virtual machine fd.
type VM struct {
	fd             int
	mem            []byte
	memSize        uint64
	guestPhysBase  uint64 // GPA where RAM starts (0 on x86, 0x40000000 on ARM64)
	vcpuMmapSz     int
	memFlags       uint32
	memfd          int
	regions        map[uint32]*MappedMemoryRegion
}

// GuestPhysBase returns the guest physical address where RAM starts.
func (v *VM) GuestPhysBase() uint64 { return v.guestPhysBase }

type MappedMemoryRegion struct {
	Slot          uint32
	Flags         uint32
	GuestPhysAddr uint64
	MemorySize    uint64
	UserspaceAddr uint64
	mem           []byte
	memfd         int
}

// VCPU wraps a KVM virtual CPU fd and its shared run data.
type VCPU struct {
	ID      int
	fd      int
	RunData *RunData
	runMmap []byte
}

type DirtyLog struct {
	Slot        uint32
	Padding1    uint32
	DirtyBitmap uint64
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

// PITConfig matches struct kvm_pit_config.
type PITConfig struct {
	Flags uint32
	Pad   [15]uint32
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

// Open opens /dev/kvm and returns a System handle.
func Open() (*System, error) {
	fd, err := unix.Open("/dev/kvm", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/kvm: %w", err)
	}
	s := &System{fd: fd}
	ver, err := s.ioctl(kvmGetAPIVersion, 0)
	if err != nil {
		return nil, fmt.Errorf("KVM_GET_API_VERSION: %w", err)
	}
	if ver != 12 {
		return nil, fmt.Errorf("unexpected KVM API version: %d", ver)
	}
	return s, nil
}

func (s *System) Close() error {
	if s == nil || s.fd <= 0 {
		return nil
	}
	err := unix.Close(s.fd)
	s.fd = -1
	return err
}

// CreateVM creates a new virtual machine with guest RAM mapped at GPA 0.
func (s *System) CreateVM(memMB uint64) (*VM, error) {
	return s.CreateVMWithBase(memMB, 0)
}

// CreateVMWithBase creates a new virtual machine with guest RAM at the given
// guest physical base address. ARM64 uses 0x40000000; x86 uses 0.
func (s *System) CreateVMWithBase(memMB uint64, guestPhysBase uint64) (*VM, error) {
	vmFd, err := s.ioctl(kvmCreateVM, 0)
	if err != nil {
		return nil, fmt.Errorf("KVM_CREATE_VM: %w", err)
	}
	mmapSz, err := s.ioctl(kvmGetVCPUMmapSize, 0)
	if err != nil {
		return nil, fmt.Errorf("KVM_GET_VCPU_MMAP_SIZE: %w", err)
	}

	memSize := memMB * 1024 * 1024
	memfd, err := unix.MemfdCreate("gocracker-guest-ram", unix.MFD_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("memfd_create guest memory: %w", err)
	}
	if err := unix.Ftruncate(memfd, int64(memSize)); err != nil {
		_ = unix.Close(memfd)
		return nil, fmt.Errorf("ftruncate guest memory: %w", err)
	}
	mem, err := unix.Mmap(memfd, 0, int(memSize),
		unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_SHARED|unix.MAP_NORESERVE)
	if err != nil {
		_ = unix.Close(memfd)
		return nil, fmt.Errorf("mmap guest memory: %w", err)
	}
	// Request transparent huge pages to reduce page faults during kernel loading.
	_ = unix.Madvise(mem, unix.MADV_HUGEPAGE)

	vm := &VM{fd: int(vmFd), mem: mem, memSize: memSize, vcpuMmapSz: int(mmapSz), memfd: memfd, regions: make(map[uint32]*MappedMemoryRegion)}

	// Register the memory region with KVM
	region := MemoryRegion{
		Slot:          0,
		Flags:         vm.memFlags,
		GuestPhysAddr: guestPhysBase,
		MemorySize:    memSize,
		UserspaceAddr: uint64(uintptr(unsafe.Pointer(&mem[0]))),
	}
	vm.guestPhysBase = guestPhysBase
	if _, err := vmIoctl(vm.fd, kvmSetUserMemory, uintptr(unsafe.Pointer(&region))); err != nil {
		_ = unix.Munmap(mem)
		_ = unix.Close(memfd)
		_ = unix.Close(int(vmFd))
		return nil, fmt.Errorf("KVM_SET_USER_MEMORY_REGION: %w", err)
	}

	if err := s.initVMArch(vm); err != nil {
		_ = vm.Close()
		return nil, err
	}

	return vm, nil
}

// Memory returns the guest RAM slice.
func (v *VM) Memory() []byte { return v.mem }

// MemoryFD returns the memfd backing guest RAM, suitable for vhost-user backends.
func (v *VM) MemoryFD() int { return v.memfd }

func (v *VM) EnableDirtyLogging() error {
	return v.setMemoryFlags(v.memFlags | kvmMemLogDirtyPages)
}

func (v *VM) Close() error {
	if v == nil {
		return nil
	}
	var firstErr error
	for slot := range v.regions {
		if err := v.RemoveMemoryRegion(slot); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if len(v.mem) > 0 {
		if err := unix.Munmap(v.mem); err != nil && firstErr == nil {
			firstErr = err
		}
		v.mem = nil
	}
	if v.memfd > 0 {
		if err := unix.Close(v.memfd); err != nil && firstErr == nil {
			firstErr = err
		}
		v.memfd = -1
	}
	if v.fd > 0 {
		if err := unix.Close(v.fd); err != nil && firstErr == nil {
			firstErr = err
		}
		v.fd = -1
	}
	return firstErr
}

func (v *VM) DisableDirtyLogging() error {
	return v.setMemoryFlags(v.memFlags &^ kvmMemLogDirtyPages)
}

func (v *VM) DirtyLoggingEnabled() bool {
	return v.memFlags&kvmMemLogDirtyPages != 0
}

func (v *VM) GetDirtyLog(slot uint32) ([]uint64, error) {
	pageSize := uint64(unix.Getpagesize())
	pageCount := (v.memSize + pageSize - 1) / pageSize
	wordCount := (pageCount + 63) / 64
	bitmap := make([]uint64, wordCount)
	if len(bitmap) == 0 {
		return nil, nil
	}
	log := DirtyLog{
		Slot:        slot,
		DirtyBitmap: uint64(uintptr(unsafe.Pointer(&bitmap[0]))),
	}
	if _, err := vmIoctl(v.fd, kvmGetDirtyLog, uintptr(unsafe.Pointer(&log))); err != nil {
		return nil, fmt.Errorf("KVM_GET_DIRTY_LOG: %w", err)
	}
	return bitmap, nil
}

func (v *VM) ResetDirtyLog(slot uint32) error {
	_, err := v.GetDirtyLog(slot)
	return err
}

// GetClock reads the in-kernel clock state for migration/snapshot.
func (v *VM) GetClock() (ClockData, error) {
	var clock ClockData
	_, err := vmIoctl(v.fd, kvmGetClock, uintptr(unsafe.Pointer(&clock)))
	if err != nil {
		return ClockData{}, fmt.Errorf("KVM_GET_CLOCK: %w", err)
	}
	return clock, nil
}

// SetClock restores the in-kernel clock state.
func (v *VM) SetClock(clock ClockData) error {
	_, err := vmIoctl(v.fd, kvmSetClock, uintptr(unsafe.Pointer(&clock)))
	if err != nil {
		return fmt.Errorf("KVM_SET_CLOCK: %w", err)
	}
	return nil
}

// GetPIT2 reads the PIT state used for timer interrupts.
func (v *VM) GetPIT2() (PITState2, error) {
	var pit PITState2
	_, err := vmIoctl(v.fd, kvmGetPIT2, uintptr(unsafe.Pointer(&pit)))
	if err != nil {
		return PITState2{}, fmt.Errorf("KVM_GET_PIT2: %w", err)
	}
	return pit, nil
}

// SetPIT2 restores the PIT state used for timer interrupts.
func (v *VM) SetPIT2(pit PITState2) error {
	_, err := vmIoctl(v.fd, kvmSetPIT2, uintptr(unsafe.Pointer(&pit)))
	if err != nil {
		return fmt.Errorf("KVM_SET_PIT2: %w", err)
	}
	return nil
}

// GetIRQChip reads one in-kernel irqchip state blob.
func (v *VM) GetIRQChip(chipID uint32) (IRQChip, error) {
	chip := IRQChip{ChipID: chipID}
	_, err := vmIoctl(v.fd, kvmGetIRQChip, uintptr(unsafe.Pointer(&chip)))
	if err != nil {
		return IRQChip{}, fmt.Errorf("KVM_GET_IRQCHIP(%d): %w", chipID, err)
	}
	return chip, nil
}

// SetIRQChip restores one in-kernel irqchip state blob.
func (v *VM) SetIRQChip(chip IRQChip) error {
	_, err := vmIoctl(v.fd, kvmSetIRQChip, uintptr(unsafe.Pointer(&chip)))
	if err != nil {
		return fmt.Errorf("KVM_SET_IRQCHIP(%d): %w", chip.ChipID, err)
	}
	return nil
}

func (v *VM) setMemoryFlags(flags uint32) error {
	region := MemoryRegion{
		Slot:          0,
		Flags:         flags,
		GuestPhysAddr: 0,
		MemorySize:    v.memSize,
		UserspaceAddr: uint64(uintptr(unsafe.Pointer(&v.mem[0]))),
	}
	if _, err := vmIoctl(v.fd, kvmSetUserMemory, uintptr(unsafe.Pointer(&region))); err != nil {
		return fmt.Errorf("KVM_SET_USER_MEMORY_REGION: %w", err)
	}
	v.memFlags = flags
	return nil
}

func (v *VM) AddMemoryRegion(slot uint32, guestPhysAddr, size uint64, flags uint32) (*MappedMemoryRegion, error) {
	if size == 0 {
		return nil, fmt.Errorf("memory region size must be positive")
	}
	if slot == 0 {
		return nil, fmt.Errorf("memory region slot 0 is reserved for base guest RAM")
	}
	if _, exists := v.regions[slot]; exists {
		return nil, fmt.Errorf("memory region slot %d already exists", slot)
	}

	memfd, err := unix.MemfdCreate(fmt.Sprintf("gocracker-hotplug-%d", slot), unix.MFD_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("memfd_create memory region %d: %w", slot, err)
	}
	if err := unix.Ftruncate(memfd, int64(size)); err != nil {
		_ = unix.Close(memfd)
		return nil, fmt.Errorf("ftruncate memory region %d: %w", slot, err)
	}
	mem, err := unix.Mmap(memfd, 0, int(size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_NORESERVE)
	if err != nil {
		_ = unix.Close(memfd)
		return nil, fmt.Errorf("mmap memory region %d: %w", slot, err)
	}

	region := &MappedMemoryRegion{
		Slot:          slot,
		Flags:         flags,
		GuestPhysAddr: guestPhysAddr,
		MemorySize:    size,
		UserspaceAddr: uint64(uintptr(unsafe.Pointer(&mem[0]))),
		mem:           mem,
		memfd:         memfd,
	}
	kvmRegion := MemoryRegion{
		Slot:          region.Slot,
		Flags:         region.Flags,
		GuestPhysAddr: region.GuestPhysAddr,
		MemorySize:    region.MemorySize,
		UserspaceAddr: region.UserspaceAddr,
	}
	if _, err := vmIoctl(v.fd, kvmSetUserMemory, uintptr(unsafe.Pointer(&kvmRegion))); err != nil {
		_ = unix.Munmap(mem)
		_ = unix.Close(memfd)
		return nil, fmt.Errorf("KVM_SET_USER_MEMORY_REGION(slot=%d): %w", slot, err)
	}
	v.regions[slot] = region
	return region, nil
}

func (v *VM) RemoveMemoryRegion(slot uint32) error {
	region, ok := v.regions[slot]
	if !ok {
		return nil
	}
	deregister := MemoryRegion{
		Slot:          region.Slot,
		Flags:         region.Flags,
		GuestPhysAddr: region.GuestPhysAddr,
		MemorySize:    0,
		UserspaceAddr: 0,
	}
	var firstErr error
	if _, err := vmIoctl(v.fd, kvmSetUserMemory, uintptr(unsafe.Pointer(&deregister))); err != nil {
		firstErr = fmt.Errorf("KVM_SET_USER_MEMORY_REGION(slot=%d remove): %w", slot, err)
	}
	if len(region.mem) > 0 {
		if err := unix.Munmap(region.mem); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if region.memfd >= 0 {
		if err := unix.Close(region.memfd); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	delete(v.regions, slot)
	return firstErr
}

// CreateVCPU creates a virtual CPU attached to this VM.
func (v *VM) CreateVCPU(id int) (*VCPU, error) {
	fd, err := vmIoctl(v.fd, kvmCreateVCPU, uintptr(id))
	if err != nil {
		return nil, fmt.Errorf("KVM_CREATE_VCPU: %w", err)
	}

	// Map the shared run data region
	data, err := unix.Mmap(int(fd), 0, v.vcpuMmapSz,
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		_ = unix.Close(int(fd))
		return nil, fmt.Errorf("mmap vcpu: %w", err)
	}

	run := (*RunData)(unsafe.Pointer(&data[0]))
	return &VCPU{ID: id, fd: int(fd), RunData: run, runMmap: data}, nil
}

func (c *VCPU) Close() error {
	if c == nil {
		return nil
	}
	var firstErr error
	if len(c.runMmap) > 0 {
		if err := unix.Munmap(c.runMmap); err != nil && firstErr == nil {
			firstErr = err
		}
		c.runMmap = nil
		c.RunData = nil
	}
	if c.fd > 0 {
		if err := unix.Close(c.fd); err != nil && firstErr == nil {
			firstErr = err
		}
		c.fd = -1
	}
	return firstErr
}

// GetRegs reads general-purpose registers.
func (c *VCPU) GetRegs() (Regs, error) {
	var r Regs
	_, err := vmIoctl(c.fd, kvmGetRegs, uintptr(unsafe.Pointer(&r)))
	return r, err
}

// SetRegs writes general-purpose registers.
func (c *VCPU) SetRegs(r Regs) error {
	_, err := vmIoctl(c.fd, kvmSetRegs, uintptr(unsafe.Pointer(&r)))
	return err
}

// GetSregs reads special registers.
func (c *VCPU) GetSregs() (Sregs, error) {
	var s Sregs
	_, err := vmIoctl(c.fd, kvmGetSregs, uintptr(unsafe.Pointer(&s)))
	return s, err
}

// SetSregs writes special registers.
func (c *VCPU) SetSregs(s Sregs) error {
	_, err := vmIoctl(c.fd, kvmSetSregs, uintptr(unsafe.Pointer(&s)))
	return err
}

// GetMPState reads the MP state for this vCPU.
func (c *VCPU) GetMPState() (MPState, error) {
	var state MPState
	_, err := vmIoctl(c.fd, kvmGetMPState, uintptr(unsafe.Pointer(&state)))
	return state, err
}

// SetMPState writes the MP state for this vCPU.
func (c *VCPU) SetMPState(state MPState) error {
	_, err := vmIoctl(c.fd, kvmSetMPState, uintptr(unsafe.Pointer(&state)))
	return err
}

// GetLAPIC reads the local APIC state for this vCPU.
func (c *VCPU) GetLAPIC() (LAPICState, error) {
	var lapic LAPICState
	_, err := vmIoctl(c.fd, kvmGetLAPIC, uintptr(unsafe.Pointer(&lapic)))
	if err != nil {
		return LAPICState{}, fmt.Errorf("KVM_GET_LAPIC: %w", err)
	}
	return lapic, nil
}

// SetLAPIC restores the local APIC state for this vCPU.
func (c *VCPU) SetLAPIC(lapic LAPICState) error {
	_, err := vmIoctl(c.fd, kvmSetLAPIC, uintptr(unsafe.Pointer(&lapic)))
	if err != nil {
		return fmt.Errorf("KVM_SET_LAPIC: %w", err)
	}
	return nil
}

// KVMClockCtrl notifies KVM that vCPU timer state was restored.
func (c *VCPU) KVMClockCtrl() error {
	_, err := vmIoctl(c.fd, kvmKVMClockCtrl, 0)
	if err != nil {
		return fmt.Errorf("KVM_KVMCLOCK_CTRL: %w", err)
	}
	return nil
}

// Run executes the vCPU until an exit event occurs.
func (c *VCPU) Run() error {
	_, err := vmIoctl(c.fd, uintptr(kvmRun), 0)
	return err
}

// GetIOData parses IO exit data from the run region.
func (c *VCPU) GetIOData() IOData {
	return *(*IOData)(unsafe.Pointer(&c.RunData.Data[0]))
}

// GetMMIOData returns a pointer to the MMIO exit data in the run region.
// Returns a pointer so writes to Data[] go directly to the shared memory
// that the kernel reads on the next KVM_RUN.
func (c *VCPU) GetMMIOData() *MMIOData {
	return (*MMIOData)(unsafe.Pointer(&c.RunData.Data[0]))
}

// cpuidEntry2 matches struct kvm_cpuid_entry2 (40 bytes).
type cpuidEntry2 struct {
	Function uint32
	Index    uint32
	Flags    uint32
	EAX      uint32
	EBX      uint32
	ECX      uint32
	EDX      uint32
	_        [3]uint32
}

// cpuid2 matches struct kvm_cpuid2 with room for 256 entries.
type cpuid2 struct {
	Nent    uint32
	Pad     uint32
	Entries [256]cpuidEntry2
}

// msrEntry matches struct kvm_msr_entry (16 bytes).
type msrEntry struct {
	Index    uint32
	Reserved uint32
	Data     uint64
}

// msrList matches struct kvm_msrs with room for 16 entries.
type msrList struct {
	Nmsrs   uint32
	Pad     uint32
	Entries [16]msrEntry
}

// fpuState matches struct kvm_fpu (416 bytes).
type fpuState struct {
	FPR    [8][16]byte
	FCW    uint16
	FSW    uint16
	FTWX   uint8
	Pad1   uint8
	FOP    uint16
	LastIP uint64
	LastDP uint64
	XMM    [16][16]byte
	MXCSR  uint32
	Pad2   uint32
}

// IRQLevel represents an IRQ line and its level (0=deassert, 1=assert).
type IRQLevel struct {
	IRQ   uint32
	Level uint32
}

type IRQRoutingIRQChip struct {
	IRQChip uint32
	Pin     uint32
}

type IRQRoutingEntry struct {
	GSI   uint32
	Type  uint32
	Flags uint32
	Pad   uint32

	IRQChip IRQRoutingIRQChip
	_       [24]byte
}

type irqRouting struct {
	Nr    uint32
	Flags uint32
}

// IRQLine asserts or deasserts an IRQ line on the in-kernel interrupt controller.
func (v *VM) IRQLine(irq uint32, level int) error {
	l := IRQLevel{IRQ: irq, Level: uint32(level)}
	_, err := vmIoctl(v.fd, kvmIRQLine, uintptr(unsafe.Pointer(&l)))
	return err
}

// kvmIRQFDData matches struct kvm_irqfd from <linux/kvm.h>.
type kvmIRQFDData struct {
	FD          uint32
	GSI         uint32
	Flags       uint32
	ResampleFD  uint32
	Pad         [16]byte
}

const kvmIRQFDFlagDeassign = 1 << 0

// RegisterIRQFD registers an eventfd with KVM so that writing to it injects
// the interrupt for the given GSI into the guest. This is how Firecracker
// delivers all device interrupts on both x86 and ARM64.
func (v *VM) RegisterIRQFD(eventFD int, gsi uint32) error {
	data := kvmIRQFDData{
		FD:  uint32(eventFD),
		GSI: gsi,
	}
	_, err := vmIoctl(v.fd, kvmIRQFD, uintptr(unsafe.Pointer(&data)))
	if err != nil {
		return fmt.Errorf("KVM_IRQFD register gsi=%d: %w", gsi, err)
	}
	return nil
}

// DeregisterIRQFD removes a previously registered irqfd.
func (v *VM) DeregisterIRQFD(eventFD int, gsi uint32) error {
	data := kvmIRQFDData{
		FD:    uint32(eventFD),
		GSI:   gsi,
		Flags: kvmIRQFDFlagDeassign,
	}
	_, err := vmIoctl(v.fd, kvmIRQFD, uintptr(unsafe.Pointer(&data)))
	if err != nil {
		return fmt.Errorf("KVM_IRQFD deregister gsi=%d: %w", gsi, err)
	}
	return nil
}

// SetGSIRouting programs the KVM GSI routing table for the provided GSIs.
// Each GSI is routed to the in-kernel IOAPIC pin with the same number,
// matching Firecracker's x86 legacy interrupt routing.
func (v *VM) SetGSIRouting(irqs []uint32) error {
	return v.SetGSIRoutingChip(irqs, kvmIRQChipIOAPIC)
}

// SetGSIRoutingGIC programs GSI routing for ARM64's in-kernel GIC (irqchip=0).
// Firecracker sets this up even on ARM64 for KVM_IRQ_LINE to work.
func (v *VM) SetGSIRoutingGIC(irqs []uint32) error {
	return v.SetGSIRoutingChip(irqs, 0)
}

// SetGSIRoutingChip programs the KVM GSI routing table using the given irqchip ID.
// x86 uses kvmIRQChipIOAPIC (2); ARM64 uses 0 (in-kernel GIC).
func (v *VM) SetGSIRoutingChip(irqs []uint32, chipID uint32) error {
	if len(irqs) == 0 {
		return nil
	}

	headerSize := unsafe.Sizeof(irqRouting{})
	entrySize := unsafe.Sizeof(IRQRoutingEntry{})
	buf := make([]byte, headerSize+uintptr(len(irqs))*entrySize)

	hdr := (*irqRouting)(unsafe.Pointer(&buf[0]))
	hdr.Nr = uint32(len(irqs))

	entriesPtr := unsafe.Pointer(uintptr(unsafe.Pointer(&buf[0])) + headerSize)
	entries := unsafe.Slice((*IRQRoutingEntry)(entriesPtr), len(irqs))
	for i, irq := range irqs {
		entries[i] = IRQRoutingEntry{
			GSI:  irq,
			Type: kvmIRQRoutingIRQChip,
			IRQChip: IRQRoutingIRQChip{
				IRQChip: chipID,
				Pin:     irq,
			},
		}
	}

	_, err := vmIoctl(v.fd, kvmSetGSIRouting, uintptr(unsafe.Pointer(&buf[0])))
	if err != nil {
		return fmt.Errorf("KVM_SET_GSI_ROUTING: %w", err)
	}
	return nil
}

// RunDataByte returns a pointer to a byte at the given offset from the start
// of RunData. DataOffset from IO exits is relative to the start of RunData,
// not the Data field, so we use unsafe.Add from the RunData base.
func (c *VCPU) RunDataByte(offset uint64) *byte {
	return (*byte)(unsafe.Add(unsafe.Pointer(c.RunData), offset))
}

// SetupCPUID passes the host-supported CPUID leaves through to the vCPU.
func SetupCPUID(sys *System, vcpu *VCPU) error {
	var supported cpuid2
	supported.Nent = 256
	if _, err := sys.ioctl(kvmGetSupportedCPUID, uintptr(unsafe.Pointer(&supported))); err != nil {
		return fmt.Errorf("KVM_GET_SUPPORTED_CPUID: %w", err)
	}
	if _, err := vmIoctl(vcpu.fd, kvmSetCPUID2, uintptr(unsafe.Pointer(&supported))); err != nil {
		return fmt.Errorf("KVM_SET_CPUID2: %w", err)
	}
	return nil
}

// SetupMSRs configures boot-time model-specific registers matching Firecracker.
func SetupMSRs(vcpu *VCPU) error {
	msrs := msrList{Nmsrs: 12}
	msrs.Entries[0] = msrEntry{Index: 0x174}                         // IA32_SYSENTER_CS
	msrs.Entries[1] = msrEntry{Index: 0x175}                         // IA32_SYSENTER_ESP
	msrs.Entries[2] = msrEntry{Index: 0x176}                         // IA32_SYSENTER_EIP
	msrs.Entries[3] = msrEntry{Index: 0x10}                          // IA32_TSC
	msrs.Entries[4] = msrEntry{Index: 0x1A0, Data: 1}                // IA32_MISC_ENABLE (fast string)
	msrs.Entries[5] = msrEntry{Index: 0xC0000080, Data: 0x500}       // EFER (LME+LMA)
	msrs.Entries[6] = msrEntry{Index: 0xC0000081}                    // STAR
	msrs.Entries[7] = msrEntry{Index: 0xC0000082}                    // LSTAR
	msrs.Entries[8] = msrEntry{Index: 0xC0000083}                    // CSTAR
	msrs.Entries[9] = msrEntry{Index: 0xC0000084}                    // SYSCALL_MASK
	msrs.Entries[10] = msrEntry{Index: 0xC0000102}                   // KERNEL_GS_BASE
	msrs.Entries[11] = msrEntry{Index: 0x2FF, Data: (1 << 11) | 0x6} // MTRRdefType: MTRR enabled + write-back
	if _, err := vmIoctl(vcpu.fd, kvmSetMSRs, uintptr(unsafe.Pointer(&msrs))); err != nil {
		return fmt.Errorf("KVM_SET_MSRS: %w", err)
	}
	return nil
}

// SetupFPU initializes the FPU to a known-good state.
func SetupFPU(vcpu *VCPU) error {
	fpu := fpuState{
		FCW:   0x37F,
		MXCSR: 0x1F80,
	}
	if _, err := vmIoctl(vcpu.fd, kvmSetFPU, uintptr(unsafe.Pointer(&fpu))); err != nil {
		return fmt.Errorf("KVM_SET_FPU: %w", err)
	}
	return nil
}

// SetupLAPIC configures LVT0 as ExtINT and LVT1 as NMI for IRQ delivery.
func SetupLAPIC(vcpu *VCPU) error {
	lapic, err := vcpu.GetLAPIC()
	if err != nil {
		return err
	}
	// LVT LINT0 at offset 0x350: ExtINT delivery mode (0x700)
	lapic.Regs[0x350] = 0x00
	lapic.Regs[0x351] = 0x07
	lapic.Regs[0x352] = 0x00
	lapic.Regs[0x353] = 0x00
	// LVT LINT1 at offset 0x360: NMI delivery mode (0x400)
	lapic.Regs[0x360] = 0x00
	lapic.Regs[0x361] = 0x04
	lapic.Regs[0x362] = 0x00
	lapic.Regs[0x363] = 0x00
	return vcpu.SetLAPIC(lapic)
}

// --- helpers ---

func (s *System) ioctl(nr uintptr, arg uintptr) (uintptr, error) {
	r, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(s.fd), nr, arg)
	if errno != 0 {
		return 0, errno
	}
	return r, nil
}

func vmIoctl(fd int, nr uintptr, arg uintptr) (uintptr, error) {
	r, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), nr, arg)
	if errno != 0 {
		return 0, errno
	}
	return r, nil
}
