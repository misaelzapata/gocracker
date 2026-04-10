//go:build linux

package virtio

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func withDiscardSupportProbe(t *testing.T, supported bool) {
	t.Helper()
	prev := probeDiscardSupport
	probeDiscardSupport = func(string) bool { return supported }
	t.Cleanup(func() { probeDiscardSupport = prev })
}

// ---------- helpers ----------

// newTempDisk creates a temp file with the given size (in bytes) and returns
// its path. The caller should defer os.Remove(path).
func newTempDisk(t *testing.T, size int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "disk.raw")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(int64(size)); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	return path
}

// newBlkTestEnv creates a BlockDevice with a temp disk and returns the device,
// the guest memory, and the queue 0 set up for testing.
func newBlkTestEnv(t *testing.T, diskSizeBytes int, readOnly bool) (*BlockDevice, []byte, *Queue) {
	t.Helper()
	path := newTempDisk(t, diskSizeBytes)

	mem := make([]byte, 256*1024) // 256 KiB guest RAM
	blk, err := NewBlockDevice(mem, 0x10000, 10, path, readOnly, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blk.Close() })

	q := blk.Queue(0)
	q.Size = 256
	q.DescAddr = 0x0000
	q.DriverAddr = 0x1000
	q.DeviceAddr = 0x2000
	q.Ready = true

	return blk, mem, q
}

// processBlkRequest builds a block request descriptor chain in guest memory,
// submits it through queue 0, and returns the guest address of the status byte.
func processBlkRequest(
	t *testing.T,
	mem []byte, q *Queue, blk *BlockDevice,
	reqType uint32, sector uint64,
	dataPayload []byte, dataLen uint32, dataFlags uint16,
	descIdx uint16,
) uint64 {
	t.Helper()

	descBase := q.DescAddr
	availBase := q.DriverAddr

	hdrAddr := uint64(0x8000 + uint64(descIdx)*0x1000)
	dataAddr := hdrAddr + 256
	statusAddr := dataAddr + uint64(dataLen) + 64

	// Descriptor 0: request header (16 bytes, read-only for device)
	var hdr [16]byte
	binary.LittleEndian.PutUint32(hdr[0:], reqType)
	binary.LittleEndian.PutUint64(hdr[8:], sector)
	copy(mem[hdrAddr:], hdr[:])
	includeData := !(reqType == BlkTFlush && dataLen == 0 && dataPayload == nil)
	if includeData {
		writeDesc(mem, descBase, descIdx, hdrAddr, 16, DescFlagNext, descIdx+1)
		if dataPayload != nil {
			copy(mem[dataAddr:], dataPayload)
		}
		writeDesc(mem, descBase, descIdx+1, dataAddr, dataLen, DescFlagNext|dataFlags, descIdx+2)
		mem[statusAddr] = 0xFF // sentinel
		writeDesc(mem, descBase, descIdx+2, statusAddr, 1, DescFlagWrite, 0)
	} else {
		writeDesc(mem, descBase, descIdx, hdrAddr, 16, DescFlagNext, descIdx+1)
		mem[statusAddr] = 0xFF // sentinel
		writeDesc(mem, descBase, descIdx+1, statusAddr, 1, DescFlagWrite, 0)
	}

	availIdx := binary.LittleEndian.Uint16(mem[availBase+2:])
	writeAvailEntry(mem, availBase, availIdx, descIdx)
	blk.HandleQueue(0, q)
	return statusAddr
}

// ---------- tests ----------

