package slirp

import (
	"encoding/binary"
	"net"
	"testing"
)

// frameOf builds a frame the slirp engine would receive from virtio-net:
// a 12-byte zero virtio_net_hdr followed by ethernet+payload.
func frameOf(payload []byte) []byte {
	out := make([]byte, vnetHeaderLen+len(payload))
	copy(out[vnetHeaderLen:], payload)
	return out
}

func TestARPRespondsForGatewayIP(t *testing.T) {
	s := New()
	t.Cleanup(func() { _ = s.Close() })

	guestMAC := net.HardwareAddr{0x52, 0x54, 0, 0, 0, 0x10}
	// who-has 10.0.2.2? tell 10.0.2.15
	body := make([]byte, ethHeaderLen+arpPacketLen)
	writeEth(body, guestMAC, net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, ethTypeARP)
	off := ethHeaderLen
	binary.BigEndian.PutUint16(body[off:off+2], arpHwEthernet)
	binary.BigEndian.PutUint16(body[off+2:off+4], arpProtoIPv4)
	body[off+4] = arpHwAddrLen
	body[off+5] = arpProtoAddrLen
	binary.BigEndian.PutUint16(body[off+6:off+8], arpOpRequest)
	copy(body[off+8:off+14], guestMAC)
	copy(body[off+14:off+18], guestIP)
	copy(body[off+18:off+24], net.HardwareAddr{0, 0, 0, 0, 0, 0})
	copy(body[off+24:off+28], gatewayIP)

	if err := s.WriteFrame(frameOf(body)); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	buf := make([]byte, 1500)
	n, err := s.ReadFrame(buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	reply := buf[:n]
	if len(reply) < vnetHeaderLen+ethHeaderLen+arpPacketLen {
		t.Fatalf("reply too short: %d", len(reply))
	}
	// Skip vnet header; check L2: dst should be the guest MAC, src the gateway MAC.
	l2 := reply[vnetHeaderLen:]
	if net.HardwareAddr(l2[0:6]).String() != guestMAC.String() {
		t.Fatalf("dst MAC mismatch: got %v want %v", l2[0:6], guestMAC)
	}
	if net.HardwareAddr(l2[6:12]).String() != gatewayMAC.String() {
		t.Fatalf("src MAC mismatch: got %v want %v", l2[6:12], gatewayMAC)
	}
	if etherType := binary.BigEndian.Uint16(l2[12:14]); etherType != ethTypeARP {
		t.Fatalf("ethertype = %#x", etherType)
	}
	arp := l2[ethHeaderLen:]
	if op := binary.BigEndian.Uint16(arp[6:8]); op != arpOpReply {
		t.Fatalf("op = %d want reply", op)
	}
	if !net.IP(arp[14:18]).Equal(gatewayIP) {
		t.Fatalf("sender IP = %v want %v", net.IP(arp[14:18]), gatewayIP)
	}
	if got := s.Snapshot()["arp_replies"]; got != 1 {
		t.Fatalf("arp_replies = %d", got)
	}
}

func TestDHCPDiscoverYieldsOffer(t *testing.T) {
	s := New()
	t.Cleanup(func() { _ = s.Close() })

	guestMAC := net.HardwareAddr{0x52, 0x54, 0, 0, 0, 0x11}

	// Build the BOOTP/DHCP DISCOVER (chaddr = guestMAC).
	dhcp := make([]byte, 244)
	dhcp[0] = dhcpOpRequest
	dhcp[1] = 1 // htype
	dhcp[2] = 6
	binary.BigEndian.PutUint32(dhcp[4:8], 0xCAFEBABE) // xid
	copy(dhcp[28:34], guestMAC)
	binary.BigEndian.PutUint32(dhcp[236:240], dhcpMagicCookie)
	dhcp[240] = optMsgType
	dhcp[241] = 1
	dhcp[242] = dhcpDiscover
	dhcp[243] = optEnd

	udp := buildUDPDatagram(net.IPv4zero.To4(), net.IPv4bcast.To4(), dhcpClientPort, dhcpServerPort, dhcp)

	// IP header
	ip := make([]byte, ipHeaderMin+len(udp))
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:4], uint16(len(ip)))
	binary.BigEndian.PutUint16(ip[6:8], 0x4000)
	ip[8] = 64
	ip[9] = ipProtoUDP
	copy(ip[12:16], net.IPv4zero.To4())
	copy(ip[16:20], net.IPv4bcast.To4())
	binary.BigEndian.PutUint16(ip[10:12], ipv4Checksum(ip[:ipHeaderMin]))
	copy(ip[ipHeaderMin:], udp)

	frame := make([]byte, ethHeaderLen+len(ip))
	writeEth(frame, guestMAC, net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, ethTypeIPv4)
	copy(frame[ethHeaderLen:], ip)

	if err := s.WriteFrame(frameOf(frame)); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	buf := make([]byte, 4096)
	n, err := s.ReadFrame(buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	reply := buf[:n]
	// Walk to BOOTP payload: vnet | eth | ip | udp | bootp.
	if len(reply) < vnetHeaderLen+ethHeaderLen+ipHeaderMin+udpHeaderLen+240 {
		t.Fatalf("DHCP reply too short: %d", len(reply))
	}
	bootp := reply[vnetHeaderLen+ethHeaderLen+ipHeaderMin+udpHeaderLen:]
	if bootp[0] != dhcpOpReply {
		t.Fatalf("bootp op = %d", bootp[0])
	}
	if !net.IP(bootp[16:20]).Equal(guestIP) {
		t.Fatalf("yiaddr = %v want %v", net.IP(bootp[16:20]), guestIP)
	}
	// Find the message-type option.
	off := 240
	var seenType byte
	for off+2 <= len(bootp) {
		c := bootp[off]
		if c == optEnd {
			break
		}
		if c == optPad {
			off++
			continue
		}
		ln := int(bootp[off+1])
		if c == optMsgType && ln >= 1 {
			seenType = bootp[off+2]
		}
		off += 2 + ln
	}
	if seenType != dhcpOffer {
		t.Fatalf("msgType = %d want OFFER", seenType)
	}
	if got := s.Snapshot()["dhcp_messages"]; got != 1 {
		t.Fatalf("dhcp_messages = %d", got)
	}
}
