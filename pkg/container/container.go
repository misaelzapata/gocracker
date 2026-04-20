// Package container is the high-level runtime.
// Accepts a git repo URL, local path, OCI image ref, or Dockerfile —
// builds a rootfs + ext4 disk, generates a kernel cmdline, and boots a VM.
package container

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gocracker/gocracker/internal/buildserver"
	"github.com/gocracker/gocracker/internal/dockerfile"
	"github.com/gocracker/gocracker/internal/guest"
	"github.com/gocracker/gocracker/internal/guestexec"
	"github.com/gocracker/gocracker/internal/hostguard"
	"github.com/gocracker/gocracker/internal/hostnet"
	gclog "github.com/gocracker/gocracker/internal/log"
	"github.com/gocracker/gocracker/internal/oci"
	"github.com/gocracker/gocracker/internal/repo"
	"github.com/gocracker/gocracker/internal/runtimecfg"
	"github.com/gocracker/gocracker/internal/worker"
	"github.com/gocracker/gocracker/pkg/vmm"
	"github.com/gocracker/gocracker/pkg/warmcache"
)

// RunOptions describes how to run a container as a microVM.
type RunOptions struct {
	// Source — exactly one must be set:
	Image      string // OCI ref e.g. "ubuntu:22.04"
	Dockerfile string // explicit path to a Dockerfile
	Context    string // build context dir (used with Dockerfile)
	RepoURL        string // git remote URL or local path — Dockerfile auto-detected
	RepoRef        string // branch/tag/commit (default: repo default branch)
	RepoSubdir     string // subdir inside repo containing the Dockerfile
	RepoDockerfile string // explicit Dockerfile path relative to RepoSubdir (skips name-based discovery); rescues non-canonical filenames like Dockerfile-envoy

	// VM
	MemMB       uint64
	Arch        string
	CPUs        int
	KernelPath  string
	TapName     string
	NetworkMode string
	X86Boot     vmm.X86BootMode

	// Container overrides
	Cmd           []string
	Entrypoint    []string
	Env           []string
	Hosts         []string
	WorkDir       string
	PID1Mode      string
	Mounts        []Mount
	KernelModules []guest.KernelModule
	ConsoleOut    io.Writer
	ConsoleIn     io.Reader

	// Disk image size in MiB (default 2048)
	DiskSizeMB int

	// Working dir for build artifacts (auto-generated if empty)
	WorkDir2 string

	// Build args (Dockerfile ARG values)
	BuildArgs map[string]string

	// VM identifier (auto-generated if empty)
	ID string

	// CacheDir enables persistent reuse of OCI and VM artifacts across runs.
	CacheDir string

	// Metadata is persisted into the VM config and surfaced by serve.
	Metadata map[string]string

	// JailerMode controls whether privileged boot happens through jailed workers.
	JailerMode string

	// Snapshot dir: if set, attempt fast restore before building
	SnapshotDir string

	// StaticIP / gateway for the guest network (optional)
	StaticIP string
	Gateway  string

	// Optional internal exec access over virtio-vsock.
	ExecEnabled bool
	// InteractiveExec boots the guest into an idle supervisor so the CLI can
	// attach a PTY over the exec agent instead of running the image process as PID 1.
	InteractiveExec bool
	// VsockUDSPath, when set, tells the VMM to expose its vsock device as a
	// Firecracker-style Unix Domain Socket at this absolute path. Clients
	// outside the VMM (sandboxd, CLI, socat) dial the path and send
	// "CONNECT <port>\n" to reach a guest vsock port. Setting this path
	// implies vsock is enabled.
	VsockUDSPath string

	// Additional create-time block devices exposed after the root disk.
	Drives []vmm.DriveConfig

	// Optional memory management devices.
	Balloon       *vmm.BalloonConfig
	MemoryHotplug *vmm.MemoryHotplugConfig

	// Explicit worker/jailer launch configuration used by serve/supervisors.
	JailerBinary string
	VMMBinary    string
	ChrootBase   string
	UID          int
	GID          int

	// RootfsPersistent forces the guest to mount the rootfs read-write
	// directly (no tmpfs overlay). Writes land on the per-VM disk file and
	// survive VM shutdown, at the cost of a slower boot (copyDiskImage has
	// to reflink/copy instead of hardlinking the template). Leave false for
	// Docker-style ephemeral writes and fast boot.
	RootfsPersistent bool

	// WarmCapture enables the automatic snapshot-cache flow:
	//   - On cache HIT:  restore from snapshot instead of cold-booting (~3 ms).
	//   - On cache MISS: cold-boot as normal, then take a snapshot and store it
	//     in the warmcache so the NEXT run hits the fast path.
	// The cache key covers (image digest, kernel sha256, cmdline, mem, vCPUs,
	// arch) so it is safe to enable permanently — any parameter change misses.
	// Equivalent to setting GOCRACKER_WARM_CACHE=1 but applies per-call.
	WarmCapture bool
}

// RunResult is returned after a VM is started.
//
// Duration is the total wall clock reported by the runtime and is kept for
// backwards compatibility. New code should prefer Timings, which breaks the
// total down into orchestration / VMM setup / start / guest-first-output
// phases so the caller can see exactly where the time is going.
type RunResult struct {
	VM           vmm.Handle
	DiskPath     string
	ID           string
	Config       oci.ImageConfig
	TapName      string
	GuestIP      string
	Gateway      string
	WorkerSocket string
	Duration     time.Duration
	Timings      vmm.BootTimings
	// WarmDone is closed when the background warmcache snapshot goroutine
	// completes. The goroutine accesses VM memory and must finish before the
	// VM is freed, so RunResult.Close() automatically drains it before
	// running user cleanup. Callers that want a deterministic "snapshot is
	// persisted" signal (e.g. the CLI happy path) can still wait on this
	// channel directly before issuing vm.Stop().
	WarmDone <-chan struct{}
	cleanup func()
}

// Close releases all host-side resources tied to the run: it first drains the
// background warm-cache capture goroutine (if any) to avoid a use-after-free
// on guest RAM, then runs the registered cleanup (TAP teardown, runtime disk
// cleanup schedule, etc.). Safe to call multiple times; no-op after the first.
func (r *RunResult) Close() {
	if r == nil {
		return
	}
	if r.WarmDone != nil {
		<-r.WarmDone
		r.WarmDone = nil
	}
	if r.cleanup != nil {
		c := r.cleanup
		r.cleanup = nil
		c()
	}
}

const (
	runtimeDiskRetention    = time.Minute
	runArtifactCacheVersion = 2

	// firstOutputWaitMax is how long to wait for the guest's first UART byte
	// (for the guest_first_output_ms metric). 500 ms is the upper-bound on
	// ARM64 first-byte latency with our kernel; x86 comes in under 50 ms.
	// Bounded short enough that a broken boot still reports "started" within
	// half a second; not a gate on anything functional.
	firstOutputWaitMax = 500 * time.Millisecond
)

// waitFirstOutput polls for the guest's first UART output, returning the
// elapsed time from startedAt. Returns 0 if h is nil or nothing arrives.
func waitFirstOutput(h vmm.Handle, startedAt time.Time, maxWait time.Duration) time.Duration {
	if h == nil {
		return 0
	}
	deadline := startedAt.Add(maxWait)
	for {
		if at := h.FirstOutputAt(); !at.IsZero() {
			d := at.Sub(startedAt)
			if d < 0 {
				// Guest wrote to the UART before vm.Start() returned — common on
				// ARM64 where vCPU setup overlaps early kernel output. Report as
				// ~instant instead of the sentinel zero.
				return time.Microsecond
			}
			return d
		}
		if time.Now().After(deadline) {
			return 0
		}
		time.Sleep(2 * time.Millisecond)
	}
}


const (
	NetworkModeNone = ""
	NetworkModeAuto = "auto"
	JailerModeOn    = "on"
	JailerModeOff   = "off"
)

func defaultCacheDir() string {
	return filepath.Join(os.TempDir(), "gocracker", "cache")
}

func resolvedCacheDir(cacheDir string) string {
	base := strings.TrimSpace(cacheDir)
	if base != "" {
		return base
	}
	return defaultCacheDir()
}

type MountBackend string

const (
	MountBackendMaterialized MountBackend = ""
	MountBackendVirtioFS     MountBackend = "virtiofs"
)

// Mount describes a host path materialized into the guest filesystem
// before the ext4 image is built, or exported live via a shared filesystem backend.
type Mount struct {
	Source   string       `json:"source"`
	Target   string       `json:"target"`
	ReadOnly bool         `json:"read_only"`
	Populate bool         `json:"populate"`
	Backend  MountBackend `json:"backend"`
}

// Run builds (if needed) and starts a microVM.
func Run(opts RunOptions) (*RunResult, error) {
	if opts.Arch == "" {
		opts.Arch = runtime.GOARCH
	}
	if opts.Arch != runtime.GOARCH {
		return nil, fmt.Errorf("arch %q is not compatible with host arch %q (same-arch only)", opts.Arch, runtime.GOARCH)
	}
	if jailerEnabled(opts.JailerMode) {
		return runViaWorker(opts)
	}
	return runLocal(opts)
}

