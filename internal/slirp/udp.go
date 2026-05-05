package slirp

import (
	"encoding/binary"
)

// buildUDPDatagram returns a UDP datagram (header + payload) with checksum
// computed over the provided IPv4 pseudo-header and src/dst addresses.
// srcPort/dstPort are host-byte-order uint16s.
func buildUDPDatagram(srcIP, dstIP []byte, srcPort, dstPort uint16, payload []byte) []byte {
	udp := make([]byte, udpHeaderLen+len(payload))
	binary.BigEndian.PutUint16(udp[0:2], srcPort)
	binary.BigEndian.PutUint16(udp[2:4], dstPort)
	binary.BigEndian.PutUint16(udp[4:6], uint16(udpHeaderLen+len(payload)))
	binary.BigEndian.PutUint16(udp[6:8], 0) // checksum placeholder
	copy(udp[udpHeaderLen:], payload)

	// Pseudo-header: src, dst, zero, proto, udp_length
	var pseudo [12]byte
	copy(pseudo[0:4], srcIP)
	copy(pseudo[4:8], dstIP)
	pseudo[8] = 0
	pseudo[9] = ipProtoUDP
	binary.BigEndian.PutUint16(pseudo[10:12], uint16(udpHeaderLen+len(payload)))
	sum := onesComplementSum(0, pseudo[:])
	sum = onesComplementSum(sum, udp)
	folded := foldChecksum(sum)
	if folded == 0 {
		folded = 0xFFFF // RFC 768: 0 means "no checksum"; use ~0 instead.
	}
	binary.BigEndian.PutUint16(udp[6:8], folded)
	return udp
}

// parseUDP returns srcPort, dstPort and payload from a UDP datagram.
func parseUDP(buf []byte) (srcPort, dstPort uint16, payload []byte, ok bool) {
	if len(buf) < udpHeaderLen {
		return 0, 0, nil, false
	}
	srcPort = binary.BigEndian.Uint16(buf[0:2])
	dstPort = binary.BigEndian.Uint16(buf[2:4])
	length := int(binary.BigEndian.Uint16(buf[4:6]))
	if length < udpHeaderLen || length > len(buf) {
		return 0, 0, nil, false
	}
	return srcPort, dstPort, buf[udpHeaderLen:length], true
}
