//go:build linux

package virtio

import (
	"encoding/binary"
	"testing"
)

// mockDevice implements Device for testing.
type mockDevice struct {
	id       uint32
	features uint64
	config   []byte
	handled  []uint32
	resets   int
	shmBase  uint64
	shmLen   uint64
	shmOK    bool
}

func (m *mockDevice) DeviceID() uint32       { return m.id }
func (m *mockDevice) DeviceFeatures() uint64 { return m.features }
func (m *mockDevice) ConfigBytes() []byte    { return m.config }
func (m *mockDevice) HandleQueue(idx uint32, q *Queue) {
	m.handled = append(m.handled, idx)
}

func (m *mockDevice) OnTransportReset(*Transport) {
	m.resets++
}

func (m *mockDevice) SharedMemoryRegion(id uint8) (uint64, uint64, bool) {
	if !m.shmOK || id != 0 {
		return 0, 0, false
	}
	return m.shmBase, m.shmLen, true
}

func newTestTransport(dev *mockDevice) (*Transport, []byte) {
	mem := make([]byte, 64*1024) // 64 KiB guest RAM
	t := NewTransport(dev, mem, 0x1000, 5, nil, nil)
	return t, mem
}

// ---------- MMIO read tests ----------

func TestTransportReadMagic(t *testing.T) {
	dev := &mockDevice{id: 2}
	tr, _ := newTestTransport(dev)

	got := tr.Read(RegMagic, 4)
	if got != 0x74726976 {
		t.Fatalf("Read(RegMagic): got %#x, want %#x", got, 0x74726976)
	}
}

func TestTransportReadVersion(t *testing.T) {
	dev := &mockDevice{id: 2}
	tr, _ := newTestTransport(dev)

	got := tr.Read(RegVersion, 4)
	if got != 2 {
		t.Fatalf("Read(RegVersion): got %d, want 2", got)
	}
}

func TestTransportReadDeviceID(t *testing.T) {
	dev := &mockDevice{id: 7}
	tr, _ := newTestTransport(dev)

	got := tr.Read(RegDeviceID, 4)
	if got != 7 {
		t.Fatalf("Read(RegDeviceID): got %d, want 7", got)
	}
}

func TestTransportReadVendorID(t *testing.T) {
	dev := &mockDevice{id: 1}
	tr, _ := newTestTransport(dev)

	got := tr.Read(RegVendorID, 4)
	want := uint32(0x554D4551)
	if got != want {
		t.Fatalf("Read(RegVendorID): got %#x, want %#x", got, want)
	}
}

func TestTransportReadDeviceFeaturesLow(t *testing.T) {
	dev := &mockDevice{id: 1, features: 0xDEADBEEF12345678}
	tr, _ := newTestTransport(dev)

	// Default devFeaturesSel is 0 -> low 32 bits
	got := tr.Read(RegDevFeatures, 4)
	want := uint32(0x12345678)
	if got != want {
		t.Fatalf("Read(RegDevFeatures) low: got %#x, want %#x", got, want)
	}
}

func TestTransportReadDeviceFeaturesHigh(t *testing.T) {
	dev := &mockDevice{id: 1, features: 0xDEADBEEF12345678}
	tr, _ := newTestTransport(dev)

	tr.Write(RegDevFeaturesSel, 1)
	got := tr.Read(RegDevFeatures, 4)
	want := uint32(0xDEADBEEF)
	if got != want {
		t.Fatalf("Read(RegDevFeatures) high: got %#x, want %#x", got, want)
	}
}

func TestTransportReadQueueNumMax(t *testing.T) {
	dev := &mockDevice{id: 1}
	tr, _ := newTestTransport(dev)

	got := tr.Read(RegQueueNumMax, 4)
	if got != 256 {
		t.Fatalf("Read(RegQueueNumMax): got %d, want 256", got)
	}
}

func TestTransportReadConfigBytes(t *testing.T) {
	dev := &mockDevice{id: 1, config: []byte{0xAA, 0xBB, 0xCC}}
	tr, _ := newTestTransport(dev)

	for i, want := range dev.config {
		got := tr.Read(RegConfig+uint32(i), 1)
		if got != uint32(want) {
			t.Fatalf("Read(RegConfig+%d): got %#x, want %#x", i, got, want)
		}
	}
}

