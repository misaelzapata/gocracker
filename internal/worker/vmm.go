package worker

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gocracker/gocracker/internal/sharedfs"
	"github.com/gocracker/gocracker/internal/vmmserver"
	"github.com/gocracker/gocracker/pkg/vmm"
)

const (
	pollInterval   = 250 * time.Millisecond
	socketWaitStep = 50 * time.Millisecond
	socketWaitMax  = 10 * time.Second
)

type subprocessLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *subprocessLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *subprocessLogBuffer) Tail(maxLines int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buf.Len() == 0 {
		return ""
	}
	lines := strings.Split(strings.ReplaceAll(b.buf.String(), "\r\n", "\n"), "\n")
	filtered := lines[:0]
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		filtered = append(filtered, line)
	}
	if len(filtered) == 0 {
		return ""
	}
	if maxLines > 0 && len(filtered) > maxLines {
		filtered = filtered[len(filtered)-maxLines:]
	}
	return strings.Join(filtered, "\n")
}

func wrapSubprocessError(err error, logs *subprocessLogBuffer) error {
	if err == nil || logs == nil {
		return err
	}
	if tail := logs.Tail(40); tail != "" {
		return fmt.Errorf("%w\nworker log tail:\n%s", err, tail)
	}
	return err
}

type VMMOptions struct {
	JailerBinary string
	VMMBinary    string
	UID          int
	GID          int
	ChrootBase   string
	NetNS        string
}

type ReattachOptions struct {
	Config    vmm.Config
	Metadata  vmm.WorkerMetadata
	Cleanup   func()
	StartedAt time.Time
}

type remoteVM struct {
	client   *vmmserver.Client
	cfg      vmm.Config
	events   *vmm.EventLog
	doneCh   chan struct{}
	stopOnce sync.Once
	doneOnce sync.Once
	cleanup  func()
	runDir   string
	socket   string
	pid      int
	jailRoot string
	created  time.Time

	mu      sync.RWMutex
	state   vmm.State
	uptime  time.Duration
	devices []vmm.DeviceInfo
	logs    []byte
}

