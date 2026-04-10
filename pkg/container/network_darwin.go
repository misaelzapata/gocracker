//go:build darwin

package container

import "github.com/gocracker/gocracker/internal/hostnet"

// setupAutoNetwork on macOS is a no-op. Virtualization.framework provides
// NAT networking natively through vz.NATNetworkDeviceAttachment. The TAP
// name remains empty so vmm.New creates a NAT device instead.
func setupAutoNetwork(opts *RunOptions) (*hostnet.AutoNetwork, error) {
	// On darwin, --net auto means "use vz NAT". No TAP setup needed.
	// TapName stays empty, which the vz backend interprets as NAT mode.
	return nil, nil
}

// activateAutoNetwork is a no-op on macOS.
func activateAutoNetwork(autoNet *hostnet.AutoNetwork) error {
	return nil
}

// closeAutoNetwork is a no-op on macOS.
func closeAutoNetwork(autoNet *hostnet.AutoNetwork) {}
