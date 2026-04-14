//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	internalapi "github.com/gocracker/gocracker/internal/api"
	"github.com/gocracker/gocracker/pkg/vmm"
)

// TestE2ERateLimiterBlock verifies that a runtime-applied block rate limiter
// actually throttles guest disk throughput. We set a 1 MB/s bandwidth budget
// on the root virtio-blk device (that is, the rootfs disk hosting /tmp inside
// Alpine) and then `dd` 20 MiB from /dev/zero to /tmp/big with fsync. At
// 1 MB/s the transfer SHOULD take ~20 s; we accept >= 18 s as "throttling
// works" and fail loudly below 10 s (which would indicate the limiter is a
// no-op or is being bypassed by the page cache).
//
// Note: the POST /run request does not currently expose a rate_limiter field
// for the root disk — the Drive struct has one, but Drive.IsRootDevice=true is
// rejected. The documented path is to PUT the limiter at runtime via
// /vms/{id}/rate-limiters/block, which is what this test does.
func TestE2ERateLimiterBlock(t *testing.T) {
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
	runCtx, runCancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer runCancel()
	runResp, err := client.Run(runCtx, internalapi.RunRequest{
		Image:       "alpine:3.20",
		KernelPath:  kernel,
		MemMB:       256,
		DiskSizeMB:  512,
		Cmd:         []string{"/bin/sh", "-lc", "sleep infinity"},
		ExecEnabled: true,
	})
	if err != nil {
		t.Fatalf("/run: %v\nserve log:\n%s", err, serveLog.String())
	}
	t.Cleanup(func() { _ = client.StopVM(context.Background(), runResp.ID) })

	if _, err := waitForVMStateViaClient(t, client, runResp.ID, "running", 90*time.Second); err != nil {
		t.Fatalf("VM never reached running: %v\nserve log:\n%s", err, serveLog.String())
	}

	// Sanity: exec should work before we start applying limiters.
	probe := waitForExecResponse(t, client, runResp.ID, internalapi.ExecRequest{
		Command: []string{"/bin/sh", "-lc", "echo rl-probe-ok"},
	}, 60*time.Second)
	if !strings.Contains(probe.Stdout, "rl-probe-ok") {
		t.Fatalf("probe exec stdout=%q stderr=%q exit=%d", probe.Stdout, probe.Stderr, probe.ExitCode)
	}

	// Apply a 1 MB/s bandwidth limit to the root virtio-blk device.
	//   Size=1_048_576, RefillTimeMs=1000  =>  1 MiB / second
	limiter := vmm.RateLimiterConfig{
		Bandwidth: vmm.TokenBucketConfig{
			Size:         1 << 20,
			RefillTimeMs: 1000,
		},
	}
	if err := putRateLimiter(serverURL, "block", runResp.ID, limiter); err != nil {
		t.Fatalf("PUT block rate-limiter: %v\nserve log:\n%s", err, serveLog.String())
	}

	// dd 20 MiB with O_SYNC equivalent (conv=fsync) so the host disk really
	// takes the hit instead of the write being absorbed by the guest page
	// cache. We wrap in `time` to capture guest-side wall-clock as a sanity
	// check but rely on host-side elapsed for the assertion.
	const wantBytes = 20
	ddCmd := fmt.Sprintf("time dd if=/dev/zero of=/tmp/big bs=1M count=%d conv=fsync 2>&1", wantBytes)
	start := time.Now()
	ddResp := waitForExecResponse(t, client, runResp.ID, internalapi.ExecRequest{
		Command: []string{"/bin/sh", "-lc", ddCmd},
	}, 90*time.Second)
	elapsed := time.Since(start)
	t.Logf("dd 20 MiB elapsed=%s\nstdout=%q\nstderr=%q\nexit=%d",
		elapsed, ddResp.Stdout, ddResp.Stderr, ddResp.ExitCode)
	if ddResp.ExitCode != 0 {
		t.Fatalf("dd failed: exit=%d stdout=%q stderr=%q", ddResp.ExitCode, ddResp.Stdout, ddResp.Stderr)
	}

	// Assertion boundaries:
	//   - < 10 s  => limiter is a no-op; fail loudly.
	//   - < 18 s  => throttling happening but under budget; warn.
	//   - >= 18 s => pass.
	if elapsed < 10*time.Second {
		t.Fatalf("dd finished in %s — the 1 MB/s block rate-limiter is not throttling (expected >= 18s at 1 MB/s for %d MiB)",
			elapsed, wantBytes)
	}
	if elapsed < 18*time.Second {
		t.Fatalf("dd elapsed=%s is under the 18s floor for %d MiB @ 1 MB/s; limiter appears leaky",
			elapsed, wantBytes)
	}

	// Lift the limiter by PUTting an all-zeroes config; follow-up write should
	// finish quickly and prove the runtime update path also clears correctly.
	zero := vmm.RateLimiterConfig{}
	if err := putRateLimiter(serverURL, "block", runResp.ID, zero); err != nil {
		t.Fatalf("PUT block rate-limiter (clear): %v", err)
	}
	clearStart := time.Now()
	clearResp := waitForExecResponse(t, client, runResp.ID, internalapi.ExecRequest{
		Command: []string{"/bin/sh", "-lc", "dd if=/dev/zero of=/tmp/big2 bs=1M count=10 conv=fsync 2>&1"},
	}, 90*time.Second)
	clearElapsed := time.Since(clearStart)
	t.Logf("dd 10 MiB (post-clear) elapsed=%s", clearElapsed)
	if clearResp.ExitCode != 0 {
		t.Fatalf("post-clear dd failed: stdout=%q stderr=%q", clearResp.Stdout, clearResp.Stderr)
	}
	// Can't assert an upper bound reliably on slow hosts; we just verify the
	// call succeeds, which is enough to cover the "limiter removal" code path.
}

// TestE2ERateLimiterNet is intentionally gated behind the network test: the
// only way to exercise a bandwidth limit on virtio-net from the HTTP e2e layer
// is to drive traffic between guest and host, which already needs the tap
// plumbing TestE2ENetworkStaticIP sets up. Duplicating that scaffold here
// would double the test runtime for marginal coverage — the block path already
// exercises the shared limiter machinery. We document the gap and skip.
func TestE2ERateLimiterNet(t *testing.T) {
	if os.Getenv("E2E") != "1" {
		t.Skip("set E2E=1 to enable")
	}
	t.Skipf("net rate-limiter e2e requires host-guest routing; covered in unit/integration tier via vmm.UpdateNetRateLimiter — see pkg/vmm/vmm_test.go")
}

// putRateLimiter is a helper that hits /vms/{id}/rate-limiters/{kind}. The
// existing Client does not expose it yet; we bypass with a bare http.Post so
// we don't pile on client surface area just for this test.
func putRateLimiter(serverURL, kind, id string, cfg vmm.RateLimiterConfig) error {
	body, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	url := serverURL + "/vms/" + id + "/rate-limiters/" + kind
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := strings.TrimSpace(os.Getenv("GOCRACKER_API_TOKEN")); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("PUT %s: %s: %s", url, resp.Status, strings.TrimSpace(string(raw)))
	}
	return nil
}