func LaunchVMM(cfg vmm.Config, opts VMMOptions) (vmm.Handle, func(), error) {
	if opts.ChrootBase == "" {
		opts.ChrootBase = DefaultChrootBaseDir()
	}
	if err := os.MkdirAll(opts.ChrootBase, 0755); err != nil {
		return nil, nil, fmt.Errorf("create chroot base %s: %w", opts.ChrootBase, err)
	}
	jailerExec, jailerPrefix, err := resolveLauncher(opts.JailerBinary, "jailer")
	if err != nil {
		return nil, nil, err
	}
	vmmExec, vmmArgsPrefix, err := resolveLauncher(opts.VMMBinary, "vmm")
	if err != nil {
		return nil, nil, err
	}
	runDir, err := os.MkdirTemp("", "gocracker-vmm-worker-*")
	if err != nil {
		return nil, nil, err
	}
	socketHostPath := filepath.Join(runDir, "vmm.sock")

	jailedCfg := cfg
	mounts := []string{
		"rw:" + runDir + ":/worker",
	}
	// Use a unique temp dir per VM session to prevent stale bind mount
	// interference between sessions. Previous approach reused a shared
	// base dir which caused ENOENT when mounts leaked from crashed VMs.
	jailDir, err := os.MkdirTemp(opts.ChrootBase, jailedCfg.ID+"-")
	if err != nil {
		_ = os.RemoveAll(runDir)
		return nil, nil, fmt.Errorf("create jail dir: %w", err)
	}
	jailRoot := filepath.Join(jailDir, "root")
	jailedCfg.KernelPath = "/worker/kernel"
	mounts = append(mounts, "ro:"+cfg.KernelPath+":"+jailedCfg.KernelPath)
	if cfg.InitrdPath != "" {
		jailedCfg.InitrdPath = "/worker/initrd"
		mounts = append(mounts, "ro:"+cfg.InitrdPath+":"+jailedCfg.InitrdPath)
	}
	originalDrives := cfg.DriveList()
	if len(originalDrives) > 0 {
		jailedCfg.Drives = make([]vmm.DriveConfig, 0, len(originalDrives))
		for i, drive := range originalDrives {
			target := fmt.Sprintf("/worker/drives/%d", i)
			mode := "rw"
			if drive.ReadOnly {
				mode = "ro"
			}
			mounts = append(mounts, mode+":"+drive.Path+":"+target)
			jailedDrive := vmm.DriveConfig{
				ID:          drive.ID,
				Path:        target,
				Root:        drive.Root,
				ReadOnly:    drive.ReadOnly,
				RateLimiter: cloneRateLimiter(drive.RateLimiter),
			}
			if drive.Root {
				jailedCfg.DiskImage = target
				jailedCfg.DiskRO = drive.ReadOnly
				jailedCfg.BlockRateLimiter = cloneRateLimiter(drive.RateLimiter)
			}
			jailedCfg.Drives = append(jailedCfg.Drives, jailedDrive)
		}
	}
	// virtiofsd spawned on host, since the binary is not visible inside the
	// jailer chroot. Each backend's UNIX socket lives under runDir (already
	// bind-mounted into the jail at /worker) so the jailed VMM can connect to
	// it via /worker/virtiofsd-N.sock.
	var virtiofsdBackends []*sharedfs.Backend
	if len(cfg.SharedFS) > 0 {
		shared := make([]vmm.SharedFSConfig, 0, len(cfg.SharedFS))
		for i, fs := range cfg.SharedFS {
			target := fmt.Sprintf("/worker/sharedfs/%d", i)
			mounts = append(mounts, "rw:"+fs.Source+":"+target)
			backend, err := sharedfs.StartAt(fs.Source, fs.Tag, filepath.Join(runDir, fmt.Sprintf("virtiofsd-%d.sock", i)))
			if err != nil {
				for _, b := range virtiofsdBackends {
					_ = b.Close()
				}
				_ = os.RemoveAll(runDir)
				return nil, nil, fmt.Errorf("start host virtiofsd for tag %s: %w", fs.Tag, err)
			}
			virtiofsdBackends = append(virtiofsdBackends, backend)
			shared = append(shared, vmm.SharedFSConfig{
				Source:     target,
				Tag:        fs.Tag,
				SocketPath: fmt.Sprintf("/worker/virtiofsd-%d.sock", i),
			})
		}
		jailedCfg.SharedFS = shared
	}

	jailerArgs := []string{
		"--id", jailedCfg.ID,
		"--uid", fmt.Sprintf("%d", firstNonNegative(opts.UID, os.Getuid())),
		"--gid", fmt.Sprintf("%d", firstNonNegative(opts.GID, os.Getgid())),
		"--exec-file", vmmExec,
	}
	if opts.ChrootBase != "" {
		jailerArgs = append(jailerArgs, "--chroot-base-dir", opts.ChrootBase)
	}
	if opts.NetNS != "" {
		jailerArgs = append(jailerArgs, "--netns", opts.NetNS)
	}
	jailerArgs = appendForwardedWorkerEnvFlags(jailerArgs)
	for _, mount := range mounts {
		jailerArgs = append(jailerArgs, "--mount", mount)
	}
	jailerArgs = append(jailerArgs, "--")
	jailerArgs = append(jailerArgs, vmmArgsPrefix...)
	jailerArgs = append(jailerArgs, "--vm-id", jailedCfg.ID)
	jailerArgs = append(jailerArgs, "--socket", "/worker/vmm.sock")

	cmd := exec.Command(jailerExec, append(jailerPrefix, jailerArgs...)...)
	logBuf := &subprocessLogBuffer{}
	cmd.Stdout = logBuf
	cmd.Stderr = logBuf
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		for _, b := range virtiofsdBackends {
			_ = b.Close()
		}
		_ = os.RemoveAll(runDir)
		return nil, nil, fmt.Errorf("start jailer: %w", err)
	}

	cleanup := func() {
		for _, b := range virtiofsdBackends {
			_ = b.Close()
		}
		_ = os.RemoveAll(runDir)
	}
	waitErrCh := make(chan error, 1)
	go func() { waitErrCh <- cmd.Wait() }()

	if exited, err := waitForSocketOrExit(socketHostPath, socketWaitMax, waitErrCh); err != nil {
		if !exited {
			_ = cmd.Process.Kill()
			<-waitErrCh
		}
		cleanup()
		return nil, nil, wrapSubprocessError(err, logBuf)
	}

	client := vmmserver.NewClient(socketHostPath)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.SetBootSource(ctx, vmmserver.BootSource{
		KernelImagePath: jailedCfg.KernelPath,
		BootArgs:        jailedCfg.Cmdline,
		InitrdPath:      jailedCfg.InitrdPath,
		X86Boot:         string(jailedCfg.X86Boot),
	}); err != nil {
		_ = cmd.Process.Kill()
		cleanup()
		return nil, nil, wrapSubprocessError(err, logBuf)
	}
	if err := client.SetMachineConfig(ctx, vmmserver.MachineConfig{
		VcpuCount:      jailedCfg.VCPUs,
		MemSizeMib:     int(jailedCfg.MemMB),
		RNGRateLimiter: jailedCfg.RNGRateLimiter,
		VsockEnabled:   jailedCfg.Vsock != nil && jailedCfg.Vsock.Enabled,
		VsockGuestCID:  vsockGuestCID(jailedCfg.Vsock),
		ExecEnabled:    jailedCfg.Exec != nil && jailedCfg.Exec.Enabled,
		ExecVsockPort:  execVsockPort(jailedCfg.Exec),
	}); err != nil {
		_ = cmd.Process.Kill()
		cleanup()
		return nil, nil, wrapSubprocessError(err, logBuf)
	}
	if jailedCfg.Balloon != nil {
		if err := client.SetBalloon(ctx, vmmserver.Balloon{
			AmountMib:             jailedCfg.Balloon.AmountMiB,
			DeflateOnOOM:          jailedCfg.Balloon.DeflateOnOOM,
			StatsPollingIntervalS: jailedCfg.Balloon.StatsPollingIntervalS,
			FreePageHinting:       jailedCfg.Balloon.FreePageHinting,
			FreePageReporting:     jailedCfg.Balloon.FreePageReporting,
		}); err != nil {
			_ = cmd.Process.Kill()
			cleanup()
			return nil, nil, wrapSubprocessError(err, logBuf)
		}
	}
	if jailedCfg.MemoryHotplug != nil {
		if err := client.SetMemoryHotplug(ctx, vmmserver.MemoryHotplugConfig{
			TotalSizeMiB: jailedCfg.MemoryHotplug.TotalSizeMiB,
			SlotSizeMiB:  jailedCfg.MemoryHotplug.SlotSizeMiB,
			BlockSizeMiB: jailedCfg.MemoryHotplug.BlockSizeMiB,
		}); err != nil {
			_ = cmd.Process.Kill()
			cleanup()
			return nil, nil, wrapSubprocessError(err, logBuf)
		}
	}
	for _, drive := range jailedCfg.DriveList() {
		if err := client.SetDrive(ctx, drive.ID, vmmserver.Drive{
			DriveID:      drive.ID,
			PathOnHost:   drive.Path,
			IsRootDevice: drive.Root,
			IsReadOnly:   drive.ReadOnly,
			RateLimiter:  cloneRateLimiter(drive.RateLimiter),
		}); err != nil {
			_ = cmd.Process.Kill()
			cleanup()
			return nil, nil, wrapSubprocessError(err, logBuf)
		}
	}
	if jailedCfg.TapName != "" {
		iface := vmmserver.NetworkInterface{
			IfaceID:     "eth0",
			HostDevName: jailedCfg.TapName,
			RateLimiter: jailedCfg.NetRateLimiter,
		}
		if len(jailedCfg.MACAddr) > 0 {
			iface.GuestMAC = jailedCfg.MACAddr.String()
		}
		if err := client.SetNetworkInterface(ctx, "eth0", iface); err != nil {
			_ = cmd.Process.Kill()
			cleanup()
			return nil, nil, wrapSubprocessError(err, logBuf)
		}
	}
	for _, fs := range jailedCfg.SharedFS {
		if err := client.SetSharedFS(ctx, fs.Tag, vmmserver.SharedFS{
			Tag:        fs.Tag,
			Source:     fs.Source,
			SocketPath: fs.SocketPath,
		}); err != nil {
			_ = cmd.Process.Kill()
			cleanup()
			return nil, nil, wrapSubprocessError(err, logBuf)
		}
	}
	if err := client.Start(ctx); err != nil {
		_ = cmd.Process.Kill()
		cleanup()
		return nil, nil, wrapSubprocessError(err, logBuf)
	}

	rvm := &remoteVM{
		client:   client,
		cfg:      cfg,
		events:   vmm.NewEventLog(),
		doneCh:   make(chan struct{}),
		cleanup:  cleanup,
		runDir:   runDir,
		socket:   socketHostPath,
		pid:      cmd.Process.Pid,
		jailRoot: jailRoot,
		created:  time.Now(),
		state:    vmm.StateCreated,
	}
	go rvm.poll()
	go func() {
		if err := <-waitErrCh; err != nil {
			rvm.events.Emit(vmm.EventError, err.Error())
		}
		rvm.finish()
	}()
	return rvm, cleanup, nil
}

