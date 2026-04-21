package client

import (
	"context"
	"strings"
	"testing"

	"github.com/gocracker/gocracker/internal/toolbox/agent"
)

// SetNetwork client tests cover wire-shape (the agent's request/
// response is JSON, no framing) and error decoding. The actual
// netlink path is exercised in the live VM smoke; here we only
// verify the host-side codec round-trips correctly against the
// real agent handler — including the error path where the handler
// rejects bad input with HTTP 400 + structured JSON.

func TestSetNetwork_RejectsEmptyIP_ViaClient(t *testing.T) {
	c := startAgentWithUDSBridge(t)
	_, err := c.SetNetwork(context.Background(), agent.SetNetworkRequest{Interface: "eth0"})
	if err == nil {
		t.Fatal("expected error from empty IP, got nil")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Fatalf("error should mention 400 status: %v", err)
	}
	if !strings.Contains(err.Error(), "ip is required") {
		t.Fatalf("error should preserve agent message 'ip is required': %v", err)
	}
}

func TestSetNetwork_OnInvalidIPFormat(t *testing.T) {
	c := startAgentWithUDSBridge(t)
	_, err := c.SetNetwork(context.Background(), agent.SetNetworkRequest{IP: "garbage"})
	if err == nil {
		t.Fatal("expected error from bad IP, got nil")
	}
	// Linux returns 400, non-Linux stub returns 501. Either is fine.
	if !strings.Contains(err.Error(), "400") && !strings.Contains(err.Error(), "501") {
		t.Fatalf("error should mention 400 or 501: %v", err)
	}
}

// SetNetwork_HappyPath against an httptest server — the netlink calls
// fail because httptest doesn't actually have eth0, but we verify the
// CLIENT'S request shape: it sends a well-formed POST that the agent
// successfully decodes (we look for a netlink-specific error which
// proves we got past the JSON decode + IP parse stages).
func TestSetNetwork_RequestReachesNetlink(t *testing.T) {
	c := startAgentWithUDSBridge(t)
	_, err := c.SetNetwork(context.Background(), agent.SetNetworkRequest{
		Interface: "definitely-not-an-iface-9999",
		IP:        "10.99.0.2/30",
		Gateway:   "10.99.0.1",
	})
	if err == nil {
		t.Fatal("expected error for nonexistent interface, got nil")
	}
	// We expect 500 (lookup failed) on Linux test runners,
	// or 501 on non-Linux. Either proves the request shape is valid
	// and reached the netlink call site.
	if !strings.Contains(err.Error(), "500") && !strings.Contains(err.Error(), "501") {
		t.Fatalf("error should be 500 (netlink lookup) or 501 (non-linux stub): %v", err)
	}
}
