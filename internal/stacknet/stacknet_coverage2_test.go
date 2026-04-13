package stacknet

import (
	"net"
	"testing"
)

// --- selectAvailableSubnet ---

func TestSelectAvailableSubnetDeterministic(t *testing.T) {
	s1, err := selectAvailableSubnet("deterministic-project", nil)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := selectAvailableSubnet("deterministic-project", nil)
	if err != nil {
		t.Fatal(err)
	}
	if s1.String() != s2.String() {
		t.Fatalf("same project different subnets: %s != %s", s1, s2)
	}
}

func TestSelectAvailableSubnetDifferentProjects(t *testing.T) {
	s1, _ := selectAvailableSubnet("project-alpha", nil)
	s2, _ := selectAvailableSubnet("project-beta", nil)
	if s1.String() == s2.String() {
		t.Fatalf("different projects same subnet: %s", s1)
	}
}

func TestSelectAvailableSubnetAvoidsMultiple(t *testing.T) {
	var occupied []*net.IPNet
	for i := 0; i < 5; i++ {
		s, err := selectAvailableSubnet("stack-test", occupied)
		if err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		if overlapsAny(s, occupied) {
			t.Fatalf("round %d: %s overlaps", i, s)
		}
		occupied = append(occupied, s)
	}
}

// --- subnetAt ---

func TestSubnetAtIndex0(t *testing.T) {
	_, pool, _ := net.ParseCIDR(SubnetPoolCIDR)
	s, err := subnetAt(pool, subnetPrefix, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s == nil {
		t.Fatal("nil subnet")
	}
	ones, _ := s.Mask.Size()
	if ones != subnetPrefix {
		t.Fatalf("prefix = %d, want %d", ones, subnetPrefix)
	}
}

func TestSubnetAtSequentialUnique(t *testing.T) {
	_, pool, _ := net.ParseCIDR(SubnetPoolCIDR)
	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		s, err := subnetAt(pool, subnetPrefix, i)
		if err != nil {
			t.Fatal(err)
		}
		key := s.String()
		if seen[key] {
			t.Fatalf("duplicate at index %d: %s", i, key)
		}
		seen[key] = true
	}
}

func TestSubnetAtInvalidPrefix(t *testing.T) {
	_, pool, _ := net.ParseCIDR(SubnetPoolCIDR)
	_, err := subnetAt(pool, 14, 0)
	if err == nil {
		t.Fatal("expected error for prefix < pool prefix")
	}
}

func TestSubnetAtIPv6Pool(t *testing.T) {
	_, pool, _ := net.ParseCIDR("::1/64")
	_, err := subnetAt(pool, 96, 0)
	if err == nil {
		t.Fatal("expected error for IPv6 pool")
	}
}

// --- firstHostIP ---

func TestFirstHostIPVariousSubnets(t *testing.T) {
	tests := []struct {
		cidr string
		want string
	}{
		{"10.0.0.0/24", "10.0.0.1"},
		{"192.168.1.0/24", "192.168.1.1"},
		{"172.16.0.0/16", "172.16.0.1"},
		{"198.18.0.0/30", "198.18.0.1"},
	}
	for _, tt := range tests {
		ip, err := firstHostIP(mustStackCIDR(t, tt.cidr))
		if err != nil {
			t.Fatalf("firstHostIP(%s): %v", tt.cidr, err)
		}
		if ip.String() != tt.want {
			t.Errorf("firstHostIP(%s) = %s, want %s", tt.cidr, ip, tt.want)
		}
	}
}

func TestFirstHostIPIPv6(t *testing.T) {
	_, v6, _ := net.ParseCIDR("::1/128")
	_, err := firstHostIP(v6)
	if err == nil {
		t.Fatal("expected error for IPv6")
	}
}

// --- cidrOverlap ---

func TestCidrOverlapNilCases(t *testing.T) {
	a := mustStackCIDR(t, "10.0.0.0/24")
	if cidrOverlap(nil, a) {
		t.Fatal("nil a should not overlap")
	}
	if cidrOverlap(a, nil) {
		t.Fatal("nil b should not overlap")
	}
	if cidrOverlap(nil, nil) {
		t.Fatal("nil nil should not overlap")
	}
}

func TestCidrOverlapIPv6(t *testing.T) {
	a := mustStackCIDR(t, "10.0.0.0/24")
	_, b, _ := net.ParseCIDR("::1/128")
	if cidrOverlap(a, b) {
		t.Fatal("mixed should not overlap")
	}
}

func TestCidrOverlapTableDriven(t *testing.T) {
	tests := []struct {
		a, b    string
		overlap bool
	}{
		{"10.0.0.0/24", "10.0.0.0/24", true},
		{"10.0.0.0/24", "10.0.0.128/25", true},
		{"10.0.0.0/24", "10.0.1.0/24", false},
		{"192.168.0.0/16", "192.168.1.0/24", true},
	}
	for _, tt := range tests {
		got := cidrOverlap(mustStackCIDR(t, tt.a), mustStackCIDR(t, tt.b))
		if got != tt.overlap {
			t.Errorf("cidrOverlap(%s, %s) = %v, want %v", tt.a, tt.b, got, tt.overlap)
		}
	}
}

