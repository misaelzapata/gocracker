//go:build darwin

package vmm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Code-Hex/vz/v3"
	"github.com/gocracker/gocracker/internal/stacknet"
)

// VM is a virtual machine backed by Apple's Virtualization.framework.
type VM struct {
	mu     sync.Mutex
	cfg    Config
	state  State
	stopCh chan struct{}
	doneCh chan struct{}

	vzVM   *vz.VirtualMachine
	vzConf *vz.VirtualMachineConfiguration

	// Console I/O pipes. The host reads guestWriteEnd and writes guestReadEnd.
	hostReadPipe  *os.File // host reads guest output from here
	hostWritePipe *os.File // host writes guest input here
	guestReadEnd  *os.File // connected to vz serial (guest reads)
	guestWriteEnd *os.File // connected to vz serial (guest writes)

	consoleBuf []byte
	consoleMu  sync.Mutex

	nextConsoleAttachID uint64
	consoleAttachments  map[uint64]*darwinConsoleAttachment

	execBroker *execAgentBroker

	startTime   time.Time
	cleanupOnce sync.Once
	stopOnce    sync.Once
	exitCode    int
	events      *EventLog
}

type darwinConsoleAttachment struct {
	vm *VM
	id uint64

	guestRead  *os.File
	guestWrite *os.File
	hostRead   *os.File
	hostWrite  *os.File

	closeOnce sync.Once
}

