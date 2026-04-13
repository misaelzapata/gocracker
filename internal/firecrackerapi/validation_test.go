package firecrackerapi

import (
	"errors"
	"net/http"
	"testing"

	"github.com/gocracker/gocracker/pkg/vmm"
)

func TestErrorHelpersAndStatusCode(t *testing.T) {
	if got := StatusCode(errors.New("x"), http.StatusTeapot); got != http.StatusTeapot {
		t.Fatalf("StatusCode() = %d", got)
	}
	if got := StatusCode(InvalidArgumentf("bad"), http.StatusTeapot); got != http.StatusBadRequest {
		t.Fatalf("StatusCode(invalid arg) = %d", got)
	}
	if got := StatusCode(InvalidStatef("bad"), http.StatusTeapot); got != http.StatusConflict {
		t.Fatalf("StatusCode(invalid state) = %d", got)
	}
}

func TestValidateBootSource(t *testing.T) {
	if err := ValidateBootSource(BootSource{}); err == nil {
		t.Fatal("ValidateBootSource() error = nil")
	}
	if err := ValidateBootSource(BootSource{KernelImagePath: "/kernel", X86Boot: "bogus"}); err == nil {
		t.Fatal("ValidateBootSource(invalid boot) error = nil")
	}
	if err := ValidateBootSource(BootSource{KernelImagePath: "/kernel", X86Boot: string(vmm.X86BootLegacy)}); err != nil {
		t.Fatalf("ValidateBootSource() error = %v", err)
	}
}

func TestValidateMachineConfig(t *testing.T) {
	cases := []MachineConfig{
		{VcpuCount: -1},
		{VcpuCount: MaxVCPUCount + 1},
		{MemSizeMib: -1},
		{MemSizeMib: MinMemSizeMib - 1},
		{MemSizeMib: MaxMemSizeMib + 1},
	}
	for _, tc := range cases {
		if err := ValidateMachineConfig(tc); err == nil {
			t.Fatalf("ValidateMachineConfig(%+v) error = nil", tc)
		}
	}
	if err := ValidateMachineConfig(MachineConfig{VcpuCount: 2, MemSizeMib: MinMemSizeMib}); err != nil {
		t.Fatalf("ValidateMachineConfig(valid) error = %v", err)
	}
}

func TestValidateBalloonAndUpdates(t *testing.T) {
	if err := ValidateBalloon(Balloon{AmountMib: MaxMemSizeMib + 1}); err == nil {
		t.Fatal("ValidateBalloon(limit) error = nil")
	}
	if err := ValidateBalloon(Balloon{StatsPollingIntervalS: -1}); err == nil {
		t.Fatal("ValidateBalloon(interval) error = nil")
	}
	if err := ValidateBalloon(Balloon{FreePageHinting: true}); err == nil {
		t.Fatal("ValidateBalloon(hinting) error = nil")
	}
	if err := ValidateBalloon(Balloon{FreePageReporting: true}); err == nil {
		t.Fatal("ValidateBalloon(reporting) error = nil")
	}
	if err := ValidateBalloon(Balloon{AmountMib: 16}); err != nil {
		t.Fatalf("ValidateBalloon(valid) error = %v", err)
	}
	if err := ValidateBalloonUpdate(BalloonUpdate{AmountMib: MaxMemSizeMib + 1}); err == nil {
		t.Fatal("ValidateBalloonUpdate() error = nil")
	}
	if err := ValidateBalloonStatsUpdate(BalloonStatsUpdate{StatsPollingIntervalS: -1}); err == nil {
		t.Fatal("ValidateBalloonStatsUpdate() error = nil")
	}
}

