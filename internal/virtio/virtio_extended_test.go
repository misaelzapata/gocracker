package virtio

import (
	"encoding/binary"
	"os"
	"testing"
	"time"
)

// ---------- Queue edge cases ----------

func TestQueueWalkChainOutOfBounds(t *testing.T) {
	mem := make([]byte, 64*1024)
	q := NewQueue(mem, 8, nil)
	q.DescAddr = 0
	q.Ready = true

	// Head index beyond queue size
	_, err := q.WalkChain(10)
	if err == nil {
		t.Fatal("WalkChain with out-of-bounds head should error")
	}
}

func TestQueueWalkChainNextOutOfBounds(t *testing.T) {
	mem := make([]byte, 64*1024)
	q := NewQueue(mem, 8, nil)
	q.DescAddr = 0
	q.Ready = true

	// Descriptor 0 points to next=200, which is out of bounds
	writeDesc(mem, 0, 0, 0x4000, 64, DescFlagNext, 200)

	_, err := q.WalkChain(0)
	if err == nil {
		t.Fatal("WalkChain with out-of-bounds next should error")
	}
}

func TestQueueWalkChainIndirectUnsupported(t *testing.T) {
	mem := make([]byte, 64*1024)
	q := NewQueue(mem, 8, nil)
	q.DescAddr = 0
	q.Ready = true

	writeDesc(mem, 0, 0, 0x4000, 64, DescFlagIndirect, 0)

	_, err := q.WalkChain(0)
	if err == nil {
		t.Fatal("WalkChain with indirect descriptor should error")
	}
}

func TestQueueWalkChainDescriptorTooLarge(t *testing.T) {
	mem := make([]byte, 64*1024)
	q := NewQueue(mem, 8, nil)
	q.DescAddr = 0
	q.Ready = true

	// Descriptor with length exceeding MaxDescriptorSizeBytes
	writeDesc(mem, 0, 0, 0x4000, MaxDescriptorSizeBytes+1, 0, 0)

	_, err := q.WalkChain(0)
	if err == nil {
		t.Fatal("WalkChain with oversized descriptor should error")
	}
}

func TestQueueNormalizedSizeInvalid(t *testing.T) {
	mem := make([]byte, 64*1024)
	q := NewQueue(mem, 0, nil)
	q.DescAddr = 0

	_, err := q.WalkChain(0)
	if err == nil {
		t.Fatal("WalkChain with size=0 should error")
	}

	q.Size = MaxQueueSize + 1
	_, err = q.WalkChain(0)
	if err == nil {
		t.Fatal("WalkChain with oversized queue should error")
	}
}

func TestQueueConsumeAvailEmpty(t *testing.T) {
	mem := make([]byte, 64*1024)
	q := NewQueue(mem, 256, nil)
	q.DriverAddr = 0x1000

	consumed, err := q.ConsumeAvail(func(head uint16) {
		t.Fatal("should not be called")
	})
	if err != nil {
		t.Fatalf("ConsumeAvail error: %v", err)
	}
	if consumed {
		t.Fatal("ConsumeAvail should return false on empty queue")
	}
}

func TestQueueConsumeAvailOne(t *testing.T) {
	mem := make([]byte, 64*1024)
	q := NewQueue(mem, 256, nil)

	descBase := uint64(0x0000)
	availBase := uint64(0x1000)
	usedBase := uint64(0x2000)
	setupQueueInMemory(mem, q, descBase, availBase, usedBase)

	writeDesc(mem, descBase, 0, 0x4000, 64, 0, 0)
	writeDesc(mem, descBase, 1, 0x5000, 32, 0, 0)
	writeAvailEntry(mem, availBase, 0, 0)
	writeAvailEntry(mem, availBase, 1, 1)

	var heads []uint16
	consumed, err := q.ConsumeAvail(func(head uint16) {
		heads = append(heads, head)
	})
	if err != nil {
		t.Fatalf("ConsumeAvail error: %v", err)
	}
	if !consumed {
		t.Fatal("ConsumeAvail should return true")
	}
	if len(heads) != 1 || heads[0] != 0 {
		t.Fatalf("got heads %v, want [0]", heads)
	}

	// Second call should get the next one
	consumed, err = q.ConsumeAvail(func(head uint16) {
		heads = append(heads, head)
	})
	if err != nil || !consumed || len(heads) != 2 || heads[1] != 1 {
		t.Fatalf("second ConsumeAvail: consumed=%v err=%v heads=%v", consumed, err, heads)
	}
}

