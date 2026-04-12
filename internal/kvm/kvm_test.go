package kvm

import (
	"encoding/binary"
	"testing"
)

func TestGuestPhysBaseAndBuildPageTables(t *testing.T) {
	vm := &VM{guestPhysBase: 0x40000000}
	if got := vm.GuestPhysBase(); got != 0x40000000 {
		t.Fatalf("GuestPhysBase() = %#x", got)
	}

	mem := make([]byte, 0x10000)
	base := uint64(0x1000)
	buildPageTables(mem, base)

	if got := binary.LittleEndian.Uint64(mem[base : base+8]); got == 0 {
		t.Fatal("expected non-zero PML4 entry")
	}
	if got := binary.LittleEndian.Uint64(mem[base+0x1000 : base+0x1000+8]); got == 0 {
		t.Fatal("expected non-zero PDPT entry")
	}
}
