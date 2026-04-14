package container

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWarmCacheEnabled_EnvParsing(t *testing.T) {
	cases := map[string]bool{
		"":      false,
		"0":     false,
		"no":    false,
		"false": false,
		"1":     true,
		"yes":   true,
		"true":  true,
		"TRUE":  true,
		" 1 ":   true,
	}
	for v, want := range cases {
		t.Setenv("GOCRACKER_WARM_CACHE", v)
		if got := warmCacheEnabled(); got != want {
			t.Errorf("GOCRACKER_WARM_CACHE=%q: got %v want %v", v, got, want)
		}
	}
}

func TestWarmCacheInputsReady(t *testing.T) {
	ready := RunOptions{Image: "alpine", KernelPath: "/boot/vmlinux"}
	if !warmCacheInputsReady(ready) {
		t.Fatal("image + kernel should be ready")
	}

	// Missing image → not ready (Dockerfile builds aren't deterministic enough).
	notReady1 := ready
	notReady1.Image = ""
	notReady1.Dockerfile = "/path/to/Dockerfile"
	if warmCacheInputsReady(notReady1) {
		t.Error("Dockerfile-only should NOT be cacheable")
	}

	// Missing kernel path → not ready.
	notReady2 := ready
	notReady2.KernelPath = ""
	if warmCacheInputsReady(notReady2) {
		t.Error("missing kernel should NOT be cacheable")
	}
}

func TestComputeWarmCacheKey_StableAcrossInvocations(t *testing.T) {
	dir := t.TempDir()
	kernel := filepath.Join(dir, "vmlinux")
	if err := os.WriteFile(kernel, []byte("fake kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := RunOptions{
		Image:      "alpine@sha256:abc",
		KernelPath: kernel,
		MemMB:      128,
		CPUs:       1,
		Arch:       "amd64",
		Cmd:        []string{"echo", "hi"},
	}
	k1, ok := computeWarmCacheKey(opts)
	if !ok || k1 == "" {
		t.Fatal("expected key")
	}
	k2, ok := computeWarmCacheKey(opts)
	if !ok || k1 != k2 {
		t.Fatalf("key unstable: %s vs %s", k1, k2)
	}
}

func TestComputeWarmCacheKey_MissingKernelMisses(t *testing.T) {
	opts := RunOptions{
		Image:      "alpine",
		KernelPath: "/does/not/exist/vmlinux",
	}
	if _, ok := computeWarmCacheKey(opts); ok {
		t.Fatal("nonexistent kernel should produce ok=false")
	}
}
