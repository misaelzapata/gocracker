package pool

import (
	"context"
	"time"

	"github.com/gocracker/gocracker/internal/toolbox/agent"
	"github.com/gocracker/gocracker/internal/toolbox/client"
)

// ToolboxNetworker is the production Networker. It dials the guest's
// toolbox UDS and POSTs /internal/setnetwork over the
// Firecracker-style CONNECT bridge — same API the sandboxd Create
// path uses to apply network config to a fresh VM.
//
// Construct once per pool (or once per Manager) and pass to
// Pool.SetNetworker. Stateless — every call builds a fresh Client
// against the per-VM UDSPath that the lease handler hands over.
type ToolboxNetworker struct {
	// DialTimeout caps the UDS dial + CONNECT handshake. Defaults
	// to 5 s when zero — same default as toolbox/client.Client.
	DialTimeout time.Duration
}

// NewToolboxNetworker is the zero-argument constructor; defaults are
// fine for production use, callers override DialTimeout for tests
// or unusually slow vsock.
func NewToolboxNetworker() *ToolboxNetworker {
	return &ToolboxNetworker{}
}

// SetNetwork applies the requested IP/Gateway/MAC/Interface to the
// guest at udsPath. Errors come back as the toolbox client wraps
// them — the pool turns any error into a stopped-state lease so the
// caller doesn't get a half-configured VM.
func (n *ToolboxNetworker) SetNetwork(ctx context.Context, udsPath, ip, gateway, mac, iface string) error {
	c := &client.Client{UDSPath: udsPath, DialTimeout: n.DialTimeout}
	_, err := c.SetNetwork(ctx, agent.SetNetworkRequest{
		Interface: iface,
		IP:        ip,
		Gateway:   gateway,
		MAC:       mac,
	})
	return err
}