func TestTransportReadConfigRange(t *testing.T) {
	dev := &mockDevice{id: 1, config: []byte{0x88, 0x77, 0x66, 0x55, 0x44, 0x33, 0x22, 0x11}}
	tr, _ := newTestTransport(dev)

	buf := make([]byte, 8)
	tr.ReadBytes(RegConfig, buf)

	if got := binary.LittleEndian.Uint64(buf); got != 0x1122334455667788 {
		t.Fatalf("ReadBytes(RegConfig): got %#x, want %#x", got, uint64(0x1122334455667788))
	}
}

func TestTransportReadConfigRangeTruncatesGracefully(t *testing.T) {
	dev := &mockDevice{id: 1, config: []byte{0xAA, 0xBB, 0xCC}}
	tr, _ := newTestTransport(dev)

	buf := []byte{0xFF, 0xFF, 0xFF, 0xFF}
	tr.ReadBytes(RegConfig+1, buf)

	want := []byte{0xBB, 0xCC, 0x00, 0x00}
	if string(buf) != string(want) {
		t.Fatalf("ReadBytes(RegConfig+1): got %v, want %v", buf, want)
	}
}

func TestTransportReadConfigBeyondLength(t *testing.T) {
	dev := &mockDevice{id: 1, config: []byte{0x01}}
	tr, _ := newTestTransport(dev)

	got := tr.Read(RegConfig+10, 1)
	if got != 0 {
		t.Fatalf("Read beyond config: got %d, want 0", got)
	}
}

func TestTransportReadSharedMemoryAbsentReturnsAllOnesLength(t *testing.T) {
	dev := &mockDevice{id: 1}
	tr, _ := newTestTransport(dev)

	if got := tr.Read(RegSHMLenLow, 4); got != 0xffffffff {
		t.Fatalf("Read(RegSHMLenLow): got %#x, want %#x", got, uint32(0xffffffff))
	}
	if got := tr.Read(RegSHMLenHigh, 4); got != 0xffffffff {
		t.Fatalf("Read(RegSHMLenHigh): got %#x, want %#x", got, uint32(0xffffffff))
	}
	if got := tr.Read(RegSHMBaseLow, 4); got != 0 {
		t.Fatalf("Read(RegSHMBaseLow): got %#x, want 0", got)
	}
	if got := tr.Read(RegSHMBaseHigh, 4); got != 0 {
		t.Fatalf("Read(RegSHMBaseHigh): got %#x, want 0", got)
	}
}

func TestTransportReadSharedMemoryPresent(t *testing.T) {
	dev := &mockDevice{id: 1, shmBase: 0x1234_5678_9abc_def0, shmLen: 0x1000_2000_3000_4000, shmOK: true}
	tr, _ := newTestTransport(dev)

	tr.Write(RegSHMSel, 0)
	if got := tr.Read(RegSHMLenLow, 4); got != uint32(dev.shmLen) {
		t.Fatalf("Read(RegSHMLenLow): got %#x, want %#x", got, uint32(dev.shmLen))
	}
	if got := tr.Read(RegSHMLenHigh, 4); got != uint32(dev.shmLen>>32) {
		t.Fatalf("Read(RegSHMLenHigh): got %#x, want %#x", got, uint32(dev.shmLen>>32))
	}
	if got := tr.Read(RegSHMBaseLow, 4); got != uint32(dev.shmBase) {
		t.Fatalf("Read(RegSHMBaseLow): got %#x, want %#x", got, uint32(dev.shmBase))
	}
	if got := tr.Read(RegSHMBaseHigh, 4); got != uint32(dev.shmBase>>32) {
		t.Fatalf("Read(RegSHMBaseHigh): got %#x, want %#x", got, uint32(dev.shmBase>>32))
	}
}

// ---------- MMIO write tests ----------

