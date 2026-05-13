//go:build windows

package vmm

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// TestVirtioBlkConfigRegisters checks the read-only registers + the
// device-specific config (capacity at 0x100) for a freshly-opened blk
// device. Doesn't issue any virtq requests.
func TestVirtioBlkConfigRegisters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rootfs.img")
	// 1 MiB image — 2048 sectors of 512 bytes each.
	if err := os.WriteFile(path, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatalf("write tmp image: %v", err)
	}
	mem := make([]byte, 4096)
	d, err := NewVirtioBlk(0xD0000000, mem, path, false, nil)
	if err != nil {
		t.Fatalf("NewVirtioBlk: %v", err)
	}
	defer d.Close()

	if v := d.ReadMMIO(0xD0000000, 4); v != VirtioMagic {
		t.Errorf("MagicValue = %#x; want %#x", v, VirtioMagic)
	}
	if v := d.ReadMMIO(0xD0000004, 4); v != VirtioVersion {
		t.Errorf("Version = %d; want %d", v, VirtioVersion)
	}
	if v := d.ReadMMIO(0xD0000008, 4); v != 2 {
		t.Errorf("DeviceId = %d; want 2 (virtio-blk)", v)
	}
	// Capacity at 0x100 is a uint64; the guest reads it as two u32s.
	lo := d.ReadMMIO(0xD0000100, 4)
	hi := d.ReadMMIO(0xD0000104, 4)
	got := uint64(hi)<<32 | uint64(lo)
	if got != 2048 {
		t.Errorf("capacity = %d sectors; want 2048", got)
	}
	if v := d.ReadMMIO(0xD000010C, 4); v != 128 {
		t.Errorf("seg_max = %d; want 128", v)
	}
}

