//go:build slirp_gvisor

package slirp

import (
	"context"
	"encoding/binary"
	"net"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	gvipv4 "gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// tcpDropped tracks frames that hit the stack but never made it to a
// successful connection. With the slirp_gvisor build tag this is mostly
// "stack closed" and "no TCP handler installed" cases — the common path
// (active connection) doesn't bump this counter.
var tcpDropped atomic.Uint64

// tcpInjected counts frames successfully handed to the gVisor stack.
// Useful for tests to verify the dispatch loop is alive.
var tcpInjected atomic.Uint64

// gvisorNICID is the NIC the channel endpoint binds to. There's only
// one NIC in the slirp stack so the constant is fine.
const gvisorNICID tcpip.NICID = 1

// tcpStack bundles the gVisor stack with its channel endpoint and the
// goroutine that pumps outbound packets back onto the guest RX queue.
type tcpStack struct {
	stack    *stack.Stack
	endpoint *channel.Endpoint
	cancel   context.CancelFunc
	done     chan struct{}
}

// tcpInit constructs the gVisor stack on first call. It is invoked from
// Slirp.New() so the stack is ready before the first packet arrives.
func (s *Slirp) tcpInit() {
	// We need AllowExternalLoopbackTraffic so the stack accepts SYNs
	// addressed to 127.0.0.0/8 — both unit tests and real callers may
	// dial a localhost service through the gVisor stack, and gVisor
	// defaults to dropping such packets as "martian".
	st := stack.New(stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{
			gvipv4.NewProtocolWithOptions(gvipv4.Options{
				AllowExternalLoopbackTraffic: true,
			}),
			arp.NewProtocol,
		},
		TransportProtocols: []stack.TransportProtocolFactory{
			tcp.NewProtocol,
		},
		// HandleLocal=false: we want the stack to terminate the TCP
		// connection (act as the peer) rather than route it elsewhere.
		HandleLocal: false,
	})

	// 1500 is the standard Ethernet MTU; the guest negotiates the same
	// value via virtio-net so framing matches end-to-end.
	ep := channel.New(256, 1500, tcpip.LinkAddress(gatewayMAC))

	if err := st.CreateNIC(gvisorNICID, ep); err != nil {
		// The only way this fails is OOM / programmer error; we crash
		// loudly rather than degrade silently to a TCP-less stack.
		panic("slirp: gvisor CreateNIC: " + err.String())
	}

	// Assign the gateway IP to the NIC so the stack accepts TCP segments
	// addressed to it. We also enable promiscuous + spoofing so the stack
	// accepts SYNs to arbitrary external IPs (the guest dials out by IP)
	// and replies with the matching source address.
	gwAddr := tcpip.AddrFrom4Slice(gatewayIP)
	protoAddr := tcpip.ProtocolAddress{
		Protocol:          gvipv4.ProtocolNumber,
		AddressWithPrefix: gwAddr.WithPrefix(),
	}
	if err := st.AddProtocolAddress(gvisorNICID, protoAddr, stack.AddressProperties{}); err != nil {
		panic("slirp: gvisor AddProtocolAddress(gw): " + err.String())
	}
	if err := st.SetPromiscuousMode(gvisorNICID, true); err != nil {
		panic("slirp: gvisor SetPromiscuousMode: " + err.String())
	}
	if err := st.SetSpoofing(gvisorNICID, true); err != nil {
		panic("slirp: gvisor SetSpoofing: " + err.String())
	}
	// Default route through the gateway IP so outbound segments leaving
	// the stack are addressed correctly.
	st.SetRouteTable([]tcpip.Route{{
		Destination: header.IPv4EmptySubnet,
		NIC:         gvisorNICID,
	}})

	// Install a TCP forwarder. The forwarder accepts SYNs the stack
	// hasn't bound a listener for and gives us a chance to either dial
	// out to the host (the outbound NAT case) or reject. With a 256-
	// connection in-flight cap we match QEMU/libslirp defaults.
	fwd := tcp.NewForwarder(st, 0, 256, func(r *tcp.ForwarderRequest) {
		s.tcpAccept(r)
	})
	st.SetTransportProtocolHandler(tcp.ProtocolNumber, fwd.HandlePacket)

	ctx, cancel := context.WithCancel(context.Background())
	tcpst := &tcpStack{
		stack:    st,
		endpoint: ep,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	s.tcp = tcpst
	go s.tcpPump(ctx)
}

// tcpClose drops the gVisor stack. Called from Slirp.Close().
func (s *Slirp) tcpClose() {
	if s.tcp == nil {
		return
	}
	s.tcp.cancel()
	s.tcp.endpoint.Close()
	s.tcp.stack.Close()
	// Wait for the pump goroutine to acknowledge shutdown so callers
	// can rely on Close() draining everything synchronously.
	select {
	case <-s.tcp.done:
	case <-time.After(2 * time.Second):
	}
	s.tcp = nil
}

// tcpPump drains outbound packets from the gVisor channel endpoint and
// pushes them onto the slirp RX queue so virtio-net hands them to the
// guest. Exits when ctx is cancelled.
func (s *Slirp) tcpPump(ctx context.Context) {
	defer close(s.tcp.done)
	for {
		pkt := s.tcp.endpoint.ReadContext(ctx)
		if pkt == nil {
			return
		}
		// Convert the gVisor PacketBuffer into a flat byte slice. The
		// buffer is composed of layered views; ToView() flattens it.
		view := pkt.ToView()
		ipFrame := view.AsSlice()
		view.Release()
		pkt.DecRef()

		// gVisor doesn't add an Ethernet header — channel.Endpoint
		// works at the network layer. Wrap the IPv4 packet in a
		// fresh ethernet frame addressed to the guest, then prepend
		// the virtio_net_hdr the guest expects.
		s.mu.Lock()
		dstMAC := append([]byte{}, s.guestMAC...)
		s.mu.Unlock()
		if dstMAC == nil {
			tcpDropped.Add(1)
			continue
		}
		frame := make([]byte, vnetHeaderLen+ethHeaderLen+len(ipFrame))
		writeEth(frame[vnetHeaderLen:], gatewayMAC, dstMAC, ethTypeIPv4)
		copy(frame[vnetHeaderLen+ethHeaderLen:], ipFrame)
		s.enqueueRX(frame)
	}
}

// handleTCP feeds a guest-originated TCP frame into the gVisor stack.
// The `eth` argument is the raw Ethernet payload (IP header + TCP); we
// inject just the IPv4 portion since the channel endpoint operates at
// the network layer.
func (s *Slirp) handleTCP(eth []byte) ([]byte, bool) {
	if s.tcp == nil {
		tcpDropped.Add(1)
		s.stats.TCPDropped.Add(1)
		return nil, false
	}
	// The frame as handed in by slirp.go is the post-Ethernet IPv4
	// header + payload. Inject it as-is.
	buf := buffer.MakeWithData(eth)
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buf,
	})
	s.tcp.endpoint.InjectInbound(gvipv4.ProtocolNumber, pkt)
	pkt.DecRef()
	tcpInjected.Add(1)
	return nil, false
}

