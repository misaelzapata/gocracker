package vmm

import "encoding/binary"

// Guest memory addresses for the boot GDT and IDT. Match Firecracker's
// layout so kernels that snoop the GDT find what they expect.
const (
	BootGDTAddr = 0x500 // 4 entries × 8 bytes = 32 bytes (0x500..0x51F)
	BootIDTAddr = 0x520 // zero IDT (8 bytes)

	// PageTableBase is the conventional location of the boot-time PML4
	// inside the first MiB of guest RAM. Each consecutive 4 KiB slot
	// holds PDPT, PD0, PD1, PD2, PD3 — 5 × 4 KiB = 20 KiB total.
	PageTableBase = 0x9000

	// BootParamsAddr is where the Linux 64-bit boot protocol expects
	// the boot_params struct (zero-page) to live before kernel entry.
	BootParamsAddr = 0x7000
)

// BuildBootPageTables writes a 4-level identity-mapped page table at
// base in mem. Maps the first 4 GiB as 2 MiB huge pages with present /
// writable / user flags. Uses 5 × 4 KiB = 20 KiB starting at base:
//
//	PML4 at base
//	PDPT at base+0x1000
//	PD0..PD3 at base+0x2000 .. base+0x5000
//
// The same layout works for both KVM and WHP — the page tables live in
// guest RAM, which both hypervisors map identically. Used by the boot
// setup paths in internal/kvm (Linux) and pkg/vmm/hypervisor_windows.go
// (Windows, Phase 2e).
func BuildBootPageTables(mem []byte, base uint64) {
	const pageSize = 0x1000
	const flags = uint64(0x07)    // Present | Writable | User
	const hugeFlag = uint64(0x83) // Present | Writable | HugePage

	pml4 := base
	pdpt := base + pageSize
	pd := base + 2*pageSize

	// PML4[0] -> PDPT
	binary.LittleEndian.PutUint64(mem[pml4:], pdpt|flags)

	// PDPT[0..3] -> PD0..PD3 (each covers 1 GiB).
	for i := uint64(0); i < 4; i++ {
		thisPD := pd + i*pageSize
		binary.LittleEndian.PutUint64(mem[pdpt+i*8:], thisPD|flags)

		// Each PD has 512 entries of 2 MiB huge pages.
		for j := uint64(0); j < 512; j++ {
			physAddr := (i*512 + j) * 0x200000 // 2 MiB
			binary.LittleEndian.PutUint64(mem[thisPD+j*8:], physAddr|hugeFlag)
		}
	}
}

// WriteBootGDT writes the Firecracker-compatible 4-entry GDT at
// BootGDTAddr and a zero IDT at BootIDTAddr inside mem.
//
// GDT layout:
//
//	[0] 0x0000000000000000  null
//	[1] 0x00AF9B000000FFFF  code64: G=1, L=1, P=1, S=1, Type=0xB
//	[2] 0x00CF93000000FFFF  data:   G=1, D=1, P=1, S=1, Type=0x3
//	[3] 0x008F8B000000FFFF  TSS:    G=1, P=1, S=0, Type=0xB
//
// IDT is zeroed — no exception handlers at boot; the kernel installs
// its own immediately after entry.
func WriteBootGDT(mem []byte) {
	gdt := [4]uint64{
		0x0000000000000000, // [0] null
		0x00AF9B000000FFFF, // [1] code64
		0x00CF93000000FFFF, // [2] data
		0x008F8B000000FFFF, // [3] TSS
	}
	for i, entry := range gdt {
		binary.LittleEndian.PutUint64(mem[BootGDTAddr+i*8:], entry)
	}
	binary.LittleEndian.PutUint64(mem[BootIDTAddr:], 0)
}

// LongModeBootRegisters returns the GP-register state required to enter
// 64-bit long mode at `entryPoint`. RSI carries the boot_params address
// (Linux 64-bit boot protocol).
//
// Stack pointer is set to 0x8FF0 — within the first MiB, below the
// kernel's expected location (0x100000). Matches kvm.SetupLongMode and
// node-vmm's boot register sequence.
func LongModeBootRegisters(entryPoint, bootParamsAddr uint64) Registers {
	return Registers{
		RIP:    entryPoint,
		RSI:    bootParamsAddr,
		RFLAGS: 0x2, // reserved bit must be 1
		RSP:    0x8FF0,
		RBP:    0x8FF0,
	}
}

// LongModeBootSegments returns the segment + control-register state
// required for 64-bit long mode. pageTableBase points at the PML4.
//
// CS = code64 (selector 0x08), DS/ES/FS/GS/SS = data (selector 0x10),
// TR = TSS (selector 0x18), all with limit 0xFFFFF and granularity bit
// so they cover the entire 4 GiB address space.
//
// CR0 = PE|PG, CR3 = pageTableBase, CR4 = PAE, EFER = LME|LMA.
func LongModeBootSegments(pageTableBase uint64) SegmentRegisters {
	codeSeg := Segment{
		Base: 0, Limit: 0xFFFFF,
		Selector: 0x08, Type: 11,
		Present: 1, S: 1, L: 1, G: 1,
	}
	dataSeg := Segment{
		Base: 0, Limit: 0xFFFFF,
		Selector: 0x10, Type: 3,
		Present: 1, S: 1, DB: 1, G: 1,
	}
	tssSeg := Segment{
		Base: 0, Limit: 0xFFFFF,
		Selector: 0x18, Type: 11,
		Present: 1, G: 1,
	}
	return SegmentRegisters{
		CS:       codeSeg,
		DS:       dataSeg,
		ES:       dataSeg,
		FS:       dataSeg,
		GS:       dataSeg,
		SS:       dataSeg,
		TR:       tssSeg,
		GDT:      DescriptorTable{Base: BootGDTAddr, Limit: 31}, // 4 entries × 8 - 1
		IDT:      DescriptorTable{Base: BootIDTAddr, Limit: 7},
		CR0:      0x80000001, // PE | PG
		CR3:      pageTableBase,
		CR4:      0x20,  // PAE
		EFER:     0x500, // LME | LMA
	}
}