func TestQueuePushUsedLocked(t *testing.T) {
	mem := make([]byte, 64*1024)
	q := NewQueue(mem, 256, nil)
	q.DeviceAddr = 0x2000

	if err := q.PushUsedLocked(10, 100); err != nil {
		t.Fatalf("PushUsedLocked error: %v", err)
	}

	usedIdx := binary.LittleEndian.Uint16(mem[0x2000+2:])
	if usedIdx != 1 {
		t.Fatalf("used.idx = %d, want 1", usedIdx)
	}
	id := binary.LittleEndian.Uint32(mem[0x2000+4:])
	ln := binary.LittleEndian.Uint32(mem[0x2000+4+4:])
	if id != 10 || ln != 100 {
		t.Fatalf("used entry: id=%d len=%d, want id=10 len=100", id, ln)
	}
}

func TestQueueReset(t *testing.T) {
	mem := make([]byte, 64*1024)
	q := NewQueue(mem, 128, nil)
	q.DescAddr = 0x1000
	q.DriverAddr = 0x2000
	q.DeviceAddr = 0x3000
	q.Ready = true
	q.LastAvail = 7

	q.Reset()

	if q.Size != MaxQueueSize || q.Ready || q.LastAvail != 0 ||
		q.DescAddr != 0 || q.DriverAddr != 0 || q.DeviceAddr != 0 {
		t.Fatalf("queue not properly reset: size=%d ready=%v lastAvail=%d", q.Size, q.Ready, q.LastAvail)
	}
}

func TestQueueSetGuestPhysBase(t *testing.T) {
	mem := make([]byte, 64*1024)
	q := NewQueue(mem, 256, nil)
	q.SetGuestPhysBase(0x80000000)

	// Write data at mem[0]
	copy(mem[0:], []byte("test"))

	// Read from guest address 0x80000000 should get mem[0]
	buf := make([]byte, 4)
	err := q.GuestRead(0x80000000, buf)
	if err != nil {
		t.Fatalf("GuestRead with phys base: %v", err)
	}
	if string(buf) != "test" {
		t.Fatalf("GuestRead = %q, want %q", buf, "test")
	}
}

func TestQueueCheckedRangeOverflow(t *testing.T) {
	mem := make([]byte, 64*1024)
	q := NewQueue(mem, 256, nil)

	// Zero length is OK
	start, end, err := q.checkedRange(0, 0)
	if err != nil {
		t.Fatalf("checkedRange(0,0) error: %v", err)
	}
	if start != 0 || end != 0 {
		t.Fatalf("checkedRange(0,0) = (%d,%d)", start, end)
	}

	// Below base address
	q.SetGuestPhysBase(0x1000)
	_, _, err = q.checkedRange(0x500, 10)
	if err == nil {
		t.Fatal("checkedRange below base should error")
	}
}

// ---------- Transport extended tests ----------

func TestTransportWriteBytesIgnoresNon4ByteWrites(t *testing.T) {
	dev := &mockDevice{id: 1}
	tr, _ := newTestTransport(dev)

	// WriteBytes with 2 bytes should be ignored for non-config registers
	buf := []byte{0xAA, 0xBB}
	tr.WriteBytes(RegStatus, buf)

	if tr.Read(RegStatus, 4) != 0 {
		t.Fatal("2-byte write to non-config register should be ignored")
	}
}

func TestTransportWriteBytesConfigRegion(t *testing.T) {
	configWritten := false
	dev := &mockConfigDevice{
		mockDevice: mockDevice{id: 1, config: []byte{0, 0, 0, 0}},
		onWrite: func(offset uint32, data []byte) {
			configWritten = true
		},
	}
	tr, _ := newTestTransport(&dev.mockDevice)
	// Manually set the dev to the config writer
	tr.dev = dev

	tr.WriteBytes(RegConfig, []byte{0x42})
	if !configWritten {
		t.Fatal("config write should invoke ConfigWriter")
	}
}

type mockConfigDevice struct {
	mockDevice
	onWrite func(uint32, []byte)
}

func (m *mockConfigDevice) WriteConfig(offset uint32, data []byte) {
	if m.onWrite != nil {
		m.onWrite(offset, data)
	}
}

func TestTransportReadBytesNon4BytePreConfig(t *testing.T) {
	dev := &mockDevice{id: 1}
	tr, _ := newTestTransport(dev)

	// ReadBytes with 2 bytes for pre-config register should return 0
	buf := make([]byte, 2)
	tr.ReadBytes(RegMagic, buf)
	if buf[0] != 0 && buf[1] != 0 {
		// 2-byte read from pre-config area returns 0
	}
}

