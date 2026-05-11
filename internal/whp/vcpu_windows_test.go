//go:build windows

package whp

import (
	"sync"
	"testing"
	"time"
)

// TestRunVCPUHaltsImmediately is the end-to-end smoke for the run loop:
// write a single HLT instruction (0xF4) into guest RAM at GPA 0,
// configure the vCPU to start executing at CS:RIP = 0:0 in real mode,
// then call WHvRunVirtualProcessor and assert it exits with X64Halt.
//
// This is the moment of truth — if WHvRunVirtualProcessor returns and
// reports X64Halt, the entire pipeline works:
//   - memory allocation + mapping
//   - register configuration via WHvSetVirtualProcessorRegisters
//   - the run loop primitive
//   - exit context decoding
//
// All four are exercised in a single test.
func TestRunVCPUHaltsImmediately(t *testing.T) {
	if !Available() {
		t.Skip("WinHvPlatform.dll not loadable")
	}
	present, err := HypervisorPresent()
	if err != nil || !present {
		t.Skip("Hypervisor Platform feature not enabled")
	}

	// 4 MiB guest RAM at GPA 0.
	mem, err := AllocateGuestMemory(4 * 1024 * 1024)
	if err != nil {
		t.Fatalf("AllocateGuestMemory: %v", err)
	}
	t.Cleanup(func() { mem.Close() })

	// Write HLT (opcode 0xF4) at GPA 0. The vCPU will reach this on
	// its first instruction fetch from real-mode CS:IP=0:0.
	mem.HostBytes()[0] = 0xF4

	h, err := CreatePartition()
	if err != nil {
		t.Fatalf("CreatePartition: %v", err)
	}
	t.Cleanup(func() { DeletePartition(h) })

	if err := SetPartitionPropertyU32(h, PropProcessorCount, 1); err != nil {
		t.Fatalf("SetPartitionProperty(ProcessorCount=1): %v", err)
	}
	if err := SetupPartition(h); err != nil {
		t.Fatalf("SetupPartition: %v", err)
	}
	if err := MapGuestMemory(h, mem, 0); err != nil {
		t.Fatalf("MapGuestMemory: %v", err)
	}
	if err := CreateVirtualProcessor(h, 0); err != nil {
		t.Fatalf("CreateVirtualProcessor(0): %v", err)
	}
	t.Cleanup(func() { DeleteVirtualProcessor(h, 0) })

	// Configure real-mode boot. After WHvCreateVirtualProcessor the
	// vCPU's CS/RIP follow the x86 reset vector convention (CS base
	// 0xFFFF0000, RIP=0xFFF0). Override to start at 0:0 so we execute
	// the HLT we just wrote.
	names := []RegisterName{RegCs, RegRip, RegRflags}
	values := make([]RegisterValue, 3)
	// Real-mode CS: selector=0, base=0, limit=0xFFFF, attributes for a
	// 16-bit code segment with Type=10 (read+exec), S=1, P=1, no L/G.
	values[0].SetSegment(SegmentValue{
		Base:       0,
		Limit:      0xFFFF,
		Selector:   0,
		Attributes: SegmentAttrs{Type: 0xA, S: 1, Present: 1}.Pack(),
	})
	values[1].SetUint64(0)        // RIP = 0
	values[2].SetUint64(0x000002) // RFLAGS reserved bit 1 set
	if err := SetVCPURegisters(h, 0, names, values); err != nil {
		t.Fatalf("SetVCPURegisters(CS/RIP/RFLAGS): %v", err)
	}

	// Run with a watchdog so an infinite loop in the vCPU never blocks
	// the test runner.
	type runResult struct {
		ctx ExitContext
		err error
	}
	resultCh := make(chan runResult, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, err := RunVirtualProcessor(h, 0)
		resultCh <- runResult{ctx: ctx, err: err}
	}()

	select {
	case res := <-resultCh:
		if res.err != nil {
			t.Fatalf("RunVirtualProcessor: %v", res.err)
		}
		if res.ctx.Reason != ExitReasonX64Halt {
			t.Fatalf("unexpected exit reason: got %#x (%d) want %#x (X64Halt). RIP=%#x",
				uint32(res.ctx.Reason), uint32(res.ctx.Reason),
				uint32(ExitReasonX64Halt), res.ctx.Rip)
		}
		t.Logf("vCPU executed HLT and exited as X64Halt at RIP=%#x (instr len %d)",
			res.ctx.Rip, res.ctx.InstructionLength)
	case <-time.After(2 * time.Second):
		// Watchdog: cancel the vCPU so the goroutine returns even on
		// hang, then fail the test.
		_ = CancelRunVirtualProcessor(h, 0)
		wg.Wait()
		t.Fatal("RunVirtualProcessor did not return within 2s (watchdog tripped)")
	}
}

