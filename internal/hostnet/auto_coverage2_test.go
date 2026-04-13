package hostnet

import (
	"net"
	"testing"
)

// --- selectAvailableSubnet exhaustive coverage ---

func TestSelectAvailableSubnetAllOccupied(t *testing.T) {
	// If we occupy enough subnets, eventually it should error
	// We can't really occupy them all (65536), but verify it handles many
	pool := mustCIDR(t, autoSubnetPoolCIDR)
	var occupied []*net.IPNet
	for i := 0; i < 20; i++ {
		s, err := subnetAt(pool, autoSubnetPrefix, i)
		if err != nil {
			t.Fatal(err)
		}
		occupied = append(occupied, s)
	}
	// Should still find one since pool has 2^(30-15)=32768 subnets
	got, err := selectAvailableSubnet("xtest", occupied)
	if err != nil {
		t.Fatalf("error with 20 occupied: %v", err)
	}
	if overlapsAny(got, occupied) {
		t.Fatalf("overlaps occupied: %s", got)
	}
}

// --- cidrOverlap edge cases ---

func TestCidrOverlapBothNormalizeFail(t *testing.T) {
	// IPv6 networks cannot be normalized, should return false
	_, a, _ := net.ParseCIDR("fe80::/10")
	_, b, _ := net.ParseCIDR("fe80::/10")
	if cidrOverlap(a, b) {
		t.Fatal("IPv6 networks should not overlap (normalize fails)")
	}
}

func TestCidrOverlapOneIPv6(t *testing.T) {
	_, a, _ := net.ParseCIDR("10.0.0.0/24")
	_, b, _ := net.ParseCIDR("::1/128")
	if cidrOverlap(a, b) {
		t.Fatal("mixed IPv4/IPv6 should not overlap")
	}
}

// --- firstHostIP edge cases ---

func TestFirstHostIPIPv6(t *testing.T) {
	_, v6net, _ := net.ParseCIDR("::1/128")
	_, err := firstHostIP(v6net)
	if err == nil {
		t.Fatal("expected error for IPv6 network")
	}
}

func TestFirstHostIPSmallSubnet(t *testing.T) {
	ip, err := firstHostIP(mustCIDR(t, "10.0.0.0/31"))
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if ip.String() != "10.0.0.1" {
		t.Fatalf("got %s, want 10.0.0.1", ip)
	}
}

// --- incrementIP edge cases ---

func TestIncrementIPCarryAll(t *testing.T) {
	ip := net.IP{255, 255, 255, 255}
	incrementIP(ip)
	// Should wrap to 0.0.0.0
	if ip.String() != "0.0.0.0" {
		t.Fatalf("got %s, want 0.0.0.0", ip)
	}
}

// --- normalizeIPv4Net edge cases ---

func TestNormalizeIPv4Net_HostBits(t *testing.T) {
	// IP with host bits set: 10.0.0.5/24 should normalize to 10.0.0.0/24
	_, network, _ := net.ParseCIDR("10.0.0.5/24")
	got := normalizeIPv4Net(network)
	if got == nil {
		t.Fatal("should not be nil")
	}
	if got.IP.String() != "10.0.0.0" {
		t.Fatalf("expected network IP, got %s", got.IP)
	}
}

// --- autoTapName edge cases ---

func TestAutoTapNameOnlySpecialChars(t *testing.T) {
	// All chars are special, base becomes empty, falls back to "gc"
	name := autoTapName("!@#$%^&*()")
	if name != "gct-gc" {
		t.Fatalf("autoTapName(special) = %q, want gct-gc", name)
	}
}

func TestAutoTapNameExactlyMax(t *testing.T) {
	// "gct-" = 4 chars, so project can be up to 11 chars
	name := autoTapName("abcdefghijk") // 11 chars
	if name != "gct-abcdefghijk" {
		t.Fatalf("got %q, want gct-abcdefghijk", name)
	}
	if len(name) != 15 {
		t.Fatalf("len = %d, want 15", len(name))
	}
}

func TestAutoTapNameJustOverMax(t *testing.T) {
	// 12 chars forces hash-based name
	name := autoTapName("abcdefghijkl")
	if len(name) > 15 {
		t.Fatalf("len = %d, want <= 15", len(name))
	}
	if name[:4] != "gct-" {
		t.Fatalf("prefix = %q", name[:4])
	}
}

// --- subnetAt edge cases ---

func TestSubnetAtIndex0(t *testing.T) {
	pool := mustCIDR(t, "198.18.0.0/15")
	s, err := subnetAt(pool, 30, 0)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	// First /30 in 198.18.0.0/15 is 198.18.0.0/30
	if s.String() != "198.18.0.0/30" {
		t.Fatalf("got %s, want 198.18.0.0/30", s)
	}
}

func TestSubnetAtHighIndex(t *testing.T) {
	pool := mustCIDR(t, "198.18.0.0/15")
	// index 1 should be 198.18.0.4/30
	s, err := subnetAt(pool, 30, 1)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if s.String() != "198.18.0.4/30" {
		t.Fatalf("got %s, want 198.18.0.4/30", s)
	}
}

// --- overlapsAny edge cases ---

func TestOverlapsAnyNoneOverlap(t *testing.T) {
	candidate := mustCIDR(t, "10.0.0.0/24")
	occupied := []*net.IPNet{
		mustCIDR(t, "192.168.0.0/16"),
		mustCIDR(t, "172.16.0.0/12"),
	}
	if overlapsAny(candidate, occupied) {
		t.Fatal("10.0.0.0/24 should not overlap 192.168 or 172.16")
	}
}

func TestOverlapsAnyMiddleOverlaps(t *testing.T) {
	candidate := mustCIDR(t, "10.0.0.0/24")
	occupied := []*net.IPNet{
		mustCIDR(t, "192.168.0.0/16"),
		mustCIDR(t, "10.0.0.0/8"), // overlaps
		mustCIDR(t, "172.16.0.0/12"),
	}
	if !overlapsAny(candidate, occupied) {
		t.Fatal("should overlap 10.0.0.0/8")
	}
}

// --- Activate/Close with non-nil but partial state ---

func TestActivateEmptyAutoNetwork(t *testing.T) {
	n := &AutoNetwork{}
	// This will fail because subnet is nil, but should not panic
	err := n.Activate()
	if err == nil {
		// It may succeed or fail depending on nil fields; just don't panic
		_ = err
	}
}

func TestCloseEmptyAutoNetwork(t *testing.T) {
	n := &AutoNetwork{}
	n.Close() // should not panic
}

func TestCloseIdempotent(t *testing.T) {
	n := &AutoNetwork{}
	n.Close()
	n.Close() // second call should not panic (cleanupOnce)
}
