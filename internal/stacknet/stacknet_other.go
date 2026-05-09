//go:build !linux

// Stacknet's Linux implementation builds compose-stack networking on top of
// netns + bridge + veth. On non-Linux hosts the equivalent will be the
// in-process L2 switch landing in Phase 9. Until then this stub satisfies
// callers (internal/api, internal/compose) at compile-time and surfaces a
// clear error if the API is exercised.
package stacknet

import (
	"errors"
	"io"
	"net"
)

const SubnetPoolCIDR = "198.18.0.0/15"

var errNotSupported = errors.New("stacknet (compose multi-VM bridge) is Linux-only; Windows/macOS support pending Phase 9")

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
	subnet  *net.IPNet
	gateway net.IP
}

func New(project string, subnet *net.IPNet, gateway net.IP) (*Manager, error) {
	return nil, errNotSupported
}

func (n *Manager) GatewayIP() string         { return "" }
func (n *Manager) GuestCIDR(ip string) string { return "" }
func (n *Manager) AttachTap(tapName string) error { return errNotSupported }
func (n *Manager) AddPortForwardMappings(serviceName, serviceIP string, mappings []PortMapping) error {
	return errNotSupported
}
func (n *Manager) Close() {}

// Closer is unused on non-Linux but kept to mirror Linux's import surface.
var _ = io.Closer(nil)

func Cleanup(project string) {}

func SelectStackSubnet(project string) (*net.IPNet, error) {
	return nil, errNotSupported
}
func SelectAvailableSubnet(project string, occupied []*net.IPNet) (*net.IPNet, error) {
	return nil, errNotSupported
}
func FirstHostIP(network *net.IPNet) (net.IP, error) { return nil, errNotSupported }
func NormalizeIPv4Net(network *net.IPNet) *net.IPNet { return nil }
func CIDROverlap(a, b *net.IPNet) bool                { return false }
