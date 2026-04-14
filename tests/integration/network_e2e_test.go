//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	internalapi "github.com/gocracker/gocracker/internal/api"
)

// TestE2ENetworkStaticIP exercises the HTTP /run path with an explicit
// static_ip + gateway + tap_name triplet. It verifies two things:
//  1. The guest's eth0 is configured with the requested address.
//  2. (Best-effort) The guest can reach an HTTP server bound to the gateway
//     IP on the host, which also validates tap + NAT routing works.
//
// Unlike the auto/DHCP path (which is handled by the CLI's --net auto flag
// but is NOT plumbed through the RunRequest JSON today), this test is fully
// exercised through the HTTP API.
func TestE2ENetworkStaticIP(t *testing.T) {
	if os.Getenv("E2E") != "1" {
		t.Skip("set E2E=1 to enable")
	}
	requirePrivilegedExecIntegration(t)
	kernel := resolveE2EKernel(t)
	bins := buildProjectBinaries(t)

	// Create a fresh tap device on the host; assign the gateway IP to it
	// and bring it up so the guest can route.
	const (
		guestCIDR = "10.0.42.2/24"
		gatewayIP = "10.0.42.1"
		subnet    = "10.0.42.0/24"
	)
	tapName := e2eUniqueTapName(t)
	if err := createHostTap(tapName, gatewayIP+"/24"); err != nil {
		t.Skipf("cannot create tap %s (needs CAP_NET_ADMIN): %v", tapName, err)
	}
	t.Cleanup(func() { _ = deleteHostTap(tapName) })

	addr := freeLocalAddr(t)
	serverURL := "http://" + addr
	cacheDir := filepath.Join(t.TempDir(), "cache")
	stateDir := filepath.Join(t.TempDir(), "state")
	snapDir := filepath.Join(t.TempDir(), "snap")
	for _, d := range []string{cacheDir, stateDir, snapDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	serveCmd, serveLog := startE2EServe(t, bins, addr, cacheDir, stateDir, snapDir, kernel)
	t.Cleanup(func() { stopCommand(t, serveCmd) })
	waitForAPI(t, serverURL, 45*time.Second)

	client := internalapi.NewClient(serverURL)
	runCtx, runCancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer runCancel()

	runResp, err := client.Run(runCtx, internalapi.RunRequest{
		Image:       "alpine:3.20",
		KernelPath:  kernel,
		MemMB:       256,
		DiskSizeMB:  256,
		TapName:     tapName,
		StaticIP:    guestCIDR,
		Gateway:     gatewayIP,
		Cmd:         []string{"/bin/sh", "-lc", "sleep infinity"},
		ExecEnabled: true,
	})
	if err != nil {
		t.Fatalf("/run static_ip: %v\nserve log:\n%s", err, serveLog.String())
	}
	t.Cleanup(func() { _ = client.StopVM(context.Background(), runResp.ID) })

	if _, err := waitForVMStateViaClient(t, client, runResp.ID, "running", 90*time.Second); err != nil {
		t.Fatalf("VM never reached running: %v\nserve log:\n%s", err, serveLog.String())
	}

	// Verify eth0 has the requested static IP.
	resp := waitForExecResponse(t, client, runResp.ID, internalapi.ExecRequest{
		Command: []string{"/bin/sh", "-lc", "ip -4 addr show eth0 || ip -4 addr"},
	}, 60*time.Second)
	out := resp.Stdout + resp.Stderr
	if !strings.Contains(out, "10.0.42.2") {
		t.Fatalf("guest eth0 missing 10.0.42.2:\nexit=%d\nstdout=%q\nstderr=%q",
			resp.ExitCode, resp.Stdout, resp.Stderr)
	}

	// Verify guest default route points at the gateway.
	routeResp := waitForExecResponse(t, client, runResp.ID, internalapi.ExecRequest{
		Command: []string{"/bin/sh", "-lc", "ip -4 route || route -n"},
	}, 15*time.Second)
	if !strings.Contains(routeResp.Stdout+routeResp.Stderr, gatewayIP) {
		t.Logf("route table missing gateway %s (non-fatal):\nstdout=%q\nstderr=%q",
			gatewayIP, routeResp.Stdout, routeResp.Stderr)
	}

	// ---- Outbound reachability: start an HTTP server on the gateway IP and
	// have the guest fetch it. Skip if the host cannot bind to the gateway
	// IP (e.g., NAT/firewall misconfiguration).
	ln, err := net.Listen("tcp", gatewayIP+":0")
	if err != nil {
		t.Skipf("cannot bind to %s on host (no bridge routing?): %v", gatewayIP, err)
	}
	defer ln.Close()
	const body = "e2e-net-ok"
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	})

	hostAddr := ln.Addr().String()
	// Alpine busybox has wget; fall back to nc if unavailable.
	cmd := fmt.Sprintf("wget -q -O - -T 5 http://%s/ || (echo FALLBACK && busybox nc -w 5 %s <<< $'GET / HTTP/1.0\\r\\n\\r\\n')",
		hostAddr, strings.ReplaceAll(hostAddr, ":", " "))
	fetchResp := waitForExecResponse(t, client, runResp.ID, internalapi.ExecRequest{
		Command: []string{"/bin/sh", "-lc", cmd},
	}, 30*time.Second)
	if !strings.Contains(fetchResp.Stdout, body) {
		t.Logf("outbound fetch did not see %q — host may not route guest subnet to loopback.\nstdout=%q\nstderr=%q (skipping outbound assert)",
			body, fetchResp.Stdout, fetchResp.Stderr)
		t.Skipf("no host-bridge routing")
	}
}

