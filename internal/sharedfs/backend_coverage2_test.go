package sharedfs

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestAttachSocketPath(t *testing.T) {
	b := Attach("/var/run/virtiofsd.sock")
	if got := b.SocketPath(); got != "/var/run/virtiofsd.sock" {
		t.Fatalf("SocketPath() = %q", got)
	}
}

func TestAttachClose(t *testing.T) {
	b := Attach("/tmp/test.sock")
	// Close on an attached backend (no cmd) should be fine
	if err := b.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
}

func TestAttachErrorOutput(t *testing.T) {
	b := Attach("/tmp/test.sock")
	if got := b.ErrorOutput(); got != "" {
		t.Fatalf("ErrorOutput() = %q", got)
	}
}

func TestNilBackendSocketPath(t *testing.T) {
	var b *Backend
	if got := b.SocketPath(); got != "" {
		t.Fatalf("nil SocketPath() = %q", got)
	}
}

func TestNilBackendClose(t *testing.T) {
	var b *Backend
	if err := b.Close(); err != nil {
		t.Fatalf("nil Close() = %v", err)
	}
}

func TestNilBackendErrorOutput(t *testing.T) {
	var b *Backend
	if got := b.ErrorOutput(); got != "" {
		t.Fatalf("nil ErrorOutput() = %q", got)
	}
}

func TestPreferredSandboxMode(t *testing.T) {
	mode := preferredSandboxMode()
	switch mode {
	case "chroot", "namespace", "none":
		// valid
	default:
		t.Fatalf("preferredSandboxMode() = %q", mode)
	}
}

func TestNamespaceSandboxAvailable(t *testing.T) {
	// Just exercise the function, result depends on system
	_ = namespaceSandboxAvailable()
}

func TestWaitForSocketCreatedBeforeTimeout(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")

	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	// Create socket after a short delay
	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = os.WriteFile(sockPath, nil, 0644)
	}()

	if err := waitForSocket(cmd, sockPath, 2*time.Second); err != nil {
		t.Fatalf("waitForSocket: %v", err)
	}
}

func TestWaitForSocketTimesOut(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "never.sock")

	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	err := waitForSocket(cmd, sockPath, 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !contains(err.Error(), "was not created in time") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWaitForSocketProcessExits(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "exit.sock")

	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	err := waitForSocket(cmd, sockPath, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected error when process exited")
	}
	if !contains(err.Error(), "exited before socket") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCloseWithRunningProcess(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	b := &Backend{
		cmd: cmd,
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
}

func TestCloseWithSocketDir(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "virtiofsd-cleanup")
	if err := os.MkdirAll(subdir, 0755); err != nil {
		t.Fatal(err)
	}

	b := &Backend{
		socketDir: subdir,
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if _, err := os.Stat(subdir); !os.IsNotExist(err) {
		t.Fatal("socketDir should have been removed")
	}
}

func TestFindVirtioFSD(t *testing.T) {
	// This test just exercises the function; it may or may not find the binary
	_, err := findVirtioFSD()
	if err != nil {
		// Acceptable: virtiofsd might not be installed
		t.Skipf("virtiofsd not found: %v", err)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