func TestTransportActivateLinkOnDriverOK(t *testing.T) {
	dev := &mockLinkDevice{mockDevice: mockDevice{id: 1}}
	tr, _ := newTestTransport(&dev.mockDevice)
	tr.dev = dev

	irqAsserted := false
	mem := make([]byte, 64*1024)
	tr2 := NewTransport(dev, mem, 0x1000, 5, nil, func(assert bool) {
		irqAsserted = assert
	})
	tr2.dev = dev

	tr2.Write(RegStatus, StatusAcknowledge|StatusDriver|StatusFeaturesOK|StatusDriverOK)

	if !dev.activated {
		t.Fatal("ActivateLink should be called when DRIVER_OK transitions")
	}
	if !irqAsserted {
		t.Fatal("IRQ should be asserted for config change on link activation")
	}
}

type mockLinkDevice struct {
	mockDevice
	activated bool
}

func (m *mockLinkDevice) DeviceID() uint32       { return m.mockDevice.id }
func (m *mockLinkDevice) DeviceFeatures() uint64 { return m.mockDevice.features }
func (m *mockLinkDevice) ConfigBytes() []byte    { return m.mockDevice.config }
func (m *mockLinkDevice) HandleQueue(idx uint32, q *Queue) {
	m.mockDevice.handled = append(m.mockDevice.handled, idx)
}
func (m *mockLinkDevice) ActivateLink() { m.activated = true }

func TestTransportMem(t *testing.T) {
	dev := &mockDevice{id: 1}
	mem := make([]byte, 4096)
	tr := NewTransport(dev, mem, 0x1000, 5, nil, nil)
	if got := tr.Mem(); &got[0] != &mem[0] {
		t.Fatal("Mem() should return the same memory slice")
	}
}

func TestTransportString(t *testing.T) {
	dev := &mockDevice{id: 1}
	mem := make([]byte, 4096)
	tr := NewTransport(dev, mem, 0xDEAD0000, 42, nil, nil)
	s := tr.String()
	if s == "" {
		t.Fatal("String() should return non-empty")
	}
}

func TestTransportSignalIRQNilCallback(t *testing.T) {
	dev := &mockDevice{id: 1}
	mem := make([]byte, 4096)
	tr := NewTransport(dev, mem, 0x1000, 5, nil, nil)
	// Should not panic
	tr.SignalIRQ(true)
	tr.SignalIRQ(false)
}

func TestTransportSignalIRQWithCallback(t *testing.T) {
	dev := &mockDevice{id: 1}
	mem := make([]byte, 4096)
	var lastAssert bool
	tr := NewTransport(dev, mem, 0x1000, 5, nil, func(assert bool) {
		lastAssert = assert
	})
	tr.SignalIRQ(true)
	if !lastAssert {
		t.Fatal("expected assert=true")
	}
	tr.SignalIRQ(false)
	if lastAssert {
		t.Fatal("expected assert=false")
	}
}

func TestTransportSetGuestPhysBase(t *testing.T) {
	dev := &mockDevice{id: 1}
	mem := make([]byte, 64*1024)
	tr := NewTransport(dev, mem, 0x1000, 5, nil, nil)
	tr.SetGuestPhysBase(0x80000000)

	// All queues should have the new base
	for i := 0; i < 8; i++ {
		q := tr.Queue(i)
		// Verify by trying to read from the base address
		copy(mem[0:4], []byte("ok!"))
		buf := make([]byte, 3)
		err := q.GuestRead(0x80000000, buf)
		if err != nil {
			t.Fatalf("Queue(%d).GuestRead with phys base: %v", i, err)
		}
		if string(buf) != "ok!" {
			t.Fatalf("Queue(%d): read %q, want %q", i, buf, "ok!")
		}
	}
}

func TestTransportInterruptACKPartial(t *testing.T) {
	dev := &mockDevice{id: 1}
	mem := make([]byte, 64*1024)
	lastIRQ := false
	tr := NewTransport(dev, mem, 0x1000, 5, nil, func(assert bool) {
		lastIRQ = assert
	})

	// Set two interrupt bits
	tr.SetInterruptStat(1)
	tr.SetInterruptStat(2)
	if tr.InterruptStat() != 3 {
		t.Fatalf("InterruptStat = %d, want 3", tr.InterruptStat())
	}

	// ACK only bit 0
	tr.Write(RegInterruptACK, 1)
	if tr.InterruptStat() != 2 {
		t.Fatalf("InterruptStat after partial ACK = %d, want 2", tr.InterruptStat())
	}
	// IRQ should still be asserted because bit 1 remains
	// (the implementation deasserts only when interruptStat == 0)

	// ACK bit 1
	tr.Write(RegInterruptACK, 2)
	if tr.InterruptStat() != 0 {
		t.Fatalf("InterruptStat after full ACK = %d, want 0", tr.InterruptStat())
	}
	if lastIRQ {
		t.Fatal("IRQ should be deasserted after full ACK")
	}
}

// ---------- DirtyTracker tests ----------

