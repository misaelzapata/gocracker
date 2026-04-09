package worker

import (
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