func jailerEnabled(mode string) bool {
	switch strings.TrimSpace(strings.ToLower(mode)) {
	case "", JailerModeOn:
		return true
	case JailerModeOff:
		return false
	default:
		return true
	}
}

func runLocal(opts RunOptions) (*RunResult, error) {
	if opts.MemMB == 0 {
		opts.MemMB = 256
	}
	if opts.Arch == "" {
		opts.Arch = runtime.GOARCH
	}
	if opts.DiskSizeMB == 0 {
		opts.DiskSizeMB = 2048
	}
	if opts.ID == "" {
		opts.ID = fmt.Sprintf("gc-%d", time.Now().UnixNano()%100000)
	}
	if err := hostguard.CheckHostDevices(hostguard.DeviceRequirements{
		NeedKVM: true,
		NeedTun: opts.TapName != "" || opts.NetworkMode == NetworkModeAuto,
	}); err != nil {
		return nil, fmt.Errorf("host device preflight: %w", err)
	}
	workDir, err := resolveRunWorkDir(opts)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return nil, fmt.Errorf("create workdir %s: %w", workDir, err)
	}

	var autoNet *hostnet.AutoNetwork
	if opts.NetworkMode != "" && opts.NetworkMode != NetworkModeAuto {
		return nil, fmt.Errorf("invalid network mode %q", opts.NetworkMode)
	}
	// network_mode=auto allocates a fresh tap + /30 + guest IP + gateway.
	// On cold boot this drives the kernel cmdline so the guest comes up with
	// the right address. On restore the guest kernel is already frozen with
	// the template's IP, but we still allocate a fresh subnet here for two
	// reasons: (1) sandboxes need their own tap/subnet to coexist with the
	// template + sibling clones without colliding, (2) the API layer issues
	// a post-restore re-IP exec when ExecEnabled to plumb the new addresses
	// into the running guest.
	if opts.NetworkMode == NetworkModeAuto {
		var err error
		autoNet, err = hostnet.NewAuto(opts.ID, opts.TapName)
		if err != nil {
			return nil, fmt.Errorf("auto network: %w", err)
		}
		opts.TapName = autoNet.TapName()
		opts.StaticIP = autoNet.GuestCIDR()
		opts.Gateway = autoNet.GatewayIP()
	}

	// Warm-cache lookup for --jailer off path (mirrors runViaWorker logic).
	var warmCacheKeyLocal string
	if opts.SnapshotDir == "" && (warmCacheEnabled() || opts.WarmCapture) && warmCacheInputsReady(opts) {
		if key, ok := computeWarmCacheKey(opts); ok {
			warmCacheKeyLocal = key
			if dir, hit := warmcache.Lookup(warmcache.DefaultRoot(), key); hit {
				gclog.Container.Info("warm-cache hit", "key", key[:12], "dir", dir)
				opts.SnapshotDir = dir
			} else {
				gclog.Container.Debug("warm-cache miss", "key", key[:12])
			}
		}
	}

	// ---- Fast path: snapshot restore ----
	if opts.SnapshotDir != "" {
		if len(opts.Drives) > 0 {
			return nil, fmt.Errorf("snapshot restore is not supported with additional block devices yet")
		}
		if err := validateRestoreMounts(opts.Mounts); err != nil {
			return nil, err
		}
		if _, err := os.Stat(opts.SnapshotDir + "/snapshot.json"); err == nil {
			gclog.Container.Info("restoring from snapshot", "dir", opts.SnapshotDir)
			t0 := time.Now()
			// OverrideID ensures the restored VM gets its caller-visible
			// ID (opts.ID) instead of inheriting the snapshot's cfg.ID.
			// Without this, the restored VM's internal cfg.ID collides
			// with the original VM's cfg.ID (both derived from the same
			// snapshot source). Although the API server tracks VMs by
			// an external key, logs and any cfg.ID-keyed lookup get
			// confused, and it becomes harder to distinguish original
			// from restored in instrumentation.
			vm, err := vmm.RestoreFromSnapshotWithOptions(opts.SnapshotDir, vmm.RestoreOptions{
				ConsoleIn:        opts.ConsoleIn,
				ConsoleOut:       opts.ConsoleOut,
				OverrideVCPUs:    opts.CPUs,
				OverrideID:       opts.ID,
				OverrideTap:      opts.TapName,
				OverrideX86Boot:  opts.X86Boot,
				SharedFSRebinds:  buildSharedFSRebinds(opts.Mounts),
			})
			if err == nil {
				// Activate the freshly-allocated host-side tap (assigns the
				// gateway IP, brings link up, installs NAT) before we resume
				// the guest. Without this, packets from the restored guest
				// hit a tap with no host IP and no NAT.
				if autoNet != nil {
					if actErr := autoNet.Activate(); actErr != nil {
						vm.Stop()
						autoNet.Close()
						return nil, fmt.Errorf("activate auto network on restore: %w", actErr)
					}
				}
				// Re-fire the virtio-vsock queue IRQ before resuming: on ARM64
				// the TRANSPORT_RESET event queued by QuiesceForSnapshot can be
				// lost across the GIC reset, and without the kick the guest's
				// vsock driver never drains the event — subsequent host dials
				// to the exec agent time out because the guest never responds.
				vm.KickVsockIRQ()
				if err := vm.Start(); err != nil {
					return nil, err
				}
				if autoNet != nil && opts.ExecEnabled {
					if ripErr := reIPGuest(vm, opts.StaticIP, opts.Gateway, 2*time.Second); ripErr != nil {
						gclog.Container.Warn("restore re-IP failed", "error", ripErr)
					}
				}
				gclog.Container.Info("restored", "duration", time.Since(t0).Round(time.Millisecond))
				tap := opts.TapName
				if tap == "" {
					tap = vm.VMConfig().TapName
				}
				result := &RunResult{
					VM:      vm,
					ID:      opts.ID,
					TapName: tap,
					GuestIP: trimCIDR(opts.StaticIP),
					Gateway: opts.Gateway,
					cleanup: func() {
						if autoNet != nil {
							autoNet.Close()
						}
					},
				}
				return result, nil
			}
			gclog.Container.Warn("snapshot restore failed, building fresh", "error", err)
			// Close the autoNet TAP so the cold-boot retry can allocate a fresh
			// one. Without this, NewAuto below fails with TUNSETIFF EBUSY.
			if autoNet != nil {
				autoNet.Close()
				autoNet = nil
				opts.TapName = ""
				opts.StaticIP = ""
				opts.Gateway = ""
			}
			// Clear SnapshotDir so the cold-boot path doesn't loop.
			opts.SnapshotDir = ""
			// Re-allocate autoNet for the fresh boot.
			if opts.NetworkMode == NetworkModeAuto {
				autoNet, err = hostnet.NewAuto(opts.ID, opts.TapName)
				if err != nil {
					return nil, fmt.Errorf("auto network (retry): %w", err)
				}
				opts.TapName = autoNet.TapName()
				opts.StaticIP = autoNet.GuestCIDR()
				opts.Gateway = autoNet.GatewayIP()
			}
		}
	}

	// ---- Resolve repo source (if given) ----
	if opts.RepoURL != "" {
		resolved, err := resolveRepo(opts, workDir)
		if err != nil {
			return nil, err
		}
		defer resolved.cleanup()
		opts.Dockerfile = resolved.dockerfile
		opts.Context = resolved.context
	}

	// ---- Build rootfs (cached on disk) ----
	rootfsDir := filepath.Join(workDir, "rootfs")
	diskPath := filepath.Join(workDir, "disk.ext4")
	initrdPath := filepath.Join(workDir, "initrd.img")
	configPath := filepath.Join(workDir, "image-config.json")
	specPath := filepath.Join(workDir, "runtime-spec.json")
	defer func() { _ = os.RemoveAll(rootfsDir) }()

	var imgConfig oci.ImageConfig
	var guestSpec runtimecfg.GuestSpec
	sharedFS := resolveSharedFSMounts(opts.Mounts)
	kernelModules := append([]guest.KernelModule{}, opts.KernelModules...)
	kernelModules = appendVirtioFSKernelModule(kernelModules, sharedFS)

	rebuildDisk := hasMaterializedMounts(opts.Mounts)
	if !rebuildDisk {
		cached, usable, reason, err := inspectCachedRunArtifacts(diskPath, configPath)
		if err != nil {
			return nil, fmt.Errorf("inspect artifact cache: %w", err)
		}
		if usable {
			imgConfig = cached
		} else {
			rebuildDisk = true
			if reason != "" {
				gclog.Container.Warn("invalidating cached artifacts", "path", workDir, "reason", reason)
			}
			if err := removeCachedRunArtifacts(diskPath, initrdPath, configPath); err != nil {
				return nil, fmt.Errorf("reset cached artifacts %s: %w", workDir, err)
			}
		}
	}

	if rebuildDisk {
		gclog.Container.Info("artifact cache miss", "path", workDir)
		if err := os.RemoveAll(rootfsDir); err != nil {
			return nil, fmt.Errorf("reset rootfs %s: %w", rootfsDir, err)
		}
		if err := os.MkdirAll(rootfsDir, 0755); err != nil {
			return nil, fmt.Errorf("create rootfs %s: %w", rootfsDir, err)
		}

		var err error
		imgConfig, err = buildRootfs(rootfsDir, opts)
		if err != nil {
			return nil, err
		}
		guestSpec = buildGuestSpec(imgConfig, opts, sharedFS)
		if err := writeRuntimeSpecToRootfs(rootfsDir, guestSpec); err != nil {
			return nil, fmt.Errorf("write runtime spec: %w", err)
		}

		injectHostCACerts(rootfsDir)
		if err := oci.BuildExt4(rootfsDir, diskPath, opts.DiskSizeMB); err != nil {
			return nil, fmt.Errorf("ext4: %w", err)
		}
		if err := writeImageConfig(configPath, imgConfig); err != nil {
			return nil, fmt.Errorf("write image config: %w", err)
		}
		if err := writeGuestSpecCache(specPath, guestSpec); err != nil {
			return nil, fmt.Errorf("write runtime spec cache: %w", err)
		}
	} else {
		gclog.Container.Info("artifact cache hit", "path", workDir)
		gclog.Container.Info("reusing cached disk", "path", diskPath)
	}
	if !guestSpec.HasStructuredFields() {
		guestSpec = buildGuestSpec(imgConfig, opts, sharedFS)
	}

	// ---- Build initrd ----
	reuseInitrd, initrdReason, err := shouldReuseCachedInitrd(initrdPath, specPath, guestSpec)
	if err != nil {
		return nil, fmt.Errorf("inspect cached initrd: %w", err)
	}
	if reuseInitrd && !rebuildDisk {
		gclog.Container.Info("reusing cached initrd", "path", initrdPath)
	} else {
		if !rebuildDisk && initrdReason != "" {
			gclog.Container.Warn("invalidating cached initrd", "path", initrdPath, "reason", initrdReason)
		}
		if err := guest.BuildInitrdWithOptions(initrdPath, guest.InitrdOptions{
			KernelModules: kernelModules,
			RuntimeSpec:   &guestSpec,
		}); err != nil {
			return nil, fmt.Errorf("initrd: %w", err)
		}
		if err := writeGuestSpecCache(specPath, guestSpec); err != nil {
			return nil, fmt.Errorf("write runtime spec cache: %w", err)
		}
	}

	// ---- Assemble kernel cmdline ----
	cmdline := buildCmdlineWithPlan(opts, sharedFS, len(kernelModules) > 0)

	bootDiskPath, cleanupRuntimeDisk, err := prepareBootDisk(workDir, diskPath, opts.ID, !opts.RootfsPersistent)
	if err != nil {
		return nil, fmt.Errorf("prepare boot disk: %w", err)
	}
	// ---- Boot ----
	gclog.Container.Info("booting", "id", opts.ID)
	t0 := time.Now()
	var timings vmm.BootTimings

	tPreNew := time.Now()
	timings.Orchestration = tPreNew.Sub(t0)
	vm, err := vmm.New(vmm.Config{
		ID:              opts.ID,
		MemMB:           opts.MemMB,
		Arch:            opts.Arch,
		VCPUs:           opts.CPUs,
		X86Boot:         opts.X86Boot,
		KernelPath:      opts.KernelPath,
		InitrdPath:      initrdPath,
		Cmdline:         cmdline,
		DiskImage:       bootDiskPath,
		Drives:          runtimeDrives(bootDiskPath, opts),
		TapName:         opts.TapName,
		Metadata:        cloneStringMap(opts.Metadata),
		SharedFS:        sharedFS.Exports,
		Vsock:           buildVsockConfig(opts),
		Exec:            buildExecConfig(opts),
		Balloon:         cloneBalloonConfig(opts.Balloon),
		MemoryHotplug:   cloneMemoryHotplugConfig(opts.MemoryHotplug),
		ConsoleOut:      opts.ConsoleOut,
		ConsoleIn:       opts.ConsoleIn,
		TrackDirtyPages: warmCacheKeyLocal != "" && opts.WarmCapture && runtime.GOARCH != "arm64",
	})
	if err != nil {
		if autoNet != nil {
			autoNet.Close()
		}
		return nil, fmt.Errorf("create vm: %w", err)
	}
	timings.VMMSetup = time.Since(tPreNew)
	if autoNet != nil {
		if err := autoNet.Activate(); err != nil {
			vm.Stop()
			autoNet.Close()
			return nil, fmt.Errorf("activate auto network: %w", err)
		}
	}
	tPreStart := time.Now()
	if err := vm.Start(); err != nil {
		if autoNet != nil {
			autoNet.Close()
		}
		return nil, fmt.Errorf("start vm: %w", err)
	}
	timings.Start = time.Since(tPreStart)
	timings.GuestFirstOutput = waitFirstOutput(vm, time.Now(), firstOutputWaitMax)

	var cleanupOnce sync.Once
	cleanupFn := cleanupRuntimeDisk
	if autoNet != nil {
		cleanupFn = func() {
			cleanupOnce.Do(func() {
				autoNet.Close()
				cleanupRuntimeDisk()
			})
		}
		go func() {
			for {
				state := vm.State()
				if state != vmm.StateRunning && state != vmm.StateCreated && state != vmm.StatePaused {
					cleanupFn()
					return
				}
				time.Sleep(250 * time.Millisecond)
			}
		}()
	}

	timings = timings.Sum()
	bootDuration := time.Since(t0).Round(time.Millisecond)
	gclog.Container.Info("started", "id", opts.ID,
		"duration", bootDuration,
		"orchestration_ms", timings.Orchestration.Milliseconds(),
		"vmm_setup_ms", timings.VMMSetup.Milliseconds(),
		"start_ms", timings.Start.Milliseconds(),
		"guest_first_output_ms", timings.GuestFirstOutput.Milliseconds())
	var warmDoneLocal <-chan struct{}
	if warmCacheKeyLocal != "" {
		ch := make(chan struct{})
		warmDoneLocal = ch
		go func() {
			defer close(ch)
			captureWarmSnapshot(vm, opts, warmCacheKeyLocal)
		}()
	}
	return &RunResult{
		VM:       vm,
		DiskPath: bootDiskPath,
		ID:       opts.ID,
		Config:   imgConfig,
		TapName:  opts.TapName,
		GuestIP:  trimCIDR(opts.StaticIP),
		Gateway:  opts.Gateway,
		Duration: bootDuration,
		Timings:  timings,
		WarmDone: warmDoneLocal,
		cleanup:  cleanupFn,
	}, nil
}

