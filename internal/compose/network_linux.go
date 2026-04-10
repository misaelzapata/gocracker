//go:build linux

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
	return ""
}

func (n *networkManager) NetworkAttachmentMode() string {
	return ""
}

// composeTapName returns the TAP device name for a compose service on Linux.
func composeTapName(tapName string) string { return tapName }

// composeNetworkMode returns the network mode for compose services on Linux.
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
	if gateway != nil {
		reserved[gateway.String()] = "gateway"
	}
	return reserved
}
