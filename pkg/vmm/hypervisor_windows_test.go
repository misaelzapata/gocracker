//go:build windows

package vmm

import (
	"testing"
	"time"

	"github.com/gocracker/gocracker/internal/whp"
)

// TestWHPHypervisorEndToEnd is the integration smoke for the WHP
// adapter: open a Hypervisor via NewWHPHypervisor, create a 1-vCPU VM
// with 4 MiB of guest RAM, write a HLT (0xF4) into RAM, configure the
// vCPU at CS:RIP=0:0 in real mode, run, and assert ExitReasonHalt.
//
// Validates every layer of the abstraction in one go:
//   NewWHPHypervisor → CreatePartition + SetupPartition
//   HVVM.AllocateGuestRAM → VirtualAlloc + tracked
//   HVVM.MapMemory → WHvMapGpaRange
//   HVVM.CreateVCPU → WHvCreateVirtualProcessor
//   HVVCPU.SetSegmentRegisters + SetRegisters → WHvSetVirtualProcessorRegisters
//   HVVCPU.Run → WHvRunVirtualProcessor + exit-context translation
//   HVVCPU.Close + HVVM.Close → DeleteVirtualProcessor + UnmapGpaRange + DeletePartition + VirtualFree
func TestWHPHypervisorEndToEnd(t *testing.T) {
	if !whp.Available() {
		t.Skip("WinHvPlatform.dll not loadable")
	}
	present, err := whp.HypervisorPresent()
	if err != nil || !present {
		t.Skip("Hypervisor Platform feature not enabled on this host")
	}

	hv, err := NewWHPHypervisor()
	if err != nil {
		t.Fatalf("NewWHPHypervisor: %v", err)
	}
	t.Cleanup(func() { _ = hv.Close() })

	caps := hv.Capabilities()
	if !caps.PauseResume || !caps.InKernelIRQChip {
		t.Errorf("Capabilities() missing expected flags: %+v", caps)
	}

	vm, err := hv.CreateVM(HVVMConfig{NumVCPUs: 1, MemoryBytes: 4 * 1024 * 1024})
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	t.Cleanup(func() { _ = vm.Close() })

	ram, err := vm.AllocateGuestRAM(4 * 1024 * 1024)
	if err != nil {
		t.Fatalf("AllocateGuestRAM: %v", err)
	}
	if len(ram) != 4*1024*1024 {
		t.Fatalf("AllocateGuestRAM returned %d bytes, want %d", len(ram), 4*1024*1024)
	}
	// HLT opcode at GPA 0. vCPU will execute this on first fetch.
	ram[0] = 0xF4

	if err := vm.MapMemory(0, ram, MemRWX); err != nil {
		t.Fatalf("MapMemory: %v", err)
	}

	vcpu, err := vm.CreateVCPU(0)
	if err != nil {
		t.Fatalf("CreateVCPU(0): %v", err)
	}
	t.Cleanup(func() { _ = vcpu.Close() })
	if vcpu.ID() != 0 {
		t.Errorf("ID() = %d, want 0", vcpu.ID())
	}

	// Real-mode boot: CS selector=0/base=0/limit=0xFFFF, 16-bit code
	// segment (Type=10, S=1, Present=1, no L/G).
	sregs, err := vcpu.GetSegmentRegisters()
	if err != nil {
		t.Fatalf("GetSegmentRegisters: %v", err)
	}
	sregs.CS = Segment{Limit: 0xFFFF, Type: 0xA, S: 1, Present: 1}
	if err := vcpu.SetSegmentRegisters(sregs); err != nil {
		t.Fatalf("SetSegmentRegisters: %v", err)
	}
	regs, err := vcpu.GetRegisters()
	if err != nil {
		t.Fatalf("GetRegisters: %v", err)
	}
	regs.RIP = 0
	regs.RFLAGS = 0x2 // reserved bit 1 always set
	if err := vcpu.SetRegisters(regs); err != nil {
		t.Fatalf("SetRegisters: %v", err)
	}

	type runResult struct {
		ctx ExitContext
		err error
	}
	resultCh := make(chan runResult, 1)
	go func() {
		ctx, err := vcpu.Run()
		resultCh <- runResult{ctx: ctx, err: err}
	}()
	select {
	case res := <-resultCh:
		if res.err != nil {
			t.Fatalf("vcpu.Run: %v", res.err)
		}
		if res.ctx.Reason != ExitReasonHalt {
			t.Fatalf("expected ExitReasonHalt, got %v (FailureMsg=%q)",
				res.ctx.Reason, res.ctx.FailureMsg)
		}
		t.Logf("WHP HVVCPU.Run returned ExitReasonHalt after executing the HLT — full abstraction chain works")
	case <-time.After(2 * time.Second):
		_ = vcpu.Cancel()
		t.Fatal("vcpu.Run did not return within 2s")
	}

	// Sanity: round-trip a GPR through the abstraction.
	regs.RAX = 0xCAFEF00D
	if err := vcpu.SetRegisters(regs); err != nil {
		t.Fatalf("SetRegisters (post-run): %v", err)
	}
	got, err := vcpu.GetRegisters()
	if err != nil {
		t.Fatalf("GetRegisters (post-run): %v", err)
	}
	if got.RAX != 0xCAFEF00D {
		t.Errorf("RAX round-trip through abstraction: got %#x want 0xCAFEF00D", got.RAX)
	}
}
