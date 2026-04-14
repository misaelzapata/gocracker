package container

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gocracker/gocracker/internal/oci"
	"github.com/gocracker/gocracker/internal/runtimecfg"
	"github.com/gocracker/gocracker/pkg/vmm"
)

// --- sanitizeRuntimePathComponent ---

func TestSanitizeRuntimePathComponentTableDriven(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello-world", "hello-world"},
		{"my vm 1", "my_vm_1"},
		{"", "vm"},
		{"   ", "vm"},
		{"abc/def", "abc_def"},
		{"UPPER.case_ok-123", "UPPER.case_ok-123"},
		{"!@#$%", "_____"},
	}
	for _, tt := range tests {
		got := sanitizeRuntimePathComponent(tt.input)
		if got != tt.want {
			t.Errorf("sanitize(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// --- stableHashKey ---

func TestStableHashKeyDeterministic(t *testing.T) {
	payload := map[string]any{"foo": "bar", "num": 42}
	h1, err := stableHashKey(payload)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := stableHashKey(payload)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatal("should be deterministic")
	}
}

func TestStableHashKeyDifferentInputs(t *testing.T) {
	h1, _ := stableHashKey(map[string]any{"a": 1})
	h2, _ := stableHashKey(map[string]any{"a": 2})
	if h1 == h2 {
		t.Fatal("different inputs should produce different hashes")
	}
}

func TestStableHashKeyLength(t *testing.T) {
	h, err := stableHashKey("test")
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 64 { // sha256 hex
		t.Fatalf("len = %d, want 64", len(h))
	}
}

// --- normalizedStringMap ---

func TestNormalizedStringMapNil(t *testing.T) {
	got := normalizedStringMap(nil)
	if got != nil {
		t.Fatalf("nil input should return nil, got %v", got)
	}
}

func TestNormalizedStringMapEmpty(t *testing.T) {
	got := normalizedStringMap(map[string]string{})
	if got != nil {
		t.Fatalf("empty input should return nil, got %v", got)
	}
}

func TestNormalizedStringMapPreservesValues(t *testing.T) {
	input := map[string]string{"b": "2", "a": "1"}
	got := normalizedStringMap(input)
	if got["a"] != "1" || got["b"] != "2" {
		t.Fatalf("got %v", got)
	}
}

// --- shellQuote ---

func TestShellQuoteEscaping(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"hello", "'hello'"},
		{"it's", "'it'\\''s'"},
		{"", "''"},
		{"path with spaces", "'path with spaces'"},
	}
	for _, tt := range tests {
		got := shellQuote(tt.input)
		if got != tt.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// --- delayedRemoveAll ---

func TestDelayedRemoveAllEmpty(t *testing.T) {
	fn := delayedRemoveAll("", 0)
	fn() // should not panic
}

func TestDelayedRemoveAllImmediate(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "to-remove")
	os.MkdirAll(sub, 0755)
	os.WriteFile(filepath.Join(sub, "file.txt"), []byte("x"), 0644)

	fn := delayedRemoveAll(sub, 0)
	fn()
	// With delay=0, removal is immediate
	if _, err := os.Stat(sub); !os.IsNotExist(err) {
		t.Fatal("directory should have been removed")
	}
}

func TestDelayedRemoveAllIdempotent(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "to-remove2")
	os.MkdirAll(sub, 0755)
	fn := delayedRemoveAll(sub, 0)
	fn()
	fn() // second call should not panic (sync.Once)
}

// --- removeCachedRunArtifacts ---

func TestRemoveCachedRunArtifactsEmpty(t *testing.T) {
	if err := removeCachedRunArtifacts("", "  ", ""); err != nil {
		t.Fatalf("error: %v", err)
	}
}

func TestRemoveCachedRunArtifactsRemovesFiles(t *testing.T) {
	dir := t.TempDir()
	f1 := filepath.Join(dir, "file1")
	f2 := filepath.Join(dir, "file2")
	os.WriteFile(f1, []byte("x"), 0644)
	os.WriteFile(f2, []byte("y"), 0644)

	if err := removeCachedRunArtifacts(f1, f2); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{f1, f2} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Fatalf("%s should have been removed", f)
		}
	}
}