func trimCIDR(value string) string {
	if value == "" {
		return ""
	}
	if idx := strings.IndexByte(value, '/'); idx >= 0 {
		return value[:idx]
	}
	return value
}

func buildVsockConfig(opts RunOptions) *vmm.VsockConfig {
	if !opts.ExecEnabled && !guestAgentRequired(opts.Balloon, opts.MemoryHotplug) && opts.VsockUDSPath == "" {
		return nil
	}
	return &vmm.VsockConfig{
		Enabled:  true,
		GuestCID: 0,
		UDSPath:  opts.VsockUDSPath,
	}
}

func workerSocket(handle vmm.Handle) string {
	workerHandle, ok := handle.(vmm.WorkerBacked)
	if !ok {
		return ""
	}
	return workerHandle.WorkerMetadata().SocketPath
}

func runViaWorker(opts RunOptions) (*RunResult, error) {
	if opts.MemMB == 0 {
		opts.MemMB = 256
	}
	if opts.Arch == "" {
		opts.Arch = runtime.GOARCH
	}
	if opts.DiskSizeMB == 0 {
		opts.DiskSizeMB = 2048
	}
	if opts.ID == "" {
		opts.ID = fmt.Sprintf("gc-%d", time.Now().UnixNano()%100000)
	}
	if err := hostguard.CheckHostDevices(hostguard.DeviceRequirements{
		NeedKVM: true,
		NeedTun: opts.TapName != "" || opts.NetworkMode == NetworkModeAuto,
	}); err != nil {
		return nil, fmt.Errorf("host device preflight: %w", err)
	}
	workDir, err := resolveRunWorkDir(opts)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return nil, fmt.Errorf("create workdir %s: %w", workDir, err)
	}

	var autoNet *hostnet.AutoNetwork
	if opts.NetworkMode != "" && opts.NetworkMode != NetworkModeAuto {
		return nil, fmt.Errorf("invalid network mode %q", opts.NetworkMode)
	}
	// See runLocal: auto-allocation only fires on cold boot; restore keeps
	// the snapshot's IP plan to avoid lying to the caller and breaking the
	// frozen guest network.
	if opts.NetworkMode == NetworkModeAuto && opts.SnapshotDir == "" {
		var err error
		autoNet, err = hostnet.NewAuto(opts.ID, opts.TapName)
		if err != nil {
			return nil, fmt.Errorf("auto network: %w", err)
		}
		opts.TapName = autoNet.TapName()
		opts.StaticIP = autoNet.GuestCIDR()
		opts.Gateway = autoNet.GatewayIP()
	}

	// Opportunistic warm-cache lookup. Active when GOCRACKER_WARM_CACHE=1 or
	// opts.WarmCapture is set (the --warm flag). On a hit, rewrite SnapshotDir
	// so the fast-restore branch below serves this run in ~3 ms instead of
	// ~200 ms. On a miss, fall through to cold boot; after the VM is up the
	// captureWarmSnapshot helper will snapshot it and store the entry so the
	// next run is a hit.
	var warmCacheKey string
	if opts.SnapshotDir == "" && (warmCacheEnabled() || opts.WarmCapture) && warmCacheInputsReady(opts) {
		if key, ok := computeWarmCacheKey(opts); ok {
			warmCacheKey = key
			if dir, hit := warmcache.Lookup(warmcache.DefaultRoot(), key); hit {
				gclog.Container.Info("warm-cache hit", "key", key[:12], "dir", dir)
				opts.SnapshotDir = dir
			} else {
				gclog.Container.Debug("warm-cache miss", "key", key[:12])
			}
		}
	}

	if opts.SnapshotDir != "" {
		if len(opts.Drives) > 0 {
			return nil, fmt.Errorf("snapshot restore is not supported with additional block devices yet")
		}
		if err := validateRestoreMounts(opts.Mounts); err != nil {
			return nil, err
		}
		if _, err := os.Stat(filepath.Join(opts.SnapshotDir, "snapshot.json")); err == nil {
			gclog.Container.Info("restoring from snapshot via worker", "dir", opts.SnapshotDir)
			handle, cleanup, err := worker.LaunchRestoredVMM(opts.SnapshotDir, vmm.RestoreOptions{
				OverrideTap:     opts.TapName,
				OverrideVCPUs:   opts.CPUs,
				OverrideX86Boot: opts.X86Boot,
				ConsoleIn:       opts.ConsoleIn,
				ConsoleOut:      opts.ConsoleOut,
				SharedFSRebinds: buildSharedFSRebinds(opts.Mounts),
			}, worker.VMMOptions{
				JailerBinary: opts.JailerBinary,
				VMMBinary:    opts.VMMBinary,
				UID:          firstNonNegative(opts.UID, os.Getuid()),
				GID:          firstNonNegative(opts.GID, os.Getgid()),
				ChrootBase:   opts.ChrootBase,
			})
			if err == nil {
				if autoNet != nil {
					if actErr := autoNet.Activate(); actErr != nil {
						handle.Stop()
						autoNet.Close()
						if cleanup != nil {
							cleanup()
						}
						return nil, fmt.Errorf("activate auto network on restore: %w", actErr)
					}
					if opts.ExecEnabled {
						if ripErr := reIPGuest(handle, opts.StaticIP, opts.Gateway, 2*time.Second); ripErr != nil {
							gclog.Container.Warn("restore re-IP failed", "error", ripErr)
						}
					}
				}
				tap := opts.TapName
				if tap == "" {
					tap = handle.VMConfig().TapName
				}
				return &RunResult{
					VM:           handle,
					ID:           handle.ID(),
					TapName:      tap,
					GuestIP:      trimCIDR(opts.StaticIP),
					Gateway:      opts.Gateway,
					WorkerSocket: workerSocket(handle),
					cleanup: func() {
						if autoNet != nil {
							autoNet.Close()
						}
						if cleanup != nil {
							cleanup()
						}
					},
				}, nil
			}
			gclog.Container.Warn("snapshot restore via worker failed, building fresh", "error", err)
		}
	}

	if opts.RepoURL != "" {
		resolved, err := resolveRepo(opts, workDir)
		if err != nil {
			return nil, err
		}
		defer resolved.cleanup()
		opts.Dockerfile = resolved.dockerfile
		opts.Context = resolved.context
	}

	rootfsDir := filepath.Join(workDir, "rootfs")
	diskPath := filepath.Join(workDir, "disk.ext4")
	initrdPath := filepath.Join(workDir, "initrd.img")
	configPath := filepath.Join(workDir, "image-config.json")
	specPath := filepath.Join(workDir, "runtime-spec.json")
	defer func() { _ = os.RemoveAll(rootfsDir) }()

	var imgConfig oci.ImageConfig
	var guestSpec runtimecfg.GuestSpec
	sharedFS := resolveSharedFSMounts(opts.Mounts)
	kernelModules := append([]guest.KernelModule{}, opts.KernelModules...)
	kernelModules = appendVirtioFSKernelModule(kernelModules, sharedFS)

	rebuildDisk := hasMaterializedMounts(opts.Mounts)
	if !rebuildDisk {
		cached, usable, reason, err := inspectCachedRunArtifacts(diskPath, configPath)
		if err != nil {
			return nil, fmt.Errorf("inspect artifact cache: %w", err)
		}
		if usable {
			imgConfig = cached
		} else {
			rebuildDisk = true
			if reason != "" {
				gclog.Container.Warn("invalidating cached artifacts", "path", workDir, "reason", reason)
			}
			if err := removeCachedRunArtifacts(diskPath, initrdPath, configPath); err != nil {
				return nil, fmt.Errorf("reset cached artifacts %s: %w", workDir, err)
			}
		}
	}
	if rebuildDisk {
		gclog.Container.Info("artifact cache miss", "path", workDir)
		if err := os.RemoveAll(rootfsDir); err != nil {
			return nil, fmt.Errorf("reset rootfs %s: %w", rootfsDir, err)
		}
		if err := os.MkdirAll(rootfsDir, 0755); err != nil {
			return nil, fmt.Errorf("create rootfs %s: %w", rootfsDir, err)
		}
		var err error
		imgConfig, err = buildRootfs(rootfsDir, opts)
		if err != nil {
			return nil, err
		}
		guestSpec = buildGuestSpec(imgConfig, opts, sharedFS)
		if err := writeRuntimeSpecToRootfs(rootfsDir, guestSpec); err != nil {
			return nil, fmt.Errorf("write runtime spec: %w", err)
		}
		injectHostCACerts(rootfsDir)
		if err := oci.BuildExt4(rootfsDir, diskPath, opts.DiskSizeMB); err != nil {
			return nil, fmt.Errorf("ext4: %w", err)
		}
		if err := writeImageConfig(configPath, imgConfig); err != nil {
			return nil, fmt.Errorf("write image config: %w", err)
		}
		if err := writeGuestSpecCache(specPath, guestSpec); err != nil {
			return nil, fmt.Errorf("write runtime spec cache: %w", err)
		}
	} else {
		gclog.Container.Info("artifact cache hit", "path", workDir)
		gclog.Container.Info("reusing cached disk", "path", diskPath)
	}
	if !guestSpec.HasStructuredFields() {
		guestSpec = buildGuestSpec(imgConfig, opts, sharedFS)
	}
	reuseInitrd, initrdReason, err := shouldReuseCachedInitrd(initrdPath, specPath, guestSpec)
	if err != nil {
		return nil, fmt.Errorf("inspect cached initrd: %w", err)
	}
	if reuseInitrd && !rebuildDisk {
		gclog.Container.Info("reusing cached initrd", "path", initrdPath)
	} else {
		if !rebuildDisk && initrdReason != "" {
			gclog.Container.Warn("invalidating cached initrd", "path", initrdPath, "reason", initrdReason)
		}
		if err := guest.BuildInitrdWithOptions(initrdPath, guest.InitrdOptions{
			KernelModules: kernelModules,
			RuntimeSpec:   &guestSpec,
		}); err != nil {
			return nil, fmt.Errorf("initrd: %w", err)
		}
		if err := writeGuestSpecCache(specPath, guestSpec); err != nil {
			return nil, fmt.Errorf("write runtime spec cache: %w", err)
		}
	}
	cmdline := buildCmdlineWithPlan(opts, sharedFS, len(kernelModules) > 0)

	bootDiskPath, cleanupRuntimeDisk, err := prepareBootDisk(workDir, diskPath, opts.ID, !opts.RootfsPersistent)
	if err != nil {
		return nil, fmt.Errorf("prepare boot disk: %w", err)
	}
	t0 := time.Now()
	handle, timings, cleanup, err := worker.LaunchVMMWithTimings(vmm.Config{
		ID:              opts.ID,
		MemMB:           opts.MemMB,
		Arch:            opts.Arch,
		VCPUs:           opts.CPUs,
		X86Boot:         opts.X86Boot,
		KernelPath:      opts.KernelPath,
		InitrdPath:      initrdPath,
		Cmdline:         cmdline,
		DiskImage:       bootDiskPath,
		Drives:          runtimeDrives(bootDiskPath, opts),
		TapName:         opts.TapName,
		Metadata:        cloneStringMap(opts.Metadata),
		SharedFS:        sharedFS.Exports,
		Vsock:           buildVsockConfig(opts),
		Exec:            buildExecConfig(opts),
		Balloon:         cloneBalloonConfig(opts.Balloon),
		MemoryHotplug:   cloneMemoryHotplugConfig(opts.MemoryHotplug),
		ConsoleOut:      opts.ConsoleOut,
		ConsoleIn:       opts.ConsoleIn,
		TrackDirtyPages: warmCacheKey != "" && opts.WarmCapture && runtime.GOARCH != "arm64",
	}, worker.VMMOptions{
		JailerBinary: opts.JailerBinary,
		VMMBinary:    opts.VMMBinary,
		UID:          firstNonNegative(opts.UID, os.Getuid()),
		GID:          firstNonNegative(opts.GID, os.Getgid()),
		ChrootBase:   opts.ChrootBase,
	})
	if err != nil {
		if autoNet != nil {
			autoNet.Close()
		}
		return nil, fmt.Errorf("launch vm worker: %w", err)
	}
	timings.GuestFirstOutput = waitFirstOutput(handle, time.Now(), firstOutputWaitMax)
	timings = timings.Sum()
	if autoNet != nil {
		if err := autoNet.Activate(); err != nil {
			handle.Stop()
			autoNet.Close()
			if cleanup != nil {
				cleanup()
			}
			return nil, fmt.Errorf("activate auto network: %w", err)
		}
	}
	// Auto-capture: if this was a cold boot and --warm / GOCRACKER_WARM_CACHE=1
	// Fire snapshot capture in background — VM is returned to the caller
	// immediately. The goroutine polls exec-ready (~150ms) then snapshots
	// dirty pages (~50ms). Caller MUST drain WarmDone before stopping the VM.
	var warmDone <-chan struct{}
	if warmCacheKey != "" && opts.SnapshotDir == "" {
		ch := make(chan struct{})
		warmDone = ch
		go func() {
			defer close(ch)
			captureWarmSnapshot(handle, opts, warmCacheKey)
		}()
	}
	var cleanupOnce sync.Once
	cleanupFn := cleanupRuntimeDisk
	if autoNet != nil || cleanup != nil {
		cleanupFn = func() {
			cleanupOnce.Do(func() {
				if autoNet != nil {
					autoNet.Close()
				}
				if cleanup != nil {
					cleanup()
				}
				cleanupRuntimeDisk()
			})
		}
		go func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_ = handle.WaitStopped(ctx)
			cleanupFn()
		}()
	}
	bootDuration := time.Since(t0).Round(time.Millisecond)
	gclog.Container.Info("started", "id", opts.ID,
		"duration", bootDuration,
		"orchestration_ms", timings.Orchestration.Milliseconds(),
		"vmm_setup_ms", timings.VMMSetup.Milliseconds(),
		"start_ms", timings.Start.Milliseconds(),
		"guest_first_output_ms", timings.GuestFirstOutput.Milliseconds())
	return &RunResult{
		VM:           handle,
		DiskPath:     bootDiskPath,
		ID:           opts.ID,
		Config:       imgConfig,
		TapName:      opts.TapName,
		GuestIP:      trimCIDR(opts.StaticIP),
		Gateway:      opts.Gateway,
		WorkerSocket: workerSocket(handle),
		Duration:     bootDuration,
		Timings:      timings,
		WarmDone:     warmDone,
		cleanup:      cleanupFn,
	}, nil
}

