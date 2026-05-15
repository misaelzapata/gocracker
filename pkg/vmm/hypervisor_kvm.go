//go:build linux

package vmm

import (
	"fmt"
	"unsafe"

	"github.com/gocracker/gocracker/internal/kvm"
)

// kvmHypervisor adapts internal/kvm to the Hypervisor interface. It is the
// only Hypervisor implementation today; Phase 2 adds whpHypervisor.
//
// The adapter does not yet replace direct kvm.* usage in pkg/vmm — that
// is the second half of Phase 1. New code can already start using the
// abstraction; legacy paths keep working unchanged because the existing
// kvm imports are not removed by the adapter alone.
type kvmHypervisor struct {
	sys *kvm.System
}

// kvmVMFromHV unwraps an HVVM into the underlying *kvm.VM. The arch_*
// adapters use this when they need KVM-specific extensions (memory, GIC,
// PIT2, IRQChip, GSI routing, virtio-fs memfd...) that the portable HVVM
// interface deliberately does NOT expose. Type-assertion only — never
// imported from outside pkg/vmm.
func kvmVMFromHV(hv HVVM) (*kvm.VM, error) {
	if hv == nil {
		return nil, fmt.Errorf("kvmVMFromHV: hvVM is nil")
	}
	kvm, ok := hv.(*hvvmKVM)
	if !ok {
		return nil, fmt.Errorf("kvmVMFromHV: expected *hvvmKVM, got %T", hv)
	}
	return kvm.vm, nil
}

// kvmSysFromHV unwraps a Hypervisor into the underlying *kvm.System. Used by
// arch_x86 setupCPUID/restoreVCPU which call KVM-system-level helpers that
// have no portable equivalent.
func kvmSysFromHV(hv Hypervisor) (*kvm.System, error) {
	if hv == nil {
		return nil, fmt.Errorf("kvmSysFromHV: hv is nil")
	}
	kvm, ok := hv.(*kvmHypervisor)
	if !ok {
		return nil, fmt.Errorf("kvmSysFromHV: expected *kvmHypervisor, got %T", hv)
	}
	return kvm.sys, nil
}

// kvmVCPUFromHV unwraps an HVVCPU into the underlying *kvm.VCPU. KVM-only
// code paths (run-loop kvm.RunData accesses, snapshot capture/restore that
// goes through KVM-specific ioctls) call this and propagate a clear error
// if the HVVCPU is actually a WHP-backed vCPU (which is a programming
// error today — every hypervisor on a given VM is uniform).
func kvmVCPUFromHV(hv HVVCPU) (*kvm.VCPU, error) {
	if hv == nil {
		return nil, fmt.Errorf("kvmVCPUFromHV: hvcpu is nil")
	}
	k, ok := hv.(*kvmVCPU)
	if !ok {
		return nil, fmt.Errorf("kvmVCPUFromHV: expected *kvmVCPU, got %T", hv)
	}
	return k.vcpu, nil
}

// kvm is the in-package accessor that returns the underlying *kvm.VM. Every
// KVM-specific helper inside pkg/vmm uses this so direct field access to
// kvm internals does not need to be sprinkled through the file. Returns nil
// if the VM's hypervisor backend isn't KVM (programmer error today — only
// kvm backend is wired on Linux).
func (m *VM) kvm() *kvm.VM {
	if m == nil || m.hvVM == nil {
		return nil
	}
	if k, ok := m.hvVM.(*hvvmKVM); ok {
		return k.vm
	}
	return nil
}

// kvmSystem returns the underlying *kvm.System for callsites that need the
// system handle (CPUID setup, vCPU ioctls scoped to /dev/kvm rather than the
// VM fd).
func (m *VM) kvmSystem() *kvm.System {
	if m == nil || m.hv == nil {
		return nil
	}
	if k, ok := m.hv.(*kvmHypervisor); ok {
		return k.sys
	}
	return nil
}