func LaunchRestoredVMM(snapshotDir string, opts vmm.RestoreOptions, workerOpts VMMOptions) (vmm.Handle, func(), error) {
	return LaunchRestoredVMMWithResume(snapshotDir, opts, true, workerOpts)
}

func LaunchRestoredVMMWithResume(snapshotDir string, opts vmm.RestoreOptions, resume bool, workerOpts VMMOptions) (vmm.Handle, func(), error) {
	if workerOpts.ChrootBase == "" {
		workerOpts.ChrootBase = DefaultChrootBaseDir()
	}
	if err := os.MkdirAll(workerOpts.ChrootBase, 0755); err != nil {
		return nil, nil, fmt.Errorf("create chroot base %s: %w", workerOpts.ChrootBase, err)
	}
	jailerExec, jailerPrefix, err := resolveLauncher(workerOpts.JailerBinary, "jailer")
	if err != nil {
		return nil, nil, err
	}
	vmmExec, vmmArgsPrefix, err := resolveLauncher(workerOpts.VMMBinary, "vmm")
	if err != nil {
		return nil, nil, err
	}
	runDir, err := os.MkdirTemp("", "gocracker-vmm-restore-*")
	if err != nil {
		return nil, nil, err
	}
	socketHostPath := filepath.Join(runDir, "vmm.sock")
	jailerArgs := []string{
		"--id", filepath.Base(snapshotDir),
		"--uid", fmt.Sprintf("%d", firstNonNegative(workerOpts.UID, os.Getuid())),
		"--gid", fmt.Sprintf("%d", firstNonNegative(workerOpts.GID, os.Getgid())),
		"--exec-file", vmmExec,
		"--mount", "rw:" + runDir + ":/worker",
		"--mount", "rw:" + snapshotDir + ":/snapshot",
		"--",
	}
	if workerOpts.ChrootBase != "" {
		jailerArgs = append(jailerArgs[:6], append([]string{"--chroot-base-dir", workerOpts.ChrootBase}, jailerArgs[6:]...)...)
	}
	if workerOpts.NetNS != "" {
		jailerArgs = append(jailerArgs[:6], append([]string{"--netns", workerOpts.NetNS}, jailerArgs[6:]...)...)
	}
	jailerArgs = insertForwardedWorkerEnvFlags(jailerArgs, 6)
	jailerArgs = append(jailerArgs, vmmArgsPrefix...)
	if id := strings.TrimSpace(opts.OverrideID); id != "" {
		jailerArgs = append(jailerArgs, "--vm-id", id)
	}
	jailerArgs = append(jailerArgs, "--socket", "/worker/vmm.sock")
	cmd := exec.Command(jailerExec, append(jailerPrefix, jailerArgs...)...)
	logBuf := &subprocessLogBuffer{}
	cmd.Stdout = logBuf
	cmd.Stderr = logBuf
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(runDir)
		return nil, nil, fmt.Errorf("start jailer: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(runDir) }
	waitErrCh := make(chan error, 1)
	go func() { waitErrCh <- cmd.Wait() }()
	if exited, err := waitForSocketOrExit(socketHostPath, socketWaitMax, waitErrCh); err != nil {
		if !exited {
			_ = cmd.Process.Kill()
			<-waitErrCh
		}
		cleanup()
		return nil, nil, wrapSubprocessError(err, logBuf)
	}
	client := vmmserver.NewClient(socketHostPath)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	info, err := client.Restore(ctx, vmmserver.RestoreRequest{
		SnapshotDir: "/snapshot",
		TapName:     opts.OverrideTap,
		VcpuCount:   opts.OverrideVCPUs,
		X86Boot:     string(opts.OverrideX86Boot),
		Resume:      resume,
	})
	if err != nil {
		_ = cmd.Process.Kill()
		cleanup()
		return nil, nil, wrapSubprocessError(err, logBuf)
	}
	rvm := &remoteVM{
		client:   client,
		cfg:      vmm.Config{ID: info.ID, TapName: opts.OverrideTap},
		events:   vmm.NewEventLog(),
		doneCh:   make(chan struct{}),
		cleanup:  cleanup,
		runDir:   runDir,
		socket:   socketHostPath,
		pid:      cmd.Process.Pid,
		jailRoot: filepath.Join(workerOpts.ChrootBase, filepath.Base(vmmExec), filepath.Base(snapshotDir), "root"),
		created:  time.Now(),
		state:    parseState(info.State),
	}
	go rvm.poll()
	go func() {
		if err := <-waitErrCh; err != nil {
			rvm.events.Emit(vmm.EventError, err.Error())
		}
		rvm.finish()
	}()
	return rvm, cleanup, nil
}

func ReattachVMM(opts ReattachOptions) (vmm.Handle, func(), error) {
	if opts.Metadata.SocketPath == "" {
		return nil, nil, fmt.Errorf("reattach worker: socket path is required")
	}
	if err := waitForSocket(opts.Metadata.SocketPath, 2*time.Second); err != nil {
		return nil, nil, err
	}
	client := vmmserver.NewClient(opts.Metadata.SocketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	info, err := client.GetInfo(ctx)
	cancel()
	if err != nil {
		return nil, nil, err
	}

	cleanup := opts.Cleanup
	if cleanup == nil && opts.Metadata.RunDir != "" {
		runDir := opts.Metadata.RunDir
		cleanup = func() { _ = os.RemoveAll(runDir) }
	}
	created := opts.Metadata.CreatedAt
	if created.IsZero() {
		created = opts.StartedAt
	}
	rvm := &remoteVM{
		client:   client,
		cfg:      opts.Config,
		events:   vmm.NewEventLog(),
		doneCh:   make(chan struct{}),
		cleanup:  cleanup,
		runDir:   opts.Metadata.RunDir,
		socket:   opts.Metadata.SocketPath,
		pid:      opts.Metadata.WorkerPID,
		jailRoot: opts.Metadata.JailRoot,
		created:  created,
		state:    parseState(info.State),
	}
	if info.ID != "" {
		rvm.cfg.ID = info.ID
	}
	if info.MemMB > 0 {
		rvm.cfg.MemMB = info.MemMB
	}
	if info.Kernel != "" {
		rvm.cfg.KernelPath = info.Kernel
	}
	if up, err := time.ParseDuration(info.Uptime); err == nil {
		rvm.uptime = up
	}
	rvm.devices = append(rvm.devices, info.Devices...)
	go rvm.poll()
	if rvm.state == vmm.StateStopped {
		rvm.finish()
	}
	return rvm, cleanup, nil
}

func (r *remoteVM) Start() error { return nil }

func (r *remoteVM) Stop() {
	r.stopOnce.Do(func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := r.client.Stop(ctx)
			cancel()
			if err != nil {
				r.events.Emit(vmm.EventError, fmt.Sprintf("worker stop rpc: %v", err))
			}

			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				if r.State() == vmm.StateStopped {
					return
				}
				time.Sleep(100 * time.Millisecond)
			}

			if r.pid > 0 && workerProcessAlive(r.pid) {
				_ = syscall.Kill(r.pid, syscall.SIGKILL)
			}
		}()
	})
}