// BuildOptions describes a build-only operation (no boot).
type BuildOptions struct {
	Image        string
	Dockerfile   string
	Context      string
	RepoURL      string
	RepoRef      string
	RepoSubdir   string
	DiskSizeMB   int
	BuildArgs    map[string]string
	OutputPath   string
	WorkDir      string
	CacheDir     string
	JailerMode   string
	JailerBinary string
	WorkerBinary string
	ChrootBase   string
	UID          int
	GID          int
}

// BuildResult is returned after a build completes.
type BuildResult struct {
	RootfsDir string
	DiskPath  string
	Config    oci.ImageConfig
}

// Build creates a rootfs and disk image without booting a VM.
func Build(opts BuildOptions) (*BuildResult, error) {
	if jailerEnabled(opts.JailerMode) {
		return buildViaWorker(opts)
	}
	return buildLocal(opts)
}

func buildLocal(opts BuildOptions) (*BuildResult, error) {
	if opts.DiskSizeMB == 0 {
		opts.DiskSizeMB = 2048
	}
	if err := hostguard.CheckHostDevices(hostguard.DeviceRequirements{}); err != nil {
		return nil, fmt.Errorf("host device preflight: %w", err)
	}
	id := fmt.Sprintf("build-%d", time.Now().UnixNano()%100000)
	workDir := opts.WorkDir
	if workDir == "" {
		workDir = buildWorkDirForCache(opts, id)
	}
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return nil, fmt.Errorf("create workdir %s: %w", workDir, err)
	}

	rootfsDir := filepath.Join(workDir, "rootfs")
	diskPath := opts.OutputPath
	if diskPath == "" {
		diskPath = filepath.Join(workDir, "disk.ext4")
	}
	if err := os.MkdirAll(filepath.Dir(diskPath), 0755); err != nil {
		return nil, fmt.Errorf("create output dir %s: %w", filepath.Dir(diskPath), err)
	}
	if err := os.MkdirAll(rootfsDir, 0755); err != nil {
		return nil, fmt.Errorf("create rootfs %s: %w", rootfsDir, err)
	}

	runOpts := RunOptions{
		Image:      opts.Image,
		Dockerfile: opts.Dockerfile,
		Context:    opts.Context,
		RepoURL:    opts.RepoURL,
		RepoRef:    opts.RepoRef,
		RepoSubdir: opts.RepoSubdir,
		BuildArgs:  opts.BuildArgs,
		ID:         id,
		CacheDir:   opts.CacheDir,
	}
	if runOpts.RepoURL != "" {
		resolved, err := resolveRepo(runOpts, workDir)
		if err != nil {
			return nil, err
		}
		defer resolved.cleanup()
		runOpts.Dockerfile = resolved.dockerfile
		runOpts.Context = resolved.context
	}

	imgConfig, err := buildRootfs(rootfsDir, runOpts)
	if err != nil {
		return nil, err
	}

	injectHostCACerts(rootfsDir)
	if err := oci.BuildExt4(rootfsDir, diskPath, opts.DiskSizeMB); err != nil {
		return nil, fmt.Errorf("ext4: %w", err)
	}

	return &BuildResult{RootfsDir: rootfsDir, DiskPath: diskPath, Config: imgConfig}, nil
}

