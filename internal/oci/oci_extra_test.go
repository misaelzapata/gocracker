package oci

import (
	"archive/tar"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

func TestMapKeysEmpty(t *testing.T) {
	got := mapKeys[string](nil)
	if got != nil {
		t.Fatalf("mapKeys(nil) = %v, want nil", got)
	}
}

func TestMapKeysNonEmpty(t *testing.T) {
	got := mapKeys(map[string]int{"b": 2, "a": 1, "c": 3})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("mapKeys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mapKeys[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCloneStringMapEmpty(t *testing.T) {
	got := cloneStringMap(nil)
	if got != nil {
		t.Fatalf("cloneStringMap(nil) = %v, want nil", got)
	}
}

func TestCloneStringMapNonEmpty(t *testing.T) {
	orig := map[string]string{"a": "1", "b": "2"}
	clone := cloneStringMap(orig)
	if len(clone) != 2 || clone["a"] != "1" || clone["b"] != "2" {
		t.Fatalf("clone = %v", clone)
	}
	clone["a"] = "X"
	if orig["a"] != "1" {
		t.Fatal("clone modified original")
	}
}

func TestHealthcheckFromOCINil(t *testing.T) {
	got := healthcheckFromOCI(nil)
	if got != nil {
		t.Fatalf("healthcheckFromOCI(nil) = %v, want nil", got)
	}
}

func TestHealthcheckFromOCIPopulated(t *testing.T) {
	cfg := &v1.HealthConfig{
		Test:        []string{"CMD", "curl", "-f", "http://localhost/"},
		Interval:    time.Duration(30 * time.Second),
		Timeout:     time.Duration(10 * time.Second),
		StartPeriod: time.Duration(5 * time.Second),
		Retries:     3,
	}
	got := healthcheckFromOCI(cfg)
	if got == nil {
		t.Fatal("healthcheckFromOCI returned nil")
	}
	if len(got.Test) != 4 || got.Test[0] != "CMD" {
		t.Fatalf("Test = %v", got.Test)
	}
	if got.Interval != 30*time.Second {
		t.Fatalf("Interval = %v", got.Interval)
	}
	if got.Retries != 3 {
		t.Fatalf("Retries = %d", got.Retries)
	}
}

func TestBlocksToGroups(t *testing.T) {
	if got := blocksToGroups(32768, 32768); got != 1 {
		t.Fatalf("blocksToGroups(32768, 32768) = %d, want 1", got)
	}
	if got := blocksToGroups(32769, 32768); got != 2 {
		t.Fatalf("blocksToGroups(32769, 32768) = %d, want 2", got)
	}
	if got := blocksToGroups(65536, 32768); got != 2 {
		t.Fatalf("blocksToGroups(65536, 32768) = %d, want 2", got)
	}
}

func TestApplyTar_DeepDirectory(t *testing.T) {
	dir := t.TempDir()
	buf := makeTar(t, []tarEntry{
		{Name: "a/b/c/d/e/f/", Type: tar.TypeDir, Mode: 0755},
		{Name: "a/b/c/d/e/f/file.txt", Type: tar.TypeReg, Mode: 0644, Body: "deep"},
	})
	if err := applyTar(dir, buf); err != nil {
		t.Fatalf("applyTar: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "a", "b", "c", "d", "e", "f", "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "deep" {
		t.Fatalf("content = %q", data)
	}
}

func TestApplyTar_WhiteoutDirectory(t *testing.T) {
	dir := t.TempDir()
	buf1 := makeTar(t, []tarEntry{
		{Name: "dir/", Type: tar.TypeDir, Mode: 0755},
		{Name: "dir/sub/", Type: tar.TypeDir, Mode: 0755},
		{Name: "dir/sub/file.txt", Type: tar.TypeReg, Mode: 0644, Body: "x"},
	})
	if err := applyTar(dir, buf1); err != nil {
		t.Fatal(err)
	}
	buf2 := makeTar(t, []tarEntry{
		{Name: "dir/.wh.sub", Type: tar.TypeReg, Mode: 0644},
	})
	if err := applyTar(dir, buf2); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dir", "sub")); !os.IsNotExist(err) {
		t.Fatal("sub should be removed by whiteout")
	}
}

func TestBuildExt4MinimalRootfs(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "test.txt"), []byte("hello"), 0644)
	image := filepath.Join(t.TempDir(), "disk.ext4")
	if err := BuildExt4(root, image, 32); err != nil {
		t.Fatalf("BuildExt4(32MB): %v", err)
	}
	fi, err := os.Stat(image)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() < 32*1024*1024 {
		t.Fatalf("image size = %d, want >= %d", fi.Size(), 32*1024*1024)
	}
}

func TestRegistryPullLockPathCustomRegistry(t *testing.T) {
	ref, err := name.ParseReference("myregistry.io/app:latest")
	if err != nil {
		t.Fatal(err)
	}
	got := registryPullLockPath("/tmp/cache", ref)
	if got != "" {
		t.Fatalf("registryPullLockPath = %q, want empty", got)
	}
}

func TestRegistryPullLockPathDockerHubLibrary(t *testing.T) {
	ref, err := name.ParseReference("library/alpine:latest")
	if err != nil {
		t.Fatal(err)
	}
	got := registryPullLockPath("/tmp/cache", ref)
	if got == "" {
		t.Fatal("registryPullLockPath should return lock path for Docker Hub")
	}
}

func TestMapKeysSorted(t *testing.T) {
	keys := mapKeys(map[string]bool{"z": true, "a": true, "m": true})
	if !sort.StringsAreSorted(keys) {
		t.Fatalf("keys not sorted: %v", keys)
	}
}

func TestApplyTar_SymlinkOverwrite(t *testing.T) {
	dir := t.TempDir()
	buf1 := makeTar(t, []tarEntry{
		{Name: "link", Type: tar.TypeSymlink, Linkname: "old-target"},
	})
	if err := applyTar(dir, buf1); err != nil {
		t.Fatal(err)
	}
	buf2 := makeTar(t, []tarEntry{
		{Name: "link", Type: tar.TypeSymlink, Linkname: "new-target"},
	})
	if err := applyTar(dir, buf2); err != nil {
		t.Fatal(err)
	}
	target, _ := os.Readlink(filepath.Join(dir, "link"))
	if target != "new-target" {
		t.Fatalf("link target = %q, want new-target", target)
	}
}

func TestApplyTar_FilePermissions(t *testing.T) {
	dir := t.TempDir()
	buf := makeTar(t, []tarEntry{
		{Name: "exec.sh", Type: tar.TypeReg, Mode: 0755, Body: "#!/bin/sh"},
		{Name: "data.txt", Type: tar.TypeReg, Mode: 0644, Body: "hello"},
	})
	if err := applyTar(dir, buf); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(filepath.Join(dir, "exec.sh"))
	if fi.Mode().Perm()&0111 == 0 {
		t.Fatal("exec.sh should be executable")
	}
}

func TestBuildExt4LargerSize(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "data.bin"), make([]byte, 1024), 0644)
	image := filepath.Join(t.TempDir(), "disk.ext4")
	if err := BuildExt4(root, image, 128); err != nil {
		t.Fatalf("BuildExt4(128MB): %v", err)
	}
}
