package hostnet

import (
	"net"
	"testing"
)

// ---------- nil AutoNetwork getters ----------

func TestNilAutoNetworkGetters(t *testing.T) {
	var n *AutoNetwork

	if n.TapName() != "" {
		t.Fatal("nil TapName should be empty")
	}
	if n.GuestCIDR() != "" {
		t.Fatal("nil GuestCIDR should be empty")
	}
	if n.GuestIP() != "" {
		t.Fatal("nil GuestIP should be empty")
	}
	if n.GatewayIP() != "" {
		t.Fatal("nil GatewayIP should be empty")
	}
	if n.UpstreamInterface() != "" {
		t.Fatal("nil UpstreamInterface should be empty")
	}
}

func TestAutoNetworkGettersNilSubnet(t *testing.T) {
	n := &AutoNetwork{
		tapName: "tap0",
	}
	if n.GuestCIDR() != "" {
		t.Fatal("GuestCIDR with nil subnet should be empty")
	}
}

func TestAutoNetworkGettersNilGuest(t *testing.T) {
	n := &AutoNetwork{
		tapName: "tap0",
		subnet:  mustCIDR(t, "10.0.0.0/24"),
	}
	if n.GuestCIDR() != "" {
		t.Fatal("GuestCIDR with nil guest should be empty")
	}
	if n.GuestIP() != "" {
		t.Fatal("GuestIP with nil guest should be empty")
	}
}

func TestAutoNetworkGettersNilGateway(t *testing.T) {
	n := &AutoNetwork{
		tapName: "tap0",
		gateway: nil,
	}
	if n.GatewayIP() != "" {
		t.Fatal("GatewayIP with nil gateway should be empty")
	}
}

// ---------- selectAvailableSubnet edge cases ----------

func TestSelectAvailableSubnetDeterministic(t *testing.T) {
	s1, err := selectAvailableSubnet("project-x", nil)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	s2, err := selectAvailableSubnet("project-x", nil)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if s1.String() != s2.String() {
		t.Fatalf("same project should get same subnet: %s != %s", s1, s2)
	}
}

func TestSelectAvailableSubnetDifferentProjects(t *testing.T) {
	s1, _ := selectAvailableSubnet("project-a", nil)
	s2, _ := selectAvailableSubnet("project-b", nil)
	// Different projects should (almost certainly) get different subnets
	if s1.String() == s2.String() {
		t.Fatalf("different projects got same subnet: %s", s1)
	}
}

func TestSelectAvailableSubnetWithHeavyOccupation(t *testing.T) {
	// Occupy a few subnets near where the hash would land
	s1, _ := selectAvailableSubnet("test-project", nil)
	occupied := []*net.IPNet{s1}
	s2, err := selectAvailableSubnet("test-project", occupied)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if s2.String() == s1.String() {
		t.Fatalf("should have picked a different subnet, both are %s", s1)
	}
}

// ---------- subnetAt ----------

func TestSubnetAtDifferentIndices(t *testing.T) {
	pool := mustCIDR(t, "198.18.0.0/15")
	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		s, err := subnetAt(pool, autoSubnetPrefix, i)
		if err != nil {
			t.Fatalf("subnetAt(%d): %v", i, err)
		}
		key := s.String()
		if seen[key] {
			t.Fatalf("duplicate subnet at index %d: %s", i, key)
		}
		seen[key] = true
	}
}

func TestSubnetAtInvalidPrefix(t *testing.T) {
	pool := mustCIDR(t, "198.18.0.0/15")
	_, err := subnetAt(pool, 14, 0) // prefix < pool prefix
	if err == nil {
		t.Fatal("expected error for prefix < pool prefix")
	}
}

// ---------- firstHostIP ----------

func TestFirstHostIPNil(t *testing.T) {
	_, err := firstHostIP(nil)
	if err == nil {
		t.Fatal("expected error for nil network")
	}
}