func buildViaWorker(opts BuildOptions) (*BuildResult, error) {
	if opts.DiskSizeMB == 0 {
		opts.DiskSizeMB = 2048
	}
	if err := hostguard.CheckHostDevices(hostguard.DeviceRequirements{}); err != nil {
		return nil, fmt.Errorf("host device preflight: %w", err)
	}
	id := fmt.Sprintf("build-%d", time.Now().UnixNano()%100000)
	workDir := opts.WorkDir
	if workDir == "" {
		workDir = buildWorkDirForCache(opts, id)
	}
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return nil, fmt.Errorf("create workdir %s: %w", workDir, err)
	}

	rootfsDir := filepath.Join(workDir, "rootfs")
	diskPath := opts.OutputPath
	if diskPath == "" {
		diskPath = filepath.Join(workDir, "disk.ext4")
	}
	if err := os.MkdirAll(filepath.Dir(diskPath), 0755); err != nil {
		return nil, fmt.Errorf("create output dir %s: %w", filepath.Dir(diskPath), err)
	}
	if err := os.RemoveAll(rootfsDir); err != nil {
		return nil, fmt.Errorf("reset rootfs %s: %w", rootfsDir, err)
	}
	if err := os.MkdirAll(rootfsDir, 0755); err != nil {
		return nil, fmt.Errorf("create rootfs %s: %w", rootfsDir, err)
	}

	runOpts := RunOptions{
		Image:        opts.Image,
		Dockerfile:   opts.Dockerfile,
		Context:      opts.Context,
		RepoURL:      opts.RepoURL,
		RepoRef:      opts.RepoRef,
		RepoSubdir:   opts.RepoSubdir,
		BuildArgs:    opts.BuildArgs,
		ID:           id,
		CacheDir:     opts.CacheDir,
		JailerMode:   opts.JailerMode,
		JailerBinary: opts.JailerBinary,
		VMMBinary:    opts.WorkerBinary,
		ChrootBase:   opts.ChrootBase,
		UID:          opts.UID,
		GID:          opts.GID,
	}
	if runOpts.RepoURL != "" {
		resolved, err := resolveRepo(runOpts, workDir)
		if err != nil {
			return nil, err
		}
		defer resolved.cleanup()
		runOpts.Dockerfile = resolved.dockerfile
		runOpts.Context = resolved.context
	}

	imgConfig, err := buildRootfsViaWorker(rootfsDir, runOpts)
	if err != nil {
		return nil, err
	}
	injectHostCACerts(rootfsDir)
	if err := oci.BuildExt4(rootfsDir, diskPath, opts.DiskSizeMB); err != nil {
		return nil, fmt.Errorf("ext4: %w", err)
	}
	return &BuildResult{RootfsDir: rootfsDir, DiskPath: diskPath, Config: imgConfig}, nil
}

