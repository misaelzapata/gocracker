package stacknet

import (
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
)

const (
	SubnetPoolCIDR = "198.18.0.0/15"
	subnetPrefix   = 24
)

type PortMapping struct {
	HostIP        string `json:"host_ip,omitempty"`
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
	Protocol      string `json:"protocol,omitempty"`
	Name          string `json:"name,omitempty"`
	AppProtocol   string `json:"app_protocol,omitempty"`
	Mode          string `json:"mode,omitempty"`
}

type Manager struct {
	netnsName      string
	bridgeName     string
	hostIfName     string
	bridgePortName string
	subnet         *net.IPNet
	gateway        net.IP

	mu       sync.Mutex
	forwards []io.Closer
}

func New(project string, subnet *net.IPNet, gateway net.IP) (*Manager, error) {
	nsName := "gcns-" + project
	name := shortIfName("gcbr-" + project)
	hostIfName := shortIfName("gch-" + project)
	bridgePortName := shortIfName("gcb-" + project)
	var err error
	if subnet == nil {
		subnet, err = selectStackSubnet(project)
		if err != nil {
			return nil, err
		}
	} else {
		subnet = normalizeIPv4Net(subnet)
		if subnet == nil {
			return nil, fmt.Errorf("compose network must be IPv4")
		}
	}
	if gateway == nil {
		gateway, err = firstHostIP(subnet)
		if err != nil {
			return nil, err
		}
	} else {
		gateway = gateway.To4()
		if gateway == nil {
			return nil, fmt.Errorf("compose gateway must be IPv4")
		}
		if !subnet.Contains(gateway) {
			return nil, fmt.Errorf("compose gateway %s is outside subnet %s", gateway, subnet)
		}
	}
	n := &Manager{
		netnsName:      nsName,
		bridgeName:     name,
		hostIfName:     hostIfName,
		bridgePortName: bridgePortName,
		subnet:         subnet,
		gateway:        gateway,
	}
	_ = runIP("netns", "del", nsName)
	if err := deleteLinkIfExists(hostIfName); err != nil {
		return nil, err
	}
	if err := deleteLinkIfExists(bridgePortName); err != nil {
		return nil, err
	}
	if err := runIP("netns", "add", nsName); err != nil {
		return nil, err
	}
	if err := runIPNetNS(nsName, "link", "add", "name", name, "type", "bridge"); err != nil && !strings.Contains(err.Error(), "File exists") {
		_ = runIP("netns", "del", nsName)
		return nil, err
	}
	if err := runIPNetNS(nsName, "addr", "flush", "dev", name); err != nil {
		_ = runIP("netns", "del", nsName)
		return nil, err
	}
	if err := runIPNetNS(nsName, "link", "set", "dev", name, "up"); err != nil {
		_ = runIP("netns", "del", nsName)
		return nil, err
	}
	if err := runIP("link", "add", "name", hostIfName, "type", "veth", "peer", "name", bridgePortName); err != nil {
		_ = runIP("netns", "del", nsName)
		return nil, err
	}
	if err := runIP("link", "set", "dev", bridgePortName, "netns", nsName); err != nil {
		_ = runIP("link", "del", "dev", hostIfName)
		_ = runIP("netns", "del", nsName)
		return nil, err
	}
	if err := runIPNetNS(nsName, "link", "set", "dev", bridgePortName, "master", name); err != nil {
		_ = runIP("link", "del", "dev", hostIfName)
		_ = runIP("netns", "del", nsName)
		return nil, err
	}
	if err := runIPNetNS(nsName, "link", "set", "dev", bridgePortName, "up"); err != nil {
		_ = runIP("link", "del", "dev", hostIfName)
		_ = runIP("netns", "del", nsName)
		return nil, err
	}
	if err := runIP("addr", "add", gatewayCIDR(subnet, gateway), "dev", hostIfName); err != nil {
		_ = runIP("link", "del", "dev", hostIfName)
		_ = runIP("netns", "del", nsName)
		return nil, err
	}
	if err := runIP("link", "set", "dev", hostIfName, "up"); err != nil {
		_ = runIP("link", "del", "dev", hostIfName)
		_ = runIP("netns", "del", nsName)
		return nil, err
	}
	return n, nil
}