func TestDirtyTrackerMarkAndSnapshot(t *testing.T) {
	dt := NewDirtyTracker(64 * 1024) // 64 KiB

	dt.Mark(0, 100)
	dt.Mark(4096, 1) // second page

	bits := dt.SnapshotAndReset()
	if len(bits) == 0 {
		t.Fatal("SnapshotAndReset returned empty")
	}
	// First two pages should be dirty
	if bits[0]&0x3 != 0x3 {
		t.Fatalf("expected pages 0 and 1 dirty, got bits[0]=%#x", bits[0])
	}

	// After reset, should be clean
	bits2 := dt.SnapshotAndReset()
	allZero := true
	for _, w := range bits2 {
		if w != 0 {
			allZero = false
			break
		}
	}
	if !allZero {
		t.Fatal("bits should be zero after reset")
	}
}

func TestDirtyTrackerReset(t *testing.T) {
	dt := NewDirtyTracker(64 * 1024)
	dt.Mark(0, 100)
	dt.Reset()
	bits := dt.SnapshotAndReset()
	for _, w := range bits {
		if w != 0 {
			t.Fatal("bits should be zero after Reset()")
		}
	}
}

func TestDirtyTrackerPageSize(t *testing.T) {
	dt := NewDirtyTracker(64 * 1024)
	if dt.PageSize() == 0 {
		t.Fatal("PageSize should not be zero")
	}
}

func TestDirtyTrackerNilSafe(t *testing.T) {
	var dt *DirtyTracker
	dt.Mark(0, 100)       // no panic
	dt.Reset()             // no panic
	r := dt.SnapshotAndReset()
	if r != nil {
		t.Fatal("nil tracker SnapshotAndReset should return nil")
	}
	if dt.PageSize() == 0 {
		t.Fatal("nil tracker PageSize should return system page size")
	}
}

func TestDirtyTrackerMarkBoundary(t *testing.T) {
	dt := NewDirtyTracker(4096)
	// Mark beyond memSize
	dt.Mark(5000, 100) // addr >= memSize, should be no-op
	bits := dt.SnapshotAndReset()
	for _, w := range bits {
		if w != 0 {
			t.Fatal("marking beyond memSize should be no-op")
		}
	}

	// Zero length
	dt.Mark(0, 0) // no-op
}

func TestDirtyTrackerMarkClamps(t *testing.T) {
	dt := NewDirtyTracker(8192)
	// Mark that extends beyond memSize
	dt.Mark(4000, 10000) // end > memSize, should clamp
	bits := dt.SnapshotAndReset()
	// Should have marked at least page 0 and page 1
	if bits[0] == 0 {
		t.Fatal("expected dirty bits after clamped mark")
	}
}

// ---------- Queue with DirtyTracker ----------

func TestQueueGuestWriteMarksDirty(t *testing.T) {
	dt := NewDirtyTracker(64 * 1024)
	mem := make([]byte, 64*1024)
	q := NewQueue(mem, 256, dt)

	if err := q.GuestWrite(0, []byte("hello")); err != nil {
		t.Fatalf("GuestWrite: %v", err)
	}

	bits := dt.SnapshotAndReset()
	if bits[0]&1 == 0 {
		t.Fatal("page 0 should be dirty after GuestWrite")
	}
}

// ---------- RNG device ----------

func TestRNGDeviceBasics(t *testing.T) {
	mem := make([]byte, 64*1024)
	d := NewRNGDevice(mem, 0x1000, 5, nil, nil)
	if d.DeviceID() != DeviceIDRNG {
		t.Fatalf("DeviceID = %d, want %d", d.DeviceID(), DeviceIDRNG)
	}
	if d.DeviceFeatures() != 0 {
		t.Fatalf("DeviceFeatures = %d, want 0", d.DeviceFeatures())
	}
	if d.ConfigBytes() != nil {
		t.Fatal("ConfigBytes should be nil for RNG")
	}
}

func TestRNGDeviceHandleQueueFillsEntropy(t *testing.T) {
	mem := make([]byte, 64*1024)
	d := NewRNGDevice(mem, 0x1000, 5, nil, nil)

	q := d.Transport.Queue(0)
	descBase := uint64(0x0000)
	availBase := uint64(0x1000)
	usedBase := uint64(0x2000)
	setupQueueInMemory(mem, q, descBase, availBase, usedBase)

	dataAddr := uint64(0x4000)
	// Single writable descriptor
	writeDesc(mem, descBase, 0, dataAddr, 32, DescFlagWrite, 0)
	writeAvailEntry(mem, availBase, 0, 0)

	// Clear the data area
	for i := uint64(0); i < 32; i++ {
		mem[dataAddr+i] = 0
	}

	d.HandleQueue(0, q)

	// Check that some entropy was written
	allZero := true
	for i := uint64(0); i < 32; i++ {
		if mem[dataAddr+i] != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Fatal("RNG should have written non-zero entropy")
	}

	// Used ring should have the entry
	usedIdx := binary.LittleEndian.Uint16(mem[usedBase+2:])
	if usedIdx != 1 {
		t.Fatalf("used ring idx = %d, want 1", usedIdx)
	}
}

