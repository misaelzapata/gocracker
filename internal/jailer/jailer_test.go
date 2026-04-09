//go:build linux

package jailer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigValidateRejectsRelativeExec(t *testing.T) {
	cfg := Config{
		ID:       "vm-1",
		ExecFile: "gocracker-vmm",
		UID:      123,
		GID:      456,
	}
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("expected absolute exec-file validation error, got %v", err)
	}
}

func TestConfigValidateRejectsInvalidID(t *testing.T) {
	cfg := Config{
		ID:       "bad/id",
		ExecFile: filepath.Join(t.TempDir(), "gocracker-vmm"),
		UID:      123,
		GID:      456,
	}
	if err := osWriteFile(cfg.ExecFile); err != nil {
		t.Fatalf("write exec file: %v", err)
	}
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "unsupported character") {
		t.Fatalf("expected invalid id error, got %v", err)
	}
}

func TestConfigChrootDir(t *testing.T) {
	cfg := Config{
		ID:            "vm-123",
		ExecFile:      "/usr/local/bin/gocracker-vmm",
		ChrootBaseDir: "/srv/jailer",
	}
	got := cfg.chrootDir()
	want := "/srv/jailer/gocracker-vmm/vm-123/root"
	if got != want {
		t.Fatalf("chrootDir() = %q, want %q", got, want)
	}
}

func TestMkdirAllNoSymlinkRejectsSymlinkComponent(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real")
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := mkdirAllNoSymlink(filepath.Join(root, "link", "child"), 0755); err == nil || !strings.Contains(err.Error(), "symlink component") {
		t.Fatalf("mkdirAllNoSymlink() error = %v, want symlink component error", err)
	}
}

func TestCopyRegularFileRejectsSymlinkDestination(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src.bin")
	if err := osWriteFile(src); err != nil {
		t.Fatalf("write src: %v", err)
	}
	realDst := filepath.Join(root, "real.bin")
	if err := osWriteFile(realDst); err != nil {
		t.Fatalf("write real dst: %v", err)
	}
	dst := filepath.Join(root, "dst.bin")
	if err := os.Symlink(realDst, dst); err != nil {
		t.Fatalf("symlink dst: %v", err)
	}
	if err := copyRegularFile(src, dst, 0755); err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("copyRegularFile() error = %v, want symlink rejection", err)
	}
}

func osWriteFile(path string) error { return os.WriteFile(path, []byte("stub"), 0755) }
