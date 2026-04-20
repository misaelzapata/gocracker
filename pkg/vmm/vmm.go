// Package vmm is the core Virtual Machine Monitor.
// It wires together KVM, virtio devices, UART, FDT, kernel loader,
// and snapshot/restore.
package vmm

import (
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/gocracker/gocracker/internal/acpi"
	"github.com/gocracker/gocracker/internal/arm64layout"
	"github.com/gocracker/gocracker/internal/i8042"
	"github.com/gocracker/gocracker/internal/kvm"
	"github.com/gocracker/gocracker/internal/loader"
	gclog "github.com/gocracker/gocracker/internal/log"
	"github.com/gocracker/gocracker/internal/mptable"
	"github.com/gocracker/gocracker/internal/runtimecfg"
	"github.com/gocracker/gocracker/internal/seccomp"
	"github.com/gocracker/gocracker/internal/uart"
	"github.com/gocracker/gocracker/internal/virtio"
	"github.com/gocracker/gocracker/internal/vsock"
	"golang.org/x/sys/unix"
)

// Fixed guest memory layout
const (
	BootParamsAddr = 0x7000
	PageTableBase  = 0x9000 // ~20 KiB for 4-level page tables
	CmdlineAddr    = 0x20000
	InitrdAddr     = 0x1000000 // 16 MiB
	KernelLoad     = 0x100000  // 1 MiB — standard bzImage load address

	// MMIO layout for virtio devices
	VirtioBase    = 0xD0000000
	VirtioStride  = 0x1000
	VirtioIRQBase = 5

	// UART
	COM1Base = 0x3F8
	COM1IRQ  = 4

	// Legacy keyboard controller used by guests to request reboot.
	I8042Base = i8042.Base
)

const (
	defaultPauseTimeout = 2 * time.Second
	vcpuKickSignal      = syscall.SIGUSR1
	linuxSARestart      = 0x10000000
)

var ignoreVCPUKickSignalOnce sync.Once
var vcpuKickSignalCh = make(chan os.Signal, 16)

type linuxSigaction struct {
	handler  uintptr
	flags    uint64
	restorer uintptr
	mask     uint64
}

type X86BootMode string

type MachineArch string

const (
	X86BootAuto   X86BootMode = "auto"
	X86BootACPI   X86BootMode = "acpi"
	X86BootLegacy X86BootMode = "legacy"

	ArchAMD64 MachineArch = "amd64"
	ArchARM64 MachineArch = "arm64"
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
// x86 maps RAM at GPA 0; ARM64 at 0x40000000 (QEMU virt convention).
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
	MemMB          uint64
	Arch           string `json:"arch,omitempty"`
	KernelPath     string
	InitrdPath     string
	Cmdline        string
	DiskImage      string
	DiskRO         bool
	Drives         []DriveConfig `json:"drives,omitempty"`
	TapName        string
	MACAddr        net.HardwareAddr
	Metadata       map[string]string  `json:"metadata,omitempty"`
	NetRateLimiter *RateLimiterConfig `json:"net_rate_limiter,omitempty"`
	// RxNetRateLimiter / TxNetRateLimiter allow separate host→guest and
	// guest→host shaping, matching Firecracker's `rx_rate_limiter` and
	// `tx_rate_limiter` fields. If both are nil, NetRateLimiter (if set)
	// applies to both directions. If either is set, it overrides the
	// generic field for that direction.
	RxNetRateLimiter *RateLimiterConfig `json:"rx_net_rate_limiter,omitempty"`
	TxNetRateLimiter *RateLimiterConfig `json:"tx_net_rate_limiter,omitempty"`
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
	// TrackDirtyPages enables KVM dirty page logging from VM start.
	// Only pages actually written by the guest are stored in the snapshot,
	// shrinking mem.bin from ~229 MB to ~40-80 MB for a typical alpine boot.
	// Restore uses MAP_PRIVATE of the resulting sparse file — clean (never-
	// written) pages fault in as zero, matching the original MAP_ANONYMOUS
	// starting state. Enable when WarmCapture is active.
	TrackDirtyPages bool `json:"-"`
}

type VsockConfig struct {
	Enabled  bool   `json:"enabled,omitempty"`
	GuestCID uint32 `json:"guest_cid,omitempty"`
	UDSPath  string `json:"uds_path,omitempty"`
}

func (c *VsockConfig) Validate() error {
	if c == nil {
		return nil
	}
	if c.UDSPath != "" && !filepath.IsAbs(c.UDSPath) {
		return fmt.Errorf("vsock: uds_path must be absolute, got %q", c.UDSPath)
	}
	return nil
}

func defaultUDSPath(vmID, stateDir string) string {
	if vmID == "" || stateDir == "" {
		return ""
	}
	return filepath.Join(stateDir, "sandboxes", vmID+".sock")
}

type ExecConfig struct {
	Enabled   bool   `json:"enabled,omitempty"`
	VsockPort uint32 `json:"vsock_port,omitempty"`
}

type SharedFSConfig struct {
	Source string `json:"source"`
	Tag    string `json:"tag"`
	// Target is the guest-side mount point the template configured for this
	// tag. Populated by the container runtime so the snapshot-restore path
	// can re-identify the slot when a caller supplies SharedFSRebinds by
	// guest Target (the caller doesn't know the server-generated Tag).
	Target string `json:"target,omitempty"`
	// SocketPath, when set, points to an already-listening virtiofsd unix socket.
	// In that case the VM does not spawn virtiofsd; it connects to this socket
	// instead. Used by the worker/jailer path so virtiofsd can run on the host
	// (where its binary is reachable) while the VMM runs jailed.
	SocketPath string `json:"socket_path,omitempty"`
}

type TokenBucketConfig struct {
	Size         uint64 `json:"size,omitempty"`
	OneTimeBurst uint64 `json:"one_time_burst,omitempty"`
	RefillTimeMs uint64 `json:"refill_time_ms,omitempty"`
}

type RateLimiterConfig struct {
	Bandwidth TokenBucketConfig `json:"bandwidth,omitempty"`
	Ops       TokenBucketConfig `json:"ops,omitempty"`
}

func (cfg TokenBucketConfig) toVirtio() virtio.TokenBucket {
	return virtio.TokenBucket{
		Size:         cfg.Size,
		OneTimeBurst: cfg.OneTimeBurst,
		RefillTime:   time.Duration(cfg.RefillTimeMs) * time.Millisecond,
	}
}

func cloneRateLimiterConfig(cfg *RateLimiterConfig) *RateLimiterConfig {
	if cfg == nil {
		return nil
	}
	cloned := *cfg
	return &cloned
}

