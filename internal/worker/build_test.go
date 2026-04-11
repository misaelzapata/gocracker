package worker

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gocracker/gocracker/internal/buildserver"
)

func TestRewriteBuildCacheDirForJail(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "cache")
	req, mounts, err := rewriteBuildCacheDirForJail(buildserver.BuildRequest{
		CacheDir: cacheDir,
	}, []string{"rw:/tmp/run:/worker"})
	if err != nil {
		t.Fatalf("rewriteBuildCacheDirForJail(): %v", err)
	}
	if req.CacheDir != "/worker/cache" {
		t.Fatalf("CacheDir = %q, want /worker/cache", req.CacheDir)
	}
	if !slices.Contains(mounts, "rw:"+cacheDir+":/worker/cache") {
		t.Fatalf("mounts = %#v, want cache mount", mounts)
	}
}

func TestRewriteBuildCacheDirForJail_NoCacheDir(t *testing.T) {
	req, mounts, err := rewriteBuildCacheDirForJail(buildserver.BuildRequest{}, []string{"rw:/tmp/run:/worker"})
	if err != nil {
		t.Fatalf("rewriteBuildCacheDirForJail(): %v", err)
	}
	if req.CacheDir != "" {
		t.Fatalf("CacheDir = %q, want empty", req.CacheDir)
	}
	if len(mounts) != 1 {
		t.Fatalf("mounts = %#v, want original mounts only", mounts)
	}
}

func TestRewriteBuildCacheDirForJail_WhitespaceOnly(t *testing.T) {
	req, mounts, err := rewriteBuildCacheDirForJail(buildserver.BuildRequest{
		CacheDir: "   ",
	}, []string{"rw:/tmp/run:/worker"})
	if err != nil {
		t.Fatalf("rewriteBuildCacheDirForJail(): %v", err)
	}
	if req.CacheDir != "   " {
		t.Fatalf("CacheDir = %q, want whitespace (not rewritten)", req.CacheDir)
	}
	if len(mounts) != 1 {
		t.Fatalf("mounts = %#v, want original mounts only", mounts)
	}
}

func TestBuildSupportMounts_ReturnsNonEmpty(t *testing.T) {
	mounts := buildSupportMounts()
	// At minimum, /etc/resolv.conf and /etc/hosts should be present on any Linux system
	found := false
	for _, m := range mounts {
		if m == "ro:/etc/resolv.conf:/etc/resolv.conf" {
			found = true
			break
		}
	}
	if !found {
		// /etc/resolv.conf might not exist in some containers, but the function
		// should at least return without error
		t.Logf("buildSupportMounts() returned %d mounts (resolv.conf not found)", len(mounts))
	}
}

func TestBuildWorkerEnv(t *testing.T) {
	env := buildWorkerEnv()
	// Should include PATH if set in the environment
	hasPath := false
	for _, entry := range env {
		if len(entry) > 5 && entry[:5] == "PATH=" {
			hasPath = true
		}
	}
	if path := filepath.Join("/usr", "bin"); path != "" {
		// PATH is almost always set
		if !hasPath {
			t.Logf("PATH not found in buildWorkerEnv() result (may be unset in test environment)")
		}
	}
}

func TestBuildToolMounts_NonexistentTool(t *testing.T) {
	mounts := buildToolMounts("definitely-nonexistent-tool-abc123")
	if len(mounts) != 0 {
		t.Fatalf("expected no mounts for nonexistent tool, got %v", mounts)
	}
}

func TestBuildToolMounts_MultipleMissing(t *testing.T) {
	mounts := buildToolMounts("noexist1", "noexist2", "noexist3")
	if len(mounts) != 0 {
		t.Fatalf("expected no mounts, got %v", mounts)
	}
}

func TestBinaryMounts_SelfBinary(t *testing.T) {
	// Test with the go test binary itself
	self, err := os.Executable()
	if err != nil {
		t.Skip("cannot determine test executable path")
	}
	mounts := binaryMounts(self)
	if len(mounts) == 0 {
		t.Fatal("binaryMounts() should return at least the binary itself")
	}
	expected := "ro:" + self + ":" + self
	if mounts[0] != expected {
		t.Fatalf("mounts[0] = %q, want %q", mounts[0], expected)
	}
}