func (r *remoteVM) TakeSnapshot(dir string) (*vmm.Snapshot, error) {
	if r.runDir != "" {
		return r.takeSnapshotViaExport(dir)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return r.client.Snapshot(ctx, vmmserver.SnapshotRequest{DestDir: dir})
}

func (r *remoteVM) takeSnapshotViaExport(dir string) (*vmm.Snapshot, error) {
	exportDir := filepath.Join(r.runDir, "snapshot-export")
	if err := os.RemoveAll(exportDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(r.runDir, 0755); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := r.client.Snapshot(ctx, vmmserver.SnapshotRequest{DestDir: "/worker/snapshot-export"}); err != nil {
		return nil, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := copyTree(exportDir, dir); err != nil {
		return nil, err
	}
	return vmm.RewriteSnapshotBundleWithConfig(dir, r.cfg)
}

func (r *remoteVM) State() vmm.State {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.state
}

func (r *remoteVM) ID() string { return r.cfg.ID }

func (r *remoteVM) Uptime() time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.uptime
}

func (r *remoteVM) Events() vmm.EventSource { return r.events }

func (r *remoteVM) VMConfig() vmm.Config { return r.cfg }

func (r *remoteVM) WorkerMetadata() vmm.WorkerMetadata {
	return vmm.WorkerMetadata{
		Kind:       "worker",
		SocketPath: r.socket,
		WorkerPID:  r.pid,
		JailRoot:   r.jailRoot,
		RunDir:     r.runDir,
		CreatedAt:  r.created,
	}
}

func (r *remoteVM) DeviceList() []vmm.DeviceInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]vmm.DeviceInfo, len(r.devices))
	copy(out, r.devices)
	return out
}

