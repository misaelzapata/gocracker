//go:build linux

// Package virtio implements the virtio 1.1 MMIO transport and virtqueue ring.
// Devices (net, blk) embed Transport and override DeviceFeatures / HandleQueue.
package virtio

import (
	"encoding/binary"
	"fmt"
	"sync"
)

const (
	MaxQueueSize              = 256
	MaxDescriptorChainLength  = MaxQueueSize
	MaxDescriptorSizeBytes    = 16 << 20
	MaxDescriptorChainBytes   = 64 << 20
)

// ---- Virtio MMIO register offsets (virtio spec 4.2.2) ----
const (
	RegMagic            = 0x000 // "virt" magic
	RegVersion          = 0x004 // 2 = virtio 1.1
	RegDeviceID         = 0x008
	RegVendorID         = 0x00C
	RegDevFeatures      = 0x010
	RegDevFeaturesSel   = 0x014
	RegDrvFeatures      = 0x020
	RegDrvFeaturesSel   = 0x024
	RegQueueSel         = 0x030
	RegQueueNumMax      = 0x034
	RegQueueNum         = 0x038
	RegQueueReady       = 0x044
	RegQueueNotify      = 0x050
	RegInterruptStat    = 0x060
	RegInterruptACK     = 0x064
	RegStatus           = 0x070
	RegQueueDescLow     = 0x080
	RegQueueDescHigh    = 0x084
	RegQueueDriverLow   = 0x090
	RegQueueDriverHigh  = 0x094
	RegQueueDeviceLow   = 0x0A0
	RegQueueDeviceHigh  = 0x0A4
	RegSHMSel           = 0x0AC
	RegSHMLenLow        = 0x0B0
	RegSHMLenHigh       = 0x0B4
	RegSHMBaseLow       = 0x0B8
	RegSHMBaseHigh      = 0x0BC
	RegConfigGeneration = 0x0FC
	RegConfig           = 0x100
)

// Virtio device IDs
const (
	DeviceIDNet   = 1
	DeviceIDBlock = 2
	DeviceIDFS    = 26
)

// Virtio status bits
const (
	StatusAcknowledge = 1
	StatusDriver      = 2
	StatusDriverOK    = 4
	StatusFeaturesOK  = 8
	StatusFailed      = 128
)

// DescFlags for virtqueue descriptors
const (
	DescFlagNext     = 1
	DescFlagWrite    = 2
	DescFlagIndirect = 4
)

// Desc is a single virtqueue descriptor (16 bytes).
type Desc struct {
	Addr  uint64
	Len   uint32
	Flags uint16
	Next  uint16
}

// AvailRing is the driver-to-device available ring.
type AvailRing struct {
	Flags uint16
	Idx   uint16
	Ring  [256]uint16
}

// UsedElem is one entry in the used ring.
type UsedElem struct {
	ID  uint32
	Len uint32
}

// UsedRing is the device-to-driver used ring.
type UsedRing struct {
	Flags uint16
	Idx   uint16
	Ring  [256]UsedElem
}

// Queue manages a single virtqueue.
type Queue struct {
	mu        sync.Mutex
	Size      uint32
	Ready     bool
	LastAvail uint16

	DescAddr   uint64
	DriverAddr uint64 // avail ring
	DeviceAddr uint64 // used ring

	mem           []byte // reference to guest RAM
	guestPhysBase uint64 // GPA of mem[0] (0 on x86, 0x80000000 on ARM64)
	dirty         *DirtyTracker
}

// NewQueue allocates a virtqueue bound to guest memory.
func NewQueue(mem []byte, size uint32, dirty *DirtyTracker) *Queue {
	return &Queue{mem: mem, Size: size, dirty: dirty}
}

// SetGuestPhysBase sets the guest physical address corresponding to mem[0].
// Must be called before the queue processes any descriptors.
func (q *Queue) SetGuestPhysBase(base uint64) { q.guestPhysBase = base }

// off translates a guest physical address to a mem[] offset.
func (q *Queue) off(gpa uint64) uint64 { return gpa - q.guestPhysBase }

