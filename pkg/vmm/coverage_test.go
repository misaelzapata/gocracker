package vmm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// --- BootTimings.Sum() ---

func TestBootTimings_Sum_AllPhases(t *testing.T) {
	bt := BootTimings{
		Orchestration:    100 * time.Millisecond,
		VMMSetup:         200 * time.Millisecond,
		Start:            5 * time.Millisecond,
		GuestFirstOutput: 50 * time.Millisecond,
	}
	result := bt.Sum()
	want := 355 * time.Millisecond
	if result.Total != want {
		t.Fatalf("Sum().Total = %v, want %v", result.Total, want)
	}
	// Original should be unmodified (value receiver)
	if bt.Total != 0 {
		t.Fatalf("original Total should be 0, got %v", bt.Total)
	}
}

func TestBootTimings_Sum_ZeroValues(t *testing.T) {
	result := BootTimings{}.Sum()
	if result.Total != 0 {
		t.Fatalf("Sum() of zero timings = %v, want 0", result.Total)
	}
}

func TestBootTimings_Sum_PreservesPhases(t *testing.T) {
	bt := BootTimings{
		Orchestration:    1 * time.Second,
		VMMSetup:         2 * time.Second,
		Start:            3 * time.Second,
		GuestFirstOutput: 4 * time.Second,
	}
	result := bt.Sum()
	if result.Orchestration != 1*time.Second {
		t.Fatalf("Orchestration = %v", result.Orchestration)
	}
	if result.VMMSetup != 2*time.Second {
		t.Fatalf("VMMSetup = %v", result.VMMSetup)
	}
	if result.Start != 3*time.Second {
		t.Fatalf("Start = %v", result.Start)
	}
	if result.GuestFirstOutput != 4*time.Second {
		t.Fatalf("GuestFirstOutput = %v", result.GuestFirstOutput)
	}
	if result.Total != 10*time.Second {
		t.Fatalf("Total = %v, want 10s", result.Total)
	}
}

func TestBootTimings_Sum_OverwritesExistingTotal(t *testing.T) {
	bt := BootTimings{
		Orchestration: 100 * time.Millisecond,
		VMMSetup:      200 * time.Millisecond,
		Total:         999 * time.Second, // stale value
	}
	result := bt.Sum()
	if result.Total != 300*time.Millisecond {
		t.Fatalf("Total = %v, want 300ms (should overwrite stale)", result.Total)
	}
}

// --- EventLog concurrent operations ---

func TestEventLog_ConcurrentEmitAndEvents(t *testing.T) {
	el := NewEventLog()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				el.Emit(EventRunning, "concurrent")
			}
		}()
	}
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = el.Events(time.Time{})
			}
		}()
	}
	wg.Wait()
	events := el.Events(time.Time{})
	if len(events) == 0 {
		t.Fatal("expected events after concurrent emission")
	}
	if len(events) > maxEvents {
		t.Fatalf("events exceed max: %d > %d", len(events), maxEvents)
	}
}

