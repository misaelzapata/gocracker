package stacknet

import (
	"bufio"
	"bytes"
	"net"
	"testing"
	"time"
)

func mustStackCIDR(t *testing.T, value string) *net.IPNet {
	t.Helper()
	_, network, err := net.ParseCIDR(value)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", value, err)
	}
	return network
}

func TestHelpers(t *testing.T) {
	mgr := &Manager{subnet: mustStackCIDR(t, "198.18.1.0/24"), gateway: net.ParseIP("198.18.1.1")}
	if mgr.GatewayIP() != "198.18.1.1" || mgr.GuestCIDR("198.18.1.10") != "198.18.1.10/24" {
		t.Fatalf("unexpected getters")
	}
	if shortIfName("abcdefghijklmnop") != "abcdfghijklmnop"[0:15] && len(shortIfName("abcdefghijklmnop")) > 15 {
		t.Fatal("shortIfName() should limit interface length")
	}
	if _, err := firstHostIP(nil); err == nil {
		t.Fatal("firstHostIP(nil) error = nil")
	}
	if got := gatewayCIDR(mustStackCIDR(t, "198.18.0.0/24"), net.ParseIP("198.18.0.1")); got != "198.18.0.1/24" {
		t.Fatalf("gatewayCIDR() = %q", got)
	}
}

func TestSubnetSelectionAndOverlap(t *testing.T) {
	occupied := []*net.IPNet{mustStackCIDR(t, "198.18.0.0/24")}
	subnet, err := selectAvailableSubnet("project", occupied)
	if err != nil {
		t.Fatalf("selectAvailableSubnet() error = %v", err)
	}
	if overlapsAny(subnet, occupied) {
		t.Fatalf("selected overlapping subnet %s", subnet)
	}
	if !cidrOverlap(mustStackCIDR(t, "198.18.0.0/24"), mustStackCIDR(t, "198.18.0.0/25")) {
		t.Fatal("expected overlap")
	}
	if cidrOverlap(mustStackCIDR(t, "198.18.0.0/24"), mustStackCIDR(t, "198.18.1.0/24")) {
		t.Fatal("unexpected overlap")
	}
}

func TestStartPortForwardUnsupportedProtocol(t *testing.T) {
	if _, err := startPortForward("", 0, "127.0.0.1", 80, "icmp"); err == nil {
		t.Fatal("startPortForward() error = nil")
	}
}

func TestStartTCPForwardProxiesTraffic(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		conn, err := target.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("pong\n"))
	}()

	forward, err := startTCPForward("127.0.0.1", 0, "127.0.0.1", target.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatalf("startTCPForward() error = %v", err)
	}
	defer forward.Close()

	addr := forward.(*tcpForward).listener.Addr().String()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != "pong\n" {
		t.Fatalf("line = %q", line)
	}
}

func TestStartUDPForwardRoundTrip(t *testing.T) {
	target, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		buf := make([]byte, 64)
		n, addr, err := target.ReadFrom(buf)
		if err != nil {
			return
		}
		_, _ = target.WriteTo(bytes.ToUpper(buf[:n]), addr)
	}()

	forward, err := startUDPForward("127.0.0.1", 0, "127.0.0.1", target.LocalAddr().(*net.UDPAddr).Port)
	if err != nil {
		t.Fatalf("startUDPForward() error = %v", err)
	}
	defer forward.Close()

	client, err := net.Dial("udp", forward.(*udpForward).conn.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "PING" {
		t.Fatalf("reply = %q", buf[:n])
	}
}