func buildWorkDirForCache(opts BuildOptions, fallbackID string) string {
	opts.CacheDir = resolvedCacheDir(opts.CacheDir)
	key, err := stableHashKey(map[string]any{
		"image":       opts.Image,
		"dockerfile":  opts.Dockerfile,
		"context":     opts.Context,
		"repo_url":    opts.RepoURL,
		"repo_ref":    opts.RepoRef,
		"repo_subdir": opts.RepoSubdir,
		"build_args":  normalizedStringMap(opts.BuildArgs),
		"disk_size":   opts.DiskSizeMB,
	})
	if err != nil {
		return filepath.Join(os.TempDir(), "gocracker-"+fallbackID)
	}
	return filepath.Join(opts.CacheDir, "build-artifacts", key)
}

// ---- Repo resolution ----

type resolvedRepo struct {
	dockerfile string
	context    string
	cleanup    func()
}

func resolveRepo(opts RunOptions, workDir string) (*resolvedRepo, error) {
	result, err := repo.Resolve(repo.Source{
		URL:        opts.RepoURL,
		Ref:        opts.RepoRef,
		Subdir:     opts.RepoSubdir,
		Dockerfile: opts.RepoDockerfile,
	})
	if err != nil {
		return nil, fmt.Errorf("repo: %w", err)
	}
	result.Summary()

	if result.DockerfilePath == "" {
		result.Cleanup()
		return nil, fmt.Errorf("no Dockerfile found in %s", result.ContextDir)
	}

	return &resolvedRepo{
		dockerfile: result.DockerfilePath,
		context:    result.ContextDir,
		cleanup:    result.Cleanup,
	}, nil
}

// ---- Image/Dockerfile builders ----

func buildFromImage(rootfsDir string, opts RunOptions) (oci.ImageConfig, error) {
	pulled, err := oci.Pull(oci.PullOptions{
		Ref:      opts.Image,
		Arch:     opts.Arch,
		CacheDir: imageCacheDir(opts.CacheDir),
	})
	if err != nil {
		return oci.ImageConfig{}, err
	}
	return pulled.Config, pulled.ExtractToDir(rootfsDir)
}

func buildFromDockerfile(rootfsDir string, opts RunOptions) (oci.ImageConfig, error) {
	result, err := dockerfile.Build(dockerfile.BuildOptions{
		DockerfilePath: opts.Dockerfile,
		ContextDir:     opts.Context,
		BuildArgs:      opts.BuildArgs,
		OutputDir:      rootfsDir,
		Tag:            opts.ID,
		CacheDir:       resolvedCacheDir(opts.CacheDir),
	})
	if err != nil {
		return oci.ImageConfig{}, err
	}
	return result.Config, nil
}

func buildRootfs(rootfsDir string, opts RunOptions) (oci.ImageConfig, error) {
	var (
		imgConfig oci.ImageConfig
		err       error
	)
	switch {
	case opts.Image != "":
		imgConfig, err = buildFromImage(rootfsDir, opts)
	case opts.Dockerfile != "":
		imgConfig, err = buildFromDockerfile(rootfsDir, opts)
	default:
		return oci.ImageConfig{}, fmt.Errorf("specify --image, --dockerfile, or --repo")
	}
	if err != nil {
		return oci.ImageConfig{}, err
	}
	if err := applyMounts(rootfsDir, opts.Mounts); err != nil {
		return oci.ImageConfig{}, fmt.Errorf("apply mounts: %w", err)
	}
	return imgConfig, nil
}

func buildRootfsViaWorker(rootfsDir string, opts RunOptions) (oci.ImageConfig, error) {
	cacheDir := resolvedCacheDir(opts.CacheDir)
	resp, err := worker.BuildRootfs(buildserver.BuildRequest{
		Image:      opts.Image,
		Dockerfile: opts.Dockerfile,
		Context:    opts.Context,
		BuildArgs:  opts.BuildArgs,
		OutputDir:  rootfsDir,
		CacheDir:   cacheDir,
	}, worker.BuildOptions{
		JailerBinary: opts.JailerBinary,
		WorkerBinary: opts.VMMBinary,
		UID:          firstNonNegative(opts.UID, os.Getuid()),
		GID:          firstNonNegative(opts.GID, os.Getgid()),
		ChrootBase:   opts.ChrootBase,
	})
	if err != nil {
		return oci.ImageConfig{}, err
	}
	if err := applyMounts(rootfsDir, opts.Mounts); err != nil {
		return oci.ImageConfig{}, fmt.Errorf("apply mounts: %w", err)
	}
	return resp, nil
}

func writeImageConfig(path string, cfg oci.ImageConfig) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func writeRuntimeSpecToRootfs(rootfsDir string, spec runtimecfg.GuestSpec) error {
	if !spec.HasStructuredFields() {
		return nil
	}
	data, err := spec.MarshalJSONBytes()
	if err != nil {
		return err
	}
	hostPath := filepath.Join(rootfsDir, strings.TrimPrefix(runtimecfg.GuestSpecPath, "/"))
	if err := os.MkdirAll(filepath.Dir(hostPath), 0755); err != nil {
		return err
	}
	return os.WriteFile(hostPath, data, 0644)
}

// injectHostCACerts copies the host's CA certificate bundle into the guest
// rootfs so that TLS-dependent tools (apk, curl, wget) work out of the box.
func injectHostCACerts(rootfsDir string) {
	// Only inject if the rootfs doesn't already have CA certs (e.g. alpine:latest has them).
	guestCerts := filepath.Join(rootfsDir, "etc/ssl/certs/ca-certificates.crt")
	if info, err := os.Stat(guestCerts); err == nil && info.Size() > 0 {
		// rootfs already has certs, don't overwrite
	} else {
		const hostBundle = "/etc/ssl/certs/ca-certificates.crt"
		data, err := os.ReadFile(hostBundle)
		if err == nil {
			_ = os.MkdirAll(filepath.Dir(guestCerts), 0755)
			_ = os.WriteFile(guestCerts, data, 0644)
		}
	}
	// Ensure working DNS inside the guest.
	resolvPath := filepath.Join(rootfsDir, "etc/resolv.conf")
	_ = os.WriteFile(resolvPath, []byte("nameserver 1.1.1.1\nnameserver 8.8.8.8\n"), 0644)
}

func readImageConfig(path string) (oci.ImageConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return oci.ImageConfig{}, err
	}
	var cfg oci.ImageConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return oci.ImageConfig{}, err
	}
	return cfg, nil
}