func buildRateLimiter(cfg *RateLimiterConfig) *virtio.RateLimiter {
	if cfg == nil {
		return nil
	}
	return virtio.NewRateLimiter(virtio.RateLimiterConfig{
		Bandwidth: cfg.Bandwidth.toVirtio(),
		Ops:       cfg.Ops.toVirtio(),
	})
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

// VM is a running virtual machine.
type VM struct {
	mu     sync.Mutex
	cfg    Config
	state  State
	stopCh chan struct{}
	doneCh chan struct{}

	kvmSys *kvm.System
	kvmVM  *kvm.VM
	vcpus  []*kvm.VCPU
	runWG  sync.WaitGroup

	archBackend machineArchBackend

	uart0          *uart.UART
	i8042          *i8042.Device
	pl011dev       any // reserved for future PL011 device, currently unused
	gicDev         any // *kvm.GICDevice on ARM64, nil on x86
	arm64GICLayout arm64layout.GICLayout
	irqEventFds    []int // eventfds for irqfd-based interrupt delivery (ARM64)
	transports     []*virtio.Transport
	rngDev         *virtio.RNGDevice
	balloonDev     *virtio.BalloonDevice
	memoryHotplug  *memoryHotplugState
	netDev         *virtio.NetDevice
	blkDev         *virtio.BlockDevice
	blkDevs        []*virtio.BlockDevice
	fsDevs         []*virtio.FSDevice
	vsockDev       *vsock.Device
	udsListener    *udsListener
	rtcDev         interface {
		ReadBytes(uint16, []byte)
		WriteBytes(uint16, []byte)
	}
	memDirty *virtio.DirtyTracker

	startTime   time.Time
	cleanupOnce sync.Once
	stopOnce    sync.Once
	// vsockDialMu protects against use-after-free when cleanup() races with
	// in-flight DialVsock calls.  DialVsock holds a read lock for the entire
	// dev.Dial call; cleanup() acquires the write lock before closing devices
	// and unmapping guest RAM, so it blocks until any concurrent dial completes.
	vsockDialMu sync.RWMutex
	exitCode    int
	events      *EventLog
	pausedVCPUs map[int]struct{}
	vcpuTIDs    map[int]int

	// restoring is set while setupDevices runs as part of snapshot restore.
	// It lets per-device constructors take shortcuts that are only safe when
	// the guest already negotiated virtio features (e.g. skipping the ~3ms
	// FALLOC_FL_PUNCH_HOLE probe on the block disk image). Cleared once the
	// restore returns to the cold-boot device construction rules.
	restoring bool
	// restoredDiscard[driveID] captures the discard feature flag from the
	// snapshot so NewBlockDeviceWithOptions can bypass the probe and still
	// advertise the right thing. Populated alongside `restoring = true`.
	restoredDiscard map[string]bool
}

// New creates a VM from config. Loads kernel, sets up devices and CPU.
// Does not start the vCPU yet.
func New(cfg Config) (*VM, error) {
	ignoreVCPUKickSignalOnce.Do(func() {
		signal.Notify(vcpuKickSignalCh, vcpuKickSignal)
		if err := clearSignalRestart(vcpuKickSignal); err != nil {
			gclog.VMM.Warn("failed to clear SA_RESTART on vcpu kick signal", "signal", vcpuKickSignal, "error", err)
		}
		go func() {
			for range vcpuKickSignalCh {
			}
		}()
	})
	if cfg.MemMB == 0 {
		cfg.MemMB = 128
	}
	if cfg.VCPUs <= 0 {
		cfg.VCPUs = 1
	}
	if cfg.ID == "" {
		cfg.ID = fmt.Sprintf("vm-%d", time.Now().UnixNano())
	}
	bootMode, err := normalizeX86BootMode(cfg.X86Boot)
	if err != nil {
		return nil, err
	}
	cfg.X86Boot = bootMode
	arch, err := normalizeMachineArch(cfg.Arch)
	if err != nil {
		return nil, err
	}
	if err := validateMachineArch(arch); err != nil {
		return nil, err
	}
	backend, err := newMachineArchBackend(arch)
	if err != nil {
		return nil, err
	}
	cfg.Arch = string(arch)
	if cfg.Vsock != nil && cfg.Vsock.Enabled && cfg.Vsock.GuestCID == 0 {
		cfg.Vsock.GuestCID = vsock.GuestCID
	}
	if cfg.Exec != nil && cfg.Exec.Enabled && cfg.Exec.VsockPort == 0 {
		cfg.Exec.VsockPort = runtimecfg.DefaultExecVsockPort
	}
	if cfg.Balloon != nil && cfg.Balloon.Auto == "" {
		cfg.Balloon.Auto = BalloonAutoOff
	}

	sys, err := kvm.Open()
	if err != nil {
		return nil, fmt.Errorf("kvm: %w", err)
	}
	kvmVM, err := sys.CreateVMWithBase(cfg.MemMB, guestRAMBase(cfg.Arch))
	if err != nil {
		return nil, fmt.Errorf("create vm: %w", err)
	}
	m := &VM{
		cfg:         cfg,
		state:       StateCreated,
		stopCh:      make(chan struct{}),
		doneCh:      make(chan struct{}),
		kvmSys:      sys,
		kvmVM:       kvmVM,
		archBackend: backend,
		events:      NewEventLog(),
		pausedVCPUs: make(map[int]struct{}),
		vcpuTIDs:    make(map[int]int),
		memDirty:    virtio.NewDirtyTracker(uint64(len(kvmVM.Memory()))),
	}
	m.events.Emit(EventCreated, fmt.Sprintf("VM %s created, %d MiB RAM", cfg.ID, cfg.MemMB))
	if err := m.setupMemoryHotplug(); err != nil {
		return nil, fmt.Errorf("setup memory hotplug: %w", err)
	}

	// Setup devices BEFORE loading kernel so we can append virtio_mmio.device= to cmdline
	if err := m.archBackend.setupDevices(m); err != nil {
		return nil, fmt.Errorf("setup devices: %w", err)
	}
	if err := m.archBackend.setupIRQs(m); err != nil {
		return nil, fmt.Errorf("setup irqs: %w", err)
	}
	m.events.Emit(EventDevicesReady, fmt.Sprintf("%d virtio devices initialized", len(m.transports)))

	kernelInfo, err := m.archBackend.loadKernel(m)
	if err != nil {
		return nil, fmt.Errorf("load kernel: %w", err)
	}
	m.events.Emit(EventKernelLoaded, fmt.Sprintf("kernel loaded from %s", cfg.KernelPath))

	// Create all vCPUs before setup. ARM64 GICv3 requires all vCPUs to
	// exist before the in-kernel interrupt controller can be initialized.
	for i := 0; i < cfg.VCPUs; i++ {
		vcpu, err := kvmVM.CreateVCPU(i)
		if err != nil {
			return nil, fmt.Errorf("create vcpu %d: %w", i, err)
		}
		m.vcpus = append(m.vcpus, vcpu)
	}
	if err := m.archBackend.postCreateVCPUs(m); err != nil {
		return nil, err
	}
	if cfg.TrackDirtyPages {
		if err := kvmVM.EnableDirtyLogging(); err != nil {
			gclog.VMM.Warn("dirty page tracking unavailable", "error", err)
		}
	}

	if m.archBackend.setupVCPUsInParallel() {
		vcpuErrs := make([]error, len(m.vcpus))
		var vcpuWG sync.WaitGroup
		for i, vcpu := range m.vcpus {
			vcpuWG.Add(1)
			go func(i int, vcpu *kvm.VCPU) {
				defer vcpuWG.Done()
				vcpuErrs[i] = m.archBackend.setupVCPU(m, vcpu, i, kernelInfo)
			}(i, vcpu)
		}
		vcpuWG.Wait()
		for _, err := range vcpuErrs {
			if err != nil {
				return nil, err
			}
		}
	} else {
		for i, vcpu := range m.vcpus {
			if err := m.archBackend.setupVCPU(m, vcpu, i, kernelInfo); err != nil {
				return nil, err
			}
		}
	}
	m.events.Emit(EventCPUConfigured, fmt.Sprintf("%d vCPU(s) configured", len(m.vcpus)))

	gclog.VMM.Info("vm created", "id", cfg.ID, "mem_mb", cfg.MemMB, "vcpus", cfg.VCPUs)
	return m, nil
}

// Start launches the vCPU goroutine.
func (m *VM) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != StateCreated && m.state != StatePaused {
		return fmt.Errorf("cannot start VM in state %s", m.state)
	}
	startFresh := m.state == StateCreated
	m.events.Emit(EventStarting, fmt.Sprintf("starting %d vCPU(s)", len(m.vcpus)))
	m.state = StateRunning
	m.startTime = time.Now()
	if startFresh && len(m.vcpus) > 0 {
		m.runWG.Add(len(m.vcpus))
		for _, vcpu := range m.vcpus {
			go m.runLoop(vcpu)
		}
		go m.awaitStop()
		if m.cfg.Balloon != nil && m.cfg.Balloon.Auto == BalloonAutoConservative {
			go m.balloonAutoLoop()
		}
	}
	m.events.Emit(EventRunning, fmt.Sprintf("%d vCPU(s) started", len(m.vcpus)))
	gclog.VMM.Info("vm started", "id", m.cfg.ID, "vcpus", len(m.vcpus))
	return nil
}

// Stop signals the VM to halt.
func (m *VM) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == StateRunning || m.state == StatePaused {
		m.state = StateStopped
		for _, vcpu := range m.vcpus {
			vcpu.RunData.ImmediateExit = 1
		}
		go m.kickVCPUs()
		select {
		case <-m.stopCh: // already closed
		default:
			close(m.stopCh)
		}
	}
}

func (m *VM) awaitStop() {
	m.runWG.Wait()
	m.finishStop()
}

func (m *VM) finishStop() {
	m.stopOnce.Do(func() {
		m.cleanup()
		close(m.doneCh)
		m.events.Emit(EventStopped, "VM stopped")
		gclog.VMM.Info("vm stopped", "id", m.cfg.ID)
	})
}

// WaitStopped blocks until the VM has fully stopped and finished cleanup.
func (m *VM) WaitStopped(ctx context.Context) error {
	select {
	case <-m.doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Pause stops vCPU execution at a consistent boundary so guest state can be captured.
// Like Firecracker, snapshotting and migration build on top of this paused state.
func (m *VM) Pause() error {
	m.mu.Lock()
	switch m.state {
	case StatePaused:
		m.mu.Unlock()
		return nil
	case StateRunning:
	default:
		state := m.state
		m.mu.Unlock()
		return fmt.Errorf("cannot pause VM in state %s", state)
	}
	m.state = StatePaused
	clear(m.pausedVCPUs)
	vcpuCount := len(m.vcpus)
	for _, vcpu := range m.vcpus {
		vcpu.RunData.ImmediateExit = 1
	}
	m.mu.Unlock()

	m.kickVCPUs()

	deadline := time.Now().Add(defaultPauseTimeout)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		paused := len(m.pausedVCPUs)
		stopped := m.state == StateStopped
		m.mu.Unlock()
		if stopped {
			return fmt.Errorf("vm stopped while pausing")
		}
		if paused == vcpuCount {
			// Close any active UDS bridges so clients observe the pause as
			// EOF and reconnect after Resume. The listener itself stays up,
			// accepting new connections that will block on DialVsock until
			// the guest resumes.
			if m.udsListener != nil {
				m.udsListener.closeAllBridges()
			}
			m.events.Emit(EventPaused, fmt.Sprintf("%d vCPU(s) paused", vcpuCount))
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}

	m.mu.Lock()
	if m.state == StatePaused {
		m.state = StateRunning
	}
	clear(m.pausedVCPUs)
	for _, vcpu := range m.vcpus {
		vcpu.RunData.ImmediateExit = 0
	}
	m.mu.Unlock()
	return fmt.Errorf("timeout waiting for %d vCPU(s) to pause", vcpuCount)
}

// Resume restarts a previously paused VM.
func (m *VM) Resume() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != StatePaused {
		return fmt.Errorf("cannot resume VM in state %s", m.state)
	}
	m.state = StateRunning
	clear(m.pausedVCPUs)
	for _, vcpu := range m.vcpus {
		vcpu.RunData.ImmediateExit = 0
	}
	m.events.Emit(EventResumed, fmt.Sprintf("%d vCPU(s) resumed", len(m.vcpus)))
	return nil
}

// State returns the current lifecycle state.
func (m *VM) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// ID returns the VM identifier.
func (m *VM) ID() string { return m.cfg.ID }

// Uptime returns how long the VM has been running.
func (m *VM) Uptime() time.Duration {
	if m.startTime.IsZero() {
		return 0
	}
	return time.Since(m.startTime)
}

// Events returns the VM's event log.
func (m *VM) Events() EventSource { return m.events }

// VMConfig returns the VM's configuration (read-only copy).
func (m *VM) VMConfig() Config { return m.cfg }

func (m *VM) UpdateNetRateLimiter(cfg *RateLimiterConfig) error {
	// Single-bucket compatibility path: applies the same config to both
	// RX and TX. New code should prefer UpdateNetRateLimiters.
	return m.UpdateNetRateLimiters(cfg, cfg)
}

// UpdateNetRateLimiters applies separate RX (host→guest) and TX (guest→host)
// token buckets at runtime. Either argument may be nil for no limit in that
// direction. Matches Firecracker's split `rx_rate_limiter` / `tx_rate_limiter`.
func (m *VM) UpdateNetRateLimiters(rx, tx *RateLimiterConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.netDev == nil {
		return fmt.Errorf("virtio-net is not configured")
	}
	m.netDev.SetRateLimiters(buildRateLimiter(rx), buildRateLimiter(tx))
	m.cfg.RxNetRateLimiter = cloneRateLimiterConfig(rx)
	m.cfg.TxNetRateLimiter = cloneRateLimiterConfig(tx)
	// Clear the legacy generic field when direction-specific ones are set
	// so snapshot/restore round-trip stays coherent.
	m.cfg.NetRateLimiter = nil
	return nil
}

func (m *VM) UpdateBlockRateLimiter(cfg *RateLimiterConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.blkDev == nil {
		return fmt.Errorf("virtio-blk is not configured")
	}
	m.blkDev.SetRateLimiter(buildRateLimiter(cfg))
	m.cfg.BlockRateLimiter = cloneRateLimiterConfig(cfg)
	return nil
}

func (m *VM) UpdateRNGRateLimiter(cfg *RateLimiterConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rngDev == nil {
		return fmt.Errorf("virtio-rng is not configured")
	}
	m.rngDev.SetRateLimiter(buildRateLimiter(cfg))
	m.cfg.RNGRateLimiter = cloneRateLimiterConfig(cfg)
	return nil
}

func (m *VM) GetBalloonConfig() (BalloonConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.balloonDev == nil || m.cfg.Balloon == nil {
		return BalloonConfig{}, fmt.Errorf("virtio-balloon is not configured")
	}
	cfg := *m.cfg.Balloon
	cfg.AmountMiB = m.balloonDev.GetConfig().AmountMiB
	cfg.StatsPollingIntervalS = int(m.balloonDev.StatsPollingInterval() / time.Second)
	cfg.SnapshotPages = m.balloonDev.SnapshotPages()
	return cfg, nil
}

func (m *VM) UpdateBalloon(update BalloonUpdate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.balloonDev == nil || m.cfg.Balloon == nil {
		return fmt.Errorf("virtio-balloon is not configured")
	}
	if err := m.balloonDev.SetTargetMiB(update.AmountMiB); err != nil {
		return err
	}
	m.cfg.Balloon.AmountMiB = update.AmountMiB
	m.cfg.Balloon.SnapshotPages = m.balloonDev.SnapshotPages()
	return nil
}

func (m *VM) GetBalloonStats() (BalloonStats, error) {
	m.mu.Lock()
	dev := m.balloonDev
	cfg := m.cfg
	m.mu.Unlock()
	if dev == nil {
		return BalloonStats{}, fmt.Errorf("virtio-balloon is not configured")
	}
	stats, err := dev.PollStats()
	if err != nil {
		stats = dev.Stats()
	}
	if cfg.Exec != nil && cfg.Exec.Enabled {
		memStats, memErr := m.readGuestMemoryStats(cfg)
		if memErr == nil {
			stats = mergeBalloonStats(stats, memStats)
			err = nil
		}
	}
	if err != nil && stats.UpdatedAt.IsZero() {
		return BalloonStats{}, err
	}
	return BalloonStats{
		TargetPages:     stats.TargetPages,
		ActualPages:     stats.ActualPages,
		TargetMiB:       stats.TargetMiB,
		ActualMiB:       stats.ActualMiB,
		SwapIn:          stats.SwapIn,
		SwapOut:         stats.SwapOut,
		MajorFaults:     stats.MajorFaults,
		MinorFaults:     stats.MinorFaults,
		FreeMemory:      stats.FreeMemory,
		TotalMemory:     stats.TotalMemory,
		AvailableMemory: stats.AvailableMemory,
		DiskCaches:      stats.DiskCaches,
		HugetlbAllocs:   stats.HugetlbAllocs,
		HugetlbFailures: stats.HugetlbFailures,
		OOMKill:         stats.OOMKill,
		AllocStall:      stats.AllocStall,
		AsyncScan:       stats.AsyncScan,
		DirectScan:      stats.DirectScan,
		AsyncReclaim:    stats.AsyncReclaim,
		DirectReclaim:   stats.DirectReclaim,
		UpdatedAt:       stats.UpdatedAt,
	}, nil
}

func (m *VM) UpdateBalloonStats(update BalloonStatsUpdate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.balloonDev == nil || m.cfg.Balloon == nil {
		return fmt.Errorf("virtio-balloon is not configured")
	}
	if err := m.balloonDev.SetStatsPollingInterval(time.Duration(update.StatsPollingIntervalS) * time.Second); err != nil {
		return err
	}
	m.cfg.Balloon.StatsPollingIntervalS = update.StatsPollingIntervalS
	return nil
}

// KickVsockIRQ fires the virtio-vsock queue interrupt one additional time.
// Must be called after RestoreFromSnapshot, before Start — the same pattern
// Firecracker uses in kick_virtio_devices() before VcpuEvent::Resume.
//
// Rationale: QuiesceForSnapshot fires a TRANSPORT_RESET interrupt pre-pause.
// If the LAPIC already had a pending interrupt at the same vector (ISR bit
// set, EOI not yet written), KVM silently drops the edge-triggered injection
// (known Linux KVM behaviour). The restored LAPIC state from mem.bin is
// whatever was captured — which may or may not have an in-flight IRQ.
// Kicking here hits a LAPIC that has just been reset from snapshot state but
// whose vCPUs have not yet executed a single instruction: the injection is
// guaranteed to succeed, and on the very first KVM_RUN the guest sees the
// interrupt and drains the event queue containing TRANSPORT_RESET.
func (m *VM) KickVsockIRQ() {
	m.mu.Lock()
	dev := m.vsockDev
	m.mu.Unlock()
	if dev == nil {
		gclog.VMM.Info("KickVsockIRQ: no vsock device")
		return
	}
	gclog.VMM.Info("KickVsockIRQ: firing", "id", m.cfg.ID)
	dev.Transport.SetInterruptStat(1)
	dev.Transport.SignalIRQ(true)
	txQ := dev.Transport.Queue(1)
	txUsedFlags := uint16(0xFFFF)
	txLastAvail := uint16(0)
	if txQ != nil {
		txUsedFlags, _ = txQ.UsedFlags()
		txLastAvail = txQ.LastAvail
	}
	gclog.VMM.Info("KickVsockIRQ: fired", "id", m.cfg.ID, "interrupt_stat", dev.Transport.InterruptStat(), "txUsedFlags", txUsedFlags, "txLastAvail", txLastAvail)
	// Start periodic TX avail poller so we can detect if the guest
	// silently adds entries to the TX vring without a QueueNotify kick.
	dev.StartTXAvailPoller(45 * time.Second)
}

func (m *VM) DialVsock(port uint32) (net.Conn, error) {
	// Hold the read lock for the entire Dial call.  cleanup() acquires the write
	// lock before unmapping guest RAM, so this prevents a concurrent cleanup from
	// freeing memory while sendPkt is reading the virtio TX queue.
	m.vsockDialMu.RLock()
	defer m.vsockDialMu.RUnlock()

	m.mu.Lock()
	if m.vsockDev == nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("virtio-vsock is not configured")
	}
	if m.state != StateRunning && m.state != StatePaused {
		state := m.state
		m.mu.Unlock()
		return nil, fmt.Errorf("cannot dial vsock while VM is in state %s", state)
	}
	dev := m.vsockDev
	m.mu.Unlock()
	return dev.Dial(port)
}

