package slirp

import (
	"encoding/binary"
	"net"
)

// ARP packet layout (Ethernet/IPv4):
//
//	hardware type     2 bytes  (0x0001 Ethernet)
//	protocol type     2 bytes  (0x0800 IPv4)
//	hw addr length    1 byte   (6)
//	proto addr length 1 byte   (4)
//	operation         2 bytes  (1 request, 2 reply)
//	sender hw addr    6 bytes
//	sender proto addr 4 bytes
//	target hw addr    6 bytes
//	target proto addr 4 bytes
const (
	arpHwEthernet   = 1
	arpProtoIPv4    = 0x0800
	arpOpRequest    = 1
	arpOpReply      = 2
	arpHwAddrLen    = 6
	arpProtoAddrLen = 4
)

type arpPacket struct {
	Op           uint16
	SenderMAC    net.HardwareAddr
	SenderIP     net.IP
	TargetMAC    net.HardwareAddr
	TargetIP     net.IP
}

func parseARP(buf []byte) (arpPacket, bool) {
	if len(buf) < arpPacketLen {
		return arpPacket{}, false
	}
	hwType := binary.BigEndian.Uint16(buf[0:2])
	protoType := binary.BigEndian.Uint16(buf[2:4])
	if hwType != arpHwEthernet || protoType != arpProtoIPv4 {
		return arpPacket{}, false
	}
	if buf[4] != arpHwAddrLen || buf[5] != arpProtoAddrLen {
		return arpPacket{}, false
	}
	pkt := arpPacket{
		Op:        binary.BigEndian.Uint16(buf[6:8]),
		SenderMAC: append(net.HardwareAddr{}, buf[8:14]...),
		SenderIP:  append(net.IP{}, buf[14:18]...),
		TargetMAC: append(net.HardwareAddr{}, buf[18:24]...),
		TargetIP:  append(net.IP{}, buf[24:28]...),
	}
	return pkt, true
}

// buildARPReply synthesizes an ARP reply on behalf of the slirp gateway.
// It returns a complete Ethernet frame prefixed with the 12-byte
// virtio_net_hdr the guest expects.
func buildARPReply(req arpPacket) []byte {
	frame := make([]byte, vnetHeaderLen+ethHeaderLen+arpPacketLen)
	off := vnetHeaderLen
	writeEth(frame[off:], gatewayMAC, req.SenderMAC, ethTypeARP)
	off += ethHeaderLen
	binary.BigEndian.PutUint16(frame[off+0:off+2], arpHwEthernet)
	binary.BigEndian.PutUint16(frame[off+2:off+4], arpProtoIPv4)
	frame[off+4] = arpHwAddrLen
	frame[off+5] = arpProtoAddrLen
	binary.BigEndian.PutUint16(frame[off+6:off+8], arpOpReply)
	copy(frame[off+8:off+14], gatewayMAC)
	copy(frame[off+14:off+18], req.TargetIP.To4())
	copy(frame[off+18:off+24], req.SenderMAC)
	copy(frame[off+24:off+28], req.SenderIP.To4())
	return frame
}
