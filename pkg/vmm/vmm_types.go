package vmm

import (
	"crypto/sha1"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"time"
)

// X86BootMode selects the x86 boot path.
type X86BootMode string

// MachineArch identifies the guest architecture.
type MachineArch string
type NetworkAttachmentMode string

const (
	X86BootAuto   X86BootMode = "auto"
	X86BootACPI   X86BootMode = "acpi"
	X86BootLegacy X86BootMode = "legacy"

	ArchAMD64 MachineArch = "amd64"
	ArchARM64 MachineArch = "arm64"

	NetworkAttachmentNAT   NetworkAttachmentMode = "nat"
	NetworkAttachmentStack NetworkAttachmentMode = "stack"
)

func normalizeX86BootMode(mode X86BootMode) (X86BootMode, error) {
	switch mode {
	case "":
		return X86BootAuto, nil
	case X86BootAuto, X86BootACPI, X86BootLegacy:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid x86 boot mode %q", mode)
	}
}

// HostArch returns the architecture of the current host.
func HostArch() MachineArch {
	return MachineArch(runtime.GOARCH)
}

func normalizeMachineArch(raw string) (MachineArch, error) {
	arch := strings.TrimSpace(raw)
	if arch == "" {
		return HostArch(), nil
	}
	switch MachineArch(arch) {
	case ArchAMD64, ArchARM64:
		return MachineArch(arch), nil
	default:
		return "", fmt.Errorf("invalid arch %q", raw)
	}
}

func normalizeSnapshotMachineArch(raw string) (MachineArch, error) {
	if strings.TrimSpace(raw) == "" {
		// Backward compatibility for snapshots created before arch persistence
		// existed; those snapshots are x86/amd64 only.
		return ArchAMD64, nil
	}
	return normalizeMachineArch(raw)
}

// guestRAMBase returns the guest physical address where RAM starts.
// x86 maps RAM at GPA 0; ARM64 at 0x80000000 (Firecracker convention).
func guestRAMBase(arch string) uint64 {
	if MachineArch(arch) == ArchARM64 {
		return 0x80000000 // Firecracker DRAM_MEM_START
	}
	return 0
}

func validateMachineArch(arch MachineArch) error {
	host := HostArch()
	if arch != host {
		return fmt.Errorf("arch %q is not compatible with host arch %q (same-arch only)", arch, host)
	}
	return nil
}

// Config holds everything needed to create a VM.
type Config struct {
	MemMB            uint64
	Arch             string `json:"arch,omitempty"`
	KernelPath       string
	InitrdPath       string
	Cmdline          string
	DiskImage        string
	DiskRO           bool
	Drives           []DriveConfig `json:"drives,omitempty"`
	TapName          string
	Network          *NetworkConfig `json:"network,omitempty"`
	MACAddr          net.HardwareAddr
	Metadata         map[string]string  `json:"metadata,omitempty"`
	NetRateLimiter   *RateLimiterConfig `json:"net_rate_limiter,omitempty"`
	BlockRateLimiter *RateLimiterConfig `json:"block_rate_limiter,omitempty"`
	RNGRateLimiter   *RateLimiterConfig `json:"rng_rate_limiter,omitempty"`
	VCPUs            int
	X86Boot          X86BootMode
	SharedFS         []SharedFSConfig
	ID               string               // unique VM identifier
	ConsoleOut       io.Writer            `json:"-"`
	ConsoleIn        io.Reader            `json:"-"`
	Vsock            *VsockConfig         `json:"vsock,omitempty"`
	Exec             *ExecConfig          `json:"exec,omitempty"`
	Balloon          *BalloonConfig       `json:"balloon,omitempty"`
	MemoryHotplug    *MemoryHotplugConfig `json:"memory_hotplug,omitempty"`
}

type NetworkConfig struct {
	Mode      NetworkAttachmentMode `json:"mode,omitempty"`
	NetworkID string                `json:"network_id,omitempty"`
}

// VsockConfig configures the virtio-vsock device.
type VsockConfig struct {
	Enabled  bool   `json:"enabled,omitempty"`
	GuestCID uint32 `json:"guest_cid,omitempty"`
}

// ExecConfig configures in-guest command execution via vsock.
type ExecConfig struct {
	Enabled   bool   `json:"enabled,omitempty"`
	VsockPort uint32 `json:"vsock_port,omitempty"`
}

// SharedFSConfig configures a shared directory between host and guest.
type SharedFSConfig struct {
	Source string `json:"source"`
	Tag    string `json:"tag"`
	// SocketPath, when set, points to an already-listening virtiofsd unix socket.
	// In that case the VM does not spawn virtiofsd; it connects to this socket
	// instead. Used by the worker/jailer path so virtiofsd can run on the host
	// (where its binary is reachable) while the VMM runs jailed.
	SocketPath string `json:"socket_path,omitempty"`
}

// TokenBucketConfig configures a token bucket for rate limiting.
type TokenBucketConfig struct {
	Size         uint64 `json:"size,omitempty"`
	OneTimeBurst uint64 `json:"one_time_burst,omitempty"`
	RefillTimeMs uint64 `json:"refill_time_ms,omitempty"`
}

// RateLimiterConfig configures rate limiting for a device.
type RateLimiterConfig struct {
	Bandwidth TokenBucketConfig `json:"bandwidth,omitempty"`
	Ops       TokenBucketConfig `json:"ops,omitempty"`
}

func cloneRateLimiterConfig(cfg *RateLimiterConfig) *RateLimiterConfig {
	if cfg == nil {
		return nil
	}
	cloned := *cfg
	return &cloned
}

// State tracks the lifecycle of a VM.
type State int

const (
	StateCreated State = iota
	StateRunning
	StatePaused
	StateStopped
)

func (s State) String() string {
	return [...]string{"created", "running", "paused", "stopped"}[s]
}

// SnapshotOptions controls snapshot behavior.
type SnapshotOptions struct {
	Resume bool
}

// RestoreOptions overrides settings when restoring a snapshot.
type RestoreOptions struct {
	ConsoleIn       io.Reader
	ConsoleOut      io.Writer
	OverrideVCPUs   int
	OverrideID      string
	OverrideTap     string
	OverrideX86Boot X86BootMode
}

func defaultGuestMAC(id, tapName string) net.HardwareAddr {
	seed := id
	if seed == "" {
		seed = tapName
	}
	if seed == "" {
		return net.HardwareAddr{0x06, 0x00, 0xAC, 0x10, 0x00, 0x02}
	}
	sum := sha1.Sum([]byte(seed))
	return net.HardwareAddr{0x06, sum[0], sum[1], sum[2], sum[3], sum[4]}
}

// defaultPauseTimeout is used by the KVM and vz backends.
const defaultPauseTimeout = 2 * time.Second