func (m *VM) PrepareMigrationBundle(dir string) error {
	return PrepareMigrationBundle(m, dir)
}

func (m *VM) FinalizeMigrationBundle(dir string) (*Snapshot, *MigrationPatchSet, error) {
	return FinalizeMigrationBundle(m, dir)
}

func (m *VM) ResetMigrationTracking() error {
	return ResetMigrationTracking(m)
}

// DeviceList returns info about attached devices.
func (m *VM) DeviceList() []DeviceInfo {
	if m.archBackend == nil {
		return nil
	}
	return m.archBackend.deviceList(m)
}

// ConsoleOutput returns the buffered UART output.
func (m *VM) ConsoleOutput() []byte {
	if m.archBackend == nil {
		return nil
	}
	return m.archBackend.consoleOutput(m)
}

// FirstOutputAt returns the wall-clock instant at which the guest first
// transmitted a byte on the UART console. Zero time until the guest has
// written anything. Used by boot-time instrumentation to report the
// guest_first_output_ms phase.
func (m *VM) FirstOutputAt() time.Time {
	if m.uart0 == nil {
		return time.Time{}
	}
	return m.uart0.FirstOutputAt()
}

// ---- Snapshot / Restore ----

type Snapshot struct {
	Version    int                     `json:"version"`
	Timestamp  time.Time               `json:"timestamp"`
	ID         string                  `json:"id"`
	Config     Config                  `json:"config"`
	VCPUs      []VCPUState             `json:"vcpus,omitempty"`
	Regs       kvm.Regs                `json:"regs,omitempty"`
	Sregs      kvm.Sregs               `json:"sregs,omitempty"`
	MPState    kvm.MPState             `json:"mp_state,omitempty"`
	MemFile    string                  `json:"mem_file"`
	Arch       *SnapshotArchState      `json:"arch,omitempty"`
	UART       *uart.UARTState         `json:"uart,omitempty"`
	Transports []virtio.TransportState `json:"transports,omitempty"`
}

type SnapshotOptions struct {
	Resume bool
	// SkipDiskBundle skips copying the root disk into the snapshot dir.
	// Safe only when the disk is guaranteed unchanged (e.g. warmcache capture
	// of an idle InteractiveExec VM). The snapshot references the original
	// absolute disk path; restore resolves it from the artifact cache.
	SkipDiskBundle bool
}

// TakeSnapshot pauses the VM and saves state to dir.
// Returns the snapshot metadata.
func (m *VM) TakeSnapshot(dir string) (*Snapshot, error) {
	return m.TakeSnapshotWithOptions(dir, SnapshotOptions{
		Resume:         true,
		SkipDiskBundle: m.kvmVM.DirtyLoggingEnabled(),
	})
}

