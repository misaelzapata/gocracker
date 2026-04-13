package firecrackerapi

import (
	"errors"
	"strings"
	"testing"
)

func TestErrorMethod(t *testing.T) {
	err := &Error{Kind: ErrorKindInvalidArgument, Message: "test error"}
	if err.Error() != "test error" {
		t.Fatalf("Error() = %q, want %q", err.Error(), "test error")
	}
}

func TestStatusCodeUnknownKind(t *testing.T) {
	err := &Error{Kind: "unknown_kind", Message: "x"}
	if got := StatusCode(err, 500); got != 500 {
		t.Fatalf("StatusCode(unknown_kind) = %d, want 500", got)
	}
}

func TestValidateBalloonUpdateValid(t *testing.T) {
	if err := ValidateBalloonUpdate(BalloonUpdate{AmountMib: 128}); err != nil {
		t.Fatalf("ValidateBalloonUpdate(valid) = %v", err)
	}
}

func TestValidateBalloonStatsUpdateValid(t *testing.T) {
	if err := ValidateBalloonStatsUpdate(BalloonStatsUpdate{StatsPollingIntervalS: 5}); err != nil {
		t.Fatalf("ValidateBalloonStatsUpdate(valid) = %v", err)
	}
}

func TestValidateMemoryHotplugSizeUpdateValid(t *testing.T) {
	if err := ValidateMemoryHotplugSizeUpdate(MemoryHotplugSizeUpdate{RequestedSizeMib: 256}); err != nil {
		t.Fatalf("ValidateMemoryHotplugSizeUpdate(valid) = %v", err)
	}
}

func TestValidateBootSourceLongBootArgs(t *testing.T) {
	longArgs := strings.Repeat("x", 4096)
	err := ValidateBootSource(BootSource{KernelImagePath: "/kernel", BootArgs: longArgs})
	if err == nil {
		t.Fatal("expected error for long boot_args")
	}
	if !strings.Contains(err.Error(), "cmdline") {
		t.Fatalf("error = %q, expected cmdline mention", err.Error())
	}
}

func TestValidateNetworkInterfaceEmptyHostDevName(t *testing.T) {
	err := ValidateNetworkInterface(NetworkInterface{IfaceID: "eth0"})
	if err == nil {
		t.Fatal("expected error for empty HostDevName")
	}
}

func TestValidateNetworkInterfaceEmptyMAC(t *testing.T) {
	err := ValidateNetworkInterface(NetworkInterface{IfaceID: "eth0", HostDevName: "tap0"})
	if err != nil {
		t.Fatalf("ValidateNetworkInterface(no MAC) = %v", err)
	}
}

func TestValidateDriveEmptyPathOnHost(t *testing.T) {
	err := ValidateDrive(Drive{DriveID: "root"})
	if err == nil {
		t.Fatal("expected error for empty PathOnHost")
	}
}

func TestValidatePrebootDuplicateDriveIDs(t *testing.T) {
	err := ValidatePrebootForStart(PrebootConfig{
		BootSource:   &BootSource{KernelImagePath: "/kernel"},
		DefaultVCPUs: 1,
		DefaultMemMB: MinMemSizeMib,
		Drives: []Drive{
			{DriveID: "a", PathOnHost: "/a", IsRootDevice: true},
			{DriveID: "a", PathOnHost: "/b"},
		},
	})
	if err == nil {
		t.Fatal("expected error for duplicate drive_id")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error = %q, expected duplicate mention", err.Error())
	}
}

func TestValidatePrebootWithMachineConfig(t *testing.T) {
	err := ValidatePrebootForStart(PrebootConfig{
		BootSource: &BootSource{KernelImagePath: "/kernel"},
		MachineCfg: &MachineConfig{VcpuCount: 4, MemSizeMib: 256},
	})
	if err != nil {
		t.Fatalf("ValidatePrebootForStart = %v", err)
	}
}

func TestValidatePrebootWithBadMachineConfig(t *testing.T) {
	err := ValidatePrebootForStart(PrebootConfig{
		BootSource: &BootSource{KernelImagePath: "/kernel"},
		MachineCfg: &MachineConfig{VcpuCount: MaxVCPUCount + 1},
		DefaultMemMB: MinMemSizeMib,
	})
	if err == nil {
		t.Fatal("expected error for invalid MachineConfig")
	}
}

func TestValidatePrebootWithBadBalloon(t *testing.T) {
	err := ValidatePrebootForStart(PrebootConfig{
		BootSource: &BootSource{KernelImagePath: "/kernel"},
		Balloon:    &Balloon{FreePageHinting: true},
		DefaultMemMB: MinMemSizeMib,
	})
	if err == nil {
		t.Fatal("expected error for invalid Balloon")
	}
}

