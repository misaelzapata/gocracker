//go:build windows

package vmm

import (
	"fmt"
	"sync"

	"github.com/gocracker/gocracker/internal/whp"
)

// NewWHPHypervisor returns a Hypervisor backed by the Windows Hypervisor
// Platform (WHP). It loads WinHvPlatform.dll dynamically; if the host
// doesn't expose the Hypervisor Platform feature (Win10 1803+ Pro/Server,
// Win11+), this returns an ErrUnsupportedHV with a clear message.
//
// Hard requirement on the host:
//   - WinHvPlatform.dll present (every Win10/11 SKU above Home ships it)
//   - HypervisorPlatform Windows feature enabled
//     (Enable-WindowsOptionalFeature -Online -FeatureName HypervisorPlatform -All)
func NewWHPHypervisor() (Hypervisor, error) {
	if !whp.Available() {
		return nil, ErrUnsupportedHV{Reason: "WinHvPlatform.dll not loadable"}
	}
	present, err := whp.HypervisorPresent()
	if err != nil {
		return nil, fmt.Errorf("whp.HypervisorPresent: %w", err)
	}
	if !present {
		return nil, ErrUnsupportedHV{Reason: "Hypervisor Platform feature not enabled; enable via 'Enable-WindowsOptionalFeature -Online -FeatureName HypervisorPlatform -All' (requires admin + reboot)"}
	}
	return &whpHypervisor{}, nil
}

// NewKVMHypervisor on Windows always errors — KVM is Linux-only.
// (The Windows-only build of hypervisor_windows.go satisfies the
// hypervisor.go declaration; without this, callers would have to
// build-tag every call to NewKVMHypervisor.)
func NewKVMHypervisor() (Hypervisor, error) {
	return nil, ErrUnsupportedHV{Reason: "KVM is Linux-only; use NewWHPHypervisor on Windows"}
}

type whpHypervisor struct{}

func (h *whpHypervisor) Capabilities() HVCapabilities {
	// WHP supports the full feature set we need. Values intentionally
	// mirror the KVM adapter's so feature-gated callers see one shape.
	return HVCapabilities{
		DirtyPageTracking: true,
		InKernelIRQChip:   true, // WHP exposes a synthetic interrupt chip
		PauseResume:       true,
		XSAVE:             true,
		MaxVCPUs:          1024,
	}
}

func (h *whpHypervisor) CreateVM(cfg HVVMConfig) (HVVM, error) {
	handle, err := whp.CreatePartition()
	if err != nil {
		return nil, fmt.Errorf("WHvCreatePartition: %w", err)
	}
	cleanup := func() { _ = whp.DeletePartition(handle) }
	if err := whp.SetPartitionPropertyU32(handle, whp.PropProcessorCount, uint32(cfg.NumVCPUs)); err != nil {
		cleanup()
		return nil, fmt.Errorf("WHvSetPartitionProperty(ProcessorCount=%d): %w", cfg.NumVCPUs, err)
	}
	if err := whp.SetupPartition(handle); err != nil {
		cleanup()
		return nil, fmt.Errorf("WHvSetupPartition: %w", err)
	}
	return &whpVM{
		handle:   handle,
		cfg:      cfg,
		vcpus:    make(map[int]*whpVCPU),
		mappings: make([]whpMapping, 0, 1),
	}, nil
}

func (h *whpHypervisor) Close() error { return nil }

// whpMapping tracks a mapped guest physical range so we can unmap it on
// Close. The host-side memory it points at is owned by guestMem (if
// allocated via AllocateGuestRAM) — releasing those is HV-VM-Close's job.
type whpMapping struct {
	gpa  uint64
	size uint64
}

