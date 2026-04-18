package hostnet

import (
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
)

const (
	autoSubnetPoolCIDR = "198.18.0.0/15"
	autoSubnetPrefix   = 30
)

type AutoNetwork struct {
	project        string
	tapName        string
	subnet         *net.IPNet
	gateway        net.IP
	guest          net.IP
	upstreamIfName string
	iptablesCmd    string
	prevIPForward  string

	cleanupOnce sync.Once
}

func NewAuto(project, tapName string) (*AutoNetwork, error) {
	if project == "" {
		project = "default"
	}
	subnet, err := selectAutoSubnet(project)
	if err != nil {
		return nil, err
	}
	gateway, err := firstHostIP(subnet)
	if err != nil {
		return nil, err
	}
	guest := append(net.IP(nil), gateway...)
	incrementIP(guest)

	upstream, err := defaultIPv4RouteInterface()
	if err != nil {
		return nil, err
	}
	iptablesCmd, err := findIPTablesBinary()
	if err != nil {
		return nil, err
	}
	if tapName == "" {
		tapName = autoTapName(project)
	}
	return &AutoNetwork{
		project:        project,
		tapName:        tapName,
		subnet:         subnet,
		gateway:        gateway,
		guest:          guest,
		upstreamIfName: upstream,
		iptablesCmd:    iptablesCmd,
	}, nil
}

func (n *AutoNetwork) TapName() string {
	if n == nil {
		return ""
	}
	return n.tapName
}

func (n *AutoNetwork) GuestCIDR() string {
	if n == nil || n.subnet == nil || n.guest == nil {
		return ""
	}
	ones, _ := n.subnet.Mask.Size()
	return fmt.Sprintf("%s/%d", n.guest.String(), ones)
}

func (n *AutoNetwork) GuestIP() string {
	if n == nil || n.guest == nil {
		return ""
	}
	return n.guest.String()
}

func (n *AutoNetwork) GatewayIP() string {
	if n == nil || n.gateway == nil {
		return ""
	}
	return n.gateway.String()
}

func (n *AutoNetwork) UpstreamInterface() string {
	if n == nil {
		return ""
	}
	return n.upstreamIfName
}

func (n *AutoNetwork) Activate() error {
	if n == nil {
		return nil
	}
	if err := runIP("addr", "flush", "dev", n.tapName); err != nil {
		return err
	}
	if err := runIP("addr", "add", gatewayCIDR(n.subnet, n.gateway), "dev", n.tapName); err != nil {
		return err
	}
	if err := runIP("link", "set", "dev", n.tapName, "up"); err != nil {
		return err
	}
	if err := n.enableIPv4Forwarding(); err != nil {
		return err
	}
	if err := n.addFirewallRules(); err != nil {
		n.Close()
		return err
	}
	return nil
}

func (n *AutoNetwork) Close() {
	if n == nil {
		return
	}
	n.cleanupOnce.Do(func() {
		_ = n.deleteFirewallRule([]string{"-D", "FORWARD", "-i", n.upstreamIfName, "-o", n.tapName, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"})
		_ = n.deleteFirewallRule([]string{"-D", "FORWARD", "-i", n.tapName, "-o", n.upstreamIfName, "-j", "ACCEPT"})
		_ = n.deleteFirewallRule([]string{"-t", "nat", "-D", "POSTROUTING", "-o", n.upstreamIfName, "-s", n.GuestIP(), "-j", "MASQUERADE"})
		if n.prevIPForward != "" && strings.TrimSpace(n.prevIPForward) != "1" {
			_ = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte(n.prevIPForward+"\n"), 0644)
		}
	})
}

func (n *AutoNetwork) enableIPv4Forwarding() error {
	data, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil {
		return fmt.Errorf("read ip_forward: %w", err)
	}
	n.prevIPForward = strings.TrimSpace(string(data))
	if n.prevIPForward == "1" {
		return nil
	}
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0644); err != nil {
		return fmt.Errorf("enable ip_forward: %w", err)
	}
	return nil
}

