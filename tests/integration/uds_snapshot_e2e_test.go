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

// TestE2EJailer_UDS_SnapshotRestore is the final gate for Fase 1: boot
// a VM under jailer-on with a UDS, clone it (which snapshots + restores
// with a fresh worker), and verify both the source and the clone each
// have their own working UDS.
//
// The VMM config serialized in snapshot.json preserves
// Vsock.UDSPath="/worker/vm.sock"; the restore in a new worker resolves
// /worker to a different RunDir on the host, so the clone's UDS lives at
// a different host path without any OverrideVsockUDSPath handling.
func TestE2EJailer_UDS_SnapshotRestore(t *testing.T) {
	if os.Getenv("E2E") != "1" {
		t.Skip("set E2E=1 to enable")
	}
	requirePrivilegedExecIntegration(t)
	requireJailerAvailable(t)

	kernel := resolveE2EKernel(t)
	bins := buildProjectBinaries(t)
	contextDir := buildVsockListenerFixture(t, 12370)

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
	runCtx, runCancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer runCancel()

	// Source VM with UDS under /worker.
	src, err := client.Run(runCtx, internalapi.RunRequest{
		Dockerfile:   filepath.Join(contextDir, "Dockerfile"),
		Context:      contextDir,
		KernelPath:   kernel,
		MemMB:        256,
		DiskSizeMB:   256,
		ExecEnabled:  true,
		NetworkMode:  "auto",
		VsockUDSPath: "/worker/vm.sock",
		Wait:         true,
	})
	if err != nil {
		t.Fatalf("/run source: %v\nserve:\n%s", err, serveLog.String())
	}
	t.Cleanup(func() { _ = client.StopVM(context.Background(), src.ID) })

	srcInfo, err := client.GetVM(context.Background(), src.ID)
	if err != nil {
		t.Fatalf("get src: %v", err)
	}
	if srcInfo.VsockUDSPath == "" {
		t.Fatalf("source has no vsock_uds_path")
	}
	if !waitForFile(srcInfo.VsockUDSPath, 15*time.Second) {
		t.Fatalf("source uds not ready at %s", srcInfo.VsockUDSPath)
	}

	// Validate source UDS actually serves traffic before we clone.
	doHandshake(t, srcInfo.VsockUDSPath, 12370, "src-ping", "echo:src-ping\n", &serveLog)

	// Clone (snapshot + restore in a fresh jailer).
	cloneCtx, cloneCancel := context.WithTimeout(context.Background(), 120*time.Second)
	clone, err := client.CloneVM(cloneCtx, src.ID, internalapi.CloneRequest{
		ExecEnabled: true,
		NetworkMode: "auto",
	})
	cloneCancel()
	if err != nil {
		t.Fatalf("/clone: %v\nserve:\n%s", err, serveLog.String())
	}
	t.Cleanup(func() { _ = client.StopVM(context.Background(), clone.ID) })
	if !clone.RestoredFromSnapshot {
		t.Errorf("clone RestoredFromSnapshot=false, want true")
	}

	cloneInfo, err := client.GetVM(context.Background(), clone.ID)
	if err != nil {
		t.Fatalf("get clone: %v", err)
	}
	if cloneInfo.VsockUDSPath == "" {
		t.Fatalf("clone lost vsock_uds_path (snapshot didn't preserve it)")
	}
	if cloneInfo.VsockUDSPath == srcInfo.VsockUDSPath {
		t.Fatalf("clone UDS path %q collides with source — each jailed VM should have its own worker RunDir",
			cloneInfo.VsockUDSPath)
	}
	if !waitForFile(cloneInfo.VsockUDSPath, 15*time.Second) {
		t.Fatalf("clone uds not ready at %s\nserve:\n%s", cloneInfo.VsockUDSPath, serveLog.String())
	}

	// Clone's UDS bridges to its own VMM → its own guest → an independent
	// in-guest listener. Exchange a distinct payload to prove isolation.
	doHandshake(t, cloneInfo.VsockUDSPath, 12370, "clone-ping", "echo:clone-ping\n", &serveLog)

	// Both sockets must remain independently operational afterwards.
	doHandshake(t, srcInfo.VsockUDSPath, 12370, "src-again", "echo:src-again\n", &serveLog)
}

// doHandshake dials the UDS, sends CONNECT <port>, expects OK, writes
// payload and expects want back. Retries briefly to ride out in-guest
// bind races on fresh boots.
func doHandshake(t *testing.T, path string, port uint32, payload, want string, serveLog *lockedBuffer) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		c, err := net.Dial("unix", path)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := fmt.Fprintf(c, "CONNECT %d\n", port); err != nil {
			c.Close()
			continue
		}
		br := bufio.NewReader(c)
		line, err := br.ReadString('\n')
		if err != nil || strings.TrimRight(line, "\r\n") != "OK" {
			c.Close()
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if _, err := io.WriteString(c, payload+"\n"); err != nil {
			c.Close()
			continue
		}
		line, err = br.ReadString('\n')
		c.Close()
		if err != nil && err != io.EOF {
			continue
		}
		got = line
		break
	}
	if got != want {
		t.Fatalf("doHandshake(%s, %q) = %q, want %q\nserve:\n%s",
			path, payload, got, want, serveLog.String())
	}
}