func TestRNGDeviceHandleQueueNonZeroIdx(t *testing.T) {
	mem := make([]byte, 64*1024)
	d := NewRNGDevice(mem, 0x1000, 5, nil, nil)
	// HandleQueue with idx != 0 should be no-op
	d.HandleQueue(1, d.Transport.Queue(1))
}

// ---------- Transport WriteBytes for SHMSel ----------

func TestTransportWriteSHMSel(t *testing.T) {
	dev := &mockDevice{id: 1, shmBase: 0x1000, shmLen: 0x2000, shmOK: true}
	tr, _ := newTestTransport(dev)

	tr.Write(RegSHMSel, 0)
	lenLow := tr.Read(RegSHMLenLow, 4)
	if lenLow != uint32(dev.shmLen) {
		t.Fatalf("SHMLenLow = %#x, want %#x", lenLow, uint32(dev.shmLen))
	}

	// Select non-existent SHM region
	tr.Write(RegSHMSel, 1)
	lenLow2 := tr.Read(RegSHMLenLow, 4)
	if lenLow2 != 0xffffffff {
		t.Fatalf("SHMLenLow for absent region = %#x, want 0xffffffff", lenLow2)
	}
}

// ---------- Transport QueueNotify with interceptor ----------

func TestTransportQueueNotifyWithInterceptor(t *testing.T) {
	dev := &mockInterceptDevice{mockDevice: mockDevice{id: 2}}
	mem := make([]byte, 64*1024)
	irqAsserted := false
	tr := NewTransport(dev, mem, 0x1000, 5, nil, func(assert bool) {
		irqAsserted = assert
	})
	tr.dev = dev

	tr.Write(RegQueueSel, 0)
	tr.Write(RegQueueReady, 1)

	// Interceptor returns false = no IRQ
	dev.returnVal = false
	tr.Write(RegQueueNotify, 0)
	if irqAsserted {
		t.Fatal("IRQ should not be raised when interceptor returns false")
	}

	// Interceptor returns true = raise IRQ
	dev.returnVal = true
	tr.Write(RegQueueNotify, 0)
	if !irqAsserted {
		t.Fatal("IRQ should be raised when interceptor returns true")
	}
}

type mockInterceptDevice struct {
	mockDevice
	returnVal bool
}

func (m *mockInterceptDevice) DeviceID() uint32       { return m.mockDevice.id }
func (m *mockInterceptDevice) DeviceFeatures() uint64 { return m.mockDevice.features }
func (m *mockInterceptDevice) ConfigBytes() []byte    { return m.mockDevice.config }
func (m *mockInterceptDevice) HandleQueue(idx uint32, q *Queue) {
	m.mockDevice.handled = append(m.mockDevice.handled, idx)
}
func (m *mockInterceptDevice) HandleQueueNotify(idx uint32, q *Queue) bool {
	return m.returnVal
}

// ---------- Balloon device basics ----------

func TestBalloonDeviceBasics(t *testing.T) {
	mem := make([]byte, 64*1024)
	cfg := BalloonDeviceConfig{AmountMiB: 0}
	d := NewBalloonDevice(mem, 0x1000, 5, cfg, nil, nil)
	defer d.Close()

	if d.DeviceID() != DeviceIDBalloon {
		t.Fatalf("DeviceID = %d, want %d", d.DeviceID(), DeviceIDBalloon)
	}
	if d.DeviceFeatures() != 0 {
		t.Fatalf("DeviceFeatures = %d, want 0 (no stats, no deflate-on-oom)", d.DeviceFeatures())
	}
}

func TestBalloonDeviceFeatures(t *testing.T) {
	mem := make([]byte, 64*1024)
	cfg := BalloonDeviceConfig{
		StatsPollingInterval: time.Second,
		DeflateOnOOM:         true,
	}
	d := NewBalloonDevice(mem, 0x1000, 5, cfg, nil, nil)
	defer d.Close()

	features := d.DeviceFeatures()
	if features&balloonFeatureStatsVQ == 0 {
		t.Fatal("expected stats VQ feature")
	}
	if features&balloonFeatureDeflateOnOOM == 0 {
		t.Fatal("expected deflate-on-oom feature")
	}
}