func (r *remoteVM) ConsoleOutput() []byte {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	logs, err := r.client.GetLogs(ctx)
	if err == nil {
		r.mu.Lock()
		r.logs = append(r.logs[:0], logs...)
		r.mu.Unlock()
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]byte, len(r.logs))
	copy(out, r.logs)
	return out
}

func (r *remoteVM) DialVsock(port uint32) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return r.client.DialVsock(ctx, port)
}

func (r *remoteVM) WaitStopped(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.doneCh:
		return nil
	}
}

func (r *remoteVM) UpdateNetRateLimiter(cfg *vmm.RateLimiterConfig) error {
	if cfg == nil {
		cfg = &vmm.RateLimiterConfig{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := r.client.SetNetRateLimiter(ctx, *cfg); err != nil {
		return err
	}
	r.mu.Lock()
	r.cfg.NetRateLimiter = cloneVMLimiter(cfg)
	r.mu.Unlock()
	return nil
}

func (r *remoteVM) UpdateBlockRateLimiter(cfg *vmm.RateLimiterConfig) error {
	if cfg == nil {
		cfg = &vmm.RateLimiterConfig{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := r.client.SetBlockRateLimiter(ctx, *cfg); err != nil {
		return err
	}
	r.mu.Lock()
	r.cfg.BlockRateLimiter = cloneVMLimiter(cfg)
	r.mu.Unlock()
	return nil
}

func (r *remoteVM) UpdateRNGRateLimiter(cfg *vmm.RateLimiterConfig) error {
	if cfg == nil {
		cfg = &vmm.RateLimiterConfig{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := r.client.SetRNGRateLimiter(ctx, *cfg); err != nil {
		return err
	}
	r.mu.Lock()
	r.cfg.RNGRateLimiter = cloneVMLimiter(cfg)
	r.mu.Unlock()
	return nil
}

func (r *remoteVM) GetBalloonConfig() (vmm.BalloonConfig, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cfg, err := r.client.GetBalloon(ctx)
	if err != nil {
		return vmm.BalloonConfig{}, err
	}
	return vmm.BalloonConfig{
		AmountMiB:             cfg.AmountMib,
		DeflateOnOOM:          cfg.DeflateOnOOM,
		StatsPollingIntervalS: cfg.StatsPollingIntervalS,
	}, nil
}

func (r *remoteVM) UpdateBalloon(update vmm.BalloonUpdate) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := r.client.PatchBalloon(ctx, vmmserver.BalloonUpdate{AmountMib: update.AmountMiB}); err != nil {
		return err
	}
	r.mu.Lock()
	if r.cfg.Balloon == nil {
		r.cfg.Balloon = &vmm.BalloonConfig{}
	}
	r.cfg.Balloon.AmountMiB = update.AmountMiB
	r.mu.Unlock()
	return nil
}

func (r *remoteVM) GetBalloonStats() (vmm.BalloonStats, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return r.client.GetBalloonStats(ctx)
}

func (r *remoteVM) UpdateBalloonStats(update vmm.BalloonStatsUpdate) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := r.client.PatchBalloonStats(ctx, vmmserver.BalloonStatsUpdate{
		StatsPollingIntervalS: update.StatsPollingIntervalS,
	}); err != nil {
		return err
	}
	r.mu.Lock()
	if r.cfg.Balloon == nil {
		r.cfg.Balloon = &vmm.BalloonConfig{}
	}
	r.cfg.Balloon.StatsPollingIntervalS = update.StatsPollingIntervalS
	r.mu.Unlock()
	return nil
}

func (r *remoteVM) GetMemoryHotplug() (vmm.MemoryHotplugStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return r.client.GetMemoryHotplug(ctx)
}

func (r *remoteVM) UpdateMemoryHotplug(update vmm.MemoryHotplugSizeUpdate) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := r.client.PatchMemoryHotplug(ctx, vmmserver.MemoryHotplugSizeUpdate{
		RequestedSizeMiB: update.RequestedSizeMiB,
	}); err != nil {
		return err
	}
	r.mu.Lock()
	if r.cfg.MemoryHotplug == nil {
		r.cfg.MemoryHotplug = &vmm.MemoryHotplugConfig{}
	}
	r.mu.Unlock()
	return nil
}

func (r *remoteVM) PrepareMigrationBundle(dir string) error {
	if r.runDir != "" {
		exportDir := filepath.Join(r.runDir, "migration-prepare")
		if err := os.RemoveAll(exportDir); err != nil {
			return err
		}
		if err := os.MkdirAll(r.runDir, 0755); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := r.client.PrepareMigrationBundle(ctx, vmmserver.SnapshotRequest{DestDir: "/worker/migration-prepare"}); err != nil {
			return err
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		return copyTree(exportDir, dir)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return r.client.PrepareMigrationBundle(ctx, vmmserver.SnapshotRequest{DestDir: dir})
}

func (r *remoteVM) FinalizeMigrationBundle(dir string) (*vmm.Snapshot, *vmm.MigrationPatchSet, error) {
	if r.runDir != "" {
		exportDir := filepath.Join(r.runDir, "migration-finalize")
		if err := os.RemoveAll(exportDir); err != nil {
			return nil, nil, err
		}
		if err := os.MkdirAll(r.runDir, 0755); err != nil {
			return nil, nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		snap, patches, err := r.client.FinalizeMigrationBundle(ctx, vmmserver.SnapshotRequest{DestDir: "/worker/migration-finalize"})
		if err != nil {
			return nil, nil, err
		}
		if err := os.RemoveAll(dir); err != nil {
			return nil, nil, err
		}
		if err := copyTree(exportDir, dir); err != nil {
			return nil, nil, err
		}
		return snap, patches, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return r.client.FinalizeMigrationBundle(ctx, vmmserver.SnapshotRequest{DestDir: dir})
}

func (r *remoteVM) ResetMigrationTracking() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return r.client.ResetMigrationTracking(ctx)
}

func (r *remoteVM) poll() {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	var since time.Time
	for {
		select {
		case <-r.doneCh:
			return
		case <-ticker.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		info, err := r.client.GetInfo(ctx)
		if err == nil {
			state := parseState(info.State)
			up, _ := time.ParseDuration(info.Uptime)
			r.mu.Lock()
			if info.ID != "" {
				r.cfg.ID = info.ID
			}
			if info.MemMB > 0 {
				r.cfg.MemMB = info.MemMB
			}
			if info.Kernel != "" {
				r.cfg.KernelPath = info.Kernel
			}
			r.state = state
			r.uptime = up
			r.devices = append(r.devices[:0], info.Devices...)
			r.mu.Unlock()
			if state == vmm.StateStopped {
				cancel()
				r.finish()
				return
			}
		} else if !workerProcessAlive(r.pid) || !socketReachable(r.socket) {
			cancel()
			r.finish()
			return
		}
		events, evErr := r.client.GetEvents(ctx, since)
		cancel()
		if evErr == nil {
			for _, ev := range events {
				r.events.Emit(ev.Type, ev.Message)
				if ev.Time.After(since) {
					since = ev.Time
				}
			}
		}
	}
}

func (r *remoteVM) finish() {
	r.doneOnce.Do(func() {
		r.mu.Lock()
		r.state = vmm.StateStopped
		r.mu.Unlock()
		close(r.doneCh)
		if r.cleanup != nil {
			r.cleanup()
		}
	})
}

func waitForSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(socketWaitStep)
	}
	return fmt.Errorf("timed out waiting for worker socket %s", path)
}

func waitForSocketOrExit(path string, timeout time.Duration, waitErrCh <-chan error) (bool, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return false, nil
		}
		select {
		case waitErr := <-waitErrCh:
			if waitErr == nil {
				return true, fmt.Errorf("worker exited before opening socket %s", path)
			}
			return true, fmt.Errorf("worker exited before opening socket %s: %w", path, waitErr)
		default:
		}
		time.Sleep(socketWaitStep)
	}
	return false, fmt.Errorf("timed out waiting for worker socket %s", path)
}

func DefaultChrootBaseDir() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("gocracker-jailer-%d", os.Getuid()))
}

func cloneVMLimiter(cfg *vmm.RateLimiterConfig) *vmm.RateLimiterConfig {
	if cfg == nil {
		return nil
	}
	cloned := *cfg
	return &cloned
}

func resolveLauncher(explicit, mode string) (string, []string, error) {
	if explicit != "" {
		return explicit, nil, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", nil, fmt.Errorf("resolve current executable: %w", err)
	}
	return exe, []string{mode}, nil
}

func firstNonNegative(values ...int) int {
	for _, v := range values {
		if v >= 0 {
			return v
		}
	}
	return 0
}

func forwardedWorkerEnv() []string {
	keys := []string{"GOCRACKER_SECCOMP"}
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			out = append(out, key+"="+value)
		}
	}
	return out
}

