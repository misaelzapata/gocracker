//go:build darwin

package stacknet

import (
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Code-Hex/vz/v3"
)

const (
	// SubnetPoolCIDR is the default CIDR pool for compose stack subnets.
	SubnetPoolCIDR = "172.20.0.0/16"
	subnetPrefix   = 24
)

// PortMapping describes a host-to-guest port forwarding rule.
type PortMapping struct {
	HostIP        string `json:"host_ip,omitempty"`
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
	Protocol      string `json:"protocol,omitempty"`
	Name          string `json:"name,omitempty"`
	AppProtocol   string `json:"app_protocol,omitempty"`
	Mode          string `json:"mode,omitempty"`
}

// Manager owns a shared vmnet-backed compose stack network on macOS.
type Manager struct {
	project   string
	networkID string
	network   *vz.VmnetNetwork
	subnet    *net.IPNet
	gateway   net.IP

	mu       sync.Mutex
	forwards []io.Closer
}

var (
	registryMu      sync.RWMutex
	vmnetNetworks   = map[string]*vz.VmnetNetwork{}
	projectManagers = map[string]*Manager{}
)

func New(project string, subnet *net.IPNet, gateway net.IP) (*Manager, error) {
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

	hostGateway, err := firstHostIP(subnet)
	if err != nil {
		return nil, err
	}
	if gateway == nil {
		gateway = hostGateway
	} else {
		gateway = gateway.To4()
		if gateway == nil {
			return nil, fmt.Errorf("compose gateway must be IPv4")
		}
		if !subnet.Contains(gateway) {
			return nil, fmt.Errorf("compose gateway %s is outside subnet %s", gateway, subnet)
		}
		if !gateway.Equal(hostGateway) {
			return nil, fmt.Errorf("darwin shared vmnet requires gateway %s for subnet %s (requested %s)", hostGateway, subnet, gateway)
		}
	}

	cfg, err := vz.NewVmnetNetworkConfiguration(vz.VmnetModeShared)
	if err != nil {
		return nil, wrapVmnetError("create shared vmnet configuration", err)
	}
	if err := cfg.SetIPv4Subnet(subnet); err != nil {
		return nil, wrapVmnetError(fmt.Sprintf("set shared vmnet subnet %s", subnet), err)
	}
	// Guests use deterministic static IPs, so DHCP is unnecessary.
	cfg.DisableDHCP()

	network, err := vz.NewVmnetNetwork(cfg)
	if err != nil {
		return nil, wrapVmnetError("create shared vmnet network", err)
	}
	if actualSubnet, err := network.IPv4Subnet(); err == nil && actualSubnet != nil {
		subnet = normalizeIPv4Net(actualSubnet)
	}
	hostGateway, err = firstHostIP(subnet)
	if err != nil {
		return nil, err
	}
	gateway = hostGateway

	manager := &Manager{
		project:   project,
		networkID: networkIDForProject(project),
		network:   network,
		subnet:    subnet,
		gateway:   gateway,
	}

	registryMu.Lock()
	defer registryMu.Unlock()
	if existing := projectManagers[project]; existing != nil {
		return nil, fmt.Errorf("compose stack network %q already exists", project)
	}
	projectManagers[project] = manager
	vmnetNetworks[manager.networkID] = network
	return manager, nil
}

func Cleanup(project string) {
	registryMu.RLock()
	manager := projectManagers[project]
	registryMu.RUnlock()
	if manager != nil {
		manager.Close()
	}
}

func LookupVmnetNetwork(networkID string) (*vz.VmnetNetwork, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	network, ok := vmnetNetworks[networkID]
	return network, ok
}

func SelectStackSubnet(project string) (*net.IPNet, error) {
	return selectStackSubnet(project)
}

func SelectAvailableSubnet(project string, occupied []*net.IPNet) (*net.IPNet, error) {
	return selectAvailableSubnet(project, occupied)
}

// FirstHostIP returns the gateway address reserved for the host on shared vmnet.
// vmnet reserves the first, second, and last IPv4 addresses in the subnet, and
// the second address is the host-side gateway.
func FirstHostIP(network *net.IPNet) (net.IP, error) {
	return firstHostIP(network)
}