// --- normalizeIPv4Net ---

func TestNormalizeIPv4NetNil(t *testing.T) {
	if normalizeIPv4Net(nil) != nil {
		t.Fatal("nil should return nil")
	}
}

func TestNormalizeIPv4NetIPv6(t *testing.T) {
	_, v6, _ := net.ParseCIDR("::1/128")
	if normalizeIPv4Net(v6) != nil {
		t.Fatal("IPv6 should return nil")
	}
}

func TestNormalizeIPv4NetValid(t *testing.T) {
	n := mustStackCIDR(t, "10.0.0.0/24")
	got := normalizeIPv4Net(n)
	if got == nil {
		t.Fatal("should not be nil")
	}
	if got.String() != "10.0.0.0/24" {
		t.Fatalf("got %s", got)
	}
}

func TestNormalizeIPv4NetWith16ByteMask(t *testing.T) {
	ip := net.ParseIP("10.0.0.0").To4()
	mask := net.CIDRMask(24, 128)
	network := &net.IPNet{IP: ip, Mask: mask}
	got := normalizeIPv4Net(network)
	if got == nil {
		t.Fatal("should handle 16-byte mask")
	}
	ones, bits := got.Mask.Size()
	if bits != 32 || ones != 24 {
		t.Fatalf("mask = /%d of %d bits", ones, bits)
	}
}

// --- incrementIP ---

func TestIncrementIP(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"10.0.0.1", "10.0.0.2"},
		{"10.0.0.255", "10.0.1.0"},
		{"255.255.255.255", "0.0.0.0"},
	}
	for _, tt := range tests {
		ip := net.ParseIP(tt.input).To4()
		incrementIP(ip)
		if ip.String() != tt.want {
			t.Errorf("incrementIP(%s) = %s, want %s", tt.input, ip, tt.want)
		}
	}
}

// --- overlapsAny ---

func TestOverlapsAnyEmpty(t *testing.T) {
	if overlapsAny(mustStackCIDR(t, "10.0.0.0/24"), nil) {
		t.Fatal("should not overlap empty list")
	}
}

func TestOverlapsAnyFindsOverlap(t *testing.T) {
	candidate := mustStackCIDR(t, "10.0.0.0/24")
	occupied := []*net.IPNet{
		mustStackCIDR(t, "192.168.0.0/16"),
		mustStackCIDR(t, "10.0.0.0/8"),
	}
	if !overlapsAny(candidate, occupied) {
		t.Fatal("should overlap 10.0.0.0/8")
	}
}

// --- shortIfName ---

func TestShortIfNameShort(t *testing.T) {
	if got := shortIfName("eth0"); got != "eth0" {
		t.Fatalf("got %q", got)
	}
}

func TestShortIfNameExact15(t *testing.T) {
	name := "abcdefghijklmno" // 15 chars
	if got := shortIfName(name); got != name {
		t.Fatalf("got %q, want %q", got, name)
	}
}

func TestShortIfNameLong(t *testing.T) {
	name := "abcdefghijklmnop" // 16 chars
	got := shortIfName(name)
	if len(got) > 15 {
		t.Fatalf("len = %d, want <= 15", len(got))
	}
	// first 4 + last 11
	if got != "abcdfghijklmnop" {
		t.Fatalf("got %q", got)
	}
}

// --- hashProject ---

func TestHashProjectConsistency(t *testing.T) {
	h1 := hashProject("test-project")
	h2 := hashProject("test-project")
	if h1 != h2 {
		t.Fatal("should be deterministic")
	}
}

func TestHashProjectVariation(t *testing.T) {
	if hashProject("a") == hashProject("b") {
		t.Fatal("different inputs should differ")
	}
}

// --- GatewayIP and GuestCIDR nil manager ---

func TestManagerGatewayIPNilSubnet(t *testing.T) {
	mgr := &Manager{}
	got := mgr.GatewayIP()
	if got != "" {
		t.Fatalf("GatewayIP with nil gateway = %q", got)
	}
}

func TestManagerGuestCIDRNilSubnet(t *testing.T) {
	mgr := &Manager{}
	got := mgr.GuestCIDR("10.0.0.2")
	// When subnet is nil, GuestCIDR returns just the IP
	if got != "10.0.0.2" {
		t.Fatalf("GuestCIDR with nil subnet = %q, want just the IP", got)
	}
}

// --- gatewayCIDR ---

func TestGatewayCIDRFormatting(t *testing.T) {
	subnet := mustStackCIDR(t, "172.16.0.0/16")
	gw := net.ParseIP("172.16.0.1")
	got := gatewayCIDR(subnet, gw)
	if got != "172.16.0.1/16" {
		t.Fatalf("got %q", got)
	}
}
