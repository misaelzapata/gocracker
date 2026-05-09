package paths

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestCacheDirIsAbsolute is the only platform-neutral invariant: the cache
// directory must be an absolute path so callers can chdir freely without
// breaking artifact resolution. The actual location depends on os.TempDir()
// which is platform-specific; we don't pin it.
func TestCacheDirIsAbsolute(t *testing.T) {
	got := CacheDir()
	if !filepath.IsAbs(got) {
		t.Fatalf("CacheDir() = %q; want absolute path", got)
	}
	// Also enforce the gocracker namespace so we don't accidentally
	// collide with another tool's cache when cleaning up.
	if !strings.Contains(got, "gocracker") {
		t.Fatalf("CacheDir() = %q; want path containing 'gocracker'", got)
	}
}

// TestPathsContainGocracker locks down the namespace invariant for every
// path the package exposes — operators reasonably expect a path with
// "gocracker" in it so they can spot stale state, snapshots, sockets etc.
// without grepping multiple namespaces. The tempprune sweep on serve
// startup also depends on this prefix.
func TestPathsContainGocracker(t *testing.T) {
	cases := map[string]string{
		"APISocket":     APISocket(),
		"VMMSocket":     VMMSocket(),
		"BuildSocket":   BuildSocket(),
		"ServeStateDir": ServeStateDir(),
		"SnapshotsDir":  SnapshotsDir(),
		"CacheDir":      CacheDir(),
		"JailerBaseDir": JailerBaseDir(),
		"ToolboxLog":    ToolboxLog(),
	}
	for name, got := range cases {
		// JailerBaseDir on Linux is "/srv/jailer" which intentionally
		// does NOT contain "gocracker" — that path predates this
		// package and is what operators script against. Skip the
		// invariant for that one entry.
		if name == "JailerBaseDir" && runtime.GOOS == "linux" {
			continue
		}
		if !strings.Contains(strings.ToLower(got), "gocracker") {
			t.Errorf("%s() = %q; want path containing 'gocracker'", name, got)
		}
	}
}

// TestLinuxPathsAreByteIdentical pins down that the Linux defaults remain
// byte-identical to the literals they replaced. Operators may have
// scripts, systemd units, or docs that reference these exact paths — a
// silent change here would break them.
func TestLinuxPathsAreByteIdentical(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-specific path invariant")
	}
	cases := map[string]struct {
		got, want string
	}{
		"APISocket":       {APISocket(), "/tmp/gocracker.sock"},
		"VMMSocket":       {VMMSocket(), "/tmp/gocracker-vmm.sock"},
		"BuildSocket":     {BuildSocket(), "/tmp/gocracker-build.sock"},
		"ServeStateDir":   {ServeStateDir(), "/tmp/gocracker-serve-state"},
		"SnapshotsDir":    {SnapshotsDir(), "/tmp/gocracker-snapshots"},
		"JailerBaseDir":   {JailerBaseDir(), "/srv/jailer"},
		"ToolboxLog":      {ToolboxLog(), "/tmp/gocracker-toolbox.log"},
		"GitConfigGlobal": {GitConfigGlobal(), "/tmp/gocracker-gitconfig"},
	}
	for name, c := range cases {
		if c.got != c.want {
			t.Errorf("%s(): got %q want %q (Linux defaults must stay byte-identical)", name, c.got, c.want)
		}
	}
}

// TestTempPrefix anchors the tempprune sweep contract. cmd/gocracker
// scans for orphaned directories matching this prefix when serve starts.
func TestTempPrefix(t *testing.T) {
	if got := TempPrefix(); got != "gocracker-" {
		t.Fatalf("TempPrefix() = %q; want %q", got, "gocracker-")
	}
}