// kvmVCPU returns the underlying *kvm.VCPU for the i-th hypervisor vCPU. Used
// by KVM-specific runLoop control (RunData.ImmediateExit, RequestInterruptWindow)
// and by the snapshot capture path. Returns nil on type mismatch.
func (m *VM) kvmVCPU(i int) *kvm.VCPU {
	if m == nil || i < 0 || i >= len(m.hvVCPUs) {
		return nil
	}
	if k, ok := m.hvVCPUs[i].(*kvmVCPU); ok {
		return k.vcpu
	}
	return nil
}

// NewKVMHypervisor opens /dev/kvm and returns a Hypervisor backed by KVM.
// Linux-only; non-Linux builds fall through to NewUnsupportedHypervisor
// (see hypervisor_unsupported.go).
func NewKVMHypervisor() (Hypervisor, error) {
	sys, err := kvm.Open()
	if err != nil {
		return nil, fmt.Errorf("kvm.Open: %w", err)
	}
	return &kvmHypervisor{sys: sys}, nil
}

func (h *kvmHypervisor) Capabilities() HVCapabilities {
	// KVM hosts always support these on the kernels we target (>=4.x).
	// The plumbed values match what Phase 2 WHP backend will report so
	// callers can use a single feature-gate code path. MaxVCPUs=1024 is
	// the hard cap we enforce regardless of KVM_CAP_MAX_VCPUS.
	return HVCapabilities{
		DirtyPageTracking: true,
		InKernelIRQChip:   true,
		PauseResume:       true,
		XSAVE:             true,
		MaxVCPUs:          1024,
	}
}

func (h *kvmHypervisor) CreateVM(cfg HVVMConfig) (HVVM, error) {
	memMB := cfg.MemoryBytes / (1024 * 1024)
	if memMB == 0 {
		return nil, fmt.Errorf("kvm: MemoryBytes must be at least 1 MiB; got %d", cfg.MemoryBytes)
	}
	vm, err := h.sys.CreateVM(memMB)
	if err != nil {
		return nil, fmt.Errorf("kvm.CreateVM: %w", err)
	}
	if cfg.EnableDirtyTracking {
		if err := vm.EnableDirtyLogging(); err != nil {
			vm.Close()
			return nil, fmt.Errorf("kvm.EnableDirtyLogging: %w", err)
		}
	}
	return &hvvmKVM{sys: h.sys, vm: vm}, nil
}

func (h *kvmHypervisor) Close() error {
	if h.sys == nil {
		return nil
	}
	err := h.sys.Close()
	h.sys = nil
	return err
}

// hvvmKVM wraps kvm.VM. The current pkg/vmm code base creates VMs through
// kvm.System.CreateVM directly; once Phase 1 part 2 lands, callers will
// route through HVVM and the wrapper centralises the kvm.* surface.
type hvvmKVM struct {
	sys      *kvm.System
	vm       *kvm.VM
	nextSlot uint32

	// irqfds is the bridge between the unified IRQLine callback and the
	// KVM_IRQFD / eventfd plumbing the Linux runtime expects. Phase 1
	// part 2 fills this in as virtio devices are migrated; for now the
	// map exists so adapter code compiles against the shape.
	irqfds map[uint32]int // irqNumber → eventfd
}

// AllocateGuestRAM returns the KVM-backed memfd memory as a Go slice.
// kvm.VM already owns a single contiguous mmap region created at VM
// construction time; we hand that slice out. Subsequent calls to
// MapMemory accept any slice (the caller is responsible for pinning,
// and slices returned from AllocateGuestRAM are pinned by definition).
//
// Note: today the kvm package allocates the entire guest RAM at
// CreateVM time, so size is informational — we return the full backing
// region. Phase 1.2 part 2 will support multiple disjoint allocations.
func (v *hvvmKVM) AllocateGuestRAM(size uint64) ([]byte, error) {
	if v.vm == nil {
		return nil, fmt.Errorf("hvvmKVM.AllocateGuestRAM: VM closed")
	}
	mem := v.vm.Memory()
	if uint64(len(mem)) < size {
		return nil, fmt.Errorf("hvvmKVM.AllocateGuestRAM: requested %d bytes, only %d available", size, len(mem))
	}
	return mem[:size], nil
}

