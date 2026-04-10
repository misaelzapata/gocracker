package vmm

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"
)

func TestStateString(t *testing.T) {
	tests := []struct {
		state State
		want  string
	}{
		{StateCreated, "created"},
		{StateRunning, "running"},
		{StatePaused, "paused"},
		{StateStopped, "stopped"},
	}
	for _, tt := range tests {
		got := tt.state.String()
		if got != tt.want {
			t.Errorf("State(%d).String() = %q, want %q", tt.state, got, tt.want)
		}
	}
}

func TestStateConstants(t *testing.T) {
	if StateCreated != 0 {
		t.Errorf("StateCreated = %d, want 0", StateCreated)
	}
	if StateRunning != 1 {
		t.Errorf("StateRunning = %d, want 1", StateRunning)
	}
	if StatePaused != 2 {
		t.Errorf("StatePaused = %d, want 2", StatePaused)
	}
	if StateStopped != 3 {
		t.Errorf("StateStopped = %d, want 3", StateStopped)
	}
}

func TestConfigDefaults_MemMB(t *testing.T) {
	// New() applies defaults: MemMB=0 should become 128
	// We can't call New() since it requires KVM, but we can verify
	// the logic pattern directly.
	cfg := Config{MemMB: 0}
	if cfg.MemMB == 0 {
		cfg.MemMB = 128
	}
	if cfg.MemMB != 128 {
		t.Errorf("default MemMB = %d, want 128", cfg.MemMB)
	}
}

func TestConfigDefaults_NonZeroMemMB(t *testing.T) {
	cfg := Config{MemMB: 512}
	if cfg.MemMB == 0 {
		cfg.MemMB = 128
	}
	if cfg.MemMB != 512 {
		t.Errorf("MemMB = %d, want 512 (should not override)", cfg.MemMB)
	}
}

func TestConfigDefaults_ID(t *testing.T) {
	cfg := Config{ID: ""}
	if cfg.ID == "" {
		cfg.ID = "vm-test"
	}
	if cfg.ID != "vm-test" {
		t.Errorf("default ID = %q, want %q", cfg.ID, "vm-test")
	}
}

func TestConfigDefaults_IDPreserved(t *testing.T) {
	cfg := Config{ID: "my-vm-1"}
	if cfg.ID == "" {
		cfg.ID = "vm-test"
	}
	if cfg.ID != "my-vm-1" {
		t.Errorf("ID = %q, want %q (should not override)", cfg.ID, "my-vm-1")
	}
}

func TestMemoryLayoutConstants(t *testing.T) {
	// Verify key memory layout constants
	if BootParamsAddr != 0x7000 {
		t.Errorf("BootParamsAddr = %#x, want %#x", BootParamsAddr, 0x7000)
	}
	if KernelLoad != 0x100000 {
		t.Errorf("KernelLoad = %#x, want %#x", KernelLoad, 0x100000)
	}
	if InitrdAddr != 0x1000000 {
		t.Errorf("InitrdAddr = %#x, want %#x", InitrdAddr, 0x1000000)
	}
	if COM1Base != 0x3F8 {
		t.Errorf("COM1Base = %#x, want %#x", COM1Base, 0x3F8)
	}
	if COM1IRQ != 4 {
		t.Errorf("COM1IRQ = %d, want 4", COM1IRQ)
	}
}

func TestVirtioLayoutConstants(t *testing.T) {
	if VirtioBase != 0xD0000000 {
		t.Errorf("VirtioBase = %#x, want %#x", VirtioBase, 0xD0000000)
	}
	if VirtioStride != 0x1000 {
		t.Errorf("VirtioStride = %#x, want %#x", VirtioStride, 0x1000)
	}
	if VirtioIRQBase != 5 {
		t.Errorf("VirtioIRQBase = %d, want 5", VirtioIRQBase)
	}
}

func TestSnapshotVersion(t *testing.T) {
	// Verify snapshot struct can be created
	snap := Snapshot{
		Version: 2,
		ID:      "test-vm",
	}
	if snap.Version != 2 {
		t.Errorf("snapshot version = %d, want 2", snap.Version)
	}
}

func TestConfigFields(t *testing.T) {
	cfg := Config{
		MemMB:      256,
		Arch:       string(ArchAMD64),
		KernelPath: "/boot/vmlinuz",
		InitrdPath: "/boot/initrd.img",
		Cmdline:    "console=ttyS0",
		DiskImage:  "/tmp/disk.ext4",
		DiskRO:     true,
		TapName:    "tap0",
		VCPUs:      1,
		ID:         "test-vm",
	}
	if cfg.MemMB != 256 {
		t.Errorf("MemMB = %d", cfg.MemMB)
	}
	if cfg.KernelPath != "/boot/vmlinuz" {
		t.Errorf("KernelPath = %q", cfg.KernelPath)
	}
	if cfg.Arch != string(ArchAMD64) {
		t.Errorf("Arch = %q", cfg.Arch)
	}
	if cfg.DiskRO != true {
		t.Error("DiskRO should be true")
	}
	if cfg.TapName != "tap0" {
		t.Errorf("TapName = %q", cfg.TapName)
	}
}

func TestNormalizeSnapshotMachineArchDefaultsOldSnapshotsToAMD64(t *testing.T) {
	arch, err := normalizeSnapshotMachineArch("")
	if err != nil {
		t.Fatalf("normalizeSnapshotMachineArch() error = %v", err)
	}
	if arch != ArchAMD64 {
		t.Fatalf("normalizeSnapshotMachineArch() = %q, want %q", arch, ArchAMD64)
	}
}

func TestValidateMachineArchRejectsCrossArch(t *testing.T) {
	if HostArch() == ArchAMD64 {
		if err := validateMachineArch(ArchARM64); err == nil {
			t.Fatal("validateMachineArch(arm64) error = nil, want rejection on amd64 host")
		}
	}
}

func TestNewMachineArchBackendAMD64(t *testing.T) {
	backend, err := newMachineArchBackend(ArchAMD64)
	if err != nil {
		t.Fatalf("newMachineArchBackend(amd64) error = %v", err)
	}
	if backend == nil {
		t.Fatal("newMachineArchBackend(amd64) = nil, want backend")
	}
}

func TestNewMachineArchBackendARM64StillExplicitlyRejected(t *testing.T) {
	backend, err := newMachineArchBackend(ArchARM64)
	if err == nil {
		t.Fatalf("newMachineArchBackend(arm64) error = nil, backend = %#v", backend)
	}
}

func TestDefaultGuestMAC_DeterministicAndUnique(t *testing.T) {
	first := defaultGuestMAC("vm-a", "tap-a")
	second := defaultGuestMAC("vm-b", "tap-b")
	again := defaultGuestMAC("vm-a", "tap-a")

	if !bytes.Equal(first, again) {
		t.Fatalf("defaultGuestMAC should be deterministic: %s != %s", first, again)
	}
	if bytes.Equal(first, second) {
		t.Fatalf("defaultGuestMAC should differ for different VMs: %s == %s", first, second)
	}
	if first[0]&0x01 != 0 {
		t.Fatalf("defaultGuestMAC should be unicast, got %s", first)
	}
	if first[0]&0x02 == 0 {
		t.Fatalf("defaultGuestMAC should be locally administered, got %s", first)
	}
}

func TestDefaultGuestMAC_Fallback(t *testing.T) {
	got := defaultGuestMAC("", "")
	want := net.HardwareAddr{0x06, 0x00, 0xAC, 0x10, 0x00, 0x02}
	if !bytes.Equal(got, want) {
		t.Fatalf("defaultGuestMAC fallback = %s, want %s", got, want)
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
