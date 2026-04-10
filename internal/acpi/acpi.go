package acpi

import (
	"encoding/binary"
	"fmt"
)

const (
	APICAddr        = 0xFEE00000
	IOAPICAddr      = 0xFEC00000
	RSDPAddr  uint64 = 0x000E0000
	// Firecracker reserves [0x9fc00, 0xe0000) as "system memory" for MP table,
	// ACPI and related low-memory firmware data. Our legacy MP table currently
	// lives in the EBDA-sized slice [0x9fc00, 0xa0000), so place ACPI tables in
	// the remainder of that reserved system region to avoid overlap in auto mode.
	SystemStart uint64 = 0x000A0000
	SystemEnd   uint64 = RSDPAddr

	dsdtAlignment = 8

	fadtLength = 276

	fadtFlagPowerButton = 1 << 4
	fadtFlagSleepButton = 1 << 5
	fadtFlagHWReduced   = 1 << 20
	iapcFlagNoVGA       = 1 << 2
)

var (
	oemID           = [6]byte{'G', 'O', 'C', 'R', 'K', 'R'}
	dsdtTableID     = [8]byte{'G', 'C', 'D', 'S', 'D', 'T', '0', '1'}
	fadtTableID     = [8]byte{'G', 'C', 'F', 'A', 'D', 'T', '0', '1'}
	madtTableID     = [8]byte{'G', 'C', 'M', 'A', 'D', 'T', '0', '1'}
	rsdtTableID     = [8]byte{'G', 'C', 'R', 'S', 'D', 'T', '0', '1'}
	xsdtTableID     = [8]byte{'G', 'C', 'X', 'S', 'D', 'T', '0', '1'}
	hypervisorID    = [8]byte{'G', 'O', 'C', 'R', 'K', 'V', 'M', ' '}
	creatorID       = [4]byte{'G', 'C', 'A', 'T'}
	creatorRevision = uint32(0x20260402)
)

type MMIODevice struct {
	Addr uint64
	Len  uint64
	GSI  uint32
}

func CreateX86Tables(mem []byte, vcpuCount int, mmio []MMIODevice) (uint64, error) {
	if vcpuCount <= 0 {
		return 0, fmt.Errorf("invalid vcpu count %d", vcpuCount)
	}
	if len(mem) < int(RSDPAddr)+36 {
		return 0, fmt.Errorf("guest memory too small for ACPI tables")
	}

	dsdtBody, err := buildDSDT(mmio)
	if err != nil {
		return 0, err
	}
	dsdt := buildDSDTTable(dsdtBody)
	madt := buildMADT(vcpuCount)

	cursor := SystemStart
	writeTable := func(table []byte) (uint64, error) {
		cursor = alignUp(cursor, dsdtAlignment)
		end := cursor + uint64(len(table))
		if end > SystemEnd {
			return 0, fmt.Errorf("acpi table overflow [%#x,%#x)", cursor, end)
		}
		copy(mem[cursor:end], table)
		addr := cursor
		cursor = end
		return addr, nil
	}

	dsdtAddr, err := writeTable(dsdt)
	if err != nil {
		return 0, err
	}
	fadt, err := buildFADT(dsdtAddr)
	if err != nil {
		return 0, err
	}
	fadtAddr, err := writeTable(fadt)
	if err != nil {
		return 0, err
	}
	madtAddr, err := writeTable(madt)
	if err != nil {
		return 0, err
	}
	rsdt, err := buildRSDT([]uint64{fadtAddr, madtAddr})
	if err != nil {
		return 0, err
	}
	rsdtAddr, err := writeTable(rsdt)
	if err != nil {
		return 0, err
	}
	xsdt := buildXSDT([]uint64{fadtAddr, madtAddr})
	xsdtAddr, err := writeTable(xsdt)
	if err != nil {
		return 0, err
	}
	rsdp, err := buildRSDP(rsdtAddr, xsdtAddr)
	if err != nil {
		return 0, err
	}
	copy(mem[RSDPAddr:RSDPAddr+uint64(len(rsdp))], rsdp)
	return RSDPAddr, nil
}

func alignUp(v, align uint64) uint64 {
	if align == 0 {
		return v
	}
	return (v + align - 1) &^ (align - 1)
}

func buildSDTHeader(signature string, length uint32, revision uint8, tableID [8]byte) []byte {
	hdr := make([]byte, 36)
	copy(hdr[0:4], []byte(signature))
	binary.LittleEndian.PutUint32(hdr[4:8], length)
	hdr[8] = revision
	hdr[9] = 0
	copy(hdr[10:16], oemID[:])
	copy(hdr[16:24], tableID[:])
	binary.LittleEndian.PutUint32(hdr[24:28], 0)
	copy(hdr[28:32], creatorID[:])
	binary.LittleEndian.PutUint32(hdr[32:36], creatorRevision)
	return hdr
}