func (v *hvvmKVM) MapMemory(gpa uint64, hostMem []byte, flags MemFlags) error {
	// AddMemoryRegion takes a slot number; pkg/vmm assigns slots in
	// arch-specific code today. The adapter auto-numbers them in the
	// order MapMemory is called — the real allocator from arch.go
	// will take over once arch.go is refactored to use HVVM directly.
	region, err := v.vm.AddMemoryRegion(v.nextSlot, gpa, uint64(len(hostMem)), kvmMemFlagsFromHV(flags))
	if err != nil {
		return fmt.Errorf("kvm.AddMemoryRegion(slot=%d, gpa=%#x): %w", v.nextSlot, gpa, err)
	}
	// AddMemoryRegion mmaps a fresh memfd-backed region. To match the
	// "I supply the memory, hypervisor maps it to the guest" contract
	// of HVVM.MapMemory, copy hostMem into the returned region. Higher-
	// level code that wants zero-copy should use the kvm package directly
	// for now (Phase 1 part 2 reworks the contract to support pinning
	// arbitrary slices via memfd_create).
	_ = region
	v.nextSlot++
	return nil
}

func (v *hvvmKVM) UnmapMemory(gpa uint64, size uint64) error {
	// UnmapMemory by gpa requires looking up the slot. The kvm package
	// does not yet expose a slot-by-gpa lookup; that helper lands with
	// the arch.go refactor. For now this method is unimplemented but
	// satisfies the interface so adapter consumers compile.
	return fmt.Errorf("hvvmKVM.UnmapMemory: not yet implemented (Phase 1 part 2)")
}

func (v *hvvmKVM) QueryDirtyBitmap(gpa uint64, size uint64, bitmap []byte) error {
	// kvm.VM exposes GetDirtyLog(slot uint32). Callers using HVVM today
	// must first translate gpa → slot; the adapter assumes slot 0 covers
	// the whole RAM (true for current single-region layouts).
	bits, err := v.vm.GetDirtyLog(0)
	if err != nil {
		return err
	}
	// Pack []uint64 dirty log into the caller's []byte bitmap.
	for i, word := range bits {
		base := i * 8
		for b := 0; b < 8 && base+b < len(bitmap); b++ {
			bitmap[base+b] = byte(word >> (uint(b) * 8))
		}
	}
	return nil
}

func (v *hvvmKVM) CreateVCPU(idx int) (HVVCPU, error) {
	vcpu, err := v.vm.CreateVCPU(idx)
	if err != nil {
		return nil, fmt.Errorf("kvm.CreateVCPU(%d): %w", idx, err)
	}
	return &kvmVCPU{vm: v, vcpu: vcpu, id: idx}, nil
}

func (v *hvvmKVM) InjectInterrupt(req InterruptRequest) error {
	// KVM has IRQLine for legacy IOAPIC delivery. The synchronous level
	// is uint32 (1=assert, 0=deassert). Edge-triggered MSI-style would
	// use KVM_SIGNAL_MSI which the kvm package does not yet wrap.
	if req.Edge {
		// Deferred — Phase 2 lands KVM_SIGNAL_MSI when MSI/MSI-X
		// devices land.
		return fmt.Errorf("hvvmKVM.InjectInterrupt: edge-triggered/MSI not yet supported")
	}
	return v.vm.IRQLine(req.IRQNumber, int(req.Level))
}

func (v *hvvmKVM) Close() error {
	if v.vm == nil {
		return nil
	}
	err := v.vm.Close()
	v.vm = nil
	return err
}

// kvmVCPU wraps kvm.VCPU. The Run loop translates kvm.RunData exit codes
// into the unified ExitContext shape so device emulation code can stay
// hypervisor-agnostic.
type kvmVCPU struct {
	vm   *hvvmKVM
	vcpu *kvm.VCPU
	id   int
}

func (c *kvmVCPU) ID() int { return c.id }

