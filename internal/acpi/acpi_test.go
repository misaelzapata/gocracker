package acpi

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/gocracker/gocracker/internal/mptable"
)

func TestCreateX86Tables_WritesChecksummedTables(t *testing.T) {
	mem := make([]byte, 2<<20)
	rsdpAddr, err := CreateX86Tables(mem, 2, []MMIODevice{
		{Addr: 0xD0000000, Len: 0x1000, GSI: 5},
		{Addr: 0xD0001000, Len: 0x1000, GSI: 6},
	})
	if err != nil {
		t.Fatalf("CreateX86Tables() error = %v", err)
	}
	if rsdpAddr != RSDPAddr {
		t.Fatalf("RSDP addr = %#x, want %#x", rsdpAddr, RSDPAddr)
	}
	if got := checksum(mem[RSDPAddr : RSDPAddr+20]); got != 0 {
		t.Fatalf("RSDP checksum = %d, want 0", got)
	}
	if got := checksum(mem[RSDPAddr : RSDPAddr+36]); got != 0 {
		t.Fatalf("RSDP extended checksum = %d, want 0", got)
	}
	xsdtAddr := binary.LittleEndian.Uint64(mem[RSDPAddr+24 : RSDPAddr+32])
	rsdtAddr := binary.LittleEndian.Uint32(mem[RSDPAddr+16 : RSDPAddr+20])
	if rsdtAddr == 0 {
		t.Fatal("RSDT addr = 0, want non-zero")
	}
	rsdtLen := binary.LittleEndian.Uint32(mem[rsdtAddr+4 : rsdtAddr+8])
	if got := checksum(mem[rsdtAddr : uint64(rsdtAddr)+uint64(rsdtLen)]); got != 0 {
		t.Fatalf("RSDT checksum = %d, want 0", got)
	}
	xsdtLen := binary.LittleEndian.Uint32(mem[xsdtAddr+4 : xsdtAddr+8])
	if got := checksum(mem[xsdtAddr : xsdtAddr+uint64(xsdtLen)]); got != 0 {
		t.Fatalf("XSDT checksum = %d, want 0", got)
	}
	if xsdtAddr < SystemStart {
		t.Fatalf("XSDT addr = %#x, want >= %#x", xsdtAddr, SystemStart)
	}
	if xsdtAddr < mptable.StartAddr(2) {
		t.Fatalf("XSDT addr = %#x overlaps legacy MP table region ending at %#x", xsdtAddr, mptable.StartAddr(2))
	}
}

func TestBuildDSDT_ContainsVirtioAndLegacyIDs(t *testing.T) {
	body, err := buildDSDT([]MMIODevice{{Addr: 0xD0000000, Len: 0x1000, GSI: 5}})
	if err != nil {
		t.Fatalf("buildDSDT() error = %v", err)
	}
	for _, needle := range [][]byte{
		[]byte("LNRO0005"),
		[]byte("COM1"),
		[]byte("PS2_"),
	} {
		if !bytes.Contains(body, needle) {
			t.Fatalf("DSDT body missing %q", needle)
		}
	}
}
