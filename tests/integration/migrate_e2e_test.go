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
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	internalapi "github.com/gocracker/gocracker/internal/api"
)

// TestE2EMigrate exercises the full live-migration pipeline across TWO
// `gocracker serve` subprocesses wired together over the loopback API. It is
// intentionally separate from TestLiveMigrationStopAndCopy (which runs the API
// server in-process via httptest): this variant catches regressions that only
// surface when the source and destination live in distinct processes (auth
// token plumbing, trusted-dir enforcement, HTTP framing on the multipart
// migration payload, state-dir isolation, etc.).
//
// Gated on E2E=1 and root (the default jailer path requires privileges even
// with --jailer off, because cacheDir creation under /tmp and the VMM worker
// re-exec need a consistent uid/gid footprint).
func TestE2EMigrate(t *testing.T) {
	if os.Getenv("E2E") != "1" {
		t.Skip("set E2E=1 to enable")
	}
	requirePrivilegedExecIntegration(t)
	requireKVMClockCtrl(t)
	kernel := resolveE2EKernel(t)
	bins := buildProjectBinaries(t)

	srcAddr := freeLocalAddr(t)
	dstAddr := freeLocalAddr(t)
	srcURL := "http://" + srcAddr
	dstURL := "http://" + dstAddr

	srcCache := filepath.Join(t.TempDir(), "cache-src")
	dstCache := filepath.Join(t.TempDir(), "cache-dst")
	srcState := filepath.Join(t.TempDir(), "state-src")
	dstState := filepath.Join(t.TempDir(), "state-dst")
	srcSnap := filepath.Join(t.TempDir(), "snap-src")
	dstSnap := filepath.Join(t.TempDir(), "snap-dst")
	for _, d := range []string{srcCache, dstCache, srcState, dstState, srcSnap, dstSnap} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	srcCmd, srcLog := startE2EServe(t, bins, srcAddr, srcCache, srcState, srcSnap, kernel)
	t.Cleanup(func() { stopCommand(t, srcCmd) })
	dstCmd, dstLog := startE2EServe(t, bins, dstAddr, dstCache, dstState, dstSnap, kernel)
	t.Cleanup(func() { stopCommand(t, dstCmd) })

	waitForAPI(t, srcURL, 45*time.Second)
	waitForAPI(t, dstURL, 45*time.Second)

	// Use node:20-alpine as the task suggests — a realistic image big enough
	// to exercise the migration bundle (disk delta, memory dirty pages).
	src := internalapi.NewClient(srcURL)
	dst := internalapi.NewClient(dstURL)
	runCtx, runCancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer runCancel()

	runResp, err := src.Run(runCtx, internalapi.RunRequest{
		Image:       "node:20-alpine",
		KernelPath:  kernel,
		MemMB:       256,
		DiskSizeMB:  512,
		Cmd:         []string{"/bin/sh", "-lc", "sleep infinity"},
		ExecEnabled: true,
	})
	if err != nil {
		t.Fatalf("src /run: %v\nsrc log:\n%s", err, srcLog.String())
	}
	if runResp.ID == "" {
		t.Fatalf("empty VM id; src log:\n%s", srcLog.String())
	}
	defer func() {
		// Best-effort: stop at whichever server currently owns the VM.
		_ = dst.StopVM(context.Background(), runResp.ID)
		_ = src.StopVM(context.Background(), runResp.ID)
	}()

	if _, err := waitForVMStateViaClient(t, src, runResp.ID, "running", 90*time.Second); err != nil {
		t.Fatalf("wait for source running: %v\nsrc log:\n%s", err, srcLog.String())
	}

	// Kick off the migration. Pointing at dst's URL triggers the built-in
	// stop-and-copy path (prepare bundle → POST bundle → finalize on dest).
	migrateReq := internalapi.MigrateRequest{DestinationURL: dstURL}
	body, _ := json.Marshal(migrateReq)
	migrateURL := srcURL + "/vms/" + runResp.ID + "/migrate"
	migResp, err := http.Post(migrateURL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v\nsrc log:\n%s", migrateURL, err, srcLog.String())
	}
	defer migResp.Body.Close()
	if migResp.StatusCode >= 400 {
		raw, _ := io.ReadAll(migResp.Body)
		t.Fatalf("migrate returned %s: %s\nsrc log:\n%s\ndst log:\n%s", migResp.Status, raw, srcLog.String(), dstLog.String())
	}
	var migration internalapi.MigrationResponse
	if err := json.NewDecoder(migResp.Body).Decode(&migration); err != nil {
		t.Fatalf("decode migrate response: %v", err)
	}
	if migration.TargetID != runResp.ID {
		t.Fatalf("target id = %q, want %q", migration.TargetID, runResp.ID)
	}

	if err := waitForVMGone(t, src, runResp.ID, 15*time.Second); err != nil {
		t.Fatalf("source still has VM after migrate: %v\nsrc log:\n%s", err, srcLog.String())
	}
	info, err := waitForVMStateViaClient(t, dst, runResp.ID, "running", 30*time.Second)
	if err != nil {
		t.Fatalf("dst never reached running: %v\ndst log:\n%s", err, dstLog.String())
	}
	if info.State != "running" {
		t.Fatalf("dst state = %s, want running", info.State)
	}

	// Exec on the migrated VM — proves the guest is alive, the exec agent
	// survived snapshot+restore, and the network/disk stack is wired at dst.
	// The exec agent talks to the API server over vsock, and its socket
	// needs a moment to re-establish after restore. Give it a settle
	// window before the first exec attempt so the waitForExecResponse
	// retry loop does not exhaust its budget on vsock handshakes.
	time.Sleep(2 * time.Second)
	execResp := waitForExecResponse(t, dst, runResp.ID, internalapi.ExecRequest{
		Command: []string{"/bin/sh", "-lc", "echo e2e-migrate-ok && uname -s"},
	}, 90*time.Second)
	if !strings.Contains(execResp.Stdout, "e2e-migrate-ok") {
		t.Fatalf("migrated exec stdout = %q, want e2e-migrate-ok\nstderr: %q\nexit=%d",
			execResp.Stdout, execResp.Stderr, execResp.ExitCode)
	}
	if execResp.ExitCode != 0 {
		t.Fatalf("migrated exec exit = %d, stderr = %q", execResp.ExitCode, execResp.Stderr)
	}

	if err := dst.StopVM(context.Background(), runResp.ID); err != nil {
		t.Fatalf("stop migrated vm: %v", err)
	}
}

