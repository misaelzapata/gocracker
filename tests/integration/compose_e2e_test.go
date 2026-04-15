//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	internalapi "github.com/gocracker/gocracker/internal/api"
	"github.com/gocracker/gocracker/internal/compose"
)

// TestE2ECompose drives a multi-service docker-compose stack through the real
// `gocracker compose` CLI against a live `gocracker serve` subprocess. There
// is no REST endpoint for compose up/down (see internal/api/api.go — only
// /run, /vms, /vms/{id}/*, /migrations/* are mounted), so the CLI path is the
// only end-to-end shape available.
//
// What this covers that nothing else does today:
//   - Parsing the real compose YAML dialect (services.*.image).
//   - Sharing a state directory and overlay cache across TWO concurrently
//     booted VMs (nginx:alpine + curlimages/curl).
//   - Per-VM exec over the API after compose attaches the stack metadata.
//
// What it does NOT cover (on purpose — out of scope for this agent):
//   - Cross-service DNS resolution (compose does not wire an internal
//     resolver today; see comment below near the DNS assertion).
//   - Port publish / iptables plumbing (Agent C owns net/limiter/balloon).
func TestE2ECompose(t *testing.T) {
	if os.Getenv("E2E") != "1" {
		t.Skip("set E2E=1 to enable")
	}
	requirePrivilegedExecIntegration(t)
	kernel := resolveE2EKernel(t)
	bins := buildProjectBinaries(t)

	cacheDir := filepath.Join(t.TempDir(), "cache")
	stateDir := filepath.Join(t.TempDir(), "state")
	snapDir := filepath.Join(t.TempDir(), "snap")
	for _, d := range []string{cacheDir, stateDir, snapDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	addr := freeLocalAddr(t)
	serverURL := "http://" + addr
	serveCmd, serveLog := startE2EServe(t, bins, addr, cacheDir, stateDir, snapDir, kernel)
	t.Cleanup(func() { stopCommand(t, serveCmd) })
	waitForAPI(t, serverURL, 45*time.Second)

	fixtureDir := t.TempDir()
	composeFile := filepath.Join(fixtureDir, "docker-compose.yml")
	// Two services: nginx:alpine as a lightweight webserver and
	// curlimages/curl as a client. The client sleeps forever so we can exec
	// against it rather than racing its natural exit. exec_enabled: true on
	// both so POST /vms/{id}/exec reaches an agent.
	composeYAML := "" +
		"services:\n" +
		"  web:\n" +
		"    image: nginx:alpine\n" +
		"    command: [\"/bin/sh\", \"-lc\", \"sleep infinity\"]\n" +
		"    x-gocracker:\n" +
		"      exec_enabled: true\n" +
		"      mem_mb: 256\n" +
		"      disk_size_mb: 512\n" +
		"  client:\n" +
		"    image: curlimages/curl:latest\n" +
		"    command: [\"/bin/sh\", \"-lc\", \"sleep infinity\"]\n" +
		"    x-gocracker:\n" +
		"      exec_enabled: true\n" +
		"      mem_mb: 256\n" +
		"      disk_size_mb: 512\n"
	if err := os.WriteFile(composeFile, []byte(composeYAML), 0644); err != nil {
		t.Fatalf("write compose file: %v", err)
	}

	// `gocracker compose` pushes each service through the API server. It
	// blocks until every service has reported healthy/running, so by the
	// time CombinedOutput returns both VMs should be visible under /vms.
	upCtx, upCancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer upCancel()
	up := exec.CommandContext(upCtx, bins.gocracker,
		"compose",
		"--server", serverURL,
		"--file", composeFile,
		"--kernel", kernel,
		"--cache-dir", cacheDir,
		"--jailer", "off",
	)
	up.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	upOut, err := up.CombinedOutput()
	if err != nil {
		t.Fatalf("compose up failed: %v\noutput:\n%s\nserve log:\n%s", err, upOut, serveLog.String())
	}
	t.Cleanup(func() {
		downCtx, downCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer downCancel()
		down := exec.CommandContext(downCtx, bins.gocracker,
			"compose", "down",
			"--server", serverURL,
			"--file", composeFile,
		)
		if out, err := down.CombinedOutput(); err != nil {
			t.Logf("compose down: %v\n%s", err, out)
		}
	})

	client := internalapi.NewClient(serverURL)
	stackName := compose.StackNameForComposePath(composeFile)

	// Both services should have registered VMs tagged with our stack name.
	webVM := waitForComposeService(t, client, stackName, "web", 90*time.Second)
	clientVM := waitForComposeService(t, client, stackName, "client", 90*time.Second)
	if webVM.ID == clientVM.ID {
		t.Fatalf("web and client resolved to the same VM id %q", webVM.ID)
	}

	// List /vms and make sure BOTH are present. This is the property the
	// original bug description was hunting for.
	listCtx, listCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer listCancel()
	vms, err := client.ListVMs(listCtx, map[string]string{
		"orchestrator": "compose",
		"stack":        stackName,
	})
	if err != nil {
		t.Fatalf("list stack vms: %v", err)
	}
	if len(vms) < 2 {
		t.Fatalf("expected >=2 VMs for stack %s, got %d: %+v", stackName, len(vms), vms)
	}

	// Exec a simple command on each VM to prove it booted an agent.
	for _, vm := range []internalapi.VMInfo{webVM, clientVM} {
		service := vm.Metadata["service"]
		resp := waitForExecResponse(t, client, vm.ID, internalapi.ExecRequest{
			Command: []string{"/bin/sh", "-lc", "hostname; echo e2e-compose-ok"},
		}, 45*time.Second)
		if resp.ExitCode != 0 {
			t.Fatalf("[%s] exec exit=%d stderr=%q", service, resp.ExitCode, resp.Stderr)
		}
		if !strings.Contains(resp.Stdout, "e2e-compose-ok") {
			t.Fatalf("[%s] exec stdout=%q missing sentinel", service, resp.Stdout)
		}
		if strings.TrimSpace(resp.Stdout) == "" {
			t.Fatalf("[%s] exec stdout empty", service)
		}
	}

	// Cross-service DNS is deliberately NOT asserted: gocracker compose
	// wires service-to-service traffic via static IPs published through the
	// stack network, but it does not currently run an in-guest resolver.
	// Skipping with a comment per task spec.
	t.Log("compose: internal DNS assertion skipped (no in-stack resolver in gocracker compose today)")

	// Stop each VM explicitly via API. compose down in t.Cleanup handles
	// teardown regardless, but hitting /stop first verifies the happy path
	// and leaves the stack network in a well-known state for the cleanup.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCancel()
	for _, vm := range []internalapi.VMInfo{webVM, clientVM} {
		if err := client.StopVM(stopCtx, vm.ID); err != nil {
			// Stop may race with compose down; just log.
			t.Logf("stop vm %s: %v", vm.ID, err)
		}
	}
}

// waitForComposeService polls /vms filtered by compose metadata until the
// named service shows up with an ID. Returns the VMInfo.
func waitForComposeService(t *testing.T, client *internalapi.Client, stackName, service string, timeout time.Duration) internalapi.VMInfo {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		vms, err := client.ListVMs(ctx, map[string]string{
			"orchestrator": "compose",
			"stack":        stackName,
			"service":      service,
		})
		if err == nil && len(vms) > 0 {
			vm := vms[0]
			if vm.State == "running" || vm.State == "ready" || vm.State == "paused" {
				return vm
			}
			lastErr = fmt.Errorf("state=%s", vm.State)
		} else if err != nil {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			t.Fatalf("stack=%s service=%s did not become ready in %s (last: %v)",
				stackName, service, timeout, lastErr)
		case <-ticker.C:
		}
	}
}
