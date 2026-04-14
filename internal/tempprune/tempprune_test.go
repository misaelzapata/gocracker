package tempprune

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Helper: set the mtime of a directory (and its contents) to an arbitrary time.
func setTreeMtime(t *testing.T, path string, when time.Time) {
	t.Helper()
	_ = filepath.WalkDir(path, func(p string, _ os.DirEntry, err error) error {
		if err == nil {
			_ = os.Chtimes(p, when, when)
		}
		return nil
	})
}

func TestPruneStaleTempDirs_StaleRemovedFreshKept(t *testing.T) {
	// Use a custom TMPDIR so this test doesn't risk touching other
	// gocracker-* dirs the developer may have on their laptop.
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	stale := filepath.Join(tmp, "gocracker-repo-OLDEST")
	fresh := filepath.Join(tmp, "gocracker-repo-FRESH")
	unrelated := filepath.Join(tmp, "some-other-OLDEST")
	for _, p := range []string{stale, fresh, unrelated} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(filepath.Join(p, "payload"), []byte("x"), 0o644); err != nil {
			t.Fatalf("write payload: %v", err)
		}
	}
	setTreeMtime(t, stale, time.Now().Add(-72*time.Hour))
	setTreeMtime(t, unrelated, time.Now().Add(-72*time.Hour))
	// fresh left at current mtime

	res := PruneStaleTempDirs([]string{"gocracker-repo"}, 48*time.Hour)
	if res.Scanned != 2 {
		t.Fatalf("scanned = %d, want 2 (both gocracker-repo-*)", res.Scanned)
	}
	if res.Removed != 1 {
		t.Fatalf("removed = %d, want 1 (only the stale one)", res.Removed)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", res.Errors)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale dir should have been removed, err = %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh dir should have survived, err = %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("unrelated dir should have survived (wrong prefix), err = %v", err)
	}
}

func TestPruneStaleTempDirs_PrefixBoundary(t *testing.T) {
	// Regression: ensure we don't match 'gocracker-repoX-*' when the prefix
	// is 'gocracker-repo'. The prefix+'-' check guards the name boundary.
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	near := filepath.Join(tmp, "gocracker-repository-OLD") // looks like, but isn't
	real := filepath.Join(tmp, "gocracker-repo-REAL")
	for _, p := range []string{near, real} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	setTreeMtime(t, near, time.Now().Add(-72*time.Hour))
	setTreeMtime(t, real, time.Now().Add(-72*time.Hour))

	res := PruneStaleTempDirs([]string{"gocracker-repo"}, 48*time.Hour)
	if res.Removed != 1 {
		t.Fatalf("removed = %d, want 1 — prefix must require name boundary", res.Removed)
	}
	if _, err := os.Stat(near); err != nil {
		t.Fatalf("gocracker-repository-* should NOT be pruned by 'gocracker-repo' prefix, err = %v", err)
	}
	if _, err := os.Stat(real); !os.IsNotExist(err) {
		t.Fatalf("gocracker-repo-REAL should have been pruned, err = %v", err)
	}
}

func TestPruneStaleTempDirs_MultiplePrefixes(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	dirs := map[string]time.Time{
		"gocracker-stages-A":       time.Now().Add(-72 * time.Hour),
		"gocracker-run-cache-B":    time.Now().Add(-72 * time.Hour),
		"gocracker-repo-C":         time.Now().Add(-72 * time.Hour),
		"gocracker-vmm-worker-D":   time.Now().Add(-72 * time.Hour),
		"gocracker-migrate-dst-E":  time.Now().Add(-72 * time.Hour),
		"gocracker-stages-FRESH":   time.Now(),
	}
	for name, when := range dirs {
		p := filepath.Join(tmp, name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		setTreeMtime(t, p, when)
	}

	res := PruneStaleTempDirs(DefaultPrefixes, 48*time.Hour)
	if res.Removed != 5 {
		t.Fatalf("removed = %d, want 5 (all 5 stale)", res.Removed)
	}
	// FRESH must survive
	if _, err := os.Stat(filepath.Join(tmp, "gocracker-stages-FRESH")); err != nil {
		t.Fatalf("fresh stages dir should have survived, err = %v", err)
	}
}

func TestPruneStaleTempDirs_EmptyTmpOK(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	res := PruneStaleTempDirs(DefaultPrefixes, 48*time.Hour)
	if res.Scanned != 0 || res.Removed != 0 || len(res.Errors) != 0 {
		t.Fatalf("empty dir should be a no-op, got %+v", res)
	}
}

func TestPruneStaleTempDirs_BytesFreeAccounted(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	p := filepath.Join(tmp, "gocracker-repo-SIZETEST")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("x", 4096)
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(filepath.Join(p, "file"+string(rune('a'+i))), []byte(payload), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	setTreeMtime(t, p, time.Now().Add(-72*time.Hour))

	res := PruneStaleTempDirs([]string{"gocracker-repo"}, 48*time.Hour)
	if res.BytesFree < int64(3*4096) {
		t.Fatalf("BytesFree = %d, want >= %d", res.BytesFree, 3*4096)
	}
}