// TakeSnapshotWithOptions saves a snapshot while optionally leaving the VM paused.
func (m *VM) TakeSnapshotWithOptions(dir string, opts SnapshotOptions) (*Snapshot, error) {
	resumeAfter := false

	m.mu.Lock()
	state := m.state
	m.mu.Unlock()
	switch state {
	case StateRunning:
		// QuiesceForSnapshot sends RST+TRANSPORT_RESET to all active vsock
		// connections so the guest can drain its queues cleanly before pause.
		// Not strictly required for correctness — after restore the host
		// simply re-dials the guest listener — but avoids stale buffers.
		if m.vsockDev != nil {
			m.vsockDev.QuiesceForSnapshot()
		}
		// 10ms grace period for in-flight vsock frames to land.
		// Reduced from 50ms — QuiesceForSnapshot already waits for
		// active connections to close; 10ms is enough for the IRQ
		// to propagate to the guest vCPU before Pause kicks it.
		time.Sleep(10 * time.Millisecond)
		if err := m.Pause(); err != nil {
			return nil, err
		}
		resumeAfter = opts.Resume
	case StatePaused:
		resumeAfter = false
	default:
		return nil, fmt.Errorf("VM must be running or paused to snapshot (state: %s)", state)
	}
	if resumeAfter {
		defer func() {
			if err := m.Resume(); err != nil {
				gclog.VMM.Warn("resume after snapshot failed", "id", m.cfg.ID, "error", err)
			}
		}()
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	if err := m.prepareSnapshot(); err != nil {
		return nil, err
	}

	memFile := filepath.Join(dir, "mem.bin")
	if m.kvmVM.DirtyLoggingEnabled() {
		tDirty := time.Now()
		bitmap, err := m.kvmVM.GetDirtyLog(0)
		if err != nil {
			gclog.VMM.Warn("dirty log unavailable, falling back to full write", "error", err)
			goto fullWrite
		}
		// The host-side kernel/initrd loader writes directly to the memory
		// mapping — those writes don't trigger KVM dirty tracking, so we must
		// also include any non-zero "clean" pages. ~15ms for 100 MB.
		augmentDirtyBitmap(m.kvmVM.Memory(), bitmap)
		dirtyCount := countDirtyBits(bitmap)
		gclog.VMM.Info("saving dirty pages", "dirty_mb", dirtyCount*4/1024, "total_mb", m.cfg.MemMB, "path", memFile, "getDirtyLog_ms", time.Since(tDirty).Milliseconds())
		tWrite := time.Now()
		if err := saveDirtyPages(memFile, m.kvmVM.Memory(), bitmap); err != nil {
			return nil, fmt.Errorf("write dirty mem: %w", err)
		}
		gclog.VMM.Info("dirty pages written", "ms", time.Since(tWrite).Milliseconds())
		tCapture := time.Now()
		snap, err2 := captureSnapshotState(m)
		if err2 != nil {
			return nil, err2
		}
		gclog.VMM.Info("vcpu state captured", "ms", time.Since(tCapture).Milliseconds())
		snap.MemFile = "mem.bin"
		tBundle := time.Now()
		bundled, err2 := rewriteSnapshotBundleOpts(dir, *snap, snap.Config, opts.SkipDiskBundle)
		if err2 != nil {
			return nil, fmt.Errorf("bundle snapshot assets: %w", err2)
		}
		gclog.VMM.Info("snapshot saved", "path", filepath.Join(dir, "snapshot.json"), "bundle_ms", time.Since(tBundle).Milliseconds())
		m.events.Emit(EventSnapshot, fmt.Sprintf("snapshot saved to %s", dir))
		return bundled, nil
	}
fullWrite:
	gclog.VMM.Info("saving RAM", "mem_mb", m.cfg.MemMB, "path", memFile)
	if err := saveMemAsync(memFile, m.kvmVM.Memory()); err != nil {
		return nil, fmt.Errorf("write mem: %w", err)
	}
	snap, err := captureSnapshotState(m)
	if err != nil {
		return nil, err
	}
	snap.MemFile = "mem.bin"

	// Bundle kernel/initrd/disk into the snapshot dir so restore stays valid
	// after the original VM's runtime dir (runs/<vm-id>/disk.ext4) is cleaned
	// up. Without this, restore falls back to cold boot with
	//   "open .../runs/<vm-id>/disk.ext4: no such file or directory"
	//
	// Honour opts.SkipDiskBundle the same way the dirty-pages path above does.
	// When the caller is the worker proxy (takeSnapshotViaExport), it will
	// hardlink the root disk on the host side AFTER the RPC returns — inside
	// the jail link(2) returns EXDEV across the read-only /worker bind-mount
	// and falls back to a ~2 GB full copy that dominates warm-capture latency
	// (~17 s on ARM64 EC2).
	bundled, err := rewriteSnapshotBundleOpts(dir, *snap, snap.Config, opts.SkipDiskBundle)
	if err != nil {
		return nil, fmt.Errorf("bundle snapshot assets: %w", err)
	}
	snap = bundled
	gclog.VMM.Info("snapshot saved", "path", filepath.Join(dir, "snapshot.json"))
	m.events.Emit(EventSnapshot, fmt.Sprintf("snapshot saved to %s", dir))

	return snap, nil
}

// RestoreFromSnapshot creates a new VM restored from a snapshot directory.
func RestoreFromSnapshot(dir string) (*VM, error) {
	return RestoreFromSnapshotWithOptions(dir, RestoreOptions{})
}

// RestoreFromSnapshotWithConsole creates a new VM restored from a snapshot
// directory and overrides the console streams for this host process.
func RestoreFromSnapshotWithConsole(dir string, consoleIn io.Reader, consoleOut io.Writer) (*VM, error) {
	return RestoreFromSnapshotWithOptions(dir, RestoreOptions{
		ConsoleIn:  consoleIn,
		ConsoleOut: consoleOut,
	})
}

type RestoreOptions struct {
	ConsoleIn       io.Reader
	ConsoleOut      io.Writer
	OverrideVCPUs   int
	OverrideID      string
	OverrideTap     string
	OverrideX86Boot X86BootMode
	// OverrideVsockUDSPath replaces the snapshot's Vsock.UDSPath. Empty
	// means "use whatever the snapshot serialized"; callers that change
	// OverrideID or restore on a different host typically pass this so the
	// resulting socket lands at a valid, unambiguous path. Set to "-" to
	// explicitly disable the UDS listener at restore time.
	OverrideVsockUDSPath string
	// SharedFSRebinds remaps virtiofs exports present in the snapshot to new
	// host source paths, keyed by the guest-side Target that the template
	// mounted the tag at. The template must have already snapshotted with a
	// SharedFS entry whose Target matches; empty list means "use the
	// snapshot's own sources unchanged".
	SharedFSRebinds []SharedFSRebind
}

// SharedFSRebind rewrites the Source behind a virtio-fs export that was
// already present in the snapshot, without changing the MMIO device layout
// or the tag. Used by the sandbox-template flow to inject a per-instance
// toolbox on top of a pre-provisioned virtiofs slot.
type SharedFSRebind struct {
	Target string `json:"target"`
	Source string `json:"source"`
}

// RestoreFromSnapshotWithOptions creates a new VM restored from a snapshot
// directory and overrides selected runtime settings for this host process.
func RestoreFromSnapshotWithOptions(dir string, opts RestoreOptions) (*VM, error) {
	snap, err := readSnapshot(dir)
	if err != nil {
		return nil, err
	}
	return restoreFromSnapshot(dir, snap, opts)
}

// applySharedFSRebinds overrides the Source (and clears any stale SocketPath)
// of virtio-fs entries in the snapshot whose guest-side Target matches a
// caller-supplied rebind. Templates that were not snapshotted with a matching
// Target fail fast with a clear error. Empty rebinds leave the snapshot
// untouched so existing callers keep today's behaviour.
func applySharedFSRebinds(snap *Snapshot, rebinds []SharedFSRebind) error {
	if snap == nil || len(rebinds) == 0 {
		return nil
	}
	idx := make(map[string]int, len(snap.Config.SharedFS))
	for i, fs := range snap.Config.SharedFS {
		if fs.Target == "" {
			continue
		}
		idx[fs.Target] = i
	}
	for _, rb := range rebinds {
		i, ok := idx[rb.Target]
		if !ok {
			return fmt.Errorf("snapshot has no virtiofs slot for target %q (available targets: %v); template must be rebuilt with a matching virtiofs mount", rb.Target, sharedFSTargets(snap.Config.SharedFS))
		}
		snap.Config.SharedFS[i].Source = rb.Source
		// SocketPath points at the template host's virtiofsd socket; it is
		// stale on the restore host. Clearing it forces the vmm to spawn a
		// fresh virtiofsd against the new Source (direct path) or requires
		// the worker to re-populate it (worker path).
		snap.Config.SharedFS[i].SocketPath = ""
	}
	return nil
}

// applyVsockUDSPathOverride mutates the snapshot's Vsock.UDSPath based on
// RestoreOptions.OverrideVsockUDSPath. Empty = no change; "-" = clear
// (disable UDS at restore); anything else = new absolute path, which is
// validated before being accepted.
func applyVsockUDSPathOverride(snap *Snapshot, override string) error {
	if override == "" {
		return nil
	}
	if snap == nil || snap.Config.Vsock == nil {
		return fmt.Errorf("OverrideVsockUDSPath: snapshot has no vsock device")
	}
	if override == "-" {
		snap.Config.Vsock.UDSPath = ""
		return nil
	}
	snap.Config.Vsock.UDSPath = override
	if err := snap.Config.Vsock.Validate(); err != nil {
		return fmt.Errorf("OverrideVsockUDSPath: %w", err)
	}
	return nil
}

func sharedFSTargets(cfgs []SharedFSConfig) []string {
	out := make([]string, 0, len(cfgs))
	for _, fs := range cfgs {
		if fs.Target == "" {
			continue
		}
		out = append(out, fs.Target)
	}
	return out
}

// ReadSnapshot parses a snapshot directory's metadata file. Exported so
// callers outside the package (e.g. worker proxy) can read a snapshot that
// was already bundled inside a jailer and copied to a host-side directory.
func ReadSnapshot(dir string) (*Snapshot, error) {
	snap, err := readSnapshot(dir)
	if err != nil {
		return nil, err
	}
	return &snap, nil
}

func readSnapshot(dir string) (Snapshot, error) {
	metaFile := filepath.Join(dir, "snapshot.json")
	data, err := os.ReadFile(metaFile)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read snapshot: %w", err)
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

func restoreFromSnapshot(dir string, snap Snapshot, opts RestoreOptions) (*VM, error) {
	arch, err := normalizeSnapshotMachineArch(snap.Config.Arch)
	if err != nil {
		return nil, err
	}
	if err := validateMachineArch(arch); err != nil {
		return nil, fmt.Errorf("snapshot arch validation failed: %w", err)
	}
	backend, err := newMachineArchBackend(arch)
	if err != nil {
		return nil, err
	}
	snap.Config.Arch = string(arch)

	snap.Config.ConsoleIn = opts.ConsoleIn
	snap.Config.ConsoleOut = opts.ConsoleOut
	if opts.OverrideID != "" {
		snap.Config.ID = opts.OverrideID
	}
	if opts.OverrideTap != "" {
		snap.Config.TapName = opts.OverrideTap
	}
	if err := applyVsockUDSPathOverride(&snap, opts.OverrideVsockUDSPath); err != nil {
		return nil, err
	}
	if err := applySharedFSRebinds(&snap, opts.SharedFSRebinds); err != nil {
		return nil, err
	}
	if opts.OverrideX86Boot != "" {
		mode, err := normalizeX86BootMode(opts.OverrideX86Boot)
		if err != nil {
			return nil, err
		}
		snapMode, err := normalizeX86BootMode(snap.Config.X86Boot)
		if err != nil {
			return nil, err
		}
		if snapMode != mode {
			return nil, fmt.Errorf("snapshot was taken with x86 boot mode %q, restore override %q is not allowed", snapMode, mode)
		}
		snap.Config.X86Boot = mode
	}
	snapVCPUCount := snapshotVCPUCount(snap)
	if opts.OverrideVCPUs > 0 && opts.OverrideVCPUs != snapVCPUCount {
		return nil, fmt.Errorf("snapshot was taken with %d vCPU(s), restore override %d is not allowed", snapVCPUCount, opts.OverrideVCPUs)
	}
	if opts.OverrideVCPUs > 0 {
		snap.Config.VCPUs = opts.OverrideVCPUs
	} else if snap.Config.VCPUs <= 0 {
		snap.Config.VCPUs = snapVCPUCount
	}
	snap.MemFile = resolveSnapshotPath(dir, snap.MemFile)
	snap.Config.KernelPath = resolveSnapshotPath(dir, snap.Config.KernelPath)
	snap.Config.InitrdPath = resolveSnapshotPath(dir, snap.Config.InitrdPath)
	snap.Config.DiskImage = resolveSnapshotPath(dir, snap.Config.DiskImage)

	gclog.VMM.Info("restoring VM", "id", snap.ID, "dir", dir)

	sys, err := kvm.Open()
	if err != nil {
		return nil, err
	}
	// Snapshots that include virtio-fs exports cannot use the MAP_PRIVATE COW
	// fast path: virtiofsd needs a memfd it can mmap, and a file-backed
	// PRIVATE mapping has no fd to share. Detect that case and fall back to
	// the slower memfd-materialize path (one O(mem) read+copy at restore
	// time, ~60-100 ms on a 128 MiB guest). Snapshots without virtio-fs keep
	// today's instant lazy-fault restore.
	var kvmVM *kvm.VM
	if len(snap.Config.SharedFS) > 0 {
		kvmVM, err = sys.CreateVMFromSnapshotFileMemfd(snap.MemFile, snap.Config.MemMB, guestRAMBase(snap.Config.Arch))
		if err != nil {
			return nil, fmt.Errorf("memfd restore: %w", err)
		}
	} else {
		kvmVM, err = sys.CreateVMFromSnapshotFile(snap.MemFile, snap.Config.MemMB, guestRAMBase(snap.Config.Arch))
		if err != nil {
			return nil, fmt.Errorf("cow restore: %w", err)
		}
	}

	m := &VM{
		cfg:         snap.Config,
		state:       StateCreated,
		stopCh:      make(chan struct{}),
		doneCh:      make(chan struct{}),
		kvmSys:      sys,
		kvmVM:       kvmVM,
		archBackend: backend,
		events:      NewEventLog(),
		pausedVCPUs: make(map[int]struct{}),
		vcpuTIDs:    make(map[int]int),
		memDirty:    virtio.NewDirtyTracker(uint64(len(kvmVM.Memory()))),
	}

	// Seed the per-device restore shortcuts before setupDevices runs so
	// NewBlockDeviceWithOptions can skip the FALLOC_FL_PUNCH_HOLE probe
	// (~3 ms per drive on cold tmpfs). We pre-advertise discard=true for
	// every writable drive on the restore path; the guest already negotiated
	// against the original DeviceFeatures before the snapshot was taken, so
	// advertising here is only informational — the actual runtime behaviour
	// is decided by Transport.drvFeatures, which is restored from the snap
	// a few dozen lines below.
	m.restoring = true
	m.restoredDiscard = make(map[string]bool, len(snap.Config.DriveList()))
	for _, drive := range snap.Config.DriveList() {
		if !drive.ReadOnly {
			m.restoredDiscard[drive.ID] = true
		}
	}

	// Re-attach devices (they reconnect to the existing memory)
	if err := m.archBackend.setupDevices(m); err != nil {
		return nil, fmt.Errorf("restore devices: %w", err)
	}
	m.restoring = false
	if err := m.archBackend.setupIRQs(m); err != nil {
		return nil, fmt.Errorf("restore irqs: %w", err)
	}
	if err := m.archBackend.restoreVMState(kvmVM, snap.Arch); err != nil {
		return nil, fmt.Errorf("restore vm arch state: %w", err)
	}

	vcpuStates := normalizeSnapshotVCPUStates(snap)
	// Create all vCPUs before restore (ARM64 GICv3 needs all vCPUs first).
	for i := 0; i < snap.Config.VCPUs; i++ {
		vcpu, err := kvmVM.CreateVCPU(i)
		if err != nil {
			return nil, fmt.Errorf("create vcpu %d: %w", i, err)
		}
		m.vcpus = append(m.vcpus, vcpu)
	}
	// Mirror the cold-boot sequence: archBackend.postCreateVCPUs runs after
	// the vCPU fds exist and before per-vCPU state is restored. On x86 it
	// registers the per-device eventfds with KVM_IRQFD against the freshly
	// created GSIs (otherwise device interrupts silently drop until the
	// first VM.Start, and we pay a reconfiguration VMexit on every queue
	// notify). On arm64 it is where the GIC is created — without this call,
	// a restored arm64 VM has no interrupt controller at all.
	if err := m.archBackend.postCreateVCPUs(m); err != nil {
		return nil, fmt.Errorf("post-create vcpus (restore): %w", err)
	}
	// Restore vCPU register state BEFORE VGIC state. On ARM64 with multi-
	// vCPU (>=3), the VGIC per-vcpu CPU_SYSREGS (ICC_*) writes land on the
	// ICC register shadows inside KVM which check validity against the
	// vCPU's already-initialised ICC_SRE_EL1 / ICC_CTLR_EL1. Writing VGIC
	// state first means those checks run against architectural reset
	// defaults and partially succeed — leaving secondaries with an ICC
	// interface whose SRE bit disagrees with the guest's view. Firecracker
	// orders restore as: vcpu_fd.restore_state → vm.restore_state(gic).
	for i, vcpu := range m.vcpus {
		// restoreVCPU already swallows the expected kvmclock-ctrl EINVAL
		// inline (arch_x86.go:96) and returns nil in that case. Doing a
		// second isIgnorableKVMClockCtrlError check here is WRONG: it was
		// masking a real EINVAL from SetSregs (e.g. stale CR0/CR4 bits)
		// as "kvmclock unsupported", so the vCPU resumed with garbage
		// segment state, triple-faulted on its first KVM_RUN, emitted
		// ExitShutdown, and cleanup closed the exec broker — surfacing to
		// callers as "exec agent broker is closed" / "connection timed
		// out". Let every non-nil error propagate so real failures are
		// loud instead of silent.
		if err := m.archBackend.restoreVCPU(sys, kvmVM, vcpu, vcpuStates[i]); err != nil {
			return nil, fmt.Errorf("restore vcpu %d: %w", i, err)
		}
	}
	// VGIC state restored AFTER vCPU state — see note above.
	// On x86 this is a no-op; IRQCHIP state was already applied in the
	// earlier restoreVMState call.
	if err := m.archBackend.restoreVMStatePostIRQ(m, snap.Arch); err != nil {
		return nil, fmt.Errorf("restore vm arch state (post-irq): %w", err)
	}

	// Restore device state
	if snap.UART != nil && m.uart0 != nil {
		m.uart0.RestoreState(*snap.UART)
	}
	for i, ts := range snap.Transports {
		if i < len(m.transports) {
			m.transports[i].RestoreState(ts)
		}
	}
	for _, fsDev := range m.fsDevs {
		if err := fsDev.RestoreBackendState(); err != nil {
			return nil, fmt.Errorf("restore virtio-fs backend: %w", err)
		}
	}

	age := time.Since(snap.Timestamp).Round(time.Second)
	gclog.VMM.Info("VM restored", "id", snap.ID, "snapshot_age", age)
	m.events.Emit(EventRestored, fmt.Sprintf("restored from %s (age: %s)", dir, age))
	return m, nil
}

func resolveSnapshotPath(dir, value string) string {
	if value == "" || filepath.IsAbs(value) {
		return value
	}
	return filepath.Join(dir, value)
}

func snapshotVCPUCount(snap Snapshot) int {
	switch {
	case len(snap.VCPUs) > 0:
		return len(snap.VCPUs)
	case snap.Config.VCPUs > 0:
		return snap.Config.VCPUs
	default:
		return 1
	}
}

func normalizeSnapshotVCPUStates(snap Snapshot) []VCPUState {
	if len(snap.VCPUs) > 0 {
		return snap.VCPUs
	}
	return []VCPUState{{
		ID:      0,
		X86:     &X86VCPUState{Regs: snap.Regs, Sregs: snap.Sregs, MPState: snap.MPState},
		Regs:    snap.Regs,
		Sregs:   snap.Sregs,
		MPState: snap.MPState,
	}}
}

func captureVCPUState(vcpu *kvm.VCPU) (VCPUState, error) {
	// Order matches Firecracker's VcpuGetState. MP_STATE first so any
	// interrupt-delivery bookkeeping the kernel updates on later ioctls
	// is already captured. Full set is required for a live restore:
	// without MSRs the guest's syscall entry points (LSTAR/STAR) vanish
	// and the first syscall triple-faults; without VCPU_EVENTS a guest
	// captured mid-HLT never wakes from the pending timer; without
	// XSAVE/XCRs AVX state is lost; without DEBUGREGS any active
	// hw-breakpoint is zeroed.
	x := X86VCPUState{}
	var err error
	if x.MPState, err = vcpu.GetMPState(); err != nil {
		return VCPUState{}, fmt.Errorf("get mp_state vcpu %d: %w", vcpu.ID, err)
	}
	if x.Regs, err = vcpu.GetRegs(); err != nil {
		return VCPUState{}, fmt.Errorf("get regs vcpu %d: %w", vcpu.ID, err)
	}
	if x.Sregs, err = vcpu.GetSregs(); err != nil {
		return VCPUState{}, fmt.Errorf("get sregs vcpu %d: %w", vcpu.ID, err)
	}
	lapic, err := vcpu.GetLAPIC()
	if err != nil {
		return VCPUState{}, fmt.Errorf("get lapic vcpu %d: %w", vcpu.ID, err)
	}
	x.LAPIC = &lapic
	msrs, err := vcpu.GetMSRs(kvm.SnapshotMSRIndices())
	if err != nil {
		return VCPUState{}, fmt.Errorf("get msrs vcpu %d: %w", vcpu.ID, err)
	}
	x.MSRs = msrs
	// TSC_DEADLINE: capture separately so restore writes it after TSC is
	// back in place. Apply Firecracker's zero-rewrite trick (see
	// `fix_zero_tsc_deadline_msr` in their vcpu.rs): if the LAPIC wasn't
	// armed at snapshot time the MSR reads 0, and writing 0 back means
	// the guest's HLT is never woken by a timer. Substituting the current
	// TSC makes the next post-restore "is deadline < TSC?" check true and
	// fires the timer IRQ immediately.
	tscD, err := vcpu.GetMSRs([]uint32{kvm.MSRIA32TSCDeadline})
	if err == nil && len(tscD) == 1 {
		x.TSCDeadline = tscD[0].Data
		if x.TSCDeadline == 0 {
			for _, m := range msrs {
				if m.Index == kvm.MSRIA32TSC {
					x.TSCDeadline = m.Data
					break
				}
			}
		}
	}
	fpu, err := vcpu.GetFPU()
	if err != nil {
		return VCPUState{}, fmt.Errorf("get fpu vcpu %d: %w", vcpu.ID, err)
	}
	x.FPU = &fpu
	xsave, err := vcpu.GetXSAVE()
	if err != nil {
		return VCPUState{}, fmt.Errorf("get xsave vcpu %d: %w", vcpu.ID, err)
	}
	x.XSAVE = &xsave
	xcrs, err := vcpu.GetXCRS()
	if err == nil {
		x.XCRs = &xcrs
	}
	events, err := vcpu.GetVCPUEvents()
	if err != nil {
		return VCPUState{}, fmt.Errorf("get vcpu_events vcpu %d: %w", vcpu.ID, err)
	}
	x.VCPUEvents = &events
	dbg, err := vcpu.GetDebugRegs()
	if err != nil {
		return VCPUState{}, fmt.Errorf("get debugregs vcpu %d: %w", vcpu.ID, err)
	}
	x.DebugRegs = &dbg
	if khz, err := vcpu.GetTSCKHz(); err == nil {
		x.TSCKHz = khz
	}
	return newX86VCPUState(vcpu.ID, x), nil
}

func captureVMArchState(vm *VM) (*SnapshotArchState, error) {
	clock, err := vm.kvmVM.GetClock()
	if err != nil {
		return nil, err
	}
	clock.Flags &^= kvm.ClockTSCStable

	pit2, err := vm.kvmVM.GetPIT2()
	if err != nil {
		return nil, err
	}

	irqChips := make([]kvm.IRQChip, 0, 3)
	for _, chipID := range []uint32{kvm.IRQChipPicMaster, kvm.IRQChipPicSlave, kvm.IRQChipIOAPIC} {
		chip, err := vm.kvmVM.GetIRQChip(chipID)
		if err != nil {
			return nil, err
		}
		irqChips = append(irqChips, chip)
	}

	return newX86SnapshotArchState(&X86MachineState{
		Clock:    clock,
		PIT2:     pit2,
		IRQChips: irqChips,
	}), nil
}

func restoreVMArchState(vm *kvm.VM, arch *SnapshotArchState) error {
	if arch == nil {
		return nil
	}
	x86State := arch.normalizedX86()
	if x86State == nil {
		return nil
	}
	if err := vm.SetPIT2(x86State.PIT2); err != nil {
		return err
	}
	if err := vm.SetClock(x86State.Clock); err != nil {
		return err
	}
	for _, chip := range x86State.IRQChips {
		if err := vm.SetIRQChip(chip); err != nil {
			return err
		}
	}
	return nil
}

func isIgnorableKVMClockCtrlError(err error) bool {
	return errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.ENOSYS)
}

func (m *VM) acpiMMIODevices() []acpi.MMIODevice {
	if len(m.transports) == 0 {
		return nil
	}
	devices := make([]acpi.MMIODevice, 0, len(m.transports))
	for _, t := range m.transports {
		devices = append(devices, acpi.MMIODevice{
			Addr: t.BasePA(),
			Len:  VirtioStride,
			GSI:  uint32(t.IRQLine()),
		})
	}
	return devices
}

// ---- Private: kernel + device setup ----

func (m *VM) loadKernel() (*loader.KernelInfo, error) {
	mem := m.kvmVM.Memory()

	info, err := loader.LoadKernel(mem, m.cfg.KernelPath, BootParamsAddr)
	if err != nil {
		return nil, err
	}

	// Write kernel cmdline at CmdlineAddr
	cmdline := m.cfg.Cmdline
	if cmdline == "" {
		cmdline = runtimecfg.DefaultKernelCmdline(false)
	}
	mode := m.cfg.X86Boot
	if mode == "" {
		mode = X86BootAuto
	}

	var acpiRSDP uint64
	if mode == X86BootAuto || mode == X86BootACPI {
		// Match Firecracker's transitional x86 path: advertise virtio-mmio
		// devices through ACPI. Auto keeps the legacy cmdline enumeration
		// as a compatibility bridge; pure acpi mode relies on ACPI only.
		acpiRSDP, err = acpi.CreateX86Tables(mem, m.cfg.VCPUs, m.acpiMMIODevices())
		if err != nil {
			return nil, fmt.Errorf("create acpi tables: %w", err)
		}
	}
	if mode == X86BootLegacy || mode == X86BootAuto {
		for i, t := range m.transports {
			cmdline += fmt.Sprintf(" virtio_mmio.device=0x%x@0x%x:%d",
				VirtioStride, t.BasePA(), VirtioIRQBase+i)
		}
	}
	if len(cmdline)+1 > runtimecfg.KernelCmdlineMax {
		return nil, fmt.Errorf("kernel cmdline too long: %d bytes exceeds limit %d", len(cmdline)+1, runtimecfg.KernelCmdlineMax)
	}
	copy(mem[CmdlineAddr:], cmdline+"\x00")

	var initrdAddr, initrdSize uint64
	if m.cfg.InitrdPath != "" {
		f, err := os.Open(m.cfg.InitrdPath)
		if err != nil {
			return nil, fmt.Errorf("open initrd: %w", err)
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("stat initrd: %w", err)
		}
		size := uint64(fi.Size())
		// Place initrd at max(InitrdAddr, kernelEnd rounded up to 2MiB)
		iAddr := uint64(InitrdAddr)
		if info.KernelEnd > iAddr {
			iAddr = (info.KernelEnd + 0x1FFFFF) &^ 0x1FFFFF // align 2MiB
		}
		if iAddr+size > uint64(len(mem)) {
			f.Close()
			return nil, fmt.Errorf("initrd at %#x (%d bytes) exceeds guest RAM", iAddr, size)
		}
		if _, err := io.ReadFull(f, mem[iAddr:iAddr+size]); err != nil {
			f.Close()
			return nil, fmt.Errorf("read initrd: %w", err)
		}
		f.Close()
		initrdAddr = iAddr
		initrdSize = size
	}

	loader.WriteBootParams(mem, info, loader.BootConfig{
		MemBytes:   m.cfg.MemMB * 1024 * 1024,
		Cmdline:    cmdline,
		InitrdAddr: initrdAddr,
		InitrdSize: initrdSize,
		ACPIRSDP:   acpiRSDP,
	})

	if mode == X86BootLegacy || mode == X86BootAuto {
		if err := mptable.Write(mem, m.cfg.VCPUs); err != nil {
			return nil, fmt.Errorf("write mp table: %w", err)
		}
	}

	// Ensure boot header fields are set for all kernel types.
	// ELF kernels don't have a setup header, so we must populate these fields
	// exactly as Firecracker does (boot_flag, header magic, type_of_loader, etc.)
	binary.LittleEndian.PutUint16(mem[BootParamsAddr+0x1FE:], 0xAA55)     // boot_flag
	binary.LittleEndian.PutUint32(mem[BootParamsAddr+0x202:], 0x53726448) // header "HdrS"
	mem[BootParamsAddr+0x210] = 0xFF                                      // type_of_loader
	mem[BootParamsAddr+0x211] |= 0x01                                     // loadflags: LOADED_HIGH

	// Store cmdline pointer in boot params
	binary.LittleEndian.PutUint32(mem[BootParamsAddr+0x228:], CmdlineAddr)
	binary.LittleEndian.PutUint32(mem[BootParamsAddr+0x238:], uint32(len(cmdline)))

	return info, nil
}

func (m *VM) makeIRQFn(irq uint32) func(bool) {
	return func(assert bool) {
		level := 0
		if assert {
			level = 1
		}
		m.kvmVM.IRQLine(irq, level)
	}
}

func (m *VM) makePulseIRQFn(irq uint32) func(bool) {
	return func(assert bool) {
		if !assert {
			return
		}
		m.kvmVM.IRQLine(irq, 1)
		m.kvmVM.IRQLine(irq, 0)
	}
}

// makeEventFDIRQFn creates an eventfd and returns an IRQ callback that writes
// a single uint64(1) into it on each assert. Paired with a KVM_IRQFD
// registration (see archBackend.postCreateVCPUs), this lets virtio devices
// inject interrupts with zero ioctl(KVM_IRQ_LINE) traffic and zero vCPU
// context switches during the injection itself — Firecracker's model, which
// arm64 already followed. The caller is expected to append the returned fd
// to vm.irqEventFds in the same order as the GSIs it later registers, so
// cleanup (vmm.cleanup) can close them on shutdown.
func (m *VM) makeEventFDIRQFn() (int, func(bool), error) {
	efd, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		return -1, nil, fmt.Errorf("eventfd: %w", err)
	}
	m.irqEventFds = append(m.irqEventFds, efd)
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], 1)
	fn := func(assert bool) {
		if !assert {
			return
		}
		_, _ = unix.Write(efd, buf[:])
	}
	return efd, fn, nil
}