// whpVM wraps a WHP partition handle plus the host-side memory regions
// we've allocated for guest RAM. The HVVM contract says AllocateGuestRAM
// returns a pinned slice; on Windows that means VirtualAlloc, tracked
// via guestMem so we can release on Close.
type whpVM struct {
	handle   whp.PartitionHandle
	cfg      HVVMConfig

	mu       sync.Mutex
	vcpus    map[int]*whpVCPU
	guestMem []*whp.GuestMemory // host-side allocations we own
	mappings []whpMapping       // gpa+size pairs to unmap on Close
}

func (v *whpVM) AllocateGuestRAM(size uint64) ([]byte, error) {
	gm, err := whp.AllocateGuestMemory(size)
	if err != nil {
		return nil, fmt.Errorf("whp.AllocateGuestMemory(%d): %w", size, err)
	}
	v.mu.Lock()
	v.guestMem = append(v.guestMem, gm)
	v.mu.Unlock()
	return gm.HostBytes(), nil
}

func (v *whpVM) MapMemory(gpa uint64, hostMem []byte, flags MemFlags) error {
	whpFlags := whpMemFlagsFromHV(flags)
	if err := whp.MapGpaRange(v.handle, hostMem, gpa, whpFlags); err != nil {
		return fmt.Errorf("WHvMapGpaRange(gpa=%#x, size=%d): %w", gpa, len(hostMem), err)
	}
	v.mu.Lock()
	v.mappings = append(v.mappings, whpMapping{gpa: gpa, size: uint64(len(hostMem))})
	v.mu.Unlock()
	return nil
}

func (v *whpVM) UnmapMemory(gpa uint64, size uint64) error {
	if err := whp.UnmapGpaRange(v.handle, gpa, size); err != nil {
		return fmt.Errorf("WHvUnmapGpaRange(gpa=%#x, size=%d): %w", gpa, size, err)
	}
	v.mu.Lock()
	for i, m := range v.mappings {
		if m.gpa == gpa && m.size == size {
			v.mappings = append(v.mappings[:i], v.mappings[i+1:]...)
			break
		}
	}
	v.mu.Unlock()
	return nil
}

func (v *whpVM) QueryDirtyBitmap(gpa uint64, size uint64, bitmap []byte) error {
	// WHvQueryGpaRangeDirtyBitmap fills a UINT64 array. Phase 1.2 part 2
	// will wire this through; for now it's the placeholder the contract
	// requires. Returning a not-implemented error rather than silently
	// returning empty so callers know not to depend on it yet.
	return fmt.Errorf("whpVM.QueryDirtyBitmap: not implemented yet (Phase 1.2 part 2)")
}

func (v *whpVM) CreateVCPU(idx int) (HVVCPU, error) {
	if err := whp.CreateVirtualProcessor(v.handle, uint32(idx)); err != nil {
		return nil, fmt.Errorf("WHvCreateVirtualProcessor(%d): %w", idx, err)
	}
	c := &whpVCPU{vm: v, idx: idx}
	v.mu.Lock()
	v.vcpus[idx] = c
	v.mu.Unlock()
	return c, nil
}

func (v *whpVM) InjectInterrupt(req InterruptRequest) error {
	// WHvRequestInterrupt takes a WHV_INTERRUPT_CONTROL struct. Phase 1.2
	// part 2 wires this in once virtio devices flow IRQs through HVVM.
	return fmt.Errorf("whpVM.InjectInterrupt: not implemented yet (Phase 1.2 part 2)")
}

func (v *whpVM) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, c := range v.vcpus {
		_ = whp.DeleteVirtualProcessor(v.handle, uint32(c.idx))
	}
	v.vcpus = nil
	for _, m := range v.mappings {
		_ = whp.UnmapGpaRange(v.handle, m.gpa, m.size)
	}
	v.mappings = nil
	for _, gm := range v.guestMem {
		_ = gm.Close()
	}
	v.guestMem = nil
	if v.handle != 0 {
		err := whp.DeletePartition(v.handle)
		v.handle = 0
		return err
	}
	return nil
}