// TestRunVCPUCancellation verifies the cancellation primitive: start a
// vCPU at a NOP loop (0xEB 0xFE = jmp $-2 = infinite loop), then call
// CancelRunVirtualProcessor from another goroutine and assert the
// running vCPU returns with ExitReasonCanceled.
func TestRunVCPUCancellation(t *testing.T) {
	if !Available() {
		t.Skip("WinHvPlatform.dll not loadable")
	}
	present, err := HypervisorPresent()
	if err != nil || !present {
		t.Skip("Hypervisor Platform feature not enabled")
	}

	mem, err := AllocateGuestMemory(4 * 1024 * 1024)
	if err != nil {
		t.Fatalf("AllocateGuestMemory: %v", err)
	}
	t.Cleanup(func() { mem.Close() })

	// 0xEB 0xFE — short jmp $-2 — infinite tight loop. Once the vCPU
	// reaches this it never voluntarily exits.
	mem.HostBytes()[0] = 0xEB
	mem.HostBytes()[1] = 0xFE

	h, err := CreatePartition()
	if err != nil {
		t.Fatalf("CreatePartition: %v", err)
	}
	t.Cleanup(func() { DeletePartition(h) })
	if err := SetPartitionPropertyU32(h, PropProcessorCount, 1); err != nil {
		t.Fatalf("SetPartitionProperty: %v", err)
	}
	if err := SetupPartition(h); err != nil {
		t.Fatalf("SetupPartition: %v", err)
	}
	if err := MapGuestMemory(h, mem, 0); err != nil {
		t.Fatalf("MapGuestMemory: %v", err)
	}
	if err := CreateVirtualProcessor(h, 0); err != nil {
		t.Fatalf("CreateVirtualProcessor: %v", err)
	}
	t.Cleanup(func() { DeleteVirtualProcessor(h, 0) })

	names := []RegisterName{RegCs, RegRip, RegRflags}
	values := make([]RegisterValue, 3)
	values[0].SetSegment(SegmentValue{
		Limit:      0xFFFF,
		Attributes: SegmentAttrs{Type: 0xA, S: 1, Present: 1}.Pack(),
	})
	values[1].SetUint64(0)
	values[2].SetUint64(0x2)
	if err := SetVCPURegisters(h, 0, names, values); err != nil {
		t.Fatalf("SetVCPURegisters: %v", err)
	}

	done := make(chan ExitContext, 1)
	go func() {
		ctx, _ := RunVirtualProcessor(h, 0)
		done <- ctx
	}()

	// Let the vCPU spin for a bit, then cancel.
	time.Sleep(50 * time.Millisecond)
	if err := CancelRunVirtualProcessor(h, 0); err != nil {
		t.Fatalf("CancelRunVirtualProcessor: %v", err)
	}

	select {
	case ctx := <-done:
		if ctx.Reason != ExitReasonCanceled {
			t.Fatalf("expected ExitReasonCanceled, got %#x at RIP=%#x",
				uint32(ctx.Reason), ctx.Rip)
		}
		t.Logf("vCPU cancelled while spinning at RIP=%#x", ctx.Rip)
	case <-time.After(2 * time.Second):
		t.Fatal("vCPU did not respond to cancel within 2s")
	}
}
