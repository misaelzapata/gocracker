package warmcache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestKey_Deterministic(t *testing.T) {
	in := KeyInput{
		ImageDigest: "sha256:abc",
		KernelHash:  "deadbeef",
		Cmdline:     "console=ttyS0 loglevel=4",
		MemMB:       128,
		VCPUs:       1,
		Arch:        "amd64",
	}
	k1 := Key(in)
	k2 := Key(in)
	if k1 != k2 {
		t.Fatalf("Key not deterministic: %s vs %s", k1, k2)
	}
	if len(k1) != 64 {
		t.Fatalf("expected 64-char hex, got %d chars", len(k1))
	}
}

func TestKey_CmdlineCanonicalised(t *testing.T) {
	base := KeyInput{ImageDigest: "a", KernelHash: "b", MemMB: 128, VCPUs: 1, Arch: "amd64"}
	a := base
	a.Cmdline = "console=ttyS0   loglevel=4"
	b := base
	b.Cmdline = "console=ttyS0 loglevel=4"
	c := base
	c.Cmdline = "  console=ttyS0\tloglevel=4  "
	if Key(a) != Key(b) || Key(b) != Key(c) {
		t.Fatalf("whitespace variants should hash equal: %s %s %s", Key(a), Key(b), Key(c))
	}
	// Order must still matter — reordering tokens changes cmdline semantics.
	d := base
	d.Cmdline = "loglevel=4 console=ttyS0"
	if Key(d) == Key(a) {
		t.Fatal("reordered tokens should NOT hash equal — cmdline order is guest-visible")
	}
}

func TestKey_FieldsMatter(t *testing.T) {
	base := KeyInput{
		ImageDigest: "a", KernelHash: "b", Cmdline: "x", MemMB: 128, VCPUs: 1, Arch: "amd64",
	}
	cases := []struct {
		name   string
		mutate func(*KeyInput)
	}{
		{"ImageDigest", func(k *KeyInput) { k.ImageDigest = "a2" }},
		{"KernelHash", func(k *KeyInput) { k.KernelHash = "b2" }},
		{"Cmdline", func(k *KeyInput) { k.Cmdline = "y" }},
		{"MemMB", func(k *KeyInput) { k.MemMB = 256 }},
		{"VCPUs", func(k *KeyInput) { k.VCPUs = 2 }},
		{"Arch", func(k *KeyInput) { k.Arch = "arm64" }},
	}
	baseKey := Key(base)
	for _, tc := range cases {
		mutated := base
		tc.mutate(&mutated)
		if Key(mutated) == baseKey {
			t.Errorf("changing %s did not change key", tc.name)
		}
	}
}

func TestLookup_MissingDirReturnsFalse(t *testing.T) {
	root := t.TempDir()
	if _, ok := Lookup(root, "nonexistent"); ok {
		t.Fatal("Lookup on missing dir should return ok=false")
	}
}