func TestValidatePrebootWithBadMemoryHotplug(t *testing.T) {
	err := ValidatePrebootForStart(PrebootConfig{
		BootSource:    &BootSource{KernelImagePath: "/kernel"},
		MemoryHotplug: &MemoryHotplugConfig{},
		DefaultMemMB:  MinMemSizeMib,
	})
	if err == nil {
		t.Fatal("expected error for invalid MemoryHotplug")
	}
}

func TestValidatePrebootWithBadNetIface(t *testing.T) {
	err := ValidatePrebootForStart(PrebootConfig{
		BootSource: &BootSource{KernelImagePath: "/kernel"},
		NetIfaces:  []NetworkInterface{{IfaceID: ""}},
		DefaultMemMB: MinMemSizeMib,
	})
	if err == nil {
		t.Fatal("expected error for invalid NetIface")
	}
}

func TestValidatePrebootWithBadDrive(t *testing.T) {
	err := ValidatePrebootForStart(PrebootConfig{
		BootSource: &BootSource{KernelImagePath: "/kernel"},
		Drives:     []Drive{{DriveID: "", IsRootDevice: true}},
		DefaultMemMB: MinMemSizeMib,
	})
	if err == nil {
		t.Fatal("expected error for invalid Drive")
	}
}

func TestValidatePrebootDefaultsApplied(t *testing.T) {
	// When no MachineCfg, defaults should be used
	err := ValidatePrebootForStart(PrebootConfig{
		BootSource: &BootSource{KernelImagePath: "/kernel"},
	})
	if err != nil {
		t.Fatalf("ValidatePrebootForStart = %v", err)
	}
}

func TestValidatePrebootMemExceedsLimit(t *testing.T) {
	err := ValidatePrebootForStart(PrebootConfig{
		BootSource: &BootSource{KernelImagePath: "/kernel"},
		DefaultMemMB: MaxMemSizeMib + 1,
	})
	if err == nil {
		t.Fatal("expected error for mem exceeding limit")
	}
}

func TestValidatePrebootVCPUExceedsLimit(t *testing.T) {
	err := ValidatePrebootForStart(PrebootConfig{
		BootSource:   &BootSource{KernelImagePath: "/kernel"},
		DefaultVCPUs: MaxVCPUCount + 1,
		DefaultMemMB: MinMemSizeMib,
	})
	if err == nil {
		t.Fatal("expected error for vcpu exceeding limit")
	}
}

func TestValidatePrebootMemTooSmall(t *testing.T) {
	err := ValidatePrebootForStart(PrebootConfig{
		BootSource:   &BootSource{KernelImagePath: "/kernel"},
		DefaultMemMB: MinMemSizeMib - 1,
	})
	if err == nil {
		t.Fatal("expected error for mem too small")
	}
}

func TestInvalidArgumentfWrapsError(t *testing.T) {
	err := InvalidArgumentf("field %s invalid", "name")
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatal("expected *Error")
	}
	if apiErr.Kind != ErrorKindInvalidArgument {
		t.Fatalf("kind = %q", apiErr.Kind)
	}
}

func TestInvalidStatefWrapsError(t *testing.T) {
	err := InvalidStatef("vm %s not started", "gc-1")
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatal("expected *Error")
	}
	if apiErr.Kind != ErrorKindInvalidState {
		t.Fatalf("kind = %q", apiErr.Kind)
	}
}

func TestValidateMemoryHotplugSlotNotMultiple(t *testing.T) {
	err := ValidateMemoryHotplugConfig(MemoryHotplugConfig{
		TotalSizeMib: 16,
		SlotSizeMib:  7,
		BlockSizeMib: 1,
	})
	if err == nil {
		t.Fatal("expected error for slot not dividing total")
	}
}

func TestValidateMachineConfigZeroValues(t *testing.T) {
	// Zero values should pass (they mean "use defaults")
	if err := ValidateMachineConfig(MachineConfig{}); err != nil {
		t.Fatalf("ValidateMachineConfig(zero) = %v", err)
	}
}

func TestValidateBootSourceValidModes(t *testing.T) {
	modes := []string{"auto", "acpi", "legacy"}
	for _, mode := range modes {
		if err := ValidateBootSource(BootSource{KernelImagePath: "/k", X86Boot: mode}); err != nil {
			t.Fatalf("ValidateBootSource(mode=%s) = %v", mode, err)
		}
	}
}