// New creates a VM using Apple Virtualization.framework.
func New(cfg Config) (*VM, error) {
	if cfg.MemMB == 0 {
		cfg.MemMB = 128
	}
	if cfg.VCPUs <= 0 {
		cfg.VCPUs = 1
	}
	if cfg.ID == "" {
		cfg.ID = fmt.Sprintf("vm-%d", time.Now().UnixNano())
	}
	arch, err := normalizeMachineArch(cfg.Arch)
	if err != nil {
		return nil, err
	}
	if err := validateMachineArch(arch); err != nil {
		return nil, err
	}
	cfg.Arch = string(arch)

	if cfg.TapName != "" {
		return nil, ErrTAPNotSupported
	}
	if cfg.MemoryHotplug != nil {
		return nil, ErrHotplugNotSupported
	}
	if cfg.Balloon != nil && cfg.Balloon.Auto == "" {
		cfg.Balloon.Auto = BalloonAutoOff
	}

	if cfg.KernelPath == "" {
		return nil, fmt.Errorf("kernel path is required")
	}
	if _, err := networkAttachmentMode(cfg.Network); err != nil {
		return nil, err
	}

	// Boot loader
	bootOpts := []vz.LinuxBootLoaderOption{}
	if cfg.InitrdPath != "" {
		bootOpts = append(bootOpts, vz.WithInitrd(cfg.InitrdPath))
	}
	if cfg.Cmdline != "" {
		bootOpts = append(bootOpts, vz.WithCommandLine(cfg.Cmdline))
	}
	bootLoader, err := vz.NewLinuxBootLoader(cfg.KernelPath, bootOpts...)
	if err != nil {
		return nil, fmt.Errorf("create linux boot loader: %w", err)
	}

	memBytes := cfg.MemMB * 1024 * 1024
	vzConf, err := vz.NewVirtualMachineConfiguration(bootLoader, uint(cfg.VCPUs), memBytes)
	if err != nil {
		return nil, fmt.Errorf("create vm configuration: %w", err)
	}

	// Storage devices
	storageDevices, err := configureBlockDevices(cfg)
	if err != nil {
		return nil, err
	}
	vzConf.SetStorageDevicesVirtualMachineConfiguration(storageDevices)

	// Network (NAT)
	netDevices, err := configureNetworkDevices(cfg)
	if err != nil {
		return nil, err
	}
	vzConf.SetNetworkDevicesVirtualMachineConfiguration(netDevices)

	// Serial console
	hostReadPipe, guestWriteEnd, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create console output pipe: %w", err)
	}
	guestReadEnd, hostWritePipe, err := os.Pipe()
	if err != nil {
		hostReadPipe.Close()
		guestWriteEnd.Close()
		return nil, fmt.Errorf("create console input pipe: %w", err)
	}
	serialAttachment, err := vz.NewFileHandleSerialPortAttachment(guestReadEnd, guestWriteEnd)
	if err != nil {
		return nil, fmt.Errorf("create serial attachment: %w", err)
	}
	serialConfig, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(serialAttachment)
	if err != nil {
		return nil, fmt.Errorf("create serial config: %w", err)
	}
	vzConf.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{serialConfig})

	// Entropy (RNG)
	entropyConfig, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("create entropy config: %w", err)
	}
	vzConf.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropyConfig})

	// Memory balloon
	if cfg.Balloon != nil {
		balloonConfig, err := vz.NewVirtioTraditionalMemoryBalloonDeviceConfiguration()
		if err != nil {
			return nil, fmt.Errorf("create balloon config: %w", err)
		}
		vzConf.SetMemoryBalloonDevicesVirtualMachineConfiguration([]vz.MemoryBalloonDeviceConfiguration{balloonConfig})
	}

	// Shared directories (virtio-fs)
	fsDevices, err := configureSharedDirectories(cfg)
	if err != nil {
		return nil, err
	}
	if len(fsDevices) > 0 {
		vzConf.SetDirectorySharingDevicesVirtualMachineConfiguration(fsDevices)
	}

	// Vsock
	var vsockConfig *vz.VirtioSocketDeviceConfiguration
	if cfg.Vsock != nil && cfg.Vsock.Enabled {
		vsockConfig, err = vz.NewVirtioSocketDeviceConfiguration()
		if err != nil {
			return nil, fmt.Errorf("create vsock config: %w", err)
		}
		vzConf.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{vsockConfig})
	}

	validated, err := vzConf.Validate()
	if err != nil {
		return nil, fmt.Errorf("validate vm configuration: %w", err)
	}
	if !validated {
		return nil, fmt.Errorf("vm configuration validation failed")
	}

	vzVM, err := vz.NewVirtualMachine(vzConf)
	if err != nil {
		return nil, fmt.Errorf("create virtual machine: %w", err)
	}

	m := &VM{
		cfg:           cfg,
		state:         StateCreated,
		stopCh:        make(chan struct{}),
		doneCh:        make(chan struct{}),
		vzVM:          vzVM,
		vzConf:        vzConf,
		hostReadPipe:  hostReadPipe,
		hostWritePipe: hostWritePipe,
		guestReadEnd:  guestReadEnd,
		guestWriteEnd: guestWriteEnd,
		events:        NewEventLog(),
	}

	if cfg.Exec != nil && cfg.Exec.Enabled && cfg.Vsock != nil && cfg.Vsock.Enabled {
		port := cfg.Exec.VsockPort
		if port == 0 {
			port = 512
		}
		m.execBroker = newExecAgentBroker(port)
	}

	m.events.Emit(EventCreated, fmt.Sprintf("VM %s created, %d MiB RAM, %d vCPU(s)", cfg.ID, cfg.MemMB, cfg.VCPUs))

	// Start console I/O goroutines
	go m.bufferConsoleOutput()
	if cfg.ConsoleIn != nil {
		go m.pumpConsoleInput()
	}

	return m, nil
}

// Start launches the virtual machine.
func (m *VM) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != StateCreated && m.state != StatePaused {
		return fmt.Errorf("cannot start VM in state %s", m.state)
	}
	if m.state == StatePaused {
		if err := m.vzVM.Resume(); err != nil {
			return fmt.Errorf("resume vm: %w", err)
		}
		m.state = StateRunning
		m.events.Emit(EventResumed, "VM resumed")
		if m.execBroker != nil && m.cfg.Vsock != nil && m.cfg.Vsock.Enabled {
			go m.runVsockExecListener()
		}
		return nil
	}
	m.events.Emit(EventStarting, fmt.Sprintf("starting %d vCPU(s)", m.cfg.VCPUs))
	if err := m.vzVM.Start(); err != nil {
		return fmt.Errorf("start vm: %w", err)
	}
	m.state = StateRunning
	m.startTime = time.Now()
	m.events.Emit(EventRunning, fmt.Sprintf("%d vCPU(s) started", m.cfg.VCPUs))

	go m.watchStateChanges()

	// Start vsock exec agent listener if configured
	if m.execBroker != nil && m.cfg.Vsock != nil && m.cfg.Vsock.Enabled {
		go m.runVsockExecListener()
	}

	return nil
}

