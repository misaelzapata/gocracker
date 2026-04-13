package vmm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- VM methods on minimal struct (no KVM required) ---

func TestVM_Start_StoppedState(t *testing.T) {
	vm := &VM{
		state:  StateStopped,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
		events: NewEventLog(),
	}
	err := vm.Start()
	if err == nil {
		t.Fatal("expected error starting stopped VM")
	}
	if !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestVM_Stop_WhenAlreadyStopped(t *testing.T) {
	ch := make(chan struct{})
	close(ch)
	vm := &VM{
		state:  StateStopped,
		stopCh: ch,
		doneCh: make(chan struct{}),
		events: NewEventLog(),
	}
	// Should not panic
	vm.Stop()
}

func TestVM_UpdateNetRateLimiter_NoDevice(t *testing.T) {
	vm := &VM{}
	err := vm.UpdateNetRateLimiter(nil)
	if err == nil {
		t.Fatal("expected error with no net device")
	}
}

func TestVM_UpdateBlockRateLimiter_NoDevice(t *testing.T) {
	vm := &VM{}
	err := vm.UpdateBlockRateLimiter(nil)
	if err == nil {
		t.Fatal("expected error with no block device")
	}
}

func TestVM_UpdateRNGRateLimiter_NoDevice(t *testing.T) {
	vm := &VM{}
	err := vm.UpdateRNGRateLimiter(nil)
	if err == nil {
		t.Fatal("expected error with no rng device")
	}
}

func TestVM_GetBalloonConfig_NoDevice(t *testing.T) {
	vm := &VM{}
	_, err := vm.GetBalloonConfig()
	if err == nil {
		t.Fatal("expected error with no balloon device")
	}
}

func TestVM_UpdateBalloon_NoDevice(t *testing.T) {
	vm := &VM{}
	err := vm.UpdateBalloon(BalloonUpdate{AmountMiB: 64})
	if err == nil {
		t.Fatal("expected error with no balloon device")
	}
}

func TestVM_GetBalloonStats_NoDevice(t *testing.T) {
	vm := &VM{}
	_, err := vm.GetBalloonStats()
	if err == nil {
		t.Fatal("expected error with no balloon device")
	}
}

func TestVM_UpdateBalloonStats_NoBalloonDevice(t *testing.T) {
	vm := &VM{}
	err := vm.UpdateBalloonStats(BalloonStatsUpdate{StatsPollingIntervalS: 5})
	if err == nil {
		t.Fatal("expected error with no balloon device")
	}
}

func TestVM_DialVsock_NoVsockDevice(t *testing.T) {
	vm := &VM{state: StateRunning}
	_, err := vm.DialVsock(52)
	if err == nil {
		t.Fatal("expected error with no vsock device")
	}
}

func TestVM_DialVsock_StoppedReportsNotConfigured(t *testing.T) {
	// With no vsock device, the "not configured" error takes priority
	vm := &VM{state: StateStopped}
	_, err := vm.DialVsock(52)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestVM_State(t *testing.T) {
	vm := &VM{state: StateRunning}
	if vm.State() != StateRunning {
		t.Fatalf("State() = %v", vm.State())
	}
}

func TestVM_WaitStoppedCancellation(t *testing.T) {
	vm := &VM{doneCh: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := vm.WaitStopped(ctx)
	if err == nil {
		t.Fatal("expected context error")
	}
}

// --- ApplyMigrationPatches ---

func TestApplyMigrationPatchesNoPatchFile(t *testing.T) {
	dir := t.TempDir()
	// No patches.json -> should be a no-op
	if err := ApplyMigrationPatches(dir); err != nil {
		t.Fatalf("error: %v", err)
	}
}

func TestApplyMigrationPatchesEmptyPatches(t *testing.T) {
	dir := t.TempDir()
	patchSet := MigrationPatchSet{Version: 1, Patches: nil}
	data, _ := json.MarshalIndent(patchSet, "", "  ")
	os.WriteFile(filepath.Join(dir, migrationPatchMeta), data, 0644)

	if err := ApplyMigrationPatches(dir); err != nil {
		t.Fatalf("error: %v", err)
	}
}

func TestApplyMigrationPatchesAppliesPatches(t *testing.T) {
	dir := t.TempDir()

	// Create a target file
	target := filepath.Join(dir, "mem.bin")
	original := []byte("AAAABBBBCCCCDDDD")
	os.WriteFile(target, original, 0644)

	// Write patch data: replace offset 4, length 4 with "XXXX"
	patchData := []byte("XXXX")
	os.WriteFile(filepath.Join(dir, migrationPatchData), patchData, 0644)

	patchSet := MigrationPatchSet{
		Version: 1,
		Patches: []DirtyFilePatch{
			{
				Path:     "mem.bin",
				PageSize: 4,
				Entries: []DirtyPatchEntry{
					{Offset: 4, Length: 4, DataOffset: 0},
				},
			},
		},
	}
	data, _ := json.MarshalIndent(patchSet, "", "  ")
	os.WriteFile(filepath.Join(dir, migrationPatchMeta), data, 0644)

	if err := ApplyMigrationPatches(dir); err != nil {
		t.Fatalf("error: %v", err)
	}

	got, _ := os.ReadFile(target)
	want := []byte("AAAAXXXXCCCCDDDD")
	if !bytes.Equal(got, want) {
		t.Fatalf("patched = %q, want %q", got, want)
	}
}

// --- copyReaderAtRange ---

func TestCopyReaderAtRangeMiddle(t *testing.T) {
	src := bytes.NewReader([]byte("0123456789ABCDEF"))
	var dst bytes.Buffer
	if err := copyReaderAtRange(&dst, src, 4, 8); err != nil {
		t.Fatal(err)
	}
	if got := dst.String(); got != "456789AB" {
		t.Fatalf("got %q, want 456789AB", got)
	}
}

func TestCopyReaderAtRangeZeroLengthNoop(t *testing.T) {
	src := bytes.NewReader([]byte("data"))
	var dst bytes.Buffer
	if err := copyReaderAtRange(&dst, src, 0, 0); err != nil {
		t.Fatal(err)
	}
	if dst.Len() != 0 {
		t.Fatal("expected empty output")
	}
}

// --- mergeDirtyBitmaps ---

func TestMergeDirtyBitmapsEmpty(t *testing.T) {
	got := mergeDirtyBitmaps(nil, nil)
	if got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestMergeDirtyBitmapsMerges(t *testing.T) {
	a := []uint64{0b0101, 0b0000}
	b := []uint64{0b1010, 0b1111, 0b0001}
	got := mergeDirtyBitmaps(a, b)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0] != 0b1111 {
		t.Fatalf("got[0] = %b, want 1111", got[0])
	}
	if got[1] != 0b1111 {
		t.Fatalf("got[1] = %b, want 1111", got[1])
	}
	if got[2] != 0b0001 {
		t.Fatalf("got[2] = %b, want 0001", got[2])
	}
}

// --- bundleAsset ---

func TestBundleAssetEmptyPath(t *testing.T) {
	got, err := bundleAsset(t.TempDir(), "", "artifacts/kernel")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestBundleAssetCopiesFile(t *testing.T) {
	dir := t.TempDir()
	srcDir := t.TempDir()
	srcFile := filepath.Join(srcDir, "vmlinuz")
	os.WriteFile(srcFile, []byte("kernel-content"), 0644)

	got, err := bundleAsset(dir, srcFile, "artifacts/kernel")
	if err != nil {
		t.Fatal(err)
	}
	if got != "artifacts/kernel" {
		t.Fatalf("got %q", got)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "artifacts/kernel"))
	if string(data) != "kernel-content" {
		t.Fatalf("content = %q", data)
	}
}

func TestBundleAssetSameFile(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "artifacts"), 0755)
	dstFile := filepath.Join(dir, "artifacts/kernel")
	os.WriteFile(dstFile, []byte("kernel"), 0644)

	// When source is same as destination, should not re-copy
	got, err := bundleAsset(dir, dstFile, "artifacts/kernel")
	if err != nil {
		t.Fatal(err)
	}
	if got != "artifacts/kernel" {
		t.Fatalf("got %q", got)
	}
}

// --- copyFile ---

func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "dst.bin")
	os.WriteFile(src, []byte("hello"), 0644)

	if err := copyFile(dst, src); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "hello" {
		t.Fatalf("content = %q", got)
	}
}