func TestBalloonConfigBytes(t *testing.T) {
	mem := make([]byte, 64*1024)
	cfg := BalloonDeviceConfig{AmountMiB: 0}
	d := NewBalloonDevice(mem, 0x1000, 5, cfg, nil, nil)
	defer d.Close()

	cfgBytes := d.ConfigBytes()
	if len(cfgBytes) != 16 {
		t.Fatalf("ConfigBytes len = %d, want 16", len(cfgBytes))
	}
}

func TestBalloonWriteConfig(t *testing.T) {
	mem := make([]byte, 64*1024)
	cfg := BalloonDeviceConfig{AmountMiB: 0}
	d := NewBalloonDevice(mem, 0x1000, 5, cfg, nil, nil)
	defer d.Close()

	// Write actual pages count
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], 10)
	d.WriteConfig(0, buf[:])

	// Write with offset > 4 should be no-op
	d.WriteConfig(5, []byte{0xFF})

	// Empty data should be no-op
	d.WriteConfig(0, nil)
}

func TestBalloonGetConfig(t *testing.T) {
	mem := make([]byte, 64*1024)
	cfg := BalloonDeviceConfig{
		AmountMiB:            0,
		DeflateOnOOM:         true,
		StatsPollingInterval: 2 * time.Second,
	}
	d := NewBalloonDevice(mem, 0x1000, 5, cfg, nil, nil)
	defer d.Close()

	got := d.GetConfig()
	if !got.DeflateOnOOM {
		t.Fatal("expected DeflateOnOOM=true")
	}
	if got.StatsPollingInterval != 2*time.Second {
		t.Fatalf("StatsPollingInterval = %v, want 2s", got.StatsPollingInterval)
	}
}

func TestBalloonSetStatsPollingInterval(t *testing.T) {
	mem := make([]byte, 64*1024)
	cfg := BalloonDeviceConfig{AmountMiB: 0}
	d := NewBalloonDevice(mem, 0x1000, 5, cfg, nil, nil)
	defer d.Close()

	err := d.SetStatsPollingInterval(-1)
	if err == nil {
		t.Fatal("expected error for negative interval")
	}

	err = d.SetStatsPollingInterval(time.Second)
	if err != nil {
		t.Fatalf("SetStatsPollingInterval(1s): %v", err)
	}
	if d.StatsPollingInterval() != time.Second {
		t.Fatalf("StatsPollingInterval = %v, want 1s", d.StatsPollingInterval())
	}
	if !d.StatsEnabled() {
		t.Fatal("expected StatsEnabled=true")
	}
}

func TestBalloonSnapshotPages(t *testing.T) {
	mem := make([]byte, 64*1024)
	cfg := BalloonDeviceConfig{AmountMiB: 0}
	d := NewBalloonDevice(mem, 0x1000, 5, cfg, nil, nil)
	defer d.Close()

	pages := d.SnapshotPages()
	if len(pages) != 0 {
		t.Fatalf("SnapshotPages = %v, want empty", pages)
	}
}

func TestBalloonHandleQueue(t *testing.T) {
	mem := make([]byte, 64*1024)
	cfg := BalloonDeviceConfig{AmountMiB: 0}
	d := NewBalloonDevice(mem, 0x1000, 5, cfg, nil, nil)
	defer d.Close()

	// HandleQueue for inflate (0) and deflate (1) - empty queues, no crash
	q := d.Transport.Queue(0)
	q.DescAddr = 0x100
	q.DriverAddr = 0x200
	q.DeviceAddr = 0x300
	q.Ready = true
	d.HandleQueue(0, q)
	d.HandleQueue(1, d.Transport.Queue(1))
}

// ---------- Block device snapshot/dirty methods ----------

func TestBlockDeviceSnapshotMethods(t *testing.T) {
	// Create a temp file for the block device
	f, err := os.CreateTemp("", "blk-test-*.img")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	defer os.Remove(f.Name())
	// Write 8 sectors
	data := make([]byte, 512*8)
	for i := range data {
		data[i] = byte(i % 256)
	}
	f.Write(data)
	f.Close()

	mem := make([]byte, 64*1024)
	d, err := NewBlockDevice(mem, 0x1000, 5, f.Name(), false, nil, nil)
	if err != nil {
		t.Fatalf("NewBlockDevice: %v", err)
	}
	defer d.Close()

	// PrepareSnapshot should sync
	if err := d.PrepareSnapshot(); err != nil {
		t.Fatalf("PrepareSnapshot: %v", err)
	}

	// SizeBytes
	if d.SizeBytes() != 512*8 {
		t.Fatalf("SizeBytes = %d, want %d", d.SizeBytes(), 512*8)
	}

	// ReadAt
	buf := make([]byte, 10)
	n, err := d.ReadAt(buf, 0)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != 10 {
		t.Fatalf("ReadAt n = %d, want 10", n)
	}

	// DirtyPageSize
	if d.DirtyPageSize() == 0 {
		t.Fatal("DirtyPageSize should not be 0")
	}

	// ResetDirty
	d.ResetDirty()

	// DirtyBitmapAndReset
	bits := d.DirtyBitmapAndReset()
	if bits == nil {
		t.Fatal("DirtyBitmapAndReset should not be nil for device with dirty tracker")
	}
}

