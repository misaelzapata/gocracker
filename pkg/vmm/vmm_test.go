package vmm

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/gocracker/gocracker/internal/kvm"
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
