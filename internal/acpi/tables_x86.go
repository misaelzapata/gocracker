package acpi

import (
	"encoding/binary"
	"fmt"
)

// Layout for the minimal ACPI tables emitted by WriteTables.
//
// Linux's BIOS-style ACPI probe scans low memory [0xE0000, 0xFFFFF) on
// 16-byte boundaries looking for "RSD PTR ". We place the RSDP at the
// start of that window, with the RSDT and MADT in the same low-memory
// region so a 32-bit ACPI 1.0 RSDP can address them.
const (
	wrRSDPAddr uint32 = 0x000E0000
	wrRSDTAddr uint32 = 0x000E0100
	wrMADTAddr uint32 = 0x000E0200
	wrHPETAddr uint32 = 0x000E0300

	rsdpLenACPI1 = 20
	sdtHeaderLen = 36
	hpetTableLen = 56

	madtEntryLAPIC       = 0
	madtEntryIOAPIC      = 1
	madtEntryIntOverride = 2

	madtFlagPCATCompat = 1
	lapicFlagEnabled   = 1

	// Local APIC default address.
	wrLocalAPICAddr uint32 = 0xFEE00000
	// I/O APIC default address.
	wrIOAPICAddr uint32 = 0xFEC00000
	// HPET default base. Matches pkg/vmm.hpetMMIOBase.
	wrHPETBase uint64 = 0xFED00000
)

var (
	wrOEMID           = [6]byte{'G', 'O', 'C', 'R', 'K', 'R'}
	wrOEMTableID      = [8]byte{'G', 'O', 'C', 'R', 'K', 'R', ' ', ' '}
	wrCreatorID       = [4]byte{'G', 'O', 'C', 'K'}
	wrCreatorRevision = uint32(1)
)

// WriteTables emits a minimal RSDP + RSDT + MADT into mem at the fixed
// low-memory offsets (0xE0000 / 0xE0100 / 0xE0200) so Linux's ACPI probe
// can discover the LAPIC at 0xFEE00000 and I/O APIC at 0xFEC00000 without
// falling back to compiled-in defaults.
//
// The function is pure: it writes bytes into the caller's guest-RAM slice
// and never touches any host resource. Callers should hand it a slice
// whose index 0 corresponds to guest physical address 0.
//
// The MADT carries one Type 0 (LAPIC) entry only. We deliberately do
// NOT advertise an I/O APIC or any interrupt-source overrides: we
// emulate the legacy 8259 PIC and inject IRQs via
// WHvRequestInterrupt — advertising an IOAPIC would make Linux mask
// the PIC and route legacy IRQs (timer, COM1) through an IOAPIC we
// don't emulate, killing every external IRQ delivery.
func WriteTables(mem []byte) error {
	madtLen := sdtHeaderLen + 8 /* LAPIC addr + flags */ +
		8 /* LAPIC entry */
	rsdtLen := sdtHeaderLen + 8 /* two u32 pointers: MADT + HPET */

	required := int(wrHPETAddr) + hpetTableLen
	if len(mem) < required {
		return fmt.Errorf("acpi: guest memory %d bytes too small for ACPI tables (need %d)", len(mem), required)
	}

	// Zero the table regions defensively so re-runs against a dirty
	// buffer don't leave stale bytes that would break the checksum.
	for i := 0; i < rsdpLenACPI1; i++ {
		mem[int(wrRSDPAddr)+i] = 0
	}
	for i := 0; i < rsdtLen; i++ {
		mem[int(wrRSDTAddr)+i] = 0
	}
	for i := 0; i < madtLen; i++ {
		mem[int(wrMADTAddr)+i] = 0
	}
	for i := 0; i < hpetTableLen; i++ {
		mem[int(wrHPETAddr)+i] = 0
	}

	writeMADT(mem[wrMADTAddr : int(wrMADTAddr)+madtLen])
	writeHPET(mem[wrHPETAddr : int(wrHPETAddr)+hpetTableLen])
	writeRSDT(mem[wrRSDTAddr:int(wrRSDTAddr)+rsdtLen], wrMADTAddr, wrHPETAddr)
	writeRSDP(mem[wrRSDPAddr : int(wrRSDPAddr)+rsdpLenACPI1])
	return nil
}

