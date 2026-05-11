//go:build windows

package whp

import "testing"

// TestRegisterValueUint64 pins the scalar register encoding: bytes 0..7
// hold the value little-endian; bytes 8..15 are zero.
func TestRegisterValueUint64(t *testing.T) {
	var v RegisterValue
	v.SetUint64(0xDEADBEEFCAFEBABE)
	if got := v.Uint64(); got != 0xDEADBEEFCAFEBABE {
		t.Fatalf("Uint64 round-trip: got %#x want %#x", got, uint64(0xDEADBEEFCAFEBABE))
	}
	// Byte order check (LE).
	if v[0] != 0xBE || v[7] != 0xDE {
		t.Errorf("byte order wrong: v[0]=%#x v[7]=%#x", v[0], v[7])
	}
	for i := 8; i < 16; i++ {
		if v[i] != 0 {
			t.Errorf("v[%d]=%#x; want 0 (upper half must be zero)", i, v[i])
		}
	}
}

// TestRegisterValueSegment locks the WHV_X64_SEGMENT_REGISTER layout
// (Base[0..7], Limit[8..11], Selector[12..13], Attributes[14..15]).
// A subtle wrong offset would surface as a guest #GP at boot.
func TestRegisterValueSegment(t *testing.T) {
	want := SegmentValue{
		Base:       0x1234_5678_9abc_def0,
		Limit:      0xFFFFFFFF,
		Selector:   0x0008,
		Attributes: 0xA09B,
	}
	var v RegisterValue
	v.SetSegment(want)
	got := v.Segment()
	if got != want {
		t.Fatalf("segment round-trip: got %+v want %+v", got, want)
	}
	// Spot-check byte positions.
	if v[0] != 0xF0 || v[7] != 0x12 {
		t.Errorf("base byte order: v[0]=%#x v[7]=%#x", v[0], v[7])
	}
	if v[8] != 0xFF || v[11] != 0xFF {
		t.Errorf("limit byte order: v[8]=%#x v[11]=%#x", v[8], v[11])
	}
	if v[12] != 0x08 || v[13] != 0x00 {
		t.Errorf("selector byte order: v[12]=%#x v[13]=%#x", v[12], v[13])
	}
	if v[14] != 0x9B || v[15] != 0xA0 {
		t.Errorf("attributes byte order: v[14]=%#x v[15]=%#x", v[14], v[15])
	}
}

// TestRegisterValueTable: WHV_X64_TABLE_REGISTER is
// Pad[3](uint16)@[0..5], Limit@[6..7], Base@[8..15]. The padding MUST
// be zero or some kernels reject the SetVCPURegisters call.
func TestRegisterValueTable(t *testing.T) {
	want := TableValue{
		Base:  0x0000_0000_0000_0500,
		Limit: 31,
	}
	var v RegisterValue
	v.SetTable(want)
	for i := 0; i < 6; i++ {
		if v[i] != 0 {
			t.Errorf("table pad byte v[%d]=%#x; must be 0", i, v[i])
		}
	}
	if got := v.Table(); got != want {
		t.Fatalf("table round-trip: got %+v want %+v", got, want)
	}
}

// TestSegmentAttrsPack pins the bit layout — match the canonical x86
// descriptor encoding. The "LongCodeSegment" example from node-vmm
// expands to Attributes = 0xA09B; we reproduce it here.
func TestSegmentAttrsPack(t *testing.T) {
	cases := []struct {
		name string
		in   SegmentAttrs
		want uint16
	}{
		{
			name: "long-mode code (CS)",
			in:   SegmentAttrs{Type: 0xB, S: 1, DPL: 0, Present: 1, L: 1, G: 1},
			want: 0xA09B,
		},
		{
			name: "long-mode data (DS/ES/SS)",
			in:   SegmentAttrs{Type: 0x3, S: 1, DPL: 0, Present: 1, DB: 1, G: 1},
			want: 0xC093,
		},
		{
			name: "TSS (64-bit)",
			in:   SegmentAttrs{Type: 0xB, Present: 1, G: 1},
			want: 0x808B,
		},
		{
			name: "zero",
			in:   SegmentAttrs{},
			want: 0,
		},
	}
	for _, c := range cases {
		got := c.in.Pack()
		if got != c.want {
			t.Errorf("%s: Pack() = %#x; want %#x", c.name, got, c.want)
		}
		// Round-trip.
		if back := UnpackSegmentAttrs(got); back != c.in {
			t.Errorf("%s: Unpack(Pack(...)) drift: got %+v want %+v", c.name, back, c.in)
		}
	}
}

// TestVCPURegistersOnLivePartition exercises the actual
// Get/SetVirtualProcessorRegisters calls against a real vCPU. Skips
// when WHP isn't enabled. Creates a partition, configures it, creates
// a vCPU, writes RAX=0xDEADBEEFCAFEBABE, reads it back, asserts match.
//
// This is the real boundary between "I encode registers correctly" and
// "the hypervisor agrees with my encoding".
func TestVCPURegistersOnLivePartition(t *testing.T) {
	if !Available() {
		t.Skip("WinHvPlatform.dll not loadable")
	}
	present, err := HypervisorPresent()
	if err != nil || !present {
		t.Skip("Hypervisor Platform feature not enabled")
	}

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
	if err := CreateVirtualProcessor(h, 0); err != nil {
		t.Fatalf("CreateVirtualProcessor(0): %v", err)
	}
	t.Cleanup(func() { DeleteVirtualProcessor(h, 0) })

	const want = uint64(0xDEADBEEFCAFEBABE)
	names := []RegisterName{RegRax}
	values := make([]RegisterValue, 1)
	values[0].SetUint64(want)
	if err := SetVCPURegisters(h, 0, names, values); err != nil {
		t.Fatalf("SetVCPURegisters(RAX=%#x): %v", want, err)
	}
	// Read back into a fresh buffer to make sure the kernel actually
	// reflects what we wrote.
	readBack := make([]RegisterValue, 1)
	if err := GetVCPURegisters(h, 0, names, readBack); err != nil {
		t.Fatalf("GetVCPURegisters: %v", err)
	}
	if got := readBack[0].Uint64(); got != want {
		t.Fatalf("RAX round-trip through WHP: got %#x want %#x", got, want)
	}
	t.Logf("vCPU 0 RAX round-trip succeeded (%#x) via WHvSet/GetVirtualProcessorRegisters", want)
}