func Cleanup(project string) {
	if project == "" {
		return
	}
	hostIfName := shortIfName("gch-" + project)
	_ = deleteLinkIfExists(hostIfName)
	_ = runIP("netns", "del", "gcns-"+project)
}

func SelectStackSubnet(project string) (*net.IPNet, error) {
	return selectStackSubnet(project)
}

func SelectAvailableSubnet(project string, occupied []*net.IPNet) (*net.IPNet, error) {
	return selectAvailableSubnet(project, occupied)
}

func FirstHostIP(network *net.IPNet) (net.IP, error) {
	return firstHostIP(network)
}

func NormalizeIPv4Net(network *net.IPNet) *net.IPNet {
	return normalizeIPv4Net(network)
}

func CIDROverlap(a, b *net.IPNet) bool {
	return cidrOverlap(a, b)
}

func (n *Manager) GatewayIP() string {
	if n == nil || n.gateway == nil {
		return ""
	}
	return n.gateway.String()
}

func (n *Manager) GuestCIDR(ip string) string {
	if n == nil || n.subnet == nil {
		return ip
	}
	ones, _ := n.subnet.Mask.Size()
	return fmt.Sprintf("%s/%d", ip, ones)
}

func (n *Manager) AttachTap(tapName string) error {
	if err := runIP("link", "set", "dev", tapName, "netns", n.netnsName); err != nil {
		return err
	}
	if err := runIPNetNS(n.netnsName, "link", "set", "dev", tapName, "master", n.bridgeName); err != nil {
		return err
	}
	return runIPNetNS(n.netnsName, "link", "set", "dev", tapName, "up")
}

func (n *Manager) AddPortForwardMappings(serviceName, serviceIP string, mappings []PortMapping) error {
	for _, mapping := range mappings {
		forward, err := startPortForward(mapping.HostIP, mapping.HostPort, serviceIP, mapping.ContainerPort, mapping.Protocol)
		if err != nil {
			return fmt.Errorf("service %s: listen %s:%d -> %s:%d: %w", serviceName, mapping.HostIP, mapping.HostPort, serviceIP, mapping.ContainerPort, err)
		}
		n.mu.Lock()
		n.forwards = append(n.forwards, forward)
		n.mu.Unlock()
	}
	return nil
}

func (n *Manager) Close() {
	n.mu.Lock()
	forwards := append([]io.Closer(nil), n.forwards...)
	n.forwards = nil
	n.mu.Unlock()

	for _, forward := range forwards {
		_ = forward.Close()
	}
	if n.hostIfName != "" {
		_ = deleteLinkIfExists(n.hostIfName)
	}
	if n.netnsName != "" {
		_ = runIP("netns", "del", n.netnsName)
	}
}

type tcpForward struct {
	listener net.Listener
}

func (f *tcpForward) Close() error {
	return f.listener.Close()
}

func startTCPForward(host string, hostPort int, targetIP string, targetPort int) (io.Closer, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(hostPort)))
	if err != nil {
		return nil, err
	}
	forward := &tcpForward{listener: listener}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go proxyConn(conn, net.JoinHostPort(targetIP, strconv.Itoa(targetPort)))
		}
	}()
	return forward, nil
}

type udpForward struct {
	conn    net.PacketConn
	target  *net.UDPAddr
	mu      sync.Mutex
	clients map[string]*udpClient
}

type udpClient struct {
	addr *net.UDPAddr
	conn *net.UDPConn
}

func (f *udpForward) Close() error {
	f.mu.Lock()
	for _, client := range f.clients {
		_ = client.conn.Close()
	}
	f.clients = nil
	f.mu.Unlock()
	return f.conn.Close()
}

func startPortForward(host string, hostPort int, targetIP string, targetPort int, protocol string) (io.Closer, error) {
	switch protocol {
	case "tcp":
		return startTCPForward(host, hostPort, targetIP, targetPort)
	case "udp":
		return startUDPForward(host, hostPort, targetIP, targetPort)
	default:
		return nil, fmt.Errorf("unsupported protocol %q", protocol)
	}
}

