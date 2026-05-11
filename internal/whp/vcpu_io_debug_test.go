//go:build windows

package whp

import (
	"encoding/hex"
	"testing"
)

// TestRunVCPUIoPortRawBytes runs a tiny real-mode program that does
// `out dx, al` to port 0x3F8 and dumps the entire WHV_RUN_VP_EXIT_CONTEXT
// buffer so we can validate the offsets against the SDK header
// empirically. Useful for debugging when the decoded fields look wrong.
func TestRunVCPUIoPortRawBytes(t *testing.T) {
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

	// mov dx, 0x3F8; mov al, 'H'; out dx, al; hlt
	program := []byte{0xBA, 0xF8, 0x03, 0xB0, 0x48, 0xEE, 0xF4}
	copy(mem.HostBytes(), program)

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

	// Configure real-mode CS at 0:0.
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

	ctx, err := RunVirtualProcessor(h, 0)
	if err != nil {
		t.Fatalf("RunVirtualProcessor: %v", err)
	}
	t.Logf("ExitReason=%#x (X64IoPortAccess=0x2 expected)", uint32(ctx.Reason))
	t.Logf("Decoded: InstrLen=%d Cr8=%d Rip=%#x Rflags=%#x", ctx.InstructionLength, ctx.Cr8, ctx.Rip, ctx.Rflags)
	t.Logf("CS: Base=%#x Limit=%#x Selector=%#x Attrs=%#x", ctx.Cs.Base, ctx.Cs.Limit, ctx.Cs.Selector, ctx.Cs.Attributes)
	t.Logf("raw bytes [0..32]:   %s", hex.EncodeToString(ctx.raw[0:32]))
	t.Logf("raw bytes [32..64]:  %s", hex.EncodeToString(ctx.raw[32:64]))
	t.Logf("raw bytes [64..96]:  %s", hex.EncodeToString(ctx.raw[64:96]))
	t.Logf("raw bytes [96..128]: %s", hex.EncodeToString(ctx.raw[96:128]))
	t.Logf("raw bytes [128..160]: %s", hex.EncodeToString(ctx.raw[128:160]))
	if ctx.Reason == ExitReasonX64IoPortAccess {
		io := ctx.IOPort()
		t.Logf("IOPort: InstrByteCount=%d AccessInfo=%#x Port=%#x Rax=%#x Rcx=%#x", io.InstructionByteCount, io.AccessInfo, io.Port, io.Rax, io.Rcx)
	}
}
