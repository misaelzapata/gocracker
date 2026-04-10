package virtio

import (
	"crypto/rand"

	gclog "github.com/gocracker/gocracker/internal/log"
)

// DeviceIDRNG is the virtio device ID for entropy source.
const DeviceIDRNG = 4

// RNGDevice is a virtio-rng device that provides entropy to the guest
// from the host's crypto/rand (backed by /dev/urandom).
type RNGDevice struct {
	*Transport
	rl *RateLimiter
}

// NewRNGDevice creates a virtio-rng device.
func NewRNGDevice(mem []byte, basePA uint64, irq uint8, dirty *DirtyTracker, irqFn func(bool)) *RNGDevice {
	d := &RNGDevice{}
	d.Transport = NewTransport(d, mem, basePA, irq, dirty, irqFn)
	return d
}

func (d *RNGDevice) SetRateLimiter(rl *RateLimiter) {
	d.rl = rl
}

func (d *RNGDevice) DeviceID() uint32       { return DeviceIDRNG }
func (d *RNGDevice) DeviceFeatures() uint64 { return 0 }
func (d *RNGDevice) ConfigBytes() []byte    { return nil }

// HandleQueue fills guest buffers with random bytes.
func (d *RNGDevice) HandleQueue(idx uint32, q *Queue) {
	if idx != 0 {
		return
	}
	if err := q.IterAvail(func(head uint16) {
		chain, err := q.WalkChain(head)
		if err != nil {
			gclog.VMM.Warn("virtio-rng invalid descriptor chain", "head", head, "error", err)
			_ = q.PushUsedLocked(uint32(head), 0)
			return
		}
		total := uint32(0)
		for _, desc := range chain {
			if desc.Flags&DescFlagWrite == 0 {
				continue
			}
			total += desc.Len
		}
		if d.rl != nil {
			d.rl.Wait(uint64(total), 1)
		}
		written := uint32(0)
		for _, desc := range chain {
			if desc.Flags&DescFlagWrite == 0 {
				continue
			}
			buf := make([]byte, desc.Len)
			if _, err := rand.Read(buf); err != nil {
				gclog.VMM.Warn("virtio-rng entropy read failed", "error", err)
				break
			}
			if err := q.GuestWrite(desc.Addr, buf); err != nil {
				gclog.VMM.Warn("virtio-rng guest write failed", "head", head, "error", err)
				break
			}
			written += desc.Len
		}
		_ = q.PushUsedLocked(uint32(head), written)
	}); err != nil {
		gclog.VMM.Warn("virtio-rng queue iteration failed", "error", err)
	}
}
