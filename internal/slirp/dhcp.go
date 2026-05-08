package slirp

import (
	"encoding/binary"
	"net"
)

// Minimal DHCPv4 server: enough to answer DISCOVER and REQUEST with a fixed
// lease for the single guest (10.0.2.15). No lease tracking, no renewal
// logic — the lease is effectively perpetual for the lifetime of the VM.
//
// This matches the behaviour of QEMU/libslirp's built-in DHCP server.

const (
	dhcpServerPort = 67
	dhcpClientPort = 68

	dhcpOpRequest = 1
	dhcpOpReply   = 2

	dhcpMagicCookie uint32 = 0x63825363

	// DHCP option codes (RFC 2132).
	optPad         = 0
	optSubnetMask  = 1
	optRouter      = 3
	optDNS         = 6
	optHostname    = 12
	optReqIP       = 50
	optLeaseTime   = 51
	optMsgType     = 53
	optServerID    = 54
	optParamReqL   = 55
	optEnd         = 255

	// DHCP message types (RFC 2131).
	dhcpDiscover = 1
	dhcpOffer    = 2
	dhcpRequest  = 3
	dhcpAck      = 5
)

// dhcpMessage is just the bytes we need to read or write — we don't model
// every BOOTP field individually.
type dhcpMessage struct {
	XID      uint32
	Flags    uint16
	CIAddr   net.IP
	YIAddr   net.IP
	SIAddr   net.IP
	GIAddr   net.IP
	CHAddr   net.HardwareAddr
	MsgType  byte
	ReqIP    net.IP
	ServerID net.IP
}

func parseDHCP(buf []byte) (dhcpMessage, bool) {
	if len(buf) < 240 {
		return dhcpMessage{}, false
	}
	if buf[0] != dhcpOpRequest {
		return dhcpMessage{}, false
	}
	if binary.BigEndian.Uint32(buf[236:240]) != dhcpMagicCookie {
		return dhcpMessage{}, false
	}
	msg := dhcpMessage{
		XID:    binary.BigEndian.Uint32(buf[4:8]),
		Flags:  binary.BigEndian.Uint16(buf[10:12]),
		CIAddr: append(net.IP{}, buf[12:16]...),
		YIAddr: append(net.IP{}, buf[16:20]...),
		SIAddr: append(net.IP{}, buf[20:24]...),
		GIAddr: append(net.IP{}, buf[24:28]...),
		CHAddr: append(net.HardwareAddr{}, buf[28:34]...),
	}
	// Walk the options TLV table.
	i := 240
	for i < len(buf) {
		opt := buf[i]
		if opt == optEnd {
			break
		}
		if opt == optPad {
			i++
			continue
		}
		if i+1 >= len(buf) {
			break
		}
		ln := int(buf[i+1])
		if i+2+ln > len(buf) {
			break
		}
		val := buf[i+2 : i+2+ln]
		switch opt {
		case optMsgType:
			if ln >= 1 {
				msg.MsgType = val[0]
			}
		case optReqIP:
			if ln >= 4 {
				msg.ReqIP = append(net.IP{}, val[:4]...)
			}
		case optServerID:
			if ln >= 4 {
				msg.ServerID = append(net.IP{}, val[:4]...)
			}
		}
		i += 2 + ln
	}
	return msg, true
}

// buildDHCPReply synthesizes an OFFER or ACK reply for the given REQUEST/
// DISCOVER. It returns the BOOTP/DHCP payload, ready to be wrapped in
// UDP/IP/Ethernet headers.
func buildDHCPReply(req dhcpMessage, msgType byte) []byte {
	buf := make([]byte, 300)
	buf[0] = dhcpOpReply
	buf[1] = 1 // htype: Ethernet
	buf[2] = 6 // hlen
	buf[3] = 0 // hops
	binary.BigEndian.PutUint32(buf[4:8], req.XID)
	binary.BigEndian.PutUint16(buf[8:10], 0) // secs
	binary.BigEndian.PutUint16(buf[10:12], req.Flags)
	copy(buf[12:16], net.IPv4zero.To4()) // ciaddr
	copy(buf[16:20], guestIP)            // yiaddr (offered IP)
	copy(buf[20:24], gatewayIP)          // siaddr (next server)
	copy(buf[24:28], net.IPv4zero.To4()) // giaddr
	copy(buf[28:34], req.CHAddr)         // chaddr (16 bytes total slot, only first 6 used)
	binary.BigEndian.PutUint32(buf[236:240], dhcpMagicCookie)

	off := 240
	addOpt := func(code byte, val []byte) {
		if off+2+len(val) > len(buf) {
			grow := make([]byte, off+2+len(val)+64)
			copy(grow, buf)
			buf = grow
		}
		buf[off] = code
		buf[off+1] = byte(len(val))
		copy(buf[off+2:], val)
		off += 2 + len(val)
	}

	addOpt(optMsgType, []byte{msgType})
	addOpt(optServerID, gatewayIP)
	leaseTime := []byte{0, 0, 0, 0}
	binary.BigEndian.PutUint32(leaseTime, 86400) // 1 day; guest will renew
	addOpt(optLeaseTime, leaseTime)
	addOpt(optSubnetMask, []byte{255, 255, 255, 0})
	addOpt(optRouter, gatewayIP)
	addOpt(optDNS, dnsIP)
	buf[off] = optEnd
	off++
	return buf[:off]
}
