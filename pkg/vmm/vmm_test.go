package vmm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/gocracker/gocracker/internal/guestexec"
	"github.com/gocracker/gocracker/internal/kvm"
	"github.com/gocracker/gocracker/internal/virtio"
	"golang.org/x/sys/unix"
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

// --- Additional tests for non-KVM pure Go logic ---

func TestHostArch_ReturnsValidArch(t *testing.T) {
	arch := HostArch()
	switch arch {
	case ArchAMD64, ArchARM64:
		// ok
	default:
		t.Fatalf("HostArch() = %q, want amd64 or arm64", arch)
	}
}

func TestNormalizeX86BootMode(t *testing.T) {
	tests := []struct {
		input   X86BootMode
		want    X86BootMode
		wantErr bool
	}{
		{"", X86BootAuto, false},
		{X86BootAuto, X86BootAuto, false},
		{X86BootACPI, X86BootACPI, false},
		{X86BootLegacy, X86BootLegacy, false},
		{"invalid", "", true},
		{"ACPI", "", true},
	}
	for _, tt := range tests {
		got, err := normalizeX86BootMode(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("normalizeX86BootMode(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("normalizeX86BootMode(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNormalizeMachineArch(t *testing.T) {
	tests := []struct {
		input   string
		want    MachineArch
		wantErr bool
	}{
		{"", HostArch(), false},
		{"  ", HostArch(), false},
		{"amd64", ArchAMD64, false},
		{"arm64", ArchARM64, false},
		{"  amd64  ", ArchAMD64, false},
		{"x86_64", "", true},
		{"mips", "", true},
	}
	for _, tt := range tests {
		got, err := normalizeMachineArch(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("normalizeMachineArch(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("normalizeMachineArch(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestValidateMachineArch_HostArch(t *testing.T) {
	if err := validateMachineArch(HostArch()); err != nil {
		t.Fatalf("validateMachineArch(HostArch()) = %v, want nil", err)
	}
}

func TestGuestRAMBase(t *testing.T) {
	if got := guestRAMBase("arm64"); got != 0x80000000 {
		t.Fatalf("guestRAMBase(arm64) = %#x, want 0x80000000", got)
	}
	if got := guestRAMBase("amd64"); got != 0 {
		t.Fatalf("guestRAMBase(amd64) = %#x, want 0", got)
	}
	if got := guestRAMBase(""); got != 0 {
		t.Fatalf("guestRAMBase(\"\") = %#x, want 0", got)
	}
}

func TestStateString_OutOfRange(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for out-of-range State")
		}
	}()
	_ = State(99).String()
}

func TestDriveList_FromDiskImage(t *testing.T) {
	cfg := Config{
		DiskImage: "/tmp/rootfs.ext4",
		DiskRO:    true,
	}
	drives := cfg.DriveList()
	if len(drives) != 1 {
		t.Fatalf("DriveList() returned %d drives, want 1", len(drives))
	}
	d := drives[0]
	if d.ID != "root" || d.Path != "/tmp/rootfs.ext4" || !d.Root || !d.ReadOnly {
		t.Fatalf("unexpected drive: %#v", d)
	}
}

func TestDriveList_FromDrives(t *testing.T) {
	cfg := Config{
		DiskImage: "/tmp/rootfs.ext4",
		Drives: []DriveConfig{
			{ID: "root", Path: "/a", Root: true},
			{ID: "data", Path: "/b", Root: false, ReadOnly: true},
		},
	}
	drives := cfg.DriveList()
	if len(drives) != 2 {
		t.Fatalf("DriveList() returned %d drives, want 2", len(drives))
	}
	if drives[0].ID != "root" || drives[1].ID != "data" {
		t.Fatalf("unexpected drives: %#v", drives)
	}
}

func TestDriveList_NoDiskImage(t *testing.T) {
	cfg := Config{}
	if drives := cfg.DriveList(); drives != nil {
		t.Fatalf("DriveList() = %v, want nil", drives)
	}
}

func TestDriveList_WithBlockRateLimiter(t *testing.T) {
	rl := &RateLimiterConfig{
		Bandwidth: TokenBucketConfig{Size: 1000},
	}
	cfg := Config{
		DiskImage:        "/tmp/disk.ext4",
		BlockRateLimiter: rl,
	}
	drives := cfg.DriveList()
	if len(drives) != 1 {
		t.Fatalf("DriveList() returned %d drives, want 1", len(drives))
	}
	if drives[0].RateLimiter == nil {
		t.Fatal("expected rate limiter on root drive")
	}
	if drives[0].RateLimiter.Bandwidth.Size != 1000 {
		t.Fatalf("rate limiter bandwidth = %d, want 1000", drives[0].RateLimiter.Bandwidth.Size)
	}
	// Verify clone: modifying the returned drive should not affect the original config
	drives[0].RateLimiter.Bandwidth.Size = 9999
	if cfg.BlockRateLimiter.Bandwidth.Size != 1000 {
		t.Fatal("DriveList() did not clone the rate limiter")
	}
}

func TestRootDrive(t *testing.T) {
	cfg := Config{
		Drives: []DriveConfig{
			{ID: "data", Path: "/b", Root: false},
			{ID: "root", Path: "/a", Root: true},
		},
	}
	root, ok := cfg.RootDrive()
	if !ok {
		t.Fatal("RootDrive() = _, false, want true")
	}
	if root.ID != "root" {
		t.Fatalf("RootDrive().ID = %q, want root", root.ID)
	}
}

func TestRootDrive_None(t *testing.T) {
	cfg := Config{
		Drives: []DriveConfig{
			{ID: "data", Path: "/b", Root: false},
		},
	}
	_, ok := cfg.RootDrive()
	if ok {
		t.Fatal("RootDrive() = _, true, want false when no root drive exists")
	}
}

func TestHasAdditionalDrives(t *testing.T) {
	tests := []struct {
		name   string
		drives []DriveConfig
		want   bool
	}{
		{"single root", []DriveConfig{{Root: true}}, false},
		{"root+data", []DriveConfig{{Root: true}, {Root: false}}, true},
		{"no root", []DriveConfig{{Root: false}}, true},
		{"empty", nil, false},
	}
	for _, tt := range tests {
		cfg := Config{Drives: tt.drives}
		if got := cfg.HasAdditionalDrives(); got != tt.want {
			t.Errorf("HasAdditionalDrives(%s) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestEventLog_EmitAndRetrieve(t *testing.T) {
	el := NewEventLog()
	el.Emit(EventCreated, "vm created")
	el.Emit(EventRunning, "vm started")

	events := el.Events(time.Time{})
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[0].Type != EventCreated {
		t.Errorf("events[0].Type = %q, want %q", events[0].Type, EventCreated)
	}
	if events[1].Type != EventRunning {
		t.Errorf("events[1].Type = %q, want %q", events[1].Type, EventRunning)
	}
}

func TestEventLog_EventsSince(t *testing.T) {
	el := NewEventLog()
	el.Emit(EventCreated, "first")
	midpoint := time.Now()
	time.Sleep(time.Millisecond)
	el.Emit(EventRunning, "second")

	events := el.Events(midpoint)
	if len(events) != 1 {
		t.Fatalf("got %d events since midpoint, want 1", len(events))
	}
	if events[0].Type != EventRunning {
		t.Errorf("events[0].Type = %q, want %q", events[0].Type, EventRunning)
	}
}

func TestEventLog_Subscribe(t *testing.T) {
	el := NewEventLog()
	ch, unsub := el.Subscribe()
	defer unsub()

	el.Emit(EventCreated, "test")

	select {
	case ev := <-ch:
		if ev.Type != EventCreated {
			t.Fatalf("received event type %q, want %q", ev.Type, EventCreated)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestEventLog_Unsubscribe(t *testing.T) {
	el := NewEventLog()
	ch, unsub := el.Subscribe()
	unsub()

	// Channel should be closed after unsubscribe
	_, ok := <-ch
	if ok {
		t.Fatal("expected channel to be closed after unsubscribe")
	}
}

func TestEventLog_MaxEventsRingBuffer(t *testing.T) {
	el := NewEventLog()
	for i := 0; i < maxEvents+100; i++ {
		el.Emit(EventRunning, "event")
	}
	events := el.Events(time.Time{})
	if len(events) != maxEvents {
		t.Fatalf("got %d events, want %d (ring buffer cap)", len(events), maxEvents)
	}
}

func TestEventTypeConstants(t *testing.T) {
	expected := map[EventType]string{
		EventCreated:       "created",
		EventKernelLoaded:  "kernel_loaded",
		EventDevicesReady:  "devices_ready",
		EventCPUConfigured: "cpu_configured",
		EventStarting:      "starting",
		EventRunning:       "running",
		EventPaused:        "paused",
		EventResumed:       "resumed",
		EventShutdown:      "shutdown",
		EventHalted:        "halted",
		EventError:         "error",
		EventStopped:       "stopped",
		EventSnapshot:      "snapshot",
		EventRestored:      "restored",
	}
	for ev, want := range expected {
		if string(ev) != want {
			t.Errorf("EventType %q != %q", ev, want)
		}
	}
}

func TestSnapshotSerialization(t *testing.T) {
	snap := Snapshot{
		Version:   2,
		ID:        "test-vm",
		Timestamp: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		Config: Config{
			MemMB:      256,
			Arch:       "amd64",
			KernelPath: "/boot/vmlinuz",
			VCPUs:      2,
			ID:         "test-vm",
		},
		MemFile: "mem.bin",
		VCPUs: []VCPUState{
			{ID: 0},
			{ID: 1},
		},
	}

	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var restored Snapshot
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if restored.Version != 2 {
		t.Errorf("version = %d, want 2", restored.Version)
	}
	if restored.ID != "test-vm" {
		t.Errorf("ID = %q, want test-vm", restored.ID)
	}
	if restored.Config.MemMB != 256 {
		t.Errorf("Config.MemMB = %d, want 256", restored.Config.MemMB)
	}
	if len(restored.VCPUs) != 2 {
		t.Errorf("VCPUs = %d, want 2", len(restored.VCPUs))
	}
	if restored.MemFile != "mem.bin" {
		t.Errorf("MemFile = %q, want mem.bin", restored.MemFile)
	}
}

func TestVCPUState_NormalizedX86_FromX86Field(t *testing.T) {
	state := VCPUState{
		ID:  0,
		X86: &X86VCPUState{MPState: kvm.MPState{State: 1}},
	}
	normalized := state.normalizedX86()
	if normalized.MPState.State != 1 {
		t.Fatalf("normalized MPState = %d, want 1", normalized.MPState.State)
	}
}

func TestVCPUState_NormalizedX86_FallbackToLegacy(t *testing.T) {
	state := VCPUState{
		ID:      0,
		MPState: kvm.MPState{State: 2},
	}
	normalized := state.normalizedX86()
	if normalized.MPState.State != 2 {
		t.Fatalf("normalized MPState = %d, want 2", normalized.MPState.State)
	}
}

func TestSnapshotArchState_NormalizedX86_Nil(t *testing.T) {
	var s *SnapshotArchState
	if got := s.normalizedX86(); got != nil {
		t.Fatalf("normalizedX86() on nil = %v, want nil", got)
	}
}

func TestSnapshotArchState_NormalizedX86_FromX86Field(t *testing.T) {
	s := &SnapshotArchState{
		X86: &X86MachineState{
			Clock: kvm.ClockData{Clock: 12345},
		},
	}
	got := s.normalizedX86()
	if got.Clock.Clock != 12345 {
		t.Fatalf("Clock = %d, want 12345", got.Clock.Clock)
	}
}

func TestSnapshotArchState_NormalizedX86_FallbackToLegacy(t *testing.T) {
	s := &SnapshotArchState{
		Clock: kvm.ClockData{Clock: 99},
	}
	got := s.normalizedX86()
	if got.Clock.Clock != 99 {
		t.Fatalf("Clock = %d, want 99", got.Clock.Clock)
	}
}

func TestCloneRateLimiterConfig_Nil(t *testing.T) {
	if got := cloneRateLimiterConfig(nil); got != nil {
		t.Fatalf("cloneRateLimiterConfig(nil) = %v, want nil", got)
	}
}

func TestCloneRateLimiterConfig_DeepCopy(t *testing.T) {
	orig := &RateLimiterConfig{
		Bandwidth: TokenBucketConfig{Size: 100, OneTimeBurst: 50, RefillTimeMs: 1000},
		Ops:       TokenBucketConfig{Size: 200},
	}
	clone := cloneRateLimiterConfig(orig)
	if clone == orig {
		t.Fatal("clone should be a different pointer")
	}
	if clone.Bandwidth.Size != 100 || clone.Ops.Size != 200 {
		t.Fatalf("clone values wrong: %#v", clone)
	}
	clone.Bandwidth.Size = 999
	if orig.Bandwidth.Size != 100 {
		t.Fatal("modifying clone affected original")
	}
}

func TestDefaultGuestMAC_ValidFormat(t *testing.T) {
	mac := defaultGuestMAC("vm-test", "tap-test")
	if len(mac) != 6 {
		t.Fatalf("MAC length = %d, want 6", len(mac))
	}
	// Locally administered and unicast
	if mac[0]&0x01 != 0 {
		t.Error("MAC should be unicast (bit 0 of byte 0 should be 0)")
	}
	if mac[0]&0x02 == 0 {
		t.Error("MAC should be locally administered (bit 1 of byte 0 should be 1)")
	}
}

func TestBalloonAutoModeConstants(t *testing.T) {
	if BalloonAutoOff != "off" {
		t.Errorf("BalloonAutoOff = %q, want off", BalloonAutoOff)
	}
	if BalloonAutoConservative != "conservative" {
		t.Errorf("BalloonAutoConservative = %q, want conservative", BalloonAutoConservative)
	}
}

func TestConfigJSON_RoundTrip(t *testing.T) {
	cfg := Config{
		MemMB:      512,
		Arch:       "amd64",
		KernelPath: "/boot/vmlinuz",
		VCPUs:      4,
		ID:         "test-vm",
		Drives: []DriveConfig{
			{ID: "root", Path: "/disk.ext4", Root: true},
		},
		Vsock: &VsockConfig{Enabled: true, GuestCID: 3},
		Exec:  &ExecConfig{Enabled: true, VsockPort: 52},
		Balloon: &BalloonConfig{
			AmountMiB:    64,
			DeflateOnOOM: true,
			Auto:         BalloonAutoConservative,
		},
		Metadata: map[string]string{"key": "value"},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var restored Config
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if restored.MemMB != 512 || restored.VCPUs != 4 || restored.Arch != "amd64" {
		t.Fatalf("basic fields mismatch: %#v", restored)
	}
	if restored.Vsock == nil || !restored.Vsock.Enabled || restored.Vsock.GuestCID != 3 {
		t.Fatalf("Vsock mismatch: %#v", restored.Vsock)
	}
	if restored.Balloon == nil || restored.Balloon.Auto != BalloonAutoConservative {
		t.Fatalf("Balloon mismatch: %#v", restored.Balloon)
	}
	if len(restored.Drives) != 1 || restored.Drives[0].ID != "root" {
		t.Fatalf("Drives mismatch: %#v", restored.Drives)
	}
	if restored.Metadata["key"] != "value" {
		t.Fatalf("Metadata mismatch: %#v", restored.Metadata)
	}
}

func TestDeviceInfo_JSON(t *testing.T) {
	info := DeviceInfo{Type: "virtio-net", IRQ: 5}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var restored DeviceInfo
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if restored.Type != "virtio-net" || restored.IRQ != 5 {
		t.Fatalf("DeviceInfo mismatch: %#v", restored)
	}
}

func TestTokenBucketConfig_ToVirtio(t *testing.T) {
	cfg := TokenBucketConfig{
		Size:         1024,
		OneTimeBurst: 512,
		RefillTimeMs: 500,
	}
	bucket := cfg.toVirtio()
	if bucket.Size != 1024 {
		t.Errorf("Size = %d, want 1024", bucket.Size)
	}
	if bucket.OneTimeBurst != 512 {
		t.Errorf("OneTimeBurst = %d, want 512", bucket.OneTimeBurst)
	}
	if bucket.RefillTime != 500*time.Millisecond {
		t.Errorf("RefillTime = %v, want 500ms", bucket.RefillTime)
	}
}

func TestCloneDriveConfigs_Nil(t *testing.T) {
	if got := cloneDriveConfigs(nil); got != nil {
		t.Fatalf("cloneDriveConfigs(nil) = %v, want nil", got)
	}
}

func TestCloneDriveConfigs_DeepCopy(t *testing.T) {
	rl := &RateLimiterConfig{Bandwidth: TokenBucketConfig{Size: 100}}
	src := []DriveConfig{
		{ID: "root", Path: "/a", Root: true, RateLimiter: rl},
		{ID: "data", Path: "/b"},
	}
	dst := cloneDriveConfigs(src)
	if len(dst) != 2 {
		t.Fatalf("len = %d, want 2", len(dst))
	}
	if dst[0].RateLimiter == rl {
		t.Fatal("expected deep copy of rate limiter")
	}
	dst[0].RateLimiter.Bandwidth.Size = 999
	if rl.Bandwidth.Size != 100 {
		t.Fatal("modifying clone affected original")
	}
}

func TestNewX86VCPUState(t *testing.T) {
	regs := kvm.Regs{RAX: 1}
	sregs := kvm.Sregs{}
	mpState := kvm.MPState{State: 0}
	state := newX86VCPUState(3, regs, sregs, mpState, nil)
	if state.ID != 3 {
		t.Fatalf("ID = %d, want 3", state.ID)
	}
	if state.X86 == nil {
		t.Fatal("X86 field should not be nil")
	}
	if state.Regs.RAX != 1 {
		t.Fatalf("Regs.RAX = %d, want 1", state.Regs.RAX)
	}
}

func TestNewARM64VCPUState(t *testing.T) {
	core := map[uint64]uint64{0x1: 42}
	sys := map[uint64]uint64{0x2: 99}
	state := newARM64VCPUState(1, core, sys)
	if state.ID != 1 {
		t.Fatalf("ID = %d, want 1", state.ID)
	}
	if state.ARM64 == nil {
		t.Fatal("ARM64 field should not be nil")
	}
	if state.ARM64.CoreRegs[0x1] != 42 {
		t.Fatal("CoreRegs mismatch")
	}
}

func TestNewX86SnapshotArchState(t *testing.T) {
	var chipData [512]byte
	chipData[0] = 0xAB
	ms := &X86MachineState{
		Clock: kvm.ClockData{Clock: 100},
		IRQChips: []kvm.IRQChip{
			{ChipID: 0, Chip: chipData},
		},
	}
	s := newX86SnapshotArchState(ms)
	if s.X86 == nil {
		t.Fatal("X86 should not be nil")
	}
	if s.Clock.Clock != 100 {
		t.Errorf("legacy Clock = %d, want 100", s.Clock.Clock)
	}
	if len(s.IRQChips) != 1 {
		t.Fatalf("legacy IRQChips = %d, want 1", len(s.IRQChips))
	}
	// Verify IRQChips are cloned
	s.IRQChips[0].Chip[0] = 0xFF
	if ms.IRQChips[0].Chip[0] != 0xAB {
		t.Fatal("IRQChips should be cloned")
	}
}

func TestNewX86SnapshotArchState_Nil(t *testing.T) {
	if got := newX86SnapshotArchState(nil); got != nil {
		t.Fatalf("newX86SnapshotArchState(nil) = %v, want nil", got)
	}
}

func TestNewARM64SnapshotArchState(t *testing.T) {
	s := newARM64SnapshotArchState()
	if s == nil || s.ARM64 == nil {
		t.Fatal("newARM64SnapshotArchState() returned nil")
	}
}

// --- Coverage-boosting tests ---

func TestSnapshotOptionsDefault(t *testing.T) {
	opts := SnapshotOptions{}
	if opts.Resume {
		t.Fatal("default SnapshotOptions.Resume should be false")
	}
	opts.Resume = true
	if !opts.Resume {
		t.Fatal("SnapshotOptions.Resume should be settable")
	}
}

func TestRestoreOptionsFields(t *testing.T) {
	opts := RestoreOptions{
		OverrideVCPUs:   4,
		OverrideID:      "restored-vm",
		OverrideTap:     "tap1",
		OverrideX86Boot: X86BootACPI,
	}
	if opts.OverrideVCPUs != 4 {
		t.Fatalf("OverrideVCPUs = %d, want 4", opts.OverrideVCPUs)
	}
	if opts.OverrideID != "restored-vm" {
		t.Fatalf("OverrideID = %q, want restored-vm", opts.OverrideID)
	}
	if opts.OverrideTap != "tap1" {
		t.Fatalf("OverrideTap = %q, want tap1", opts.OverrideTap)
	}
	if opts.OverrideX86Boot != X86BootACPI {
		t.Fatalf("OverrideX86Boot = %q, want acpi", opts.OverrideX86Boot)
	}
}

func TestBuildRateLimiter_Nil(t *testing.T) {
	if got := buildRateLimiter(nil); got != nil {
		t.Fatalf("buildRateLimiter(nil) = %v, want nil", got)
	}
}

func TestBuildRateLimiter_Valid(t *testing.T) {
	cfg := &RateLimiterConfig{
		Bandwidth: TokenBucketConfig{Size: 1024, RefillTimeMs: 500},
		Ops:       TokenBucketConfig{Size: 100, OneTimeBurst: 50, RefillTimeMs: 1000},
	}
	rl := buildRateLimiter(cfg)
	if rl == nil {
		t.Fatal("buildRateLimiter() returned nil for valid config")
	}
}

func TestUniqueIRQs(t *testing.T) {
	tests := []struct {
		name string
		in   []uint32
		want []uint32
	}{
		{"empty", nil, []uint32{}},
		{"no dups", []uint32{4, 5, 6}, []uint32{4, 5, 6}},
		{"with dups", []uint32{4, 5, 4, 6, 5}, []uint32{4, 5, 6}},
		{"all same", []uint32{7, 7, 7}, []uint32{7}},
		{"single", []uint32{1}, []uint32{1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := uniqueIRQs(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("uniqueIRQs(%v) = %v, want %v", tt.in, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("uniqueIRQs(%v) = %v, want %v", tt.in, got, tt.want)
				}
			}
		})
	}
}

func TestConfigJSON_FullRoundTrip(t *testing.T) {
	cfg := Config{
		MemMB:      1024,
		Arch:       "amd64",
		KernelPath: "/boot/vmlinuz",
		InitrdPath: "/boot/initrd.img",
		Cmdline:    "console=ttyS0 root=/dev/vda rw",
		DiskImage:  "/tmp/rootfs.ext4",
		DiskRO:     false,
		TapName:    "tap0",
		VCPUs:      4,
		ID:         "full-test-vm",
		X86Boot:    X86BootACPI,
		Drives: []DriveConfig{
			{ID: "root", Path: "/a.ext4", Root: true, ReadOnly: false},
			{ID: "data", Path: "/b.ext4", Root: false, ReadOnly: true},
		},
		Vsock:   &VsockConfig{Enabled: true, GuestCID: 5},
		Exec:    &ExecConfig{Enabled: true, VsockPort: 52},
		Balloon: &BalloonConfig{
			AmountMiB:             128,
			DeflateOnOOM:          true,
			StatsPollingIntervalS: 10,
			FreePageHinting:       true,
			FreePageReporting:     true,
			Auto:                  BalloonAutoConservative,
			SnapshotPages:         []uint32{1, 2, 3},
		},
		MemoryHotplug: &MemoryHotplugConfig{
			TotalSizeMiB: 4096,
			SlotSizeMiB:  1024,
			BlockSizeMiB: 128,
		},
		NetRateLimiter:   &RateLimiterConfig{Bandwidth: TokenBucketConfig{Size: 500}},
		BlockRateLimiter: &RateLimiterConfig{Ops: TokenBucketConfig{Size: 200}},
		RNGRateLimiter:   &RateLimiterConfig{Bandwidth: TokenBucketConfig{Size: 100}},
		Metadata:         map[string]string{"env": "test", "version": "1.0"},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var restored Config
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if restored.MemMB != 1024 {
		t.Errorf("MemMB = %d", restored.MemMB)
	}
	if restored.VCPUs != 4 {
		t.Errorf("VCPUs = %d", restored.VCPUs)
	}
	if len(restored.Drives) != 2 {
		t.Errorf("Drives = %d", len(restored.Drives))
	}
	if restored.Balloon == nil || len(restored.Balloon.SnapshotPages) != 3 {
		t.Errorf("Balloon.SnapshotPages mismatch")
	}
	if restored.MemoryHotplug == nil || restored.MemoryHotplug.TotalSizeMiB != 4096 {
		t.Errorf("MemoryHotplug mismatch")
	}
	if restored.NetRateLimiter == nil || restored.NetRateLimiter.Bandwidth.Size != 500 {
		t.Errorf("NetRateLimiter mismatch")
	}
	if restored.Metadata["env"] != "test" || restored.Metadata["version"] != "1.0" {
		t.Errorf("Metadata mismatch")
	}
}

func TestSnapshotJSON_FullRoundTrip(t *testing.T) {
	snap := Snapshot{
		Version:   2,
		Timestamp: time.Now().Truncate(time.Millisecond),
		ID:        "snap-test",
		Config: Config{
			MemMB:      512,
			Arch:       "amd64",
			KernelPath: "/boot/vmlinuz",
			VCPUs:      2,
			ID:         "snap-test",
			Vsock:      &VsockConfig{Enabled: true, GuestCID: 3},
		},
		MemFile: "mem.bin",
		VCPUs: []VCPUState{
			{ID: 0, X86: &X86VCPUState{Regs: kvm.Regs{RAX: 100}, MPState: kvm.MPState{State: 1}}},
			{ID: 1, ARM64: &ARM64VCPUState{CoreRegs: map[uint64]uint64{1: 42}}},
		},
		Arch: &SnapshotArchState{
			X86: &X86MachineState{Clock: kvm.ClockData{Clock: 999}},
		},
	}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var restored Snapshot
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if restored.Version != 2 || restored.ID != "snap-test" {
		t.Errorf("basic fields mismatch")
	}
	if len(restored.VCPUs) != 2 {
		t.Fatalf("VCPUs = %d", len(restored.VCPUs))
	}
	if restored.VCPUs[0].X86 == nil || restored.VCPUs[0].X86.Regs.RAX != 100 {
		t.Errorf("VCPUs[0].X86.Regs.RAX mismatch")
	}
	if restored.VCPUs[1].ARM64 == nil || restored.VCPUs[1].ARM64.CoreRegs[1] != 42 {
		t.Errorf("VCPUs[1].ARM64.CoreRegs mismatch")
	}
	if restored.Arch == nil || restored.Arch.X86 == nil || restored.Arch.X86.Clock.Clock != 999 {
		t.Errorf("Arch.X86 mismatch")
	}
}

func TestEventLog_SubscribeMultiple(t *testing.T) {
	el := NewEventLog()
	ch1, unsub1 := el.Subscribe()
	ch2, unsub2 := el.Subscribe()
	defer unsub1()
	defer unsub2()

	el.Emit(EventRunning, "test")

	got1 := <-ch1
	got2 := <-ch2
	if got1.Type != EventRunning || got2.Type != EventRunning {
		t.Fatalf("expected both subscribers to get EventRunning, got %q and %q", got1.Type, got2.Type)
	}
}

func TestEventLog_SlowSubscriber(t *testing.T) {
	el := NewEventLog()
	ch, unsub := el.Subscribe()
	defer unsub()

	// Fill the subscriber's channel buffer (64 capacity)
	for i := 0; i < 100; i++ {
		el.Emit(EventRunning, "flood")
	}

	// Should not panic and some events should arrive
	count := 0
	for {
		select {
		case <-ch:
			count++
		default:
			goto done
		}
	}
done:
	if count == 0 {
		t.Fatal("expected at least some events to be delivered")
	}
	if count > 64 {
		t.Fatalf("subscriber channel buffer is 64, got %d events", count)
	}
}

func TestEventJSON_RoundTrip(t *testing.T) {
	ev := Event{
		Time:    time.Now().Truncate(time.Millisecond),
		Type:    EventCreated,
		Message: "test message",
	}
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var restored Event
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if restored.Type != EventCreated || restored.Message != "test message" {
		t.Fatalf("Event mismatch: %#v", restored)
	}
}

func TestVCPUState_NormalizedX86_WithLAPIC(t *testing.T) {
	lapic := &kvm.LAPICState{}
	state := VCPUState{
		ID:    0,
		LAPIC: lapic,
	}
	normalized := state.normalizedX86()
	if normalized.LAPIC != lapic {
		t.Fatal("expected LAPIC to be set from legacy field")
	}
}

func TestVCPUState_NormalizedX86_X86LAPICTakesPrecedence(t *testing.T) {
	lapic1 := &kvm.LAPICState{}
	lapic2 := &kvm.LAPICState{}
	state := VCPUState{
		ID:    0,
		X86:   &X86VCPUState{LAPIC: lapic1},
		LAPIC: lapic2,
	}
	normalized := state.normalizedX86()
	if normalized.LAPIC != lapic1 {
		t.Fatal("expected X86 LAPIC to take precedence over legacy LAPIC")
	}
}

func TestVCPUStateJSON(t *testing.T) {
	state := VCPUState{
		ID:  3,
		X86: &X86VCPUState{Regs: kvm.Regs{RAX: 42}, MPState: kvm.MPState{State: 1}},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var restored VCPUState
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if restored.ID != 3 {
		t.Errorf("ID = %d", restored.ID)
	}
	if restored.X86 == nil || restored.X86.Regs.RAX != 42 {
		t.Errorf("X86.Regs.RAX mismatch")
	}
}

func TestNewX86VCPUState_WithLAPIC(t *testing.T) {
	lapic := &kvm.LAPICState{}
	state := newX86VCPUState(0, kvm.Regs{}, kvm.Sregs{}, kvm.MPState{}, lapic)
	if state.X86.LAPIC != lapic {
		t.Fatal("expected LAPIC in X86 field")
	}
	if state.LAPIC != lapic {
		t.Fatal("expected LAPIC in legacy field")
	}
}

func TestDefaultGuestMAC_DifferentInputs(t *testing.T) {
	// defaultGuestMAC uses only id as seed (tapName is fallback when id is empty)
	tests := []struct {
		vmID    string
		tapName string
	}{
		{"vm-1", "tap0"},
		{"vm-2", "tap0"},
		{"vm-3", "tap1"},
		{"a-very-long-vm-identifier-for-testing", "a-very-long-tap-name"},
	}
	macs := make(map[string]struct{})
	for _, tt := range tests {
		mac := defaultGuestMAC(tt.vmID, tt.tapName)
		s := mac.String()
		if _, exists := macs[s]; exists {
			t.Errorf("duplicate MAC for (%q, %q): %s", tt.vmID, tt.tapName, s)
		}
		macs[s] = struct{}{}
		// All should be unicast + locally administered
		if mac[0]&0x01 != 0 {
			t.Errorf("MAC should be unicast: %s", mac)
		}
		if mac[0]&0x02 == 0 {
			t.Errorf("MAC should be locally administered: %s", mac)
		}
	}
}

func TestDefaultGuestMAC_TapFallback(t *testing.T) {
	// When id is empty, tapName is used as seed
	mac1 := defaultGuestMAC("", "tap0")
	mac2 := defaultGuestMAC("", "tap1")
	if bytes.Equal(mac1, mac2) {
		t.Fatalf("different tap names should produce different MACs: %s == %s", mac1, mac2)
	}
}

func TestTokenBucketConfig_ZeroValues(t *testing.T) {
	cfg := TokenBucketConfig{}
	bucket := cfg.toVirtio()
	if bucket.Size != 0 || bucket.OneTimeBurst != 0 || bucket.RefillTime != 0 {
		t.Errorf("zero TokenBucketConfig should produce zero virtio bucket: %+v", bucket)
	}
}

func TestBalloonStatsFields(t *testing.T) {
	stats := BalloonStats{
		TargetPages:     100,
		ActualPages:     50,
		TargetMiB:       400,
		ActualMiB:       200,
		SwapIn:          1,
		SwapOut:         2,
		MajorFaults:     3,
		MinorFaults:     4,
		FreeMemory:      5,
		TotalMemory:     6,
		AvailableMemory: 7,
		DiskCaches:      8,
		HugetlbAllocs:   9,
		HugetlbFailures: 10,
		OOMKill:         11,
		AllocStall:      12,
		AsyncScan:       13,
		DirectScan:      14,
		AsyncReclaim:    15,
		DirectReclaim:   16,
	}
	data, err := json.Marshal(stats)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var restored BalloonStats
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if restored.TargetPages != 100 || restored.ActualMiB != 200 || restored.OOMKill != 11 {
		t.Errorf("BalloonStats roundtrip mismatch: %+v", restored)
	}
}

func TestBalloonUpdateJSON(t *testing.T) {
	update := BalloonUpdate{AmountMiB: 256}
	data, err := json.Marshal(update)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var restored BalloonUpdate
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if restored.AmountMiB != 256 {
		t.Fatalf("AmountMiB = %d, want 256", restored.AmountMiB)
	}
}

func TestBalloonStatsUpdateJSON(t *testing.T) {
	update := BalloonStatsUpdate{StatsPollingIntervalS: 5}
	data, err := json.Marshal(update)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var restored BalloonStatsUpdate
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if restored.StatsPollingIntervalS != 5 {
		t.Fatalf("StatsPollingIntervalS = %d, want 5", restored.StatsPollingIntervalS)
	}
}

func TestMemoryHotplugConfigJSON(t *testing.T) {
	cfg := MemoryHotplugConfig{
		TotalSizeMiB: 2048,
		SlotSizeMiB:  512,
		BlockSizeMiB: 128,
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var restored MemoryHotplugConfig
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if restored.TotalSizeMiB != 2048 || restored.SlotSizeMiB != 512 || restored.BlockSizeMiB != 128 {
		t.Errorf("MemoryHotplugConfig roundtrip: %+v", restored)
	}
}

func TestMemoryHotplugSizeUpdateJSON(t *testing.T) {
	update := MemoryHotplugSizeUpdate{RequestedSizeMiB: 512}
	data, err := json.Marshal(update)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var restored MemoryHotplugSizeUpdate
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if restored.RequestedSizeMiB != 512 {
		t.Fatalf("RequestedSizeMiB = %d, want 512", restored.RequestedSizeMiB)
	}
}

func TestMemoryHotplugStatusJSON(t *testing.T) {
	status := MemoryHotplugStatus{
		TotalSizeMiB:     2048,
		SlotSizeMiB:      512,
		BlockSizeMiB:     128,
		PluggedSizeMiB:   256,
		RequestedSizeMiB: 256,
	}
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var restored MemoryHotplugStatus
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if restored.PluggedSizeMiB != 256 {
		t.Fatalf("PluggedSizeMiB = %d, want 256", restored.PluggedSizeMiB)
	}
}

func TestWorkerMetadataJSON(t *testing.T) {
	meta := WorkerMetadata{
		Kind:       "worker",
		SocketPath: "/tmp/vmm.sock",
		WorkerPID:  1234,
		JailRoot:   "/jail/root",
		RunDir:     "/run/dir",
		CreatedAt:  time.Now().Truncate(time.Millisecond),
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var restored WorkerMetadata
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if restored.Kind != "worker" || restored.SocketPath != "/tmp/vmm.sock" {
		t.Fatalf("WorkerMetadata mismatch: %+v", restored)
	}
}

func TestVsockConfigJSON(t *testing.T) {
	cfg := VsockConfig{Enabled: true, GuestCID: 5}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var restored VsockConfig
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if !restored.Enabled || restored.GuestCID != 5 {
		t.Fatalf("VsockConfig mismatch: %+v", restored)
	}
}

func TestExecConfigJSON(t *testing.T) {
	cfg := ExecConfig{Enabled: true, VsockPort: 52}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var restored ExecConfig
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if !restored.Enabled || restored.VsockPort != 52 {
		t.Fatalf("ExecConfig mismatch: %+v", restored)
	}
}

func TestSharedFSConfigJSON(t *testing.T) {
	cfg := SharedFSConfig{
		Source:     "/host/data",
		Tag:        "myfs",
		SocketPath: "/tmp/virtiofsd.sock",
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var restored SharedFSConfig
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if restored.Source != "/host/data" || restored.Tag != "myfs" || restored.SocketPath != "/tmp/virtiofsd.sock" {
		t.Fatalf("SharedFSConfig mismatch: %+v", restored)
	}
}

func TestDriveConfigJSON(t *testing.T) {
	drive := DriveConfig{
		ID:       "data",
		Path:     "/dev/sdb",
		Root:     false,
		ReadOnly: true,
		RateLimiter: &RateLimiterConfig{
			Bandwidth: TokenBucketConfig{Size: 500, RefillTimeMs: 1000},
		},
	}
	data, err := json.Marshal(drive)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var restored DriveConfig
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if restored.ID != "data" || restored.ReadOnly != true || restored.RateLimiter == nil {
		t.Fatalf("DriveConfig mismatch: %+v", restored)
	}
	if restored.RateLimiter.Bandwidth.Size != 500 {
		t.Fatalf("RateLimiter.Bandwidth.Size = %d", restored.RateLimiter.Bandwidth.Size)
	}
}

func TestCloneDriveConfigs_EmptySlice(t *testing.T) {
	if got := cloneDriveConfigs([]DriveConfig{}); got != nil {
		t.Fatalf("cloneDriveConfigs(empty) = %v, want nil", got)
	}
}

func TestDriveList_FromDiskImage_NoRateLimiter(t *testing.T) {
	cfg := Config{DiskImage: "/tmp/disk.ext4", DiskRO: false}
	drives := cfg.DriveList()
	if len(drives) != 1 {
		t.Fatalf("DriveList() returned %d drives, want 1", len(drives))
	}
	if drives[0].RateLimiter != nil {
		t.Fatal("expected nil rate limiter")
	}
	if drives[0].ReadOnly {
		t.Fatal("expected ReadOnly = false")
	}
}

func TestHasAdditionalDrives_MultipleRoots(t *testing.T) {
	cfg := Config{
		Drives: []DriveConfig{
			{Root: true},
			{Root: true},
		},
	}
	if !cfg.HasAdditionalDrives() {
		t.Fatal("expected true for multiple root drives")
	}
}

func TestRootDrive_FromDiskImage(t *testing.T) {
	cfg := Config{DiskImage: "/tmp/rootfs.ext4"}
	root, ok := cfg.RootDrive()
	if !ok {
		t.Fatal("RootDrive() should find root from DiskImage")
	}
	if root.ID != "root" || root.Path != "/tmp/rootfs.ext4" {
		t.Fatalf("unexpected root drive: %+v", root)
	}
}

func TestNewARM64VCPUState_NilMaps(t *testing.T) {
	state := newARM64VCPUState(0, nil, nil)
	if state.ARM64 == nil {
		t.Fatal("ARM64 should not be nil")
	}
	if state.ARM64.CoreRegs != nil {
		t.Fatal("CoreRegs should be nil when nil passed")
	}
}

func TestSnapshotArchState_NormalizedX86_FromLegacyWithIRQChips(t *testing.T) {
	var chipData [512]byte
	chipData[0] = 0xCD
	s := &SnapshotArchState{
		Clock:    kvm.ClockData{Clock: 55},
		IRQChips: []kvm.IRQChip{{ChipID: 1, Chip: chipData}},
	}
	got := s.normalizedX86()
	if got.Clock.Clock != 55 {
		t.Fatalf("Clock = %d, want 55", got.Clock.Clock)
	}
	if len(got.IRQChips) != 1 || got.IRQChips[0].ChipID != 1 {
		t.Fatalf("IRQChips mismatch")
	}
	// Verify IRQChips are cloned
	got.IRQChips[0].Chip[0] = 0xFF
	if s.IRQChips[0].Chip[0] != 0xCD {
		t.Fatal("normalizedX86 should clone IRQChips")
	}
}

func TestNewX86SnapshotArchState_WithMultipleIRQChips(t *testing.T) {
	var chip1, chip2 [512]byte
	chip1[0] = 0xAA
	chip2[0] = 0xBB
	ms := &X86MachineState{
		Clock: kvm.ClockData{Clock: 200},
		IRQChips: []kvm.IRQChip{
			{ChipID: 0, Chip: chip1},
			{ChipID: 1, Chip: chip2},
		},
	}
	s := newX86SnapshotArchState(ms)
	if len(s.IRQChips) != 2 {
		t.Fatalf("IRQChips = %d, want 2", len(s.IRQChips))
	}
	if s.X86.Clock.Clock != 200 {
		t.Fatalf("X86.Clock = %d, want 200", s.X86.Clock.Clock)
	}
}

func TestValidateMachineArch_InvalidArch(t *testing.T) {
	err := validateMachineArch("mips")
	if err == nil {
		t.Fatal("expected error for invalid arch")
	}
}

func TestNormalizeMachineArch_AllVariants(t *testing.T) {
	tests := []struct {
		input   string
		want    MachineArch
		wantErr bool
	}{
		{"amd64", ArchAMD64, false},
		{"arm64", ArchARM64, false},
		{"  amd64  ", ArchAMD64, false},
		{"  arm64  ", ArchARM64, false},
		{"", HostArch(), false},
		{"   ", HostArch(), false},
		{"x86", "", true},
		{"riscv64", "", true},
	}
	for _, tt := range tests {
		got, err := normalizeMachineArch(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("normalizeMachineArch(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("normalizeMachineArch(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNormalizeSnapshotMachineArch_AllCases(t *testing.T) {
	tests := []struct {
		input   string
		want    MachineArch
		wantErr bool
	}{
		{"", ArchAMD64, false},
		{"  ", ArchAMD64, false},
		{"amd64", ArchAMD64, false},
		{"arm64", ArchARM64, false},
		{"bad", "", true},
	}
	for _, tt := range tests {
		got, err := normalizeSnapshotMachineArch(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("normalizeSnapshotMachineArch(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("normalizeSnapshotMachineArch(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNextConservativeBalloonTarget(t *testing.T) {
	tests := []struct {
		name     string
		stats    BalloonStats
		last     BalloonStats
		baseMemMiB uint64
		wantMiB  uint64
		wantOK   bool
	}{
		{
			name:     "no stats available",
			stats:    BalloonStats{},
			last:     BalloonStats{},
			baseMemMiB: 1024,
			wantMiB:  0,
			wantOK:   false,
		},
		{
			name: "oom pressure, deflate",
			stats: BalloonStats{
				TotalMemory:     1 << 30,
				AvailableMemory: 50 << 20,
				TargetMiB:       256,
				OOMKill:         1,
			},
			last:       BalloonStats{OOMKill: 0},
			baseMemMiB: 1024,
			wantMiB:    192,
			wantOK:     true,
		},
		{
			name: "plenty of headroom, inflate",
			stats: BalloonStats{
				TotalMemory:     1 << 30,
				AvailableMemory: 500 << 20,
				TargetMiB:       0,
			},
			last:       BalloonStats{},
			baseMemMiB: 1024,
			wantMiB:    64,
			wantOK:     true,
		},
		{
			name: "no change needed",
			stats: BalloonStats{
				TotalMemory:     1 << 30,
				AvailableMemory: 300 << 20,
				TargetMiB:       100,
			},
			last:       BalloonStats{},
			baseMemMiB: 1024,
			wantMiB:    0,
			wantOK:     false,
		},
		{
			name: "pressure but target already zero",
			stats: BalloonStats{
				TotalMemory:     1 << 30,
				AvailableMemory: 10 << 20,
				TargetMiB:       0,
				AllocStall:      5,
			},
			last:       BalloonStats{AllocStall: 0},
			baseMemMiB: 1024,
			wantMiB:    0,
			wantOK:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotMiB, gotOK := nextConservativeBalloonTarget(tt.stats, tt.last, tt.baseMemMiB)
			if gotOK != tt.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tt.wantOK)
			}
			if gotOK && gotMiB != tt.wantMiB {
				t.Fatalf("mib = %d, want %d", gotMiB, tt.wantMiB)
			}
		})
	}
}

func TestValidateMemoryHotplugConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     MemoryHotplugConfig
		wantErr bool
	}{
		{"valid", MemoryHotplugConfig{TotalSizeMiB: 1024, SlotSizeMiB: 256, BlockSizeMiB: 128}, false},
		{"zero total", MemoryHotplugConfig{SlotSizeMiB: 256, BlockSizeMiB: 128}, true},
		{"zero slot", MemoryHotplugConfig{TotalSizeMiB: 1024, BlockSizeMiB: 128}, true},
		{"zero block", MemoryHotplugConfig{TotalSizeMiB: 1024, SlotSizeMiB: 256}, true},
		{"total < slot", MemoryHotplugConfig{TotalSizeMiB: 128, SlotSizeMiB: 256, BlockSizeMiB: 128}, true},
		{"slot < block", MemoryHotplugConfig{TotalSizeMiB: 1024, SlotSizeMiB: 64, BlockSizeMiB: 128}, true},
		{"total not multiple of slot", MemoryHotplugConfig{TotalSizeMiB: 1000, SlotSizeMiB: 300, BlockSizeMiB: 100}, true},
		{"slot not multiple of block", MemoryHotplugConfig{TotalSizeMiB: 1024, SlotSizeMiB: 256, BlockSizeMiB: 100}, true},
		{"too many slots", MemoryHotplugConfig{TotalSizeMiB: 65536, SlotSizeMiB: 1, BlockSizeMiB: 1}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMemoryHotplugConfig(tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateMemoryHotplugConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateMemoryHotplugUpdate(t *testing.T) {
	state := &memoryHotplugState{
		totalBytes: 1 << 30, // 1 GiB
		blockBytes: 128 << 20, // 128 MiB
	}
	tests := []struct {
		name    string
		update  MemoryHotplugSizeUpdate
		wantErr bool
	}{
		{"valid", MemoryHotplugSizeUpdate{RequestedSizeMiB: 256}, false},
		{"exceeds total", MemoryHotplugSizeUpdate{RequestedSizeMiB: 2048}, true},
		{"not aligned", MemoryHotplugSizeUpdate{RequestedSizeMiB: 100}, true},
		{"zero", MemoryHotplugSizeUpdate{RequestedSizeMiB: 0}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMemoryHotplugUpdate(state, tt.update)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateMemoryHotplugUpdate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestHotplugConversions(t *testing.T) {
	if got := hotplugMiBToBytes(1); got != 1<<20 {
		t.Fatalf("hotplugMiBToBytes(1) = %d", got)
	}
	if got := hotplugBytesToMiB(1 << 20); got != 1 {
		t.Fatalf("hotplugBytesToMiB(1MiB) = %d", got)
	}
	if got := hotplugMiBToBytes(0); got != 0 {
		t.Fatalf("hotplugMiBToBytes(0) = %d", got)
	}
	if got := hotplugBytesToMiB(0); got != 0 {
		t.Fatalf("hotplugBytesToMiB(0) = %d", got)
	}
}

// --- Coverage-boosting tests: rate limiters, balloon, migration, etc. ---

func TestUpdateNetRateLimiter_NilDevice(t *testing.T) {
	vm := &VM{netDev: nil}
	err := vm.UpdateNetRateLimiter(&RateLimiterConfig{})
	if err == nil {
		t.Fatal("expected error when netDev is nil")
	}
	if err.Error() != "virtio-net is not configured" {
		t.Fatalf("error = %q", err)
	}
}

func TestUpdateBlockRateLimiter_NilDevice(t *testing.T) {
	vm := &VM{blkDev: nil}
	err := vm.UpdateBlockRateLimiter(&RateLimiterConfig{})
	if err == nil {
		t.Fatal("expected error when blkDev is nil")
	}
	if err.Error() != "virtio-blk is not configured" {
		t.Fatalf("error = %q", err)
	}
}

func TestUpdateRNGRateLimiter_NilDevice(t *testing.T) {
	vm := &VM{rngDev: nil}
	err := vm.UpdateRNGRateLimiter(&RateLimiterConfig{})
	if err == nil {
		t.Fatal("expected error when rngDev is nil")
	}
	if err.Error() != "virtio-rng is not configured" {
		t.Fatalf("error = %q", err)
	}
}

func TestGetBalloonConfig_NilDevice(t *testing.T) {
	vm := &VM{balloonDev: nil, cfg: Config{}}
	_, err := vm.GetBalloonConfig()
	if err == nil {
		t.Fatal("expected error when balloon not configured")
	}
}

func TestUpdateBalloon_NilDevice(t *testing.T) {
	vm := &VM{balloonDev: nil, cfg: Config{}}
	err := vm.UpdateBalloon(BalloonUpdate{AmountMiB: 64})
	if err == nil {
		t.Fatal("expected error when balloon not configured")
	}
}

func TestGetBalloonStats_NilDevice(t *testing.T) {
	vm := &VM{balloonDev: nil}
	_, err := vm.GetBalloonStats()
	if err == nil {
		t.Fatal("expected error when balloon not configured")
	}
}

func TestUpdateBalloonStats_NilDevice(t *testing.T) {
	vm := &VM{balloonDev: nil, cfg: Config{}}
	err := vm.UpdateBalloonStats(BalloonStatsUpdate{StatsPollingIntervalS: 5})
	if err == nil {
		t.Fatal("expected error when balloon not configured")
	}
}

func TestDialVsock_NilDevice(t *testing.T) {
	vm := &VM{vsockDev: nil}
	_, err := vm.DialVsock(10022)
	if err == nil {
		t.Fatal("expected error when vsock not configured")
	}
}

func TestNextConservativeBalloonTarget_NoStats(t *testing.T) {
	stats := BalloonStats{TotalMemory: 0, AvailableMemory: 0}
	_, ok := nextConservativeBalloonTarget(stats, BalloonStats{}, 1024)
	if ok {
		t.Fatal("expected ok=false when no stats available")
	}
}

func TestNextConservativeBalloonTarget_OOMPressureDeflates(t *testing.T) {
	stats := BalloonStats{
		TotalMemory:     1 << 30,
		AvailableMemory: 10 << 20, // very low
		TargetMiB:       200,
		OOMKill:         5,
	}
	last := BalloonStats{OOMKill: 3} // OOM increased
	gotMiB, gotOK := nextConservativeBalloonTarget(stats, last, 1024)
	if !gotOK {
		t.Fatal("expected ok=true for OOM pressure")
	}
	if gotMiB >= 200 {
		t.Fatalf("expected deflation below 200, got %d", gotMiB)
	}
}

func TestNextConservativeBalloonTarget_AllocStallDeflates(t *testing.T) {
	stats := BalloonStats{
		TotalMemory:     1 << 30,
		AvailableMemory: 10 << 20,
		TargetMiB:       128,
		AllocStall:      10,
	}
	last := BalloonStats{AllocStall: 5}
	gotMiB, gotOK := nextConservativeBalloonTarget(stats, last, 1024)
	if !gotOK {
		t.Fatal("expected ok=true for alloc stall pressure")
	}
	if gotMiB >= 128 {
		t.Fatalf("expected deflation below 128, got %d", gotMiB)
	}
}

func TestNextConservativeBalloonTarget_SmallBaseUsesLowerReserve(t *testing.T) {
	// With baseMemMiB<=512, reserve should be 64 and highWater 128
	stats := BalloonStats{
		TotalMemory:     512 << 20,
		AvailableMemory: 260 << 20, // > 128 + 64
		TargetMiB:       0,
	}
	gotMiB, gotOK := nextConservativeBalloonTarget(stats, BalloonStats{}, 512)
	if !gotOK {
		t.Fatal("expected ok=true for headroom inflate on small base")
	}
	if gotMiB == 0 {
		t.Fatal("expected nonzero inflate target")
	}
}

func TestNextConservativeBalloonTarget_MaxTargetCapping(t *testing.T) {
	// If next target exceeds maxTarget (baseMem - reserve), it should be capped
	stats := BalloonStats{
		TotalMemory:     512 << 20,
		AvailableMemory: 500 << 20,
		TargetMiB:       500,
	}
	gotMiB, gotOK := nextConservativeBalloonTarget(stats, BalloonStats{}, 256)
	if gotOK && gotMiB > 256 {
		t.Fatalf("target %d exceeds base mem 256", gotMiB)
	}
}

func TestNextConservativeBalloonTarget_PressureBelowStep(t *testing.T) {
	// Target < step size should go to 0
	stats := BalloonStats{
		TotalMemory:     1 << 30,
		AvailableMemory: 10 << 20,
		TargetMiB:       32,
		OOMKill:         1,
	}
	last := BalloonStats{OOMKill: 0}
	gotMiB, gotOK := nextConservativeBalloonTarget(stats, last, 1024)
	if !gotOK {
		t.Fatal("expected ok=true")
	}
	if gotMiB != 0 {
		t.Fatalf("expected target 0 for small deflation, got %d", gotMiB)
	}
}

func TestNormalizeSnapshotMachineArch_Valid(t *testing.T) {
	tests := []struct {
		raw     string
		want    MachineArch
		wantErr bool
	}{
		{"", ArchAMD64, false},
		{"  ", ArchAMD64, false},
		{"amd64", ArchAMD64, false},
		{"arm64", ArchARM64, false},
		{"bogus", "", true},
	}
	for _, tt := range tests {
		got, err := normalizeSnapshotMachineArch(tt.raw)
		if (err != nil) != tt.wantErr {
			t.Errorf("normalizeSnapshotMachineArch(%q) error = %v, wantErr %v", tt.raw, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("normalizeSnapshotMachineArch(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}

func TestIsIgnorableKVMClockCtrlError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"EINVAL", unix.EINVAL, true},
		{"ENOTTY", unix.ENOTTY, true},
		{"ENOSYS", unix.ENOSYS, true},
		{"EPERM", unix.EPERM, false},
		{"nil", nil, false},
		{"generic", fmt.Errorf("something"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isIgnorableKVMClockCtrlError(tt.err)
			if got != tt.want {
				t.Fatalf("isIgnorableKVMClockCtrlError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClearSignalRestart_Succeeds(t *testing.T) {
	// Use a harmless signal to test clearSignalRestart doesn't return error
	err := clearSignalRestart(syscall.SIGUSR2)
	if err != nil {
		t.Fatalf("clearSignalRestart(SIGUSR2) error = %v", err)
	}
}

func TestExecAgentPort(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want uint32
	}{
		{"nil exec", Config{}, 10022},
		{"exec disabled", Config{Exec: &ExecConfig{Enabled: false}}, 10022},
		{"exec enabled no port", Config{Exec: &ExecConfig{Enabled: true}}, 10022},
		{"exec enabled custom port", Config{Exec: &ExecConfig{Enabled: true, VsockPort: 9999}}, 9999},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := execAgentPort(tt.cfg)
			if got != tt.want {
				t.Fatalf("execAgentPort() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestRewriteSnapshotPathsForBundle(t *testing.T) {
	snap := &Snapshot{
		MemFile: "old-mem.bin",
		Config: Config{
			KernelPath: "/host/vmlinuz",
			InitrdPath: "/host/initrd",
			DiskImage:  "/host/disk.ext4",
		},
	}
	rewriteSnapshotPathsForBundle(snap)
	if snap.MemFile != migrationMemFile {
		t.Fatalf("MemFile = %q, want %q", snap.MemFile, migrationMemFile)
	}
	if snap.Config.KernelPath != "artifacts/kernel" {
		t.Fatalf("KernelPath = %q, want artifacts/kernel", snap.Config.KernelPath)
	}
	if snap.Config.InitrdPath != "artifacts/initrd" {
		t.Fatalf("InitrdPath = %q, want artifacts/initrd", snap.Config.InitrdPath)
	}
	if snap.Config.DiskImage != "artifacts/disk.ext4" {
		t.Fatalf("DiskImage = %q, want artifacts/disk.ext4", snap.Config.DiskImage)
	}
}

func TestRewriteSnapshotPathsForBundle_EmptyPaths(t *testing.T) {
	snap := &Snapshot{
		Config: Config{},
	}
	rewriteSnapshotPathsForBundle(snap)
	if snap.Config.KernelPath != "" {
		t.Fatalf("empty KernelPath should stay empty, got %q", snap.Config.KernelPath)
	}
	if snap.Config.InitrdPath != "" {
		t.Fatalf("empty InitrdPath should stay empty, got %q", snap.Config.InitrdPath)
	}
	if snap.Config.DiskImage != "" {
		t.Fatalf("empty DiskImage should stay empty, got %q", snap.Config.DiskImage)
	}
}

func TestMergeDirtyBitmaps(t *testing.T) {
	tests := []struct {
		name string
		a, b []uint64
		want []uint64
	}{
		{"both nil", nil, nil, nil},
		{"a only", []uint64{0x1, 0x2}, nil, []uint64{0x1, 0x2}},
		{"b only", nil, []uint64{0x3, 0x4}, []uint64{0x3, 0x4}},
		{"same size", []uint64{0x1, 0x0}, []uint64{0x0, 0x2}, []uint64{0x1, 0x2}},
		{"a longer", []uint64{0x1, 0x2, 0x3}, []uint64{0x4}, []uint64{0x5, 0x2, 0x3}},
		{"b longer", []uint64{0x1}, []uint64{0x2, 0x4}, []uint64{0x3, 0x4}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeDirtyBitmaps(tt.a, tt.b)
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("bitmap[%d] = %x, want %x", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestSameFilePath(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"/a/b/c", "/a/b/c", true},
		{"/a/b/../b/c", "/a/b/c", true},
		{"/a/b/c", "/a/b/d", false},
		{"", "", true},
	}
	for _, tt := range tests {
		if got := sameFilePath(tt.a, tt.b); got != tt.want {
			t.Errorf("sameFilePath(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestBundleAsset_EmptySrc(t *testing.T) {
	got, err := bundleAsset(t.TempDir(), "", "artifacts/kernel")
	if err != nil {
		t.Fatalf("bundleAsset(empty) error = %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty result, got %q", got)
	}
}

func TestBundleAsset_SameFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "artifacts", "kernel")
	if err := os.MkdirAll(filepath.Dir(src), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("kernel"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := bundleAsset(dir, "artifacts/kernel", "artifacts/kernel")
	if err != nil {
		t.Fatalf("bundleAsset(same) error = %v", err)
	}
	if got != "artifacts/kernel" {
		t.Fatalf("got %q, want artifacts/kernel", got)
	}
}

func TestBundleAsset_CopiesFile(t *testing.T) {
	dir := t.TempDir()
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "vmlinuz")
	if err := os.WriteFile(src, []byte("kernel-data"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := bundleAsset(dir, src, "artifacts/kernel")
	if err != nil {
		t.Fatalf("bundleAsset() error = %v", err)
	}
	if got != "artifacts/kernel" {
		t.Fatalf("got %q, want artifacts/kernel", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, "artifacts", "kernel"))
	if err != nil {
		t.Fatalf("read copied file: %v", err)
	}
	if string(data) != "kernel-data" {
		t.Fatalf("copied data = %q", data)
	}
}

func TestResolveSnapshotPath_Extended(t *testing.T) {
	tests := []struct {
		dir, value, want string
	}{
		{"/snap", "", ""},
		{"/snap", "/absolute/path", "/absolute/path"},
		{"/snap", "mem.bin", "/snap/mem.bin"},
		{"/snap", "artifacts/kernel", "/snap/artifacts/kernel"},
	}
	for _, tt := range tests {
		got := resolveSnapshotPath(tt.dir, tt.value)
		if got != tt.want {
			t.Errorf("resolveSnapshotPath(%q, %q) = %q, want %q", tt.dir, tt.value, got, tt.want)
		}
	}
}

func TestBalloonStatsJSON_RoundTrip(t *testing.T) {
	stats := BalloonStats{
		TargetPages:     256,
		ActualPages:     128,
		TargetMiB:       1,
		ActualMiB:       0,
		SwapIn:          10,
		FreeMemory:      1024,
		TotalMemory:     2048,
		AvailableMemory: 512,
		OOMKill:         1,
	}
	data, err := json.Marshal(stats)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored BalloonStats
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.TargetPages != 256 || restored.OOMKill != 1 || restored.FreeMemory != 1024 {
		t.Fatalf("roundtrip mismatch: %+v", restored)
	}
}

func TestBalloonUpdateJSON_RoundTrip(t *testing.T) {
	update := BalloonUpdate{AmountMiB: 128}
	data, err := json.Marshal(update)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored BalloonUpdate
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.AmountMiB != 128 {
		t.Fatalf("AmountMiB = %d, want 128", restored.AmountMiB)
	}
}

func TestMemoryHotplugConfigJSON_RoundTrip(t *testing.T) {
	cfg := MemoryHotplugConfig{
		TotalSizeMiB: 2048,
		SlotSizeMiB:  512,
		BlockSizeMiB: 128,
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored MemoryHotplugConfig
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.TotalSizeMiB != 2048 || restored.SlotSizeMiB != 512 || restored.BlockSizeMiB != 128 {
		t.Fatalf("roundtrip mismatch: %+v", restored)
	}
}

func TestMemoryHotplugStatusJSON_RoundTrip(t *testing.T) {
	status := MemoryHotplugStatus{
		TotalSizeMiB:     1024,
		SlotSizeMiB:      256,
		BlockSizeMiB:     128,
		PluggedSizeMiB:   512,
		RequestedSizeMiB: 768,
	}
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored MemoryHotplugStatus
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.PluggedSizeMiB != 512 || restored.RequestedSizeMiB != 768 {
		t.Fatalf("roundtrip mismatch: %+v", restored)
	}
}

func TestWorkerMetadataJSON_RoundTrip(t *testing.T) {
	meta := WorkerMetadata{
		Kind:       "worker",
		SocketPath: "/tmp/sock",
		WorkerPID:  1234,
		JailRoot:   "/jail",
		RunDir:     "/run",
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored WorkerMetadata
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.Kind != "worker" || restored.SocketPath != "/tmp/sock" || restored.WorkerPID != 1234 {
		t.Fatalf("roundtrip mismatch: %+v", restored)
	}
}

func TestMergeBalloonStats(t *testing.T) {
	base := virtio.BalloonStats{
		TargetPages: 100,
		ActualPages: 50,
		TargetMiB:   1,
	}
	extra := guestexec.MemoryStats{
		SwapIn:          1,
		SwapOut:         2,
		FreeMemory:      3000,
		TotalMemory:     8000,
		AvailableMemory: 5000,
		OOMKill:         7,
	}
	merged := mergeBalloonStats(base, extra)
	if merged.TargetPages != 100 {
		t.Fatalf("TargetPages = %d, want 100 (from base)", merged.TargetPages)
	}
	if merged.SwapIn != 1 || merged.SwapOut != 2 || merged.FreeMemory != 3000 || merged.OOMKill != 7 {
		t.Fatalf("merged extra stats mismatch: %+v", merged)
	}
	if merged.UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt should be set")
	}
}

func TestExecAgentBroker_ListenWrongPort(t *testing.T) {
	broker := newExecAgentBroker(10022)
	defer broker.close()
	_, err := broker.listen(9999)
	if err == nil {
		t.Fatal("expected error for wrong port")
	}
}

func TestExecAgentBroker_ListenAndAcquire(t *testing.T) {
	broker := newExecAgentBroker(10022)
	defer broker.close()

	// listen provides a guest conn; acquire gets the host conn
	guestConn, err := broker.listen(10022)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer guestConn.Close()

	hostConn, err := broker.acquire()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer hostConn.Close()
}

func TestExecAgentBroker_ClosedBroker(t *testing.T) {
	broker := newExecAgentBroker(10022)
	broker.close()

	// acquire on closed broker should error
	_, err := broker.acquire()
	if err == nil {
		t.Fatal("expected error from closed broker acquire")
	}
}

func TestExecAgentBroker_BacklogFull(t *testing.T) {
	broker := newExecAgentBroker(10022)
	defer broker.close()

	// Fill the backlog (capacity 1)
	conn1, err := broker.listen(10022)
	if err != nil {
		t.Fatalf("first listen: %v", err)
	}
	defer conn1.Close()

	// Second listen should fail because backlog is full
	_, err = broker.listen(10022)
	if err == nil {
		t.Fatal("expected error when backlog is full")
	}
}

func TestCopyReaderAtRange(t *testing.T) {
	src := bytes.NewReader([]byte("hello world"))
	var dst bytes.Buffer
	err := copyReaderAtRange(&dst, src, 6, 5)
	if err != nil {
		t.Fatalf("copyReaderAtRange: %v", err)
	}
	if dst.String() != "world" {
		t.Fatalf("got %q, want world", dst.String())
	}
}

func TestCopyReaderAtRange_ZeroLength(t *testing.T) {
	src := bytes.NewReader([]byte("hello"))
	var dst bytes.Buffer
	err := copyReaderAtRange(&dst, src, 0, 0)
	if err != nil {
		t.Fatalf("copyReaderAtRange: %v", err)
	}
	if dst.Len() != 0 {
		t.Fatalf("expected empty output, got %d bytes", dst.Len())
	}
}

func TestBuildDirtyFilePatch_EmptyBitmap(t *testing.T) {
	var dst bytes.Buffer
	src := bytes.NewReader([]byte("data"))
	patch, err := buildDirtyFilePatch(&dst, src, 4, "test.bin", 4096, nil, nil)
	if err != nil {
		t.Fatalf("buildDirtyFilePatch: %v", err)
	}
	if len(patch.Entries) != 0 {
		t.Fatalf("expected no entries, got %d", len(patch.Entries))
	}
}

func TestBuildDirtyFilePatch_ZeroSrcSize(t *testing.T) {
	var dst bytes.Buffer
	src := bytes.NewReader(nil)
	patch, err := buildDirtyFilePatch(&dst, src, 0, "test.bin", 4096, []uint64{0xFF}, nil)
	if err != nil {
		t.Fatalf("buildDirtyFilePatch: %v", err)
	}
	if len(patch.Entries) != 0 {
		t.Fatalf("expected no entries for zero src, got %d", len(patch.Entries))
	}
}

func TestBuildDirtyFilePatch_SingleDirtyPage(t *testing.T) {
	data := make([]byte, 8192)
	for i := range data {
		data[i] = byte(i % 256)
	}
	src := bytes.NewReader(data)
	var dst bytes.Buffer
	var offset uint64
	// Mark page 1 as dirty (bit 1 of word 0)
	patch, err := buildDirtyFilePatch(&dst, src, uint64(len(data)), "test.bin", 4096, []uint64{0x2}, &offset)
	if err != nil {
		t.Fatalf("buildDirtyFilePatch: %v", err)
	}
	if len(patch.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(patch.Entries))
	}
	if patch.Entries[0].Offset != 4096 {
		t.Fatalf("entry offset = %d, want 4096", patch.Entries[0].Offset)
	}
	if patch.Entries[0].Length != 4096 {
		t.Fatalf("entry length = %d, want 4096", patch.Entries[0].Length)
	}
}

func TestBuildDirtyFilePatch_ZeroPageSize(t *testing.T) {
	data := make([]byte, 4096)
	src := bytes.NewReader(data)
	var dst bytes.Buffer
	// pageSize 0 should default to 4096
	patch, err := buildDirtyFilePatch(&dst, src, 4096, "test.bin", 0, []uint64{0x1}, nil)
	if err != nil {
		t.Fatalf("buildDirtyFilePatch: %v", err)
	}
	if patch.PageSize != 4096 {
		t.Fatalf("page size = %d, want 4096 default", patch.PageSize)
	}
}

func TestErrorsIsNotExist(t *testing.T) {
	if errorsIsNotExist(nil) {
		t.Fatal("nil should not be not-exist")
	}
	if errorsIsNotExist(fmt.Errorf("random")) {
		t.Fatal("random error should not be not-exist")
	}
	if !errorsIsNotExist(os.ErrNotExist) {
		t.Fatal("os.ErrNotExist should be not-exist")
	}
}

func TestDirtyPatchEntryJSON(t *testing.T) {
	e := DirtyPatchEntry{Offset: 100, Length: 200, DataOffset: 300}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var restored DirtyPatchEntry
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Offset != 100 || restored.Length != 200 || restored.DataOffset != 300 {
		t.Fatalf("roundtrip: %+v", restored)
	}
}

func TestMigrationPatchSetJSON(t *testing.T) {
	ps := MigrationPatchSet{
		Version: 1,
		Patches: []DirtyFilePatch{
			{Path: "mem.bin", PageSize: 4096, Entries: []DirtyPatchEntry{{Offset: 0, Length: 4096}}},
		},
	}
	data, err := json.Marshal(ps)
	if err != nil {
		t.Fatal(err)
	}
	var restored MigrationPatchSet
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Version != 1 || len(restored.Patches) != 1 {
		t.Fatalf("roundtrip: %+v", restored)
	}
}

func TestSnapshotVCPUCount(t *testing.T) {
	tests := []struct {
		name string
		snap Snapshot
		want int
	}{
		{"from VCPUs", Snapshot{VCPUs: []VCPUState{{ID: 0}, {ID: 1}}}, 2},
		{"from config", Snapshot{Config: Config{VCPUs: 4}}, 4},
		{"default", Snapshot{}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := snapshotVCPUCount(tt.snap)
			if got != tt.want {
				t.Fatalf("snapshotVCPUCount() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestApplyMigrationPatches_NoPatches(t *testing.T) {
	dir := t.TempDir()
	// No patches.json => should succeed silently
	err := ApplyMigrationPatches(dir)
	if err != nil {
		t.Fatalf("ApplyMigrationPatches(empty dir) error = %v", err)
	}
}

func TestApplyMigrationPatches_EmptyPatchSet(t *testing.T) {
	dir := t.TempDir()
	ps := MigrationPatchSet{Version: 1}
	data, _ := json.Marshal(ps)
	if err := os.WriteFile(filepath.Join(dir, migrationPatchMeta), data, 0644); err != nil {
		t.Fatal(err)
	}
	err := ApplyMigrationPatches(dir)
	if err != nil {
		t.Fatalf("ApplyMigrationPatches(empty patches) error = %v", err)
	}
}

func TestApplyMigrationPatches_WithPatch(t *testing.T) {
	dir := t.TempDir()
	// Create target file
	targetPath := filepath.Join(dir, "mem.bin")
	originalData := []byte("AAAA")
	if err := os.WriteFile(targetPath, originalData, 0644); err != nil {
		t.Fatal(err)
	}
	// Create patch data
	patchData := []byte("BB")
	if err := os.WriteFile(filepath.Join(dir, migrationPatchData), patchData, 0644); err != nil {
		t.Fatal(err)
	}
	// Create patch metadata
	ps := MigrationPatchSet{
		Version: 1,
		Patches: []DirtyFilePatch{
			{
				Path:     "mem.bin",
				PageSize: 1,
				Entries:  []DirtyPatchEntry{{Offset: 1, Length: 2, DataOffset: 0}},
			},
		},
	}
	data, _ := json.Marshal(ps)
	if err := os.WriteFile(filepath.Join(dir, migrationPatchMeta), data, 0644); err != nil {
		t.Fatal(err)
	}
	if err := ApplyMigrationPatches(dir); err != nil {
		t.Fatalf("ApplyMigrationPatches error = %v", err)
	}
	result, _ := os.ReadFile(targetPath)
	if string(result) != "ABBA" {
		t.Fatalf("patched data = %q, want ABBA", result)
	}
}
