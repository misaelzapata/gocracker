package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDebugKernelPathPrefersEnv(t *testing.T) {
	t.Setenv("GOCRACKER_DEBUG_KERNEL", "/tmp/custom-kernel")
	if got := debugKernelPath(); got != "/tmp/custom-kernel" {
		t.Fatalf("debugKernelPath() = %q", got)
	}
}

func TestDebugKernelPathUsesLocalArtifacts(t *testing.T) {
	dir := t.TempDir()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(prev) }()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join("artifacts", "kernels"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("artifacts", "kernels", "gocracker-guest-standard-vmlinux")
	if err := os.WriteFile(path, []byte("kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOCRACKER_DEBUG_KERNEL", "")
	if got := debugKernelPath(); got != path {
		t.Fatalf("debugKernelPath() = %q, want %q", got, path)
	}
}