// TestVirtioBlkGetID drives a minimal T_GET_ID request through the
// MMIO interface — the simplest happy-path that touches every code path
// (descriptor walk, status write, used-ring push).
func TestVirtioBlkGetID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rootfs.img")
	if err := os.WriteFile(path, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatalf("write tmp image: %v", err)
	}
	mem := make([]byte, 4*4096) // 16 KiB so we have room for queue + buffers
	irqCount := 0
	d, err := NewVirtioBlk(0xD0000000, mem, path, false, func() { irqCount++ })
	if err != nil {
		t.Fatalf("NewVirtioBlk: %v", err)
	}
	defer d.Close()

	// Lay out queue 0 in guest "RAM" at fixed offsets:
	//   desc table @ 0x1000 (4 KiB, 16 bytes * 256 = 4 KiB)
	//   avail ring @ 0x2000
	//   used ring  @ 0x3000
	const (
		descGPA   uint64 = 0x1000
		availGPA  uint64 = 0x2000
		usedGPA   uint64 = 0x3000
		hdrGPA    uint64 = 0x100 // request header (16 bytes)
		idBufGPA  uint64 = 0x200 // 32-byte buffer for the ID string
		statusGPA uint64 = 0x300 // 1-byte status
	)

	// Build a 3-descriptor chain:
	//   desc[0] -> header (read-only by device, len=16)
	//   desc[1] -> id buffer (writable by device, len=32)
	//   desc[2] -> status byte (writable, len=1)
	writeDesc := func(idx uint16, addr uint64, length uint32, flags uint16, next uint16) {
		o := descGPA + uint64(idx)*16
		binary.LittleEndian.PutUint64(mem[o:], addr)
		binary.LittleEndian.PutUint32(mem[o+8:], length)
		binary.LittleEndian.PutUint16(mem[o+12:], flags)
		binary.LittleEndian.PutUint16(mem[o+14:], next)
	}
	writeDesc(0, hdrGPA, 16, VirtioDescFNext, 1)
	writeDesc(1, idBufGPA, 32, VirtioDescFNext|VirtioDescFWrite, 2)
	writeDesc(2, statusGPA, 1, VirtioDescFWrite, 0)

	// Avail ring: flags=0, idx=1, ring[0]=0 (head descriptor index).
	binary.LittleEndian.PutUint16(mem[availGPA+2:], 1) // idx = 1
	binary.LittleEndian.PutUint16(mem[availGPA+4:], 0) // ring[0] = 0

	// Write request header: type=GET_ID (8), reserved=0, sector=0.
	binary.LittleEndian.PutUint32(mem[hdrGPA:], virtioBlkTGetID)
	// (rest already zero)

	// Configure the device via MMIO writes (matches what Linux does in
	// virtio_mmio.c during probe).
	w := func(off uint64, v uint32) { d.WriteMMIO(0xD0000000+off, 4, v) }
	w(VirtioMmioDeviceFeaturesSel, 1)
	w(VirtioMmioDriverFeaturesSel, 1)
	w(VirtioMmioDriverFeatures, 1) // accept VIRTIO_F_VERSION_1 (bit 32)
	w(VirtioMmioStatus, VirtioStatusAck|VirtioStatusDriver|VirtioStatusFeaturesOk|VirtioStatusDriverOk)
	w(VirtioMmioQueueSel, 0)
	w(VirtioMmioQueueNum, 256)
	w(VirtioMmioQueueDescLow, uint32(descGPA))
	w(VirtioMmioQueueDescHigh, 0)
	w(VirtioMmioQueueDriverLow, uint32(availGPA))
	w(VirtioMmioQueueDriverHigh, 0)
	w(VirtioMmioQueueDeviceLow, uint32(usedGPA))
	w(VirtioMmioQueueDeviceHigh, 0)
	w(VirtioMmioQueueReady, 1)

	// Kick: writing the queue index to QueueNotify drains the avail ring.
	w(VirtioMmioQueueNotify, 0)

	// Status byte should be VIRTIO_BLK_S_OK (0).
	if mem[statusGPA] != virtioBlkSOk {
		t.Errorf("status byte = %d; want 0 (OK)", mem[statusGPA])
	}

	// Used ring: idx should be 1, ring[0].id = 0 (head), .len = id_len+1.
	usedIdx := binary.LittleEndian.Uint16(mem[usedGPA+2:])
	if usedIdx != 1 {
		t.Errorf("used.idx = %d; want 1", usedIdx)
	}
	entryID := binary.LittleEndian.Uint32(mem[usedGPA+4:])
	entryLen := binary.LittleEndian.Uint32(mem[usedGPA+8:])
	if entryID != 0 {
		t.Errorf("used[0].id = %d; want 0", entryID)
	}
	// "gocracker" is 9 bytes; +1 for status = 10.
	if entryLen != 10 {
		t.Errorf("used[0].len = %d; want 10 (9 ID bytes + 1 status)", entryLen)
	}

	// ID buffer should contain "gocracker" followed by zeros.
	if !bytes.HasPrefix(mem[idBufGPA:idBufGPA+32], []byte("gocracker")) {
		t.Errorf("ID buffer = %q; want prefix \"gocracker\"", mem[idBufGPA:idBufGPA+9])
	}

	// Interrupt should have been raised exactly once.
	if irqCount != 1 {
		t.Errorf("raiseIRQ called %d times; want 1", irqCount)
	}

	// InterruptStatus should have the vring bit set.
	if v := d.ReadMMIO(0xD0000060, 4); v&VirtioInterruptVring == 0 {
		t.Errorf("InterruptStatus = %#x; want vring bit (%#x) set", v, VirtioInterruptVring)
	}
}

// TestVirtioBlkRead drives a T_IN (read) request and verifies that the
// requested sector ends up in the guest's buffer.
func TestVirtioBlkRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rootfs.img")
	// 4 sectors of distinct bytes: sector 0 = 'A', sector 1 = 'B', etc.
	img := make([]byte, 4*512)
	for s := 0; s < 4; s++ {
		for i := 0; i < 512; i++ {
			img[s*512+i] = byte('A' + s)
		}
	}
	if err := os.WriteFile(path, img, 0o644); err != nil {
		t.Fatalf("write tmp image: %v", err)
	}
	mem := make([]byte, 4*4096)
	d, err := NewVirtioBlk(0xD0000000, mem, path, false, func() {})
	if err != nil {
		t.Fatalf("NewVirtioBlk: %v", err)
	}
	defer d.Close()

	const (
		descGPA   uint64 = 0x1000
		availGPA  uint64 = 0x2000
		usedGPA   uint64 = 0x3000
		hdrGPA    uint64 = 0x100
		dataGPA   uint64 = 0x400 // 512 bytes for the sector read
		statusGPA uint64 = 0x800
	)

	writeDesc := func(idx uint16, addr uint64, length uint32, flags uint16, next uint16) {
		o := descGPA + uint64(idx)*16
		binary.LittleEndian.PutUint64(mem[o:], addr)
		binary.LittleEndian.PutUint32(mem[o+8:], length)
		binary.LittleEndian.PutUint16(mem[o+12:], flags)
		binary.LittleEndian.PutUint16(mem[o+14:], next)
	}
	writeDesc(0, hdrGPA, 16, VirtioDescFNext, 1)
	writeDesc(1, dataGPA, 512, VirtioDescFNext|VirtioDescFWrite, 2)
	writeDesc(2, statusGPA, 1, VirtioDescFWrite, 0)

	binary.LittleEndian.PutUint16(mem[availGPA+2:], 1)
	binary.LittleEndian.PutUint16(mem[availGPA+4:], 0)

	// Request: type=T_IN (0), sector=2 ('C'-filled).
	binary.LittleEndian.PutUint32(mem[hdrGPA:], virtioBlkTIn)
	binary.LittleEndian.PutUint64(mem[hdrGPA+8:], 2)

	w := func(off uint64, v uint32) { d.WriteMMIO(0xD0000000+off, 4, v) }
	w(VirtioMmioQueueSel, 0)
	w(VirtioMmioQueueNum, 256)
	w(VirtioMmioQueueDescLow, uint32(descGPA))
	w(VirtioMmioQueueDriverLow, uint32(availGPA))
	w(VirtioMmioQueueDeviceLow, uint32(usedGPA))
	w(VirtioMmioQueueReady, 1)
	w(VirtioMmioQueueNotify, 0)

	if mem[statusGPA] != virtioBlkSOk {
		t.Errorf("status = %d; want 0", mem[statusGPA])
	}
	for i := uint64(0); i < 512; i++ {
		if mem[dataGPA+i] != 'C' {
			t.Errorf("data[%d] = %q; want 'C'", i, mem[dataGPA+i])
			break
		}
	}
}