func TestRemoveCachedRunArtifactsNonExistent(t *testing.T) {
	if err := removeCachedRunArtifacts("/nonexistent/file"); err != nil {
		t.Fatalf("should not error for missing files: %v", err)
	}
}

// --- writeImageConfig / readImageConfig ---

func TestWriteAndReadImageConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	cfg := oci.ImageConfig{
		Entrypoint: []string{"/app"},
		Cmd:        []string{"--port", "8080"},
		Env:        []string{"FOO=bar"},
		WorkingDir: "/app",
		User:       "nobody",
	}
	if err := writeImageConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := readImageConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkingDir != "/app" || got.User != "nobody" {
		t.Fatalf("got %+v", got)
	}
}

func TestReadImageConfigMissing(t *testing.T) {
	_, err := readImageConfig("/nonexistent/config.json")
	if err == nil {
		t.Fatal("expected error")
	}
}

// --- writeGuestSpecCache ---

func TestWriteGuestSpecCacheContainsVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spec.json")

	spec := runtimecfg.GuestSpec{
		Process: runtimecfg.Process{Exec: "/app"},
		Env:     []string{"FOO=bar"},
	}
	if err := writeGuestSpecCache(path, spec); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Version != 1 {
		t.Fatalf("version = %d", payload.Version)
	}
}

// --- writeRuntimeSpecToRootfs ---

func TestWriteRuntimeSpecToRootfsEmpty(t *testing.T) {
	// Empty spec should not write anything
	dir := t.TempDir()
	err := writeRuntimeSpecToRootfs(dir, runtimecfg.GuestSpec{})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
}

func TestWriteRuntimeSpecToRootfsWithData(t *testing.T) {
	dir := t.TempDir()
	spec := runtimecfg.GuestSpec{
		Process: runtimecfg.Process{Exec: "/bin/sh"},
		Env:     []string{"PATH=/usr/bin"},
	}
	err := writeRuntimeSpecToRootfs(dir, spec)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	// Check file was created
	path := filepath.Join(dir, strings.TrimPrefix(runtimecfg.GuestSpecPath, "/"))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("spec file not created: %v", err)
	}
}

// --- inspectCachedRunArtifacts ---

func TestInspectCachedRunArtifactsMissing(t *testing.T) {
	dir := t.TempDir()
	_, ok, reason, err := inspectCachedRunArtifacts(
		filepath.Join(dir, "missing.ext4"),
		filepath.Join(dir, "config.json"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("should not be ok when disk missing")
	}
	if reason != "disk image missing" {
		t.Fatalf("reason = %q", reason)
	}
}

func TestInspectCachedRunArtifactsNoConfig(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.ext4")
	os.WriteFile(disk, []byte("disk"), 0644)

	_, ok, reason, err := inspectCachedRunArtifacts(disk, filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("should not be ok when config missing")
	}
	if reason != "image config missing" {
		t.Fatalf("reason = %q", reason)
	}
}

func TestInspectCachedRunArtifactsValid(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.ext4")
	config := filepath.Join(dir, "config.json")
	os.WriteFile(disk, []byte("disk"), 0644)

	cfg := oci.ImageConfig{WorkingDir: "/app"}
	data, _ := json.Marshal(cfg)
	os.WriteFile(config, data, 0644)

	got, ok, _, err := inspectCachedRunArtifacts(disk, config)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("should be ok")
	}
	if got.WorkingDir != "/app" {
		t.Fatalf("workdir = %q", got.WorkingDir)
	}
}

// --- copyDiskImage ---

func TestCopyDiskImagePreservesContent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.ext4")
	dst := filepath.Join(dir, "dst.ext4")
	content := []byte("fake disk image content here")
	if err := os.WriteFile(src, content, 0644); err != nil {
		t.Fatal(err)
	}
	if err := copyDiskImage(src, dst, false); err != nil {
		t.Fatalf("copyDiskImage: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatal("content mismatch")
	}
}