func TestLookup_IncompleteSnapshotReturnsFalse(t *testing.T) {
	root := t.TempDir()
	key := "abc"
	d := filepath.Join(root, key)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	// Only snapshot.json, missing mem.bin.
	if err := os.WriteFile(filepath.Join(d, "snapshot.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := Lookup(root, key); ok {
		t.Fatal("Lookup with missing mem.bin should return ok=false")
	}

	// Add empty mem.bin — still incomplete.
	if err := os.WriteFile(filepath.Join(d, "mem.bin"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := Lookup(root, key); ok {
		t.Fatal("Lookup with zero-byte mem.bin should return ok=false")
	}
}

func TestStore_AtomicPromote(t *testing.T) {
	root := t.TempDir()
	key := "k1"

	// Build a complete snapshot in a sibling scratch dir.
	src := filepath.Join(t.TempDir(), "snap-staging")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(src, "snapshot.json"), `{"id":"x"}`)
	writeFile(t, filepath.Join(src, "mem.bin"), "RAM")

	if err := Store(src, root, key); err != nil {
		t.Fatalf("Store: %v", err)
	}

	d, ok := Lookup(root, key)
	if !ok {
		t.Fatal("Lookup should hit after Store")
	}
	if d != Dir(root, key) {
		t.Fatalf("dir mismatch: %s vs %s", d, Dir(root, key))
	}
	// src should be gone (moved).
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("src should have been moved, got err=%v", err)
	}
}

func TestStore_FirstWriterWins(t *testing.T) {
	root := t.TempDir()
	key := "same"

	src1 := filepath.Join(t.TempDir(), "src1")
	src2 := filepath.Join(t.TempDir(), "src2")
	for _, d := range []string{src1, src2} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(d, "snapshot.json"), `{}`)
		writeFile(t, filepath.Join(d, "mem.bin"), "x")
	}

	if err := Store(src1, root, key); err != nil {
		t.Fatalf("first Store: %v", err)
	}
	// Second call: first entry still wins, src2 dropped.
	if err := Store(src2, root, key); err != nil {
		t.Fatalf("second Store: %v", err)
	}
	if _, err := os.Stat(src2); !os.IsNotExist(err) {
		t.Fatal("second src should have been cleaned up")
	}
}

func TestStore_RejectsIncompleteSrc(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(t.TempDir(), "incomplete")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(src, "snapshot.json"), `{}`)
	// No mem.bin.
	if err := Store(src, root, "k"); err == nil {
		t.Fatal("Store should reject incomplete src")
	}
}

func TestEvict_RemovesStaleEntries(t *testing.T) {
	root := t.TempDir()

	// Fresh entry.
	fresh := filepath.Join(root, "fresh")
	if err := os.MkdirAll(fresh, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(fresh, "snapshot.json"), "{}")
	writeFile(t, filepath.Join(fresh, "mem.bin"), "x")

	// Stale entry — backdate its mtime.
	stale := filepath.Join(root, "stale")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(stale, "snapshot.json"), "{}")
	writeFile(t, filepath.Join(stale, "mem.bin"), "x")
	old := time.Now().Add(-24 * time.Hour)
	_ = os.Chtimes(stale, old, old)

	// Corrupt entry — no mem.bin. Should always be removed.
	broken := filepath.Join(root, "broken")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(broken, "snapshot.json"), "{}")

	removed, err := Evict(root, 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("expected 2 evictions (stale + broken), got %d", removed)
	}
	if _, ok := Lookup(root, "fresh"); !ok {
		t.Error("fresh entry should survive")
	}
	if _, ok := Lookup(root, "stale"); ok {
		t.Error("stale entry should be gone")
	}
	if _, ok := Lookup(root, "broken"); ok {
		t.Error("broken entry should be gone")
	}
}

func TestEvict_MissingRootNoError(t *testing.T) {
	removed, err := Evict(filepath.Join(t.TempDir(), "does-not-exist"), time.Hour)
	if err != nil {
		t.Fatalf("Evict on missing root should not error: %v", err)
	}
	if removed != 0 {
		t.Fatalf("expected 0, got %d", removed)
	}
}

func TestDefaultRoot_XDGPrecedence(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/tmp/fake-xdg")
	t.Setenv("HOME", "/tmp/fake-home")
	if got := DefaultRoot(); got != "/tmp/fake-xdg/gocracker/snapshots" {
		t.Fatalf("XDG_CACHE_HOME not honoured: %s", got)
	}
	t.Setenv("XDG_CACHE_HOME", "")
	if got := DefaultRoot(); got != "/tmp/fake-home/.cache/gocracker/snapshots" {
		t.Fatalf("HOME fallback wrong: %s", got)
	}
}

func TestHashFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k")
	writeFile(t, p, "kernel bytes")
	h1, err := HashFile(p)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := HashFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 || len(h1) != 64 {
		t.Fatalf("unexpected hash: %q %q", h1, h2)
	}
	// Changing contents changes the hash.
	writeFile(t, p, "different")
	h3, _ := HashFile(p)
	if h3 == h1 {
		t.Fatal("hash should change with content")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
