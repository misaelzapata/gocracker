package virtio

// NetBackend is the host-side carrier for a virtio-net device. The TAP backend
// (the historical default) speaks to a Linux kernel TAP fd; alternative
// backends like slirp speak to an in-process userspace network stack. Both
// produce and consume Ethernet frames prefixed with the 12-byte
// virtio_net_hdr_v1 that the guest expects when VIRTIO_NET_F_MRG_RXBUF is
// negotiated.
//
// ReadFrame should return frames as they arrive. When no frame is ready the
// backend should return a transient error (syscall.EAGAIN or syscall.EINTR);
// the receive pump treats those specially and polls for shutdown without
// busy-spinning. After Close the backend should return a shutdown error
// (syscall.EBADF or any other error in isTapShutdownError) so the receive
// pump exits.
type NetBackend interface {
	ReadFrame(buf []byte) (int, error)
	WriteFrame(pkt []byte) error
	Close() error
	// Name returns a short identifier for logs (e.g. "tap:tap0", "slirp").
	Name() string
}