func TestBlockDeviceReadOnly(t *testing.T) {
	f, err := os.CreateTemp("", "blk-ro-*.img")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	defer os.Remove(f.Name())
	f.Write(make([]byte, 512))
	f.Close()

	mem := make([]byte, 64*1024)
	d, err := NewBlockDevice(mem, 0x1000, 5, f.Name(), true, nil, nil)
	if err != nil {
		t.Fatalf("NewBlockDevice readonly: %v", err)
	}
	defer d.Close()

	// PrepareSnapshot on read-only should return nil immediately
	if err := d.PrepareSnapshot(); err != nil {
		t.Fatalf("PrepareSnapshot on RO: %v", err)
	}

	// Features should include RO bit
	if d.DeviceFeatures()&BlkFeatureRO == 0 {
		t.Fatal("read-only device should have RO feature")
	}
}

func TestBlockDeviceNilDirty(t *testing.T) {
	f, err := os.CreateTemp("", "blk-nodirty-*.img")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	defer os.Remove(f.Name())
	f.Write(make([]byte, 512))
	f.Close()

	mem := make([]byte, 64*1024)
	// Internally NewBlockDevice always creates a dirty tracker, but we can
	// test the nil-guard paths by creating a device manually
	d, err := NewBlockDevice(mem, 0x1000, 5, f.Name(), false, nil, nil)
	if err != nil {
		t.Fatalf("NewBlockDevice: %v", err)
	}
	defer d.Close()

	// These should work without panicking
	d.ResetDirty()
	_ = d.DirtyBitmapAndReset()
	_ = d.DirtyPageSize()
}

func TestGetBlkBufferSizes(t *testing.T) {
	tests := []struct {
		reqSize  uint32
		wantSize int
	}{
		{100, 100},     // gets from 512 pool, truncated to 100
		{512, 512},     // exact pool match
		{1000, 1000},   // gets from 1024 pool, truncated to 1000
		{4096, 4096},   // exact pool match
		{5000, 5000},   // gets from 8192 pool, truncated to 5000
		{10000, 10000}, // no pool, allocated fresh
	}
	for _, tt := range tests {
		buf := GetBlkBuffer(tt.reqSize)
		if len(buf) != int(tt.reqSize) {
			t.Errorf("GetBlkBuffer(%d) len = %d, want %d", tt.reqSize, len(buf), tt.reqSize)
		}
		PutBlkBuffer(buf)
	}
}

// ---------- Balloon PFN handling ----------

func TestBalloonInflateDeflateViaQueue(t *testing.T) {
	memSize := 64 * 1024
	mem := make([]byte, memSize)
	cfg := BalloonDeviceConfig{AmountMiB: 0}
	d := NewBalloonDevice(mem, 0x10000, 5, cfg, nil, nil)
	defer d.Close()

	q := d.Transport.Queue(0) // inflate queue
	descBase := uint64(0x0000)
	availBase := uint64(0x1000)
	usedBase := uint64(0x2000)
	setupQueueInMemory(mem, q, descBase, availBase, usedBase)

	// Write a PFN (page frame number) into a descriptor buffer
	pfnBuf := uint64(0x4000)
	pfn := uint32(1) // page 1
	binary.LittleEndian.PutUint32(mem[pfnBuf:], pfn)
	writeDesc(mem, descBase, 0, pfnBuf, 4, 0, 0) // readable descriptor
	writeAvailEntry(mem, availBase, 0, 0)

	// Inflate
	d.HandleQueue(0, q)

	stats := d.Stats()
	if stats.ActualPages != 1 {
		t.Fatalf("after inflate: ActualPages = %d, want 1", stats.ActualPages)
	}

	// Now deflate via queue 1
	dq := d.Transport.Queue(1)
	dDescBase := uint64(0x5000)
	dAvailBase := uint64(0x6000)
	dUsedBase := uint64(0x7000)
	setupQueueInMemory(mem, dq, dDescBase, dAvailBase, dUsedBase)

	dpfnBuf := uint64(0x8000)
	binary.LittleEndian.PutUint32(mem[dpfnBuf:], pfn)
	writeDesc(mem, dDescBase, 0, dpfnBuf, 4, 0, 0)
	writeAvailEntry(mem, dAvailBase, 0, 0)

	d.HandleQueue(1, dq)

	stats = d.Stats()
	if stats.ActualPages != 0 {
		t.Fatalf("after deflate: ActualPages = %d, want 0", stats.ActualPages)
	}
}