func TestTransportWriteQueueAddresses(t *testing.T) {
	dev := &mockDevice{id: 2}
	tr, _ := newTestTransport(dev)

	// Select queue 0 (default)
	tr.Write(RegQueueSel, 0)

	tr.Write(RegQueueDescLow, 0x1000)
	tr.Write(RegQueueDescHigh, 0x0002)
	tr.Write(RegQueueDriverLow, 0x3000)
	tr.Write(RegQueueDriverHigh, 0x0004)
	tr.Write(RegQueueDeviceLow, 0x5000)
	tr.Write(RegQueueDeviceHigh, 0x0006)

	q := tr.Queue(0)

	wantDesc := uint64(0x0002)<<32 | uint64(0x1000)
	if q.DescAddr != wantDesc {
		t.Fatalf("DescAddr: got %#x, want %#x", q.DescAddr, wantDesc)
	}
	wantDriver := uint64(0x0004)<<32 | uint64(0x3000)
	if q.DriverAddr != wantDriver {
		t.Fatalf("DriverAddr: got %#x, want %#x", q.DriverAddr, wantDriver)
	}
	wantDevice := uint64(0x0006)<<32 | uint64(0x5000)
	if q.DeviceAddr != wantDevice {
		t.Fatalf("DeviceAddr: got %#x, want %#x", q.DeviceAddr, wantDevice)
	}
}

func TestTransportWriteBytesQueueAddresses(t *testing.T) {
	dev := &mockDevice{id: 2}
	tr, _ := newTestTransport(dev)

	put32 := func(offset, val uint32) {
		buf := make([]byte, 4)
		binary.LittleEndian.PutUint32(buf, val)
		tr.WriteBytes(offset, buf)
	}

	put32(RegQueueSel, 0)
	put32(RegQueueDescLow, 0x1000)
	put32(RegQueueDescHigh, 0x0002)
	put32(RegQueueDriverLow, 0x3000)
	put32(RegQueueDriverHigh, 0x0004)
	put32(RegQueueDeviceLow, 0x5000)
	put32(RegQueueDeviceHigh, 0x0006)

	q := tr.Queue(0)
	if q.DescAddr != uint64(0x0002)<<32|0x1000 {
		t.Fatalf("DescAddr: got %#x", q.DescAddr)
	}
	if q.DriverAddr != uint64(0x0004)<<32|0x3000 {
		t.Fatalf("DriverAddr: got %#x", q.DriverAddr)
	}
	if q.DeviceAddr != uint64(0x0006)<<32|0x5000 {
		t.Fatalf("DeviceAddr: got %#x", q.DeviceAddr)
	}
}

func TestTransportWriteQueueSelects(t *testing.T) {
	dev := &mockDevice{id: 2}
	tr, _ := newTestTransport(dev)

	// Configure queue 0 desc addr
	tr.Write(RegQueueSel, 0)
	tr.Write(RegQueueDescLow, 0xAAAA)

	// Configure queue 1 desc addr
	tr.Write(RegQueueSel, 1)
	tr.Write(RegQueueDescLow, 0xBBBB)

	if tr.Queue(0).DescAddr != 0xAAAA {
		t.Fatalf("Queue(0).DescAddr: got %#x, want 0xAAAA", tr.Queue(0).DescAddr)
	}
	if tr.Queue(1).DescAddr != 0xBBBB {
		t.Fatalf("Queue(1).DescAddr: got %#x, want 0xBBBB", tr.Queue(1).DescAddr)
	}
}

func TestTransportWriteQueueSelBound(t *testing.T) {
	dev := &mockDevice{id: 2}
	tr, _ := newTestTransport(dev)

	// Attempting to select queue >= 8 should be ignored
	tr.Write(RegQueueSel, 0)
	tr.Write(RegQueueDescLow, 0x1111)
	tr.Write(RegQueueSel, 99)         // ignored
	tr.Write(RegQueueDescLow, 0x2222) // still writes to queue 0

	if tr.Queue(0).DescAddr != 0x2222 {
		t.Fatalf("QueueSel boundary: queue 0 desc got %#x, want 0x2222", tr.Queue(0).DescAddr)
	}
}

func TestTransportWriteStatus(t *testing.T) {
	dev := &mockDevice{id: 2}
	tr, _ := newTestTransport(dev)

	tr.Write(RegStatus, StatusAcknowledge|StatusDriver)
	got := tr.Read(RegStatus, 4)
	want := uint32(StatusAcknowledge | StatusDriver)
	if got != want {
		t.Fatalf("Status: got %#x, want %#x", got, want)
	}
}