// whpVCPU wraps a WHP virtual processor by index. Lifetime is tied to
// the parent whpVM; once the partition is deleted, every vCPU is invalid.
type whpVCPU struct {
	vm  *whpVM
	idx int
}

func (c *whpVCPU) ID() int { return c.idx }

func (c *whpVCPU) Run() (ExitContext, error) {
	rawCtx, err := whp.RunVirtualProcessor(c.vm.handle, uint32(c.idx))
	if err != nil {
		return ExitContext{Reason: ExitReasonInternal, FailureMsg: err.Error()}, err
	}
	return whpExitContextToPortable(rawCtx), nil
}

func (c *whpVCPU) Cancel() error {
	return whp.CancelRunVirtualProcessor(c.vm.handle, uint32(c.idx))
}

func (c *whpVCPU) GetRegisters() (Registers, error) {
	values := make([]whp.RegisterValue, len(gprRegisterNames))
	if err := whp.GetVCPURegisters(c.vm.handle, uint32(c.idx), gprRegisterNames, values); err != nil {
		return Registers{}, err
	}
	return Registers{
		RAX: values[0].Uint64(), RCX: values[1].Uint64(), RDX: values[2].Uint64(), RBX: values[3].Uint64(),
		RSP: values[4].Uint64(), RBP: values[5].Uint64(), RSI: values[6].Uint64(), RDI: values[7].Uint64(),
		R8: values[8].Uint64(), R9: values[9].Uint64(), R10: values[10].Uint64(), R11: values[11].Uint64(),
		R12: values[12].Uint64(), R13: values[13].Uint64(), R14: values[14].Uint64(), R15: values[15].Uint64(),
		RIP: values[16].Uint64(), RFLAGS: values[17].Uint64(),
	}, nil
}

func (c *whpVCPU) SetRegisters(r Registers) error {
	values := make([]whp.RegisterValue, len(gprRegisterNames))
	values[0].SetUint64(r.RAX)
	values[1].SetUint64(r.RCX)
	values[2].SetUint64(r.RDX)
	values[3].SetUint64(r.RBX)
	values[4].SetUint64(r.RSP)
	values[5].SetUint64(r.RBP)
	values[6].SetUint64(r.RSI)
	values[7].SetUint64(r.RDI)
	values[8].SetUint64(r.R8)
	values[9].SetUint64(r.R9)
	values[10].SetUint64(r.R10)
	values[11].SetUint64(r.R11)
	values[12].SetUint64(r.R12)
	values[13].SetUint64(r.R13)
	values[14].SetUint64(r.R14)
	values[15].SetUint64(r.R15)
	values[16].SetUint64(r.RIP)
	values[17].SetUint64(r.RFLAGS)
	return whp.SetVCPURegisters(c.vm.handle, uint32(c.idx), gprRegisterNames, values)
}

func (c *whpVCPU) GetSegmentRegisters() (SegmentRegisters, error) {
	values := make([]whp.RegisterValue, len(sregsRegisterNames))
	if err := whp.GetVCPURegisters(c.vm.handle, uint32(c.idx), sregsRegisterNames, values); err != nil {
		return SegmentRegisters{}, err
	}
	cs := whpSegmentValueToPortable(values[0].Segment())
	ds := whpSegmentValueToPortable(values[1].Segment())
	es := whpSegmentValueToPortable(values[2].Segment())
	fs := whpSegmentValueToPortable(values[3].Segment())
	gs := whpSegmentValueToPortable(values[4].Segment())
	ss := whpSegmentValueToPortable(values[5].Segment())
	tr := whpSegmentValueToPortable(values[6].Segment())
	ldt := whpSegmentValueToPortable(values[7].Segment())
	gdt := values[8].Table()
	idt := values[9].Table()
	return SegmentRegisters{
		CS:       cs,
		DS:       ds,
		ES:       es,
		FS:       fs,
		GS:       gs,
		SS:       ss,
		TR:       tr,
		LDT:      ldt,
		GDT:      DescriptorTable{Base: gdt.Base, Limit: gdt.Limit},
		IDT:      DescriptorTable{Base: idt.Base, Limit: idt.Limit},
		CR0:      values[10].Uint64(),
		CR2:      values[11].Uint64(),
		CR3:      values[12].Uint64(),
		CR4:      values[13].Uint64(),
		CR8:      values[14].Uint64(),
		EFER:     values[15].Uint64(),
		ApicBase: values[16].Uint64(),
	}, nil
}

