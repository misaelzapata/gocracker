package slirp

import (
	"encoding/binary"
)

// Minimal ICMPv4: respond to echo requests addressed to the gateway. This
// is mostly a smoke-test affordance — guests use `ping 10.0.2.2` to verify
// the userspace stack is alive before trusting UDP/TCP. We do not proxy
// ICMP to the host (that would need raw sockets / CAP_NET_RAW).

const (
	icmpTypeEchoReply   = 0
	icmpTypeEchoRequest = 8
)

func (s *Slirp) handleICMP(ip ipv4, _ ethHeader) {
	if !ip.Dst.Equal(gatewayIP) {
		// Could ping out to the world, but that requires CAP_NET_RAW
		// on the host which defeats the rootless promise. Drop.
		return
	}
	if len(ip.Payload) < 4 {
		return
	}
	if ip.Payload[0] != icmpTypeEchoRequest {
		return
	}
	s.stats.ICMPEchoes.Add(1)
	// Build the echo reply: flip type to 0, recompute checksum, keep
	// identifier/sequence/data verbatim.
	reply := make([]byte, len(ip.Payload))
	copy(reply, ip.Payload)
	reply[0] = icmpTypeEchoReply
	reply[2] = 0
	reply[3] = 0
	binary.BigEndian.PutUint16(reply[2:4], foldChecksum(onesComplementSum(0, reply)))

	s.mu.Lock()
	dstMAC := append([]byte{}, s.guestMAC...)
	s.mu.Unlock()
	if dstMAC == nil {
		return
	}
	s.enqueueRX(emitIPv4Frame(dstMAC, gatewayIP, ip.Src, ipProtoICMP, reply))
}
