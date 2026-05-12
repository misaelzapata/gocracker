//go:build windows

package vmm

import (
	"encoding/binary"
	"sync"
)

// Port of node-vmm's native/whp/virtio/desc.h to Go. virtio-mmio v2
// constants + register offsets + descriptor-chain walker.
//
// The constants match qemu/include/standard-headers/linux/virtio_mmio.h
// and virtio_ring.h, so any Linux guest that speaks virtio-mmio v2 talks
// to these device implementations.
//
// All virtio devices on the WHP path (blk, net, rng, etc.) reuse these
// constants and the descriptor walking primitives below. A spec bump
// only needs to land in one place.

const (
	VirtioMagic       uint32 = 0x74726976 // "virt"
	VirtioVendor      uint32 = 0x554D4551 // "QEMU"
	VirtioVersion     uint32 = 2
	VirtioMaxQueueSize uint32 = 256

	// Status bits the driver writes to status register (offset 0x070).
	VirtioStatusAck       uint32 = 0x01
	VirtioStatusDriver    uint32 = 0x02
	VirtioStatusDriverOk  uint32 = 0x04
	VirtioStatusFeaturesOk uint32 = 0x08
	VirtioStatusFailed    uint32 = 0x80

	// Descriptor flags.
	VirtioDescFNext     uint16 = 0x1
	VirtioDescFWrite    uint16 = 0x2
	VirtioDescFIndirect uint16 = 0x4
	VirtioAvailFNoInterrupt uint16 = 0x1

	// Common feature bits.
	VirtioFVersion1 uint64 = 1 << 32

	// Interrupt-status register bit meanings (offset 0x060).
	VirtioInterruptVring  uint32 = 0x1
	VirtioInterruptConfig uint32 = 0x2

	// virtio-mmio register offsets from virtio_mmio.h.
	VirtioMmioMagicValue        = 0x000
	VirtioMmioVersion           = 0x004
	VirtioMmioDeviceId          = 0x008
	VirtioMmioVendorId          = 0x00C
	VirtioMmioDeviceFeatures    = 0x010
	VirtioMmioDeviceFeaturesSel = 0x014
	VirtioMmioDriverFeatures    = 0x020
	VirtioMmioDriverFeaturesSel = 0x024
	VirtioMmioQueueSel          = 0x030
	VirtioMmioQueueNumMax       = 0x034
	VirtioMmioQueueNum          = 0x038
	VirtioMmioQueueReady        = 0x044
	VirtioMmioQueueNotify       = 0x050
	VirtioMmioInterruptStatus   = 0x060
	VirtioMmioInterruptAck      = 0x064
	VirtioMmioStatus            = 0x070
	VirtioMmioQueueDescLow      = 0x080
	VirtioMmioQueueDescHigh     = 0x084
	VirtioMmioQueueDriverLow    = 0x090
	VirtioMmioQueueDriverHigh   = 0x094
	VirtioMmioQueueDeviceLow    = 0x0A0
	VirtioMmioQueueDeviceHigh   = 0x0A4
	VirtioMmioConfigGeneration  = 0x0FC
	VirtioMmioConfig            = 0x100
)

// VirtqDesc mirrors the in-memory layout of a virtqueue descriptor
// (16 bytes total in the guest's RAM).
type VirtqDesc struct {
	Addr  uint64
	Len   uint32
	Flags uint16
	Next  uint16
}

// ReadVirtqDesc reads a 16-byte descriptor from guest RAM at addr.
// The caller's mem slice aliases the host-side view of guest RAM (the
// VirtualAlloc'd buffer from HVVM.AllocateGuestRAM). Returns the
// descriptor + ok=false if addr is out of bounds.
func ReadVirtqDesc(mem []byte, addr uint64) (VirtqDesc, bool) {
	if addr+16 > uint64(len(mem)) {
		return VirtqDesc{}, false
	}
	return VirtqDesc{
		Addr:  binary.LittleEndian.Uint64(mem[addr : addr+8]),
		Len:   binary.LittleEndian.Uint32(mem[addr+8 : addr+12]),
		Flags: binary.LittleEndian.Uint16(mem[addr+12 : addr+14]),
		Next:  binary.LittleEndian.Uint16(mem[addr+14 : addr+16]),
	}, true
}

// VirtioMmioQueue tracks per-queue state on a virtio-mmio device.
// Mirrors node-vmm's `Queue` struct embedded inside each device.
type VirtioMmioQueue struct {
	Size       uint32 // negotiated queue size (≤ VirtioMaxQueueSize)
	Ready      bool   // driver wrote 1 to QueueReady — queue is live
	LastAvail  uint16 // next entry in the available ring we haven't processed
	DescAddr   uint64 // guest physical address of descriptor table
	DriverAddr uint64 // GPA of driver area (available ring)
	DeviceAddr uint64 // GPA of device area (used ring)
}

// VirtioMmioBase carries the common register state every virtio-mmio
// device needs (status, features, queue selector, interrupt status).
// Device-specific config lives at offset 0x100+ and is handled by each
// device's read/write helpers, not here.
type VirtioMmioBase struct {
	mu sync.Mutex

	DeviceID  uint32 // 1=net, 2=blk, 4=rng, 26=vsock (subset of virtio IDs)
	Status    uint32 // STATUS register (status bits the driver wrote)
	DevFeatures uint64 // OR of features this device offers
	DrvFeatures uint64 // features the driver acked

	DevFeaturesSel uint32 // 0 → expose bits 0..31; 1 → expose bits 32..63
	DrvFeaturesSel uint32 // same for driver-side reads

	QueueSel    uint32 // currently selected queue index
	IntrStatus  uint32 // INTERRUPT_STATUS bits (vring/config)
	ConfigGen   uint32 // bumped on every config-space change

	Queues [16]VirtioMmioQueue
}