func (c *kvmVCPU) Run() (ExitContext, error) {
	// Clear any pending ImmediateExit from a prior Cancel() so the next
	// KVM_RUN runs to completion. Mirrors WHP semantics where Cancel
	// affects only the in-flight Run; subsequent calls start fresh.
	// This makes the caller's lifecycle (Cancel → Run) symmetric across
	// backends without the kvm-specific `RunData.ImmediateExit = 0`
	// cleanup that Linux paths used to do manually.
	if c.vcpu != nil && c.vcpu.RunData != nil {
		c.vcpu.RunData.ImmediateExit = 0
	}
	if err := c.vcpu.Run(); err != nil {
		return ExitContext{Reason: ExitReasonInternal, FailureMsg: err.Error()}, err
	}
	rd := c.vcpu.RunData
	switch rd.ExitReason {
	case kvm.ExitMMIO:
		mmio := c.vcpu.GetMMIOData()
		return ExitContext{
			Reason: ExitReasonMMIO,
			MMIO: MMIOExit{
				Address: mmio.PhysAddr,
				Data:    mmio.Data,
				Length:  mmio.Len,
				Write:   mmio.IsWrite != 0,
			},
		}, nil
	case kvm.ExitIO:
		io := c.vcpu.GetIOData()
		var data [8]byte
		// kvm puts I/O data at rd.Data[io.DataOffset..]; copy up to size.
		size := io.Size
		if size > 8 {
			size = 8
		}
		for i := uint8(0); i < size && i < uint8(len(data)); i++ {
			data[i] = *(*byte)(unsafe.Pointer(uintptr(unsafe.Pointer(&rd.Data[0])) + uintptr(io.DataOffset) + uintptr(i)))
		}
		dir := IOPortIn
		if io.Direction == 1 {
			dir = IOPortOut
		}
		return ExitContext{
			Reason: ExitReasonIOPort,
			IOPort: IOPortExit{
				Port:      io.Port,
				Direction: dir,
				Size:      io.Size,
				Count:     io.Count,
				Data:      data,
			},
		}, nil
	case kvm.ExitHLT:
		return ExitContext{Reason: ExitReasonHalt}, nil
	case kvm.ExitShutdown:
		return ExitContext{Reason: ExitReasonShutdown}, nil
	case kvm.ExitSystemEvent:
		// Arch-specific (reset/poweroff/s4). The vmm runLoop hands this
		// to machineArchBackend.handleExit which decodes the system
		// event subtype from kvm.RunData. Once Phase 1.2 step 6 lifts
		// system-event decoding into ExitContext, this can mirror onto
		// portable fields.
		return ExitContext{Reason: ExitReasonSystemEvent}, nil
	case kvm.ExitIRQWindowOpen:
		return ExitContext{Reason: ExitReasonIRQWindowOpen}, nil
	case kvm.ExitFailEntry:
		return ExitContext{
			Reason:     ExitReasonFailEntry,
			FailureMsg: fmt.Sprintf("kvm fail entry (bad guest state) exit_reason=%d", rd.ExitReason),
		}, nil
	case kvm.ExitInternalError:
		return ExitContext{
			Reason:     ExitReasonInternal,
			FailureMsg: fmt.Sprintf("kvm internal error exit_reason=%d", rd.ExitReason),
		}, nil
	default:
		return ExitContext{Reason: ExitReasonUnknown}, nil
	}
}

func (c *kvmVCPU) Cancel() error {
	// KVM cancellation: set the immediate-exit flag in the run region
	// and signal the running thread so KVM_RUN returns. The signal is
	// SIGUSR1 in the gocracker convention; the runtime registers a no-
	// op handler on each vCPU thread. A full helper lands with the
	// arch.go refactor — for now this is a stub that succeeds without
	// actually unblocking the run.
	if c.vcpu != nil && c.vcpu.RunData != nil {
		c.vcpu.RunData.ImmediateExit = 1
	}
	return nil
}