// writeSDTHeader populates the 36-byte System Description Table header
// at the start of buf. The caller is responsible for setting buf[9]
// (the checksum byte) once the rest of the table has been written.
func writeSDTHeader(buf []byte, signature string, length uint32, revision uint8) {
	copy(buf[0:4], signature)
	binary.LittleEndian.PutUint32(buf[4:8], length)
	buf[8] = revision
	buf[9] = 0 // checksum placeholder
	copy(buf[10:16], wrOEMID[:])
	copy(buf[16:24], wrOEMTableID[:])
	binary.LittleEndian.PutUint32(buf[24:28], 1) // OEM revision
	copy(buf[28:32], wrCreatorID[:])
	binary.LittleEndian.PutUint32(buf[32:36], wrCreatorRevision)
}

// onesComplementChecksum computes the byte that, when stored at the
// checksum slot, makes the running sum of all bytes in buf equal 0 mod 256.
func onesComplementChecksum(buf []byte) byte {
	var sum byte
	for _, b := range buf {
		sum += b
	}
	return byte(-int8(sum))
}

func writeRSDP(buf []byte) {
	copy(buf[0:8], "RSD PTR ")
	// buf[8] is the checksum byte (filled in below).
	copy(buf[9:15], wrOEMID[:])
	buf[15] = 0 // revision 0 == ACPI 1.0
	binary.LittleEndian.PutUint32(buf[16:20], wrRSDTAddr)
	buf[8] = onesComplementChecksum(buf[:rsdpLenACPI1])
}

func writeRSDT(buf []byte, madtAddr, hpetAddr uint32) {
	writeSDTHeader(buf, "RSDT", uint32(len(buf)), 1)
	binary.LittleEndian.PutUint32(buf[36:40], madtAddr)
	binary.LittleEndian.PutUint32(buf[40:44], hpetAddr)
	buf[9] = onesComplementChecksum(buf)
}

// writeHPET emits the 56-byte ACPI HPET description table (per ACPI 4.0
// spec) so Linux discovers the high-resolution timer at 0xFED00000.
// Without it, Linux falls back to PIT-based TSC calibration which
// doesn't converge against our software-only PIT counter readbacks.
func writeHPET(buf []byte) {
	writeSDTHeader(buf, "HPET", uint32(len(buf)), 1)
	// Event Timer Block ID — encodes vendor (Intel = 0x8086), revision,
	// number of timers, and 64-bit support. Mirrors pkg/vmm.hpetCapabilities
	// low 32 bits.
	const blockID uint32 = 0x8086A201
	binary.LittleEndian.PutUint32(buf[36:40], blockID)
	buf[40] = 0  // Address Space ID: 0 = system memory
	buf[41] = 64 // Register Bit Width
	buf[42] = 0  // Register Bit Offset
	buf[43] = 0  // Reserved
	binary.LittleEndian.PutUint64(buf[44:52], wrHPETBase)
	buf[52] = 0  // HPET Number
	binary.LittleEndian.PutUint16(buf[53:55], 0) // Main Counter Minimum
	buf[55] = 0  // Page Protection / OEM Attribute
	buf[9] = onesComplementChecksum(buf)
}

func writeMADT(buf []byte) {
	writeSDTHeader(buf, "APIC", uint32(len(buf)), 1)

	// MADT body: LocalAPIC-address + flags, then variable-length entries.
	binary.LittleEndian.PutUint32(buf[36:40], wrLocalAPICAddr)
	binary.LittleEndian.PutUint32(buf[40:44], madtFlagPCATCompat)

	off := 44

	// Type 0: Processor Local APIC (8 bytes).
	buf[off+0] = madtEntryLAPIC
	buf[off+1] = 8
	buf[off+2] = 0 // ACPI processor ID
	buf[off+3] = 0 // APIC ID
	binary.LittleEndian.PutUint32(buf[off+4:off+8], lapicFlagEnabled)
	off += 8

	if off != len(buf) {
		panic(fmt.Sprintf("acpi: MADT layout mismatch: wrote %d bytes, buffer is %d", off, len(buf)))
	}

	buf[9] = onesComplementChecksum(buf)
}
