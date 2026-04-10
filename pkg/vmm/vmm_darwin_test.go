//go:build darwin

package vmm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewRejectsEmptyKernel(t *testing.T) {
	_, err := New(Config{MemMB: 256, VCPUs: 1})
	if err == nil {
		t.Fatal("New() with empty kernel should return error")
	}
}

func TestNewRejectsTAPNetworking(t *testing.T) {
	_, err := New(Config{
		MemMB:      256,
		VCPUs:      1,
		KernelPath: "/nonexistent/vmlinuz",
		TapName:    "tap0",
	})
	if !errors.Is(err, ErrTAPNotSupported) {
		t.Fatalf("New() with TapName should return ErrTAPNotSupported, got: %v", err)
	}
}

func TestNewRejectsMemoryHotplug(t *testing.T) {
	_, err := New(Config{
		MemMB:         256,
		VCPUs:         1,
		KernelPath:    "/nonexistent/vmlinuz",
		MemoryHotplug: &MemoryHotplugConfig{TotalSizeMiB: 1024},
	})
	if !errors.Is(err, ErrHotplugNotSupported) {
		t.Fatalf("New() with MemoryHotplug should return ErrHotplugNotSupported, got: %v", err)
	}
}

func TestNewRejectsCrossArch(t *testing.T) {
	opposite := "amd64"
	if HostArch() == ArchAMD64 {
		opposite = "arm64"
	}
	_, err := New(Config{
		MemMB:      256,
		VCPUs:      1,
		KernelPath: "/nonexistent/vmlinuz",
		Arch:       opposite,
	})
	if err == nil {
		t.Fatalf("New() with cross-arch %q should return error", opposite)
	}
}

func TestSnapshotRequiresRunningOrPaused(t *testing.T) {
	vm := &VM{
		cfg:    Config{ID: "test"},
		state:  StateCreated,
		doneCh: make(chan struct{}),
		stopCh: make(chan struct{}),
		events: NewEventLog(),
	}

	_, err := vm.TakeSnapshot("/tmp/snap")
	if err == nil {
		t.Fatal("TakeSnapshot() in StateCreated should return error")
	}
}

func TestRestoreFromSnapshotMissingDir(t *testing.T) {
	_, err := RestoreFromSnapshot("/nonexistent/snapshot")
	if err == nil {
		t.Fatal("RestoreFromSnapshot() with missing dir should return error")
	}
}

func TestMigrationNotSupported(t *testing.T) {
	vm := &VM{
		cfg:    Config{ID: "test"},
		doneCh: make(chan struct{}),
		stopCh: make(chan struct{}),
		events: NewEventLog(),
	}

	if err := vm.PrepareMigrationBundle("/tmp/bundle"); !errors.Is(err, ErrMigrationNotSupported) {
		t.Fatalf("PrepareMigrationBundle() = %v, want ErrMigrationNotSupported", err)
	}

	_, _, err := vm.FinalizeMigrationBundle("/tmp/bundle")
	if !errors.Is(err, ErrMigrationNotSupported) {
		t.Fatalf("FinalizeMigrationBundle() = %v, want ErrMigrationNotSupported", err)
	}

	if err := vm.ResetMigrationTracking(); !errors.Is(err, ErrMigrationNotSupported) {
		t.Fatalf("ResetMigrationTracking() = %v, want ErrMigrationNotSupported", err)
	}
}

func TestMemoryHotplugNotSupported(t *testing.T) {
	vm := &VM{
		cfg:    Config{ID: "test"},
		doneCh: make(chan struct{}),
		stopCh: make(chan struct{}),
		events: NewEventLog(),
	}

	_, err := vm.GetMemoryHotplug()
	if !errors.Is(err, ErrHotplugNotSupported) {
		t.Fatalf("GetMemoryHotplug() = %v, want ErrHotplugNotSupported", err)
	}

	if err := vm.UpdateMemoryHotplug(MemoryHotplugSizeUpdate{}); !errors.Is(err, ErrHotplugNotSupported) {
		t.Fatalf("UpdateMemoryHotplug() = %v, want ErrHotplugNotSupported", err)
	}
}

func TestVMStateAfterConstruction(t *testing.T) {
	vm := &VM{
		cfg:    Config{ID: "test"},
		state:  StateCreated,
		doneCh: make(chan struct{}),
		stopCh: make(chan struct{}),
		events: NewEventLog(),
	}

	if vm.State() != StateCreated {
		t.Fatalf("State() = %v, want StateCreated", vm.State())
	}
	if vm.ID() != "test" {
		t.Fatalf("ID() = %q, want %q", vm.ID(), "test")
	}
	if vm.Uptime() != 0 {
		t.Fatalf("Uptime() = %v, want 0", vm.Uptime())
	}
}

func TestPauseRequiresRunning(t *testing.T) {
	vm := &VM{
		cfg:    Config{ID: "test"},
		state:  StateCreated,
		doneCh: make(chan struct{}),
		stopCh: make(chan struct{}),
		events: NewEventLog(),
	}
	if err := vm.Pause(); err == nil {
		t.Fatal("Pause() in StateCreated should return error")
	}
}