// Reset returns the virtqueue to its post-creation state.
func (q *Queue) Reset() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.Size = MaxQueueSize
	q.Ready = false
	q.LastAvail = 0
	q.DescAddr = 0
	q.DriverAddr = 0
	q.DeviceAddr = 0
}

// IterAvail calls fn for each new descriptor chain in the available ring.
// fn receives the head descriptor index; call WalkChain to traverse it.
// IMPORTANT: fn runs with q.mu held — use pushUsedLocked(), not PushUsed().
func (q *Queue) IterAvail(fn func(head uint16)) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	avail, err := q.readAvail()
	if err != nil {
		return err
	}
	for q.LastAvail != avail.Idx {
		head := avail.Ring[q.LastAvail%uint16(q.Size)]
		q.LastAvail++
		fn(head)
	}
	return nil
}

// ConsumeAvail consumes at most one available descriptor chain.
// fn runs with q.mu held, so callers can safely use WalkChain and PushUsedLocked.
func (q *Queue) ConsumeAvail(fn func(head uint16)) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	avail, err := q.readAvail()
	if err != nil {
		return false, err
	}
	if q.LastAvail == avail.Idx {
		return false, nil
	}
	head := avail.Ring[q.LastAvail%uint16(q.Size)]
	q.LastAvail++
	fn(head)
	return true, nil
}

// WalkChain returns all descriptors in the chain starting at head.
func (q *Queue) WalkChain(head uint16) ([]Desc, error) {
	size, err := q.normalizedSize()
	if err != nil {
		return nil, err
	}
	if head >= size {
		return nil, fmt.Errorf("descriptor head %d out of bounds for queue size %d", head, size)
	}
	var chain []Desc
	idx := head
	var totalBytes uint64
	seen := make([]bool, size)
	for {
		if seen[idx] {
			return nil, fmt.Errorf("descriptor chain cycle detected at index %d", idx)
		}
		seen[idx] = true
		d, err := q.readDesc(idx)
		if err != nil {
			return nil, err
		}
		if d.Flags&DescFlagIndirect != 0 {
			return nil, fmt.Errorf("indirect descriptors are not supported")
		}
		if d.Len > MaxDescriptorSizeBytes {
			return nil, fmt.Errorf("descriptor length %d exceeds limit %d", d.Len, MaxDescriptorSizeBytes)
		}
		totalBytes += uint64(d.Len)
		if totalBytes > MaxDescriptorChainBytes {
			return nil, fmt.Errorf("descriptor chain length %d exceeds limit %d", totalBytes, MaxDescriptorChainBytes)
		}
		chain = append(chain, d)
		if len(chain) > MaxDescriptorChainLength {
			return nil, fmt.Errorf("descriptor chain exceeds %d entries", MaxDescriptorChainLength)
		}
		if d.Flags&DescFlagNext == 0 {
			break
		}
		if d.Next >= size {
			return nil, fmt.Errorf("descriptor next index %d out of bounds for queue size %d", d.Next, size)
		}
		idx = d.Next
	}
	return chain, nil
}

// pushUsedLocked adds an entry to the used ring. Caller must hold q.mu.
func (q *Queue) pushUsedLocked(id uint32, written uint32) error {
	size, err := q.normalizedSize()
	if err != nil {
		return err
	}
	used, err := q.readUsedIdx()
	if err != nil {
		return err
	}
	entryGPA := q.DeviceAddr + 4 + uint64(used%uint16(q.Size))*8
	if _, _, err := q.checkedRange(entryGPA, 8); err != nil {
		return err
	}
	if _, _, err := q.checkedRange(q.DeviceAddr+2, 2); err != nil {
		return err
	}
	entryOff := q.off(entryGPA)
	idxOff := q.off(q.DeviceAddr + 2)
	binary.LittleEndian.PutUint32(q.mem[entryOff:], id)
	binary.LittleEndian.PutUint32(q.mem[entryOff+4:], written)
	binary.LittleEndian.PutUint16(q.mem[idxOff:], used+1)
	q.markDirty(entryGPA, 8)
	q.markDirty(q.DeviceAddr+2, 2)
	_ = size
	return nil
}

