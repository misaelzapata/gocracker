package vmm

import (
	"bytes"
	"net"
	"runtime"
	"testing"
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

func TestNormalizeX86BootMode(t *testing.T) {
	tests := []struct {
		input X86BootMode
		want  X86BootMode
		err   bool
	}{
		{"", X86BootAuto, false},
		{X86BootAuto, X86BootAuto, false},
		{X86BootACPI, X86BootACPI, false},
		{X86BootLegacy, X86BootLegacy, false},
		{"invalid", "", true},
	}
	for _, tt := range tests {
		got, err := normalizeX86BootMode(tt.input)
		if (err != nil) != tt.err {
			t.Errorf("normalizeX86BootMode(%q) error = %v, wantErr %v", tt.input, err, tt.err)
		}
		if got != tt.want {
			t.Errorf("normalizeX86BootMode(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNormalizeMachineArch(t *testing.T) {
	tests := []struct {
		input string
		want  MachineArch
		err   bool
	}{
		{"", HostArch(), false},
		{"  ", HostArch(), false},
		{"amd64", ArchAMD64, false},
		{"arm64", ArchARM64, false},
		{"invalid", "", true},
	}
	for _, tt := range tests {
		got, err := normalizeMachineArch(tt.input)
		if (err != nil) != tt.err {
			t.Errorf("normalizeMachineArch(%q) error = %v, wantErr %v", tt.input, err, tt.err)
		}
		if got != tt.want {
			t.Errorf("normalizeMachineArch(%q) = %q, want %q", tt.input, got, tt.want)
		}
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

func TestHostArch(t *testing.T) {
	got := HostArch()
	if string(got) != runtime.GOARCH {
		t.Errorf("HostArch() = %q, want %q", got, runtime.GOARCH)
	}
}

func TestGuestRAMBase(t *testing.T) {
	if guestRAMBase("amd64") != 0 {
		t.Errorf("guestRAMBase(amd64) = %#x, want 0", guestRAMBase("amd64"))
	}
	if guestRAMBase("arm64") != 0x80000000 {
		t.Errorf("guestRAMBase(arm64) = %#x, want 0x80000000", guestRAMBase("arm64"))
	}
}

func TestValidateMachineArchRejectsCrossArch(t *testing.T) {
	// On any host, the opposite arch should be rejected.
	opposite := ArchARM64
	if HostArch() == ArchARM64 {
		opposite = ArchAMD64
	}
	if err := validateMachineArch(opposite); err == nil {
		t.Errorf("validateMachineArch(%q) = nil, want error on %s host", opposite, HostArch())
	}
}

func TestValidateMachineArchAcceptsSameArch(t *testing.T) {
	if err := validateMachineArch(HostArch()); err != nil {
		t.Errorf("validateMachineArch(%q) = %v, want nil", HostArch(), err)
	}
}

func TestCloneRateLimiterConfig(t *testing.T) {
	if cloneRateLimiterConfig(nil) != nil {
		t.Error("cloneRateLimiterConfig(nil) should return nil")
	}
	cfg := &RateLimiterConfig{
		Bandwidth: TokenBucketConfig{Size: 100, OneTimeBurst: 50, RefillTimeMs: 1000},
		Ops:       TokenBucketConfig{Size: 10},
	}
	cloned := cloneRateLimiterConfig(cfg)
	if cloned == cfg {
		t.Error("cloneRateLimiterConfig should return a different pointer")
	}
	if cloned.Bandwidth.Size != 100 || cloned.Ops.Size != 10 {
		t.Error("cloneRateLimiterConfig should copy values")
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

func TestConfigDriveList(t *testing.T) {
	cfg := Config{DiskImage: "/tmp/disk.ext4", DiskRO: true}
	drives := cfg.DriveList()
	if len(drives) != 1 {
		t.Fatalf("DriveList() len = %d, want 1", len(drives))
	}
	if drives[0].ID != "root" || !drives[0].ReadOnly {
		t.Errorf("DriveList()[0] = %+v, want root/readonly", drives[0])
	}
}

func TestConfigDriveListEmpty(t *testing.T) {
	cfg := Config{}
	if drives := cfg.DriveList(); drives != nil {
		t.Fatalf("DriveList() = %v, want nil for empty config", drives)
	}
}
