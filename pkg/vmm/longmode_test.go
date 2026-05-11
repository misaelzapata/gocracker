package vmm

import (
	"encoding/binary"
	"testing"
)

// TestBuildBootPageTablesIdentity verifies that the page tables built
// by BuildBootPageTables identity-map the first 4 GiB:
//   - PML4[0] → PDPT (at base+0x1000)
//   - PDPT[0..3] → PD0..PD3 (at base+0x2000 .. base+0x5000)
//   - Each PD entry is a 2 MiB huge page with Present|Writable|HugePage
//   - PD[0][0] maps GPA 0, PD[3][511] maps GPA 4 GiB - 2 MiB
//
// Breaking this layout means the kernel page-faults on its first
// memory access after long-mode entry — silent boot hang, not even a
// triple-fault. Worth pinning down explicitly.
func TestBuildBootPageTablesIdentity(t *testing.T) {
	mem := make([]byte, 64*1024*1024) // 64 MiB scratch
	base := uint64(0x9000)
	BuildBootPageTables(mem, base)

	const pageSize = 0x1000
	pml4 := base
	pdpt := base + pageSize
	pd := base + 2*pageSize

	// PML4[0] should point at PDPT with Present|Writable|User (0x07).
	pml4e := binary.LittleEndian.Uint64(mem[pml4:])
	if pml4e&^uint64(0xFFF) != pdpt || pml4e&0x07 != 0x07 {
		t.Errorf("PML4[0] = %#x; want PDPT=%#x | flags=0x07", pml4e, pdpt)
	}

	// PDPT[0..3] should each point at the corresponding PD.
	for i := uint64(0); i < 4; i++ {
		pdpte := binary.LittleEndian.Uint64(mem[pdpt+i*8:])
		wantPD := pd + i*pageSize
		if pdpte&^uint64(0xFFF) != wantPD || pdpte&0x07 != 0x07 {
			t.Errorf("PDPT[%d] = %#x; want PD=%#x | flags=0x07", i, pdpte, wantPD)
		}
	}

	// Spot-check a few PD entries.
	cases := []struct {
		pdIdx, entry uint64
		wantPhys     uint64
	}{
		{0, 0, 0},                            // GPA 0
		{0, 1, 0x200000},                      // GPA 2 MiB
		{0, 511, 511 * 0x200000},              // GPA ~1 GiB - 2 MiB
		{1, 0, 0x40000000},                    // GPA 1 GiB
		{3, 511, (3*512 + 511) * 0x200000},   // GPA 4 GiB - 2 MiB
	}
	for _, c := range cases {
		pdEntry := binary.LittleEndian.Uint64(mem[pd+c.pdIdx*pageSize+c.entry*8:])
		gotPhys := pdEntry &^ uint64(0xFFF)
		if gotPhys != c.wantPhys {
			t.Errorf("PD[%d][%d] physaddr = %#x; want %#x", c.pdIdx, c.entry, gotPhys, c.wantPhys)
		}
		if pdEntry&0xFF != 0x83 {
			t.Errorf("PD[%d][%d] flags = %#x; want 0x83 (Present|Writable|HugePage)", c.pdIdx, c.entry, pdEntry&0xFF)
		}
	}
}

// TestWriteBootGDT pins the Firecracker-compatible GDT layout. The
// values are well-defined x86 descriptors; if any byte changes the
// kernel will general-protection-fault on its first segment load.
func TestWriteBootGDT(t *testing.T) {
	mem := make([]byte, 0x1000)
	WriteBootGDT(mem)

	want := []uint64{
		0x0000000000000000, // [0] null
		0x00AF9B000000FFFF, // [1] code64
		0x00CF93000000FFFF, // [2] data
		0x008F8B000000FFFF, // [3] TSS
	}
	for i, w := range want {
		got := binary.LittleEndian.Uint64(mem[BootGDTAddr+i*8:])
		if got != w {
			t.Errorf("GDT[%d] = %#016x; want %#016x", i, got, w)
		}
	}
	idt := binary.LittleEndian.Uint64(mem[BootIDTAddr:])
	if idt != 0 {
		t.Errorf("IDT[0] = %#x; want 0 (zero IDT at boot)", idt)
	}
}