func TestValidateMemoryHotplug(t *testing.T) {
	bad := []MemoryHotplugConfig{
		{},
		{TotalSizeMib: 1},
		{TotalSizeMib: 4, SlotSizeMib: 8, BlockSizeMib: 4},
		{TotalSizeMib: 8, SlotSizeMib: 4, BlockSizeMib: 8},
		{TotalSizeMib: 10, SlotSizeMib: 4, BlockSizeMib: 2},
		{TotalSizeMib: 8, SlotSizeMib: 6, BlockSizeMib: 4},
		{TotalSizeMib: MaxMemSizeMib + 1, SlotSizeMib: 1, BlockSizeMib: 1},
	}
	for _, tc := range bad {
		if err := ValidateMemoryHotplugConfig(tc); err == nil {
			t.Fatalf("ValidateMemoryHotplugConfig(%+v) error = nil", tc)
		}
	}
	if err := ValidateMemoryHotplugConfig(MemoryHotplugConfig{TotalSizeMib: 16, SlotSizeMib: 8, BlockSizeMib: 4}); err != nil {
		t.Fatalf("ValidateMemoryHotplugConfig(valid) error = %v", err)
	}
	if err := ValidateMemoryHotplugSizeUpdate(MemoryHotplugSizeUpdate{RequestedSizeMib: MaxMemSizeMib + 1}); err == nil {
		t.Fatal("ValidateMemoryHotplugSizeUpdate() error = nil")
	}
}

func TestValidateDriveAndNetworkInterface(t *testing.T) {
	if err := ValidateDrive(Drive{}); err == nil {
		t.Fatal("ValidateDrive() error = nil")
	}
	if err := ValidateDrive(Drive{DriveID: "root", PathOnHost: "/disk"}); err != nil {
		t.Fatalf("ValidateDrive(valid) error = %v", err)
	}
	if err := ValidateNetworkInterface(NetworkInterface{}); err == nil {
		t.Fatal("ValidateNetworkInterface() error = nil")
	}
	if err := ValidateNetworkInterface(NetworkInterface{IfaceID: "eth0", HostDevName: "tap0", GuestMAC: "nope"}); err == nil {
		t.Fatal("ValidateNetworkInterface(bad mac) error = nil")
	}
	if err := ValidateNetworkInterface(NetworkInterface{IfaceID: "eth0", HostDevName: "tap0", GuestMAC: "02:00:00:00:00:01"}); err != nil {
		t.Fatalf("ValidateNetworkInterface(valid) error = %v", err)
	}
}

func TestValidatePrebootForStart(t *testing.T) {
	base := PrebootConfig{
		BootSource:   &BootSource{KernelImagePath: "/kernel"},
		DefaultVCPUs: 1,
		DefaultMemMB: MinMemSizeMib,
	}
	if err := ValidatePrebootForStart(PrebootConfig{}); err == nil {
		t.Fatal("ValidatePrebootForStart(no boot) error = nil")
	}
	if err := ValidatePrebootForStart(PrebootConfig{
		BootSource: &BootSource{KernelImagePath: "/kernel"},
		Drives: []Drive{
			{DriveID: "a", PathOnHost: "/a"},
			{DriveID: "b", PathOnHost: "/b"},
		},
		DefaultVCPUs: 1,
		DefaultMemMB: MinMemSizeMib,
	}); err == nil {
		t.Fatal("ValidatePrebootForStart(missing root count) error = nil")
	}
	if err := ValidatePrebootForStart(PrebootConfig{
		BootSource: &BootSource{KernelImagePath: "/kernel"},
		Drives: []Drive{
			{DriveID: "a", PathOnHost: "/a", IsRootDevice: true},
			{DriveID: "b", PathOnHost: "/b", IsRootDevice: true},
		},
		DefaultVCPUs: 1,
		DefaultMemMB: MinMemSizeMib,
	}); err == nil {
		t.Fatal("ValidatePrebootForStart(multiple roots) error = nil")
	}
	if err := ValidatePrebootForStart(PrebootConfig{
		BootSource: &BootSource{KernelImagePath: "/kernel"},
		NetIfaces: []NetworkInterface{
			{IfaceID: "eth0", HostDevName: "tap0"},
			{IfaceID: "eth1", HostDevName: "tap1"},
		},
		DefaultVCPUs: 1,
		DefaultMemMB: MinMemSizeMib,
	}); err == nil {
		t.Fatal("ValidatePrebootForStart(multi iface) error = nil")
	}
	if err := ValidatePrebootForStart(PrebootConfig{
		BootSource: &BootSource{KernelImagePath: "/kernel"},
		DefaultVCPUs: 1,
		DefaultMemMB: MinMemSizeMib,
		Balloon: &Balloon{AmountMib: MinMemSizeMib + 1},
	}); err == nil {
		t.Fatal("ValidatePrebootForStart(balloon exceeds mem) error = nil")
	}
	if err := ValidatePrebootForStart(base); err != nil {
		t.Fatalf("ValidatePrebootForStart(valid) error = %v", err)
	}
}