// PushUsed adds an entry to the used ring (acquires lock).
func (q *Queue) PushUsed(id uint32, written uint32) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.pushUsedLocked(id, written)
}

// PushUsedLocked adds an entry to the used ring while the caller already holds
// q.mu, for example from within IterAvail callbacks.
func (q *Queue) PushUsedLocked(id uint32, written uint32) error {
	return q.pushUsedLocked(id, written)
}

// GuestRead reads len bytes from guest physical address addr.
func (q *Queue) GuestRead(addr uint64, buf []byte) error {
	start, end, err := q.checkedRange(addr, uint64(len(buf)))
	if err != nil {
		return err
	}
	copy(buf, q.mem[start:end])
	return nil
}

// GuestWrite writes buf to guest physical address addr.
func (q *Queue) GuestWrite(addr uint64, buf []byte) error {
	start, end, err := q.checkedRange(addr, uint64(len(buf)))
	if err != nil {
		return err
	}
	copy(q.mem[start:end], buf)
	q.markDirty(addr, uint64(len(buf)))
	return nil
}

func (q *Queue) markDirty(addr, length uint64) {
	if q.dirty != nil {
		q.dirty.Mark(addr, length)
	}
}

func (q *Queue) readDesc(idx uint16) (Desc, error) {
	size, err := q.normalizedSize()
	if err != nil {
		return Desc{}, err
	}
	if idx >= size {
		return Desc{}, fmt.Errorf("descriptor index %d out of bounds for queue size %d", idx, size)
	}
	gpa := q.DescAddr + uint64(idx)*16
	if _, _, err := q.checkedRange(gpa, 16); err != nil {
		return Desc{}, err
	}
	o := q.off(gpa)
	return Desc{
		Addr:  binary.LittleEndian.Uint64(q.mem[o:]),
		Len:   binary.LittleEndian.Uint32(q.mem[o+8:]),
		Flags: binary.LittleEndian.Uint16(q.mem[o+12:]),
		Next:  binary.LittleEndian.Uint16(q.mem[o+14:]),
	}, nil
}

func (q *Queue) readAvail() (AvailRing, error) {
	size, err := q.normalizedSize()
	if err != nil {
		return AvailRing{}, err
	}
	gpa := q.DriverAddr
	if _, _, err := q.checkedRange(gpa, 4+uint64(size)*2); err != nil {
		return AvailRing{}, err
	}
	o := q.off(gpa)
	a := AvailRing{
		Flags: binary.LittleEndian.Uint16(q.mem[o:]),
		Idx:   binary.LittleEndian.Uint16(q.mem[o+2:]),
	}
	for i := uint16(0); i < size; i++ {
		a.Ring[i] = binary.LittleEndian.Uint16(q.mem[o+4+uint64(i)*2:])
	}
	return a, nil
}

func (q *Queue) readUsedIdx() (uint16, error) {
	if _, _, err := q.checkedRange(q.DeviceAddr+2, 2); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(q.mem[q.off(q.DeviceAddr+2):]), nil
}

func (q *Queue) normalizedSize() (uint16, error) {
	if q.Size == 0 || q.Size > MaxQueueSize {
		return 0, fmt.Errorf("invalid queue size %d", q.Size)
	}
	return uint16(q.Size), nil
}

func (q *Queue) checkedRange(addr, length uint64) (uint64, uint64, error) {
	if length == 0 {
		return 0, 0, nil
	}
	// Translate guest physical address to mem[] offset.
	if addr < q.guestPhysBase {
		return 0, 0, fmt.Errorf("guest memory access below base: addr=%d base=%d", addr, q.guestPhysBase)
	}
	off := addr - q.guestPhysBase
	end := off + length
	if end < off {
		return 0, 0, fmt.Errorf("guest memory access overflow: addr=%d len=%d", addr, length)
	}
	if end > uint64(len(q.mem)) {
		return 0, 0, fmt.Errorf("guest memory access out of bounds: addr=%d len=%d mem=%d", addr, length, len(q.mem))
	}
	return off, end, nil
}

// ---- MMIO Transport ----