func TestTransportWriteStatusResetClearsState(t *testing.T) {
	dev := &mockDevice{id: 2}
	tr, _ := newTestTransport(dev)

	tr.Write(RegQueueSel, 0)
	tr.Write(RegQueueNum, 64)
	tr.Write(RegQueueDescLow, 0x1000)
	tr.Write(RegQueueDriverLow, 0x2000)
	tr.Write(RegQueueDeviceLow, 0x3000)
	tr.Write(RegQueueReady, 1)
	tr.Write(RegDrvFeaturesSel, 1)
	tr.Write(RegDrvFeatures, 0xfeedbeef)
	tr.Write(RegStatus, StatusAcknowledge|StatusDriver|StatusFeaturesOK|StatusDriverOK)

	tr.Write(RegStatus, 0)

	if got := tr.Read(RegStatus, 4); got != 0 {
		t.Fatalf("Status after reset: got %#x, want 0", got)
	}
	if dev.resets != 1 {
		t.Fatalf("reset observer count = %d, want 1", dev.resets)
	}
	if q := tr.Queue(0); q.Ready || q.LastAvail != 0 || q.DescAddr != 0 || q.DriverAddr != 0 || q.DeviceAddr != 0 || q.Size != 256 {
		t.Fatalf("queue not reset: %+v", *q)
	}
	st := tr.State()
	if st.DrvFeatures != 0 || st.QueueSel != 0 || st.InterruptStat != 0 {
		t.Fatalf("transport state not cleared after reset: %+v", st)
	}
}

func TestTransportWriteDriverFeatures(t *testing.T) {
	dev := &mockDevice{id: 1}
	tr, _ := newTestTransport(dev)

	tr.Write(RegDrvFeaturesSel, 0)
	tr.Write(RegDrvFeatures, 0x11223344)
	tr.Write(RegDrvFeaturesSel, 1)
	tr.Write(RegDrvFeatures, 0x55667788)

	st := tr.State()
	want := uint64(0x55667788_11223344)
	if st.DrvFeatures != want {
		t.Fatalf("DrvFeatures: got %#x, want %#x", st.DrvFeatures, want)
	}
}

func TestTransportQueueReady(t *testing.T) {
	dev := &mockDevice{id: 2}
	tr, _ := newTestTransport(dev)

	tr.Write(RegQueueSel, 0)
	if tr.Read(RegQueueReady, 4) != 0 {
		t.Fatal("queue should not be ready initially")
	}
	tr.Write(RegQueueReady, 1)
	if tr.Read(RegQueueReady, 4) != 1 {
		t.Fatal("queue should be ready after writing 1")
	}
}

func TestTransportQueueNum(t *testing.T) {
	dev := &mockDevice{id: 2}
	tr, _ := newTestTransport(dev)

	tr.Write(RegQueueSel, 0)
	tr.Write(RegQueueNum, 128)
	if tr.Queue(0).Size != 128 {
		t.Fatalf("Queue size: got %d, want 128", tr.Queue(0).Size)
	}
}

// ---------- Queue notify ----------

func TestTransportQueueNotify(t *testing.T) {
	dev := &mockDevice{id: 2}
	tr, _ := newTestTransport(dev)

	tr.Write(RegQueueSel, 0)
	tr.Write(RegQueueReady, 1)

	tr.Write(RegQueueNotify, 0)

	if len(dev.handled) != 1 {
		t.Fatalf("HandleQueue call count: got %d, want 1", len(dev.handled))
	}
	if dev.handled[0] != 0 {
		t.Fatalf("HandleQueue idx: got %d, want 0", dev.handled[0])
	}
	// Interrupt status should be set
	if tr.Read(RegInterruptStat, 4) != 1 {
		t.Fatal("InterruptStat should be 1 after notify")
	}
}

func TestTransportQueueNotifyNotReady(t *testing.T) {
	dev := &mockDevice{id: 2}
	tr, _ := newTestTransport(dev)

	// Queue 0 is not ready; notify should be ignored
	tr.Write(RegQueueNotify, 0)
	if len(dev.handled) != 0 {
		t.Fatal("HandleQueue should not be called for non-ready queue")
	}
}