// TestVirtioBlkWrite verifies that a T_OUT request makes it to the
// underlying file.
func TestVirtioBlkWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rootfs.img")
	if err := os.WriteFile(path, make([]byte, 4*512), 0o644); err != nil {
		t.Fatalf("write tmp image: %v", err)
	}
	mem := make([]byte, 4*4096)
	d, err := NewVirtioBlk(0xD0000000, mem, path, false, func() {})
	if err != nil {
		t.Fatalf("NewVirtioBlk: %v", err)
	}
	defer d.Close()

	const (
		descGPA   uint64 = 0x1000
		availGPA  uint64 = 0x2000
		usedGPA   uint64 = 0x3000
		hdrGPA    uint64 = 0x100
		dataGPA   uint64 = 0x400
		statusGPA uint64 = 0x800
	)

	// Fill the source buffer with 'Z's.
	for i := uint64(0); i < 512; i++ {
		mem[dataGPA+i] = 'Z'
	}

	writeDesc := func(idx uint16, addr uint64, length uint32, flags uint16, next uint16) {
		o := descGPA + uint64(idx)*16
		binary.LittleEndian.PutUint64(mem[o:], addr)
		binary.LittleEndian.PutUint32(mem[o+8:], length)
		binary.LittleEndian.PutUint16(mem[o+12:], flags)
		binary.LittleEndian.PutUint16(mem[o+14:], next)
	}
	writeDesc(0, hdrGPA, 16, VirtioDescFNext, 1)
	writeDesc(1, dataGPA, 512, VirtioDescFNext, 2) // device reads (no WRITE flag)
	writeDesc(2, statusGPA, 1, VirtioDescFWrite, 0)

	binary.LittleEndian.PutUint16(mem[availGPA+2:], 1)
	binary.LittleEndian.PutUint16(mem[availGPA+4:], 0)

	// Request: T_OUT to sector 1.
	binary.LittleEndian.PutUint32(mem[hdrGPA:], virtioBlkTOut)
	binary.LittleEndian.PutUint64(mem[hdrGPA+8:], 1)

	w := func(off uint64, v uint32) { d.WriteMMIO(0xD0000000+off, 4, v) }
	w(VirtioMmioQueueSel, 0)
	w(VirtioMmioQueueNum, 256)
	w(VirtioMmioQueueDescLow, uint32(descGPA))
	w(VirtioMmioQueueDriverLow, uint32(availGPA))
	w(VirtioMmioQueueDeviceLow, uint32(usedGPA))
	w(VirtioMmioQueueReady, 1)
	w(VirtioMmioQueueNotify, 0)

	if mem[statusGPA] != virtioBlkSOk {
		t.Fatalf("status = %d; want 0", mem[statusGPA])
	}
	// Verify the file got the write at sector 1 (offset 512).
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tmp image: %v", err)
	}
	for i := 0; i < 512; i++ {
		if got[512+i] != 'Z' {
			t.Errorf("sector 1 byte %d = %q; want 'Z'", i, got[512+i])
			break
		}
	}
}