// ReadCommon dispatches reads to the common virtio-mmio registers.
// Returns the value + true for known offsets; the device-specific read
// path handles offsets ≥ 0x100 (Config space).
func (b *VirtioMmioBase) ReadCommon(off uint32) (uint32, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch off {
	case VirtioMmioMagicValue:
		return VirtioMagic, true
	case VirtioMmioVersion:
		return VirtioVersion, true
	case VirtioMmioDeviceId:
		return b.DeviceID, true
	case VirtioMmioVendorId:
		return VirtioVendor, true
	case VirtioMmioDeviceFeatures:
		if b.DevFeaturesSel == 0 {
			return uint32(b.DevFeatures), true
		}
		return uint32(b.DevFeatures >> 32), true
	case VirtioMmioQueueNumMax:
		return VirtioMaxQueueSize, true
	case VirtioMmioQueueReady:
		q := &b.Queues[b.QueueSel%uint32(len(b.Queues))]
		if q.Ready {
			return 1, true
		}
		return 0, true
	case VirtioMmioInterruptStatus:
		return b.IntrStatus, true
	case VirtioMmioStatus:
		return b.Status, true
	case VirtioMmioConfigGeneration:
		return b.ConfigGen, true
	}
	return 0, false
}

// WriteCommon dispatches writes to the common virtio-mmio registers.
// onQueueNotify is invoked when the driver writes the QueueNotify
// register; the device-specific code handles requests there.
//
// Returns true if the offset was handled, false if the device needs to
// handle a config-space write at offset ≥ 0x100.
func (b *VirtioMmioBase) WriteCommon(off, value uint32, onQueueNotify func(queue uint32)) bool {
	b.mu.Lock()
	switch off {
	case VirtioMmioDeviceFeaturesSel:
		b.DevFeaturesSel = value
		b.mu.Unlock()
		return true
	case VirtioMmioDriverFeatures:
		if b.DrvFeaturesSel == 0 {
			b.DrvFeatures = (b.DrvFeatures &^ 0xFFFFFFFF) | uint64(value)
		} else {
			b.DrvFeatures = (b.DrvFeatures & 0xFFFFFFFF) | (uint64(value) << 32)
		}
		b.mu.Unlock()
		return true
	case VirtioMmioDriverFeaturesSel:
		b.DrvFeaturesSel = value
		b.mu.Unlock()
		return true
	case VirtioMmioQueueSel:
		b.QueueSel = value
		b.mu.Unlock()
		return true
	case VirtioMmioQueueNum:
		q := &b.Queues[b.QueueSel%uint32(len(b.Queues))]
		q.Size = value
		b.mu.Unlock()
		return true
	case VirtioMmioQueueReady:
		q := &b.Queues[b.QueueSel%uint32(len(b.Queues))]
		q.Ready = value != 0
		b.mu.Unlock()
		return true
	case VirtioMmioQueueNotify:
		b.mu.Unlock()
		if onQueueNotify != nil {
			onQueueNotify(value)
		}
		return true
	case VirtioMmioInterruptAck:
		b.IntrStatus &^= value
		b.mu.Unlock()
		return true
	case VirtioMmioStatus:
		b.Status = value
		b.mu.Unlock()
		return true
	case VirtioMmioQueueDescLow:
		q := &b.Queues[b.QueueSel%uint32(len(b.Queues))]
		q.DescAddr = (q.DescAddr &^ 0xFFFFFFFF) | uint64(value)
		b.mu.Unlock()
		return true
	case VirtioMmioQueueDescHigh:
		q := &b.Queues[b.QueueSel%uint32(len(b.Queues))]
		q.DescAddr = (q.DescAddr & 0xFFFFFFFF) | (uint64(value) << 32)
		b.mu.Unlock()
		return true
	case VirtioMmioQueueDriverLow:
		q := &b.Queues[b.QueueSel%uint32(len(b.Queues))]
		q.DriverAddr = (q.DriverAddr &^ 0xFFFFFFFF) | uint64(value)
		b.mu.Unlock()
		return true
	case VirtioMmioQueueDriverHigh:
		q := &b.Queues[b.QueueSel%uint32(len(b.Queues))]
		q.DriverAddr = (q.DriverAddr & 0xFFFFFFFF) | (uint64(value) << 32)
		b.mu.Unlock()
		return true
	case VirtioMmioQueueDeviceLow:
		q := &b.Queues[b.QueueSel%uint32(len(b.Queues))]
		q.DeviceAddr = (q.DeviceAddr &^ 0xFFFFFFFF) | uint64(value)
		b.mu.Unlock()
		return true
	case VirtioMmioQueueDeviceHigh:
		q := &b.Queues[b.QueueSel%uint32(len(b.Queues))]
		q.DeviceAddr = (q.DeviceAddr & 0xFFFFFFFF) | (uint64(value) << 32)
		b.mu.Unlock()
		return true
	}
	b.mu.Unlock()
	return false
}

// CurrentQueue returns a pointer to the currently selected queue.
// Caller must serialise access (devices typically hold a separate mu).
func (b *VirtioMmioBase) CurrentQueue() *VirtioMmioQueue {
	return &b.Queues[b.QueueSel%uint32(len(b.Queues))]
}

// SignalUsed atomically sets the interrupt-status bit for a vring
// notification. Combined with the device's raise_irq callback this is
// what tells the guest "the used ring has new entries".
func (b *VirtioMmioBase) SignalUsed() {
	b.mu.Lock()
	b.IntrStatus |= VirtioInterruptVring
	b.mu.Unlock()
}