func TestTransportQueueNotifyIRQCallback(t *testing.T) {
	dev := &mockDevice{id: 2}
	mem := make([]byte, 64*1024)
	irqAsserted := false
	tr := NewTransport(dev, mem, 0x1000, 5, nil, func(assert bool) {
		irqAsserted = assert
	})

	tr.Write(RegQueueSel, 0)
	tr.Write(RegQueueReady, 1)
	tr.Write(RegQueueNotify, 0)

	if !irqAsserted {
		t.Fatal("IRQ should be asserted after queue notify")
	}

	// Acknowledge the interrupt
	tr.Write(RegInterruptACK, 1)
	if irqAsserted {
		t.Fatal("IRQ should be deasserted after ACK clears all bits")
	}
	if tr.Read(RegInterruptStat, 4) != 0 {
		t.Fatal("InterruptStat should be 0 after ACK")
	}
}

// ---------- Queue operations with in-memory rings ----------

// setupQueueInMemory places descriptor table, avail ring, and used ring in
// the provided memory slice and configures the queue accordingly.
//
// Layout:
//
//	descBase:  descriptor table   (16 bytes per entry)
//	availBase: avail ring header  (4 bytes) + ring entries (2 bytes each)
//	usedBase:  used ring header   (4 bytes) + ring entries (8 bytes each)
func setupQueueInMemory(mem []byte, q *Queue, descBase, availBase, usedBase uint64) {
	q.DescAddr = descBase
	q.DriverAddr = availBase
	q.DeviceAddr = usedBase
	q.Ready = true
}

// writeDesc writes a descriptor at index idx into guest memory at the
// queue's descriptor table base.
func writeDesc(mem []byte, descBase uint64, idx uint16, addr uint64, length uint32, flags uint16, next uint16) {
	off := descBase + uint64(idx)*16
	binary.LittleEndian.PutUint64(mem[off:], addr)
	binary.LittleEndian.PutUint32(mem[off+8:], length)
	binary.LittleEndian.PutUint16(mem[off+12:], flags)
	binary.LittleEndian.PutUint16(mem[off+14:], next)
}

// writeAvailEntry writes an entry into the avail ring and bumps the avail idx.
func writeAvailEntry(mem []byte, availBase uint64, ringIdx uint16, descHead uint16) {
	// ring[ringIdx]
	off := availBase + 4 + uint64(ringIdx)*2
	binary.LittleEndian.PutUint16(mem[off:], descHead)
	// bump avail.idx to ringIdx+1
	binary.LittleEndian.PutUint16(mem[availBase+2:], ringIdx+1)
}

func TestQueueIterAvailAndWalkChain(t *testing.T) {
	mem := make([]byte, 64*1024)
	q := NewQueue(mem, 256, nil)

	descBase := uint64(0x0000)
	availBase := uint64(0x1000)
	usedBase := uint64(0x2000)
	setupQueueInMemory(mem, q, descBase, availBase, usedBase)

	// Build a chain of 3 descriptors: 0 -> 1 -> 2
	dataAddr := uint64(0x4000)
	writeDesc(mem, descBase, 0, dataAddr, 64, DescFlagNext, 1)
	writeDesc(mem, descBase, 1, dataAddr+64, 128, DescFlagNext|DescFlagWrite, 2)
	writeDesc(mem, descBase, 2, dataAddr+192, 1, DescFlagWrite, 0)

	// Post chain head 0 in avail ring
	writeAvailEntry(mem, availBase, 0, 0)

	// IterAvail should yield exactly one chain head
	var heads []uint16
	if err := q.IterAvail(func(head uint16) {
		heads = append(heads, head)
	}); err != nil {
		t.Fatalf("IterAvail() error = %v", err)
	}
	if len(heads) != 1 || heads[0] != 0 {
		t.Fatalf("IterAvail heads: got %v, want [0]", heads)
	}

	// WalkChain from head 0 should return 3 descriptors
	chain, err := q.WalkChain(0)
	if err != nil {
		t.Fatalf("WalkChain() error = %v", err)
	}
	if len(chain) != 3 {
		t.Fatalf("WalkChain length: got %d, want 3", len(chain))
	}
	if chain[0].Addr != dataAddr || chain[0].Len != 64 {
		t.Fatalf("chain[0]: addr=%#x len=%d, want addr=%#x len=64", chain[0].Addr, chain[0].Len, dataAddr)
	}
	if chain[1].Flags&DescFlagWrite == 0 {
		t.Fatal("chain[1] should have Write flag")
	}
	if chain[2].Flags&DescFlagNext != 0 {
		t.Fatal("chain[2] should not have Next flag")
	}
}

