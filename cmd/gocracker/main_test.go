package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gocracker/gocracker/internal/runtimecfg"
)

func TestResolveRequiredExistingPath_ReturnsAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "kernel.bin")
	writeTestFile(t, file, []byte("kernel"))

	got := resolveRequiredExistingPath("kernel", file)
	if !filepath.IsAbs(got) {
		t.Fatalf("resolveRequiredExistingPath returned %q, want absolute path", got)
	}
	if got != file {
		t.Fatalf("resolveRequiredExistingPath = %q, want %q", got, file)
	}
}

func TestPid1ModeForCLIWait(t *testing.T) {
	if got := pid1ModeForCLIWait(true); got != runtimecfg.PID1ModeSupervised {
		t.Fatalf("pid1ModeForCLIWait(true) = %q, want %q", got, runtimecfg.PID1ModeSupervised)
	}
	if got := pid1ModeForCLIWait(false); got != "" {
		t.Fatalf("pid1ModeForCLIWait(false) = %q, want empty", got)
	}
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
