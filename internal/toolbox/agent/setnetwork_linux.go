//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// handleSetNetwork applies a host-supplied IP/MAC/gateway to the
// guest's primary interface. Used after warm restore to re-IP a
// snapshot-restored VM whose serialized network config is stale.
//
// Sequence (PLAN_SANDBOXD §3 step 2):
//   1. LinkByName → resolve the interface
//   2. LinkSetDown — required before changing MAC
//   3. LinkSetHardwareAddr (optional, only if MAC present)
//   4. LinkSetUp
//   5. AddrReplace — atomic swap, doesn't error if the same addr
//      is already configured
//   6. RouteReplace default via gateway (optional)
//   7. arping -U (gratuitous) — best-effort, refreshes the host
//      bridge's FDB so it forwards to the new MAC immediately
//
// Fail-close: any error in steps 1-6 returns HTTP 5xx so the host
// control plane can tear down the VM and retry with a different
// warm slot. arping failure is logged but not surfaced — the
// network is functional, just the bridge may take a few seconds
// to learn the new MAC via normal traffic.
//
// This endpoint is intentionally on /internal/* — sandboxd-side
// trusted callers only. It performs no validation that the
// supplied IP belongs to any pool; that is the host's job.
func handleSetNetwork(w http.ResponseWriter, r *http.Request) {
	var req SetNetworkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode setnetwork: %w", err))
		return
	}
	if req.IP == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("setnetwork: ip is required"))
		return
	}
	addr, err := netlink.ParseAddr(req.IP)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("setnetwork: parse ip %q: %w", req.IP, err))
		return
	}

	iface := req.Interface
	if iface == "" {
		iface = "eth0"
	}
	link, err := netlink.LinkByName(iface)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("setnetwork: lookup %s: %w", iface, err))
		return
	}

	if req.MAC != "" {
		mac, err := net.ParseMAC(req.MAC)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("setnetwork: parse mac %q: %w", req.MAC, err))
			return
		}
		if err := netlink.LinkSetDown(link); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("setnetwork: link down: %w", err))
			return
		}
		if err := netlink.LinkSetHardwareAddr(link, mac); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("setnetwork: set mac: %w", err))
			return
		}
	}

	if err := netlink.LinkSetUp(link); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("setnetwork: link up: %w", err))
		return
	}
	// Flush IPv4 addresses before adding the new one so back-to-back
	// SetNetwork calls don't accumulate stale IPs (the snapshot-restore
	// case is the canonical example: every restore brings back the
	// original IP + we layer the new lease on top, polluting routing).
	// Family 0 = ALL, but we only flush v4 here — IPv6 link-local is
	// kernel-managed and will be re-derived from the MAC anyway.
	if existing, err := netlink.AddrList(link, unix.AF_INET); err == nil {
		for _, a := range existing {
			_ = netlink.AddrDel(link, &a)
		}
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("setnetwork: addr add: %w", err))
		return
	}

	if req.Gateway != "" {
		gw := net.ParseIP(req.Gateway)
		if gw == nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("setnetwork: parse gateway %q", req.Gateway))
			return
		}
		route := &netlink.Route{
			LinkIndex: link.Attrs().Index,
			Gw:        gw,
		}
		if err := netlink.RouteReplace(route); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("setnetwork: route replace: %w", err))
			return
		}
	}

	// Gratuitous ARP — best-effort, refreshes the host bridge's FDB.
	// Uses the system arping if installed (busybox / iputils-arping
	// in most base images). On miss, log only; the bridge will learn
	// the new MAC via the next outbound packet anyway.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		arpingPath, err := exec.LookPath("arping")
		if err != nil {
			return
		}
		// addr.IP includes the host portion; arping wants bare IP.
		_ = exec.CommandContext(ctx, arpingPath, "-U", "-c", "2", "-I", iface, addr.IP.String()).Run()
	}()

	writeJSON(w, http.StatusOK, SetNetworkResponse{
		OK:        true,
		Interface: iface,
		IP:        req.IP,
		Gateway:   req.Gateway,
		MAC:       req.MAC,
	})
}