func TestQueuePushUsed(t *testing.T) {
	mem := make([]byte, 64*1024)
	q := NewQueue(mem, 256, nil)

	usedBase := uint64(0x2000)
	q.DeviceAddr = usedBase

	// Push two used entries
	if err := q.PushUsed(42, 512); err != nil {
		t.Fatalf("PushUsed() error = %v", err)
	}
	if err := q.PushUsed(7, 1024); err != nil {
		t.Fatalf("PushUsed() error = %v", err)
	}

	// used.idx should be 2
	usedIdx := binary.LittleEndian.Uint16(mem[usedBase+2:])
	if usedIdx != 2 {
		t.Fatalf("used.idx: got %d, want 2", usedIdx)
	}

	// Check first entry (offset 4 from base)
	entry0Off := usedBase + 4
	id0 := binary.LittleEndian.Uint32(mem[entry0Off:])
	len0 := binary.LittleEndian.Uint32(mem[entry0Off+4:])
	if id0 != 42 || len0 != 512 {
		t.Fatalf("used[0]: id=%d len=%d, want id=42 len=512", id0, len0)
	}

	// Check second entry (offset 4+8 = 12 from base)
	entry1Off := usedBase + 4 + 8
	id1 := binary.LittleEndian.Uint32(mem[entry1Off:])
	len1 := binary.LittleEndian.Uint32(mem[entry1Off+4:])
	if id1 != 7 || len1 != 1024 {
		t.Fatalf("used[1]: id=%d len=%d, want id=7 len=1024", id1, len1)
	}
}

func TestQueueIterAvailMultiple(t *testing.T) {
	mem := make([]byte, 64*1024)
	q := NewQueue(mem, 256, nil)

	descBase := uint64(0x0000)
	availBase := uint64(0x1000)
	usedBase := uint64(0x2000)
	setupQueueInMemory(mem, q, descBase, availBase, usedBase)

	// Two independent single-descriptor chains at desc index 0 and 1
	writeDesc(mem, descBase, 0, 0x4000, 32, 0, 0)
	writeDesc(mem, descBase, 1, 0x5000, 64, 0, 0)

	writeAvailEntry(mem, availBase, 0, 0)
	writeAvailEntry(mem, availBase, 1, 1)

	var heads []uint16
	if err := q.IterAvail(func(head uint16) {
		heads = append(heads, head)
	}); err != nil {
		t.Fatalf("IterAvail() error = %v", err)
	}
	if len(heads) != 2 {
		t.Fatalf("IterAvail count: got %d, want 2", len(heads))
	}
	if heads[0] != 0 || heads[1] != 1 {
		t.Fatalf("IterAvail heads: got %v, want [0 1]", heads)
	}
}

func TestQueueIterAvailNone(t *testing.T) {
	mem := make([]byte, 64*1024)
	q := NewQueue(mem, 256, nil)

	availBase := uint64(0x1000)
	q.DriverAddr = availBase

	// avail.idx = 0, LastAvail = 0 => nothing to iterate
	called := false
	if err := q.IterAvail(func(head uint16) {
		called = true
	}); err != nil {
		t.Fatalf("IterAvail() error = %v", err)
	}
	if called {
		t.Fatal("IterAvail should not call fn when no new entries")
	}
}

func TestQueueGuestReadWrite(t *testing.T) {
	mem := make([]byte, 64*1024)
	q := NewQueue(mem, 256, nil)

	data := []byte("hello, guest memory!")
	addr := uint64(0x3000)
	if err := q.GuestWrite(addr, data); err != nil {
		t.Fatalf("GuestWrite() error = %v", err)
	}

	buf := make([]byte, len(data))
	if err := q.GuestRead(addr, buf); err != nil {
		t.Fatalf("GuestRead() error = %v", err)
	}
	if string(buf) != string(data) {
		t.Fatalf("GuestRead: got %q, want %q", buf, data)
	}
}

func TestQueueGuestReadWriteRejectsOutOfBounds(t *testing.T) {
	mem := make([]byte, 64)
	q := NewQueue(mem, 8, nil)
	if err := q.GuestWrite(60, []byte("hello")); err == nil {
		t.Fatal("GuestWrite() = nil, want bounds error")
	}
	buf := make([]byte, 8)
	if err := q.GuestRead(60, buf); err == nil {
		t.Fatal("GuestRead() = nil, want bounds error")
	}
}

