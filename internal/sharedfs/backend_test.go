package sharedfs

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestAttachAndNilBackendHelpers(t *testing.T) {
	backend := Attach("/tmp/virtiofs.sock")
	if got := backend.SocketPath(); got != "/tmp/virtiofs.sock" {
		t.Fatalf("SocketPath() = %q", got)
	}
	if err := (*Backend)(nil).Close(); err != nil {
		t.Fatalf("nil Close() error = %v", err)
	}
	if got := (*Backend)(nil).ErrorOutput(); got != "" {
		t.Fatalf("nil ErrorOutput() = %q", got)
	}
}

func TestWaitForSocketSuccessAndFailure(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "sock")
	cmd := exec.Command("sleep", "1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = os.WriteFile(socketPath, nil, 0o644)
	}()
	if err := waitForSocket(cmd, socketPath, time.Second); err != nil {
		t.Fatalf("waitForSocket() error = %v", err)
	}

	doneCmd := exec.Command("true")
	if err := doneCmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = doneCmd.Wait()
	if err := waitForSocket(doneCmd, filepath.Join(t.TempDir(), "missing"), 100*time.Millisecond); err == nil {
		t.Fatal("waitForSocket(exited process) error = nil")
	}
}

func TestSandboxHelpers(t *testing.T) {
	mode := preferredSandboxMode()
	switch mode {
	case "chroot", "namespace", "none":
	default:
		t.Fatalf("preferredSandboxMode() = %q", mode)
	}
	_ = namespaceSandboxAvailable()
}