func appendForwardedWorkerEnvFlags(args []string) []string {
	for _, entry := range forwardedWorkerEnv() {
		args = append(args, "--env", entry)
	}
	return args
}

func insertForwardedWorkerEnvFlags(args []string, insertAt int) []string {
	for _, entry := range forwardedWorkerEnv() {
		args = append(args[:insertAt], append([]string{"--env", entry}, args[insertAt:]...)...)
		insertAt += 2
	}
	return args
}

func parseState(raw string) vmm.State {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "running":
		return vmm.StateRunning
	case "paused":
		return vmm.StatePaused
	case "stopped":
		return vmm.StateStopped
	default:
		return vmm.StateCreated
	}
}

func vsockGuestCID(cfg *vmm.VsockConfig) uint32 {
	if cfg == nil {
		return 0
	}
	return cfg.GuestCID
}

func execVsockPort(cfg *vmm.ExecConfig) uint32 {
	if cfg == nil {
		return 0
	}
	return cfg.VsockPort
}

func cloneRateLimiter(cfg *vmm.RateLimiterConfig) *vmm.RateLimiterConfig {
	if cfg == nil {
		return nil
	}
	cloned := *cfg
	return &cloned
}

func workerProcessAlive(pid int) bool {
	if pid <= 0 {
		return true
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func socketReachable(path string) bool {
	if path == "" {
		return false
	}
	conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func copyTree(srcDir, dstDir string) error {
	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dstDir, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, info.Mode())
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			return os.Symlink(link, target)
		default:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, info.Mode())
		}
	})
}
