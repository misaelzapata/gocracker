package firecrackerapi

import (
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/gocracker/gocracker/internal/runtimecfg"
	"github.com/gocracker/gocracker/pkg/vmm"
)

const (
	MaxVCPUCount  = 32
	MinMemSizeMib = 128
	MaxMemSizeMib = 1048576
)

type ErrorKind string

const (
	ErrorKindInvalidArgument ErrorKind = "invalid_argument"
	ErrorKindInvalidState    ErrorKind = "invalid_state"
)

type Error struct {
	Kind    ErrorKind
	Message string
}

func (e *Error) Error() string {
	return e.Message
}

func InvalidArgumentf(format string, args ...any) error {
	return &Error{Kind: ErrorKindInvalidArgument, Message: fmt.Sprintf(format, args...)}
}

func InvalidStatef(format string, args ...any) error {
	return &Error{Kind: ErrorKindInvalidState, Message: fmt.Sprintf(format, args...)}
}

func StatusCode(err error, fallback int) int {
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		return fallback
	}
	switch apiErr.Kind {
	case ErrorKindInvalidState:
		return 409
	case ErrorKindInvalidArgument:
		return 400
	default:
		return fallback
	}
}

type BootSource struct {
	KernelImagePath string
	BootArgs        string
	InitrdPath      string
	X86Boot         string
}

type MachineConfig struct {
	VcpuCount  int
	MemSizeMib int
}

type Balloon struct {
	AmountMib             uint64
	DeflateOnOOM          bool
	StatsPollingIntervalS int
	FreePageHinting       bool
	FreePageReporting     bool
}

type BalloonUpdate struct {
	AmountMib uint64
}

type BalloonStatsUpdate struct {
	StatsPollingIntervalS int
}

type MemoryHotplugConfig struct {
	TotalSizeMib uint64
	SlotSizeMib  uint64
	BlockSizeMib uint64
}

type MemoryHotplugSizeUpdate struct {
	RequestedSizeMib uint64
}

type Drive struct {
	DriveID      string
	PathOnHost   string
	IsRootDevice bool
}

type NetworkInterface struct {
	IfaceID     string
	HostDevName string
	GuestMAC    string
}

type PrebootConfig struct {
	BootSource   *BootSource
	MachineCfg   *MachineConfig
	Balloon      *Balloon
	MemoryHotplug *MemoryHotplugConfig
	Drives       []Drive
	NetIfaces    []NetworkInterface
	DefaultVCPUs int
	DefaultMemMB uint64
}

func ValidateBootSource(spec BootSource) error {
	if strings.TrimSpace(spec.KernelImagePath) == "" {
		return InvalidArgumentf("kernel_image_path is required")
	}
	if spec.X86Boot != "" {
		switch vmm.X86BootMode(strings.TrimSpace(spec.X86Boot)) {
		case vmm.X86BootAuto, vmm.X86BootACPI, vmm.X86BootLegacy:
		default:
			return InvalidArgumentf("invalid x86 boot mode %q", spec.X86Boot)
		}
	}
	if len(spec.BootArgs)+1 > runtimecfg.KernelCmdlineMax {
		return InvalidArgumentf(
			"boot_args exceeds kernel cmdline limit: %d bytes > %d",
			len(spec.BootArgs)+1,
			runtimecfg.KernelCmdlineMax,
		)
	}
	return nil
}

func ValidateMachineConfig(spec MachineConfig) error {
	if spec.VcpuCount < 0 {
		return InvalidArgumentf("vcpu_count must be positive")
	}
	if spec.VcpuCount > MaxVCPUCount {
		return InvalidArgumentf("vcpu_count exceeds limit %d", MaxVCPUCount)
	}
	if spec.MemSizeMib < 0 {
		return InvalidArgumentf("mem_size_mib must be positive")
	}
	if spec.MemSizeMib > 0 && spec.MemSizeMib < MinMemSizeMib {
		return InvalidArgumentf("mem_size_mib must be at least %d", MinMemSizeMib)
	}
	if spec.MemSizeMib > MaxMemSizeMib {
		return InvalidArgumentf("mem_size_mib exceeds limit %d", MaxMemSizeMib)
	}
	return nil
}

func ValidateBalloon(spec Balloon) error {
	if spec.AmountMib > MaxMemSizeMib {
		return InvalidArgumentf("balloon amount_mib exceeds limit %d", MaxMemSizeMib)
	}
	if spec.StatsPollingIntervalS < 0 {
		return InvalidArgumentf("stats_polling_interval_s must be positive")
	}
	if spec.FreePageHinting {
		return InvalidArgumentf("free_page_hinting is not supported")
	}
	if spec.FreePageReporting {
		return InvalidArgumentf("free_page_reporting is not supported")
	}
	return nil
}

func ValidateBalloonUpdate(spec BalloonUpdate) error {
	if spec.AmountMib > MaxMemSizeMib {
		return InvalidArgumentf("balloon amount_mib exceeds limit %d", MaxMemSizeMib)
	}
	return nil
}

func ValidateBalloonStatsUpdate(spec BalloonStatsUpdate) error {
	if spec.StatsPollingIntervalS < 0 {
		return InvalidArgumentf("stats_polling_interval_s must be positive")
	}
	return nil
}