// Device is implemented by virtio-net and virtio-blk.
type Device interface {
	DeviceID() uint32
	DeviceFeatures() uint64
	HandleQueue(idx uint32, q *Queue)
	ConfigBytes() []byte
}

type StateChangeObserver interface {
	OnTransportStateChange(*Transport)
}

type ResetObserver interface {
	OnTransportReset(*Transport)
}

type QueueNotifyInterceptor interface {
	HandleQueueNotify(idx uint32, q *Queue) bool
}

type SharedMemoryProvider interface {
	SharedMemoryRegion(id uint8) (base uint64, length uint64, ok bool)
}

type ConfigWriter interface {
	WriteConfig(offset uint32, data []byte)
}

// Transport emulates the virtio MMIO register space.
// Embed this in each device.
type Transport struct {
	dev     Device
	mem     []byte
	basePA  uint64 // guest physical base of MMIO region
	irqLine uint8
	irqFn   func(bool) // callback: assert(true) or deassert(false) IRQ

	status         uint32
	drvFeatures    uint64
	devFeaturesSel uint32
	drvFeaturesSel uint32
	queueSel       uint32
	shmSel         uint32
	interruptStat  uint32

	queues [8]*Queue
}

// NewTransport creates a transport for a device at guest MMIO address base.
func NewTransport(dev Device, mem []byte, base uint64, irq uint8, dirty *DirtyTracker, irqFn func(bool)) *Transport {
	t := &Transport{dev: dev, mem: mem, basePA: base, irqLine: irq, irqFn: irqFn}
	for i := range t.queues {
		t.queues[i] = NewQueue(mem, 256, dirty)
	}
	return t
}

// SetGuestPhysBase sets the guest RAM base address on all queues in this
// transport. On x86 this is 0; on ARM64 it is 0x80000000.
func (t *Transport) SetGuestPhysBase(base uint64) {
	for _, q := range t.queues {
		if q != nil {
			q.guestPhysBase = base
		}
	}
}

// Read handles a guest MMIO read at offset within the device's region.
// It is kept as a convenience wrapper for tests; the VMM uses ReadBytes so
// reads honor the real MMIO access width like Firecracker does.
func (t *Transport) Read(offset uint32, size uint8) uint32 {
	buf := make([]byte, size)
	t.ReadBytes(offset, buf)
	if len(buf) >= 4 {
		return binary.LittleEndian.Uint32(buf[:4])
	}
	var tmp [4]byte
	copy(tmp[:], buf)
	return binary.LittleEndian.Uint32(tmp[:])
}

// ReadBytes handles a guest MMIO read at offset within the device's region.
func (t *Transport) ReadBytes(offset uint32, data []byte) {
	for i := range data {
		data[i] = 0
	}

	switch {
	case offset < RegConfig:
		if len(data) != 4 {
			return
		}
		var v uint32
		switch offset {
		case RegMagic:
			v = 0x74726976 // "virt"
		case RegVersion:
			v = 2
		case RegDeviceID:
			v = t.dev.DeviceID()
		case RegVendorID:
			v = 0x554D4551 // "QEMU" vendor
		case RegDevFeatures:
			feat := t.dev.DeviceFeatures() | (1 << 32) // VIRTIO_F_VERSION_1
			if t.devFeaturesSel == 1 {
				v = uint32(feat >> 32)
			} else {
				v = uint32(feat)
			}
		case RegQueueNumMax:
			v = MaxQueueSize
		case RegQueueReady:
			v = boolU32(t.queues[t.queueSel].Ready)
		case RegInterruptStat:
			v = t.interruptStat
		case RegStatus:
			v = t.status
		case RegSHMLenLow, RegSHMLenHigh, RegSHMBaseLow, RegSHMBaseHigh:
			base, length, ok := t.sharedMemoryRegion()
			if !ok {
				base = 0
				length = ^uint64(0)
			}
			switch offset {
			case RegSHMLenLow:
				v = uint32(length)
			case RegSHMLenHigh:
				v = uint32(length >> 32)
			case RegSHMBaseLow:
				v = uint32(base)
			case RegSHMBaseHigh:
				v = uint32(base >> 32)
			}
		case RegConfigGeneration:
			v = 0
		default:
			return
		}
		binary.LittleEndian.PutUint32(data, v)
	case offset >= RegConfig:
		cfg := t.dev.ConfigBytes()
		start := int(offset - RegConfig)
		if start < 0 || start >= len(cfg) {
			return
		}
		copy(data, cfg[start:])
	}
}

