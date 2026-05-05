package slirp

import (
	"net"
)

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
	// Outbound UDP NAT path (DNS, NTP, etc.) is the next chunk of work.
	// See docs/design/slirp-udp.md for the planned implementation.
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