func (n *AutoNetwork) addFirewallRules() error {
	rules := [][]string{
		{"-t", "nat", "-A", "POSTROUTING", "-o", n.upstreamIfName, "-s", n.GuestIP(), "-j", "MASQUERADE"},
		{"-I", "FORWARD", "1", "-i", n.tapName, "-o", n.upstreamIfName, "-j", "ACCEPT"},
		{"-I", "FORWARD", "1", "-i", n.upstreamIfName, "-o", n.tapName, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
	}
	for _, rule := range rules {
		if err := n.runIPTables(rule...); err != nil {
			return err
		}
	}
	return nil
}

func (n *AutoNetwork) deleteFirewallRule(args []string) error {
	return n.runIPTables(args...)
}

func (n *AutoNetwork) runIPTables(args ...string) error {
	cmd := exec.Command(n.iptablesCmd, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", filepath.Base(n.iptablesCmd), strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func findIPTablesBinary() (string, error) {
	for _, candidate := range []string{"iptables-nft", "iptables"} {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("iptables-nft or iptables not found on host")
}

func defaultIPv4RouteInterface() (string, error) {
	for _, probe := range []string{"1.1.1.1", "8.8.8.8"} {
		routes, err := netlink.RouteGet(net.ParseIP(probe))
		if err != nil {
			continue
		}
		for _, route := range routes {
			if route.LinkIndex == 0 {
				continue
			}
			link, err := netlink.LinkByIndex(route.LinkIndex)
			if err != nil {
				continue
			}
			if name := link.Attrs().Name; name != "" {
				return name, nil
			}
		}
	}

	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return "", fmt.Errorf("list IPv4 routes: %w", err)
	}
	sort.SliceStable(routes, func(i, j int) bool {
		return routes[i].Priority < routes[j].Priority
	})
	for _, route := range routes {
		if route.Dst != nil || route.LinkIndex == 0 {
			continue
		}
		link, err := netlink.LinkByIndex(route.LinkIndex)
		if err != nil {
			continue
		}
		if name := link.Attrs().Name; name != "" {
			return name, nil
		}
	}
	return "", fmt.Errorf("no IPv4 default route found")
}

func runIP(args ...string) error {
	cmd := exec.Command("ip", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func selectAutoSubnet(project string) (*net.IPNet, error) {
	// Retry up to 10 times on ErrDumpInterrupted: the kernel returns this when
	// a concurrent route-table modification races with our netlink dump. A
	// short backoff (2ms, 4ms, 8ms, …) is enough to let the other writer
	// finish. 10 attempts (max ~1s cumulative wait) handles heavy concurrency.
	var occupied []*net.IPNet
	var err error
	for attempt := range 10 {
		occupied, err = occupiedIPv4Networks()
		if err == nil || !errors.Is(err, netlink.ErrDumpInterrupted) {
			break
		}
		time.Sleep(time.Duration(2<<attempt) * time.Millisecond) // 2,4,8,16,32,64,128,256,512,1024 ms
	}
	if err != nil {
		return nil, fmt.Errorf("list occupied networks: %w", err)
	}
	return selectAvailableSubnet(project, occupied)
}

func occupiedIPv4Networks() ([]*net.IPNet, error) {
	seen := map[string]bool{}
	var networks []*net.IPNet

	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}
	for _, route := range routes {
		if route.Dst == nil {
			continue
		}
		network := normalizeIPv4Net(route.Dst)
		if network == nil {
			continue
		}
		if ones, _ := network.Mask.Size(); ones == 0 {
			continue
		}
		key := network.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		networks = append(networks, network)
	}

	links, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}
	for _, link := range links {
		addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
		if err != nil {
			return nil, err
		}
		for _, addr := range addrs {
			network := normalizeIPv4Net(addr.IPNet)
			if network == nil {
				continue
			}
			key := network.String()
			if seen[key] {
				continue
			}
			seen[key] = true
			networks = append(networks, network)
		}
	}
	return networks, nil
}

func selectAvailableSubnet(project string, occupied []*net.IPNet) (*net.IPNet, error) {
	_, pool, err := net.ParseCIDR(autoSubnetPoolCIDR)
	if err != nil {
		return nil, err
	}
	ones, _ := pool.Mask.Size()
	availableBits := autoSubnetPrefix - ones
	if availableBits <= 0 {
		return nil, fmt.Errorf("invalid subnet pool %s", autoSubnetPoolCIDR)
	}
	total := 1 << availableBits
	start := int(hashProject(project) % uint32(total))
	for i := 0; i < total; i++ {
		index := (start + i) % total
		candidate, err := subnetAt(pool, autoSubnetPrefix, index)
		if err != nil {
			return nil, err
		}
		if overlapsAny(candidate, occupied) {
			continue
		}
		return candidate, nil
	}
	return nil, fmt.Errorf("no free /%d network available in %s", autoSubnetPrefix, autoSubnetPoolCIDR)
}

func subnetAt(pool *net.IPNet, prefixBits, index int) (*net.IPNet, error) {
	base := pool.IP.To4()
	if base == nil {
		return nil, fmt.Errorf("subnet pool must be IPv4")
	}
	ones, bits := pool.Mask.Size()
	if bits != 32 || prefixBits < ones || prefixBits > bits {
		return nil, fmt.Errorf("invalid subnet prefix /%d for pool %s", prefixBits, pool.String())
	}
	blockSize := 1 << (bits - prefixBits)
	offset := index * blockSize
	ip := append(net.IP(nil), base...)
	for i := len(ip) - 1; i >= 0 && offset > 0; i-- {
		offset += int(ip[i])
		ip[i] = byte(offset & 0xff)
		offset >>= 8
	}
	mask := net.CIDRMask(prefixBits, 32)
	return &net.IPNet{IP: ip.Mask(mask), Mask: mask}, nil
}

func firstHostIP(network *net.IPNet) (net.IP, error) {
	if network == nil {
		return nil, fmt.Errorf("network is nil")
	}
	ip := append(net.IP(nil), network.IP.To4()...)
	if ip == nil {
		return nil, fmt.Errorf("network must be IPv4")
	}
	incrementIP(ip)
	return ip, nil
}

func incrementIP(ip net.IP) {
	for i := len(ip) - 1; i >= 0; i-- {
		ip[i]++
		if ip[i] != 0 {
			break
		}
	}
}

func gatewayCIDR(network *net.IPNet, gateway net.IP) string {
	ones, _ := network.Mask.Size()
	return fmt.Sprintf("%s/%d", gateway.String(), ones)
}

func overlapsAny(candidate *net.IPNet, occupied []*net.IPNet) bool {
	for _, existing := range occupied {
		if cidrOverlap(candidate, existing) {
			return true
		}
	}
	return false
}

func cidrOverlap(a, b *net.IPNet) bool {
	if a == nil || b == nil {
		return false
	}
	a = normalizeIPv4Net(a)
	b = normalizeIPv4Net(b)
	if a == nil || b == nil {
		return false
	}
	return a.Contains(b.IP) || b.Contains(a.IP)
}

func normalizeIPv4Net(network *net.IPNet) *net.IPNet {
	if network == nil {
		return nil
	}
	ip := network.IP.To4()
	if ip == nil {
		return nil
	}
	mask := network.Mask
	if len(mask) != net.IPv4len {
		ones, _ := mask.Size()
		mask = net.CIDRMask(ones, 32)
	}
	return &net.IPNet{IP: ip.Mask(mask), Mask: mask}
}

func hashProject(project string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(project))
	return h.Sum32()
}

func autoTapName(project string) string {
	base := strings.ToLower(project)
	base = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, base)
	base = strings.Trim(base, "-")
	if base == "" {
		base = "gc"
	}
	name := "gct-" + base
	if len(name) <= 15 {
		return name
	}
	return "gct-" + fmt.Sprintf("%08x", hashProject(project))
}
