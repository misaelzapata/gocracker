package mptable

import (
	"encoding/binary"
	"fmt"
)

const (
	baseRAMEnd      = 0xA0000
	apicDefaultBase = 0xFEE00000
	ioAPICBase      = 0xFEC00000
	apicVersion     = 0x14
	cpuStepping     = 0x600
	cpuFeatureAPIC  = 0x200
	cpuFeatureFPU   = 0x001
	maxCPUs         = 254
	maxLegacyGSI    = 23
	specVersion     = 4
)

var (
	floatingSignature = []byte("_MP_")
	tableSignature    = []byte("PCMP")
	oemID             = []byte("FC      ")
	productID         = []byte("000000000000")
	isaBusType        = []byte("ISA   ")
)

// Size returns the MP table size in bytes for the requested CPU count.
func Size(numCPUs int) int {
	return 16 + 44 + (20 * numCPUs) + 8 + 8 + (8 * (maxLegacyGSI + 1)) + (8 * 2)
}

// StartAddr returns the guest physical address where the MP table should start.
func StartAddr(numCPUs int) uint64 {
	start := baseRAMEnd - uint64(Size(numCPUs))
	return start &^ 0xF
}

// Write emits a Firecracker-style MP table into low guest memory so Linux can
// discover the IOAPIC/APIC topology and route legacy GSIs.
func Write(mem []byte, numCPUs int) error {
	if numCPUs <= 0 {
		return fmt.Errorf("invalid cpu count %d", numCPUs)
	}
	if numCPUs > maxCPUs {
		return fmt.Errorf("cpu count %d exceeds max %d", numCPUs, maxCPUs)
	}

	size := Size(numCPUs)
	start := StartAddr(numCPUs)
	end := start + uint64(size)
	if end > uint64(len(mem)) {
		return fmt.Errorf("mp table [%#x,%#x) exceeds guest memory", start, end)
	}

	buf := make([]byte, size)
	tableAddr := uint32(start + 16)
	ioAPICID := uint8(numCPUs + 1)

	// MP floating pointer structure.
	copy(buf[0:4], floatingSignature)
	binary.LittleEndian.PutUint32(buf[4:8], tableAddr)
	buf[8] = 1
	buf[9] = specVersion
	buf[10] = checksum(buf[0:16])

	// MP configuration table header and entries.
	entries := make([]byte, 0, size-16-44)
	entryCount := uint16(0)

	for cpuID := 0; cpuID < numCPUs; cpuID++ {
		entry := make([]byte, 20)
		entry[0] = 0 // MP_PROCESSOR
		entry[1] = uint8(cpuID)
		entry[2] = apicVersion
		entry[3] = 0x01
		if cpuID == 0 {
			entry[3] |= 0x02 // CPU_BOOTPROCESSOR
		}
		binary.LittleEndian.PutUint32(entry[4:8], cpuStepping)
		binary.LittleEndian.PutUint32(entry[8:12], cpuFeatureAPIC|cpuFeatureFPU)
		entries = append(entries, entry...)
		entryCount++
	}

	busEntry := make([]byte, 8)
	busEntry[0] = 1 // MP_BUS
	busEntry[1] = 0
	copy(busEntry[2:8], isaBusType)
	entries = append(entries, busEntry...)
	entryCount++

	ioAPICEntry := make([]byte, 8)
	ioAPICEntry[0] = 2 // MP_IOAPIC
	ioAPICEntry[1] = ioAPICID
	ioAPICEntry[2] = apicVersion
	ioAPICEntry[3] = 1 // usable
	binary.LittleEndian.PutUint32(ioAPICEntry[4:8], ioAPICBase)
	entries = append(entries, ioAPICEntry...)
	entryCount++

	for irq := 0; irq <= maxLegacyGSI; irq++ {
		entry := make([]byte, 8)
		entry[0] = 3                                 // MP_INTSRC
		entry[1] = 0                                 // mp_INT
		binary.LittleEndian.PutUint16(entry[2:4], 0) // default polarity/trigger
		entry[4] = 0                                 // ISA bus
		entry[5] = uint8(irq)
		entry[6] = ioAPICID
		entry[7] = uint8(irq)
		entries = append(entries, entry...)
		entryCount++
	}

	extINT := make([]byte, 8)
	extINT[0] = 4 // MP_LINTSRC
	extINT[1] = 3 // mp_ExtINT
	binary.LittleEndian.PutUint16(extINT[2:4], 0)
	extINT[4] = 0
	extINT[5] = 0
	extINT[6] = 0
	extINT[7] = 0
	entries = append(entries, extINT...)
	entryCount++

	nmi := make([]byte, 8)
	nmi[0] = 4 // MP_LINTSRC
	nmi[1] = 1 // mp_NMI
	binary.LittleEndian.PutUint16(nmi[2:4], 0)
	nmi[4] = 0
	nmi[5] = 0
	nmi[6] = 0xFF
	nmi[7] = 1
	entries = append(entries, nmi...)
	entryCount++

	header := buf[16 : 16+44]
	copy(header[0:4], tableSignature)
	binary.LittleEndian.PutUint16(header[4:6], uint16(44+len(entries)))
	header[6] = specVersion
	header[7] = 0 // checksum filled below
	copy(header[8:16], oemID)
	copy(header[16:28], productID)
	binary.LittleEndian.PutUint32(header[28:32], 0)
	binary.LittleEndian.PutUint16(header[32:34], 0)
	binary.LittleEndian.PutUint16(header[34:36], entryCount)
	binary.LittleEndian.PutUint32(header[36:40], apicDefaultBase)
	binary.LittleEndian.PutUint32(header[40:44], 0)

	copy(buf[16+44:], entries)
	header[7] = checksum(buf[16 : 16+44+len(entries)])

	copy(mem[start:end], buf)
	return nil
}

func checksum(data []byte) byte {
	var sum byte
	for _, b := range data {
		sum += b
	}
	return ^sum + 1
}
