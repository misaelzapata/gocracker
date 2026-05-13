//go:build windows

package vmm

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestVirtioRngFillsBuffer drives a single descriptor request: avail
// ring points at a descriptor whose buffer is writable; the device
// should fill it with entropy and push the used ring.
func TestVirtioRngFillsBuffer(t *testing.T) {
	mem := make([]byte, 4*4096)
	irqCount := 0
	d := NewVirtioRng(0xD0001000, mem, func() { irqCount++ })

	if v := d.ReadMMIO(0xD0001000, 4); v != VirtioMagic {
		t.Errorf("MagicValue = %#x; want %#x", v, VirtioMagic)
	}
	if v := d.ReadMMIO(0xD0001008, 4); v != 4 {
		t.Errorf("DeviceId = %d; want 4 (virtio-rng)", v)
	}

	const (
		descGPA  uint64 = 0x1000
		availGPA uint64 = 0x2000
		usedGPA  uint64 = 0x3000
		dataGPA  uint64 = 0x100
	)

	// Single descriptor: writable, 256 bytes.
	binary.LittleEndian.PutUint64(mem[descGPA:], dataGPA)
	binary.LittleEndian.PutUint32(mem[descGPA+8:], 256)
	binary.LittleEndian.PutUint16(mem[descGPA+12:], VirtioDescFWrite)
	binary.LittleEndian.PutUint16(mem[descGPA+14:], 0)

	// Avail ring: idx=1, ring[0]=0.
	binary.LittleEndian.PutUint16(mem[availGPA+2:], 1)
	binary.LittleEndian.PutUint16(mem[availGPA+4:], 0)

	w := func(off uint64, v uint32) { d.WriteMMIO(0xD0001000+off, 4, v) }
	w(VirtioMmioQueueSel, 0)
	w(VirtioMmioQueueNum, 64)
	w(VirtioMmioQueueDescLow, uint32(descGPA))
	w(VirtioMmioQueueDriverLow, uint32(availGPA))
	w(VirtioMmioQueueDeviceLow, uint32(usedGPA))
	w(VirtioMmioQueueReady, 1)
	w(VirtioMmioQueueNotify, 0)

	// Used ring should have idx=1 with len=256.
	usedIdx := binary.LittleEndian.Uint16(mem[usedGPA+2:])
	if usedIdx != 1 {
		t.Errorf("used.idx = %d; want 1", usedIdx)
	}
	entryLen := binary.LittleEndian.Uint32(mem[usedGPA+8:])
	if entryLen != 256 {
		t.Errorf("used[0].len = %d; want 256", entryLen)
	}

	// Buffer should be filled with entropy — extremely unlikely to be
	// all-zero by chance.
	if bytes.Count(mem[dataGPA:dataGPA+256], []byte{0}) == 256 {
		t.Error("RNG buffer is all zero; entropy source not delivering")
	}

	if irqCount != 1 {
		t.Errorf("raiseIRQ called %d times; want 1", irqCount)
	}
}