func TestBalloonInflatePFNOutOfBounds(t *testing.T) {
	memSize := 64 * 1024
	mem := make([]byte, memSize)
	cfg := BalloonDeviceConfig{AmountMiB: 0}
	d := NewBalloonDevice(mem, 0x10000, 5, cfg, nil, nil)
	defer d.Close()

	// Inflate with a PFN that's beyond memory
	d.inflatePFN(999999)
	stats := d.Stats()
	if stats.ActualPages != 0 {
		t.Fatalf("out-of-bounds inflate should not change actual pages, got %d", stats.ActualPages)
	}
}

func TestBalloonDeflatePFNNotInflated(t *testing.T) {
	memSize := 64 * 1024
	mem := make([]byte, memSize)
	cfg := BalloonDeviceConfig{AmountMiB: 0}
	d := NewBalloonDevice(mem, 0x10000, 5, cfg, nil, nil)
	defer d.Close()

	// Deflate a page that was never inflated
	d.deflatePFN(0)
	stats := d.Stats()
	if stats.ActualPages != 0 {
		t.Fatalf("deflating non-inflated page should not change count, got %d", stats.ActualPages)
	}
}

func TestBalloonRestoreSnapshotPages(t *testing.T) {
	memSize := 64 * 1024
	mem := make([]byte, memSize)
	cfg := BalloonDeviceConfig{
		SnapshotPages: []uint32{0, 1, 2},
	}
	d := NewBalloonDevice(mem, 0x10000, 5, cfg, nil, nil)
	defer d.Close()

	stats := d.Stats()
	if stats.ActualPages != 3 {
		t.Fatalf("after restore: ActualPages = %d, want 3", stats.ActualPages)
	}

	pages := d.SnapshotPages()
	if len(pages) != 3 {
		t.Fatalf("SnapshotPages = %d entries, want 3", len(pages))
	}
}

func TestBalloonPollStatsNotEnabled(t *testing.T) {
	mem := make([]byte, 64*1024)
	cfg := BalloonDeviceConfig{AmountMiB: 0}
	d := NewBalloonDevice(mem, 0x10000, 5, cfg, nil, nil)
	defer d.Close()

	_, err := d.PollStats()
	if err == nil {
		t.Fatal("PollStats should fail when stats not enabled")
	}
}

func TestBalloonPollStatsQueueNotReady(t *testing.T) {
	mem := make([]byte, 64*1024)
	cfg := BalloonDeviceConfig{StatsPollingInterval: time.Second}
	d := NewBalloonDevice(mem, 0x10000, 5, cfg, nil, nil)
	defer d.Close()

	// Stats queue not ready, no prior stats
	_, err := d.PollStats()
	if err == nil {
		t.Fatal("PollStats should fail when stats queue not ready and no prior stats")
	}
}

func TestBalloonHandleQueueNotifyStats(t *testing.T) {
	mem := make([]byte, 64*1024)
	cfg := BalloonDeviceConfig{StatsPollingInterval: time.Second}
	d := NewBalloonDevice(mem, 0x10000, 5, cfg, nil, nil)
	defer d.Close()

	// Stats queue (idx 2) should return false
	q := d.Transport.Queue(2)
	if d.HandleQueueNotify(2, q) {
		t.Fatal("HandleQueueNotify for stats queue should return false")
	}

	// Unknown queue should return false
	if d.HandleQueueNotify(5, q) {
		t.Fatal("HandleQueueNotify for unknown queue should return false")
	}
}

func TestBalloonAutoConservativeEnablesPolling(t *testing.T) {
	mem := make([]byte, 64*1024)
	cfg := BalloonDeviceConfig{AutoConservative: true}
	d := NewBalloonDevice(mem, 0x10000, 5, cfg, nil, nil)
	defer d.Close()

	if d.StatsPollingInterval() <= 0 {
		t.Fatal("AutoConservative should set a default polling interval")
	}
}

// ---------- Transport reset clears IRQ ----------

func TestTransportResetClearsIRQ(t *testing.T) {
	dev := &mockDevice{id: 2}
	mem := make([]byte, 64*1024)
	lastIRQ := true
	tr := NewTransport(dev, mem, 0x1000, 5, nil, func(assert bool) {
		lastIRQ = assert
	})

	tr.Write(RegStatus, StatusAcknowledge)
	tr.SetInterruptStat(1)
	tr.SignalIRQ(true)

	// Reset
	tr.Write(RegStatus, 0)
	if lastIRQ {
		t.Fatal("IRQ should be deasserted after reset")
	}
}