func (m *VM) setupIRQs() error {
	routes := []uint32{COM1IRQ}
	for _, t := range m.transports {
		routes = append(routes, uint32(t.IRQLine()))
	}
	return m.kvmVM.SetGSIRouting(uniqueIRQs(routes))
}

func uniqueIRQs(irqs []uint32) []uint32 {
	seen := make(map[uint32]struct{}, len(irqs))
	out := make([]uint32, 0, len(irqs))
	for _, irq := range irqs {
		if _, ok := seen[irq]; ok {
			continue
		}
		seen[irq] = struct{}{}
		out = append(out, irq)
	}
	return out
}

func (m *VM) setupDevices() error {
	mem := m.kvmVM.Memory()
	slot := 0

	// UART (serial console) — not virtio, handled via IO exits
	// Firecracker signals serial interrupts via EventFd pulses, not a
	// level-held line, so we mirror that behavior here.
	consoleOut := m.cfg.ConsoleOut
	if consoleOut == nil {
		consoleOut = os.Stdout
	}
	consoleIn := m.cfg.ConsoleIn
	if consoleIn == nil {
		consoleIn = os.Stdin
	}
	// IRQ delivery on x86 uses KVM_IRQFD: each device owns an eventfd and
	// writing to it injects the GSI without a ioctl(KVM_IRQ_LINE). The
	// eventfds are registered with KVM in postCreateVCPUs once GSI routing
	// exists; the order of makeEventFDIRQFn calls here must mirror the GSI
	// order produced by setupIRQs (COM1, then each transport in append order)
	// so registerIRQFDs below can pair them.
	_, serialIRQFn, err := m.makeEventFDIRQFn()
	if err != nil {
		return fmt.Errorf("serial irqfd: %w", err)
	}
	m.uart0 = uart.New(consoleOut, consoleIn, serialIRQFn)
	m.i8042 = i8042.New(func() {
		gclog.VMM.Info("guest reboot requested via i8042", "id", m.cfg.ID)
		m.events.Emit(EventShutdown, "guest reboot requested via i8042")
		m.Stop()
	})

	// virtio-rng (entropy source for guest /dev/random)
	{
		base := uint64(VirtioBase) + uint64(slot)*VirtioStride
		irq := uint8(VirtioIRQBase + slot)
		_, irqFn, err := m.makeEventFDIRQFn()
		if err != nil {
			return fmt.Errorf("virtio-rng irqfd: %w", err)
		}
		rng := virtio.NewRNGDevice(mem, base, irq, m.memDirty, irqFn)
		rng.SetRateLimiter(buildRateLimiter(m.cfg.RNGRateLimiter))
		m.rngDev = rng
		m.transports = append(m.transports, rng.Transport)
		slot++
	}

	// virtio-balloon
	if m.cfg.Balloon != nil {
		base := uint64(VirtioBase) + uint64(slot)*VirtioStride
		irq := uint8(VirtioIRQBase + slot)
		_, irqFn, err := m.makeEventFDIRQFn()
		if err != nil {
			return fmt.Errorf("virtio-balloon irqfd: %w", err)
		}
		balloon := virtio.NewBalloonDevice(mem, base, irq, virtio.BalloonDeviceConfig{
			AmountMiB:            m.cfg.Balloon.AmountMiB,
			DeflateOnOOM:         m.cfg.Balloon.DeflateOnOOM,
			StatsPollingInterval: time.Duration(m.cfg.Balloon.StatsPollingIntervalS) * time.Second,
			SnapshotPages:        append([]uint32(nil), m.cfg.Balloon.SnapshotPages...),
		}, m.memDirty, irqFn)
		m.balloonDev = balloon
		m.transports = append(m.transports, balloon.Transport)
		slot++
	}

	// virtio-net
	if m.cfg.TapName != "" {
		mac := m.cfg.MACAddr
		if mac == nil {
			mac = defaultGuestMAC(m.cfg.ID, m.cfg.TapName)
		}
		base := uint64(VirtioBase) + uint64(slot)*VirtioStride
		irq := uint8(VirtioIRQBase + slot)
		_, irqFn, err := m.makeEventFDIRQFn()
		if err != nil {
			return fmt.Errorf("virtio-net irqfd: %w", err)
		}
		nd, err := virtio.NewNetDevice(mem, base, irq, mac, m.cfg.TapName, m.memDirty, irqFn)
		if err != nil {
			return fmt.Errorf("virtio-net: %w", err)
		}
		// Per-direction limiters override the generic NetRateLimiter when
		// set. If only NetRateLimiter is provided, we apply it to both.
		rxCfg := m.cfg.RxNetRateLimiter
		txCfg := m.cfg.TxNetRateLimiter
		if rxCfg == nil {
			rxCfg = m.cfg.NetRateLimiter
		}
		if txCfg == nil {
			txCfg = m.cfg.NetRateLimiter
		}
		nd.SetRateLimiters(buildRateLimiter(rxCfg), buildRateLimiter(txCfg))
		m.netDev = nd
		m.transports = append(m.transports, nd.Transport)
		slot++
	}

	// virtio-vsock — no listenFn needed: exec agent now listens inside the
	// guest and the host dials in (Device.Dial) rather than the reverse.
	if m.cfg.Vsock != nil && m.cfg.Vsock.Enabled {
		base := uint64(VirtioBase) + uint64(slot)*VirtioStride
		irq := uint8(VirtioIRQBase + slot)
		_, irqFn, err := m.makeEventFDIRQFn()
		if err != nil {
			return fmt.Errorf("virtio-vsock irqfd: %w", err)
		}
		vsockDev := vsock.NewDevice(mem, base, irq, nil, m.memDirty, irqFn)
		vsockDev.Label = m.cfg.ID
		m.vsockDev = vsockDev
		m.transports = append(m.transports, vsockDev.Transport)
		slot++

		if m.cfg.Vsock.UDSPath != "" {
			listener, err := newUDSListener(m.cfg.Vsock.UDSPath, m)
			if err != nil {
				return fmt.Errorf("vsock uds listener: %w", err)
			}
			m.udsListener = listener
			go listener.run()
		}
	}

	// virtio-blk
	for _, drive := range m.cfg.DriveList() {
		base := uint64(VirtioBase) + uint64(slot)*VirtioStride
		irq := uint8(VirtioIRQBase + slot)
		_, irqFn, err := m.makeEventFDIRQFn()
		if err != nil {
			return fmt.Errorf("virtio-blk %s irqfd: %w", drive.ID, err)
		}
		bd, err := virtio.NewBlockDeviceWithOptions(mem, base, irq, drive.Path, drive.ReadOnly, m.memDirty, irqFn, virtio.BlockDeviceOptions{
			SkipDiscardProbe: m.restoring,
			Discard:          m.restoredDiscard[drive.ID],
		})
		if err != nil {
			return fmt.Errorf("virtio-blk %s: %w", drive.ID, err)
		}
		bd.SetRateLimiter(buildRateLimiter(drive.RateLimiter))
		if drive.Root && m.blkDev == nil {
			m.blkDev = bd
		}
		m.blkDevs = append(m.blkDevs, bd)
		m.transports = append(m.transports, bd.Transport)
		slot++
	}

	for _, fsCfg := range m.cfg.SharedFS {
		base := uint64(VirtioBase) + uint64(slot)*VirtioStride
		irq := uint8(VirtioIRQBase + slot)
		_, irqFn, err := m.makeEventFDIRQFn()
		if err != nil {
			return fmt.Errorf("virtio-fs %s irqfd: %w", fsCfg.Tag, err)
		}
		fsDev, err := virtio.NewFSDevice(mem, m.kvmVM.MemoryFD(), base, irq, fsCfg.Source, fsCfg.Tag, fsCfg.SocketPath, m.memDirty, irqFn)
		if err != nil {
			return fmt.Errorf("virtio-fs %s: %w", fsCfg.Tag, err)
		}
		m.fsDevs = append(m.fsDevs, fsDev)
		m.transports = append(m.transports, fsDev.Transport)
		slot++
	}

	return nil
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

// ---- vCPU run loop ----

func (m *VM) runLoop(vcpu *kvm.VCPU) {
	runtime.LockOSThread()
	if err := seccomp.InstallThreadProfile(seccomp.ProfileVCPU); err != nil {
		gclog.VMM.Error("install vcpu seccomp profile failed", "id", m.cfg.ID, "vcpu", vcpu.ID, "error", err)
		m.events.Emit(EventError, fmt.Sprintf("install vcpu seccomp profile: %v", err))
		// Seccomp was NOT installed, so the thread is clean — unlock it
		// to avoid leaking a locked OS thread.
		runtime.UnlockOSThread()
		m.Stop()
		m.runWG.Done()
		return
	}
	m.registerVCPUThread(vcpu.ID, unix.Gettid())
	defer m.runWG.Done()
	defer func() {
		m.unregisterVCPUThread(vcpu.ID)
		// DO NOT unlock after successful seccomp install — the thread
		// is tainted with a strict filter and must not be reused.
	}()
	for {
		if m.waitIfPaused(vcpu.ID) {
			return
		}

		select {
		case <-m.stopCh:
			return
		default:
		}

		interrupted := false
		if err := vcpu.Run(); err != nil {
			// Firecracker treats both EINTR and EAGAIN as transient KVM_RUN
			// conditions, but they still mean "leave KVM_RUN and process
			// pending control flow" before re-entering.
			if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
				interrupted = true
			} else {
				gclog.VMM.Error("vcpu run error", "id", m.cfg.ID, "vcpu", vcpu.ID, "error", err)
				m.events.Emit(EventError, fmt.Sprintf("vcpu %d run: %v", vcpu.ID, err))
				m.Stop()
				return
			}
		}

		if !interrupted {
			switch vcpu.RunData.ExitReason {
			case kvm.ExitHLT:
				// A 1-vCPU guest executing `hlt` normally means
				// "done" (no sibling thread can wake it) — this is
				// Firecracker's behavior too (VcpuExit::Hlt →
				// VcpuEmulation::Stopped). But when exec is enabled
				// the VM is expected to stay alive idling between
				// host-initiated exec calls; and a guest resumed
				// from snapshot captured mid-`hlt` re-enters `hlt`
				// on its first KVM_RUN. Keep the VM alive while exec
				// is wired so the idle HLT wakes on the next device
				// IRQ (vsock dial from the host, timer tick, etc).
				if len(m.vcpus) == 1 && (m.cfg.Exec == nil || !m.cfg.Exec.Enabled) {
					gclog.VMM.Info("guest HLT", "id", m.cfg.ID, "vcpu", vcpu.ID)
					m.events.Emit(EventHalted, "guest HLT")
					m.Stop()
					return
				}
				// With in-kernel IRQCHIP, re-entering KVM_RUN after HLT
				// blocks the vCPU thread inside the kernel until the next
				// interrupt (PIT, LAPIC timer, IPI). No userspace sleep
				// needed — the kernel's kvm_vcpu_block handles it.

			case kvm.ExitIO:
				m.handleIO(vcpu)

			case kvm.ExitMMIO:
				m.handleMMIO(vcpu)

			case kvm.ExitSystemEvent:
				if handled, stop, err := m.archBackend.handleExit(m, vcpu); err != nil {
					gclog.VMM.Error("arch exit handling failed", "id", m.cfg.ID, "vcpu", vcpu.ID, "error", err)
					m.events.Emit(EventError, fmt.Sprintf("arch-specific exit handling on vcpu %d: %v", vcpu.ID, err))
					m.Stop()
					return
				} else if handled {
					if stop {
						m.Stop()
						return
					}
					continue
				}
				gclog.VMM.Warn("unhandled system event", "id", m.cfg.ID, "vcpu", vcpu.ID)
				m.events.Emit(EventError, fmt.Sprintf("unhandled system event on vcpu %d", vcpu.ID))
				m.Stop()
				return

			case kvm.ExitShutdown:
				gclog.VMM.Info("guest shutdown", "id", m.cfg.ID, "vcpu", vcpu.ID)
				m.events.Emit(EventShutdown, "guest shutdown")
				m.Stop()
				return

			case kvm.ExitIRQWindowOpen:
				vcpu.RunData.RequestInterruptWindow = 0

			case kvm.ExitInternalError:
				gclog.VMM.Error("KVM internal error", "id", m.cfg.ID, "vcpu", vcpu.ID)
				m.events.Emit(EventError, fmt.Sprintf("KVM internal error on vcpu %d", vcpu.ID))
				m.Stop()
				return

			case kvm.ExitFailEntry:
				gclog.VMM.Error("KVM fail entry", "id", m.cfg.ID, "vcpu", vcpu.ID)
				m.events.Emit(EventError, fmt.Sprintf("KVM fail entry on vcpu %d (bad guest state)", vcpu.ID))
				m.Stop()
				return

			case kvm.ExitUnknown:
				gclog.VMM.Warn("KVM exit unknown", "id", m.cfg.ID, "vcpu", vcpu.ID)
				m.events.Emit(EventError, fmt.Sprintf("KVM exit unknown on vcpu %d", vcpu.ID))
				m.Stop()
				return

			default:
				gclog.VMM.Warn("unhandled exit reason", "id", m.cfg.ID, "vcpu", vcpu.ID, "reason", vcpu.RunData.ExitReason)
				m.events.Emit(EventError, fmt.Sprintf("unhandled exit reason on vcpu %d: %d", vcpu.ID, vcpu.RunData.ExitReason))
				m.Stop()
				return
			}
		}
	}
}