func TestQueueWalkChainRejectsCycle(t *testing.T) {
	mem := make([]byte, 64*1024)
	q := NewQueue(mem, 256, nil)

	descBase := uint64(0x0000)
	availBase := uint64(0x1000)
	usedBase := uint64(0x2000)
	setupQueueInMemory(mem, q, descBase, availBase, usedBase)

	writeDesc(mem, descBase, 0, 0x4000, 64, DescFlagNext, 1)
	writeDesc(mem, descBase, 1, 0x4040, 64, DescFlagNext, 0)

	if _, err := q.WalkChain(0); err == nil {
		t.Fatal("WalkChain() = nil, want cycle error")
	}
}

func TestTransportRejectsInvalidQueueNum(t *testing.T) {
	dev := &mockDevice{id: 2}
	tr, _ := newTestTransport(dev)

	tr.Write(RegQueueSel, 0)
	tr.Write(RegQueueNum, 0)
	if got := tr.Queue(0).Size; got != 256 {
		t.Fatalf("queue size after invalid zero write = %d, want 256", got)
	}
	tr.Write(RegQueueNum, MaxQueueSize+1)
	if got := tr.Queue(0).Size; got != 256 {
		t.Fatalf("queue size after oversized write = %d, want 256", got)
	}
}

// ---------- State snapshot/restore ----------

func TestQueueStateRoundTrip(t *testing.T) {
	mem := make([]byte, 64*1024)
	q := NewQueue(mem, 256, nil)
	q.DescAddr = 0x1000
	q.DriverAddr = 0x2000
	q.DeviceAddr = 0x3000
	q.Ready = true
	q.LastAvail = 5

	st := q.State()
	q2 := NewQueue(mem, 128, nil)
	q2.RestoreState(st)

	if q2.Size != 256 || q2.DescAddr != 0x1000 || q2.DriverAddr != 0x2000 ||
		q2.DeviceAddr != 0x3000 || !q2.Ready || q2.LastAvail != 5 {
		t.Fatal("Queue state round-trip mismatch")
	}
}

func TestTransportStateRoundTrip(t *testing.T) {
	dev := &mockDevice{id: 2}
	tr, _ := newTestTransport(dev)

	tr.Write(RegStatus, StatusDriverOK)
	tr.Write(RegDrvFeaturesSel, 0)
	tr.Write(RegDrvFeatures, 0xAABBCCDD)
	tr.Write(RegQueueSel, 1)
	tr.Write(RegQueueReady, 1)

	st := tr.State()

	dev2 := &mockDevice{id: 2}
	tr2, _ := newTestTransport(dev2)
	tr2.RestoreState(st)

	if tr2.Read(RegStatus, 4) != StatusDriverOK {
		t.Fatal("Status not restored")
	}
	st2 := tr2.State()
	if st2.DrvFeatures != 0xAABBCCDD {
		t.Fatalf("DrvFeatures: got %#x, want 0xAABBCCDD", st2.DrvFeatures)
	}
	if !tr2.Queue(1).Ready {
		t.Fatal("Queue 1 should be ready after restore")
	}
}

func TestTransportBaseAndIRQ(t *testing.T) {
	dev := &mockDevice{id: 2}
	mem := make([]byte, 4096)
	tr := NewTransport(dev, mem, 0xDEAD0000, 42, nil, nil)

	if tr.BasePA() != 0xDEAD0000 {
		t.Fatalf("BasePA: got %#x, want 0xDEAD0000", tr.BasePA())
	}
	if tr.IRQLine() != 42 {
		t.Fatalf("IRQLine: got %d, want 42", tr.IRQLine())
	}
}

func TestTransportReadConfigGeneration(t *testing.T) {
	dev := &mockDevice{id: 1}
	tr, _ := newTestTransport(dev)

	got := tr.Read(RegConfigGeneration, 4)
	if got != 0 {
		t.Fatalf("ConfigGeneration: got %d, want 0", got)
	}
}

func TestTransportReadUnknownOffset(t *testing.T) {
	dev := &mockDevice{id: 1}
	tr, _ := newTestTransport(dev)

	// An offset that is not handled should return 0
	got := tr.Read(0xFFF, 4)
	if got != 0 {
		t.Fatalf("Read(unknown offset): got %d, want 0", got)
	}
}