func startUDPForward(host string, hostPort int, targetIP string, targetPort int) (io.Closer, error) {
	conn, err := net.ListenPacket("udp", net.JoinHostPort(host, strconv.Itoa(hostPort)))
	if err != nil {
		return nil, err
	}
	target, err := net.ResolveUDPAddr("udp", net.JoinHostPort(targetIP, strconv.Itoa(targetPort)))
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	forward := &udpForward{
		conn:    conn,
		target:  target,
		clients: map[string]*udpClient{},
	}
	go forward.serve()
	return forward, nil
}

func (f *udpForward) serve() {
	buf := make([]byte, 64*1024)
	for {
		n, addr, err := f.conn.ReadFrom(buf)
		if err != nil {
			return
		}
		udpAddr, ok := addr.(*net.UDPAddr)
		if !ok {
			continue
		}
		client, err := f.clientFor(udpAddr)
		if err != nil {
			continue
		}
		_, _ = client.conn.Write(buf[:n])
	}
}

func (f *udpForward) clientFor(addr *net.UDPAddr) (*udpClient, error) {
	key := addr.String()
	f.mu.Lock()
	if client, ok := f.clients[key]; ok {
		f.mu.Unlock()
		return client, nil
	}
	conn, err := net.DialUDP("udp", nil, f.target)
	if err != nil {
		f.mu.Unlock()
		return nil, err
	}
	client := &udpClient{addr: addr, conn: conn}
	f.clients[key] = client
	f.mu.Unlock()

	go f.readReplies(client)
	return client, nil
}

func (f *udpForward) readReplies(client *udpClient) {
	buf := make([]byte, 64*1024)
	for {
		_ = client.conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
		n, err := client.conn.Read(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			break
		}
		_, _ = f.conn.WriteTo(buf[:n], client.addr)
	}
	f.mu.Lock()
	delete(f.clients, client.addr.String())
	f.mu.Unlock()
	_ = client.conn.Close()
}

func proxyConn(src net.Conn, target string) {
	defer src.Close()
	dst, err := net.Dial("tcp", target)
	if err != nil {
		return
	}
	defer dst.Close()

	go func() {
		_, _ = io.Copy(dst, src)
		_ = dst.Close()
	}()
	_, _ = io.Copy(src, dst)
}

func runIP(args ...string) error {
	cmd := exec.Command("ip", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func runIPNetNS(ns string, args ...string) error {
	all := append([]string{"-n", ns}, args...)
	return runIP(all...)
}

func deleteLinkIfExists(name string) error {
	if name == "" {
		return nil
	}
	err := runIP("link", "del", "dev", name)
	if err == nil || strings.Contains(err.Error(), "Cannot find device") {
		return nil
	}
	return err
}

func selectStackSubnet(project string) (*net.IPNet, error) {
	occupied, err := occupiedIPv4Networks()
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
	_, pool, err := net.ParseCIDR(SubnetPoolCIDR)
	if err != nil {
		return nil, err
	}
	ones, _ := pool.Mask.Size()
	availableBits := subnetPrefix - ones
	if availableBits <= 0 {
		return nil, fmt.Errorf("invalid subnet pool %s", SubnetPoolCIDR)
	}
	total := 1 << availableBits
	start := int(hashProject(project) % uint32(total))
	for i := 0; i < total; i++ {
		index := (start + i) % total
		candidate, err := subnetAt(pool, subnetPrefix, index)
		if err != nil {
			return nil, err
		}
		if overlapsAny(candidate, occupied) {
			continue
		}
		return candidate, nil
	}
	return nil, fmt.Errorf("no free /%d network available in %s", subnetPrefix, SubnetPoolCIDR)
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
	_, _ = io.WriteString(h, project)
	return h.Sum32()
}

func incrementIP(ip net.IP) {
	for i := len(ip) - 1; i >= 0; i-- {
		ip[i]++
		if ip[i] != 0 {
			break
		}
	}
}

func shortIfName(value string) string {
	if len(value) <= 15 {
		return value
	}
	return value[:4] + value[len(value)-11:]
}