func TestEventLog_SubscribeReceivesOnlyNewEvents(t *testing.T) {
	el := NewEventLog()
	el.Emit(EventCreated, "before subscribe")

	ch, unsub := el.Subscribe()
	defer unsub()

	el.Emit(EventRunning, "after subscribe")

	select {
	case ev := <-ch:
		if ev.Type != EventRunning {
			t.Fatalf("got %q, want %q", ev.Type, EventRunning)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestEventLog_UnsubscribeIdempotent(t *testing.T) {
	el := NewEventLog()
	_, unsub := el.Subscribe()
	unsub()
	// Calling again should not panic
	unsub()
}

func TestEventLog_MultipleUnsubscribesDontCorrupt(t *testing.T) {
	el := NewEventLog()
	ch1, unsub1 := el.Subscribe()
	_, unsub2 := el.Subscribe()
	ch3, unsub3 := el.Subscribe()

	unsub2() // remove middle subscriber

	el.Emit(EventRunning, "after middle unsub")

	got1 := <-ch1
	got3 := <-ch3
	if got1.Type != EventRunning || got3.Type != EventRunning {
		t.Fatalf("remaining subscribers should receive: %q, %q", got1.Type, got3.Type)
	}

	unsub1()
	unsub3()
}

// --- ExecAgentBroker ---

func TestExecAgentBroker_CloseBlocksListen(t *testing.T) {
	broker := newExecAgentBroker(52)
	broker.close()

	// After close, listen should eventually return an error.
	// The select may non-deterministically succeed on the conns channel
	// if both cases are ready, so we retry.
	var gotErr bool
	for i := 0; i < 5; i++ {
		_, err := broker.listen(52)
		if err != nil {
			gotErr = true
			break
		}
		// drain any connection that landed
		select {
		case <-broker.conns:
		default:
		}
	}
	if !gotErr {
		t.Fatal("expected error after close within 5 attempts")
	}
}

func TestExecAgentBroker_CloseBlocksAcquire(t *testing.T) {
	broker := newExecAgentBroker(52)
	broker.close()

	_, err := broker.acquire()
	if err == nil {
		t.Fatal("expected error after close")
	}
}

func TestExecAgentBroker_CloseIdempotent(t *testing.T) {
	broker := newExecAgentBroker(52)
	broker.close()
	broker.close() // should not panic
}

func TestExecAgentBroker_ListenBacklogFull(t *testing.T) {
	broker := newExecAgentBroker(52)
	defer broker.close()

	// Fill the backlog (capacity 1)
	_, err := broker.listen(52)
	if err != nil {
		t.Fatalf("first listen: %v", err)
	}

	// Second listen should fail with backlog full
	_, err = broker.listen(52)
	if err == nil {
		t.Fatal("expected error for full backlog")
	}
}

// --- VM state method tests (using minimal VM structs, no KVM) ---

func TestVM_Uptime_NotStarted(t *testing.T) {
	vm := &VM{}
	if vm.Uptime() != 0 {
		t.Fatalf("Uptime before start = %v, want 0", vm.Uptime())
	}
}

func TestVM_Uptime_Running(t *testing.T) {
	vm := &VM{startTime: time.Now().Add(-5 * time.Second)}
	up := vm.Uptime()
	if up < 4*time.Second || up > 10*time.Second {
		t.Fatalf("Uptime() = %v, expected ~5s", up)
	}
}

func TestVM_VMConfig_ReturnsConfig(t *testing.T) {
	vm := &VM{cfg: Config{MemMB: 512, ID: "cfg-test"}}
	cfg := vm.VMConfig()
	if cfg.MemMB != 512 || cfg.ID != "cfg-test" {
		t.Fatalf("VMConfig() mismatch: %+v", cfg)
	}
}

func TestVM_Events_ReturnsEventLog(t *testing.T) {
	el := NewEventLog()
	vm := &VM{events: el}
	if vm.Events() != el {
		t.Fatal("Events() should return the VM's event log")
	}
}

func TestVM_DeviceList_NilBackend(t *testing.T) {
	vm := &VM{}
	if devices := vm.DeviceList(); devices != nil {
		t.Fatalf("DeviceList() with nil backend = %v, want nil", devices)
	}
}

func TestVM_ConsoleOutput_NilBackend(t *testing.T) {
	vm := &VM{}
	if output := vm.ConsoleOutput(); output != nil {
		t.Fatalf("ConsoleOutput() with nil backend = %v, want nil", output)
	}
}

func TestVM_FirstOutputAt_NilUART(t *testing.T) {
	vm := &VM{}
	if at := vm.FirstOutputAt(); !at.IsZero() {
		t.Fatalf("FirstOutputAt() with nil uart = %v, want zero", at)
	}
}

func TestVM_UpdateMemoryHotplug_ExecNotEnabled(t *testing.T) {
	vm := &VM{
		memoryHotplug: &memoryHotplugState{
			totalBytes: 4096 << 20,
			blockBytes: 128 << 20,
		},
		cfg: Config{},
	}
	err := vm.UpdateMemoryHotplug(MemoryHotplugSizeUpdate{RequestedSizeMiB: 256})
	if err == nil {
		t.Fatal("expected error when exec not enabled")
	}
}

func TestVM_UpdateMemoryHotplug_InvalidUpdate(t *testing.T) {
	vm := &VM{
		memoryHotplug: &memoryHotplugState{
			totalBytes: 1 << 30,
			blockBytes: 128 << 20,
		},
		cfg: Config{Exec: &ExecConfig{Enabled: true, VsockPort: 52}},
	}
	// Exceeds total
	err := vm.UpdateMemoryHotplug(MemoryHotplugSizeUpdate{RequestedSizeMiB: 2048})
	if err == nil {
		t.Fatal("expected error for exceeds total")
	}
}

func TestVM_Start_WrongState(t *testing.T) {
	vm := &VM{
		state:  StateStopped,
		events: NewEventLog(),
	}
	err := vm.Start()
	if err == nil {
		t.Fatal("expected error starting stopped VM")
	}
}

func TestVM_Start_FromCreated(t *testing.T) {
	vm := &VM{
		state:  StateCreated,
		events: NewEventLog(),
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	err := vm.Start()
	if err != nil {
		t.Fatalf("Start() from created = %v", err)
	}
	if vm.State() != StateRunning {
		t.Fatalf("state after start = %v, want Running", vm.State())
	}
	if vm.startTime.IsZero() {
		t.Fatal("startTime should be set after Start()")
	}
}

func TestVM_Pause_WrongState(t *testing.T) {
	vm := &VM{
		state:  StateCreated,
		events: NewEventLog(),
	}
	err := vm.Pause()
	if err == nil {
		t.Fatal("expected error pausing created VM")
	}
}

func TestVM_Pause_AlreadyPaused(t *testing.T) {
	vm := &VM{
		state:  StatePaused,
		events: NewEventLog(),
	}
	err := vm.Pause()
	if err != nil {
		t.Fatalf("pause on already paused VM should be nil, got %v", err)
	}
}

func TestVM_Resume_WrongState(t *testing.T) {
	vm := &VM{
		state:  StateRunning,
		events: NewEventLog(),
	}
	err := vm.Resume()
	if err == nil {
		t.Fatal("expected error resuming running VM")
	}
}

func TestVM_Resume_FromPaused(t *testing.T) {
	vm := &VM{
		state:       StatePaused,
		events:      NewEventLog(),
		pausedVCPUs: make(map[int]struct{}),
	}
	err := vm.Resume()
	if err != nil {
		t.Fatalf("Resume() = %v, want nil", err)
	}
	if vm.State() != StateRunning {
		t.Fatalf("state after resume = %v, want Running", vm.State())
	}
}

func TestVM_WaitStopped_ContextCanceled(t *testing.T) {
	vm := &VM{doneCh: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := vm.WaitStopped(ctx)
	if err == nil {
		t.Fatal("expected context error")
	}
}

func TestVM_DialVsock_NoDevice(t *testing.T) {
	vm := &VM{cfg: Config{}}
	_, err := vm.DialVsock(52)
	if err == nil {
		t.Fatal("expected error when vsock not configured")
	}
}

// --- normalizeSnapshotVCPUStates ---

func TestNormalizeSnapshotVCPUStates(t *testing.T) {
	t.Run("has vcpus", func(t *testing.T) {
		snap := Snapshot{VCPUs: []VCPUState{{ID: 0}, {ID: 1}}}
		states := normalizeSnapshotVCPUStates(snap)
		if len(states) != 2 {
			t.Fatalf("len = %d, want 2", len(states))
		}
	})
	t.Run("no vcpus", func(t *testing.T) {
		snap := Snapshot{Config: Config{VCPUs: 4}}
		states := normalizeSnapshotVCPUStates(snap)
		if len(states) != 1 {
			t.Fatalf("len = %d, want 1 (default fallback)", len(states))
		}
	})
}

// --- VM.GetMemoryHotplug with state configured ---

func TestVM_GetMemoryHotplug_Configured(t *testing.T) {
	vm := &VM{
		memoryHotplug: &memoryHotplugState{
			cfg:            MemoryHotplugConfig{TotalSizeMiB: 4096, SlotSizeMiB: 1024, BlockSizeMiB: 128},
			requestedBytes: 256 << 20,
			pluggedBytes:   128 << 20,
		},
		cfg:   Config{},
		state: StateCreated,
	}
	status, err := vm.GetMemoryHotplug()
	if err != nil {
		t.Fatalf("GetMemoryHotplug() error = %v", err)
	}
	if status.TotalSizeMiB != 4096 {
		t.Fatalf("TotalSizeMiB = %d", status.TotalSizeMiB)
	}
	if status.PluggedSizeMiB != 128 {
		t.Fatalf("PluggedSizeMiB = %d", status.PluggedSizeMiB)
	}
	if status.RequestedSizeMiB != 256 {
		t.Fatalf("RequestedSizeMiB = %d", status.RequestedSizeMiB)
	}
}

// --- VM.Stop on a fully stopped VM ---

func TestVM_Stop_AlreadyStopped(t *testing.T) {
	vm := &VM{
		state:  StateStopped,
		events: NewEventLog(),
		stopCh: make(chan struct{}),
	}
	// Should not panic
	vm.Stop()
}

// --- VM.finishStop ---

func TestVM_FinishStop(t *testing.T) {
	vm := &VM{
		doneCh: make(chan struct{}),
		events: NewEventLog(),
		cfg:    Config{ID: "test"},
	}
	vm.finishStop()
	// doneCh should be closed
	select {
	case <-vm.doneCh:
	default:
		t.Fatal("doneCh should be closed after finishStop")
	}
	// idempotent
	vm.finishStop()
}

// --- VM.cleanup with no devices ---

func TestVM_Cleanup_NoDevices(t *testing.T) {
	vm := &VM{cfg: Config{ID: "cleanup-test"}}
	// Should not panic
	vm.cleanup()
	// Idempotent
	vm.cleanup()
}

// --- VM.UpdateBalloonStats no device ---

func TestVM_UpdateBalloonStats_NoDevice(t *testing.T) {
	vm := &VM{}
	err := vm.UpdateBalloonStats(BalloonStatsUpdate{StatsPollingIntervalS: 5})
	if err == nil {
		t.Fatal("expected error when balloon not configured")
	}
}

// --- RewriteSnapshotBundleWithConfig ---

func TestRewriteSnapshotBundleWithConfig_Coverage(t *testing.T) {
	dir := t.TempDir()

	// Create minimal snapshot.json
	snap := Snapshot{
		Version: 2,
		ID:      "test-vm",
		MemFile: "mem.bin",
		Config: Config{
			MemMB:      256,
			Arch:       "amd64",
			KernelPath: "kernel.bin",
			VCPUs:      1,
			ID:         "test-vm",
		},
	}
	data, _ := json.MarshalIndent(snap, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "snapshot.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	// Create the kernel file referenced
	if err := os.WriteFile(filepath.Join(dir, "kernel.bin"), []byte("kernel-data"), 0644); err != nil {
		t.Fatal(err)
	}

	newCfg := Config{MemMB: 512, KernelPath: filepath.Join(dir, "kernel.bin"), ID: "new-vm"}
	result, err := RewriteSnapshotBundleWithConfig(dir, newCfg)
	if err != nil {
		t.Fatalf("RewriteSnapshotBundleWithConfig: %v", err)
	}
	if result.Config.MemMB != 512 {
		t.Fatalf("MemMB = %d, want 512", result.Config.MemMB)
	}
	if result.Config.KernelPath != "artifacts/kernel" {
		t.Fatalf("KernelPath = %q, want artifacts/kernel", result.Config.KernelPath)
	}
}

func TestRewriteSnapshotBundleWithConfig_MissingSnapshot(t *testing.T) {
	dir := t.TempDir()
	_, err := RewriteSnapshotBundleWithConfig(dir, Config{})
	if err == nil {
		t.Fatal("expected error for missing snapshot.json")
	}
}

// --- readSnapshot ---

func TestReadSnapshot_Coverage(t *testing.T) {
	dir := t.TempDir()
	snap := Snapshot{Version: 2, ID: "read-test", Config: Config{MemMB: 128}}
	data, _ := json.MarshalIndent(snap, "", "  ")
	os.WriteFile(filepath.Join(dir, "snapshot.json"), data, 0644)

	result, err := readSnapshot(dir)
	if err != nil {
		t.Fatalf("readSnapshot: %v", err)
	}
	if result.ID != "read-test" || result.Config.MemMB != 128 {
		t.Fatalf("mismatch: %+v", result)
	}
}

// --- copyFile ---

func TestCopyFile_Coverage(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst", "dst.txt")
	os.WriteFile(src, []byte("hello"), 0644)

	if err := copyFile(dst, src); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "hello" {
		t.Fatalf("dst content = %q", got)
	}
}

func TestCopyFile_NonexistentSource(t *testing.T) {
	dir := t.TempDir()
	err := copyFile(filepath.Join(dir, "dst"), filepath.Join(dir, "nonexistent"))
	if err == nil {
		t.Fatal("expected error for nonexistent source")
	}
}

// --- bundleAsset ---

func TestBundleAsset_EmptyPath(t *testing.T) {
	dir := t.TempDir()
	result, err := bundleAsset(dir, "", "artifacts/kernel")
	if err != nil {
		t.Fatalf("bundleAsset: %v", err)
	}
	if result != "" {
		t.Fatalf("expected empty, got %q", result)
	}
}

func TestBundleAsset_SamePath(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "artifacts"), 0755)
	assetPath := filepath.Join(dir, "artifacts", "kernel")
	os.WriteFile(assetPath, []byte("data"), 0644)

	result, err := bundleAsset(dir, assetPath, "artifacts/kernel")
	if err != nil {
		t.Fatalf("bundleAsset: %v", err)
	}
	if result != "artifacts/kernel" {
		t.Fatalf("result = %q", result)
	}
}

// --- resolveSnapshotPath ---

func TestResolveSnapshotPath_Coverage(t *testing.T) {
	tests := []struct {
		dir   string
		value string
		want  string
	}{
		{"/snap", "", ""},
		{"/snap", "/absolute/path", "/absolute/path"},
		{"/snap", "relative/file", "/snap/relative/file"},
	}
	for _, tt := range tests {
		got := resolveSnapshotPath(tt.dir, tt.value)
		if got != tt.want {
			t.Errorf("resolveSnapshotPath(%q, %q) = %q, want %q", tt.dir, tt.value, got, tt.want)
		}
	}
}

// --- writeMemoryFile ---

func TestWriteMemoryFile_Coverage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mem.bin")
	data := []byte("memory contents")
	if err := writeMemoryFile(path, data); err != nil {
		t.Fatalf("writeMemoryFile: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "memory contents" {
		t.Fatalf("content = %q", got)
	}
}

// --- Pause timeout (verify it returns error for running VM with no vcpus to ack) ---

func TestVM_Pause_TimeoutNoVCPUs(t *testing.T) {
	vm := &VM{
		state:       StateRunning,
		events:      NewEventLog(),
		pausedVCPUs: make(map[int]struct{}),
		vcpus:       nil, // no vcpus
	}
	// Pause with 0 vcpus should succeed immediately (0 == 0)
	err := vm.Pause()
	if err != nil {
		t.Fatalf("Pause() with no vcpus = %v, want nil", err)
	}
}