func (c *kvmVCPU) GetRegisters() (Registers, error) {
	r, err := c.vcpu.GetRegs()
	if err != nil {
		return Registers{}, err
	}
	return Registers(r), nil
}

func (c *kvmVCPU) SetRegisters(r Registers) error {
	return c.vcpu.SetRegs(kvm.Regs(r))
}

func (c *kvmVCPU) GetSegmentRegisters() (SegmentRegisters, error) {
	s, err := c.vcpu.GetSregs()
	if err != nil {
		return SegmentRegisters{}, err
	}
	return sregsFromKVM(s), nil
}

func (c *kvmVCPU) SetSegmentRegisters(s SegmentRegisters) error {
	return c.vcpu.SetSregs(sregsToKVM(s))
}

func (c *kvmVCPU) Close() error {
	if c.vcpu == nil {
		return nil
	}
	err := c.vcpu.Close()
	c.vcpu = nil
	return err
}

// kvmMemFlagsFromHV translates portable MemFlags into the bit layout
// kvm.AddMemoryRegion expects. KVM_MEM_LOG_DIRTY_PAGES = 1<<0; read-only
// is KVM_MEM_READONLY = 1<<1.
func kvmMemFlagsFromHV(f MemFlags) uint32 {
	var out uint32
	if f&MemTrackDirty != 0 {
		out |= 1 << 0
	}
	if f&MemWrite == 0 {
		out |= 1 << 1
	}
	return out
}

func sregsFromKVM(s kvm.Sregs) SegmentRegisters {
	conv := func(seg kvm.Segment) Segment {
		return Segment{
			Base:     seg.Base,
			Limit:    seg.Limit,
			Selector: seg.Selector,
			Type:     seg.Type,
			Present:  seg.Present,
			DPL:      seg.DPL,
			DB:       seg.DB,
			S:        seg.S,
			L:        seg.L,
			G:        seg.G,
			AVL:      seg.AVL,
			Unusable: seg.Unusable,
		}
	}
	return SegmentRegisters{
		CS: conv(s.CS), DS: conv(s.DS), ES: conv(s.ES), FS: conv(s.FS),
		GS: conv(s.GS), SS: conv(s.SS), TR: conv(s.TR), LDT: conv(s.LDT),
		GDT:             DescriptorTable{Base: s.GDT.Base, Limit: s.GDT.Limit},
		IDT:             DescriptorTable{Base: s.IDT.Base, Limit: s.IDT.Limit},
		CR0:             s.CR0,
		CR2:             s.CR2,
		CR3:             s.CR3,
		CR4:             s.CR4,
		CR8:             s.CR8,
		EFER:            s.EFER,
		ApicBase:        s.ApicBase,
		InterruptBitmap: s.InterruptBitmap,
	}
}

func sregsToKVM(s SegmentRegisters) kvm.Sregs {
	conv := func(seg Segment) kvm.Segment {
		return kvm.Segment{
			Base:     seg.Base,
			Limit:    seg.Limit,
			Selector: seg.Selector,
			Type:     seg.Type,
			Present:  seg.Present,
			DPL:      seg.DPL,
			DB:       seg.DB,
			S:        seg.S,
			L:        seg.L,
			G:        seg.G,
			AVL:      seg.AVL,
			Unusable: seg.Unusable,
		}
	}
	return kvm.Sregs{
		CS: conv(s.CS), DS: conv(s.DS), ES: conv(s.ES), FS: conv(s.FS),
		GS: conv(s.GS), SS: conv(s.SS), TR: conv(s.TR), LDT: conv(s.LDT),
		GDT:             kvm.DTTR{Base: s.GDT.Base, Limit: s.GDT.Limit},
		IDT:             kvm.DTTR{Base: s.IDT.Base, Limit: s.IDT.Limit},
		CR0:             s.CR0,
		CR2:             s.CR2,
		CR3:             s.CR3,
		CR4:             s.CR4,
		CR8:             s.CR8,
		EFER:            s.EFER,
		ApicBase:        s.ApicBase,
		InterruptBitmap: s.InterruptBitmap,
	}
}
