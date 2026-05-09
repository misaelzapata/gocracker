package slirp

import (
	"encoding/binary"
	"errors"
	"net"
)

// Network constants. These deliberately match QEMU's libslirp defaults so
// guests configured for either backend behave identically.
var (
	subnetIP  = net.IPv4(10, 0, 2, 0).To4()
	subnetMsk = net.IPv4Mask(255, 255, 255, 0)
	guestIP   = net.IPv4(10, 0, 2, 15).To4()
	gatewayIP = net.IPv4(10, 0, 2, 2).To4()
	dnsIP     = net.IPv4(10, 0, 2, 3).To4()

	// gatewayMAC is the synthetic MAC the slirp engine answers ARPs with.
	// Using QEMU's well-known prefix (52:55:0A) keeps the guest-side
	// experience identical between backends.
	gatewayMAC = net.HardwareAddr{0x52, 0x55, 0x0A, 0x00, 0x02, 0x02}
	// dnsMAC is the MAC for the DNS sub-IP. ARP-wise it's the same logical
	// host as the gateway; we keep a separate constant only so frames look
	// right when a curious guest snoops.
	dnsMAC = net.HardwareAddr{0x52, 0x55, 0x0A, 0x00, 0x02, 0x03}
)

const (
	ethTypeIPv4 = 0x0800
	ethTypeARP  = 0x0806

	ipProtoICMP = 1
	ipProtoTCP  = 6
	ipProtoUDP  = 17

	ethHeaderLen = 14
	arpPacketLen = 28
	ipHeaderMin  = 20
	udpHeaderLen = 8

	// vnetHeaderLen mirrors virtio.netHeaderLen — every frame on the
	// virtio side carries a 12-byte virtio_net_hdr_v1 prefix because we
	// negotiate VIRTIO_NET_F_MRG_RXBUF.
	vnetHeaderLen = 12
)

var errShortFrame = errors.New("slirp: short frame")

// ethHeader is a fixed 14-byte Ethernet II header (no VLAN tag).
type ethHeader struct {
	Dst       [6]byte
	Src       [6]byte
	EtherType uint16
}

func parseEth(buf []byte) (ethHeader, []byte, error) {
	if len(buf) < ethHeaderLen {
		return ethHeader{}, nil, errShortFrame
	}
	var h ethHeader
	copy(h.Dst[:], buf[0:6])
	copy(h.Src[:], buf[6:12])
	h.EtherType = binary.BigEndian.Uint16(buf[12:14])
	return h, buf[ethHeaderLen:], nil
}

// writeEth writes a 14-byte Ethernet header into dst. Caller must ensure
// dst has at least ethHeaderLen bytes available.
func writeEth(dst, srcMAC, dstMAC []byte, etherType uint16) {
	copy(dst[0:6], dstMAC)
	copy(dst[6:12], srcMAC)
	binary.BigEndian.PutUint16(dst[12:14], etherType)
}

// ipv4 holds a parsed IPv4 header plus a slice over the payload.
type ipv4 struct {
	IHL      int
	TotalLen int
	TTL      uint8
	Proto    uint8
	Src      net.IP
	Dst      net.IP
	Payload  []byte
}

func parseIPv4(buf []byte) (ipv4, error) {
	if len(buf) < ipHeaderMin {
		return ipv4{}, errShortFrame
	}
	ihl := int(buf[0]&0x0F) * 4
	if ihl < ipHeaderMin || len(buf) < ihl {
		return ipv4{}, errShortFrame
	}
	total := int(binary.BigEndian.Uint16(buf[2:4]))
	if total < ihl || len(buf) < total {
		return ipv4{}, errShortFrame
	}
	return ipv4{
		IHL:      ihl,
		TotalLen: total,
		TTL:      buf[8],
		Proto:    buf[9],
		Src:      net.IP(buf[12:16]).To4(),
		Dst:      net.IP(buf[16:20]).To4(),
		Payload:  buf[ihl:total],
	}, nil
}

// ipv4Checksum computes the standard IPv4 header checksum (RFC 1071) over
// hdr. Bytes 10..11 (the existing checksum field) are zero-treated by
// callers before invoking this.
func ipv4Checksum(hdr []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(hdr); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(hdr[i : i+2]))
	}
	if len(hdr)%2 == 1 {
		sum += uint32(hdr[len(hdr)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xFFFF) + (sum >> 16)
	}
	return ^uint16(sum)
}

// onesComplementSum runs the running ones-complement sum used for both IPv4
// header checksums and the UDP/TCP pseudo-header + payload checksum.
func onesComplementSum(initial uint32, b []byte) uint32 {
	sum := initial
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i : i+2]))
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	return sum
}

func foldChecksum(sum uint32) uint16 {
	for sum>>16 != 0 {
		sum = (sum & 0xFFFF) + (sum >> 16)
	}
	return ^uint16(sum)
}

// emitIPv4Frame builds a complete Ethernet+IPv4 frame (with the 12-byte
// virtio_net_hdr prefix the guest expects) carrying payload as the IP
// payload. proto is the IP protocol number; src/dst are 4-byte IPv4
// addresses; dstMAC is the destination MAC (the guest's, in practice).
//
// The caller is responsible for any L4 checksum already embedded in
// payload.
func emitIPv4Frame(dstMAC []byte, src, dst net.IP, proto uint8, payload []byte) []byte {
	const ipLen = ipHeaderMin
	total := vnetHeaderLen + ethHeaderLen + ipLen + len(payload)
	frame := make([]byte, total)
	// vnet header is left zero — no GSO, no checksum offload.
	off := vnetHeaderLen
	writeEth(frame[off:], gatewayMAC, dstMAC, ethTypeIPv4)
	off += ethHeaderLen
	hdr := frame[off : off+ipLen]
	hdr[0] = 0x45 // version 4, IHL 5
	hdr[1] = 0x00 // DSCP/ECN
	binary.BigEndian.PutUint16(hdr[2:4], uint16(ipLen+len(payload)))
	binary.BigEndian.PutUint16(hdr[4:6], 0)
	binary.BigEndian.PutUint16(hdr[6:8], 0x4000) // DF, no offset
	hdr[8] = 64                                  // TTL
	hdr[9] = proto
	binary.BigEndian.PutUint16(hdr[10:12], 0) // checksum placeholder
	copy(hdr[12:16], src.To4())
	copy(hdr[16:20], dst.To4())
	binary.BigEndian.PutUint16(hdr[10:12], ipv4Checksum(hdr))
	copy(frame[off+ipLen:], payload)
	return frame
}
