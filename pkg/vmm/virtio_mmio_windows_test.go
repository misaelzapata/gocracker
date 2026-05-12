//go:build windows

package vmm

import (
	"encoding/binary"
	"testing"
)

// TestVirtioMmioRegisterRoundTrip exercises the virtio-mmio common
// register handling — the device-side state machine the guest driver
// drives during init. Doesn't touch WHP at all; pure pkg/vmm.
func TestVirtioMmioRegisterRoundTrip(t *testing.T) {
	b := &VirtioMmioBase{DeviceID: 2 /* virtio-blk */, DevFeatures: VirtioFVersion1}

	// Magic, version, device ID, vendor — fixed read-only values.
	if v, ok := b.ReadCommon(VirtioMmioMagicValue); !ok || v != VirtioMagic {
		t.Errorf("MagicValue = %#x ok=%v; want %#x true", v, ok, VirtioMagic)
	}
	if v, _ := b.ReadCommon(VirtioMmioVersion); v != VirtioVersion {
		t.Errorf("Version = %d; want %d", v, VirtioVersion)
	}
	if v, _ := b.ReadCommon(VirtioMmioDeviceId); v != 2 {
		t.Errorf("DeviceId = %d; want 2", v)
	}
	if v, _ := b.ReadCommon(VirtioMmioVendorId); v != VirtioVendor {
		t.Errorf("VendorId = %#x; want %#x", v, VirtioVendor)
	}
	if v, _ := b.ReadCommon(VirtioMmioQueueNumMax); v != VirtioMaxQueueSize {
		t.Errorf("QueueNumMax = %d; want %d", v, VirtioMaxQueueSize)
	}

	// Driver advertises FeaturesSel=1, expects bit-32 (VIRTIO_F_VERSION_1).
	b.WriteCommon(VirtioMmioDeviceFeaturesSel, 1, nil)
	if v, _ := b.ReadCommon(VirtioMmioDeviceFeatures); v != 1 {
		t.Errorf("DeviceFeatures (sel=1) = %#x; want 1 (VIRTIO_F_VERSION_1)", v)
	}
	b.WriteCommon(VirtioMmioDeviceFeaturesSel, 0, nil)
	if v, _ := b.ReadCommon(VirtioMmioDeviceFeatures); v != 0 {
		t.Errorf("DeviceFeatures (sel=0) = %#x; want 0", v)
	}

	// Driver negotiates: writes 1 to bit 32 of DrvFeatures.
	b.WriteCommon(VirtioMmioDriverFeaturesSel, 1, nil)
	b.WriteCommon(VirtioMmioDriverFeatures, 1, nil)
	if b.DrvFeatures != VirtioFVersion1 {
		t.Errorf("DrvFeatures = %#x; want %#x", b.DrvFeatures, VirtioFVersion1)
	}

	// Queue programming: select queue 0, set size, configure ring
	// addresses, mark ready.
	b.WriteCommon(VirtioMmioQueueSel, 0, nil)
	b.WriteCommon(VirtioMmioQueueNum, 128, nil)
	b.WriteCommon(VirtioMmioQueueDescLow, 0x10000, nil)
	b.WriteCommon(VirtioMmioQueueDescHigh, 0, nil)
	b.WriteCommon(VirtioMmioQueueDriverLow, 0x20000, nil)
	b.WriteCommon(VirtioMmioQueueDeviceLow, 0x30000, nil)
	b.WriteCommon(VirtioMmioQueueReady, 1, nil)

	q := b.CurrentQueue()
	if q.Size != 128 {
		t.Errorf("queue.Size = %d; want 128", q.Size)
	}
	if !q.Ready {
		t.Error("queue.Ready = false; want true")
	}
	if q.DescAddr != 0x10000 {
		t.Errorf("queue.DescAddr = %#x; want 0x10000", q.DescAddr)
	}
	if q.DriverAddr != 0x20000 {
		t.Errorf("queue.DriverAddr = %#x; want 0x20000", q.DriverAddr)
	}
	if q.DeviceAddr != 0x30000 {
		t.Errorf("queue.DeviceAddr = %#x; want 0x30000", q.DeviceAddr)
	}

	// QueueNotify dispatches to the device's callback with the queue index.
	notified := uint32(0xFFFFFFFF)
	b.WriteCommon(VirtioMmioQueueNotify, 7, func(q uint32) { notified = q })
	if notified != 7 {
		t.Errorf("QueueNotify callback got %d; want 7", notified)
	}

	// SignalUsed sets the vring interrupt bit; InterruptAck clears bits.
	b.SignalUsed()
	if v, _ := b.ReadCommon(VirtioMmioInterruptStatus); v&VirtioInterruptVring == 0 {
		t.Errorf("after SignalUsed: InterruptStatus = %#x; want vring bit set", v)
	}
	b.WriteCommon(VirtioMmioInterruptAck, VirtioInterruptVring, nil)
	if v, _ := b.ReadCommon(VirtioMmioInterruptStatus); v != 0 {
		t.Errorf("after InterruptAck: InterruptStatus = %#x; want 0", v)
	}
}

// TestReadVirtqDesc verifies the descriptor layout matches what a Linux
// virtio driver writes into guest RAM (16 bytes: addr/len/flags/next).
func TestReadVirtqDesc(t *testing.T) {
	mem := make([]byte, 4096)
	// Write a descriptor at offset 0x100.
	binary.LittleEndian.PutUint64(mem[0x100:], 0xDEADBEEFCAFE0000)
	binary.LittleEndian.PutUint32(mem[0x108:], 0x1234)
	binary.LittleEndian.PutUint16(mem[0x10C:], VirtioDescFNext|VirtioDescFWrite)
	binary.LittleEndian.PutUint16(mem[0x10E:], 0x55)

	d, ok := ReadVirtqDesc(mem, 0x100)
	if !ok {
		t.Fatal("ReadVirtqDesc returned ok=false")
	}
	if d.Addr != 0xDEADBEEFCAFE0000 {
		t.Errorf("Addr = %#x; want 0xDEADBEEFCAFE0000", d.Addr)
	}
	if d.Len != 0x1234 {
		t.Errorf("Len = %#x; want 0x1234", d.Len)
	}
	if d.Flags != (VirtioDescFNext | VirtioDescFWrite) {
		t.Errorf("Flags = %#x; want NEXT|WRITE", d.Flags)
	}
	if d.Next != 0x55 {
		t.Errorf("Next = %#x; want 0x55", d.Next)
	}

	// Out-of-bounds returns ok=false rather than panicking.
	if _, ok := ReadVirtqDesc(mem, uint64(len(mem))-15); ok {
		t.Error("ReadVirtqDesc near end of mem should return ok=false (not enough bytes)")
	}
}
