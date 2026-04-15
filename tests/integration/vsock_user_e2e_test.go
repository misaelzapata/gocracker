//go:build integration

package integration

import (
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

// TestE2EVsockUserRoundTrip boots a VM with a tiny in-guest listener that
// opens an AF_VSOCK server on port 12345 (NOT the exec broker port). The
// host dials through /vms/{id}/vsock/connect and exchanges a ping/echo to
// prove the user-level vsock path works. Gated on E2E=1 and root.
func TestE2EVsockUserRoundTrip(t *testing.T) {
	if os.Getenv("E2E") != "1" {
		t.Skip("set E2E=1 to enable")
	}
	requirePrivilegedExecIntegration(t)
	kernel := resolveE2EKernel(t)
	bins := buildProjectBinaries(t)

	// Build the in-guest vsock listener and stage it into a scratch fixture.
	contextDir := buildVsockListenerFixture(t, 12345)

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

	// ExecEnabled=true triggers buildVsockConfig → enables virtio-vsock on
	// the VM. Our user-level port 12345 is distinct from the exec broker's
	// default port, so the two don't collide.
	runResp, err := client.Run(runCtx, internalapi.RunRequest{
		Dockerfile:  filepath.Join(contextDir, "Dockerfile"),
		Context:     contextDir,
		KernelPath:  kernel,
		MemMB:       256,
		DiskSizeMB:  256,
		ExecEnabled: true,
	})
	if err != nil {
		t.Fatalf("api run: %v\nserve log:\n%s", err, serveLog.String())
	}
	defer func() { _ = client.StopVM(context.Background(), runResp.ID) }()

	if _, err := waitForVMStateViaClient(t, client, runResp.ID, "running", 90*time.Second); err != nil {
		t.Fatalf("wait for running: %v\nserve log:\n%s", err, serveLog.String())
	}

	// Give the in-guest listener a moment to bind the vsock port after the
	// CMD starts. The guest's systemd-less init starts the workload almost
	// immediately, but bind() can still race the first host dial.
	var conn net.Conn
	var dialErr error
	dialDeadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(dialDeadline) {
		dialCtx, dialCancel := context.WithTimeout(context.Background(), 3*time.Second)
		conn, dialErr = client.DialVsock(dialCtx, runResp.ID, 12345)
		dialCancel()
		if dialErr == nil {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if dialErr != nil {
		t.Fatalf("DialVsock port 12345: %v\nserve log:\n%s", dialErr, serveLog.String())
	}
	defer conn.Close()

	// ping -> expect "echo:ping\n"
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(conn, "ping\n"); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("read echo: %v\nserve log:\n%s", err, serveLog.String())
	}
	got := strings.TrimRight(string(buf[:n]), "\r\n")
	if got != "echo:ping" {
		t.Fatalf("echo = %q, want %q\nserve log:\n%s", got, "echo:ping", serveLog.String())
	}

	if err := client.StopVM(context.Background(), runResp.ID); err != nil {
		t.Fatalf("stop vm: %v", err)
	}
}

// buildVsockListenerFixture compiles the in-guest vsock listener Go program
// and drops it together with a FROM-scratch Dockerfile into a fresh context
// directory. Returns the context directory path.
func buildVsockListenerFixture(t *testing.T, port uint32) string {
	t.Helper()
	dir := t.TempDir()
	src := strings.Replace(vsockListenerSource, "@PORT@", fmt.Sprintf("%d", port), 1)
	binPath := buildGuestProgram(t, src)
	copyFileIntoContext(t, binPath, filepath.Join(dir, "listener"))
	dockerfile := "FROM scratch\nCOPY listener /listener\nCMD [\"/listener\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	return dir
}

// vsockListenerSource is a self-contained Go program that opens an
// AF_VSOCK SOCK_STREAM listener on port @PORT@ (VMADDR_CID_ANY so any
// context can dial it) and echoes each frame back prefixed with "echo:".
// The dependency on golang.org/x/sys/unix is already vendored by the
// repo, so this compiles cleanly via buildGuestProgram (CGO_ENABLED=0).
const vsockListenerSource = `
package main

import (
	"bufio"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

const (
	vmaddrCIDAny = 0xffffffff
	listenPort   = @PORT@
)

func main() {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "socket: %v\n", err)
		os.Exit(1)
	}
	// Retry bind briefly — on cold boot the vsock transport may not be
	// fully registered before main() runs.
	sa := &unix.SockaddrVM{CID: vmaddrCIDAny, Port: listenPort}
	var bindErr error
	for i := 0; i < 50; i++ {
		bindErr = unix.Bind(fd, sa)
		if bindErr == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if bindErr != nil {
		fmt.Fprintf(os.Stderr, "bind: %v\n", bindErr)
		os.Exit(1)
	}
	if err := unix.Listen(fd, 8); err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "vsock-listener ready on port", listenPort)
	for {
		cfd, _, err := unix.Accept(fd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "accept: %v\n", err)
			continue
		}
		go handle(cfd)
	}
}

func handle(cfd int) {
	defer unix.Close(cfd)
	cf := os.NewFile(uintptr(cfd), "vsock-conn")
	defer cf.Close()
	scanner := bufio.NewScanner(cf)
	for scanner.Scan() {
		line := scanner.Text()
		if _, err := fmt.Fprintf(cf, "echo:%s\n", line); err != nil {
			return
		}
	}
}
`