// Stop signals the VM to halt.
func (m *VM) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == StateRunning || m.state == StatePaused {
		m.state = StateStopped
		go func() {
			if ok, _ := m.vzVM.RequestStop(); !ok {
				_ = m.vzVM.Stop()
			}
		}()
		select {
		case <-m.stopCh:
		default:
			close(m.stopCh)
		}
	}
}

// Pause pauses the virtual machine.
func (m *VM) Pause() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != StateRunning {
		return fmt.Errorf("cannot pause VM in state %s", m.state)
	}
	if err := m.vzVM.Pause(); err != nil {
		return fmt.Errorf("pause vm: %w", err)
	}
	m.state = StatePaused
	m.events.Emit(EventPaused, "VM paused")
	return nil
}

// Resume resumes a previously paused VM.
func (m *VM) Resume() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != StatePaused {
		return fmt.Errorf("cannot resume VM in state %s", m.state)
	}
	if err := m.vzVM.Resume(); err != nil {
		return fmt.Errorf("resume vm: %w", err)
	}
	m.state = StateRunning
	m.events.Emit(EventResumed, "VM resumed")
	if m.execBroker != nil && m.cfg.Vsock != nil && m.cfg.Vsock.Enabled {
		go m.runVsockExecListener()
	}
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

// VMConfig returns the VM's configuration.
func (m *VM) VMConfig() Config { return m.cfg }

// DeviceList returns info about attached devices.
func (m *VM) DeviceList() []DeviceInfo {
	var devs []DeviceInfo
	for _, d := range m.cfg.DriveList() {
		devs = append(devs, DeviceInfo{Type: "virtio-blk:" + d.ID})
	}
	netType := "virtio-net:nat"
	if mode, err := networkAttachmentMode(m.cfg.Network); err == nil && mode == NetworkAttachmentStack {
		netType = "virtio-net:vmnet"
	}
	devs = append(devs, DeviceInfo{Type: netType})
	devs = append(devs, DeviceInfo{Type: "virtio-rng"})
	if m.cfg.Balloon != nil {
		devs = append(devs, DeviceInfo{Type: "virtio-balloon"})
	}
	for _, fs := range m.cfg.SharedFS {
		devs = append(devs, DeviceInfo{Type: "virtio-fs:" + fs.Tag})
	}
	if m.cfg.Vsock != nil && m.cfg.Vsock.Enabled {
		devs = append(devs, DeviceInfo{Type: "virtio-vsock"})
	}
	devs = append(devs, DeviceInfo{Type: "virtio-console"})
	return devs
}

// ConsoleOutput returns the buffered console output.
func (m *VM) ConsoleOutput() []byte {
	m.consoleMu.Lock()
	defer m.consoleMu.Unlock()
	out := make([]byte, len(m.consoleBuf))
	copy(out, m.consoleBuf)
	return out
}

func (m *VM) AttachConsole() (io.ReadWriteCloser, error) {
	if m == nil || m.hostWritePipe == nil || m.hostReadPipe == nil {
		return nil, fmt.Errorf("console is not available")
	}
	guestRead, hostWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	hostRead, guestWrite, err := os.Pipe()
	if err != nil {
		_ = guestRead.Close()
		_ = hostWrite.Close()
		return nil, err
	}

	attachment := &darwinConsoleAttachment{
		vm:         m,
		guestRead:  guestRead,
		guestWrite: guestWrite,
		hostRead:   hostRead,
		hostWrite:  hostWrite,
	}

	m.consoleMu.Lock()
	m.nextConsoleAttachID++
	attachment.id = m.nextConsoleAttachID
	if m.consoleAttachments == nil {
		m.consoleAttachments = map[uint64]*darwinConsoleAttachment{}
	}
	m.consoleAttachments[attachment.id] = attachment
	m.consoleMu.Unlock()

	go attachment.pumpInput()

	return attachment, nil
}

