package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/gocracker/gocracker/internal/sharedfs"
	"github.com/gocracker/gocracker/internal/vmmserver"
	"github.com/gocracker/gocracker/pkg/vmm"
)

const (
	pollInterval   = 250 * time.Millisecond
	socketWaitStep = 25 * time.Millisecond
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
	client      *vmmserver.Client
	cfg         vmm.Config
	events      *vmm.EventLog
	doneCh      chan struct{}
	stopOnce    sync.Once
	doneOnce    sync.Once
	cleanup     func()
	runDir      string
	socket      string
	pid         int
	jailRoot    string
	created     time.Time
	// hostDiskPath is the HOST-side path to the root disk image, kept so
	// takeSnapshotViaExport can hardlink it directly without going through the
	// jailer's bind-mount (which blocks hardlinks across mount points).
	hostDiskPath string

	mu            sync.RWMutex
	state         vmm.State
	started       time.Time
	uptime        time.Duration
	devices       []vmm.DeviceInfo
	logs          []byte
	firstOutputAt time.Time
}

// LaunchVMM is a thin wrapper over LaunchVMMWithTimings for callers that
// don't need the phase breakdown. New code should prefer the timed variant.
func LaunchVMM(cfg vmm.Config, opts VMMOptions) (vmm.Handle, func(), error) {
	h, _, cleanup, err := LaunchVMMWithTimings(cfg, opts)
	return h, cleanup, err
}