// tcpAccept is invoked by the TCP forwarder for each SYN the stack
// receives. For outbound (guest → external) the destination address is
// the real external host; we honor port-forward registrations for the
// reverse direction (host → guest) by dialing the guest endpoint.
func (s *Slirp) tcpAccept(r *tcp.ForwarderRequest) {
	id := r.ID()
	// LocalAddress / LocalPort = where the SYN was sent (the target).
	// RemoteAddress / RemotePort = the source (guest).
	dstIP := net.IP(id.LocalAddress.AsSlice())
	dstPort := id.LocalPort

	// If a port-forward rule covers this (proto,ip,port), redirect to
	// the registered guest target. Otherwise dial out to the real
	// destination using the host's stack.
	var dialAddr string
	if pf, ok := s.portfwd.Lookup("tcp", dstIP, dstPort); ok {
		dialAddr = net.JoinHostPort(pf.GuestIP.String(), itoa(pf.GuestPort))
	} else {
		dialAddr = net.JoinHostPort(dstIP.String(), itoa(dstPort))
	}

	// Dial the external endpoint *before* completing the SYN handshake
	// so we can return RST if the destination is unreachable. 5 s is the
	// same connect timeout libslirp uses.
	dialer := net.Dialer{Timeout: 5 * time.Second}
	hostConn, err := dialer.Dial("tcp", dialAddr)
	if err != nil {
		r.Complete(true) // sendReset = true
		tcpDropped.Add(1)
		return
	}

	// Complete the handshake on the gVisor side and pair it with the
	// host-side conn.
	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	if terr != nil {
		_ = hostConn.Close()
		r.Complete(true)
		tcpDropped.Add(1)
		return
	}
	r.Complete(false)

	guestConn := gonet.NewTCPConn(&wq, ep)
	go pipeTCP(guestConn, hostConn)
}

// pipeTCP shuttles bytes between a guest-side gonet.TCPConn and a host-
// side net.Conn. Closes both sides on first error so half-closed states
// don't strand goroutines.
func pipeTCP(guest, host net.Conn) {
	done := make(chan struct{}, 2)
	copyOne := func(dst, src net.Conn) {
		buf := make([]byte, 32*1024)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		done <- struct{}{}
	}
	go copyOne(guest, host)
	go copyOne(host, guest)
	<-done
	_ = guest.Close()
	_ = host.Close()
}

// itoa is a tiny helper to render a uint16 port without pulling in
// strconv for one line.
func itoa(p uint16) string {
	var b [5]byte
	i := len(b)
	if p == 0 {
		return "0"
	}
	for p > 0 {
		i--
		b[i] = byte('0' + p%10)
		p /= 10
	}
	return string(b[i:])
}

// TCPMetrics returns the lifetime count of TCP frames the gVisor stack
// has accepted from the guest. Mirrors the stub's API so callers don't
// need to know which backend is compiled in.
func TCPMetrics() uint64 { return tcpInjected.Load() }

// Compile-time guard: keep these imports anchored so go vet doesn't
// trim them if the helpers become unused during refactors.
var _ = binary.BigEndian