func TestResumeRequiresPaused(t *testing.T) {
	vm := &VM{
		cfg:    Config{ID: "test"},
		state:  StateCreated,
		doneCh: make(chan struct{}),
		stopCh: make(chan struct{}),
		events: NewEventLog(),
	}
	if err := vm.Resume(); err == nil {
		t.Fatal("Resume() in StateCreated should return error")
	}
}

func TestVMDeviceList(t *testing.T) {
	vm := &VM{
		cfg: Config{
			ID:        "test",
			DiskImage: "/tmp/disk.ext4",
			Balloon:   &BalloonConfig{},
			SharedFS:  []SharedFSConfig{{Source: "/tmp", Tag: "data"}},
			Vsock:     &VsockConfig{Enabled: true},
		},
		events: NewEventLog(),
	}

	devices := vm.DeviceList()
	typeSet := make(map[string]bool)
	for _, d := range devices {
		typeSet[d.Type] = true
	}

	expected := []string{"virtio-blk:root", "virtio-net:nat", "virtio-rng", "virtio-balloon", "virtio-fs:data", "virtio-vsock", "virtio-console"}
	for _, e := range expected {
		if !typeSet[e] {
			t.Errorf("DeviceList() missing %q", e)
		}
	}
}

func TestVMDeviceList_ShowsVmnetForStackNetwork(t *testing.T) {
	vm := &VM{
		cfg: Config{
			ID:      "test",
			Network: &NetworkConfig{Mode: NetworkAttachmentStack, NetworkID: "stack:test"},
		},
		events: NewEventLog(),
	}

	devices := vm.DeviceList()
	found := false
	for _, device := range devices {
		if device.Type == "virtio-net:vmnet" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("DeviceList() = %#v, want virtio-net:vmnet", devices)
	}
}

func TestSnapshotRejectsDarwinStackNetwork(t *testing.T) {
	vm := &VM{
		cfg: Config{
			ID:      "test",
			Network: &NetworkConfig{Mode: NetworkAttachmentStack, NetworkID: "stack:test"},
		},
		state:  StateRunning,
		doneCh: make(chan struct{}),
		stopCh: make(chan struct{}),
		events: NewEventLog(),
	}

	if _, err := vm.TakeSnapshotWithOptions(t.TempDir(), SnapshotOptions{}); err == nil {
		t.Fatal("TakeSnapshotWithOptions() error = nil, want darwin stack-network rejection")
	}
}

func TestRestoreRejectsDarwinStackNetworkSnapshot(t *testing.T) {
	dir := t.TempDir()
	snapshot := Snapshot{
		Version:   2,
		Timestamp: time.Now(),
		ID:        "test",
		Config: Config{
			ID:         "test",
			KernelPath: "/kernel",
			Network:    &NetworkConfig{Mode: NetworkAttachmentStack, NetworkID: "stack:test"},
		},
		MemFile: "vm-state.vzvmsave",
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "snapshot.json"), data, 0644); err != nil {
		t.Fatalf("write snapshot.json: %v", err)
	}

	if _, err := RestoreFromSnapshotWithOptions(dir, RestoreOptions{}); err == nil {
		t.Fatal("RestoreFromSnapshotWithOptions() error = nil, want darwin stack-network rejection")
	}
}

func TestWaitStoppedReturnsWhenDone(t *testing.T) {
	vm := &VM{doneCh: make(chan struct{})}
	close(vm.doneCh)
	if err := vm.WaitStopped(context.Background()); err != nil {
		t.Fatalf("WaitStopped() = %v, want nil", err)
	}
}

func TestWaitStoppedHonorsContext(t *testing.T) {
	vm := &VM{doneCh: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := vm.WaitStopped(ctx); err == nil {
		t.Fatal("WaitStopped() = nil, want context error")
	}
}

func TestDialVsockWithoutConfig(t *testing.T) {
	vm := &VM{
		cfg:    Config{ID: "test"},
		state:  StateRunning,
		doneCh: make(chan struct{}),
		stopCh: make(chan struct{}),
		events: NewEventLog(),
	}

	_, err := vm.DialVsock(512)
	if err == nil {
		t.Fatal("DialVsock() without vsock config should return error")
	}
}

func TestRestoreFromSnapshotMissingMeta(t *testing.T) {
	dir := t.TempDir()
	_, err := RestoreFromSnapshot(dir)
	if err == nil {
		t.Fatal("RestoreFromSnapshot() with empty dir should return error")
	}
}

func TestApplyMigrationPatchesNotSupported(t *testing.T) {
	if err := ApplyMigrationPatches("/tmp/bundle"); !errors.Is(err, ErrMigrationNotSupported) {
		t.Fatalf("ApplyMigrationPatches() = %v, want ErrMigrationNotSupported", err)
	}
}

func TestRateLimitersNoOp(t *testing.T) {
	vm := &VM{events: NewEventLog()}
	if err := vm.UpdateNetRateLimiter(nil); err != nil {
		t.Fatalf("UpdateNetRateLimiter() = %v, want nil", err)
	}
	if err := vm.UpdateBlockRateLimiter(nil); err != nil {
		t.Fatalf("UpdateBlockRateLimiter() = %v, want nil", err)
	}
	if err := vm.UpdateRNGRateLimiter(nil); err != nil {
		t.Fatalf("UpdateRNGRateLimiter() = %v, want nil", err)
	}
}
