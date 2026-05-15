package stacknet

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

// buildQuery constructs a minimal DNS A-query for `fqdn` with id 0x1234, RD=1.
func buildQuery(fqdn string) []byte {
	out := []byte{
		0x12, 0x34, // ID
		0x01, 0x00, // flags: standard query, RD=1
		0x00, 0x01, // QDCOUNT=1
		0x00, 0x00, // ANCOUNT=0
		0x00, 0x00, // NSCOUNT=0
		0x00, 0x00, // ARCOUNT=0
	}
	// Question name (labels).
	for _, lbl := range splitLabels(fqdn) {
		out = append(out, byte(len(lbl)))
		out = append(out, []byte(lbl)...)
	}
	out = append(out, 0x00)       // root label
	out = append(out, 0x00, 0x01) // QTYPE=A
	out = append(out, 0x00, 0x01) // QCLASS=IN
	return out
}

func splitLabels(name string) []string {
	var labels []string
	start := 0
	for i := 0; i <= len(name); i++ {
		if i == len(name) || name[i] == '.' {
			if i > start {
				labels = append(labels, name[start:i])
			}
			start = i + 1
		}
	}
	return labels
}

// startDNS spins up an InProcDNS, runs it in a goroutine, and returns the
// resolver + its UDP address.
func startDNS(t *testing.T, project string) (*InProcDNS, *net.UDPAddr) {
	t.Helper()
	srv, err := NewInProcDNS(project, net.IPv4(127, 0, 0, 1))
	if err != nil {
		t.Fatalf("NewInProcDNS: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = srv.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("dns Run did not exit on Close")
		}
	})
	addr := srv.Addr().(*net.UDPAddr)
	return srv, addr
}

// roundTrip sends `query` to `addr` and returns the response (or fails the
// test on timeout / network error).
func roundTrip(t *testing.T, addr *net.UDPAddr, query []byte) []byte {
	t.Helper()
	conn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write(query); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return buf[:n]
}

func TestInProcDNS_ResolvesKnownName(t *testing.T) {
	srv, addr := startDNS(t, "demo")
	want := net.IPv4(10, 42, 1, 5)
	srv.Add("web", want)

	resp := roundTrip(t, addr, buildQuery("web.demo.compose.local"))
	if len(resp) < 12 {
		t.Fatalf("response too short: %d bytes", len(resp))
	}
	// ID echoed.
	if binary.BigEndian.Uint16(resp[0:2]) != 0x1234 {
		t.Errorf("id mismatch: got 0x%04x", binary.BigEndian.Uint16(resp[0:2]))
	}
	// QR=1, RCODE=0.
	if resp[2]&0x80 == 0 {
		t.Errorf("QR not set: 0x%02x", resp[2])
	}
	if resp[3]&0x0f != 0 {
		t.Errorf("rcode = %d, want 0 (NOERROR)", resp[3]&0x0f)
	}
	// AA=1 (authoritative).
	if resp[2]&0x04 == 0 {
		t.Errorf("AA bit not set: 0x%02x", resp[2])
	}
	// ANCOUNT=1.
	if binary.BigEndian.Uint16(resp[6:8]) != 1 {
		t.Fatalf("ancount = %d, want 1", binary.BigEndian.Uint16(resp[6:8]))
	}
	// Locate the answer A record — last 4 bytes of the response are the
	// IPv4 RDATA.
	gotIP := net.IPv4(resp[len(resp)-4], resp[len(resp)-3], resp[len(resp)-2], resp[len(resp)-1])
	if !gotIP.Equal(want) {
		t.Errorf("answer IP = %s, want %s", gotIP, want)
	}
}

func TestInProcDNS_NXDOMAINForUnknown(t *testing.T) {
	_, addr := startDNS(t, "demo")
	resp := roundTrip(t, addr, buildQuery("nope.demo.compose.local"))
	if resp[3]&0x0f != 3 {
		t.Errorf("rcode = %d, want 3 (NXDOMAIN)", resp[3]&0x0f)
	}
	if binary.BigEndian.Uint16(resp[6:8]) != 0 {
		t.Errorf("ancount = %d, want 0", binary.BigEndian.Uint16(resp[6:8]))
	}
}

func TestInProcDNS_RemoveDropsRecord(t *testing.T) {
	srv, addr := startDNS(t, "demo")
	srv.Add("web", net.IPv4(10, 0, 0, 1))
	// Confirm it resolves.
	resp := roundTrip(t, addr, buildQuery("web.demo.compose.local"))
	if resp[3]&0x0f != 0 {
		t.Fatalf("expected NOERROR before Remove")
	}
	srv.Remove("web")
	resp = roundTrip(t, addr, buildQuery("web.demo.compose.local"))
	if resp[3]&0x0f != 3 {
		t.Errorf("after Remove, rcode = %d, want 3 (NXDOMAIN)", resp[3]&0x0f)
	}
}

func TestInProcDNS_CloseUnblocksRun(t *testing.T) {
	srv, err := NewInProcDNS("demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- srv.Run(context.Background())
	}()
	_ = srv.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after Close")
	}
}

func TestInProcDNS_RejectsEmptyProject(t *testing.T) {
	if _, err := NewInProcDNS("", nil); err == nil {
		t.Fatal("expected error for empty project")
	}
}

func TestInProcDNS_IgnoresIPv6Add(t *testing.T) {
	srv, addr := startDNS(t, "demo")
	srv.Add("v6", net.ParseIP("::1"))
	resp := roundTrip(t, addr, buildQuery("v6.demo.compose.local"))
	if resp[3]&0x0f != 3 {
		t.Errorf("IPv6 Add should not register: rcode = %d", resp[3]&0x0f)
	}
}
