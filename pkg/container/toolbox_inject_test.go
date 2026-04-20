package container

import (
	"os"
	"path/filepath"
	"testing"

	toolboxembed "github.com/gocracker/gocracker/internal/toolbox/embed"
	toolboxspec "github.com/gocracker/gocracker/internal/toolbox/spec"
)

// TestInjectToolboxBinary asserts the binary lands at the exact guest
// path internal/guest/init.go reads from, with executable mode and a
// VERSION sibling. If this test ever skips silently because the embed
// is empty, the build is broken — embed.Binary must be populated by
// `go generate ./internal/toolbox/embed` and committed.
func TestInjectToolboxBinary(t *testing.T) {
	if len(toolboxembed.Binary) == 0 {
		t.Fatal("toolboxembed.Binary is empty — run `go generate ./internal/toolbox/embed` and commit the binaries")
	}

	dir := t.TempDir()
	injectToolboxBinary(dir)

	guestBin := filepath.Join(dir, toolboxspec.BinaryPath)
	info, err := os.Stat(guestBin)
	if err != nil {
		t.Fatalf("stat injected binary: %v", err)
	}
	if int(info.Size()) != len(toolboxembed.Binary) {
		t.Fatalf("size mismatch: got %d, want %d", info.Size(), len(toolboxembed.Binary))
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("binary not executable: mode=%v", info.Mode())
	}

	versionFile := filepath.Join(dir, toolboxspec.VersionFilePath)
	versionData, err := os.ReadFile(versionFile)
	if err != nil {
		t.Fatalf("read VERSION: %v", err)
	}
	if string(versionData) != toolboxspec.Version+"\n" {
		t.Fatalf("VERSION content: got %q, want %q", versionData, toolboxspec.Version+"\n")
	}
}

