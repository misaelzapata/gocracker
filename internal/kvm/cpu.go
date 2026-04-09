package kvm

import (
	"encoding/binary"
	"fmt"
)

// Guest memory addresses for GDT and IDT.
const (
	gdtAddr = 0x500 // 4 entries × 8 bytes = 32 bytes (0x500..0x51F)
	idtAddr = 0x520 // zero IDT (8 bytes)
)

// SetupLongMode configures a vCPU to boot directly into x86-64 long mode.
// Writes a Firecracker-compatible GDT/IDT into guest memory, sets segment
// registers, enables PAE paging, and points RIP at entryPoint.
func SetupLongMode(vcpu *VCPU, mem []byte, entryPoint, pageTableBase, bootParamsAddr uint64) error {
	// ---- Build identity-mapped page tables in guest memory ----
	buildPageTables(mem, pageTableBase)

	// ---- Write GDT entries at 0x500 in guest memory ----
	gdtEntries := [4]uint64{
		0x0000000000000000, // [0] null
		0x00AF9B000000FFFF, // [1] code64: G=1, L=1, P=1, S=1, Type=0xB
		0x00CF93000000FFFF, // [2] data:   G=1, D=1, P=1, S=1, Type=0x3
		0x008F8B000000FFFF, // [3] TSS:    G=1, P=1, S=0, Type=0xB
	}
	for i, entry := range gdtEntries {
		binary.LittleEndian.PutUint64(mem[gdtAddr+i*8:], entry)
	}

	// ---- Write zero IDT at 0x520 ----
	binary.LittleEndian.PutUint64(mem[idtAddr:], 0)

	// ---- Special registers: segments + control registers ----
	sregs, err := vcpu.GetSregs()
	if err != nil {
		return fmt.Errorf("get sregs: %w", err)
	}

	// CS: code64, selector 0x08
	sregs.CS = Segment{
		Base: 0, Limit: 0xFFFFF,
		Selector: 0x08, Type: 11,
		Present: 1, S: 1, L: 1, G: 1,
	}
	// DS/ES/FS/GS/SS: data, selector 0x10
	dataSeg := Segment{
		Base: 0, Limit: 0xFFFFF,
		Selector: 0x10, Type: 3,
		Present: 1, S: 1, DB: 1, G: 1,
	}
	sregs.DS = dataSeg
	sregs.ES = dataSeg
	sregs.FS = dataSeg
	sregs.GS = dataSeg
	sregs.SS = dataSeg
	// TR: TSS, selector 0x18
	sregs.TR = Segment{
		Base: 0, Limit: 0xFFFFF,
		Selector: 0x18, Type: 11,
		Present: 1, G: 1,
	}

	// GDT/IDT descriptor table registers
	sregs.GDT = DTTR{Base: gdtAddr, Limit: 31}  // 4 entries × 8 - 1
	sregs.IDT = DTTR{Base: idtAddr, Limit: 7}

	// Control registers (OR with existing to preserve KVM defaults)
	sregs.CR0 |= 0x80000001 // PE + PG
	sregs.CR3 = pageTableBase
	sregs.CR4 |= 0x20   // PAE
	sregs.EFER |= 0x500 // LME + LMA

	if err := vcpu.SetSregs(sregs); err != nil {
		return fmt.Errorf("set sregs: %w", err)
	}

	// ---- General-purpose registers ----
	regs, err := vcpu.GetRegs()
	if err != nil {
		return fmt.Errorf("get regs: %w", err)
	}
	regs.RIP = entryPoint
	regs.RSI = bootParamsAddr // Linux 64-bit boot protocol
	regs.RFLAGS = 0x2         // reserved bit must be 1
	regs.RSP = 0x8FF0
	regs.RBP = 0x8FF0
	if err := vcpu.SetRegs(regs); err != nil {
		return fmt.Errorf("set regs: %w", err)
	}
	return nil
}

// buildPageTables writes a minimal 4-level identity-mapped page table
// at pageTableBase in guest memory (requires ~5 × 4 KiB = 20 KiB).
// Maps the first 4 GiB as 2 MiB huge pages (present, writable, user).
func buildPageTables(mem []byte, base uint64) {
	const pageSize = 0x1000
	const flags = 0x07    // Present | Writable | User
	const hugeFlag = 0x83 // Present | Writable | HugePage

	pml4 := base
	pdpt := base + pageSize
	pd := base + 2*pageSize

	writeU64 := func(addr, val uint64) {
		off := addr
		mem[off] = byte(val)
		mem[off+1] = byte(val >> 8)
		mem[off+2] = byte(val >> 16)
		mem[off+3] = byte(val >> 24)
		mem[off+4] = byte(val >> 32)
		mem[off+5] = byte(val >> 40)
		mem[off+6] = byte(val >> 48)
		mem[off+7] = byte(val >> 56)
	}

	// PML4[0] -> PDPT
	writeU64(pml4, pdpt|flags)

	// PDPT[0..3] -> PD0..PD3  (each covers 1 GiB)
	for i := uint64(0); i < 4; i++ {
		thisPD := pd + i*pageSize
		writeU64(pdpt+i*8, thisPD|flags)

		// Each PD has 512 entries of 2 MiB huge pages
		for j := uint64(0); j < 512; j++ {
			physAddr := (i*512 + j) * 0x200000 // 2 MiB
			writeU64(thisPD+j*8, physAddr|uint64(hugeFlag))
		}
	}
}
