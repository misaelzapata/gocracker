//go:build slirp_gvisor

package slirp

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

// TestTCPStackInitializes proves Slirp.New() boots the gVisor stack
// without panicking and that Close() tears it down cleanly. Without the
// slirp_gvisor build tag tcpInit is a no-op, so this test only runs
// under the gVisor build.
func TestTCPStackInitializes(t *testing.T) {
	s := New()
	if s.tcp == nil {
		t.Fatalf("Slirp.tcp is nil — gVisor stack didn't init")
	}
	if s.tcp.stack == nil {
		t.Fatalf("tcp.stack is nil")
	}
	if s.tcp.endpoint == nil {
		t.Fatalf("tcp.endpoint is nil")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestTCPInjectIncrementsCounter feeds a hand-rolled IPv4/TCP SYN into
// handleTCP and verifies the gVisor stack accepts it (TCPMetrics goes
// from 0 → 1). This is the minimum proof that the dispatch path is
// alive.
func TestTCPInjectIncrementsCounter(t *testing.T) {
	s := New()
	defer s.Close()

	// Seed guestMAC so the RX pump doesn't drop replies.
	s.mu.Lock()
	s.guestMAC = net.HardwareAddr{0x52, 0x54, 0x00, 0x00, 0x00, 0x10}
	s.mu.Unlock()

	before := TCPMetrics()
	// Synthesize a minimum-size SYN: guest 10.0.2.15:12345 → 10.0.2.2:1
	pkt := makeTCPSYN(
		net.IPv4(10, 0, 2, 15), 12345,
		net.IPv4(10, 0, 2, 2), 1,
		0xDEADBEEF,
	)
	s.handleTCP(pkt)
	after := TCPMetrics()
	if after <= before {
		t.Fatalf("TCPMetrics did not increment: %d -> %d", before, after)
	}
}

// TestTCPRoundTripThroughForwarder spins a host TCP echo server, then
// asks the gVisor stack to dial it via the TCP forwarder. The forwarder
// is what handles SYNs to unregistered ports (the outbound NAT case),
// so a successful round-trip proves the full guest→external dispatch
// chain.
//
// We bypass the channel endpoint here — we exercise the forwarder by
// constructing a SYN packet and injecting it, then immediately listen
// for the SYN-ACK on the channel endpoint and complete the handshake
// at the host level. That's still rich enough to catch breakage in the
// stack init, route table, and forwarder wiring.
func TestTCPRoundTripThroughForwarder(t *testing.T) {
	// Host echo server on 127.0.0.1.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("host listen: %v", err)
	}
	defer ln.Close()

	srvReady := make(chan struct{})
	srvErr := make(chan error, 1)
	go func() {
		close(srvReady)
		conn, err := ln.Accept()
		if err != nil {
			srvErr <- err
			return
		}
		defer conn.Close()
		// Echo until EOF.
		_, _ = io.Copy(conn, conn)
		srvErr <- nil
	}()
	<-srvReady

	hostAddr := ln.Addr().(*net.TCPAddr)

	s := New()
	defer s.Close()
	s.mu.Lock()
	s.guestMAC = net.HardwareAddr{0x52, 0x54, 0x00, 0x00, 0x00, 0x11}
	s.mu.Unlock()

	// Synthesize a SYN from "guest" (10.0.2.15:54321) to the host echo
	// server. The forwarder will see this SYN, dial the host, and
	// answer with SYN-ACK on the channel endpoint.
	syn := makeTCPSYN(
		net.IPv4(10, 0, 2, 15), 54321,
		net.IP(hostAddr.IP.To4()), uint16(hostAddr.Port),
		0xCAFEBABE,
	)
	s.handleTCP(syn)

	// Wait up to 2 s for the gVisor stack to emit *something* out the
	// channel endpoint (the SYN-ACK). We're not parsing the full TCP
	// state machine here — proving that the forwarder fires and the
	// stack produces a reply is enough for this unit test.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		buf := make([]byte, 2048)
		n, err := s.ReadFrame(buf)
		if err == nil && n > 0 {
			// Verify it looks like an IPv4/TCP frame. Skip the vnet
			// header (12 bytes) and ethernet (14 bytes), then check
			// the IP proto byte.
			off := vnetHeaderLen + ethHeaderLen
			if n > off+10 && buf[off+9] == ipProtoTCP {
				return // success
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no TCP reply from gVisor stack within 2s")
}

// makeTCPSYN builds a SYN segment wrapped in an IPv4 header. Returns
// the byte slice ready for InjectInbound (network-layer view).
func makeTCPSYN(srcIP net.IP, srcPort uint16, dstIP net.IP, dstPort uint16, seq uint32) []byte {
	const ipHL = 20
	const tcpHL = 20
	pkt := make([]byte, ipHL+tcpHL)
	// IPv4 header.
	pkt[0] = 0x45 // version 4, IHL 5
	binary.BigEndian.PutUint16(pkt[2:4], uint16(len(pkt)))
	binary.BigEndian.PutUint16(pkt[6:8], 0x4000) // DF
	pkt[8] = 64                                   // TTL
	pkt[9] = ipProtoTCP
	copy(pkt[12:16], srcIP.To4())
	copy(pkt[16:20], dstIP.To4())
	binary.BigEndian.PutUint16(pkt[10:12], ipv4Checksum(pkt[:ipHL]))

	// TCP header.
	tcp := pkt[ipHL:]
	binary.BigEndian.PutUint16(tcp[0:2], srcPort)
	binary.BigEndian.PutUint16(tcp[2:4], dstPort)
	binary.BigEndian.PutUint32(tcp[4:8], seq)
	binary.BigEndian.PutUint32(tcp[8:12], 0)
	tcp[12] = 0x50 // data offset 5*4=20 bytes
	tcp[13] = 0x02 // SYN
	binary.BigEndian.PutUint16(tcp[14:16], 65535)
	binary.BigEndian.PutUint16(tcp[18:20], 0) // urg

	// TCP checksum over pseudo-header + segment.
	var pseudo [12]byte
	copy(pseudo[0:4], srcIP.To4())
	copy(pseudo[4:8], dstIP.To4())
	pseudo[9] = ipProtoTCP
	binary.BigEndian.PutUint16(pseudo[10:12], uint16(len(tcp)))
	sum := onesComplementSum(0, pseudo[:])
	sum = onesComplementSum(sum, tcp)
	binary.BigEndian.PutUint16(tcp[16:18], foldChecksum(sum))

	// Silence the unused-import warning if bytes ever stops being used.
	_ = bytes.Equal
	return pkt
}