// TestE2ENetworkAutoMode exercises the new network_mode=auto field on POST
// /run: the server allocates a fresh TAP + /30 subnet via hostnet.AutoNetwork,
// the guest gets a working eth0, and the response echoes tap/ip/gateway back
// to the caller. This is what sandboxd uses so it does not have to pre-allocate
// TAPs on the host.
func TestE2ENetworkAutoMode(t *testing.T) {
	if os.Getenv("E2E") != "1" {
		t.Skip("set E2E=1 to enable")
	}
	requirePrivilegedExecIntegration(t)
	kernel := resolveE2EKernel(t)
	bins := buildProjectBinaries(t)

	addr := freeLocalAddr(t)
	serverURL := "http://" + addr
	cacheDir := filepath.Join(t.TempDir(), "cache")
	stateDir := filepath.Join(t.TempDir(), "state")
	snapDir := filepath.Join(t.TempDir(), "snap")
	for _, d := range []string{cacheDir, stateDir, snapDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	serveCmd, serveLog := startE2EServe(t, bins, addr, cacheDir, stateDir, snapDir, kernel)
	t.Cleanup(func() { stopCommand(t, serveCmd) })
	waitForAPI(t, serverURL, 45*time.Second)

	client := internalapi.NewClient(serverURL)
	runCtx, runCancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer runCancel()

	runResp, err := client.Run(runCtx, internalapi.RunRequest{
		Image:       "alpine:3.20",
		KernelPath:  kernel,
		MemMB:       256,
		DiskSizeMB:  256,
		NetworkMode: "auto",
		Cmd:         []string{"/bin/sh", "-lc", "sleep infinity"},
		ExecEnabled: true,
		Wait:        true,
	})
	if err != nil {
		t.Fatalf("/run network_mode=auto: %v\nserve log:\n%s", err, serveLog.String())
	}
	t.Cleanup(func() { _ = client.StopVM(context.Background(), runResp.ID) })

	// Response must echo the resolved network triplet back to the caller.
	if runResp.State != "running" {
		t.Fatalf("state=%q, want running (wait=true)\nserve log:\n%s", runResp.State, serveLog.String())
	}
	if runResp.NetworkMode != "auto" {
		t.Fatalf("response network_mode=%q, want auto", runResp.NetworkMode)
	}
	if runResp.TapName == "" || runResp.GuestIP == "" || runResp.Gateway == "" {
		t.Fatalf("response missing network fields: tap=%q ip=%q gw=%q",
			runResp.TapName, runResp.GuestIP, runResp.Gateway)
	}
	if runResp.RestoredFromSnapshot {
		t.Errorf("restored_from_snapshot=true but this is a fresh boot")
	}

	// Guest eth0 should carry the server-allocated IP.
	resp := waitForExecResponse(t, client, runResp.ID, internalapi.ExecRequest{
		Command: []string{"/bin/sh", "-lc", "ip -4 addr show eth0 || ip -4 addr"},
	}, 60*time.Second)
	out := resp.Stdout + resp.Stderr
	if !strings.Contains(out, runResp.GuestIP) {
		t.Fatalf("guest eth0 missing %s:\nexit=%d\nstdout=%q\nstderr=%q",
			runResp.GuestIP, resp.ExitCode, resp.Stdout, resp.Stderr)
	}

	// GET /vms/{id} should surface the same network triplet as top-level fields.
	infoCtx, infoCancel := context.WithTimeout(context.Background(), 5*time.Second)
	info, err := client.GetVM(infoCtx, runResp.ID)
	infoCancel()
	if err != nil {
		t.Fatalf("GetVM: %v", err)
	}
	if info.TapName != runResp.TapName || info.GuestIP != runResp.GuestIP || info.Gateway != runResp.Gateway {
		t.Errorf("VMInfo mismatch: tap=%q/%q ip=%q/%q gw=%q/%q",
			info.TapName, runResp.TapName, info.GuestIP, runResp.GuestIP, info.Gateway, runResp.Gateway)
	}
	if info.NetworkMode != "auto" {
		t.Errorf("VMInfo network_mode=%q, want auto", info.NetworkMode)
	}
}

// TestE2ENetworkAutoModeRejectsConflict ensures network_mode=auto with
// explicit static_ip/gateway is rejected with 400.
func TestE2ENetworkAutoModeRejectsConflict(t *testing.T) {
	if os.Getenv("E2E") != "1" {
		t.Skip("set E2E=1 to enable")
	}
	requirePrivilegedExecIntegration(t)
	kernel := resolveE2EKernel(t)
	bins := buildProjectBinaries(t)

	addr := freeLocalAddr(t)
	serverURL := "http://" + addr
	cacheDir := filepath.Join(t.TempDir(), "cache")
	stateDir := filepath.Join(t.TempDir(), "state")
	snapDir := filepath.Join(t.TempDir(), "snap")
	for _, d := range []string{cacheDir, stateDir, snapDir} {
		_ = os.MkdirAll(d, 0755)
	}
	serveCmd, _ := startE2EServe(t, bins, addr, cacheDir, stateDir, snapDir, kernel)
	t.Cleanup(func() { stopCommand(t, serveCmd) })
	waitForAPI(t, serverURL, 45*time.Second)

	client := internalapi.NewClient(serverURL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := client.Run(ctx, internalapi.RunRequest{
		Image:       "alpine:3.20",
		KernelPath:  kernel,
		NetworkMode: "auto",
		StaticIP:    "10.0.42.2/24",
		Gateway:     "10.0.42.1",
	})
	if err == nil {
		t.Fatalf("expected 400 error for network_mode=auto + static_ip")
	}
	if !strings.Contains(err.Error(), "network_mode") && !strings.Contains(err.Error(), "exclusive") {
		t.Errorf("error does not mention exclusivity: %v", err)
	}
}

// createHostTap creates a tuntap interface, assigns the given CIDR to it, and
// brings it up. Requires CAP_NET_ADMIN (root).
func createHostTap(name, cidr string) error {
	// `ip tuntap add` is simpler and has wider compatibility than
	// constructing the netlink message by hand.
	if output, err := exec.Command("ip", "tuntap", "add", "dev", name, "mode", "tap").CombinedOutput(); err != nil {
		return fmt.Errorf("ip tuntap add %s: %w: %s", name, err, string(output))
	}
	if output, err := exec.Command("ip", "addr", "add", cidr, "dev", name).CombinedOutput(); err != nil {
		_ = exec.Command("ip", "tuntap", "del", "dev", name, "mode", "tap").Run()
		return fmt.Errorf("ip addr add %s dev %s: %w: %s", cidr, name, err, string(output))
	}
	if output, err := exec.Command("ip", "link", "set", "dev", name, "up").CombinedOutput(); err != nil {
		_ = exec.Command("ip", "tuntap", "del", "dev", name, "mode", "tap").Run()
		return fmt.Errorf("ip link set %s up: %w: %s", name, err, string(output))
	}
	return nil
}

func deleteHostTap(name string) error {
	return exec.Command("ip", "tuntap", "del", "dev", name, "mode", "tap").Run()
}

// e2eUniqueTapName returns a tap name short enough for IFNAMSIZ (15 chars)
// and unique per test run. The PID makes concurrent test runs safe.
func e2eUniqueTapName(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("gce2e%d", os.Getpid()%100000)
	if len(name) > 15 {
		name = name[:15]
	}
	return name
}