func TestCopyFileMissingSrc(t *testing.T) {
	if err := copyFile("/tmp/dst", "/nonexistent"); err == nil {
		t.Fatal("expected error")
	}
}

// --- sameFilePath ---

func TestSameFilePathCleansAndCompares(t *testing.T) {
	if !sameFilePath("/a/b/../c", "/a/c") {
		t.Fatal("should match after Clean")
	}
	if sameFilePath("/a/b", "/a/c") {
		t.Fatal("should not match")
	}
}

// --- buildRateLimiter ---

func TestBuildRateLimiterNil(t *testing.T) {
	got := buildRateLimiter(nil)
	if got != nil {
		t.Fatal("nil input should return nil")
	}
}

func TestBuildRateLimiterNonNil(t *testing.T) {
	got := buildRateLimiter(&RateLimiterConfig{
		Bandwidth: TokenBucketConfig{Size: 1000, RefillTimeMs: 100},
	})
	if got == nil {
		t.Fatal("non-nil input should return non-nil")
	}
}

// --- EventLog additional coverage ---

func TestEventLog_SubscribeMultipleEmitters(t *testing.T) {
	el := NewEventLog()
	ch, unsub := el.Subscribe()
	defer unsub()

	var wg sync.WaitGroup
	wg.Add(3)
	for i := 0; i < 3; i++ {
		go func() {
			defer wg.Done()
			el.Emit(EventRunning, "test")
		}()
	}
	wg.Wait()

	// Drain at least one event
	select {
	case ev := <-ch:
		if ev.Type != EventRunning {
			t.Fatalf("got %q", ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestEventLog_EventsFiltersCorrectly(t *testing.T) {
	el := NewEventLog()
	el.Emit(EventCreated, "1")
	time.Sleep(2 * time.Millisecond)
	since := time.Now()
	time.Sleep(2 * time.Millisecond)
	el.Emit(EventRunning, "2")
	el.Emit(EventStopped, "3")

	events := el.Events(since)
	if len(events) != 2 {
		t.Fatalf("got %d events since, want 2", len(events))
	}
}

// --- writeMemoryFile ---

func TestWriteMemoryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mem.bin")
	mem := []byte("fake memory data here 123456789")
	if err := writeMemoryFile(path, mem); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, mem) {
		t.Fatal("content mismatch")
	}
}

// --- buildDirtyFilePatch edge cases ---

func TestBuildDirtyFilePatchEmptyBitmap(t *testing.T) {
	var out bytes.Buffer
	patch, err := buildDirtyFilePatch(&out, bytes.NewReader([]byte("data")), 4, "f", 4096, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(patch.Entries) != 0 {
		t.Fatal("expected no entries")
	}
}

func TestBuildDirtyFilePatchZeroSrcSize(t *testing.T) {
	var out bytes.Buffer
	patch, err := buildDirtyFilePatch(&out, bytes.NewReader(nil), 0, "f", 4096, []uint64{0xFF}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(patch.Entries) != 0 {
		t.Fatal("expected no entries for zero src size")
	}
}

func TestBuildDirtyFilePatchZeroPageSize(t *testing.T) {
	src := []byte("AAAABBBB")
	var out bytes.Buffer
	var next uint64
	patch, err := buildDirtyFilePatch(&out, bytes.NewReader(src), 8, "f", 0, []uint64{0b01}, &next)
	if err != nil {
		t.Fatal(err)
	}
	// pageSize=0 should default to 4096, but our src is only 8 bytes
	// so first page at offset 0 with length min(4096, 8) = 8
	if patch.PageSize != 4096 {
		t.Fatalf("pageSize = %d, want 4096", patch.PageSize)
	}
}

// --- Snapshot readSnapshot ---

func TestReadSnapshotMissing(t *testing.T) {
	_, err := readSnapshot("/nonexistent")
	if err == nil {
		t.Fatal("expected error for missing directory")
	}
}

func TestReadSnapshotValid(t *testing.T) {
	dir := t.TempDir()
	snap := Snapshot{
		Version: 3,
		ID:      "test-id",
		Config:  Config{MemMB: 256, ID: "test-id"},
		MemFile: "mem.bin",
	}
	data, _ := json.MarshalIndent(snap, "", "  ")
	os.WriteFile(filepath.Join(dir, "snapshot.json"), data, 0644)

	got, err := readSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "test-id" || got.Version != 3 {
		t.Fatalf("got %+v", got)
	}
}

// --- resolveSnapshotPath ---

func TestResolveSnapshotPathRelative(t *testing.T) {
	got := resolveSnapshotPath("/base/dir", "relative/file")
	if got != filepath.Join("/base/dir", "relative/file") {
		t.Fatalf("got %q", got)
	}
}

func TestResolveSnapshotPathAbsolute(t *testing.T) {
	got := resolveSnapshotPath("/base/dir", "/absolute/file")
	if got != "/absolute/file" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveSnapshotPathEmpty(t *testing.T) {
	got := resolveSnapshotPath("/base", "")
	if got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

// --- rewriteSnapshotBundleWithConfig ---

func TestRewriteSnapshotBundleWithConfigUpdatesConfig(t *testing.T) {
	dir := t.TempDir()
	// Create required assets
	os.WriteFile(filepath.Join(dir, "mem.bin"), []byte("mem"), 0600)
	kernelDir := t.TempDir()
	kernelPath := filepath.Join(kernelDir, "vmlinuz")
	os.WriteFile(kernelPath, []byte("kernel"), 0644)

	snap := Snapshot{
		Version: 3,
		ID:      "test",
		Config: Config{
			ID:         "test",
			KernelPath: kernelPath,
		},
		MemFile: "mem.bin",
	}

	got, err := rewriteSnapshotBundleWithConfig(dir, snap, Config{
		ID:         "test",
		KernelPath: kernelPath,
		MemMB:      512,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Config.MemMB != 512 {
		t.Fatalf("MemMB = %d", got.Config.MemMB)
	}
	if got.Config.KernelPath != "artifacts/kernel" {
		t.Fatalf("KernelPath = %q", got.Config.KernelPath)
	}
}

// --- waitIfPaused helper (test through Pause on minimal VM) ---

func TestVM_Pause_CreatedState(t *testing.T) {
	vm := &VM{
		state:       StateCreated,
		events:      NewEventLog(),
		pausedVCPUs: make(map[int]struct{}),
	}
	err := vm.Pause()
	if err == nil {
		t.Fatal("expected error pausing created VM")
	}
}

func TestVM_Pause_AlreadyPausedNoError(t *testing.T) {
	vm := &VM{
		state:       StatePaused,
		events:      NewEventLog(),
		pausedVCPUs: make(map[int]struct{}),
	}
	err := vm.Pause()
	if err != nil {
		t.Fatalf("re-pause should not error: %v", err)
	}
}

// --- errorsIsNotExist ---

func TestErrorsIsNotExistCases(t *testing.T) {
	if !errorsIsNotExist(os.ErrNotExist) {
		t.Fatal("should recognize ErrNotExist")
	}
	if errorsIsNotExist(nil) {
		t.Fatal("nil should not be NotExist")
	}
	if errorsIsNotExist(io.EOF) {
		t.Fatal("EOF should not be NotExist")
	}
}