// LaunchVMMWithTimings starts a jailed gocracker-vmm subprocess, drives it
// through the Firecracker REST API to boot the guest, and returns a Handle
// plus a vmm.BootTimings populated with the host-side phase split:
//
//   - Orchestration = fork-exec(jailer) + chroot setup + socket wait +
//     the SET* setup RPCs (everything BEFORE InstanceStart).
//   - VMMSetup      = InstanceStart RPC round trip (which triggers the
//     remote vmm.New + vm.Start inside gocracker-vmm).
//   - Start and GuestFirstOutput are left zero here; the caller (container
//     package) fills GuestFirstOutput once the guest has printed a byte.
func LaunchVMMWithTimings(cfg vmm.Config, opts VMMOptions) (vmm.Handle, vmm.BootTimings, func(), error) {
	var timings vmm.BootTimings
	t0 := time.Now()
	if opts.ChrootBase == "" {
		opts.ChrootBase = DefaultChrootBaseDir()
	}
	if err := os.MkdirAll(opts.ChrootBase, 0755); err != nil {
		return nil, timings, nil, fmt.Errorf("create chroot base %s: %w", opts.ChrootBase, err)
	}
	jailerExec, jailerPrefix, err := resolveLauncher(opts.JailerBinary, "jailer")
	if err != nil {
		return nil, timings, nil, err
	}
	vmmExec, vmmArgsPrefix, err := resolveLauncher(opts.VMMBinary, "vmm")
	if err != nil {
		return nil, timings, nil, err
	}
	runDir, err := os.MkdirTemp("", "gocracker-vmm-worker-*")
	if err != nil {
		return nil, timings, nil, err
	}
	// Worker runs as opts.UID; MkdirTemp leaves the dir owned by root. The
	// jailer bind-mounts it at /worker in the chroot, and the worker then
	// needs to create /worker/vmm.sock — which fails with EACCES unless the
	// dir is owned by the worker UID. Chown before the jailer spawns.
	workerUID := firstNonNegative(opts.UID, os.Getuid())
	workerGID := firstNonNegative(opts.GID, os.Getgid())
	if err := os.Chown(runDir, workerUID, workerGID); err != nil && !os.IsPermission(err) {
		return nil, timings, nil, fmt.Errorf("chown worker rundir %s to %d:%d: %w", runDir, workerUID, workerGID, err)
	}
	socketHostPath := filepath.Join(runDir, "vmm.sock")
	jailerID := jailerInstanceID(runDir)

	jailedCfg := cfg
	mounts := []string{
		"rw:" + runDir + ":/worker",
	}
	// The jail root must derive from the same unique identifier that we pass
	// to the jailer, otherwise we silently fall back to the shared vm-id path.
	jailRoot := filepath.Join(opts.ChrootBase, filepath.Base(vmmExec), jailerID, "root")
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
			// Chown the drive image to the worker UID so the jailed VMM
			// can open it rw. Silently skip on EPERM (cross-fs bind, or
			// running non-root without CAP_CHOWN — test harness).
			if !drive.ReadOnly {
				if err := os.Chown(drive.Path, workerUID, workerGID); err != nil && !os.IsPermission(err) {
					fmt.Fprintf(os.Stderr, "warn: chown drive for jailer %s: %v\n", drive.Path, err)
				}
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
				return nil, timings, nil, fmt.Errorf("start host virtiofsd for tag %s: %w", fs.Tag, err)
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
		"--id", jailerID,
		"--uid", fmt.Sprintf("%d", workerUID),
		"--gid", fmt.Sprintf("%d", workerGID),
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
		return nil, timings, nil, fmt.Errorf("start jailer: %w", err)
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
		return nil, timings, nil, wrapSubprocessError(err, logBuf)
	}

	client := vmmserver.NewClient(socketHostPath)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	casReq := vmmserver.ConfigureAndStartRequest{
		BootSource: vmmserver.BootSource{
			KernelImagePath: jailedCfg.KernelPath,
			BootArgs:        jailedCfg.Cmdline,
			InitrdPath:      jailedCfg.InitrdPath,
			X86Boot:         string(jailedCfg.X86Boot),
		},
		MachineConfig: &vmmserver.MachineConfig{
			VcpuCount:       jailedCfg.VCPUs,
			MemSizeMib:      int(jailedCfg.MemMB),
			RNGRateLimiter:  jailedCfg.RNGRateLimiter,
			VsockEnabled:    jailedCfg.Vsock != nil && jailedCfg.Vsock.Enabled,
			VsockGuestCID:   vsockGuestCID(jailedCfg.Vsock),
			VsockUDSPath:    vsockUDSPath(jailedCfg.Vsock),
			ExecEnabled:     jailedCfg.Exec != nil && jailedCfg.Exec.Enabled,
			ExecVsockPort:   execVsockPort(jailedCfg.Exec),
			TrackDirtyPages: jailedCfg.TrackDirtyPages,
		},
	}
	if jailedCfg.Balloon != nil {
		casReq.Balloon = &vmmserver.Balloon{
			AmountMib:             jailedCfg.Balloon.AmountMiB,
			DeflateOnOOM:          jailedCfg.Balloon.DeflateOnOOM,
			StatsPollingIntervalS: jailedCfg.Balloon.StatsPollingIntervalS,
			FreePageHinting:       jailedCfg.Balloon.FreePageHinting,
			FreePageReporting:     jailedCfg.Balloon.FreePageReporting,
		}
	}
	if jailedCfg.MemoryHotplug != nil {
		casReq.MemoryHotplug = &vmmserver.MemoryHotplugConfig{
			TotalSizeMiB: jailedCfg.MemoryHotplug.TotalSizeMiB,
			SlotSizeMiB:  jailedCfg.MemoryHotplug.SlotSizeMiB,
			BlockSizeMiB: jailedCfg.MemoryHotplug.BlockSizeMiB,
		}
	}
	for _, drive := range jailedCfg.DriveList() {
		casReq.Drives = append(casReq.Drives, vmmserver.Drive{
			DriveID:      drive.ID,
			PathOnHost:   drive.Path,
			IsRootDevice: drive.Root,
			IsReadOnly:   drive.ReadOnly,
			RateLimiter:  cloneRateLimiter(drive.RateLimiter),
		})
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
		casReq.NetworkInterfaces = append(casReq.NetworkInterfaces, iface)
	}
	for _, fs := range jailedCfg.SharedFS {
		casReq.SharedFS = append(casReq.SharedFS, vmmserver.SharedFS{
			Tag:        fs.Tag,
			Source:     fs.Source,
			SocketPath: fs.SocketPath,
		})
	}

	tPreStart := time.Now()
	timings.Orchestration = tPreStart.Sub(t0)
	if err := client.ConfigureAndStart(ctx, casReq); err != nil {
		_ = cmd.Process.Kill()
		cleanup()
		return nil, timings, nil, wrapSubprocessError(err, logBuf)
	}
	timings.VMMSetup = time.Since(tPreStart)
	timings = timings.Sum()

	hostDisk := cfg.DiskImage // original host path before jailing
	rvm := &remoteVM{
		client:       client,
		cfg:          cfg,
		events:       vmm.NewEventLog(),
		doneCh:       make(chan struct{}),
		cleanup:      cleanup,
		runDir:       runDir,
		socket:       socketHostPath,
		pid:          cmd.Process.Pid,
		jailRoot:     jailRoot,
		created:      time.Now(),
		state:        vmm.StateCreated,
		hostDiskPath: hostDisk,
	}
	go rvm.poll()
	go func() {
		if err := <-waitErrCh; err != nil {
			rvm.events.Emit(vmm.EventError, err.Error())
		}
		rvm.finish()
	}()
	return rvm, timings, cleanup, nil
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
	// The worker process runs as workerOpts.UID (typically 1000), but
	// MkdirTemp was just called by root and left runDir with mode 0700/root.
	// The jailer bind-mounts runDir at /worker inside the chroot; the worker
	// then tries to `listen unix socket /worker/vmm.sock`, which fails with
	// "permission denied" because it can't create files under a root-owned
	// directory. Chown the dir to the configured worker UID/GID before the
	// jailer spawns the worker.
	workerUID := firstNonNegative(workerOpts.UID, os.Getuid())
	workerGID := firstNonNegative(workerOpts.GID, os.Getgid())
	if err := os.Chown(runDir, workerUID, workerGID); err != nil && !os.IsPermission(err) {
		return nil, nil, fmt.Errorf("chown worker rundir %s to %d:%d: %w", runDir, workerUID, workerGID, err)
	}
	socketHostPath := filepath.Join(runDir, "vmm.sock")
	jailerID := jailerInstanceID(runDir)
	jailerArgs := []string{
		"--id", jailerID,
		"--uid", fmt.Sprintf("%d", workerUID),
		"--gid", fmt.Sprintf("%d", workerGID),
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
		SnapshotDir:     "/snapshot",
		TapName:         opts.OverrideTap,
		VcpuCount:       opts.OverrideVCPUs,
		X86Boot:         string(opts.OverrideX86Boot),
		Resume:          resume,
		SharedFSRebinds: opts.SharedFSRebinds,
		// Plumb the new UDS path across the worker RPC so the
		// in-jail vmmserver can rebind the snapshot's vsock device
		// to the caller's socket (sandboxd's per-sandbox UDS).
		// Empty = keep the snapshot's original path.
		VsockUDSPath: opts.OverrideVsockUDSPath,
	})
	if err != nil {
		_ = cmd.Process.Kill()
		cleanup()
		return nil, nil, wrapSubprocessError(err, logBuf)
	}
	restoredCfg := vmm.Config{ID: info.ID, TapName: opts.OverrideTap}
	if info.ExecEnabled {
		restoredCfg.Exec = &vmm.ExecConfig{
			Enabled:   true,
			VsockPort: info.ExecVsockPort,
		}
	}
	rvm := &remoteVM{
		client:   client,
		cfg:      restoredCfg,
		events:   vmm.NewEventLog(),
		doneCh:   make(chan struct{}),
		cleanup:  cleanup,
		runDir:   runDir,
		socket:   socketHostPath,
		pid:      cmd.Process.Pid,
		jailRoot: filepath.Join(workerOpts.ChrootBase, filepath.Base(vmmExec), jailerID, "root"),
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

func (r *remoteVM) Pause() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return r.client.Pause(ctx)
}

