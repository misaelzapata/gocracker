//go:build windows

package vmm

import (
	"crypto/rand"
	"sync"
)

// VirtioRng is a minimal virtio-rng-mmio device. The guest queues
// descriptor chains describing buffers it wants filled with random
// bytes; we fill them from the host's CSPRNG (crypto/rand) and push
// the used entries back. Mirrors node-vmm's
// native/whp/virtio/rng.cc.
//
// Why we have this: the Linux kernel's early init blocks on entropy
// during initramfs init (especially for systemd, ASLR re-randomisation,
// and most network stacks). Without an entropy source, the guest can
// hang for minutes waiting for /dev/random to seed.
type VirtioRng struct {
	base     *VirtioMmioBase
	mmioBase uint64
	mem      []byte
	raiseIRQ func()

	mu sync.Mutex
}

// NewVirtioRng constructs a virtio-rng device at mmioBase. The IRQ
// callback is invoked after each queue notify that produces entries.
func NewVirtioRng(mmioBase uint64, mem []byte, raiseIRQ func()) *VirtioRng {
	return &VirtioRng{
		base: &VirtioMmioBase{
			DeviceID:    4, // virtio-rng (entropy)
			DevFeatures: VirtioFVersion1,
		},
		mmioBase: mmioBase,
		mem:      mem,
		raiseIRQ: raiseIRQ,
	}
}

// MmioBase / MmioEnd / HandlesAddr mirror VirtioBlk's surface so the
// boot session can route both devices through a single dispatcher.
func (d *VirtioRng) MmioBase() uint64        { return d.mmioBase }
func (d *VirtioRng) MmioEnd() uint64         { return d.mmioBase + 0x1000 }
func (d *VirtioRng) HandlesAddr(a uint64) bool { return a >= d.mmioBase && a < d.MmioEnd() }

// ReadMMIO services common-register reads. virtio-rng has no
// device-specific config — every byte at offset ≥ 0x100 reads as 0.
func (d *VirtioRng) ReadMMIO(addr uint64, length uint32) uint32 {
	off := uint32(addr - d.mmioBase)
	if off >= 0x100 || length != 4 {
		return 0
	}
	v, _ := d.base.ReadCommon(off)
	return v
}

// WriteMMIO services common-register writes.
func (d *VirtioRng) WriteMMIO(addr uint64, length uint32, value uint32) {
	off := uint32(addr - d.mmioBase)
	if off >= 0x100 || length != 4 {
		return
	}
	d.base.WriteCommon(off, value, func(queue uint32) {
		if queue != 0 {
			return
		}
		d.handleQueue()
	})
}

// handleQueue drains the available ring, filling each descriptor's
// writable buffer with random bytes.
func (d *VirtioRng) handleQueue() {
	d.mu.Lock()
	defer d.mu.Unlock()
	q := d.base.CurrentQueue()
	if !q.Ready {
		return
	}
	availIdx, ok := readU16(d.mem, q.DriverAddr+2)
	if !ok {
		return
	}
	processed := false
	for q.LastAvail != availIdx {
		ringOff := q.DriverAddr + 4 + uint64(q.LastAvail%uint16(q.Size))*2
		head, ok := readU16(d.mem, ringOff)
		if !ok {
			break
		}
		q.LastAvail++
		written := d.fillChain(q, head)
		pushUsed(d.mem, q.DeviceAddr, uint16(q.Size), uint32(head), written)
		processed = true
	}
	if processed {
		d.base.SignalUsed()
		if d.raiseIRQ != nil {
			d.raiseIRQ()
		}
	}
}

// fillChain walks the descriptor chain at head, filling each writable
// descriptor with random bytes. Returns the total bytes written.
func (d *VirtioRng) fillChain(q *VirtioMmioQueue, head uint16) uint32 {
	var written uint32
	chain, err := walkChain(d.mem, q.DescAddr, uint16(q.Size), head)
	if err != nil {
		return 0
	}
	for _, desc := range chain {
		if desc.Flags&VirtioDescFWrite == 0 || desc.Len == 0 {
			continue
		}
		buf, ok := readBytes(d.mem, desc.Addr, desc.Len)
		if !ok {
			break
		}
		if _, err := rand.Read(buf); err != nil {
			// crypto/rand only fails when the OS RNG is unavailable;
			// fall back to leaving the buffer zero (still safer than
			// leaking host memory).
			break
		}
		written += desc.Len
	}
	return written
}
