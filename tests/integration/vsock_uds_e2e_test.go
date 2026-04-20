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
	"sync"
	"syscall"
	"testing"
	"time"

	internalapi "github.com/gocracker/gocracker/internal/api"
)

// TestE2EUDS_DialAndExchange boots a VM with --vsock-uds-path and a tiny
// in-guest listener on vsock port 12346, then dials the UDS from the test
// process, sends "CONNECT 12346\n", expects "OK\n", and exchanges a
// ping/echo. This proves the Firecracker-style UDS path is wired
// end-to-end: CLI flag → VM config → udsListener → bridge to DialVsock
// → virtio-vsock → guest listener.
//
// Jailer-off. The jailer-on variant lives in jailer_uds_e2e_test.go.
func TestE2EUDS_DialAndExchange(t *testing.T) {
	if os.Getenv("E2E") != "1" {
		t.Skip("set E2E=1 to enable")
	}
	requirePrivilegedExecIntegration(t)
	t.Setenv("GOCRACKER_JAILER_OFF", "1")

	kernel := resolveE2EKernel(t)
	bins := buildProjectBinaries(t)
	contextDir := buildVsockListenerFixture(t, 12346)

	udsDir := t.TempDir()
	if err := os.Chmod(udsDir, 0o755); err != nil {
		t.Fatalf("chmod tempdir: %v", err)
	}
	udsPath := filepath.Join(udsDir, "vm.sock")

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
	runCtx, runCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer runCancel()
	runResp, err := client.Run(runCtx, internalapi.RunRequest{
		Dockerfile:   filepath.Join(contextDir, "Dockerfile"),
		Context:      contextDir,
		KernelPath:   kernel,
		MemMB:        256,
		DiskSizeMB:   256,
		ExecEnabled:  true,
		VsockUDSPath: udsPath,
	})
	if err != nil {
		t.Fatalf("api run: %v\nserve log:\n%s", err, serveLog.String())
	}
	defer func() { _ = client.StopVM(context.Background(), runResp.ID) }()

	if _, err := waitForVMStateViaClient(t, client, runResp.ID, "running", 90*time.Second); err != nil {
		t.Fatalf("wait running: %v\nserve log:\n%s", err, serveLog.String())
	}

	// The API's GET /vms/{id} exposes the host-side resolved path. Under
	// jailer-off it's the same path we passed; this test also validates
	// the field is populated.
	info, err := client.GetVM(context.Background(), runResp.ID)
	if err != nil {
		t.Fatalf("get vm: %v", err)
	}
	if info.VsockUDSPath != udsPath {
		t.Fatalf("info.VsockUDSPath = %q, want %q", info.VsockUDSPath, udsPath)
	}

	// Poll until the UDS exists (VMM creates it after device init).
	if !waitForFile(udsPath, 15*time.Second) {
		t.Fatalf("uds path not created within 15s: %s\nserve log:\n%s", udsPath, serveLog.String())
	}

	// Dial the UDS and hand-shake, retrying briefly while the in-guest
	// listener binds on fresh boots. connectUDS closes any failed
	// attempts itself and only returns the final successful pair.
	conn, br := connectUDS(t, udsPath, 12346, 30*time.Second, &serveLog)
	t.Cleanup(func() { conn.Close() })

	// Exchange ping/echo with the in-guest listener.
	if _, err := io.WriteString(conn, "ping\n"); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	got, err := br.ReadString('\n')
	if err != nil && err != io.EOF {
		t.Fatalf("read echo: %v", err)
	}
	if want := "echo:ping\n"; got != want {
		t.Fatalf("echo = %q, want %q", got, want)
	}

	if err := client.StopVM(context.Background(), runResp.ID); err != nil {
		t.Fatalf("stop vm: %v", err)
	}

	// Socket file should be removed by cleanup().
	if _, err := os.Stat(udsPath); !os.IsNotExist(err) {
		t.Fatalf("uds socket file not removed after stop: stat err=%v", err)
	}
}

