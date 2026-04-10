package virtio

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"

	gclog "github.com/gocracker/gocracker/internal/log"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

var tapReadFrameFn = readTapFrame
var tapTransientRetryDelay = 5 * time.Millisecond

// virtio-net feature bits
const (
	NetFeatureMac      = 1 << 5
	NetFeatureMrgRxBuf = 1 << 15
	NetFeatureStatus   = 1 << 16

	// Firecracker advertises VIRTIO_NET_F_MRG_RXBUF and uses virtio_net_hdr_v1
	// end-to-end in the frame path, so the host TAP fd also carries this 12-byte
	// header.
	//
	// Matching that layout keeps the guest virtio-net driver and the TAP backend
	// in agreement about the framing.
	//
	// Firecracker opens TAP with IFF_VNET_HDR and sets the vnet header size to
	// TAP with IFF_VNET_HDR, so the host TAP fd also carries this 12-byte header.
	netHeaderLen = 12
)

// virtio-net config space (MAC + status)
type netConfig struct {
	MAC    [6]byte
	Status uint16
}

// NetDevice is a virtio-net device backed by a Linux TAP interface.
type NetDevice struct {
	*Transport
	cfg     netConfig
	tapFd   *os.File
	tapName string
	rl      *RateLimiter
}

// NewNetDevice creates a virtio-net device with a TAP backend.
// mac is a 6-byte hardware address; tapName is the kernel interface name (e.g. "tap0").
func NewNetDevice(mem []byte, basePA uint64, irq uint8, mac net.HardwareAddr, tapName string, dirty *DirtyTracker, irqFn func(bool)) (*NetDevice, error) {
	d := &NetDevice{}
	copy(d.cfg.MAC[:], mac)
	d.cfg.Status = 0 // starts link-down; flipped to link-up on DRIVER_OK via ActivateLink()

	// Open /dev/net/tun and configure the TAP interface
	tap, err := openTAP(tapName)
	if err != nil {
		return nil, fmt.Errorf("TAP %s: %w", tapName, err)
	}
	d.tapFd = tap
	d.tapName = tapName
	d.Transport = NewTransport(d, mem, basePA, irq, dirty, irqFn)

	// Start receive pump: TAP -> guest virtqueue
	go d.rxPump()
	return d, nil
}

func (d *NetDevice) SetRateLimiter(rl *RateLimiter) {
	d.rl = rl
}

// ActivateLink sets the link status to UP. Called by the transport when
// DRIVER_OK is set, triggering a config change interrupt so the guest
// virtio-net driver detects the carrier. Matches Firecracker's behavior
// where link status transitions from down to up after driver init.
func (d *NetDevice) ActivateLink() {
	d.cfg.Status = 1
}

func (d *NetDevice) DeviceID() uint32 { return DeviceIDNet }
func (d *NetDevice) DeviceFeatures() uint64 {
	return NetFeatureMac | NetFeatureMrgRxBuf | NetFeatureStatus
}
func (d *NetDevice) ConfigBytes() []byte {
	b := make([]byte, 8)
	copy(b, d.cfg.MAC[:])
	binary.LittleEndian.PutUint16(b[6:], d.cfg.Status)
	return b
}