func TestFirstHostIPVariousSubnets(t *testing.T) {
	tests := []struct {
		cidr string
		want string
	}{
		{"10.0.0.0/24", "10.0.0.1"},
		{"192.168.1.0/24", "192.168.1.1"},
		{"198.18.0.0/30", "198.18.0.1"},
		{"172.16.0.0/16", "172.16.0.1"},
	}
	for _, tt := range tests {
		ip, err := firstHostIP(mustCIDR(t, tt.cidr))
		if err != nil {
			t.Fatalf("firstHostIP(%s): %v", tt.cidr, err)
		}
		if ip.String() != tt.want {
			t.Errorf("firstHostIP(%s) = %s, want %s", tt.cidr, ip, tt.want)
		}
	}
}

// ---------- incrementIP ----------

func TestIncrementIP(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"10.0.0.1", "10.0.0.2"},
		{"10.0.0.255", "10.0.1.0"},
		{"0.0.0.0", "0.0.0.1"},
	}
	for _, tt := range tests {
		ip := net.ParseIP(tt.input).To4()
		incrementIP(ip)
		if ip.String() != tt.want {
			t.Errorf("incrementIP(%s) = %s, want %s", tt.input, ip, tt.want)
		}
	}
}

// ---------- cidrOverlap ----------

func TestCidrOverlapNil(t *testing.T) {
	a := mustCIDR(t, "10.0.0.0/24")
	if cidrOverlap(nil, a) {
		t.Fatal("nil should not overlap")
	}
	if cidrOverlap(a, nil) {
		t.Fatal("nil should not overlap")
	}
	if cidrOverlap(nil, nil) {
		t.Fatal("nil should not overlap")
	}
}

func TestCidrOverlapTableDriven(t *testing.T) {
	tests := []struct {
		a, b    string
		overlap bool
	}{
		{"10.0.0.0/24", "10.0.0.0/24", true},   // identical
		{"10.0.0.0/24", "10.0.0.0/16", true},   // contained
		{"10.0.0.0/24", "10.0.1.0/24", false},  // adjacent, no overlap
		{"192.168.0.0/16", "192.168.1.0/24", true},
		{"10.0.0.0/8", "11.0.0.0/8", false},
	}
	for _, tt := range tests {
		a := mustCIDR(t, tt.a)
		b := mustCIDR(t, tt.b)
		got := cidrOverlap(a, b)
		if got != tt.overlap {
			t.Errorf("cidrOverlap(%s, %s) = %v, want %v", tt.a, tt.b, got, tt.overlap)
		}
	}
}

// ---------- normalizeIPv4Net ----------

func TestNormalizeIPv4NetIPv6(t *testing.T) {
	_, net6, _ := net.ParseCIDR("::1/128")
	got := normalizeIPv4Net(net6)
	if got != nil {
		t.Fatal("IPv6 should normalize to nil")
	}
}

func TestNormalizeIPv4NetValid(t *testing.T) {
	_, network, _ := net.ParseCIDR("10.0.0.0/24")
	got := normalizeIPv4Net(network)
	if got == nil {
		t.Fatal("valid IPv4 should not normalize to nil")
	}
	if got.String() != "10.0.0.0/24" {
		t.Fatalf("normalized = %s, want 10.0.0.0/24", got)
	}
}

func TestNormalizeIPv4NetWithIPv4MappedIPv6(t *testing.T) {
	// net.ParseCIDR can produce 16-byte IPs for IPv4 addresses
	ip := net.IP([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 10, 0, 0, 0})
	mask := net.CIDRMask(24, 128)
	network := &net.IPNet{IP: ip, Mask: mask}
	got := normalizeIPv4Net(network)
	if got == nil {
		t.Fatal("IPv4-mapped IPv6 should normalize to valid IPv4")
	}
}

// ---------- autoTapName ----------

func TestAutoTapNameShort(t *testing.T) {
	name := autoTapName("web")
	if name != "gct-web" {
		t.Fatalf("autoTapName(web) = %q, want gct-web", name)
	}
}

