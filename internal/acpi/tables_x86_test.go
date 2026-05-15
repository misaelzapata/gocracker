package acpi

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func sumMod256(buf []byte) byte {
	var sum byte
	for _, b := range buf {
		sum += b
	}
	return sum
}

func TestWriteTables_RSDPSignatureAndChecksum(t *testing.T) {
	mem := make([]byte, 1<<20)
	if err := WriteTables(mem); err != nil {
		t.Fatalf("WriteTables: %v", err)
	}

	rsdp := mem[0xE0000 : 0xE0000+20]
	if !bytes.Equal(rsdp[0:8], []byte("RSD PTR ")) {
		t.Fatalf("RSDP signature = %q, want %q", rsdp[0:8], "RSD PTR ")
	}
	if got := sumMod256(rsdp); got != 0 {
		t.Fatalf("RSDP checksum sum = %d, want 0", got)
	}

	// Revision 0 for ACPI 1.0.
	if rsdp[15] != 0 {
		t.Fatalf("RSDP revision = %d, want 0", rsdp[15])
	}
}

func TestWriteTables_RSDTReachableFromRSDP(t *testing.T) {
	mem := make([]byte, 1<<20)
	if err := WriteTables(mem); err != nil {
		t.Fatalf("WriteTables: %v", err)
	}

	rsdtAddr := binary.LittleEndian.Uint32(mem[0xE0000+16 : 0xE0000+20])
	if rsdtAddr == 0 {
		t.Fatal("RSDT address in RSDP is zero")
	}
	if !bytes.Equal(mem[rsdtAddr:rsdtAddr+4], []byte("RSDT")) {
		t.Fatalf("RSDT signature = %q, want %q", mem[rsdtAddr:rsdtAddr+4], "RSDT")
	}

	rsdtLen := binary.LittleEndian.Uint32(mem[rsdtAddr+4 : rsdtAddr+8])
	wantRSDTLen := uint32(36 + 4 + 4) // SDT header + MADT pointer + HPET pointer
	if rsdtLen != wantRSDTLen {
		t.Fatalf("RSDT length = %d, want %d", rsdtLen, wantRSDTLen)
	}

	if got := sumMod256(mem[rsdtAddr : rsdtAddr+rsdtLen]); got != 0 {
		t.Fatalf("RSDT checksum sum = %d, want 0", got)
	}
}

func TestWriteTables_MADTContents(t *testing.T) {
	mem := make([]byte, 1<<20)
	if err := WriteTables(mem); err != nil {
		t.Fatalf("WriteTables: %v", err)
	}

	rsdtAddr := binary.LittleEndian.Uint32(mem[0xE0000+16 : 0xE0000+20])
	madtAddr := binary.LittleEndian.Uint32(mem[rsdtAddr+36 : rsdtAddr+40])
	if madtAddr == 0 {
		t.Fatal("MADT address in RSDT is zero")
	}
	if !bytes.Equal(mem[madtAddr:madtAddr+4], []byte("APIC")) {
		t.Fatalf("MADT signature = %q, want %q", mem[madtAddr:madtAddr+4], "APIC")
	}

	madtLen := binary.LittleEndian.Uint32(mem[madtAddr+4 : madtAddr+8])
	if got := sumMod256(mem[madtAddr : madtAddr+madtLen]); got != 0 {
		t.Fatalf("MADT checksum sum = %d, want 0", got)
	}

	lapicAddr := binary.LittleEndian.Uint32(mem[madtAddr+36 : madtAddr+40])
	if lapicAddr != 0xFEE00000 {
		t.Fatalf("MADT LocalAPIC address = %#x, want 0xFEE00000", lapicAddr)
	}
	flags := binary.LittleEndian.Uint32(mem[madtAddr+40 : madtAddr+44])
	if flags&1 == 0 {
		t.Fatalf("MADT flags = %#x, want PCAT_COMPAT bit set", flags)
	}

	// Walk the entries starting at offset 44.
	var (
		gotTypes  []byte
		gotIOAPIC uint32
		overrides []struct {
			source byte
			gsi    uint32
		}
	)
	off := uint32(44)
	end := madtAddr + madtLen
	for madtAddr+off < end {
		eType := mem[madtAddr+off]
		eLen := mem[madtAddr+off+1]
		if eLen == 0 {
			t.Fatalf("MADT entry at offset %d has zero length", off)
		}
		gotTypes = append(gotTypes, eType)
		_ = gotIOAPIC
		_ = overrides
		off += uint32(eLen)
	}

	// LAPIC-only MADT: the kernel falls back to PIC for legacy IRQs,
	// which is what WHvRequestInterrupt delivers.
	wantTypes := []byte{0}
	if !bytes.Equal(gotTypes, wantTypes) {
		t.Fatalf("MADT entry types = %v, want %v", gotTypes, wantTypes)
	}
}