// TestLongModeBootRegisters pins the GP-register state.
func TestLongModeBootRegisters(t *testing.T) {
	regs := LongModeBootRegisters(0x100000, 0x7000)
	if regs.RIP != 0x100000 {
		t.Errorf("RIP = %#x; want 0x100000", regs.RIP)
	}
	if regs.RSI != 0x7000 {
		t.Errorf("RSI = %#x; want 0x7000 (boot_params address per Linux 64-bit boot protocol)", regs.RSI)
	}
	if regs.RFLAGS != 0x2 {
		t.Errorf("RFLAGS = %#x; want 0x2 (reserved bit 1)", regs.RFLAGS)
	}
	if regs.RSP != 0x8FF0 || regs.RBP != 0x8FF0 {
		t.Errorf("RSP/RBP = %#x/%#x; want 0x8FF0/0x8FF0", regs.RSP, regs.RBP)
	}
}

// TestLongModeBootSegments pins the segment + control register state.
// A subtle wrong bit (e.g. L=0 on CS, missing PE in CR0, missing LME
// in EFER) yields a triple-fault on the first instruction.
func TestLongModeBootSegments(t *testing.T) {
	const pageTableBase = uint64(0x9000)
	s := LongModeBootSegments(pageTableBase)

	// CS: code64, selector 0x08, L=1 (long mode), G=1.
	if s.CS.Selector != 0x08 || s.CS.L != 1 || s.CS.Type != 11 || s.CS.S != 1 || s.CS.Present != 1 || s.CS.G != 1 {
		t.Errorf("CS not long-mode code: %+v", s.CS)
	}
	// DS/ES/FS/GS/SS: data, selector 0x10, DB=1, no L.
	for name, seg := range map[string]Segment{"DS": s.DS, "ES": s.ES, "FS": s.FS, "GS": s.GS, "SS": s.SS} {
		if seg.Selector != 0x10 || seg.Type != 3 || seg.S != 1 || seg.Present != 1 || seg.DB != 1 || seg.G != 1 {
			t.Errorf("%s not boot data segment: %+v", name, seg)
		}
	}
	// TR: selector 0x18, Type=11, no L, no DB.
	if s.TR.Selector != 0x18 || s.TR.Type != 11 || s.TR.Present != 1 || s.TR.G != 1 {
		t.Errorf("TR not TSS: %+v", s.TR)
	}
	// GDT base/limit per Firecracker layout.
	if s.GDT.Base != BootGDTAddr || s.GDT.Limit != 31 {
		t.Errorf("GDT = {base=%#x, limit=%d}; want {base=%#x, limit=31}", s.GDT.Base, s.GDT.Limit, BootGDTAddr)
	}
	if s.IDT.Base != BootIDTAddr || s.IDT.Limit != 7 {
		t.Errorf("IDT = {base=%#x, limit=%d}; want {base=%#x, limit=7}", s.IDT.Base, s.IDT.Limit, BootIDTAddr)
	}
	// Control regs: PE+PG, PAE, LME+LMA.
	if s.CR0 != 0x80000001 {
		t.Errorf("CR0 = %#x; want 0x80000001 (PE|PG)", s.CR0)
	}
	if s.CR3 != pageTableBase {
		t.Errorf("CR3 = %#x; want %#x (page table base)", s.CR3, pageTableBase)
	}
	if s.CR4 != 0x20 {
		t.Errorf("CR4 = %#x; want 0x20 (PAE)", s.CR4)
	}
	if s.EFER != 0x500 {
		t.Errorf("EFER = %#x; want 0x500 (LME|LMA)", s.EFER)
	}
}