func TestAutoTapNameLong(t *testing.T) {
	name := autoTapName("this-is-a-very-long-project-name")
	if len(name) > 15 {
		t.Fatalf("autoTapName(long) = %q (len %d), must be <= 15", name, len(name))
	}
	// Should use hash-based fallback
	if name[:4] != "gct-" {
		t.Fatalf("autoTapName(long) prefix = %q, want gct-", name[:4])
	}
}

func TestAutoTapNameEmpty(t *testing.T) {
	name := autoTapName("")
	if name != "gct-gc" {
		t.Fatalf("autoTapName('') = %q, want gct-gc", name)
	}
}

func TestAutoTapNameSpecialChars(t *testing.T) {
	name := autoTapName("My!@#$Project")
	if len(name) > 15 {
		t.Fatalf("len = %d, want <= 15", len(name))
	}
	// Should strip special chars
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
			t.Fatalf("autoTapName contains invalid char %q in %q", string(r), name)
		}
	}
}

// ---------- gatewayCIDR ----------

func TestGatewayCIDR(t *testing.T) {
	subnet := mustCIDR(t, "10.0.0.0/24")
	gw := net.ParseIP("10.0.0.1")
	got := gatewayCIDR(subnet, gw)
	if got != "10.0.0.1/24" {
		t.Fatalf("gatewayCIDR = %q, want 10.0.0.1/24", got)
	}
}

// ---------- hashProject ----------

func TestHashProjectConsistency(t *testing.T) {
	h1 := hashProject("test")
	h2 := hashProject("test")
	if h1 != h2 {
		t.Fatal("hashProject should be deterministic")
	}
}

func TestHashProjectVariation(t *testing.T) {
	h1 := hashProject("project-a")
	h2 := hashProject("project-b")
	if h1 == h2 {
		t.Fatal("different projects should have different hashes (with overwhelming probability)")
	}
}

// ---------- overlapsAny ----------

func TestOverlapsAnyEmpty(t *testing.T) {
	candidate := mustCIDR(t, "10.0.0.0/24")
	if overlapsAny(candidate, nil) {
		t.Fatal("should not overlap with empty list")
	}
}

// ---------- Activate and Close on nil ----------

func TestActivateNilNoPanic(t *testing.T) {
	var n *AutoNetwork
	err := n.Activate()
	if err != nil {
		t.Fatalf("nil Activate should return nil, got %v", err)
	}
}

func TestCloseNilNoPanic(t *testing.T) {
	var n *AutoNetwork
	n.Close() // should not panic
}

// ---------- selectAvailableSubnet with different occupied sets ----------

func TestSelectAvailableSubnetAvoidsSingleOccupied(t *testing.T) {
	// Get the initial subnet for the project
	initial, _ := selectAvailableSubnet("myproject", nil)
	// Now occupy it and try again
	occupied := []*net.IPNet{initial}
	next, err := selectAvailableSubnet("myproject", occupied)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if overlapsAny(next, occupied) {
		t.Fatalf("new subnet %s overlaps occupied", next)
	}
}

func TestSelectAvailableSubnetAvoidsMultipleOccupied(t *testing.T) {
	var occupied []*net.IPNet
	for i := 0; i < 5; i++ {
		s, err := selectAvailableSubnet("myproject", occupied)
		if err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		if overlapsAny(s, occupied) {
			t.Fatalf("round %d: %s overlaps occupied", i, s)
		}
		occupied = append(occupied, s)
	}
}

// ---------- normalizeIPv4Net with 16-byte mask ----------

func TestNormalizeIPv4NetWith16ByteMask(t *testing.T) {
	// Create a network with IPv4 IP but IPv6-length mask
	ip := net.ParseIP("10.0.0.0").To4()
	mask := net.CIDRMask(24, 128) // 16-byte mask
	network := &net.IPNet{IP: ip, Mask: mask}
	got := normalizeIPv4Net(network)
	if got == nil {
		t.Fatal("should handle 16-byte mask for IPv4 network")
	}
	ones, bits := got.Mask.Size()
	if bits != 32 {
		t.Fatalf("normalized mask bits = %d, want 32", bits)
	}
	if ones != 24 {
		t.Fatalf("normalized mask ones = %d, want 24", ones)
	}
}