func (r *remoteVM) Resume() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return r.client.Resume(ctx)
}

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
	// SkipDiskBundle=true: the in-jail VMM can't reliably hardlink the root
	// disk into artifacts/disk.ext4 (link(2) across the read-only /worker
	// bind-mount returns EXDEV, forcing a 2 GB full copy that used to
	// dominate warm-capture latency on ARM64). The host-side hardlink a
	// few lines below handles that instead.
	if _, err := r.client.Snapshot(ctx, vmmserver.SnapshotRequest{
		DestDir:        "/worker/snapshot-export",
		SkipDiskBundle: true,
	}); err != nil {
		return nil, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := copyTree(exportDir, dir); err != nil {
		return nil, err
	}
	// If the snapshot was taken with SkipDiskBundle=true (warmcache fast path),
	// artifacts/disk.ext4 is missing. Hardlink it on the host from the original
	// disk path — the worker can't do this itself because it lives inside a
	// jailer bind-mount that blocks hardlinks across mount points.
	diskDst := filepath.Join(dir, "artifacts", "disk.ext4")
	if _, err := os.Stat(diskDst); os.IsNotExist(err) && r.hostDiskPath != "" {
		if err := os.MkdirAll(filepath.Dir(diskDst), 0755); err != nil {
			return nil, err
		}
		if err := os.Link(r.hostDiskPath, diskDst); err != nil {
			// Same-FS hardlink failed (rare); rewrite snapshot.json to point
			// at the absolute host path instead of copying the whole disk.
			snap, rerr := vmm.ReadSnapshot(dir)
			if rerr != nil {
				return nil, rerr
			}
			snap.Config.DiskImage = r.hostDiskPath
			data, jerr := json.MarshalIndent(snap, "", "  ")
			if jerr != nil {
				return nil, jerr
			}
			// Use the same fsync-file + fsync-dir pattern as
			// rewriteSnapshotBundleOpts; without it a crash between write and
			// the next sync leaves a half-written snapshot.json that a later
			// restore feeds to the kernel.
			if werr := vmm.WriteSnapshotJSON(dir, data); werr != nil {
				return nil, werr
			}
			return snap, nil
		}
		// Patch snapshot.json to reference the bundled disk.
		snap, rerr := vmm.ReadSnapshot(dir)
		if rerr != nil {
			return nil, rerr
		}
		snap.Config.DiskImage = "artifacts/disk.ext4"
		data, jerr := json.MarshalIndent(snap, "", "  ")
		if jerr != nil {
			return nil, jerr
		}
		if werr := vmm.WriteSnapshotJSON(dir, data); werr != nil {
			return nil, werr
		}
		return snap, nil
	}
	return vmm.ReadSnapshot(dir)
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
	if !r.started.IsZero() && r.state == vmm.StateRunning {
		return time.Since(r.started) + r.uptime
	}
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

// FirstOutputAt returns the cached timestamp at which the guest first
// wrote to the UART. The value is updated by the poll() loop which
// already refreshes VM info periodically — this method is a pure
// accessor with no I/O to avoid HTTP polling storms when called in a
// tight loop (e.g. the 2ms waitFirstOutput poller in container.go).
func (r *remoteVM) FirstOutputAt() time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.firstOutputAt
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
			if r.state != vmm.StateRunning && state == vmm.StateRunning {
				r.started = time.Now()
			}
			if state != vmm.StateRunning {
				r.started = time.Time{}
			}
			r.state = state
			r.uptime = up
			r.devices = append(r.devices[:0], info.Devices...)
			if r.firstOutputAt.IsZero() && !info.FirstOutputAt.IsZero() {
				r.firstOutputAt = info.FirstOutputAt
			}
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
	// Try inotify first; fall back to polling on any setup error.
	exited, err, ok := waitForSocketInotify(path, timeout, waitErrCh)
	if ok {
		return exited, err
	}
	return waitForSocketPoll(path, timeout, waitErrCh)
}

