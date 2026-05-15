package slirp

import (
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Slirp is the userspace network stack. It implements virtio.NetBackend so
// it drops into virtio.NewNetDeviceWithBackend wherever a TAP fd would have
// gone. The MVP scope (this branch) handles ARP, DHCPv4 and ICMP echo to
// the gateway, plus UDP outbound NAT incl. DNS forwarding. TCP outbound
// NAT is conditionally compiled: the default build drops TCP frames and
// bumps a metric, while -tags=slirp_gvisor drops in a real gVisor netstack
// for full outbound + port-forward support.
type Slirp struct {
	// rxQueue carries fully-formed Ethernet frames (with the 12-byte
	// virtio_net_hdr prefix) destined for the guest. It's drained by
	// virtio-net's rxPump via ReadFrame.
	rxQueue chan []byte

	closed atomic.Bool
	done   chan struct{}

	mu       sync.Mutex
	guestMAC net.HardwareAddr // learned from the first guest frame; before that we use the synthesized broadcast addr

	// onClose is run once when Close() fires. Slirp doesn't currently own
	// long-lived sockets in MVP scope, but TCP/UDP NAT will hang
	// per-flow conns here when those land.
	onClose []func()

	// portfwd holds the host→guest port-forwarding registry. It's owned
	// by the Slirp instance so the TCP dispatcher can consult it without
	// needing extra plumbing, and so Close() can tear down host listeners.
	portfwd *PortFwdRegistry

	// tcp is the per-instance TCP backend state. With the default build
	// tag it's an empty struct (handleTCP is a stub); with slirp_gvisor
	// it holds the gVisor stack + channel endpoint.
	tcp *tcpStack

	// udpFlows tracks active outbound UDP NAT entries so the idle janitor
	// can prune dead sessions.
	udpFlows *udpFlowTable

	// stats — exposed for testing & operator debugging.
	stats Stats
}

// Stats counts frames per category. Exposed for tests and metrics.
type Stats struct {
	ARPRequests  atomic.Uint64
	ARPReplies   atomic.Uint64
	DHCPMessages atomic.Uint64
	ICMPEchoes   atomic.Uint64
	UDPOut       atomic.Uint64
	UDPIn        atomic.Uint64
	TCPDropped   atomic.Uint64 // until TCP NAT lands
	UnknownDrop  atomic.Uint64
}

// New creates a fresh Slirp engine. The returned value is safe to share
// across goroutines.
func New() *Slirp {
	s := &Slirp{
		rxQueue:  make(chan []byte, 256),
		done:     make(chan struct{}),
		portfwd:  NewPortFwdRegistry(),
		udpFlows: newUDPFlowTable(),
	}
	s.tcpInit()
	return s
}

// PortFwd exposes the port-forwarding registry so operators can register
// host→guest rules before VM boot. Returns the live registry; the
// returned value is owned by the Slirp instance and torn down on Close.
func (s *Slirp) PortFwd() *PortFwdRegistry { return s.portfwd }

// Snapshot returns a point-in-time copy of the counters. Useful for tests.
func (s *Slirp) Snapshot() map[string]uint64 {
	return map[string]uint64{
		"arp_requests":  s.stats.ARPRequests.Load(),
		"arp_replies":   s.stats.ARPReplies.Load(),
		"dhcp_messages": s.stats.DHCPMessages.Load(),
		"icmp_echoes":   s.stats.ICMPEchoes.Load(),
		"udp_out":       s.stats.UDPOut.Load(),
		"udp_in":        s.stats.UDPIn.Load(),
		"tcp_dropped":   s.stats.TCPDropped.Load(),
		"unknown_drop":  s.stats.UnknownDrop.Load(),
	}
}

// Name reports a stable identifier used in logs.
func (s *Slirp) Name() string { return "slirp" }

// Close drains pending replies and flips the engine into shutdown so any
// blocked ReadFrame returns a shutdown error promptly.
func (s *Slirp) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	close(s.done)
	if s.udpFlows != nil {
		s.udpFlows.closeAll()
	}
	if s.portfwd != nil {
		s.portfwd.closeAll()
	}
	s.tcpClose()
	s.mu.Lock()
	for _, fn := range s.onClose {
		fn()
	}
	s.onClose = nil
	s.mu.Unlock()
	return nil
}

