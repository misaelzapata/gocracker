package hostnet

import (
	"net"
	"testing"
)

func mustCIDR(t *testing.T, value string) *net.IPNet {
	t.Helper()
	_, network, err := net.ParseCIDR(value)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", value, err)
	}
	return network
}

func TestAutoNetworkGetters(t *testing.T) {
	n := &AutoNetwork{
		tapName:        "tap0",
		subnet:         mustCIDR(t, "198.18.0.0/30"),
		gateway:        net.ParseIP("198.18.0.1"),
		guest:          net.ParseIP("198.18.0.2"),
		upstreamIfName: "eth0",
	}
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"TapName", n.TapName(), "tap0"},
		{"GuestCIDR", n.GuestCIDR(), "198.18.0.2/30"},
		{"GuestIP", n.GuestIP(), "198.18.0.2"},
		{"GatewayIP", n.GatewayIP(), "198.18.0.1"},
		{"UpstreamInterface", n.UpstreamInterface(), "eth0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("%s() = %q, want %q", tc.name, tc.got, tc.want)
			}
		})
	}
}

func TestSelectAvailableSubnetAvoidsOverlap(t *testing.T) {
	occupied := []*net.IPNet{
		mustCIDR(t, "198.18.0.0/30"),
		mustCIDR(t, "198.18.0.4/30"),
	}
	got, err := selectAvailableSubnet("project", occupied)
	if err != nil {
		t.Fatalf("selectAvailableSubnet() error = %v", err)
	}
	if overlapsAny(got, occupied) {
		t.Fatalf("selected overlapping subnet %s", got)
	}
}

func TestSubnetHelpers(t *testing.T) {
	pool := mustCIDR(t, "198.18.0.0/15")
	subnet, err := subnetAt(pool, autoSubnetPrefix, 2)
	if err != nil {
		t.Fatalf("subnetAt() error = %v", err)
	}
	if subnet.String() == "" {
		t.Fatal("subnetAt() returned empty subnet")
	}
	if _, err := subnetAt(&net.IPNet{IP: net.ParseIP("::1"), Mask: net.CIDRMask(64, 128)}, 30, 0); err == nil {
		t.Fatal("subnetAt(non-ipv4) error = nil")
	}
	ip, err := firstHostIP(mustCIDR(t, "198.18.0.8/30"))
	if err != nil {
		t.Fatalf("firstHostIP() error = %v", err)
	}
	if got := ip.String(); got != "198.18.0.9" {
		t.Fatalf("firstHostIP() = %s", got)
	}
}

func TestNetworkHelpers(t *testing.T) {
	a := mustCIDR(t, "198.18.0.0/30")
	b := mustCIDR(t, "198.18.0.0/29")
	c := mustCIDR(t, "198.18.0.8/30")
	if !cidrOverlap(a, b) || cidrOverlap(a, c) {
		t.Fatal("unexpected overlap result")
	}
	if !overlapsAny(a, []*net.IPNet{c, b}) {
		t.Fatal("expected overlapsAny() to report overlap")
	}
	if got := gatewayCIDR(a, net.ParseIP("198.18.0.1")); got != "198.18.0.1/30" {
		t.Fatalf("gatewayCIDR() = %q", got)
	}
	if normalizeIPv4Net(nil) != nil {
		t.Fatal("normalizeIPv4Net(nil) should be nil")
	}
	if got := autoTapName("Project Name With Spaces"); len(got) == 0 || len(got) > 15 {
		t.Fatalf("autoTapName() = %q", got)
	}
	if hashProject("a") == hashProject("b") {
		t.Fatal("hashProject() should vary by project")
	}
}
