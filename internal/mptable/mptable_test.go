package mptable

import (
	"encoding/binary"
	"testing"
)

func TestStartAddrAlignedTo16Bytes(t *testing.T) {
	for _, cpus := range []int{1, 2, 4, 8, 16} {
		if got := StartAddr(cpus) % 16; got != 0 {
			t.Fatalf("StartAddr(%d) alignment = %d, want 0", cpus, got)
		}
	}
}

func TestSizeGrowsWithCPUs(t *testing.T) {
	prev := Size(1)
	for _, cpus := range []int{2, 4, 8} {
		s := Size(cpus)
		if s <= prev {
			t.Errorf("Size(%d) = %d, expected > %d", cpus, s, prev)
		}
		prev = s
	}
}

func TestSizeDiffPerCPU(t *testing.T) {
	// Each additional CPU adds a 20-byte processor entry.
	for cpus := 1; cpus < 8; cpus++ {
		diff := Size(cpus+1) - Size(cpus)
		if diff != 20 {
			t.Errorf("Size(%d)-Size(%d) = %d, want 20", cpus+1, cpus, diff)
		}
	}
}

func TestWriteErrorCases(t *testing.T) {
	tests := []struct {
		name    string
		numCPUs int
		memSize int
		wantErr bool
	}{
		{"zero cpus", 0, 1 << 20, true},
		{"negative cpus", -1, 1 << 20, true},
		{"exceeds max cpus", 255, 1 << 20, true},
		{"max cpus ok", 254, 1 << 20, false},
		{"memory too small", 1, 100, true},
		{"1 cpu valid", 1, 1 << 20, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mem := make([]byte, tc.memSize)
			err := Write(mem, tc.numCPUs)
			if tc.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestWriteFloatingPointerChecksum(t *testing.T) {
	for _, cpus := range []int{1, 2, 4} {
		mem := make([]byte, 1<<20)
		if err := Write(mem, cpus); err != nil {
			t.Fatalf("Write(%d cpus): %v", cpus, err)
		}
		start := StartAddr(cpus)
		fp := mem[start : start+16]

		// Verify signature
		sig := string(fp[0:4])
		if sig != "_MP_" {
			t.Errorf("%d cpus: floating pointer signature = %q, want %q", cpus, sig, "_MP_")
		}

		// Verify checksum: sum of all 16 bytes mod 256 must be 0
		var sum uint8
		for _, b := range fp {
			sum += b
		}
		if sum != 0 {
			t.Errorf("%d cpus: floating pointer checksum invalid, sum = %d", cpus, sum)
		}

		// Verify spec version
		if fp[9] != specVersion {
			t.Errorf("%d cpus: spec version = %d, want %d", cpus, fp[9], specVersion)
		}
	}
}

func TestWriteTableHeaderChecksum(t *testing.T) {
	for _, cpus := range []int{1, 2, 4} {
		mem := make([]byte, 1<<20)
		if err := Write(mem, cpus); err != nil {
			t.Fatalf("Write(%d cpus): %v", cpus, err)
		}
		start := StartAddr(cpus)
		tableOff := start + 16

		// Read the table length from header bytes [4:6]
		header := mem[tableOff:]
		tableLen := binary.LittleEndian.Uint16(header[4:6])

		// Verify PCMP signature
		sig := string(header[0:4])
		if sig != "PCMP" {
			t.Errorf("%d cpus: table signature = %q, want %q", cpus, sig, "PCMP")
		}

		// Verify checksum over the entire table
		var sum uint8
		for i := uint16(0); i < tableLen; i++ {
			sum += header[i]
		}
		if sum != 0 {
			t.Errorf("%d cpus: table checksum invalid, sum = %d", cpus, sum)
		}
	}
}

func TestWriteOEMString(t *testing.T) {
	mem := make([]byte, 1<<20)
	if err := Write(mem, 1); err != nil {
		t.Fatalf("Write: %v", err)
	}
	start := StartAddr(1)
	header := mem[start+16:]
	oem := string(header[8:16])
	if oem != "FC      " {
		t.Errorf("OEM ID = %q, want %q", oem, "FC      ")
	}
	product := string(header[16:28])
	if product != "000000000000" {
		t.Errorf("Product ID = %q, want %q", product, "000000000000")
	}
}

func TestWriteCPUEntryCount(t *testing.T) {
	for _, cpus := range []int{1, 2, 4, 8} {
		mem := make([]byte, 1<<20)
		if err := Write(mem, cpus); err != nil {
			t.Fatalf("Write(%d cpus): %v", cpus, err)
		}
		start := StartAddr(cpus)
		header := mem[start+16:]

		entryCount := binary.LittleEndian.Uint16(header[34:36])
		// Expected: cpus (processor) + 1 (bus) + 1 (IOAPIC) + 24 (IRQ sources 0-23) + 2 (LINTSRC)
		wantEntries := uint16(cpus + 1 + 1 + (maxLegacyGSI + 1) + 2)
		if entryCount != wantEntries {
			t.Errorf("%d cpus: entry count = %d, want %d", cpus, entryCount, wantEntries)
		}
	}
}

func TestWriteTableLength(t *testing.T) {
	for _, cpus := range []int{1, 2, 4} {
		mem := make([]byte, 1<<20)
		if err := Write(mem, cpus); err != nil {
			t.Fatalf("Write(%d cpus): %v", cpus, err)
		}
		start := StartAddr(cpus)
		header := mem[start+16:]
		tableLen := binary.LittleEndian.Uint16(header[4:6])
		// Table = 44 (header) + entries
		// entries = cpus*20 + 8 (bus) + 8 (ioapic) + 8*24 (irqs) + 8*2 (lintsrc)
		wantLen := uint16(44 + cpus*20 + 8 + 8 + 8*(maxLegacyGSI+1) + 8*2)
		if tableLen != wantLen {
			t.Errorf("%d cpus: table length = %d, want %d", cpus, tableLen, wantLen)
		}
	}
}

func TestWriteCPUEntriesContent(t *testing.T) {
	mem := make([]byte, 1<<20)
	cpus := 4
	if err := Write(mem, cpus); err != nil {
		t.Fatalf("Write: %v", err)
	}
	start := StartAddr(cpus)
	// CPU entries start right after the 44-byte header
	entriesStart := start + 16 + 44

	for i := 0; i < cpus; i++ {
		entry := mem[entriesStart+uint64(i*20):]
		if entry[0] != 0 { // MP_PROCESSOR type
			t.Errorf("cpu %d: entry type = %d, want 0", i, entry[0])
		}
		if entry[1] != uint8(i) { // APIC ID
			t.Errorf("cpu %d: APIC ID = %d, want %d", i, entry[1], i)
		}
		flags := entry[3]
		if flags&0x01 == 0 {
			t.Errorf("cpu %d: CPU not enabled", i)
		}
		if i == 0 && flags&0x02 == 0 {
			t.Error("cpu 0: BSP flag not set")
		}
		if i != 0 && flags&0x02 != 0 {
			t.Errorf("cpu %d: BSP flag should not be set", i)
		}
	}
}

func TestWriteAPICAddress(t *testing.T) {
	mem := make([]byte, 1<<20)
	if err := Write(mem, 1); err != nil {
		t.Fatalf("Write: %v", err)
	}
	start := StartAddr(1)
	header := mem[start+16:]
	apicAddr := binary.LittleEndian.Uint32(header[36:40])
	if apicAddr != apicDefaultBase {
		t.Errorf("APIC address = %#x, want %#x", apicAddr, apicDefaultBase)
	}
}