func (m *VM) waitIfPaused(vcpuID int) bool {
	for {
		m.mu.Lock()
		paused := m.state == StatePaused
		stopped := m.state == StateStopped
		if paused {
			m.pausedVCPUs[vcpuID] = struct{}{}
		} else {
			delete(m.pausedVCPUs, vcpuID)
		}
		m.mu.Unlock()

		if stopped {
			return true
		}
		if !paused {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (m *VM) cleanup() {
	m.cleanupOnce.Do(func() {
		// The UDS listener holds bufio/io.Copy goroutines that call
		// DialVsock; close it FIRST (without holding vsockDialMu) so those
		// goroutines exit and release their read locks. Then take the
		// write lock and tear devices down.
		if m.udsListener != nil {
			_ = m.udsListener.Close()
			m.udsListener = nil
		}

		// Block until any in-flight DialVsock call completes before freeing
		// devices and guest RAM.  See vsockDialMu on the VM struct.
		m.vsockDialMu.Lock()
		defer m.vsockDialMu.Unlock()

		if m.vsockDev != nil {
			m.vsockDev.Close()
		}
		if m.balloonDev != nil {
			_ = m.balloonDev.Close()
		}
		for _, fsDev := range m.fsDevs {
			if err := fsDev.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				gclog.VMM.Warn("virtio-fs cleanup failed", "id", m.cfg.ID, "error", err)
			}
		}
		for _, blkDev := range m.blkDevs {
			if err := blkDev.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				gclog.VMM.Warn("block device cleanup failed", "id", m.cfg.ID, "error", err)
			}
		}
		if m.netDev != nil {
			if err := m.netDev.Close(); err != nil {
				gclog.VMM.Warn("net device cleanup failed", "id", m.cfg.ID, "error", err)
			}
		}
		for _, efd := range m.irqEventFds {
			unix.Close(efd)
		}
		m.irqEventFds = nil
		for _, vcpu := range m.vcpus {
			if vcpu != nil {
				if err := vcpu.Close(); err != nil {
					gclog.VMM.Warn("vcpu cleanup failed", "id", m.cfg.ID, "error", err)
				}
			}
		}
		if m.kvmVM != nil {
			if err := m.kvmVM.Close(); err != nil {
				gclog.VMM.Warn("kvm vm cleanup failed", "id", m.cfg.ID, "error", err)
			}
		}
		if m.kvmSys != nil {
			if err := m.kvmSys.Close(); err != nil {
				gclog.VMM.Warn("kvm system cleanup failed", "id", m.cfg.ID, "error", err)
			}
		}
	})
}

func (m *VM) prepareSnapshot() error {
	if m.cfg.MemoryHotplug != nil {
		return fmt.Errorf("snapshot/restore is not supported with memory hotplug yet")
	}
	if m.cfg.HasAdditionalDrives() {
		return fmt.Errorf("snapshot/restore is not supported with additional block devices yet")
	}
	if m.blkDev != nil {
		if err := m.blkDev.PrepareSnapshot(); err != nil {
			return fmt.Errorf("prepare block device snapshot: %w", err)
		}
	}
	return nil
}

func transportInBlockDevices(blocks []*virtio.BlockDevice, transport *virtio.Transport) bool {
	for _, blkDev := range blocks {
		if blkDev != nil && blkDev.Transport == transport {
			return true
		}
	}
	return false
}

func (m *VM) registerVCPUThread(id, tid int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vcpuTIDs[id] = tid
}

func (m *VM) unregisterVCPUThread(id int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.vcpuTIDs, id)
	delete(m.pausedVCPUs, id)
}