func writeGuestSpecCache(path string, spec runtimecfg.GuestSpec) error {
	payload := struct {
		Version    int                  `json:"version"`
		InitDigest string               `json:"init_digest,omitempty"`
		Spec       runtimecfg.GuestSpec `json:"spec"`
	}{
		Version:    1,
		InitDigest: guest.EmbeddedInitDigest(),
		Spec:       spec,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func shouldReuseCachedInitrd(initrdPath, specPath string, spec runtimecfg.GuestSpec) (bool, string, error) {
	if _, err := os.Stat(initrdPath); err != nil {
		if os.IsNotExist(err) {
			return false, "initrd missing", nil
		}
		return false, "", err
	}
	match, reason, err := cachedGuestSpecMatches(specPath, spec)
	if err != nil {
		return false, "", err
	}
	if !match {
		if err := os.Remove(initrdPath); err != nil && !os.IsNotExist(err) {
			return false, "", err
		}
	}
	return match, reason, nil
}

func cachedGuestSpecMatches(path string, spec runtimecfg.GuestSpec) (bool, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, "runtime spec missing", nil
		}
		return false, "", err
	}
	var payload struct {
		Version    int                  `json:"version"`
		InitDigest string               `json:"init_digest,omitempty"`
		Spec       runtimecfg.GuestSpec `json:"spec"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return false, fmt.Sprintf("runtime spec unreadable: %v", err), nil
	}
	cached := payload.Spec
	if payload.InitDigest == "" {
		if err := json.Unmarshal(data, &cached); err != nil {
			return false, fmt.Sprintf("runtime spec unreadable: %v", err), nil
		}
		return false, "guest init version missing", nil
	}
	if payload.InitDigest != guest.EmbeddedInitDigest() {
		return false, "guest init version changed", nil
	}
	cachedData, err := cached.MarshalJSONBytes()
	if err != nil {
		return false, "", err
	}
	currentData, err := spec.MarshalJSONBytes()
	if err != nil {
		return false, "", err
	}
	if !bytes.Equal(cachedData, currentData) {
		return false, "runtime spec changed", nil
	}
	return true, "", nil
}

func inspectCachedRunArtifacts(diskPath, configPath string) (oci.ImageConfig, bool, string, error) {
	if _, err := os.Stat(diskPath); err != nil {
		if os.IsNotExist(err) {
			return oci.ImageConfig{}, false, "disk image missing", nil
		}
		return oci.ImageConfig{}, false, "", err
	}
	cfg, err := readImageConfig(configPath)
	if err == nil {
		return cfg, true, "", nil
	}
	if os.IsNotExist(err) {
		return oci.ImageConfig{}, false, "image config missing", nil
	}
	return oci.ImageConfig{}, false, fmt.Sprintf("image config unreadable: %v", err), nil
}

func prepareBootDisk(workDir, templateDiskPath, id string, overlay bool) (string, func(), error) {
	runtimeDir := filepath.Join(workDir, "runs", sanitizeRuntimePathComponent(id))
	if err := os.RemoveAll(runtimeDir); err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return "", nil, err
	}
	runtimeDiskPath := filepath.Join(runtimeDir, filepath.Base(templateDiskPath))
	if err := copyDiskImage(templateDiskPath, runtimeDiskPath, overlay); err != nil {
		return "", nil, err
	}
	return runtimeDiskPath, delayedRemoveAll(runtimeDir, runtimeDiskRetention), nil
}


func sanitizeRuntimePathComponent(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "vm"
	}
	sanitized := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, value)
	if sanitized == "" {
		return "vm"
	}
	return sanitized
}

// copyDiskImage provisions a per-VM disk at dst from the cached template at
// src: hardlink (if safe) → reflink → full copy.
//
// Hardlinks share the inode with src. When the guest mounts the block device
// read-write, a hardlinked runs/ file silently bleeds writes back into the
// cached template and contaminates every future VM spawned from it — stale
// /etc/shadow, lockfiles, leftover /marker files, etc. Hardlinks ARE safe
// when the guest mounts the rootfs read-only and layers tmpfs on top (the
// `--rootfs-overlay` opt-in path — see mountRootDisk() in internal/guest/init.go).
// Callers signal that safety by passing overlay=true.
func copyDiskImage(src, dst string, overlay bool) error {
	if overlay {
		// Hardlink is safe because the guest will never write to the
		// backing file — writes go to a tmpfs upper layer.
		if err := os.Link(src, dst); err == nil {
			return nil
		}
	}
	// Try reflink (CoW clone). FICLONE = 0x40049409 on Linux. Safe in both
	// modes because it produces independent inodes that only share data
	// pages until first write.
	if err := tryReflink(src, dst); err == nil {
		return nil
	}

	// Fallback: full copy.
	return copyDiskImageFull(src, dst)
}

// ficlone is the Linux ioctl number for FICLONE (copy-on-write clone).
const ficlone = 0x40049409

func tryReflink(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, out.Fd(), ficlone, in.Fd())
	if errno != 0 {
		out.Close()
		os.Remove(dst)
		return errno
	}
	return out.Close()
}

func copyDiskImageFull(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func delayedRemoveAll(path string, delay time.Duration) func() {
	if strings.TrimSpace(path) == "" {
		return func() {}
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if delay <= 0 {
				_ = os.RemoveAll(path)
				return
			}
			delaySeconds := int(delay / time.Second)
			if delay%time.Second != 0 {
				delaySeconds++
			}
			script := fmt.Sprintf("sleep %d; rm -rf -- %s", delaySeconds, shellQuote(path))
			cmd := exec.Command("sh", "-c", script)
			cmd.Stdout = io.Discard
			cmd.Stderr = io.Discard
			if err := cmd.Start(); err != nil {
				go func() {
					time.Sleep(delay)
					_ = os.RemoveAll(path)
				}()
				return
			}
			_ = cmd.Process.Release()
		})
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func removeCachedRunArtifacts(paths ...string) error {
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// ---- Cmdline builder ----

func buildCmdline(imgConfig oci.ImageConfig, opts RunOptions) string {
	sharedFS := resolveSharedFSMounts(opts.Mounts)
	kernelModules := appendVirtioFSKernelModule(append([]guest.KernelModule{}, opts.KernelModules...), sharedFS)
	return buildCmdlineWithPlan(opts, sharedFS, len(kernelModules) > 0)
}

func buildCmdlineWithPlan(opts RunOptions, sharedFS sharedFSPlan, allowKernelModules bool) string {
	parts := append(runtimecfg.DefaultKernelArgsForRuntime(true, allowKernelModules),
		"rw",
		"root=/dev/vda",
		"rootfstype=ext4",
	)

	// Network
	if opts.StaticIP != "" {
		parts = append(parts, "gc.ip="+opts.StaticIP)
		if opts.Gateway != "" {
			parts = append(parts, "gc.gw="+opts.Gateway)
		}
	} else if opts.TapName != "" {
		parts = append(parts, "gc.wait_network=1")
	}
	if hasMaterializedMounts(opts.Mounts) {
		parts = append(parts, "gc.fs_sync=1")
	}
	if opts.RootfsPersistent {
		// Tells guest init to mount /dev/vda read-write directly, no
		// overlay. Writes persist on the per-VM disk file after VM
		// shutdown, at the cost of a slower boot (see prepareBootDisk).
		parts = append(parts, "gc.rootfs_overlay=off")
	}

	// Working dir
	return strings.Join(parts, " ")
}

func buildGuestSpec(imgConfig oci.ImageConfig, opts RunOptions, sharedFS sharedFSPlan) runtimecfg.GuestSpec {
	entrypoint := effectiveSlice(opts.Entrypoint, imgConfig.Entrypoint)
	cmd := effectiveSlice(opts.Cmd, imgConfig.Cmd)
	workDir := opts.WorkDir
	if workDir == "" {
		workDir = imgConfig.WorkingDir
	}
	pid1Mode := opts.PID1Mode
	if pid1Mode == "" && (opts.ConsoleIn != nil || opts.InteractiveExec) {
		// Interactive console sessions need a supervised PID 1 so the guest
		// process gets a controlling TTY and exits cleanly when the shell ends.
		pid1Mode = runtimecfg.PID1ModeSupervised
	}

	env := append([]string{}, imgConfig.Env...)
	env = append(env, opts.Env...)

	process := runtimecfg.ResolveProcess(entrypoint, cmd)
	if opts.InteractiveExec {
		process = runtimecfg.Process{}
	}

	return runtimecfg.GuestSpec{
		Process:  process,
		Env:      env,
		Hosts:    append([]string{}, opts.Hosts...),
		SharedFS: append([]runtimecfg.SharedFSMount{}, sharedFS.Mounts...),
		WorkDir:  workDir,
		User:     imgConfig.User,
		PID1Mode: pid1Mode,
		Exec: runtimecfg.ExecConfig{
			Enabled:   opts.ExecEnabled || guestAgentRequired(opts.Balloon, opts.MemoryHotplug),
			VsockPort: runtimecfg.DefaultExecVsockPort,
		},
	}
}

func resolveRunWorkDir(opts RunOptions) (string, error) {
	if opts.WorkDir2 != "" {
		return opts.WorkDir2, nil
	}
	opts.CacheDir = resolvedCacheDir(opts.CacheDir)
	key, err := runArtifactCacheKey(opts)
	if err != nil {
		return "", fmt.Errorf("compute run cache key: %w", err)
	}
	return filepath.Join(opts.CacheDir, "artifacts", key), nil
}

func imageCacheDir(cacheDir string) string {
	base := resolvedCacheDir(cacheDir)
	return filepath.Join(base, "layers")
}

func runArtifactCacheKey(opts RunOptions) (string, error) {
	payload := map[string]any{
		"artifact_cache_version": runArtifactCacheVersion,
		"image":                  opts.Image,
		"arch":                   opts.Arch,
		"dockerfile":             opts.Dockerfile,
		"context":                opts.Context,
		"repo_url":               opts.RepoURL,
		"repo_ref":               opts.RepoRef,
		"repo_subdir":            opts.RepoSubdir,
		"build_args":             normalizedStringMap(opts.BuildArgs),
		"disk_size_mb":           opts.DiskSizeMB,
		"cmd":                    opts.Cmd,
		"entrypoint":             opts.Entrypoint,
		"env":                    opts.Env,
		"hosts":                  opts.Hosts,
		"workdir":                opts.WorkDir,
		"pid1_mode":              opts.PID1Mode,
		"mounts":                 opts.Mounts,
		"kernelModules":          opts.KernelModules,
		"metadata":               normalizedStringMap(opts.Metadata),
		"exec_enabled":           opts.ExecEnabled,
		"interactive_exec":       opts.InteractiveExec,
		"drives":                 opts.Drives,
		"balloon":                opts.Balloon,
		"memory_hotplug":         opts.MemoryHotplug,
	}
	return stableHashKey(payload)
}

func cloneBalloonConfig(cfg *vmm.BalloonConfig) *vmm.BalloonConfig {
	if cfg == nil {
		return nil
	}
	cloned := *cfg
	if len(cfg.SnapshotPages) > 0 {
		cloned.SnapshotPages = append([]uint32(nil), cfg.SnapshotPages...)
	}
	return &cloned
}

func cloneMemoryHotplugConfig(cfg *vmm.MemoryHotplugConfig) *vmm.MemoryHotplugConfig {
	if cfg == nil {
		return nil
	}
	cloned := *cfg
	return &cloned
}

func buildExecConfig(opts RunOptions) *vmm.ExecConfig {
	if !opts.ExecEnabled && !guestAgentRequired(opts.Balloon, opts.MemoryHotplug) {
		return nil
	}
	return &vmm.ExecConfig{
		Enabled:   true,
		VsockPort: runtimecfg.DefaultExecVsockPort,
	}
}

func guestAgentRequired(balloon *vmm.BalloonConfig, hotplug *vmm.MemoryHotplugConfig) bool {
	return balloonNeedsGuestAgent(balloon) || hotplugNeedsGuestAgent(hotplug)
}

func balloonNeedsGuestAgent(cfg *vmm.BalloonConfig) bool {
	return cfg != nil && (cfg.StatsPollingIntervalS > 0 || cfg.Auto == vmm.BalloonAutoConservative)
}

func hotplugNeedsGuestAgent(cfg *vmm.MemoryHotplugConfig) bool {
	return cfg != nil
}

func runtimeDrives(rootDisk string, opts RunOptions) []vmm.DriveConfig {
	if len(opts.Drives) == 0 {
		return nil
	}
	drives := []vmm.DriveConfig{{
		ID:       "root",
		Path:     rootDisk,
		Root:     true,
		ReadOnly: false,
	}}
	for _, drive := range opts.Drives {
		drives = append(drives, vmm.DriveConfig{
			ID:          drive.ID,
			Path:        drive.Path,
			Root:        false,
			ReadOnly:    drive.ReadOnly,
			RateLimiter: cloneVMLimiter(drive.RateLimiter),
		})
	}
	return drives
}

func cloneVMLimiter(cfg *vmm.RateLimiterConfig) *vmm.RateLimiterConfig {
	if cfg == nil {
		return nil
	}
	cloned := *cfg
	return &cloned
}

func stableHashKey(payload any) (string, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func normalizedStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for _, key := range sortedKeys(src) {
		dst[key] = src[key]
	}
	return dst
}

func sortedKeys(src map[string]string) []string {
	keys := make([]string, 0, len(src))
	for key := range src {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func effectiveSlice(override, fallback []string) []string {
	if len(override) > 0 {
		return append([]string{}, override...)
	}
	return append([]string{}, fallback...)
}

type sharedFSPlan struct {
	Exports []vmm.SharedFSConfig
	Mounts  []runtimecfg.SharedFSMount
}

func hasMaterializedMounts(mounts []Mount) bool {
	for _, mount := range mounts {
		if mount.Backend != MountBackendVirtioFS {
			return true
		}
	}
	return false
}

func resolveSharedFSMounts(mounts []Mount) sharedFSPlan {
	if len(mounts) == 0 {
		return sharedFSPlan{}
	}
	plan := sharedFSPlan{}
	seen := map[string]string{}
	for _, mount := range mounts {
		if mount.Backend != MountBackendVirtioFS {
			continue
		}
		source := filepath.Clean(mount.Source)
		tag, ok := seen[source]
		if !ok {
			tag = fmt.Sprintf("gocracker-fs-%d", len(seen))
			seen[source] = tag
			plan.Exports = append(plan.Exports, vmm.SharedFSConfig{
				Source: source,
				Tag:    tag,
				Target: mount.Target,
			})
		}
		plan.Mounts = append(plan.Mounts, runtimecfg.SharedFSMount{
			Tag:      tag,
			Target:   mount.Target,
			ReadOnly: mount.ReadOnly,
		})
	}
	return plan
}

func appendVirtioFSKernelModule(kernelModules []guest.KernelModule, sharedFS sharedFSPlan) []guest.KernelModule {
	if len(sharedFS.Exports) == 0 || hasKernelModule(kernelModules, "virtiofs") {
		return kernelModules
	}
	hostPath := hostVirtioFSModulePath()
	if hostPath == "" {
		return kernelModules
	}
	return append(kernelModules, guest.KernelModule{
		Name:     "virtiofs",
		HostPath: hostPath,
	})
}

func hasKernelModule(modules []guest.KernelModule, name string) bool {
	for _, module := range modules {
		if module.Name == name {
			return true
		}
	}
	return false
}

var (
	hostVirtioFSModuleOnce sync.Once
	hostVirtioFSModuleVal  string
)

func hostVirtioFSModulePath() string {
	hostVirtioFSModuleOnce.Do(func() {
		release, err := os.ReadFile("/proc/sys/kernel/osrelease")
		if err != nil {
			return
		}
		base := strings.TrimSpace(string(release))
		if base == "" {
			return
		}
		matches, err := filepath.Glob(filepath.Join("/lib/modules", base, "kernel", "fs", "fuse", "virtiofs.ko*"))
		if err != nil || len(matches) == 0 {
			return
		}
		hostVirtioFSModuleVal = matches[0]
	})
	return hostVirtioFSModuleVal
}

func firstNonNegative(values ...int) int {
	for _, v := range values {
		if v >= 0 {
			return v
		}
	}
	return 0
}

// validateRestoreMounts enforces the sandbox-template contract: restore
// cannot materialize rootfs content (that would defeat the fast-path), so
// only virtiofs mounts are accepted. Materialized mounts return a clear
// error the API layer surfaces as 400.
func validateRestoreMounts(mounts []Mount) error {
	for _, m := range mounts {
		if m.Backend != MountBackendVirtioFS {
			return fmt.Errorf("snapshot restore accepts only virtiofs mounts; got backend=%q for target %q", m.Backend, m.Target)
		}
	}
	return nil
}

// buildSharedFSRebinds converts the API-level virtiofs mounts into the
// tag-agnostic rebind records the vmm restore path consumes. The Target
// string is what the template mounted the virtiofs tag at; the vmm layer
// matches rebinds against snap.Config.SharedFS[i].Target.
func buildSharedFSRebinds(mounts []Mount) []vmm.SharedFSRebind {
	if len(mounts) == 0 {
		return nil
	}
	rebinds := make([]vmm.SharedFSRebind, 0, len(mounts))
	for _, m := range mounts {
		if m.Backend != MountBackendVirtioFS {
			continue
		}
		rebinds = append(rebinds, vmm.SharedFSRebind{
			Target: m.Target,
			Source: filepath.Clean(m.Source),
		})
	}
	return rebinds
}

// reIPGuest reconfigures eth0 inside a just-restored guest so it uses the
// new host TAP's CIDR and gateway instead of the snapshot's frozen addresses.
// Without this, warm restores with --net auto have no internet connectivity
// because the guest routes to the old gateway that no longer exists on the
// new TAP interface.
func reIPGuest(handle vmm.Handle, newCIDR, newGateway string, timeout time.Duration) error {
	if newCIDR == "" || newGateway == "" {
		return nil
	}
	dialer, ok := handle.(vmm.VsockDialer)
	if !ok {
		return nil
	}
	cfg := handle.VMConfig()
	if cfg.Exec == nil || !cfg.Exec.Enabled {
		return nil
	}
	port := uint32(guestexec.DefaultVsockPort)
	if cfg.Exec.VsockPort != 0 {
		port = cfg.Exec.VsockPort
	}
	deadline := time.Now().Add(timeout)
	var dialErr error
	var conn interface {
		io.ReadWriter
		Close() error
	}
	for time.Now().Before(deadline) {
		c, err := dialer.DialVsock(port)
		if err == nil {
			conn = c
			break
		}
		dialErr = err
		time.Sleep(10 * time.Millisecond)
	}
	if conn == nil {
		return fmt.Errorf("exec agent not ready: %w", dialErr)
	}
	defer conn.Close()
	// `ip route add default` fails with "File exists" when the guest was
	// restored from a snapshot whose rootfs already configured a default
	// route — use `route replace` so the call is idempotent.
	script := fmt.Sprintf(
		"ip addr flush dev eth0 && ip addr add %s dev eth0 && ip route replace default via %s dev eth0",
		newCIDR, newGateway)
	req := guestexec.Request{Mode: guestexec.ModeExec, Command: []string{"/bin/sh", "-c", script}}
	if err := guestexec.Encode(conn, req); err != nil {
		return fmt.Errorf("re-IP encode: %w", err)
	}
	var resp guestexec.Response
	if err := guestexec.Decode(conn, &resp); err != nil {
		return fmt.Errorf("re-IP decode: %w", err)
	}
	if resp.Error != "" {
		return fmt.Errorf("re-IP guest: %s", resp.Error)
	}
	return nil
}
