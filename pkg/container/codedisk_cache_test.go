package container

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCodeDiskCacheNew_RequiresRoot guards the constructor against the
// trivial misuse of passing an empty root path — happens when callers
// thread an unset config field, and the resulting cache would silently
// land at the cwd.
func TestCodeDiskCacheNew_RequiresRoot(t *testing.T) {
	if _, err := NewCodeDiskCache(""); err == nil {
		t.Fatal("expected error for empty root, got nil")
	}
}

// TestCodeDiskCacheNew_CreatesDir confirms the constructor materialises
// the cache root if it doesn't exist (the typical path on a fresh host
// or a fresh test).
func TestCodeDiskCacheNew_CreatesDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing", "child")
	c, err := NewCodeDiskCache(root)
	if err != nil {
		t.Fatalf("NewCodeDiskCache: %v", err)
	}
	if c.Root() != root {
		t.Errorf("Root() = %q, want %q", c.Root(), root)
	}
	if _, err := os.Stat(root); err != nil {
		t.Errorf("root not created: %v", err)
	}
}

// TestHashSourceDir_StableForSameContent is the cache invariant:
// hashing the same logical content twice yields the same digest, even
// when the dirs live at different absolute paths. Without this the
// cache would always miss and we'd burn the build cost on every call.
func TestHashSourceDir_StableForSameContent(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	for _, p := range []string{a, b} {
		if err := os.WriteFile(filepath.Join(p, "main.sh"), []byte("#!/bin/sh\necho v1\n"), 0o755); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(p, "lib"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(p, "lib", "x.txt"), []byte("hello"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	hashA, err := HashSourceDir(a, "ext4", false)
	if err != nil {
		t.Fatalf("hash a: %v", err)
	}
	hashB, err := HashSourceDir(b, "ext4", false)
	if err != nil {
		t.Fatalf("hash b: %v", err)
	}
	if hashA != hashB {
		t.Errorf("hashA = %q, hashB = %q — same content should hash equally", hashA, hashB)
	}
	if hashA == "" {
		t.Errorf("hash empty")
	}
}

// TestHashSourceDir_ChangesOnContent guards against false-hits — a
// single byte change in any file should change the digest.
func TestHashSourceDir_ChangesOnContent(t *testing.T) {
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "x"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	h1, err := HashSourceDir(d, "ext4", false)
	if err != nil {
		t.Fatalf("h1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(d, "x"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	h2, err := HashSourceDir(d, "ext4", false)
	if err != nil {
		t.Fatalf("h2: %v", err)
	}
	if h1 == h2 {
		t.Errorf("hash should differ after content change: both %q", h1)
	}
}

// TestHashSourceDir_ChangesOnFSType confirms the fs type is part of
// the key — an ext4 build and a (hypothetical future) squashfs build
// of the same dir get distinct cache slots.
func TestHashSourceDir_ChangesOnFSType(t *testing.T) {
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "x"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	hExt4, _ := HashSourceDir(d, "ext4", false)
	hSquash, _ := HashSourceDir(d, "squashfs", false)
	if hExt4 == hSquash {
		t.Errorf("fs type should distinguish hash; both %q", hExt4)
	}
}

// TestHashSourceDir_ChangesOnReadOnly: same dir, ro vs rw → distinct
// slots so callers reasoning about RO reuse don't accidentally pick
// up an RW image (the bytes are identical but the mount semantics
// differ at boot time).
func TestHashSourceDir_ChangesOnReadOnly(t *testing.T) {
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "x"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	hRW, _ := HashSourceDir(d, "ext4", false)
	hRO, _ := HashSourceDir(d, "ext4", true)
	if hRW == hRO {
		t.Errorf("ro flag should distinguish hash; both %q", hRW)
	}
}

// TestHashSourceDir_NonExistentReturnsStable confirms a missing
// source dir is treated as "empty content" rather than an error —
// the resulting hash is stable across calls so a caller can still
// look up / store an empty image.
func TestHashSourceDir_NonExistentReturnsStable(t *testing.T) {
	h1, err := HashSourceDir("/this/path/does/not/exist", "ext4", false)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	h2, err := HashSourceDir("/another/missing", "ext4", false)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if h1 != h2 {
		t.Errorf("hashes differ for missing dirs: %q vs %q", h1, h2)
	}
}

// TestCacheLookupAndStore covers the round trip: miss → store → hit.
func TestCacheLookupAndStore(t *testing.T) {
	c, err := NewCodeDiskCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewCodeDiskCache: %v", err)
	}
	hash := "deadbeef"

	// Miss.
	path, hit, err := c.Lookup(hash, "ext4", false)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if hit {
		t.Errorf("expected miss, got hit at %q", path)
	}

	// Build a tiny "image" file and Store it.
	src := filepath.Join(t.TempDir(), "src.ext4")
	if err := os.WriteFile(src, []byte("FAKE_EXT4_BYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	stored, err := c.Store(hash, "ext4", false, src)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if stored != path {
		t.Errorf("Store path %q != Lookup miss path %q", stored, path)
	}

	// Hit.
	gotPath, hit, err := c.Lookup(hash, "ext4", false)
	if err != nil {
		t.Fatalf("Lookup post-store: %v", err)
	}
	if !hit {
		t.Errorf("expected hit, got miss")
	}
	if gotPath != path {
		t.Errorf("hit path %q != %q", gotPath, path)
	}
	// Bytes round-trip.
	got, err := os.ReadFile(gotPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "FAKE_EXT4_BYTES" {
		t.Errorf("cached bytes %q != source", string(got))
	}
}

// TestCacheLookupRejectsZeroByte: a previous interrupted Store may have
// left a zero-length file. Lookup must treat that as a miss and
// remove it so the next Store rebuilds.
func TestCacheLookupRejectsZeroByte(t *testing.T) {
	c, err := NewCodeDiskCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := c.CachePath("xyz", "ext4", false)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, hit, err := c.Lookup("xyz", "ext4", false)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if hit {
		t.Errorf("zero-byte sentinel should not count as hit")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("zero-byte sentinel should be cleaned up: stat err = %v", statErr)
	}
}

// TestCacheEvict: explicit eviction removes the cached file.
func TestCacheEvict(t *testing.T) {
	c, err := NewCodeDiskCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "src.ext4")
	if err := os.WriteFile(src, []byte("xx"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Store("h", "ext4", false, src); err != nil {
		t.Fatal(err)
	}
	removed, err := c.Evict("h", "ext4", false)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if !removed {
		t.Errorf("Evict reported false on existing entry")
	}
	// Second evict is a no-op.
	removed, err = c.Evict("h", "ext4", false)
	if err != nil {
		t.Fatalf("Evict2: %v", err)
	}
	if removed {
		t.Errorf("Evict reported true on absent entry")
	}
}

// TestCachePathDistinct: distinct keys must yield distinct paths so
// rw and ro variants of the same hash don't shadow each other.
func TestCachePathDistinct(t *testing.T) {
	c, err := NewCodeDiskCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rw := c.CachePath("h", "ext4", false)
	ro := c.CachePath("h", "ext4", true)
	other := c.CachePath("h", "squashfs", false)
	if rw == ro || rw == other || ro == other {
		t.Errorf("paths should differ: rw=%s ro=%s other=%s", rw, ro, other)
	}
}