// Write handles a guest MMIO write at offset within the device's region.
// It is kept as a convenience wrapper for tests; the VMM uses WriteBytes so
// writes honor the real MMIO access width like Firecracker does.
func (t *Transport) Write(offset uint32, val uint32) {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], val)
	t.WriteBytes(offset, buf[:])
}

// WriteBytes handles a guest MMIO write at offset within the device's region.
func (t *Transport) WriteBytes(offset uint32, data []byte) {
	if offset >= RegConfig {
		if writer, ok := t.dev.(ConfigWriter); ok {
			writer.WriteConfig(offset-RegConfig, data)
		}
		return
	}
	if len(data) != 4 {
		return
	}

	val := binary.LittleEndian.Uint32(data)
	q := t.queues[t.queueSel]
	notifyStateChange := func() {
		if observer, ok := t.dev.(StateChangeObserver); ok {
			observer.OnTransportStateChange(t)
		}
	}
	resetTransport := func() {
		t.status = 0
		t.drvFeatures = 0
		t.devFeaturesSel = 0
		t.drvFeaturesSel = 0
		t.queueSel = 0
		t.shmSel = 0
		t.interruptStat = 0
		for _, queue := range t.queues {
			queue.Reset()
		}
		if t.irqFn != nil {
			t.irqFn(false)
		}
		if observer, ok := t.dev.(ResetObserver); ok {
			observer.OnTransportReset(t)
		}
	}
	switch offset {
	case RegDevFeaturesSel:
		t.devFeaturesSel = val
	case RegDrvFeaturesSel:
		t.drvFeaturesSel = val
	case RegDrvFeatures:
		if t.drvFeaturesSel == 0 {
			t.drvFeatures = (t.drvFeatures &^ 0xFFFFFFFF) | uint64(val)
		} else {
			t.drvFeatures = (t.drvFeatures & 0xFFFFFFFF) | (uint64(val) << 32)
		}
		notifyStateChange()
	case RegQueueSel:
		if val < 8 {
			t.queueSel = val
		}
	case RegSHMSel:
		t.shmSel = val
	case RegQueueNum:
		if val >= 1 && val <= MaxQueueSize {
			q.Size = val
			notifyStateChange()
		}
	case RegQueueReady:
		q.Ready = val != 0
		notifyStateChange()
	case RegQueueNotify:
		if val < 8 && t.queues[val].Ready {
			raiseIRQ := true
			if interceptor, ok := t.dev.(QueueNotifyInterceptor); ok {
				raiseIRQ = interceptor.HandleQueueNotify(val, t.queues[val])
			} else {
				t.dev.HandleQueue(val, t.queues[val])
			}
			if raiseIRQ {
				t.interruptStat |= 1
				if t.irqFn != nil {
					t.irqFn(true)
				}
			}
		}
	case RegInterruptACK:
		t.interruptStat &^= val
		if t.interruptStat == 0 && t.irqFn != nil {
			t.irqFn(false)
		}
	case RegStatus:
		if val == 0 {
			resetTransport()
			return
		}
		prevStatus := t.status
		t.status = val
		notifyStateChange()
		// When the driver transitions to DRIVER_OK (bit 2), activate
		// the link if the device supports it, then raise a config change
		// interrupt so the virtio-net driver detects carrier up.
		// Matches Firecracker behavior.
		if val&4 != 0 && prevStatus&4 == 0 {
			if la, ok := t.dev.(interface{ ActivateLink() }); ok {
				la.ActivateLink()
				t.interruptStat |= 2 // config change
				if t.irqFn != nil {
					t.irqFn(true)
				}
			}
		}
	case RegQueueDescLow:
		q.DescAddr = (q.DescAddr &^ 0xFFFFFFFF) | uint64(val)
		notifyStateChange()
	case RegQueueDescHigh:
		q.DescAddr = (q.DescAddr & 0xFFFFFFFF) | (uint64(val) << 32)
		notifyStateChange()
	case RegQueueDriverLow:
		q.DriverAddr = (q.DriverAddr &^ 0xFFFFFFFF) | uint64(val)
		notifyStateChange()
	case RegQueueDriverHigh:
		q.DriverAddr = (q.DriverAddr & 0xFFFFFFFF) | (uint64(val) << 32)
		notifyStateChange()
	case RegQueueDeviceLow:
		q.DeviceAddr = (q.DeviceAddr &^ 0xFFFFFFFF) | uint64(val)
		notifyStateChange()
	case RegQueueDeviceHigh:
		q.DeviceAddr = (q.DeviceAddr & 0xFFFFFFFF) | (uint64(val) << 32)
		notifyStateChange()
	}
}