func finalizeChecksum(table []byte, checksumOffset int) []byte {
	table[checksumOffset] = 0
	table[checksumOffset] = checksum(table)
	return table
}

func checksum(parts ...[]byte) byte {
	var sum byte
	for _, part := range parts {
		for _, b := range part {
			sum += b
		}
	}
	return ^sum + 1
}

func buildDSDTTable(body []byte) []byte {
	hdr := buildSDTHeader("DSDT", uint32(36+len(body)), 2, dsdtTableID)
	out := append(hdr, body...)
	return finalizeChecksum(out, 9)
}

func buildFADT(dsdtAddr uint64) ([]byte, error) {
	if dsdtAddr > 0xFFFFFFFF {
		return nil, fmt.Errorf("dsdt address %#x exceeds 32-bit range", dsdtAddr)
	}
	out := make([]byte, fadtLength)
	copy(out[:36], buildSDTHeader("FACP", fadtLength, 6, fadtTableID))
	binary.LittleEndian.PutUint32(out[40:44], uint32(dsdtAddr))
	binary.LittleEndian.PutUint16(out[109:111], iapcFlagNoVGA)
	binary.LittleEndian.PutUint32(out[112:116], fadtFlagHWReduced|fadtFlagPowerButton|fadtFlagSleepButton)
	out[131] = 5
	binary.LittleEndian.PutUint64(out[140:148], dsdtAddr)
	copy(out[268:276], hypervisorID[:])
	return finalizeChecksum(out, 9), nil
}

func buildMADT(vcpuCount int) []byte {
	body := make([]byte, 8+12+(vcpuCount*8))
	binary.LittleEndian.PutUint32(body[0:4], APICAddr)
	binary.LittleEndian.PutUint32(body[4:8], 0)
	ioapic := body[8:20]
	ioapic[0] = 1
	ioapic[1] = 12
	ioapic[2] = 0
	ioapic[3] = 0
	binary.LittleEndian.PutUint32(ioapic[4:8], IOAPICAddr)
	binary.LittleEndian.PutUint32(ioapic[8:12], 0)
	off := 20
	for i := 0; i < vcpuCount; i++ {
		lapic := body[off : off+8]
		lapic[0] = 0
		lapic[1] = 8
		lapic[2] = byte(i)
		lapic[3] = byte(i)
		binary.LittleEndian.PutUint32(lapic[4:8], 1)
		off += 8
	}
	hdr := buildSDTHeader("APIC", uint32(36+len(body)), 6, madtTableID)
	out := append(hdr, body...)
	return finalizeChecksum(out, 9)
}

func buildRSDT(addrs []uint64) ([]byte, error) {
	body := make([]byte, 4*len(addrs))
	for i, addr := range addrs {
		if addr > 0xFFFFFFFF {
			return nil, fmt.Errorf("rsdt entry %#x exceeds 32-bit range", addr)
		}
		binary.LittleEndian.PutUint32(body[i*4:], uint32(addr))
	}
	hdr := buildSDTHeader("RSDT", uint32(36+len(body)), 1, rsdtTableID)
	out := append(hdr, body...)
	return finalizeChecksum(out, 9), nil
}

func buildXSDT(addrs []uint64) []byte {
	body := make([]byte, 8*len(addrs))
	for i, addr := range addrs {
		binary.LittleEndian.PutUint64(body[i*8:], addr)
	}
	hdr := buildSDTHeader("XSDT", uint32(36+len(body)), 1, xsdtTableID)
	out := append(hdr, body...)
	return finalizeChecksum(out, 9)
}

func buildRSDP(rsdtAddr, xsdtAddr uint64) ([]byte, error) {
	if rsdtAddr > 0xFFFFFFFF {
		return nil, fmt.Errorf("rsdt address %#x exceeds 32-bit range", rsdtAddr)
	}
	out := make([]byte, 36)
	copy(out[0:8], []byte("RSD PTR "))
	copy(out[9:15], oemID[:])
	out[15] = 2
	binary.LittleEndian.PutUint32(out[16:20], uint32(rsdtAddr))
	binary.LittleEndian.PutUint32(out[20:24], 36)
	binary.LittleEndian.PutUint64(out[24:32], xsdtAddr)
	out[8] = checksum(out[:20])
	out[32] = checksum(out)
	return out, nil
}