func TestWriteTables_AllChecksumsZero(t *testing.T) {
	mem := make([]byte, 1<<20)
	if err := WriteTables(mem); err != nil {
		t.Fatalf("WriteTables: %v", err)
	}

	if got := sumMod256(mem[0xE0000 : 0xE0000+20]); got != 0 {
		t.Fatalf("RSDP sum = %d, want 0", got)
	}

	rsdtAddr := binary.LittleEndian.Uint32(mem[0xE0000+16 : 0xE0000+20])
	rsdtLen := binary.LittleEndian.Uint32(mem[rsdtAddr+4 : rsdtAddr+8])
	if got := sumMod256(mem[rsdtAddr : rsdtAddr+rsdtLen]); got != 0 {
		t.Fatalf("RSDT sum = %d, want 0", got)
	}

	madtAddr := binary.LittleEndian.Uint32(mem[rsdtAddr+36 : rsdtAddr+40])
	madtLen := binary.LittleEndian.Uint32(mem[madtAddr+4 : madtAddr+8])
	if got := sumMod256(mem[madtAddr : madtAddr+madtLen]); got != 0 {
		t.Fatalf("MADT sum = %d, want 0", got)
	}
}

func TestWriteTables_RejectsTooSmall(t *testing.T) {
	mem := make([]byte, 0xE0000) // ends right before the RSDP slot
	if err := WriteTables(mem); err == nil {
		t.Fatal("WriteTables on short buffer: got nil error, want non-nil")
	}
}

// TestWriteTables_HPETReachable verifies the HPET table is reachable
// from the RSDT (second pointer), has the right signature, address,
// and a zero checksum.
func TestWriteTables_HPETReachable(t *testing.T) {
	mem := make([]byte, 1<<20)
	if err := WriteTables(mem); err != nil {
		t.Fatalf("WriteTables: %v", err)
	}
	rsdtAddr := binary.LittleEndian.Uint32(mem[0xE0000+16 : 0xE0000+20])
	// Second pointer in RSDT is HPET (offset 40, after the MADT at 36).
	hpetAddr := binary.LittleEndian.Uint32(mem[rsdtAddr+40 : rsdtAddr+44])
	if hpetAddr == 0 {
		t.Fatal("HPET pointer in RSDT is zero")
	}
	if got := string(mem[hpetAddr : hpetAddr+4]); got != "HPET" {
		t.Fatalf("HPET signature = %q, want \"HPET\"", got)
	}
	hpetLen := binary.LittleEndian.Uint32(mem[hpetAddr+4 : hpetAddr+8])
	if hpetLen != 56 {
		t.Fatalf("HPET length = %d, want 56", hpetLen)
	}
	if got := sumMod256(mem[hpetAddr : hpetAddr+hpetLen]); got != 0 {
		t.Fatalf("HPET checksum sum = %d, want 0", got)
	}
	hpetBase := binary.LittleEndian.Uint64(mem[hpetAddr+44 : hpetAddr+52])
	if hpetBase != 0xFED00000 {
		t.Fatalf("HPET base = %#x, want 0xFED00000", hpetBase)
	}
}