// resolveE2EKernel returns the absolute path to the minimal guest kernel. We
// do NOT fall back to the integration kernel here because the migrate/compose
// flows exercise real Docker Hub pulls and need the same kernel across
// servers; hard-coding the minimal artifact keeps the two agents synced.
func resolveE2EKernel(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("KVM not available: %v", err)
	}
	// Honor GOCRACKER_KERNEL when the caller sets it (CI/override); otherwise
	// use the canonical minimal kernel under artifacts/.
	if k := os.Getenv("GOCRACKER_KERNEL"); k != "" {
		resolved, err := resolveIntegrationPath(repoRoot(t), k)
		if err != nil {
			t.Fatalf("resolve GOCRACKER_KERNEL: %v", err)
		}
		return resolved
	}
	path := filepath.Join(repoRoot(t), "artifacts", "kernels", "gocracker-guest-minimal-vmlinux")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("minimal kernel missing at %s: %v", path, err)
	}
	return path
}

// startE2EServe spawns `gocracker serve` as a subprocess with jailer off and
// returns the cmd handle plus a live log buffer. Caller is responsible for
// stopping the command in t.Cleanup.
func startE2EServe(t *testing.T, bins builtBinaries, addr, cacheDir, stateDir, snapDir, kernel string) (*exec.Cmd, *lockedBuffer) {
	t.Helper()
	args := []string{
		"serve",
		"--addr", addr,
		"--jailer", "off",
		"--cache-dir", cacheDir,
		"--state-dir", stateDir,
		"--vmm-binary", bins.vmm,
		"--trusted-kernel-dir", filepath.Dir(kernel),
		"--trusted-snapshot-dir", snapDir,
	}
	cmd := exec.Command(bins.gocracker, args...)
	var log lockedBuffer
	cmd.Stdout = &log
	cmd.Stderr = &log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start gocracker serve @ %s: %v", addr, err)
	}
	return cmd, &log
}

// waitForVMStateViaClient polls /vms/{id} until the VM is in the desired state.
// Named distinctly from the existing waitForVMState (which operates on a
// vmm.Handle) to avoid symbol collisions within the integration package.
func waitForVMStateViaClient(t *testing.T, client *internalapi.Client, id, want string, timeout time.Duration) (internalapi.VMInfo, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var last internalapi.VMInfo
	for {
		info, err := client.GetVM(ctx, id)
		if err == nil {
			last = info
			if info.State == want {
				return info, nil
			}
		}
		select {
		case <-ctx.Done():
			return last, fmt.Errorf("vm %s state=%q after %s (want %q)", id, last.State, timeout, want)
		case <-ticker.C:
		}
	}
}

// waitForVMGone polls /vms/{id} until the GET returns a 404 (the migration
// source clears the VM from its table once finalize succeeds on dst).
func waitForVMGone(t *testing.T, client *internalapi.Client, id string, timeout time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := client.GetVM(ctx, id)
		if err != nil {
			// API client returns an error on 404 — good enough signal.
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("vm %s still present after %s", id, timeout)
		case <-ticker.C:
		}
	}
}
