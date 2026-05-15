package slirp

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// udpIdleTimeout is how long a UDP flow may sit silent before the
// janitor closes it. 30 s matches RFC 4787 REQ-5 / libslirp's default.
const udpIdleTimeout = 30 * time.Second

// udpJanitorInterval is how often the janitor wakes to sweep idle flows.
// Picking 5 s gives O(idleTimeout/interval) ≈ 6 sweeps per session,
// which is plenty of resolution without burning CPU on idle VMs.
const udpJanitorInterval = 5 * time.Second

// udpFlow tracks one outbound UDP NAT mapping. Each guest (srcPort, dst,
// dstPort) tuple becomes a host net.PacketConn that ferries datagrams
// out to the destination and reads replies for the guest. lastActivity
// is the wall-clock time of the most recent guest or host packet on
// this flow.
type udpFlow struct {
	key          udpFlowKey
	conn         net.PacketConn
	guestMAC     net.HardwareAddr
	guestSrcIP   net.IP
	lastActivity atomic.Int64 // unix-nanos; atomically updated by both directions
}

// udpFlowKey is the (guestPort, dstIP, dstPort) tuple keying udpFlowTable.
// guestIP is implicit (we only serve one guest in MVP scope) so it's not
// part of the key.
type udpFlowKey struct {
	GuestPort uint16
	DstIP     [4]byte
	DstPort   uint16
}

// udpFlowTable is a thread-safe map of active UDP flows plus the janitor
// goroutine that prunes idle entries.
type udpFlowTable struct {
	mu     sync.Mutex
	flows  map[udpFlowKey]*udpFlow
	stop   chan struct{}
	closed atomic.Bool
	now    func() time.Time // injectable clock for tests
}

// newUDPFlowTable creates an empty table and starts the janitor.
func newUDPFlowTable() *udpFlowTable {
	t := &udpFlowTable{
		flows: make(map[udpFlowKey]*udpFlow),
		stop:  make(chan struct{}),
		now:   time.Now,
	}
	go t.janitor()
	return t
}

// janitor sweeps idle flows on a fixed cadence. Closes & removes any
// flow whose lastActivity is older than udpIdleTimeout.
func (t *udpFlowTable) janitor() {
	tick := time.NewTicker(udpJanitorInterval)
	defer tick.Stop()
	for {
		select {
		case <-t.stop:
			return
		case <-tick.C:
			t.sweep()
		}
	}
}

// sweep is the side-effecting heart of the janitor; factored out so
// tests can fire it deterministically without waiting on the ticker.
func (t *udpFlowTable) sweep() {
	cutoff := t.now().Add(-udpIdleTimeout).UnixNano()
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, f := range t.flows {
		if f.lastActivity.Load() < cutoff {
			_ = f.conn.Close()
			delete(t.flows, k)
		}
	}
}

// closeAll shuts every flow and stops the janitor. Safe to call more
// than once; subsequent calls no-op.
func (t *udpFlowTable) closeAll() {
	if t.closed.Swap(true) {
		return
	}
	close(t.stop)
	t.mu.Lock()
	for k, f := range t.flows {
		_ = f.conn.Close()
		delete(t.flows, k)
	}
	t.mu.Unlock()
}

// len returns the current number of tracked flows. Used by tests.
func (t *udpFlowTable) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.flows)
}

// handleUDP routes an inbound UDP datagram from the guest. Two cases:
//
//  1. dst == gateway, dstPort == 67 → DHCPv4 server. We synthesize an
//     OFFER or ACK and unicast it back to the guest (broadcast in the
//     unfortunate case where the guest hasn't claimed an IP yet).
//
//  2. dst == DNS or any external IP → outbound UDP NAT (deferred —
//     scaffolded but not wired up in this branch). The intended shape is
//     a per-flow net.PacketConn dialed on demand and torn down on Close.
//
// All other destinations are dropped.
func (s *Slirp) handleUDP(ip ipv4, eth ethHeader) {
	srcPort, dstPort, payload, ok := parseUDP(ip.Payload)
	if !ok {
		s.stats.UnknownDrop.Add(1)
		return
	}
	// DHCP traffic before the guest has an IP is broadcast; afterwards
	// (renewals) it can be unicast to the gateway. Accept both.
	if dstPort == dhcpServerPort && srcPort == dhcpClientPort &&
		(ip.Dst.Equal(gatewayIP) || ip.Dst.Equal(net.IPv4bcast.To4()) || ip.Dst[3] == 0xFF) {
		s.handleDHCP(payload, eth)
		return
	}
	// Outbound UDP NAT path (DNS, NTP, etc.) — track the flow so the
	// idle janitor can prune dead sessions even when the upstream NAT
	// step lands in a follow-up patch.
	if s.udpFlows != nil && !s.udpFlows.closed.Load() {
		key := udpFlowKey{
			GuestPort: srcPort,
			DstPort:   dstPort,
		}
		copy(key.DstIP[:], ip.Dst.To4())
		s.udpFlows.mu.Lock()
		if f, ok := s.udpFlows.flows[key]; ok {
			f.lastActivity.Store(s.udpFlows.now().UnixNano())
		}
		s.udpFlows.mu.Unlock()
	}
	s.stats.UDPOut.Add(1)
}

// handleDHCP services DISCOVER/REQUEST. The reply is unicast to the guest
// MAC we already learned (no need for the broadcast bit because we can
// always address by MAC at L2 even before the guest has an IP).
func (s *Slirp) handleDHCP(payload []byte, eth ethHeader) {
	msg, ok := parseDHCP(payload)
	if !ok {
		s.stats.UnknownDrop.Add(1)
		return
	}
	s.stats.DHCPMessages.Add(1)
	var reply []byte
	switch msg.MsgType {
	case dhcpDiscover:
		reply = buildDHCPReply(msg, dhcpOffer)
	case dhcpRequest:
		reply = buildDHCPReply(msg, dhcpAck)
	default:
		// Other types (RELEASE/INFORM/etc.) we don't implement.
		return
	}
	dstMAC := append(net.HardwareAddr{}, eth.Src[:]...)
	udp := buildUDPDatagram(gatewayIP, net.IPv4bcast.To4(), dhcpServerPort, dhcpClientPort, reply)
	frame := emitIPv4Frame(dstMAC, gatewayIP, net.IPv4bcast.To4(), ipProtoUDP, udp)
	s.enqueueRX(frame)
}