func (t *Transport) sharedMemoryRegion() (base uint64, length uint64, ok bool) {
	provider, ok := t.dev.(SharedMemoryProvider)
	if !ok {
		return 0, 0, false
	}
	return provider.SharedMemoryRegion(uint8(t.shmSel))
}

// BasePA returns the MMIO base address of this device.
func (t *Transport) BasePA() uint64 { return t.basePA }

// IRQLine returns the IRQ line for this device.
func (t *Transport) IRQLine() uint8 { return t.irqLine }

// Queue returns the virtqueue at the given index.
func (t *Transport) Queue(idx int) *Queue { return t.queues[idx] }

// SetInterruptStat sets interrupt status bits.
func (t *Transport) SetInterruptStat(bits uint32) { t.interruptStat |= bits }

// Mem returns the guest memory slice.
func (t *Transport) Mem() []byte { return t.mem }

// InterruptStat returns the current interrupt status.
func (t *Transport) InterruptStat() uint32 { return t.interruptStat }

// SignalIRQ calls the IRQ callback if set.
func (t *Transport) SignalIRQ(assert bool) {
	if t.irqFn != nil {
		t.irqFn(assert)
	}
}

// String describes the transport for debugging.
func (t *Transport) String() string {
	return fmt.Sprintf("virtio-mmio@%#x irq=%d", t.basePA, t.irqLine)
}

// State returns a snapshot of the queue state.
func (q *Queue) State() QueueState {
	q.mu.Lock()
	defer q.mu.Unlock()
	return QueueState{
		Size: q.Size, Ready: q.Ready, LastAvail: q.LastAvail,
		DescAddr: q.DescAddr, DriverAddr: q.DriverAddr, DeviceAddr: q.DeviceAddr,
	}
}

// RestoreState restores queue state from a snapshot.
func (q *Queue) RestoreState(s QueueState) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.Size = s.Size
	q.Ready = s.Ready
	q.LastAvail = s.LastAvail
	q.DescAddr = s.DescAddr
	q.DriverAddr = s.DriverAddr
	q.DeviceAddr = s.DeviceAddr
}

// State returns a snapshot of the transport state.
func (t *Transport) State() TransportState {
	s := TransportState{
		Status: t.status, DrvFeatures: t.drvFeatures,
		DevFeaturesSel: t.devFeaturesSel, DrvFeaturesSel: t.drvFeaturesSel,
		QueueSel: t.queueSel, InterruptStat: t.interruptStat,
	}
	for _, q := range t.queues {
		s.Queues = append(s.Queues, q.State())
	}
	return s
}

// RestoreState restores transport state from a snapshot.
func (t *Transport) RestoreState(s TransportState) {
	t.status = s.Status
	t.drvFeatures = s.DrvFeatures
	t.devFeaturesSel = s.DevFeaturesSel
	t.drvFeaturesSel = s.DrvFeaturesSel
	t.queueSel = s.QueueSel
	t.interruptStat = s.InterruptStat
	for i, qs := range s.Queues {
		if i < len(t.queues) {
			t.queues[i].RestoreState(qs)
		}
	}
}

func boolU32(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}
