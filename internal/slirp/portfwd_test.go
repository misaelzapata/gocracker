package slirp

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

// TestPortFwdAddLookup checks the basic registration + lookup round-trip.
// We add a TCP rule for 127.0.0.1:0 → guest:80, then verify Lookup
// returns it by host port (with and without IP), and rejects mismatches.
func TestPortFwdAddLookup(t *testing.T) {
	r := NewPortFwdRegistry()
	hostIP := net.IPv4(127, 0, 0, 1)
	pf := PortForward{
		HostIP:    hostIP,
		HostPort:  18080,
		GuestPort: 80,
		Proto:     "tcp",
	}
	if err := r.Add(pf); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, ok := r.Lookup("tcp", hostIP, 18080)
	if !ok {
		t.Fatalf("Lookup miss for registered rule")
	}
	if got.GuestPort != 80 {
		t.Fatalf("GuestPort = %d want 80", got.GuestPort)
	}
	if !got.HostIP.Equal(hostIP) {
		t.Fatalf("HostIP = %v want %v", got.HostIP, hostIP)
	}
	if !got.GuestIP.Equal(guestIP) {
		t.Fatalf("GuestIP defaulted wrong: %v", got.GuestIP)
	}

	// Lookup by wildcard IP (caller doesn't know the bind).
	if _, ok := r.Lookup("tcp", nil, 18080); !ok {
		t.Fatalf("wildcard Lookup missed")
	}

	// Wrong proto -> miss.
	if _, ok := r.Lookup("udp", hostIP, 18080); ok {
		t.Fatalf("Lookup hit on wrong proto")
	}
	// Wrong port -> miss.
	if _, ok := r.Lookup("tcp", hostIP, 18081); ok {
		t.Fatalf("Lookup hit on wrong port")
	}
}

// TestPortFwdRejectsInvalid checks the validation rules: proto must be
// tcp/udp, and duplicate (proto,hostIP,hostPort) are refused.
func TestPortFwdRejectsInvalid(t *testing.T) {
	r := NewPortFwdRegistry()
	if err := r.Add(PortForward{Proto: "sctp", HostPort: 1234}); err != ErrInvalidProto {
		t.Fatalf("expected ErrInvalidProto, got %v", err)
	}
	if err := r.Add(PortForward{Proto: "tcp", HostPort: 1234}); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if err := r.Add(PortForward{Proto: "tcp", HostPort: 1234}); err != ErrDuplicateForward {
		t.Fatalf("expected ErrDuplicateForward, got %v", err)
	}
	// UDP on the same port is allowed (different proto).
	if err := r.Add(PortForward{Proto: "udp", HostPort: 1234}); err != nil {
		t.Fatalf("udp Add: %v", err)
	}
}

// TestPortFwdListenAccept binds a real TCP listener via the registry
// (port 0 → kernel-assigned), connects to it from the host stack, and
// asserts onAccept fires with the matched rule.
func TestPortFwdListenAccept(t *testing.T) {
	// Bind on port 0 first to discover an unused port, then close and
	// register that port with the registry.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	port := uint16(probe.Addr().(*net.TCPAddr).Port)
	_ = probe.Close()

	r := NewPortFwdRegistry()
	rule := PortForward{
		HostIP:    net.IPv4(127, 0, 0, 1),
		HostPort:  port,
		GuestIP:   net.IPv4(10, 0, 2, 15),
		GuestPort: 8080,
		Proto:     "tcp",
	}
	if err := r.Add(rule); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var (
		gotPF   PortForward
		gotConn net.Conn
		wg      sync.WaitGroup
	)
	wg.Add(1)
	listenDone := make(chan struct{})
	go func() {
		defer close(listenDone)
		err := r.Listen(ctx, func(pf PortForward, c net.Conn) {
			gotPF = pf
			gotConn = c
			wg.Done()
		})
		if err != nil {
			t.Logf("Listen returned: %v", err)
		}
	}()

	// Give Listen a moment to bind. We retry-dial because there's an
	// inherent race between the goroutine binding and the test dialing.
	deadline := time.Now().Add(2 * time.Second)
	var clientConn net.Conn
	for time.Now().Before(deadline) {
		clientConn, err = net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoaPort(port)), 200*time.Millisecond)
		if err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer clientConn.Close()

	// Wait for onAccept to fire.
	waitDone := make(chan struct{})
	go func() { wg.Wait(); close(waitDone) }()
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("onAccept never fired")
	}

	if gotPF.GuestPort != 8080 {
		t.Fatalf("onAccept got GuestPort %d want 8080", gotPF.GuestPort)
	}
	if !gotPF.HostIP.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("onAccept got HostIP %v want 127.0.0.1", gotPF.HostIP)
	}
	if gotConn == nil {
		t.Fatalf("onAccept got nil conn")
	}
	_ = gotConn.Close()

	cancel()
	<-listenDone
}

// TestPortFwdAllReturnsCopy ensures All() returns a snapshot, not the
// internal slice; mutating the result mustn't affect future lookups.
func TestPortFwdAllReturnsCopy(t *testing.T) {
	r := NewPortFwdRegistry()
	_ = r.Add(PortForward{Proto: "tcp", HostPort: 1})
	_ = r.Add(PortForward{Proto: "tcp", HostPort: 2})
	out := r.All()
	if len(out) != 2 {
		t.Fatalf("len(All) = %d want 2", len(out))
	}
	out[0] = PortForward{}
	again := r.All()
	if again[0].HostPort != 1 {
		t.Fatalf("mutating snapshot leaked into registry: HostPort = %d", again[0].HostPort)
	}
}

// TestPortFwdSlirpIntegration verifies New() wires the registry into
// the Slirp instance and Close() tears it down without panicking.
func TestPortFwdSlirpIntegration(t *testing.T) {
	s := New()
	defer s.Close()
	r := s.PortFwd()
	if r == nil {
		t.Fatalf("PortFwd() returned nil")
	}
	if err := r.Add(PortForward{Proto: "tcp", HostPort: 22000, GuestPort: 22}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, ok := r.Lookup("tcp", nil, 22000); !ok {
		t.Fatalf("Lookup miss after Add via Slirp.PortFwd()")
	}
}

// itoaPort renders a uint16 port for net.JoinHostPort. Avoids strconv
// for parity with the rest of the package (and to dodge an import in
// the test file).
func itoaPort(p uint16) string {
	var b [5]byte
	i := len(b)
	if p == 0 {
		return "0"
	}
	for p > 0 {
		i--
		b[i] = byte('0' + p%10)
		p /= 10
	}
	return string(b[i:])
}
