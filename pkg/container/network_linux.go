//go:build linux

package container

import (
	"fmt"

	"github.com/gocracker/gocracker/internal/hostnet"
)

// setupAutoNetwork creates a TAP-based auto network on Linux.
// Returns the AutoNetwork handle (to be closed later) and updates opts
// with the TAP name, static IP, and gateway.
func setupAutoNetwork(opts *RunOptions) (*hostnet.AutoNetwork, error) {
	autoNet, err := hostnet.NewAuto(opts.ID, opts.TapName)
	if err != nil {
		return nil, fmt.Errorf("auto network: %w", err)
	}
	opts.TapName = autoNet.TapName()
	opts.StaticIP = autoNet.GuestCIDR()
	opts.Gateway = autoNet.GatewayIP()
	return autoNet, nil
}

// activateAutoNetwork enables NAT/forwarding for the auto network on Linux.
func activateAutoNetwork(autoNet *hostnet.AutoNetwork) error {
	if autoNet == nil {
		return nil
	}
	return autoNet.Activate()
}

// closeAutoNetwork tears down the auto network on Linux.
func closeAutoNetwork(autoNet *hostnet.AutoNetwork) {
	if autoNet != nil {
		autoNet.Close()
	}
}