func (m *VM) kickVCPUs() {
	m.mu.Lock()
	tids := make([]int, 0, len(m.vcpuTIDs))
	for _, tid := range m.vcpuTIDs {
		if tid > 0 {
			tids = append(tids, tid)
		}
	}
	m.mu.Unlock()
	pid := os.Getpid()
	for _, tid := range tids {
		_ = unix.Tgkill(pid, tid, vcpuKickSignal)
	}
}

func clearSignalRestart(sig syscall.Signal) error {
	var act linuxSigaction
	if _, _, errno := unix.RawSyscall6(
		unix.SYS_RT_SIGACTION,
		uintptr(sig),
		0,
		uintptr(unsafe.Pointer(&act)),
		unsafe.Sizeof(act.mask),
		0,
		0,
	); errno != 0 {
		return errno
	}
	act.flags &^= linuxSARestart
	if _, _, errno := unix.RawSyscall6(
		unix.SYS_RT_SIGACTION,
		uintptr(sig),
		uintptr(unsafe.Pointer(&act)),
		0,
		unsafe.Sizeof(act.mask),
		0,
		0,
	); errno != 0 {
		return errno
	}
	return nil
}

func (m *VM) handleIO(vcpu *kvm.VCPU) {
	io := vcpu.GetIOData()
	port := io.Port
	if m.uart0 != nil && port >= COM1Base && port < COM1Base+8 {
		offset := uint8(port - COM1Base)
		if io.Direction == 1 { // out
			b := *vcpu.RunDataByte(io.DataOffset)
			m.uart0.Write(offset, b)
		} else { // in
			*vcpu.RunDataByte(io.DataOffset) = m.uart0.Read(offset)
		}
	} else if m.i8042 != nil && (port == I8042Base || port == I8042Base+4) {
		offset := uint8(port - I8042Base)
		if io.Direction == 1 { // out
			m.i8042.Write(offset, *vcpu.RunDataByte(io.DataOffset))
		} else { // in
			*vcpu.RunDataByte(io.DataOffset) = m.i8042.Read(offset)
		}
	} else if io.Direction == 0 { // unhandled IN: return 0xFF (no device)
		*vcpu.RunDataByte(io.DataOffset) = 0xFF
	}
}