func ValidateMemoryHotplugConfig(spec MemoryHotplugConfig) error {
	if spec.TotalSizeMib == 0 {
		return InvalidArgumentf("total_size_mib is required")
	}
	if spec.SlotSizeMib == 0 {
		return InvalidArgumentf("slot_size_mib is required")
	}
	if spec.BlockSizeMib == 0 {
		return InvalidArgumentf("block_size_mib is required")
	}
	if spec.TotalSizeMib < spec.SlotSizeMib {
		return InvalidArgumentf("total_size_mib must be >= slot_size_mib")
	}
	if spec.SlotSizeMib < spec.BlockSizeMib {
		return InvalidArgumentf("slot_size_mib must be >= block_size_mib")
	}
	if spec.TotalSizeMib%spec.SlotSizeMib != 0 {
		return InvalidArgumentf("total_size_mib must be a multiple of slot_size_mib")
	}
	if spec.SlotSizeMib%spec.BlockSizeMib != 0 {
		return InvalidArgumentf("slot_size_mib must be a multiple of block_size_mib")
	}
	if spec.TotalSizeMib > MaxMemSizeMib {
		return InvalidArgumentf("total_size_mib exceeds limit %d", MaxMemSizeMib)
	}
	return nil
}

func ValidateMemoryHotplugSizeUpdate(spec MemoryHotplugSizeUpdate) error {
	if spec.RequestedSizeMib > MaxMemSizeMib {
		return InvalidArgumentf("requested_size_mib exceeds limit %d", MaxMemSizeMib)
	}
	return nil
}

func ValidateDrive(spec Drive) error {
	if strings.TrimSpace(spec.DriveID) == "" {
		return InvalidArgumentf("drive_id is required")
	}
	if strings.TrimSpace(spec.PathOnHost) == "" {
		return InvalidArgumentf("path_on_host is required")
	}
	return nil
}

func ValidateNetworkInterface(spec NetworkInterface) error {
	if strings.TrimSpace(spec.IfaceID) == "" {
		return InvalidArgumentf("iface_id is required")
	}
	if strings.TrimSpace(spec.HostDevName) == "" {
		return InvalidArgumentf("host_dev_name is required")
	}
	if mac := strings.TrimSpace(spec.GuestMAC); mac != "" {
		if _, err := net.ParseMAC(mac); err != nil {
			return InvalidArgumentf("invalid guest_mac: %v", err)
		}
	}
	return nil
}

func ValidatePrebootForStart(spec PrebootConfig) error {
	if spec.BootSource == nil {
		return InvalidArgumentf("boot-source not configured")
	}
	if err := ValidateBootSource(*spec.BootSource); err != nil {
		return err
	}
	if spec.MachineCfg != nil {
		if err := ValidateMachineConfig(*spec.MachineCfg); err != nil {
			return err
		}
	}
	if spec.Balloon != nil {
		if err := ValidateBalloon(*spec.Balloon); err != nil {
			return err
		}
	}
	if spec.MemoryHotplug != nil {
		if err := ValidateMemoryHotplugConfig(*spec.MemoryHotplug); err != nil {
			return err
		}
	}
	if len(spec.NetIfaces) > 1 {
		return InvalidArgumentf("multiple network interfaces are not supported")
	}
	rootDrives := 0
	seenDriveIDs := map[string]struct{}{}
	for _, drive := range spec.Drives {
		if err := ValidateDrive(drive); err != nil {
			return err
		}
		if _, ok := seenDriveIDs[drive.DriveID]; ok {
			return InvalidArgumentf("duplicate drive_id %q", drive.DriveID)
		}
		seenDriveIDs[drive.DriveID] = struct{}{}
		if drive.IsRootDevice {
			rootDrives++
			if rootDrives > 1 {
				return InvalidArgumentf("multiple root drives are not supported")
			}
		}
	}
	if len(spec.Drives) > 0 && rootDrives == 0 {
		return InvalidArgumentf("exactly one root drive is required when drives are configured")
	}
	for _, iface := range spec.NetIfaces {
		if err := ValidateNetworkInterface(iface); err != nil {
			return err
		}
	}

	finalVCPUs := spec.DefaultVCPUs
	if finalVCPUs <= 0 {
		finalVCPUs = 1
	}
	finalMem := spec.DefaultMemMB
	if finalMem == 0 {
		finalMem = 128
	}
	if spec.MachineCfg != nil {
		if spec.MachineCfg.VcpuCount > 0 {
			finalVCPUs = spec.MachineCfg.VcpuCount
		}
		if spec.MachineCfg.MemSizeMib > 0 {
			finalMem = uint64(spec.MachineCfg.MemSizeMib)
		}
	}
	if finalVCPUs > MaxVCPUCount {
		return InvalidArgumentf("vcpu_count exceeds limit %d", MaxVCPUCount)
	}
	if finalMem < MinMemSizeMib {
		return InvalidArgumentf("mem_size_mib must be at least %d", MinMemSizeMib)
	}
	if finalMem > MaxMemSizeMib {
		return InvalidArgumentf("mem_size_mib exceeds limit %d", MaxMemSizeMib)
	}
	if spec.Balloon != nil && spec.Balloon.AmountMib > finalMem {
		return InvalidArgumentf("balloon amount_mib exceeds guest memory %d", finalMem)
	}
	return nil
}
