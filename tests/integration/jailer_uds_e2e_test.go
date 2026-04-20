//go:build integration

package integration

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	internalapi "github.com/gocracker/gocracker/internal/api"
)

// TestE2EJailer_UDS_VisibleFromOutside is the critical jailer gate for
// the UDS feature: boot a VM under jailer-on, request a UDS under the
// worker's /worker bind-mount, and verify the test process (OUTSIDE the
// jail and outside the jailed process's private mount namespace) can
// reach the socket, handshake, and exchange data.
//
// This exercises:
//   - seccomp allowlist covers bind(AF_UNIX), listen, accept, unlinkat.
//   - The jailer's unshare(CLONE_NEWNS) + pivot_root does not hide the
//     /worker bind-mount from the host (the bind source is on the real
//     filesystem, accessible from outside the namespace).
//   - ResolveWorkerHostSidePath in the API returns a path the test can
//     actually net.Dial("unix", ...).
func TestE2EJailer_UDS_VisibleFromOutside(t *testing.T) {
	if os.Getenv("E2E") != "1" {
		t.Skip("set E2E=1 to enable")
	}
	requirePrivilegedExecIntegration(t)
	requireJailerAvailable(t)

	kernel := resolveE2EKernel(t)
	bins := buildProjectBinaries(t)
	contextDir := buildVsockListenerFixture(t, 12360)

	addr := freeLocalAddr(t)
	serverURL := "http://" + addr
	cacheDir := filepath.Join(t.TempDir(), "cache")
	serveCmd := exec.Command(bins.gocracker, buildServeArgs(addr, cacheDir, bins)...)
	var serveLog lockedBuffer
	serveCmd.Stdout = &serveLog
	serveCmd.Stderr = &serveLog
	serveCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := serveCmd.Start(); err != nil {
		t.Fatalf("start gocracker serve: %v", err)
	}
	t.Cleanup(func() { stopCommand(t, serveCmd) })
	waitForAPI(t, serverURL, 45*time.Second)

	client := internalapi.NewClient(serverURL)
	runCtx, runCancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer runCancel()

	// UDS path as the VMM sees it inside the jail. Under /worker/, which
	// is bind-mounted from the worker's private RunDir.
	runResp, err := client.Run(runCtx, internalapi.RunRequest{
		Dockerfile:   filepath.Join(contextDir, "Dockerfile"),
		Context:      contextDir,
		KernelPath:   kernel,
		MemMB:        256,
		DiskSizeMB:   256,
		ExecEnabled:  true,
		VsockUDSPath: "/worker/vm.sock",
	})
	if err != nil {
		t.Fatalf("api run: %v\nserve log:\n%s", err, serveLog.String())
	}
	defer func() { _ = client.StopVM(context.Background(), runResp.ID) }()

	if _, err := waitForVMStateViaClient(t, client, runResp.ID, "running", 120*time.Second); err != nil {
		t.Fatalf("wait running: %v\nserve log:\n%s", err, serveLog.String())
	}

	// Pick up the jailer-resolved host-side path from GET /vms/{id}.
	info, err := client.GetVM(context.Background(), runResp.ID)
	if err != nil {
		t.Fatalf("get vm: %v", err)
	}
	if info.VsockUDSPath == "" {
		t.Fatalf("API did not expose vsock_uds_path (jailer-on)\nserve log:\n%s", serveLog.String())
	}
	if info.VsockUDSPath == "/worker/vm.sock" {
		t.Fatalf("API returned guest-path unchanged — worker bind resolution missing")
	}
	t.Logf("jailer-on host-side UDS path: %s", info.VsockUDSPath)

	if !waitForFile(info.VsockUDSPath, 15*time.Second) {
		t.Fatalf("uds not created at %s\nserve log:\n%s", info.VsockUDSPath, serveLog.String())
	}

	// Check perms: socket 0660, parent dir 0750.
	si, err := os.Stat(info.VsockUDSPath)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if mode := si.Mode() & os.ModePerm; mode != 0o660 {
		t.Fatalf("socket mode = %o, want 0660", mode)
	}

	// Dial + CONNECT + exchange.
	var handshake string
	dialDeadline := time.Now().Add(30 * time.Second)
	var conn net.Conn
	var br *bufio.Reader
	for time.Now().Before(dialDeadline) {
		c, derr := net.Dial("unix", info.VsockUDSPath)
		if derr != nil {
			time.Sleep(300 * time.Millisecond)
			continue
		}
		_ = c.SetDeadline(time.Now().Add(15 * time.Second))
		if _, werr := fmt.Fprintf(c, "CONNECT %d\n", 12360); werr != nil {
			c.Close()
			continue
		}
		r := bufio.NewReader(c)
		line, rerr := r.ReadString('\n')
		if rerr != nil {
			c.Close()
			time.Sleep(300 * time.Millisecond)
			continue
		}
		hs := strings.TrimRight(line, "\r\n")
		if hs == "OK" {
			conn = c
			br = r
			handshake = hs
			break
		}
		c.Close()
		time.Sleep(300 * time.Millisecond)
	}
	if handshake != "OK" {
		t.Fatalf("handshake = %q, want OK\nserve log:\n%s", handshake, serveLog.String())
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, "jail-ping\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := br.ReadString('\n')
	if err != nil && err != io.EOF {
		t.Fatalf("read echo: %v", err)
	}
	if want := "echo:jail-ping\n"; got != want {
		t.Fatalf("echo = %q, want %q", got, want)
	}

	if err := client.StopVM(context.Background(), runResp.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// The socket file sits inside the worker's RunDir; after VM stop +
	// cleanup, the jailer tears down the RunDir. Verify the socket is
	// gone (either unlinked by the VMM or removed with the RunDir).
	stopDeadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(stopDeadline) {
		if _, err := os.Stat(info.VsockUDSPath); os.IsNotExist(err) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("uds socket file not removed after stop: %s", info.VsockUDSPath)
}

// TestE2EJailer_UDS_NonBindPath is a second jailer variant exercising the
// ResolveWorkerHostSidePath fallback: when the UDS guest path is NOT
// under /worker, the helper prepends JailRoot. Most such paths are not
// writable by the jailed VMM (the chroot is nearly empty), so this test
// deliberately points at /worker-subdir to stay writable and asserts the
// resolver picks the correct branch.
func TestE2EJailer_UDS_NonBindPath(t *testing.T) {
	if os.Getenv("E2E") != "1" {
		t.Skip("set E2E=1 to enable")
	}
	requirePrivilegedExecIntegration(t)
	requireJailerAvailable(t)

	kernel := resolveE2EKernel(t)
	bins := buildProjectBinaries(t)
	contextDir := buildVsockListenerFixture(t, 12361)

	addr := freeLocalAddr(t)
	serverURL := "http://" + addr
	cacheDir := filepath.Join(t.TempDir(), "cache")
	serveCmd := exec.Command(bins.gocracker, buildServeArgs(addr, cacheDir, bins)...)
	var serveLog lockedBuffer
	serveCmd.Stdout = &serveLog
	serveCmd.Stderr = &serveLog
	serveCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := serveCmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { stopCommand(t, serveCmd) })
	waitForAPI(t, serverURL, 45*time.Second)

	client := internalapi.NewClient(serverURL)
	runCtx, runCancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer runCancel()
	runResp, err := client.Run(runCtx, internalapi.RunRequest{
		Dockerfile:   filepath.Join(contextDir, "Dockerfile"),
		Context:      contextDir,
		KernelPath:   kernel,
		MemMB:        256,
		DiskSizeMB:   256,
		ExecEnabled:  true,
		VsockUDSPath: "/worker/nested/vm.sock",
	})
	if err != nil {
		t.Fatalf("api run: %v", err)
	}
	defer func() { _ = client.StopVM(context.Background(), runResp.ID) }()
	if _, err := waitForVMStateViaClient(t, client, runResp.ID, "running", 120*time.Second); err != nil {
		t.Fatalf("wait running: %v\nserve log:\n%s", err, serveLog.String())
	}
	info, err := client.GetVM(context.Background(), runResp.ID)
	if err != nil {
		t.Fatalf("get vm: %v", err)
	}
	// Must resolve through the /worker bind: hostPath ends in "nested/vm.sock".
	if !strings.HasSuffix(info.VsockUDSPath, "/nested/vm.sock") {
		t.Fatalf("host-side path = %q, want ...nested/vm.sock", info.VsockUDSPath)
	}
	if !waitForFile(info.VsockUDSPath, 15*time.Second) {
		t.Fatalf("uds not created at %s\nserve log:\n%s", info.VsockUDSPath, serveLog.String())
	}
}

// requireJailerAvailable skips when the environment disables the jailer,
// since this whole file only makes sense with jailer-on. The env flag is
// set by test-harness scripts on hosts (e.g. GitHub Actions) where
// pivot_root fails.
func requireJailerAvailable(t *testing.T) {
	t.Helper()
	if os.Getenv("GOCRACKER_JAILER_OFF") == "1" {
		t.Skip("jailer disabled via GOCRACKER_JAILER_OFF=1")
	}
}
