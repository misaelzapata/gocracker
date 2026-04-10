//go:build darwin

package compose

import (
	"fmt"
	"net"

	"github.com/gocracker/gocracker/internal/stacknet"
)

type networkManager struct {
	*stacknet.Manager
}

func newNetworkManager(project string, subnet *net.IPNet, gateway net.IP) (*networkManager, error) {
	manager, err := stacknet.New(project, subnet, gateway)
	if err != nil {
		return nil, err
	}
	return &networkManager{Manager: manager}, nil
}

func (n *networkManager) AddPortForwards(serviceName, serviceIP string, ports interface{}) error {
	mappings, err := parsePortMappings(ports)
	if err != nil {
		return fmt.Errorf("parse port mappings for %s: %w", serviceName, err)
	}
	return n.Manager.AddPortForwardMappings(serviceName, serviceIP, mappings)
}

func (n *networkManager) NetworkID() string {
	if n == nil || n.Manager == nil {
		return ""
	}
	return n.Manager.NetworkID()
}

func (n *networkManager) NetworkAttachmentMode() string {
	if n == nil || n.Manager == nil {
		return ""
	}
	return n.Manager.NetworkAttachmentMode()
}

// composeTapName on darwin returns empty because shared vmnet does not use TAP devices.
func composeTapName(tapName string) string { return "" }

// composeNetworkMode on darwin leaves auto-NAT disabled for compose stacks.
func composeNetworkMode() string { return "" }

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

func reserveStackIPs(subnet *net.IPNet, gateway net.IP) map[string]string {
	reserved := map[string]string{}
	if subnet == nil {
		return reserved
	}
	base := append(net.IP(nil), subnet.IP.To4()...)
	if base == nil {
		return reserved
	}

	firstReserved := append(net.IP(nil), base...)
	incrementIP(firstReserved)
	reserved[firstReserved.String()] = "vmnet-reserved"

	hostGateway := append(net.IP(nil), base...)
	incrementIP(hostGateway)
	incrementIP(hostGateway)
	reserved[hostGateway.String()] = "gateway"

	last := lastIPv4(subnet)
	if last != nil {
		reserved[last.String()] = "vmnet-reserved"
	}

	if gateway != nil {
		reserved[gateway.String()] = "gateway"
	}
	return reserved
}

func lastIPv4(subnet *net.IPNet) net.IP {
	if subnet == nil {
		return nil
	}
	base := subnet.IP.To4()
	if base == nil {
		return nil
	}
	mask := net.IP(subnet.Mask).To4()
	if mask == nil {
		return nil
	}
	last := append(net.IP(nil), base...)
	for i := range last {
		last[i] |= ^mask[i]
	}
	return last
}