// waitForSocketInotify uses inotify to watch for socket creation, then
// confirms it is connectable. Returns (exited, err, true) on success or
// (_, _, false) if inotify setup failed and the caller should fall back.
func waitForSocketInotify(path string, timeout time.Duration, waitErrCh <-chan error) (bool, error, bool) {
	dir := filepath.Dir(path)
	base := filepath.Base(path)

	// Ensure the parent directory exists before adding a watch.
	if err := os.MkdirAll(dir, 0755); err != nil {
		return false, nil, false
	}

	ifd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return false, nil, false
	}

	// os.File owns the fd; do not also unix.Close(ifd) (double-close).
	inotifyFile := os.NewFile(uintptr(ifd), "inotify")
	defer inotifyFile.Close()

	_, err = unix.InotifyAddWatch(ifd, dir, unix.IN_CREATE)
	if err != nil {
		return false, nil, false
	}

	eventCh := make(chan string, 16)
	go func() {
		defer close(eventCh)
		buf := make([]byte, 4096)
		for {
			n, err := inotifyFile.Read(buf)
			if err != nil {
				return
			}
			offset := 0
			for offset+unix.SizeofInotifyEvent <= n {
				event := (*unix.InotifyEvent)(unsafe.Pointer(&buf[offset]))
				nameLen := int(event.Len)
				if nameLen > 0 && offset+unix.SizeofInotifyEvent+nameLen <= n {
					nameBytes := buf[offset+unix.SizeofInotifyEvent : offset+unix.SizeofInotifyEvent+nameLen]
					// Name is null-terminated.
					if idx := bytes.IndexByte(nameBytes, 0); idx >= 0 {
						nameBytes = nameBytes[:idx]
					}
					eventCh <- string(nameBytes)
				}
				offset += unix.SizeofInotifyEvent + nameLen
			}
		}
	}()

	// Check if the socket already exists (race: it may have been created
	// before the watch was established).
	if conn, err := net.DialTimeout("unix", path, 100*time.Millisecond); err == nil {
		_ = conn.Close()
		return false, nil, true
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case name, ok := <-eventCh:
			if !ok {
				// inotify reader closed unexpectedly; fall back.
				return false, nil, false
			}
			if name != base {
				continue
			}
			// File appeared — try to connect (the process may not be
			// listening yet). Use exponential backoff starting at 1ms.
			backoff := time.Millisecond
			for i := 0; i < 15; i++ {
				conn, err := net.DialTimeout("unix", path, 50*time.Millisecond)
				if err == nil {
					_ = conn.Close()
					return false, nil, true
				}
				time.Sleep(backoff)
				if backoff < 20*time.Millisecond {
					backoff *= 2
				}
			}
			return false, fmt.Errorf("socket %s appeared but is not connectable", path), true

		case waitErr := <-waitErrCh:
			if waitErr == nil {
				return true, fmt.Errorf("worker exited before opening socket %s", path), true
			}
			return true, fmt.Errorf("worker exited before opening socket %s: %w", path, waitErr), true

		case <-timer.C:
			return false, fmt.Errorf("timed out waiting for worker socket %s", path), true
		}
	}
}

// waitForSocketPoll is the legacy polling fallback.
func waitForSocketPoll(path string, timeout time.Duration, waitErrCh <-chan error) (bool, error) {
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

func jailerInstanceID(runDir string) string {
	base := strings.TrimSpace(filepath.Base(runDir))
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "gocracker-vmm"
	}
	return base
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
		// A bare "gocracker" binary dispatches by first arg (run/serve/vmm/
		// jailer/build-worker/...) so we still need to prepend the mode.
		// Standalone gocracker-vmm / gocracker-jailer / gocracker-build-worker
		// binaries take flags directly — no prefix. Decide by basename.
		if filepath.Base(explicit) == "gocracker" {
			return explicit, []string{mode}, nil
		}
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

func vsockUDSPath(cfg *vmm.VsockConfig) string {
	if cfg == nil {
		return ""
	}
	return cfg.UDSPath
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
			// Hardlink first: instant and zero-copy on same filesystem.
			// Snapshot assets are read-only after bundling so sharing an
			// inode is safe. Falls back to full read/write copy on EXDEV.
			if err := os.Link(path, target); err == nil {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, info.Mode())
		}
	})
}