func TestBlkDeviceID(t *testing.T) {
	path := newTempDisk(t, 512)
	mem := make([]byte, 64*1024)
	blk, err := NewBlockDevice(mem, 0x10000, 10, path, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blk.Close()

	if blk.DeviceID() != DeviceIDBlock {
		t.Fatalf("DeviceID: got %d, want %d", blk.DeviceID(), DeviceIDBlock)
	}
}

func TestBlkConfigCapacity(t *testing.T) {
	const diskSize = 4096
	path := newTempDisk(t, diskSize)
	mem := make([]byte, 64*1024)
	blk, err := NewBlockDevice(mem, 0x10000, 10, path, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blk.Close()

	cfg := blk.ConfigBytes()
	capacity := binary.LittleEndian.Uint64(cfg[0:8])
	wantSectors := uint64(diskSize / 512)
	if capacity != wantSectors {
		t.Fatalf("capacity: got %d sectors, want %d", capacity, wantSectors)
	}
}

func TestBlkConfigSegMax(t *testing.T) {
	path := newTempDisk(t, 4096)
	mem := make([]byte, 64*1024)
	blk, err := NewBlockDevice(mem, 0x10000, 10, path, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blk.Close()

	cfg := blk.ConfigBytes()
	if len(cfg) < 16 {
		t.Fatalf("ConfigBytes too short: %d bytes", len(cfg))
	}
	segMax := binary.LittleEndian.Uint32(cfg[12:16])
	if segMax != 128 {
		t.Fatalf("seg_max: got %d, want 128", segMax)
	}
}

func TestBlkFeaturesReadOnly(t *testing.T) {
	withDiscardSupportProbe(t, true)
	path := newTempDisk(t, 512)
	mem := make([]byte, 64*1024)
	blk, err := NewBlockDevice(mem, 0x10000, 10, path, true, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blk.Close()

	feat := blk.DeviceFeatures()
	if feat&BlkFeatureRO == 0 {
		t.Fatal("read-only disk should advertise BlkFeatureRO")
	}
	if feat&BlkFeatureFlush == 0 {
		t.Fatal("should advertise BlkFeatureFlush")
	}
}

func TestBlkFeaturesReadWrite(t *testing.T) {
	withDiscardSupportProbe(t, true)
	path := newTempDisk(t, 512)
	mem := make([]byte, 64*1024)
	blk, err := NewBlockDevice(mem, 0x10000, 10, path, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blk.Close()

	feat := blk.DeviceFeatures()
	if feat&BlkFeatureRO != 0 {
		t.Fatal("writable disk should not advertise BlkFeatureRO")
	}
	if feat&BlkFeatureDiscard == 0 {
		t.Fatal("writable disk with discard support should advertise BlkFeatureDiscard")
	}
}

func TestBlkFeaturesSkipDiscardWhenUnsupported(t *testing.T) {
	withDiscardSupportProbe(t, false)
	path := newTempDisk(t, 512)
	mem := make([]byte, 64*1024)
	blk, err := NewBlockDevice(mem, 0x10000, 10, path, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blk.Close()

	feat := blk.DeviceFeatures()
	if feat&BlkFeatureDiscard != 0 {
		t.Fatal("writable disk without discard support must not advertise BlkFeatureDiscard")
	}
}

func TestBlkWriteAndRead(t *testing.T) {
	const diskSize = 8192 // 16 sectors
	blk, mem, q := newBlkTestEnv(t, diskSize, false)

	// Write "DEADBEEF" repeated to sector 0
	pattern := bytes.Repeat([]byte("DEADBEEF"), 64) // 512 bytes
	statusAddr := processBlkRequest(t, mem, q, blk,
		BlkTOut, 0,
		pattern, 512, 0, // read-only data desc (no DescFlagWrite)
		0,
	)
	if mem[statusAddr] != BlkSOK {
		t.Fatalf("write status: got %d, want %d (OK)", mem[statusAddr], BlkSOK)
	}

	// Read sector 0 back
	statusAddr = processBlkRequest(t, mem, q, blk,
		BlkTIn, 0,
		nil, 512, DescFlagWrite, // device-writable data desc
		3,
	)
	if mem[statusAddr] != BlkSOK {
		t.Fatalf("read status: got %d, want %d (OK)", mem[statusAddr], BlkSOK)
	}

	// Verify data in guest memory at the data descriptor's address
	dataAddr := uint64(0x8000 + 3*0x1000 + 256)
	readBack := mem[dataAddr : dataAddr+512]
	if !bytes.Equal(readBack, pattern) {
		t.Fatalf("read-back data mismatch: first 16 bytes got %x, want %x", readBack[:16], pattern[:16])
	}
}

func TestBlkWriteMultipleSectors(t *testing.T) {
	const diskSize = 16384 // 32 sectors
	blk, mem, q := newBlkTestEnv(t, diskSize, false)

	// Write 1024 bytes (2 sectors) starting at sector 2
	pattern := bytes.Repeat([]byte("ABCD"), 256) // 1024 bytes
	statusAddr := processBlkRequest(t, mem, q, blk,
		BlkTOut, 2,
		pattern, 1024, 0,
		0,
	)
	if mem[statusAddr] != BlkSOK {
		t.Fatalf("write status: got %d, want %d (OK)", mem[statusAddr], BlkSOK)
	}

	// Read back from sector 2
	statusAddr = processBlkRequest(t, mem, q, blk,
		BlkTIn, 2,
		nil, 1024, DescFlagWrite,
		3,
	)
	if mem[statusAddr] != BlkSOK {
		t.Fatalf("read status: got %d, want %d (OK)", mem[statusAddr], BlkSOK)
	}

	dataAddr := uint64(0x8000 + 3*0x1000 + 256)
	readBack := mem[dataAddr : dataAddr+1024]
	if !bytes.Equal(readBack, pattern) {
		t.Fatal("multi-sector read-back mismatch")
	}
}

func TestBlkFlush(t *testing.T) {
	const diskSize = 4096
	blk, mem, q := newBlkTestEnv(t, diskSize, false)

	statusAddr := processBlkRequest(t, mem, q, blk,
		BlkTFlush, 0,
		nil, 0, DescFlagWrite,
		0,
	)
	if mem[statusAddr] != BlkSOK {
		t.Fatalf("flush status: got %d, want %d (OK)", mem[statusAddr], BlkSOK)
	}
}

func TestBlkGetID(t *testing.T) {
	const diskSize = 4096
	blk, mem, q := newBlkTestEnv(t, diskSize, false)

	statusAddr := processBlkRequest(t, mem, q, blk,
		BlkTGetID, 0,
		nil, 64, DescFlagWrite,
		0,
	)
	if mem[statusAddr] != BlkSOK {
		t.Fatalf("getid status: got %d, want %d (OK)", mem[statusAddr], BlkSOK)
	}

	dataAddr := uint64(0x8000 + 0*0x1000 + 256)
	idBytes := mem[dataAddr : dataAddr+10]
	if string(idBytes) != "gocracker\x00" {
		t.Fatalf("GetID: got %q, want %q", idBytes, "gocracker\x00")
	}
}

func TestBlkWriteReadOnlyReturnsError(t *testing.T) {
	const diskSize = 4096
	blk, mem, q := newBlkTestEnv(t, diskSize, true)

	data := make([]byte, 512)
	statusAddr := processBlkRequest(t, mem, q, blk,
		BlkTOut, 0,
		data, 512, 0,
		0,
	)
	if mem[statusAddr] != BlkSIOErr {
		t.Fatalf("write on read-only: got status %d, want %d (IOErr)", mem[statusAddr], BlkSIOErr)
	}
}

func TestBlkUnsupportedRequestType(t *testing.T) {
	const diskSize = 4096
	blk, mem, q := newBlkTestEnv(t, diskSize, false)

	statusAddr := processBlkRequest(t, mem, q, blk,
		99, 0, // unknown request type
		nil, 64, DescFlagWrite,
		0,
	)
	if mem[statusAddr] != BlkSUnsup {
		t.Fatalf("unsupported req: got status %d, want %d (Unsup)", mem[statusAddr], BlkSUnsup)
	}
}

func TestBlkHandleQueueIgnoresNonZero(t *testing.T) {
	const diskSize = 4096
	blk, _, _ := newBlkTestEnv(t, diskSize, false)

	// HandleQueue should silently return for queue indices other than 0.
	// We pass a queue with no avail entries, so even if idx==0 it would
	// return immediately. The key check is: no panic.
	q := blk.Queue(1)
	q.Ready = true
	blk.HandleQueue(1, q)
}

func TestBlkOpenInvalidPath(t *testing.T) {
	mem := make([]byte, 4096)
	_, err := NewBlockDevice(mem, 0x10000, 10, "/nonexistent/path/disk.raw", false, nil, nil)
	if err == nil {
		t.Fatal("expected error for invalid image path")
	}
}

func TestBlkString(t *testing.T) {
	const diskSize = 2048
	path := newTempDisk(t, diskSize)
	mem := make([]byte, 64*1024)
	blk, err := NewBlockDevice(mem, 0x10000, 10, path, true, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blk.Close()

	s := blk.String()
	if s == "" {
		t.Fatal("String() should return a non-empty description")
	}
}

func TestBlkReadFromZeroDisk(t *testing.T) {
	const diskSize = 4096
	blk, mem, q := newBlkTestEnv(t, diskSize, false)

	// Read sector 0 from a fresh zero-filled disk
	statusAddr := processBlkRequest(t, mem, q, blk,
		BlkTIn, 0,
		nil, 512, DescFlagWrite,
		0,
	)
	if mem[statusAddr] != BlkSOK {
		t.Fatalf("read status: got %d, want %d (OK)", mem[statusAddr], BlkSOK)
	}

	dataAddr := uint64(0x8000 + 0*0x1000 + 256)
	readBack := mem[dataAddr : dataAddr+512]
	expected := make([]byte, 512) // all zeros
	if !bytes.Equal(readBack, expected) {
		t.Fatal("expected all zeros from fresh disk")
	}
}

func TestBlkMMIOReadDeviceID(t *testing.T) {
	const diskSize = 4096
	blk, _, _ := newBlkTestEnv(t, diskSize, false)

	// Test reading MMIO registers through the Transport
	magic := blk.Transport.Read(RegMagic, 4)
	if magic != 0x74726976 {
		t.Fatalf("blk MMIO magic: got %#x, want 0x74726976", magic)
	}
	devID := blk.Transport.Read(RegDeviceID, 4)
	if devID != DeviceIDBlock {
		t.Fatalf("blk MMIO device ID: got %d, want %d", devID, DeviceIDBlock)
	}
}

func TestBlkHandleQueueRejectsOutOfBoundsWrite(t *testing.T) {
	const diskSize = 4096 // 8 sectors
	blk, mem, q := newBlkTestEnv(t, diskSize, false)

	fi, err := blk.file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	initialSize := fi.Size()

	data := bytes.Repeat([]byte("A"), 512)
	statusAddr := processBlkRequest(t, mem, q, blk,
		BlkTOut, uint64(diskSize/blkSectorSize),
		data, 512, 0,
		0,
	)

	if mem[statusAddr] != 0xFF {
		t.Fatalf("out-of-bounds status byte should be untouched, got %d", mem[statusAddr])
	}
	if got := binary.LittleEndian.Uint16(mem[q.DeviceAddr+2:]); got != 1 {
		t.Fatalf("used ring idx: got %d, want 1", got)
	}

	fi, err = blk.file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != initialSize {
		t.Fatalf("disk size changed after out-of-bounds write: got %d, want %d", fi.Size(), initialSize)
	}
}

func TestBlkHandleQueueFlushWithTwoDescriptors(t *testing.T) {
	const diskSize = 4096
	blk, mem, q := newBlkTestEnv(t, diskSize, false)

	statusAddr := processBlkRequest(t, mem, q, blk,
		BlkTFlush, 0,
		nil, 0, 0,
		0,
	)
	if mem[statusAddr] != BlkSOK {
		t.Fatalf("flush status: got %d, want %d (OK)", mem[statusAddr], BlkSOK)
	}
	if got := binary.LittleEndian.Uint16(mem[q.DeviceAddr+2:]); got != 1 {
		t.Fatalf("used ring idx: got %d, want 1", got)
	}
}

func TestBlkDiscardPunchesHoleAndReadsBackZeroes(t *testing.T) {
	withDiscardSupportProbe(t, true)
	const diskSize = 8192
	blk, mem, q := newBlkTestEnv(t, diskSize, false)

	pattern := bytes.Repeat([]byte("ABCD"), 128)
	statusAddr := processBlkRequest(t, mem, q, blk, BlkTOut, 0, pattern, 512, 0, 0)
	if mem[statusAddr] != BlkSOK {
		t.Fatalf("write status: got %d, want %d (OK)", mem[statusAddr], BlkSOK)
	}

	var discard [blkDiscardSegmentBytes]byte
	binary.LittleEndian.PutUint64(discard[0:8], 0)
	binary.LittleEndian.PutUint32(discard[8:12], 1)
	statusAddr = processBlkRequest(t, mem, q, blk, BlkTDiscard, 0, discard[:], uint32(len(discard)), 0, 3)
	if mem[statusAddr] != BlkSOK {
		t.Fatalf("discard status: got %d, want %d (OK)", mem[statusAddr], BlkSOK)
	}

	statusAddr = processBlkRequest(t, mem, q, blk, BlkTIn, 0, nil, 512, DescFlagWrite, 6)
	if mem[statusAddr] != BlkSOK {
		t.Fatalf("read status: got %d, want %d (OK)", mem[statusAddr], BlkSOK)
	}

	dataAddr := uint64(0x8000 + 6*0x1000 + 256)
	readBack := mem[dataAddr : dataAddr+512]
	if !bytes.Equal(readBack, make([]byte, 512)) {
		t.Fatal("discarded sector did not read back as zeroes")
	}
}

func TestBlkDiscardReturnsUnsupWhenFeatureDisabled(t *testing.T) {
	withDiscardSupportProbe(t, false)
	const diskSize = 4096
	blk, mem, q := newBlkTestEnv(t, diskSize, false)

	var discard [blkDiscardSegmentBytes]byte
	binary.LittleEndian.PutUint32(discard[8:12], 1)
	statusAddr := processBlkRequest(t, mem, q, blk, BlkTDiscard, 0, discard[:], uint32(len(discard)), 0, 0)
	if mem[statusAddr] != BlkSUnsup {
		t.Fatalf("discard status: got %d, want %d (Unsup)", mem[statusAddr], BlkSUnsup)
	}
}

func TestBlkDiscardRejectsMisalignedSegmentBuffer(t *testing.T) {
	withDiscardSupportProbe(t, true)
	const diskSize = 4096
	blk, mem, q := newBlkTestEnv(t, diskSize, false)

	statusAddr := processBlkRequest(t, mem, q, blk, BlkTDiscard, 0, []byte{1, 2, 3}, 3, 0, 0)
	if mem[statusAddr] != 0xFF {
		t.Fatalf("invalid discard status byte should be untouched, got %d", mem[statusAddr])
	}
}