func (d *NetDevice) Close() error {
	var errs []string
	if d.tapFd != nil {
		if err := d.tapFd.Close(); err != nil && !strings.Contains(err.Error(), "file already closed") {
			errs = append(errs, err.Error())
		}
		d.tapFd = nil
	}
	if d.tapName != "" {
		link, err := netlink.LinkByName(d.tapName)
		switch {
		case err == nil:
			if err := netlink.LinkDel(link); err != nil {
				errs = append(errs, err.Error())
			}
		case !strings.Contains(err.Error(), "Link not found"):
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("close net device: %s", strings.Join(errs, "; "))
	}
	return nil
}

// HandleQueue is called when the guest notifies a queue.
// Queue 0 = RX (guest provides buffers), Queue 1 = TX (guest sends packets).
func (d *NetDevice) HandleQueue(idx uint32, q *Queue) {
	if idx == 1 {
		d.transmit(q)
	}
	// RX buffers are consumed by rxPump goroutine
}

// transmit drains the TX queue and writes packets to the TAP fd.
func (d *NetDevice) transmit(q *Queue) {
	if err := q.IterAvail(func(head uint16) {
		chain, err := q.WalkChain(head)
		if err != nil {
			gclog.VMM.Warn("virtio-net invalid TX descriptor chain", "head", head, "error", err)
			_ = q.PushUsedLocked(uint32(head), 0)
			return
		}
		var pkt []byte
		for _, desc := range chain {
			buf := make([]byte, desc.Len)
			if err := q.GuestRead(desc.Addr, buf); err != nil {
				gclog.VMM.Warn("virtio-net TX guest read failed", "head", head, "error", err)
				_ = q.PushUsedLocked(uint32(head), 0)
				return
			}
			pkt = append(pkt, buf...)
		}
		if d.rl != nil {
			d.rl.Wait(uint64(len(pkt)), 1)
		}
		_ = writeTapFrame(int(d.tapFd.Fd()), pkt)
		_ = q.PushUsedLocked(uint32(head), 0)
	}); err != nil {
		gclog.VMM.Warn("virtio-net TX queue iteration failed", "error", err)
	}
}

// rxPump reads packets from TAP and places them into the RX virtqueue.
// Uses manual avail ring access instead of IterAvail to avoid holding
// the lock across the blocking TAP read.
func (d *NetDevice) rxPump() {
	rxQ := d.Transport.queues[0]
	buf := make([]byte, 65536)
	for {
		n, err := tapReadFrameFn(int(d.tapFd.Fd()), buf)
		if err != nil {
			if isTapShutdownError(err) {
				return
			}
			if isTapTransientReadError(err) {
				if tapTransientRetryDelay > 0 {
					time.Sleep(tapTransientRetryDelay)
				}
				continue
			}
			return
		}
		if n < netHeaderLen {
			continue
		}
		rxQ.mu.Lock()
		ready := rxQ.Ready
		rxQ.mu.Unlock()
		if !ready {
			continue
		}
		if written, ok := d.deliverRXPacket(buf[:n]); ok {
			_ = written
			d.Transport.SetInterruptStat(1)
			d.Transport.SignalIRQ(true)
		}
	}
}

func (d *NetDevice) deliverRXPacket(pkt []byte) (uint32, bool) {
	rxQ := d.Transport.queues[0]
	if len(pkt) < netHeaderLen || !rxQ.Ready {
		return 0, false
	}
	if d.rl != nil {
		d.rl.Wait(uint64(len(pkt)), 1)
	}

	rxQ.mu.Lock()
	defer rxQ.mu.Unlock()

	avail, err := rxQ.readAvail()
	if err != nil {
		gclog.VMM.Warn("virtio-net RX avail ring read failed", "error", err)
		return 0, false
	}
	if rxQ.LastAvail == avail.Idx {
		return 0, false
	}
	head := avail.Ring[rxQ.LastAvail%uint16(rxQ.Size)]
	rxQ.LastAvail++

	chain, err := rxQ.WalkChain(head)
	if err != nil {
		gclog.VMM.Warn("virtio-net invalid RX descriptor chain", "head", head, "error", err)
		return 0, false
	}
	if len(chain) == 0 {
		return 0, false
	}

	if len(pkt) >= netHeaderLen {
		// With VIRTIO_NET_F_MRG_RXBUF negotiated, the guest expects
		// num_buffers to describe how many receive buffers were consumed.
		// We currently deliver each frame into a single available head chain.
		binary.LittleEndian.PutUint16(pkt[10:12], 1)
	}
	written := uint32(0)
	for _, desc := range chain {
		if desc.Flags&DescFlagWrite == 0 {
			continue
		}
		sz := uint32(len(pkt))
		if sz > desc.Len {
			sz = desc.Len
		}
		if err := rxQ.GuestWrite(desc.Addr, pkt[:sz]); err != nil {
			gclog.VMM.Warn("virtio-net RX guest write failed", "head", head, "error", err)
			return written, false
		}
		pkt = pkt[sz:]
		written += sz
		if len(pkt) == 0 {
			break
		}
	}
	_ = rxQ.PushUsedLocked(uint32(head), written)
	return written, true
}

// ---- TAP helpers ----

func readTapFrame(fd int, buf []byte) (int, error) {
	for {
		n, err := unix.Read(fd, buf)
		switch {
		case err == nil:
			return n, nil
		case err == syscall.EINTR:
			continue
		default:
			return 0, err
		}
	}
}

func writeTapFrame(fd int, pkt []byte) error {
	for len(pkt) > 0 {
		n, err := unix.Write(fd, pkt)
		switch {
		case err == nil:
			pkt = pkt[n:]
		case err == syscall.EINTR:
			continue
		default:
			return err
		}
	}
	return nil
}

func isTapShutdownError(err error) bool {
	return err == syscall.EBADF || err == syscall.EIO || err == syscall.EINVAL || err == syscall.ENODEV || err == syscall.EFAULT
}

func isTapTransientReadError(err error) bool {
	return err == syscall.EINTR || err == syscall.EAGAIN || err == syscall.ENOBUFS
}

const (
	tunSetIFF       = 0x400454CA
	tunGetIFF       = 0x800454D2
	tunSetVnetHdrSz = 0x400454d8
	iffTAP          = 0x0002
	iffNoPi         = 0x1000
	iffVnetHdr      = 0x4000
)

type ifreq struct {
	Name  [16]byte
	Flags uint16
	_     [22]byte
}

func openTAP(name string) (*os.File, error) {
	f, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	ifr := ifreq{Flags: iffTAP | iffNoPi | iffVnetHdr}
	copy(ifr.Name[:], name)
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL,
		f.Fd(), tunSetIFF, uintptr(unsafe.Pointer(&ifr))); errno != 0 {
		f.Close()
		return nil, fmt.Errorf("TUNSETIFF: %w", errno)
	}
	size := int32(netHeaderLen)
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL,
		f.Fd(), tunSetVnetHdrSz, uintptr(unsafe.Pointer(&size))); errno != 0 {
		f.Close()
		return nil, fmt.Errorf("TUNSETVNETHDRSZ: %w", errno)
	}
	return f, nil
}