func TestCopyDiskImageMissingSrcErrors(t *testing.T) {
	err := copyDiskImage("/nonexistent", "/tmp/dst", false)
	if err == nil {
		t.Fatal("expected error")
	}
}

// --- firstNonNegative ---

func TestFirstNonNegative(t *testing.T) {
	tests := []struct {
		values []int
		want   int
	}{
		{[]int{-1, -2, 5}, 5},
		{[]int{3, -1}, 3},
		{[]int{-1, -1}, 0},
		{[]int{0, 1}, 0},
	}
	for _, tt := range tests {
		got := firstNonNegative(tt.values...)
		if got != tt.want {
			t.Errorf("firstNonNegative(%v) = %d, want %d", tt.values, got, tt.want)
		}
	}
}

// --- effectiveSlice ---

func TestEffectiveSliceOverrideAndFallback(t *testing.T) {
	override := []string{"a", "b"}
	fallback := []string{"x", "y"}
	got := effectiveSlice(override, fallback)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("with override = %v", got)
	}
	got = effectiveSlice(nil, fallback)
	if len(got) != 2 || got[0] != "x" {
		t.Fatalf("with fallback = %v", got)
	}
}

// --- buildExecConfig ---

func TestBuildExecConfigDisabled(t *testing.T) {
	got := buildExecConfig(RunOptions{})
	if got != nil {
		t.Fatal("should be nil when exec not enabled")
	}
}

func TestBuildExecConfigEnabled(t *testing.T) {
	got := buildExecConfig(RunOptions{ExecEnabled: true})
	if got == nil {
		t.Fatal("should not be nil")
	}
	if !got.Enabled {
		t.Fatal("Enabled should be true")
	}
}

func TestBuildExecConfigBalloonNeedsAgent(t *testing.T) {
	got := buildExecConfig(RunOptions{
		Balloon: &vmm.BalloonConfig{Auto: vmm.BalloonAutoConservative},
	})
	if got == nil || !got.Enabled {
		t.Fatal("balloon auto should force exec config")
	}
}

// --- cloneBalloonConfig ---

func TestCloneBalloonConfigNil(t *testing.T) {
	if cloneBalloonConfig(nil) != nil {
		t.Fatal("nil should clone to nil")
	}
}

func TestCloneBalloonConfigDeepCopy(t *testing.T) {
	cfg := &vmm.BalloonConfig{
		AmountMiB:     64,
		SnapshotPages: []uint32{1, 2, 3},
	}
	cloned := cloneBalloonConfig(cfg)
	cloned.AmountMiB = 128
	cloned.SnapshotPages[0] = 99
	if cfg.AmountMiB != 64 {
		t.Fatal("original modified")
	}
	if cfg.SnapshotPages[0] != 1 {
		t.Fatal("original snapshot pages modified")
	}
}


// --- guestAgentRequired ---

func TestGuestAgentRequiredCases(t *testing.T) {
	if guestAgentRequired(nil, nil) {
		t.Fatal("nil nil should not require agent")
	}
	if !guestAgentRequired(nil, &vmm.MemoryHotplugConfig{}) {
		t.Fatal("memory hotplug should require agent")
	}
	if !guestAgentRequired(&vmm.BalloonConfig{StatsPollingIntervalS: 1}, nil) {
		t.Fatal("balloon stats should require agent")
	}
}

// --- rootfsPath ---

func TestRootfsPathWithTraversal(t *testing.T) {
	tests := []struct {
		rootfs, path, want string
	}{
		{"/mnt/rootfs", "/usr/bin", "/mnt/rootfs/usr/bin"},
		{"/mnt/rootfs", "usr/bin", "/mnt/rootfs/usr/bin"},
		{"/mnt/rootfs", "/", "/mnt/rootfs"},
		{"/mnt/rootfs", ".", "/mnt/rootfs"},
		{"/mnt/rootfs", "/a/../b", "/mnt/rootfs/b"},
	}
	for _, tt := range tests {
		got := rootfsPath(tt.rootfs, tt.path)
		if got != tt.want {
			t.Errorf("rootfsPath(%q, %q) = %q, want %q", tt.rootfs, tt.path, got, tt.want)
		}
	}
}