func (m *VM) handleMMIO(vcpu *kvm.VCPU) {
	mmio := vcpu.GetMMIOData()
	// ARM64 UART dispatch via MMIO (Firecracker uses ns16550a at 0x40002000).
	// On x86, uart0 is accessed via I/O ports in handleIO; on ARM64 it's MMIO.
	// PL031 RTC at 0x40001000 (Firecracker: RTC_MEM_START).
	if m.rtcDev != nil {
		const rtcBase = 0x40001000
		const rtcSize = 0x1000
		if mmio.PhysAddr >= rtcBase && mmio.PhysAddr < rtcBase+rtcSize {
			offset := uint16(mmio.PhysAddr - rtcBase)
			if mmio.IsWrite == 1 {
				m.rtcDev.WriteBytes(offset, mmio.Data[:mmio.Len])
			} else {
				for i := range mmio.Data {
					mmio.Data[i] = 0
				}
				m.rtcDev.ReadBytes(offset, mmio.Data[:mmio.Len])
			}
			return
		}
	}
	if m.uart0 != nil {
		const serialBase = 0x40002000
		const serialSize = 0x1000
		if mmio.PhysAddr >= serialBase && mmio.PhysAddr < serialBase+serialSize {
			offset := uint8(mmio.PhysAddr - serialBase)
			if mmio.IsWrite == 1 {
				m.uart0.Write(offset, mmio.Data[0])
			} else {
				for i := range mmio.Data {
					mmio.Data[i] = 0
				}
				mmio.Data[0] = m.uart0.Read(offset)
			}
			return
		}
	}
	for _, t := range m.transports {
		base := t.BasePA()
		if mmio.PhysAddr >= base && mmio.PhysAddr < base+VirtioStride {
			offset := uint32(mmio.PhysAddr - base)
			if mmio.IsWrite == 1 {
				t.WriteBytes(offset, mmio.Data[:mmio.Len])
			} else {
				for i := range mmio.Data {
					mmio.Data[i] = 0
				}
				t.ReadBytes(offset, mmio.Data[:mmio.Len])
			}
			return
		}
	}
}

func countDirtyBits(bitmap []uint64) int {
	n := 0
	for _, w := range bitmap {
		n += bits.OnesCount64(w)
	}
	return n
}

// augmentDirtyBitmap adds bits for any non-zero "clean" page. The kernel/initrd
// are loaded by the host directly into guest RAM before the guest runs — those
// writes don't trigger KVM dirty tracking, so the pages come back clean even
// though they hold real data. Walking the clean pages for a non-zero byte is
// ~11ms for 100MB on commodity hardware and guarantees correctness.
func augmentDirtyBitmap(mem []byte, bitmap []uint64) {
	pageSize := unix.Getpagesize()
	totalPages := (len(mem) + pageSize - 1) / pageSize
	for pageIdx := 0; pageIdx < totalPages; pageIdx++ {
		wordIdx := pageIdx / 64
		bitIdx := uint(pageIdx % 64)
		if wordIdx >= len(bitmap) {
			break
		}
		if bitmap[wordIdx]&(1<<bitIdx) != 0 {
			continue // already dirty
		}
		offset := pageIdx * pageSize
		end := offset + pageSize
		if end > len(mem) {
			end = len(mem)
		}
		if !isAllZero(mem[offset:end]) {
			bitmap[wordIdx] |= 1 << bitIdx
		}
	}
}

// isAllZero returns true when buf contains only zero bytes. Uses the 8-byte
// stride fast path — one comparison per 8 bytes — which is 5-10 GB/s on modern
// CPUs, fast enough to scan 100 MB in ~15 ms.
func isAllZero(buf []byte) bool {
	i := 0
	for ; i+8 <= len(buf); i += 8 {
		if binary.LittleEndian.Uint64(buf[i:]) != 0 {
			return false
		}
	}
	for ; i < len(buf); i++ {
		if buf[i] != 0 {
			return false
		}
	}
	return true
}

// saveDirtyPages writes only the dirty pages into a true sparse file.
// The file is truncated to the full guest RAM size; non-dirty regions remain
// as holes and read back as zero via MAP_PRIVATE — identical to the original
// MAP_ANONYMOUS state. Dirty pages are coalesced into sequential WriteAt calls
// to avoid per-page syscall overhead (~11K pages → ~50 large writes).
// Restore uses MAP_PRIVATE of this file (lazy, O(1) cost).
func saveDirtyPages(path string, mem []byte, bitmap []uint64) error {
	pageSize := unix.Getpagesize()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(int64(len(mem))); err != nil {
		return err
	}

	// Coalesce consecutive dirty pages into single WriteAt calls.
	runStart, runLen := -1, 0
	flush := func() error {
		if runStart < 0 {
			return nil
		}
		start := runStart * pageSize
		end := start + runLen*pageSize
		if end > len(mem) {
			end = len(mem)
		}
		_, err := f.WriteAt(mem[start:end], int64(start))
		runStart, runLen = -1, 0
		return err
	}
	for wi, word := range bitmap {
		if word == 0 {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		for bit := 0; bit < 64; bit++ {
			pageIdx := wi*64 + bit
			if pageIdx*pageSize >= len(mem) {
				break
			}
			if word&(1<<uint(bit)) != 0 {
				if runStart < 0 {
					runStart = pageIdx
				}
				runLen++
			} else {
				if err := flush(); err != nil {
					return err
				}
			}
		}
	}
	return flush()
}

// saveMemAsync writes the VM's guest RAM to a file using mmap+msync(MS_ASYNC).
// The approach: create the file, fallocate to the right size, mmap MAP_SHARED,
// copy the VM pages into the file-backed mapping, then msync(MS_ASYNC) to kick
// off the kernel's async write-back. The function returns as soon as the memcpy
// finishes — the disk I/O happens in background in the kernel's page reclaim.
// This is equivalent to os.WriteFile but without blocking on disk flush.
func saveMemAsync(path string, mem []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	size := len(mem)
	if size == 0 {
		return nil
	}
	// Truncate (not Fallocate): creates a sparse file with no disk I/O —
	// only the file size metadata is set. Blocks are allocated on first write
	// when the kernel flushes dirty pages from the page cache to disk. This
	// is ~10x faster than Fallocate(mode=0) which zero-fills all disk blocks.
	if err := f.Truncate(int64(size)); err != nil {
		return err
	}
	// Map the file as writable shared mapping. Writes to this mapping go into
	// the page cache immediately and are flushed to disk by the kernel in
	// background — no synchronous disk I/O on our goroutine.
	mapped, err := unix.Mmap(int(f.Fd()), 0, size,
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return fmt.Errorf("mmap output file: %w", err)
	}
	copy(mapped, mem)
	// Kick off async write-back. MS_ASYNC schedules the flush without
	// blocking; the data is already in the page cache after copy().
	_ = unix.Msync(mapped, unix.MS_ASYNC)
	// Do NOT Munmap: on Linux, munmap(MAP_SHARED) with dirty pages blocks
	// until all dirty pages are flushed to disk, defeating the async intent.
	// The mapping leaks until the process exits — acceptable for the single-
	// use worker subprocess that terminates after the snapshot is complete.
	// For long-lived processes (runLocal path), this is ~229 MB of virtual
	// address space held until VM cleanup; acceptable given VM lifetime.
	return nil
}