func (c *whpVCPU) SetSegmentRegisters(s SegmentRegisters) error {
	values := make([]whp.RegisterValue, len(sregsRegisterNames))
	values[0].SetSegment(whpSegmentValueFromPortable(s.CS))
	values[1].SetSegment(whpSegmentValueFromPortable(s.DS))
	values[2].SetSegment(whpSegmentValueFromPortable(s.ES))
	values[3].SetSegment(whpSegmentValueFromPortable(s.FS))
	values[4].SetSegment(whpSegmentValueFromPortable(s.GS))
	values[5].SetSegment(whpSegmentValueFromPortable(s.SS))
	values[6].SetSegment(whpSegmentValueFromPortable(s.TR))
	values[7].SetSegment(whpSegmentValueFromPortable(s.LDT))
	values[8].SetTable(whp.TableValue{Base: s.GDT.Base, Limit: s.GDT.Limit})
	values[9].SetTable(whp.TableValue{Base: s.IDT.Base, Limit: s.IDT.Limit})
	values[10].SetUint64(s.CR0)
	values[11].SetUint64(s.CR2)
	values[12].SetUint64(s.CR3)
	values[13].SetUint64(s.CR4)
	values[14].SetUint64(s.CR8)
	values[15].SetUint64(s.EFER)
	values[16].SetUint64(s.ApicBase)
	return whp.SetVCPURegisters(c.vm.handle, uint32(c.idx), sregsRegisterNames, values)
}

func (c *whpVCPU) Close() error {
	c.vm.mu.Lock()
	delete(c.vm.vcpus, c.idx)
	c.vm.mu.Unlock()
	return whp.DeleteVirtualProcessor(c.vm.handle, uint32(c.idx))
}

// gprRegisterNames is the ordered set of WHP register names that
// corresponds 1:1 with the Registers struct fields.
var gprRegisterNames = []whp.RegisterName{
	whp.RegRax, whp.RegRcx, whp.RegRdx, whp.RegRbx,
	whp.RegRsp, whp.RegRbp, whp.RegRsi, whp.RegRdi,
	whp.RegR8, whp.RegR9, whp.RegR10, whp.RegR11,
	whp.RegR12, whp.RegR13, whp.RegR14, whp.RegR15,
	whp.RegRip, whp.RegRflags,
}

// sregsRegisterNames is the ordered set that corresponds to
// SegmentRegisters: 8 segment regs, 2 table regs, 5 control regs, EFER,
// ApicBase. Order MUST match the Get/SetSegmentRegisters indexing above.
var sregsRegisterNames = []whp.RegisterName{
	whp.RegCs, whp.RegDs, whp.RegEs, whp.RegFs, whp.RegGs, whp.RegSs,
	whp.RegTr, whp.RegLdtr,
	whp.RegGdtr, whp.RegIdtr,
	whp.RegCr0, whp.RegCr2, whp.RegCr3, whp.RegCr4, whp.RegCr8,
	whp.RegEfer, whp.RegApicBase,
}

// whpMemFlagsFromHV translates portable MemFlags into the WHP
// MapGpaRangeFlags bit layout.
func whpMemFlagsFromHV(f MemFlags) whp.MapGpaRangeFlags {
	var out whp.MapGpaRangeFlags
	if f&MemRead != 0 {
		out |= whp.MapGpaRead
	}
	if f&MemWrite != 0 {
		out |= whp.MapGpaWrite
	}
	if f&MemExecute != 0 {
		out |= whp.MapGpaExecute
	}
	if f&MemTrackDirty != 0 {
		out |= whp.MapGpaTrack
	}
	return out
}

