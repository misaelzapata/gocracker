package compose

import (
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"path/filepath"
	"strings"

	"github.com/gocracker/gocracker/internal/stacknet"
)

const composeSubnetPoolCIDR = stacknet.SubnetPoolCIDR

type networkManager struct {
	*stacknet.Manager
}

type portMapping = stacknet.PortMapping

type plannedNetwork struct {
	subnet  *net.IPNet
	gateway net.IP
}

func newNetworkManager(project string, subnet *net.IPNet, gateway net.IP) (*networkManager, error) {
	manager, err := stacknet.New(project, subnet, gateway)
	if err != nil {
		return nil, err
	}
	return &networkManager{Manager: manager}, nil
}

func newPlannedNetwork(subnet *net.IPNet, gateway net.IP) *plannedNetwork {
	return &plannedNetwork{subnet: subnet, gateway: gateway}
}

func (n *plannedNetwork) GatewayIP() string {
	if n == nil || n.gateway == nil {
		return ""
	}
	return n.gateway.String()
}

func (n *plannedNetwork) GuestCIDR(ip string) string {
	if n == nil || n.subnet == nil {
		return ip
	}
	ones, _ := n.subnet.Mask.Size()
	return fmt.Sprintf("%s/%d", ip, ones)
}

func (n *plannedNetwork) AttachTap(string) error {
	return nil
}

func (n *plannedNetwork) AddPortForwards(string, string, interface{}) error {
	return nil
}

func (n *plannedNetwork) Close() {}

func (n *networkManager) AddPortForwards(serviceName, serviceIP string, ports interface{}) error {
	mappings, err := parsePortMappings(ports)
	if err != nil {
		return fmt.Errorf("parse port mappings for %s: %w", serviceName, err)
	}
	return n.Manager.AddPortForwardMappings(serviceName, serviceIP, mappings)
}

func cleanupStackNetwork(project string) {
	stacknet.Cleanup(project)
}

func parsePortMappings(value interface{}) ([]portMapping, error) {
	return parsePortSpecs(value)
}

func selectStackSubnet(project string) (*net.IPNet, error) {
	return stacknet.SelectStackSubnet(project)
}

func selectAvailableSubnet(project string, occupied []*net.IPNet) (*net.IPNet, error) {
	return stacknet.SelectAvailableSubnet(project, occupied)
}

func firstHostIP(network *net.IPNet) (net.IP, error) {
	return stacknet.FirstHostIP(network)
}

func normalizeIPv4Net(network *net.IPNet) *net.IPNet {
	return stacknet.NormalizeIPv4Net(network)
}

func cidrOverlap(a, b *net.IPNet) bool {
	return stacknet.CIDROverlap(a, b)
}

func hashProject(project string) uint32 {
	h := fnv.New32a()
	_, _ = io.WriteString(h, project)
	return h.Sum32()
}

func projectName(composePath string) string {
	abs, err := filepath.Abs(composePath)
	if err != nil {
		abs = composePath
	}
	base := filepath.Base(filepath.Dir(abs))
	base = strings.ToLower(base)
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
	return fmt.Sprintf("%s-%x", strings.Trim(base, "-"), hashProject(abs))
}

func shortIfName(value string) string {
	if len(value) <= 15 {
		return value
	}
	return value[:4] + value[len(value)-11:]
}