// WaitStopped blocks until the VM has fully stopped.
func (m *VM) WaitStopped(ctx context.Context) error {
	select {
	case <-m.doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TakeSnapshot pauses the VM and saves state to dir.
func (m *VM) TakeSnapshot(dir string) (*Snapshot, error) {
	return m.TakeSnapshotWithOptions(dir, SnapshotOptions{Resume: true})
}

// TakeSnapshotWithOptions saves a snapshot. Uses vz SaveMachineStateToPath (macOS 14+).
func (m *VM) TakeSnapshotWithOptions(dir string, opts SnapshotOptions) (*Snapshot, error) {
	if _, err := networkAttachmentMode(m.cfg.Network); err != nil {
		return nil, err
	}

	m.mu.Lock()
	state := m.state
	m.mu.Unlock()
	if m.vzConf == nil || m.vzVM == nil {
		return nil, fmt.Errorf("save/restore not supported for an uninitialized darwin VM")
	}

	resumeAfter := false
	switch state {
	case StateRunning:
		if err := m.Pause(); err != nil {
			return nil, err
		}
		resumeAfter = opts.Resume
	case StatePaused:
	default:
		return nil, fmt.Errorf("VM must be running or paused to snapshot (state: %s)", state)
	}
	if resumeAfter {
		defer m.Resume()
	}

	if supported, err := m.vzConf.ValidateSaveRestoreSupport(); err != nil {
		return nil, fmt.Errorf("save/restore not supported for this configuration: %w", err)
	} else if !supported {
		return nil, fmt.Errorf("save/restore not supported for this VM configuration")
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	stateFile := filepath.Join(dir, "vm-state.vzvmsave")
	if err := m.vzVM.SaveMachineStateToPath(stateFile); err != nil {
		return nil, fmt.Errorf("save vm state: %w", err)
	}

	cfg := m.cfg
	cfg.KernelPath, _ = filepath.Abs(cfg.KernelPath)
	cfg.InitrdPath, _ = filepath.Abs(cfg.InitrdPath)
	cfg.DiskImage, _ = filepath.Abs(cfg.DiskImage)

	snap := &Snapshot{
		Version:   2,
		Timestamp: time.Now(),
		ID:        m.cfg.ID,
		Config:    cfg,
		MemFile:   "vm-state.vzvmsave",
	}

	metaFile := filepath.Join(dir, "snapshot.json")
	data, _ := json.MarshalIndent(snap, "", "  ")
	if err := os.WriteFile(metaFile, data, 0644); err != nil {
		return nil, err
	}
	m.events.Emit(EventSnapshot, fmt.Sprintf("snapshot saved to %s", dir))
	return snap, nil
}

// DialVsock connects to a guest vsock port.
func (m *VM) DialVsock(port uint32) (net.Conn, error) {
	m.mu.Lock()
	if m.cfg.Vsock == nil || !m.cfg.Vsock.Enabled {
		m.mu.Unlock()
		return nil, fmt.Errorf("virtio-vsock is not configured")
	}
	if m.state != StateRunning && m.state != StatePaused {
		state := m.state
		m.mu.Unlock()
		return nil, fmt.Errorf("cannot dial vsock while VM is in state %s", state)
	}
	broker := m.execBroker
	execCfg := m.cfg.Exec
	m.mu.Unlock()

	if broker != nil && execCfg != nil && execCfg.Enabled && port == execCfg.VsockPort {
		return broker.acquire()
	}

	devices := m.vzVM.SocketDevices()
	if len(devices) == 0 {
		return nil, fmt.Errorf("no vsock device available")
	}
	conn, err := devices[0].Connect(port)
	if err != nil {
		return nil, fmt.Errorf("vsock connect to port %d: %w", port, err)
	}
	return conn, nil
}

func (m *VM) UpdateNetRateLimiter(cfg *RateLimiterConfig) error   { return nil }
func (m *VM) UpdateBlockRateLimiter(cfg *RateLimiterConfig) error { return nil }
func (m *VM) UpdateRNGRateLimiter(cfg *RateLimiterConfig) error   { return nil }

func (m *VM) GetBalloonConfig() (BalloonConfig, error) {
	if m.cfg.Balloon == nil {
		return BalloonConfig{}, fmt.Errorf("virtio-balloon is not configured")
	}
	return *m.cfg.Balloon, nil
}

func (m *VM) UpdateBalloon(update BalloonUpdate) error {
	if m.cfg.Balloon == nil {
		return fmt.Errorf("virtio-balloon is not configured")
	}
	m.cfg.Balloon.AmountMiB = update.AmountMiB
	return nil
}

func (m *VM) GetBalloonStats() (BalloonStats, error) {
	return BalloonStats{}, fmt.Errorf("balloon stats not available on macOS")
}

func (m *VM) UpdateBalloonStats(update BalloonStatsUpdate) error {
	return fmt.Errorf("balloon stats polling not available on macOS")
}

func (m *VM) GetMemoryHotplug() (MemoryHotplugStatus, error) {
	return MemoryHotplugStatus{}, ErrHotplugNotSupported
}

func (m *VM) UpdateMemoryHotplug(update MemoryHotplugSizeUpdate) error {
	return ErrHotplugNotSupported
}

func (m *VM) PrepareMigrationBundle(dir string) error {
	return ErrMigrationNotSupported
}

func (m *VM) FinalizeMigrationBundle(dir string) (*Snapshot, *MigrationPatchSet, error) {
	return nil, nil, ErrMigrationNotSupported
}

func (m *VM) ResetMigrationTracking() error {
	return ErrMigrationNotSupported
}

// RestoreFromSnapshot creates a new VM restored from a snapshot directory.
func RestoreFromSnapshot(dir string) (*VM, error) {
	return RestoreFromSnapshotWithOptions(dir, RestoreOptions{})
}

// RestoreFromSnapshotWithConsole restores a VM with console overrides.
func RestoreFromSnapshotWithConsole(dir string, consoleIn interface{}, consoleOut interface{}) (*VM, error) {
	opts := RestoreOptions{}
	if r, ok := consoleIn.(io.Reader); ok {
		opts.ConsoleIn = r
	}
	if w, ok := consoleOut.(io.Writer); ok {
		opts.ConsoleOut = w
	}
	return RestoreFromSnapshotWithOptions(dir, opts)
}

// RestoreFromSnapshotWithOptions creates a new VM restored from a snapshot.
// On macOS, cross-process restore is not supported by Virtualization.framework.
// The state file can only be restored into the same VM object that created it.
// Use the API server's snapshot/restore endpoints for in-process restore.
func RestoreFromSnapshotWithOptions(dir string, opts RestoreOptions) (*VM, error) {
	metaFile := filepath.Join(dir, "snapshot.json")
	data, err := os.ReadFile(metaFile)
	if err != nil {
		return nil, fmt.Errorf("read snapshot: %w", err)
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, err
	}

	cfg := snap.Config
	if _, err := networkAttachmentMode(cfg.Network); err != nil {
		return nil, err
	}
	if opts.ConsoleIn != nil {
		cfg.ConsoleIn = opts.ConsoleIn
	}
	if opts.ConsoleOut != nil {
		cfg.ConsoleOut = opts.ConsoleOut
	}
	if opts.OverrideID != "" {
		cfg.ID = opts.OverrideID
	}
	if opts.OverrideVCPUs > 0 {
		cfg.VCPUs = opts.OverrideVCPUs
	}

	// Resolve paths relative to snapshot dir
	if cfg.KernelPath != "" && !filepath.IsAbs(cfg.KernelPath) {
		cfg.KernelPath = filepath.Join(dir, cfg.KernelPath)
	}
	if cfg.InitrdPath != "" && !filepath.IsAbs(cfg.InitrdPath) {
		cfg.InitrdPath = filepath.Join(dir, cfg.InitrdPath)
	}
	if cfg.DiskImage != "" && !filepath.IsAbs(cfg.DiskImage) {
		cfg.DiskImage = filepath.Join(dir, cfg.DiskImage)
	}

	vm, err := New(cfg)
	if err != nil {
		return nil, fmt.Errorf("create vm for restore: %w", err)
	}

	stateFile := filepath.Join(dir, snap.MemFile)
	if err := vm.vzVM.RestoreMachineStateFromURL(stateFile); err != nil {
		return nil, fmt.Errorf("restore vm state: %w", err)
	}

	vm.state = StatePaused
	vm.events.Emit(EventRestored, fmt.Sprintf("restored from %s", dir))
	return vm, nil
}

// ApplyMigrationPatches applies dirty memory patches. Not supported on macOS.
func ApplyMigrationPatches(dir string) error {
	return ErrMigrationNotSupported
}

// RewriteSnapshotBundleWithConfig rewrites snapshot metadata. Not supported on macOS.
func RewriteSnapshotBundleWithConfig(dir string, cfg Config) (*Snapshot, error) {
	return nil, ErrMigrationNotSupported
}

// ---- Private helpers ----

func configureBlockDevices(cfg Config) ([]vz.StorageDeviceConfiguration, error) {
	var devices []vz.StorageDeviceConfiguration
	for _, drive := range cfg.DriveList() {
		attachment, err := vz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(
			drive.Path, drive.ReadOnly,
			vz.DiskImageCachingModeCached,
			vz.DiskImageSynchronizationModeNone,
		)
		if err != nil {
			return nil, fmt.Errorf("create disk attachment for %s: %w", drive.ID, err)
		}
		blkConfig, err := vz.NewVirtioBlockDeviceConfiguration(attachment)
		if err != nil {
			return nil, fmt.Errorf("create block device config for %s: %w", drive.ID, err)
		}
		devices = append(devices, blkConfig)
	}
	return devices, nil
}

func configureNetworkDevices(cfg Config) ([]*vz.VirtioNetworkDeviceConfiguration, error) {
	mode, err := networkAttachmentMode(cfg.Network)
	if err != nil {
		return nil, err
	}

	var (
		attachment vz.NetworkDeviceAttachment
		macSeed    = "nat"
	)
	switch mode {
	case NetworkAttachmentNAT:
		natAttachment, err := vz.NewNATNetworkDeviceAttachment()
		if err != nil {
			return nil, fmt.Errorf("create NAT attachment: %w", err)
		}
		attachment = natAttachment
	case NetworkAttachmentStack:
		networkID := strings.TrimSpace(cfg.Network.NetworkID)
		network, ok := stacknet.LookupVmnetNetwork(networkID)
		if !ok {
			return nil, fmt.Errorf("darwin stack network %q is not available in this process; start it through compose or serve", networkID)
		}
		vmnetAttachment, err := vz.NewVmnetNetworkDeviceAttachment(network)
		if err != nil {
			return nil, fmt.Errorf("create shared vmnet attachment %q: %w", networkID, err)
		}
		attachment = vmnetAttachment
		macSeed = "stack:" + networkID
	default:
		return nil, fmt.Errorf("unsupported darwin network attachment mode %q", mode)
	}

	netConfig, err := vz.NewVirtioNetworkDeviceConfiguration(attachment)
	if err != nil {
		return nil, fmt.Errorf("create network config: %w", err)
	}
	if cfg.MACAddr != nil {
		mac, err := vz.NewMACAddress(cfg.MACAddr)
		if err != nil {
			return nil, fmt.Errorf("set MAC address: %w", err)
		}
		netConfig.SetMACAddress(mac)
	} else {
		mac := defaultGuestMAC(cfg.ID, macSeed)
		macAddr, err := vz.NewMACAddress(mac)
		if err != nil {
			return nil, fmt.Errorf("set default MAC address: %w", err)
		}
		netConfig.SetMACAddress(macAddr)
	}
	return []*vz.VirtioNetworkDeviceConfiguration{netConfig}, nil
}

func networkAttachmentMode(cfg *NetworkConfig) (NetworkAttachmentMode, error) {
	if cfg == nil {
		return NetworkAttachmentNAT, nil
	}
	mode := cfg.Mode
	if mode == "" {
		if strings.TrimSpace(cfg.NetworkID) == "" {
			return NetworkAttachmentNAT, nil
		}
		mode = NetworkAttachmentStack
	}
	switch mode {
	case NetworkAttachmentNAT:
		if strings.TrimSpace(cfg.NetworkID) != "" {
			return "", fmt.Errorf("network_id is only supported with darwin stack networking")
		}
		return NetworkAttachmentNAT, nil
	case NetworkAttachmentStack:
		if strings.TrimSpace(cfg.NetworkID) == "" {
			return "", fmt.Errorf("darwin stack networking requires network_id")
		}
		return NetworkAttachmentStack, nil
	default:
		return "", fmt.Errorf("unsupported darwin network attachment mode %q", mode)
	}
}

func configureSharedDirectories(cfg Config) ([]vz.DirectorySharingDeviceConfiguration, error) {
	var devices []vz.DirectorySharingDeviceConfiguration
	for _, fsCfg := range cfg.SharedFS {
		fsConfig, err := vz.NewVirtioFileSystemDeviceConfiguration(fsCfg.Tag)
		if err != nil {
			return nil, fmt.Errorf("create fs config for %s: %w", fsCfg.Tag, err)
		}
		share, err := vz.NewSharedDirectory(fsCfg.Source, false)
		if err != nil {
			return nil, fmt.Errorf("create shared directory %s: %w", fsCfg.Source, err)
		}
		singleShare, err := vz.NewSingleDirectoryShare(share)
		if err != nil {
			return nil, fmt.Errorf("create single directory share for %s: %w", fsCfg.Tag, err)
		}
		fsConfig.SetDirectoryShare(singleShare)
		devices = append(devices, fsConfig)
	}
	return devices, nil
}

func (m *VM) watchStateChanges() {
	ch := m.vzVM.StateChangedNotify()
	for vzState := range ch {
		m.mu.Lock()
		switch vzState {
		case vz.VirtualMachineStateStopped, vz.VirtualMachineStateError:
			if m.state != StateStopped {
				m.state = StateStopped
				select {
				case <-m.stopCh:
				default:
					close(m.stopCh)
				}
			}
			m.mu.Unlock()
			m.finishStop()
			return
		case vz.VirtualMachineStatePaused:
			m.state = StatePaused
		case vz.VirtualMachineStateRunning:
			m.state = StateRunning
		}
		m.mu.Unlock()
	}
	// Channel closed means VM is done.
	m.finishStop()
}

func (m *VM) finishStop() {
	m.stopOnce.Do(func() {
		m.cleanup()
		close(m.doneCh)
		m.events.Emit(EventStopped, "VM stopped")
	})
}

func (m *VM) cleanup() {
	m.cleanupOnce.Do(func() {
		if m.execBroker != nil {
			m.execBroker.close()
		}
		if m.hostReadPipe != nil {
			m.hostReadPipe.Close()
		}
		if m.hostWritePipe != nil {
			m.hostWritePipe.Close()
		}
		if m.guestReadEnd != nil {
			m.guestReadEnd.Close()
		}
		if m.guestWriteEnd != nil {
			m.guestWriteEnd.Close()
		}
	})
}

// runVsockExecListener listens on the exec agent vsock port and bridges
// guest-initiated connections to the exec agent broker, matching the KVM
// backend's behavior where the custom vsock device calls the broker's listen
// callback when the guest connects.
func (m *VM) runVsockExecListener() {
	devices := m.vzVM.SocketDevices()
	if len(devices) == 0 || m.execBroker == nil {
		return
	}
	port := m.cfg.Exec.VsockPort
	if port == 0 {
		port = 512
	}
	listener, err := devices[0].Listen(port)
	if err != nil {
		m.events.Emit(EventError, fmt.Sprintf("vsock listen port %d: %v", port, err))
		return
	}
	go func() {
		<-m.stopCh
		listener.Close()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return // listener closed
		}
		// The exec broker expects: host calls acquire() → blocks until a
		// guest connection arrives. Wire the vz connection to the broker by
		// handing it through the broker's conn channel.
		hostConn, guestConn := net.Pipe()
		go func() {
			// Bridge: vz vsock conn ↔ guestConn side of net.Pipe
			go func() { _, _ = io.Copy(conn, guestConn); conn.Close() }()
			_, _ = io.Copy(guestConn, conn)
			guestConn.Close()
		}()
		select {
		case m.execBroker.conns <- hostConn:
		case <-m.stopCh:
			hostConn.Close()
			guestConn.Close()
			conn.Close()
			return
		}
	}
}

func (m *VM) pumpConsoleInput() {
	_, _ = io.Copy(m.hostWritePipe, m.cfg.ConsoleIn)
}

func (m *VM) bufferConsoleOutput() {
	buf := make([]byte, 4096)
	for {
		n, err := m.hostReadPipe.Read(buf)
		if n > 0 {
			m.consoleMu.Lock()
			m.consoleBuf = append(m.consoleBuf, buf[:n]...)
			// Forward to ConsoleOut if configured
			if m.cfg.ConsoleOut != nil {
				_, _ = m.cfg.ConsoleOut.Write(buf[:n])
			}
			m.writeConsoleAttachmentsLocked(buf[:n])
			m.consoleMu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// Compile-time interface assertions.
var (
	_ Handle                  = (*VM)(nil)
	_ ConsoleDialer           = (*VM)(nil)
	_ VsockDialer             = (*VM)(nil)
	_ BalloonController       = (*VM)(nil)
	_ MemoryHotplugController = (*VM)(nil)
)

func (m *VM) writeConsoleAttachmentsLocked(data []byte) {
	if len(m.consoleAttachments) == 0 || len(data) == 0 {
		return
	}
	var stale []uint64
	for id, attachment := range m.consoleAttachments {
		if _, err := attachment.guestWrite.Write(data); err != nil {
			stale = append(stale, id)
		}
	}
	for _, id := range stale {
		if attachment := m.consoleAttachments[id]; attachment != nil {
			delete(m.consoleAttachments, id)
			go attachment.closeOwned()
		}
	}
}

func (m *VM) detachConsoleAttachment(id uint64) {
	m.consoleMu.Lock()
	attachment := m.consoleAttachments[id]
	delete(m.consoleAttachments, id)
	m.consoleMu.Unlock()
	if attachment != nil {
		attachment.closeOwned()
	}
}

func (a *darwinConsoleAttachment) Read(p []byte) (int, error) {
	return a.hostRead.Read(p)
}

func (a *darwinConsoleAttachment) Write(p []byte) (int, error) {
	return a.hostWrite.Write(p)
}

func (a *darwinConsoleAttachment) Close() error {
	if a == nil || a.vm == nil {
		return nil
	}
	a.vm.detachConsoleAttachment(a.id)
	return nil
}

func (a *darwinConsoleAttachment) closeOwned() {
	if a == nil {
		return
	}
	a.closeOnce.Do(func() {
		if a.hostRead != nil {
			_ = a.hostRead.Close()
		}
		if a.hostWrite != nil {
			_ = a.hostWrite.Close()
		}
		if a.guestRead != nil {
			_ = a.guestRead.Close()
		}
		if a.guestWrite != nil {
			_ = a.guestWrite.Close()
		}
	})
}

func (a *darwinConsoleAttachment) pumpInput() {
	buf := make([]byte, 4096)
	for {
		n, err := a.guestRead.Read(buf)
		if n > 0 {
			_, _ = a.vm.hostWritePipe.Write(buf[:n])
		}
		if err != nil {
			a.vm.detachConsoleAttachment(a.id)
			return
		}
	}
}