// TestE2EUDS_MultipleClients boots one VM with a UDS and the in-guest
// listener on port 12347, then opens 5 concurrent UDS connections, each
// running its own ping/echo round. They must not cross-talk.
func TestE2EUDS_MultipleClients(t *testing.T) {
	if os.Getenv("E2E") != "1" {
		t.Skip("set E2E=1 to enable")
	}
	requirePrivilegedExecIntegration(t)
	t.Setenv("GOCRACKER_JAILER_OFF", "1")

	kernel := resolveE2EKernel(t)
	bins := buildProjectBinaries(t)
	contextDir := buildVsockListenerFixture(t, 12347)

	udsPath := filepath.Join(t.TempDir(), "vm.sock")

	addr := freeLocalAddr(t)
	serverURL := "http://" + addr
	cacheDir := filepath.Join(t.TempDir(), "cache")
	serveCmd := exec.Command(bins.gocracker, buildServeArgs(addr, cacheDir, bins)...)
	var serveLog lockedBuffer
	serveCmd.Stdout = &serveLog
	serveCmd.Stderr = &serveLog
	serveCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := serveCmd.Start(); err != nil {
		t.Fatalf("start serve: %v", err)
	}
	t.Cleanup(func() { stopCommand(t, serveCmd) })
	waitForAPI(t, serverURL, 45*time.Second)

	client := internalapi.NewClient(serverURL)
	runCtx, runCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer runCancel()
	runResp, err := client.Run(runCtx, internalapi.RunRequest{
		Dockerfile:   filepath.Join(contextDir, "Dockerfile"),
		Context:      contextDir,
		KernelPath:   kernel,
		MemMB:        256,
		DiskSizeMB:   256,
		ExecEnabled:  true,
		VsockUDSPath: udsPath,
	})
	if err != nil {
		t.Fatalf("api run: %v", err)
	}
	defer func() { _ = client.StopVM(context.Background(), runResp.ID) }()
	if _, err := waitForVMStateViaClient(t, client, runResp.ID, "running", 90*time.Second); err != nil {
		t.Fatalf("wait running: %v", err)
	}
	if !waitForFile(udsPath, 15*time.Second) {
		t.Fatalf("uds path missing: %s", udsPath)
	}

	const clients = 5
	var wg sync.WaitGroup
	errs := make(chan error, clients)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Retry the whole handshake briefly to ride out in-guest bind races.
			var got string
			deadline := time.Now().Add(30 * time.Second)
			for time.Now().Before(deadline) {
				c, derr := net.Dial("unix", udsPath)
				if derr != nil {
					time.Sleep(200 * time.Millisecond)
					continue
				}
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				if _, werr := fmt.Fprintf(c, "CONNECT 12347\n"); werr != nil {
					c.Close()
					continue
				}
				br := bufio.NewReader(c)
				hs, rerr := br.ReadString('\n')
				if rerr != nil || strings.TrimRight(hs, "\r\n") != "OK" {
					c.Close()
					time.Sleep(200 * time.Millisecond)
					continue
				}
				payload := fmt.Sprintf("client-%d-ping\n", i)
				if _, werr := io.WriteString(c, payload); werr != nil {
					c.Close()
					continue
				}
				line, rerr := br.ReadString('\n')
				c.Close()
				if rerr != nil && rerr != io.EOF {
					continue
				}
				got = line
				break
			}
			want := fmt.Sprintf("echo:client-%d-ping\n", i)
			if got != want {
				errs <- fmt.Errorf("client %d: got %q, want %q", i, got, want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestE2EUDS_HTTPEndpointStillWorks is a regression check: when UDS is
// configured, the pre-existing /vms/{id}/vsock/connect HTTP fallback
// must still function. This guards against accidentally disabling the
// HTTP path as part of exposing the UDS.
func TestE2EUDS_HTTPEndpointStillWorks(t *testing.T) {
	if os.Getenv("E2E") != "1" {
		t.Skip("set E2E=1 to enable")
	}
	requirePrivilegedExecIntegration(t)
	t.Setenv("GOCRACKER_JAILER_OFF", "1")

	kernel := resolveE2EKernel(t)
	bins := buildProjectBinaries(t)
	contextDir := buildVsockListenerFixture(t, 12348)
	udsPath := filepath.Join(t.TempDir(), "vm.sock")

	addr := freeLocalAddr(t)
	serverURL := "http://" + addr
	cacheDir := filepath.Join(t.TempDir(), "cache")
	serveCmd := exec.Command(bins.gocracker, buildServeArgs(addr, cacheDir, bins)...)
	var serveLog lockedBuffer
	serveCmd.Stdout = &serveLog
	serveCmd.Stderr = &serveLog
	serveCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := serveCmd.Start(); err != nil {
		t.Fatalf("start serve: %v", err)
	}
	t.Cleanup(func() { stopCommand(t, serveCmd) })
	waitForAPI(t, serverURL, 45*time.Second)

	client := internalapi.NewClient(serverURL)
	runCtx, runCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer runCancel()
	runResp, err := client.Run(runCtx, internalapi.RunRequest{
		Dockerfile:   filepath.Join(contextDir, "Dockerfile"),
		Context:      contextDir,
		KernelPath:   kernel,
		MemMB:        256,
		DiskSizeMB:   256,
		ExecEnabled:  true,
		VsockUDSPath: udsPath,
	})
	if err != nil {
		t.Fatalf("api run: %v", err)
	}
	defer func() { _ = client.StopVM(context.Background(), runResp.ID) }()
	if _, err := waitForVMStateViaClient(t, client, runResp.ID, "running", 90*time.Second); err != nil {
		t.Fatalf("wait running: %v", err)
	}

	var conn net.Conn
	dialDeadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(dialDeadline) {
		dialCtx, dialCancel := context.WithTimeout(context.Background(), 3*time.Second)
		c, derr := client.DialVsock(dialCtx, runResp.ID, 12348)
		dialCancel()
		if derr == nil {
			conn = c
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if conn == nil {
		t.Fatalf("HTTP /vsock/connect path failed\nserve log:\n%s", serveLog.String())
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(conn, "ping\n"); err != nil {
		t.Fatalf("write over HTTP-upgraded vsock: %v", err)
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("read echo: %v", err)
	}
	got := strings.TrimRight(string(buf[:n]), "\r\n")
	if got != "echo:ping" {
		t.Fatalf("echo via HTTP fallback = %q, want echo:ping", got)
	}
}

// connectUDS retries dial + CONNECT handshake until OK or deadline.
// Every failed attempt's socket is closed before the next dial so the
// test never leaks file descriptors. Returns the final, still-open
// connection and its bufio.Reader on success; fails the test otherwise.
func connectUDS(t *testing.T, path string, port uint32, timeout time.Duration, serveLog *lockedBuffer) (net.Conn, *bufio.Reader) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.Dial("unix", path)
		if err != nil {
			time.Sleep(300 * time.Millisecond)
			continue
		}
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		if _, werr := fmt.Fprintf(c, "CONNECT %d\n", port); werr != nil {
			c.Close()
			continue
		}
		br := bufio.NewReader(c)
		line, rerr := br.ReadString('\n')
		if rerr != nil {
			c.Close()
			time.Sleep(300 * time.Millisecond)
			continue
		}
		got := strings.TrimRight(line, "\r\n")
		if got == "OK" {
			// Handshake done — clear the deadline so the caller isn't
			// fighting the short handshake deadline during streaming.
			_ = c.SetDeadline(time.Time{})
			return c, br
		}
		c.Close()
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("connectUDS: never got OK handshake for %s port %d\nserve:\n%s",
		path, port, serveLog.String())
	return nil, nil
}