// whpSegmentValueToPortable converts a WHP segment value into the
// pkg/vmm.Segment shape (with attribute bits unpacked).
func whpSegmentValueToPortable(s whp.SegmentValue) Segment {
	a := whp.UnpackSegmentAttrs(s.Attributes)
	return Segment{
		Base:     s.Base,
		Limit:    s.Limit,
		Selector: s.Selector,
		Type:     a.Type,
		Present:  a.Present,
		DPL:      a.DPL,
		DB:       a.DB,
		S:        a.S,
		L:        a.L,
		G:        a.G,
		AVL:      a.AVL,
	}
}

// whpSegmentValueFromPortable inverts the above.
func whpSegmentValueFromPortable(s Segment) whp.SegmentValue {
	attrs := whp.SegmentAttrs{
		Type:    s.Type,
		S:       s.S,
		DPL:     s.DPL,
		Present: s.Present,
		AVL:     s.AVL,
		L:       s.L,
		DB:      s.DB,
		G:       s.G,
	}.Pack()
	return whp.SegmentValue{
		Base:       s.Base,
		Limit:      s.Limit,
		Selector:   s.Selector,
		Attributes: attrs,
	}
}

// whpExitContextToPortable maps the WHP-decoded exit into the portable
// ExitContext shape from pkg/vmm/hypervisor.go.
func whpExitContextToPortable(c whp.ExitContext) ExitContext {
	out := ExitContext{
		RIP:               c.Rip,
		InstructionLength: c.InstructionLength,
	}
	switch c.Reason {
	case whp.ExitReasonMemoryAccess:
		m := c.MMIO()
		out.Reason = ExitReasonMMIO
		out.MMIO.Address = m.Gpa
		out.MMIO.Length = uint32(m.InstructionByteCount)
		out.MMIO.Write = m.IsWrite()
	case whp.ExitReasonX64IoPortAccess:
		io := c.IOPort()
		out.Reason = ExitReasonIOPort
		out.IOPort.Port = io.Port
		out.IOPort.Size = io.AccessSize()
		if io.IsWrite() {
			out.IOPort.Direction = IOPortOut
		} else {
			out.IOPort.Direction = IOPortIn
		}
		out.IOPort.Count = 1
	case whp.ExitReasonX64Halt:
		out.Reason = ExitReasonHalt
	case whp.ExitReasonX64InterruptWindow:
		out.Reason = ExitReasonIRQWindowOpen
	case whp.ExitReasonX64Cpuid:
		ci := c.CPUID()
		out.Reason = ExitReasonCPUID
		out.CPUID.Function = uint32(ci.Rax)
		out.CPUID.Subfunction = uint32(ci.Rcx)
		out.CPUID.OutEAX = uint32(ci.DefaultResultRax)
		out.CPUID.OutEBX = uint32(ci.DefaultResultRbx)
		out.CPUID.OutECX = uint32(ci.DefaultResultRcx)
		out.CPUID.OutEDX = uint32(ci.DefaultResultRdx)
	case whp.ExitReasonCanceled:
		out.Reason = ExitReasonCancelled
	case whp.ExitReasonUnrecoverableException,
		whp.ExitReasonInvalidVpRegisterValue,
		whp.ExitReasonUnsupportedFeature:
		out.Reason = ExitReasonInternal
		out.FailureMsg = fmt.Sprintf("WHP exit reason %#x", uint32(c.Reason))
	default:
		out.Reason = ExitReasonUnknown
		out.FailureMsg = fmt.Sprintf("unhandled WHP exit reason %#x", uint32(c.Reason))
	}
	return out
}
