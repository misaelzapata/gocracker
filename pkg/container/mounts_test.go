package container

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestApplyMounts_SkipsSpecialFilesInDirectoryMounts(t *testing.T) {
	rootfsDir := t.TempDir()
	sourceDir := t.TempDir()

	regularPath := filepath.Join(sourceDir, "regular.txt")
	if err := os.WriteFile(regularPath, []byte("ok"), 0644); err != nil {
		t.Fatalf("write regular file: %v", err)
	}

	socketPath := filepath.Join(sourceDir, "socket.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	defer listener.Close()

	if err := applyMounts(rootfsDir, []Mount{{
		Source: sourceDir,
		Target: "/tmp",
	}}); err != nil {
		t.Fatalf("applyMounts: %v", err)
	}

	outRegular := filepath.Join(rootfsDir, "tmp", "regular.txt")
	data, err := os.ReadFile(outRegular)
	if err != nil {
		t.Fatalf("read copied regular file: %v", err)
	}
	if string(data) != "ok" {
		t.Fatalf("copied regular file = %q, want %q", string(data), "ok")
	}

	outSocket := filepath.Join(rootfsDir, "tmp", "socket.sock")
	if _, err := os.Lstat(outSocket); !os.IsNotExist(err) {
		t.Fatalf("socket placeholder should be skipped, got err=%v", err)
	}
}