func NormalizeIPv4Net(network *net.IPNet) *net.IPNet {
	return normalizeIPv4Net(network)
}

func CIDROverlap(a, b *net.IPNet) bool {
	return cidrOverlap(a, b)
}

func (m *Manager) GatewayIP() string {
	if m == nil || m.gateway == nil {
		return ""
	}
	return m.gateway.String()
}

func (m *Manager) GuestCIDR(ip string) string {
	if m == nil || m.subnet == nil {
		return ip
	}
	ones, _ := m.subnet.Mask.Size()
	return fmt.Sprintf("%s/%d", ip, ones)
}

func (m *Manager) NetworkID() string {
	if m == nil {
		return ""
	}
	return m.networkID
}

func (m *Manager) NetworkAttachmentMode() string {
	return "stack"
}

func (m *Manager) AttachTap(tapName string) error {
	if strings.TrimSpace(tapName) == "" {
		return nil
	}
	return fmt.Errorf("darwin shared vmnet does not use TAP devices (got %q)", tapName)
}

func (m *Manager) AddPortForwardMappings(serviceName, serviceIP string, mappings []PortMapping) error {
	for _, mapping := range mappings {
		forward, err := startPortForward(mapping.HostIP, mapping.HostPort, serviceIP, mapping.ContainerPort, mapping.Protocol)
		if err != nil {
			return fmt.Errorf("service %s: listen %s:%d -> %s:%d: %w", serviceName, mapping.HostIP, mapping.HostPort, serviceIP, mapping.ContainerPort, err)
		}
		m.mu.Lock()
		m.forwards = append(m.forwards, forward)
		m.mu.Unlock()
	}
	return nil
}

func (m *Manager) Close() {
	if m == nil {
		return
	}

	m.mu.Lock()
	forwards := append([]io.Closer(nil), m.forwards...)
	m.forwards = nil
	m.mu.Unlock()

	for _, forward := range forwards {
		_ = forward.Close()
	}

	registryMu.Lock()
	if current := projectManagers[m.project]; current == m {
		delete(projectManagers, m.project)
	}
	if current := vmnetNetworks[m.networkID]; current == m.network {
		delete(vmnetNetworks, m.networkID)
	}
	registryMu.Unlock()
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

func selectStackSubnet(project string) (*net.IPNet, error) {
	return selectAvailableSubnet(project, nil)
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
	incrementIP(ip)
	return ip, nil
}

func normalizeIPv4Net(network *net.IPNet) *net.IPNet {
	if network == nil {
		return nil
	}
	ip4 := network.IP.To4()
	if ip4 == nil {
		return nil
	}
	mask := network.Mask
	if len(mask) != net.IPv4len {
		mask = net.CIDRMask(maskSize(mask), 32)
	}
	masked := ip4.Mask(mask)
	return &net.IPNet{IP: masked, Mask: mask}
}

func cidrOverlap(a, b *net.IPNet) bool {
	if a == nil || b == nil {
		return false
	}
	return a.Contains(b.IP) || b.Contains(a.IP)
}

func overlapsAny(candidate *net.IPNet, occupied []*net.IPNet) bool {
	for _, existing := range occupied {
		if cidrOverlap(candidate, existing) {
			return true
		}
	}
	return false
}

func incrementIP(ip net.IP) {
	for i := len(ip) - 1; i >= 0; i-- {
		ip[i]++
		if ip[i] != 0 {
			return
		}
	}
}

func maskSize(mask net.IPMask) int {
	ones, _ := mask.Size()
	return ones
}

func hashProject(project string) uint32 {
	h := fnv.New32a()
	_, _ = io.WriteString(h, project)
	return h.Sum32()
}

func networkIDForProject(project string) string {
	return "stack:" + strings.TrimSpace(project)
}

func wrapVmnetError(op string, err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "not authorized") {
		return fmt.Errorf("%s: %w; darwin shared stack networking requires the com.apple.vm.networking entitlement", op, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}