// ReadFrame implements virtio.NetBackend. It blocks on the synthesized RX
// queue with a periodic wake-up so the receive pump can observe shutdown.
func (s *Slirp) ReadFrame(buf []byte) (int, error) {
	if s.closed.Load() {
		return 0, syscall.EBADF
	}
	const tick = 50 * time.Millisecond
	select {
	case frame := <-s.rxQueue:
		n := copy(buf, frame)
		return n, nil
	case <-time.After(tick):
		return 0, syscall.EAGAIN
	case <-s.done:
		return 0, syscall.EBADF
	}
}

// WriteFrame implements virtio.NetBackend. It dispatches the frame to the
// appropriate handler (ARP / IPv4) and lets the handler enqueue any reply
// onto the RX queue. Returns nil even on packet drops — silent drops are
// the canonical behaviour of a usermode netstack.
func (s *Slirp) WriteFrame(pkt []byte) error {
	if s.closed.Load() {
		return syscall.EBADF
	}
	// Strip the 12-byte virtio_net_hdr the guest prepends; the actual
	// Ethernet frame starts at +12.
	if len(pkt) < vnetHeaderLen+ethHeaderLen {
		s.stats.UnknownDrop.Add(1)
		return nil
	}
	frame := pkt[vnetHeaderLen:]
	eth, body, err := parseEth(frame)
	if err != nil {
		s.stats.UnknownDrop.Add(1)
		return nil
	}
	s.mu.Lock()
	if s.guestMAC == nil {
		s.guestMAC = append(net.HardwareAddr{}, eth.Src[:]...)
	}
	s.mu.Unlock()
	switch eth.EtherType {
	case ethTypeARP:
		s.handleARP(body)
	case ethTypeIPv4:
		s.handleIPv4(body, eth)
	default:
		s.stats.UnknownDrop.Add(1)
	}
	return nil
}

// enqueueRX pushes a fully-built Ethernet frame (with virtio header) onto
// the RX queue. Drops the frame if the queue is saturated — the guest will
// retransmit if it cares.
func (s *Slirp) enqueueRX(frame []byte) {
	if s.closed.Load() {
		return
	}
	select {
	case s.rxQueue <- frame:
	default:
		// queue full; drop. Same backpressure model as a kernel TAP
		// hitting txqueuelen.
	}
}

func (s *Slirp) handleARP(body []byte) {
	pkt, ok := parseARP(body)
	if !ok {
		s.stats.UnknownDrop.Add(1)
		return
	}
	s.stats.ARPRequests.Add(1)
	if pkt.Op != arpOpRequest {
		return
	}
	target := pkt.TargetIP.To4()
	if target == nil {
		return
	}
	// Only answer for IPs we synthesize: gateway and DNS.
	if !target.Equal(gatewayIP) && !target.Equal(dnsIP) {
		return
	}
	reply := buildARPReply(pkt)
	s.stats.ARPReplies.Add(1)
	s.enqueueRX(reply)
}

func (s *Slirp) handleIPv4(body []byte, eth ethHeader) {
	ip, err := parseIPv4(body)
	if err != nil {
		s.stats.UnknownDrop.Add(1)
		return
	}
	switch ip.Proto {
	case ipProtoUDP:
		s.handleUDP(ip, eth)
	case ipProtoICMP:
		s.handleICMP(ip, eth)
	case ipProtoTCP:
		// Pass the IPv4 packet (header + TCP payload) to handleTCP.
		// Default build: counts & drops. slirp_gvisor build: injects
		// into the gVisor stack for full TCP NAT.
		s.handleTCP(body)
	default:
		s.stats.UnknownDrop.Add(1)
	}
}
