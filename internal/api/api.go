// Package api implements the gocracker REST API.
// Core endpoints are Firecracker-compatible; extended endpoints add
// OCI/Dockerfile build, snapshot, and multi-VM management.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gocracker/gocracker/internal/firecrackerapi"
	"github.com/gocracker/gocracker/internal/guestexec"
	gclog "github.com/gocracker/gocracker/internal/log"
	"github.com/gocracker/gocracker/internal/stacknet"
	"github.com/gocracker/gocracker/pkg/container"
	"github.com/gocracker/gocracker/pkg/vmm"
)

// Server is the gocracker API server. Manages multiple VM instances.
type Server struct {
	mu                  sync.RWMutex
	vms                 map[string]*vmEntry
	vmDirs              map[string]string
	migrationSessions   map[string]string
	router              chi.Router
	defaultX86Boot      vmm.X86BootMode
	jailerMode          string
	jailerBinary        string
	vmmBinary           string
	chrootBaseDir       string
	stateDir            string
	cacheDir            string
	uid                 int
	gid                 int
	rootVMID            string
	composeStacks       map[string]*composeStack
	runFn               func(container.RunOptions) (*container.RunResult, error)
	buildFn             func(container.BuildOptions) (*container.BuildResult, error)
	launchVMMFn         func(vmm.Config) (vmm.Handle, func(), error)
	restoreVMMFn        func(string, vmm.RestoreOptions) (vmm.Handle, func(), error)
	reattachVMMFn       func(vmm.Config, vmm.WorkerMetadata) (vmm.Handle, func(), error)
	authToken           string
	trustedKernelDirs   []string
	trustedWorkDirs     []string
	trustedSnapshotDirs []string

	// Firecracker single-VM pre-boot config
	preboot prebootConfig
}

type Options struct {
	DefaultX86Boot      vmm.X86BootMode
	JailerMode          string
	JailerBinary        string
	VMMBinary           string
	ChrootBaseDir       string
	StateDir            string
	CacheDir            string
	UID                 int
	GID                 int
	RunFn               func(container.RunOptions) (*container.RunResult, error)
	BuildFn             func(container.BuildOptions) (*container.BuildResult, error)
	LaunchVMMFn         func(vmm.Config) (vmm.Handle, func(), error)
	RestoreVMMFn        func(string, vmm.RestoreOptions) (vmm.Handle, func(), error)
	ReattachVMMFn       func(vmm.Config, vmm.WorkerMetadata) (vmm.Handle, func(), error)
	AuthToken           string
	TrustedKernelDirs   []string
	TrustedWorkDirs     []string
	TrustedSnapshotDirs []string
}

type prebootConfig struct {
	bootSource                *BootSource
	machineConf               *MachineConfig
	balloon                   *Balloon
	memoryHotplug             *vmm.MemoryHotplugConfig
	memoryHotplugRequestedMiB uint64
	drives                    []Drive
	netIfaces                 []NetworkInterface
}

// ---- Request/response types (Firecracker-compatible) ----

type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
	InitrdPath      string `json:"initrd_path,omitempty"`
	X86Boot         string `json:"x86_boot,omitempty"`
}

type MachineConfig struct {
	VcpuCount      int                    `json:"vcpu_count"`
	MemSizeMib     int                    `json:"mem_size_mib"`
	RNGRateLimiter *vmm.RateLimiterConfig `json:"rng_rate_limiter,omitempty"`
}

type Balloon struct {
	AmountMib             uint64 `json:"amount_mib"`
	DeflateOnOOM          bool   `json:"deflate_on_oom"`
	StatsPollingIntervalS int    `json:"stats_polling_interval_s,omitempty"`
	FreePageHinting       bool   `json:"free_page_hinting,omitempty"`
	FreePageReporting     bool   `json:"free_page_reporting,omitempty"`
}

type BalloonUpdate struct {
	AmountMib uint64 `json:"amount_mib"`
}

type BalloonStatsUpdate struct {
	StatsPollingIntervalS int `json:"stats_polling_interval_s"`
}

type MemoryHotplugConfig = vmm.MemoryHotplugConfig
type MemoryHotplugSizeUpdate = vmm.MemoryHotplugSizeUpdate
type MemoryHotplugStatus = vmm.MemoryHotplugStatus

type Drive struct {
	DriveID      string                 `json:"drive_id"`
	PathOnHost   string                 `json:"path_on_host"`
	IsRootDevice bool                   `json:"is_root_device"`
	IsReadOnly   bool                   `json:"is_read_only"`
	RateLimiter  *vmm.RateLimiterConfig `json:"rate_limiter,omitempty"`
}

type NetworkInterface struct {
	IfaceID       string                 `json:"iface_id"`
	HostDevName   string                 `json:"host_dev_name"`
	GuestMAC      string                 `json:"guest_mac,omitempty"`
	RateLimiter   *vmm.RateLimiterConfig `json:"rate_limiter,omitempty"`     // legacy: applied to both RX and TX
	RxRateLimiter *vmm.RateLimiterConfig `json:"rx_rate_limiter,omitempty"` // Firecracker-parity: host→guest only
	TxRateLimiter *vmm.RateLimiterConfig `json:"tx_rate_limiter,omitempty"` // Firecracker-parity: guest→host only
}

type Action struct {
	ActionType string `json:"action_type"`
}

type InstanceInfo struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	AppName    string `json:"app_name"`
	VMMVersion string `json:"vmm_version"`
	HostArch   string `json:"host_arch,omitempty"`
}

// ---- Extended types ----

type RunRequest struct {
	// Source: one of Image or Dockerfile
	Image      string `json:"image,omitempty"`
	Dockerfile string `json:"dockerfile,omitempty"`
	Context    string `json:"context,omitempty"`

	// VM
	VcpuCount  int    `json:"vcpu_count,omitempty"`
	MemMB      uint64 `json:"mem_mb,omitempty"`
	Arch       string `json:"arch,omitempty"`
	KernelPath string `json:"kernel_path"`
	TapName    string `json:"tap_name,omitempty"`
	X86Boot    string `json:"x86_boot,omitempty"`

	// Container
	Cmd        []string          `json:"cmd,omitempty"`
	Entrypoint []string          `json:"entrypoint,omitempty"`
	Env        []string          `json:"env,omitempty"`
	Hosts      []string          `json:"hosts,omitempty"`
	WorkDir    string            `json:"workdir,omitempty"`
	PID1Mode   string            `json:"pid1_mode,omitempty"`
	BuildArgs  map[string]string `json:"build_args,omitempty"`
	DiskSizeMB int               `json:"disk_size_mb,omitempty"`
	Mounts     []container.Mount `json:"mounts,omitempty"`
	Drives     []Drive           `json:"drives,omitempty"`

	// Snapshot to restore from (optional)
	SnapshotDir   string                   `json:"snapshot_dir,omitempty"`
	StaticIP      string                   `json:"static_ip,omitempty"`
	Gateway       string                   `json:"gateway,omitempty"`
	CacheDir      string                   `json:"cache_dir,omitempty"`
	Metadata      map[string]string        `json:"metadata,omitempty"`
	ExecEnabled   bool                     `json:"exec_enabled,omitempty"`
	Balloon       *Balloon                 `json:"balloon,omitempty"`
	MemoryHotplug *vmm.MemoryHotplugConfig `json:"memory_hotplug,omitempty"`

	// NetworkMode selects how gocracker provisions the guest NIC. "" or
	// "none" keeps today's explicit behaviour (caller supplies tap_name /
	// static_ip / gateway). "auto" makes the server allocate a fresh TAP
	// + /30 guest/gateway pair via hostnet.AutoNetwork. When "auto" is set
	// the caller MUST leave static_ip and gateway empty.
	NetworkMode string `json:"network_mode,omitempty"`

	// Wait, when true (or when the "wait=true" query parameter is set),
	// makes the /run handler run synchronously: it performs the full
	// runFn + vm.Start() inline and only returns once the vCPUs are
	// spinning (state="running"). For snapshot_dir this is typically
	// single-digit ms; for a fresh boot this blocks for the kernel boot
	// duration. When false (default), the handler launches a goroutine and
	// returns state="starting" immediately.
	Wait bool `json:"wait,omitempty"`
}

type RunResponse struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Message string `json:"message,omitempty"`

	// Network fields echo the effective tap/ip/gateway for the VM. They
	// are populated only when the handler ran synchronously (wait=true
	// or the snapshot-restore fast path) and the network was actually
	// resolved. On async state="starting" these fields are empty; the
	// caller should poll GET /vms/{id} for the live values.
	TapName              string `json:"tap_name,omitempty"`
	GuestIP              string `json:"guest_ip,omitempty"`
	Gateway              string `json:"gateway,omitempty"`
	NetworkMode          string `json:"network_mode,omitempty"`
	RestoredFromSnapshot bool   `json:"restored_from_snapshot,omitempty"`
}

type SnapshotRequest struct {
	DestDir string `json:"dest_dir"`
}

// CloneRequest clones a running VM in-place (no second gocracker serve
// required). The server snapshots the source to a scratch dir, restores as a
// new VM with a fresh ID, and applies the per-instance overrides below. The
// source VM is unaffected (it keeps running). Intended for sandbox-template
// warm pools: publish one template VM, clone it per sandbox.
type CloneRequest struct {
	// Optional: where to drop the intermediate snapshot. If empty, the
	// server uses a temp dir and deletes it after the clone completes.
	// Set this if you want to publish the snapshot as a durable template
	// for later /run snapshot_dir=… calls.
	SnapshotDir string `json:"snapshot_dir,omitempty"`
	// TapName / StaticIP / Gateway override the restored VM's network.
	// Mutually exclusive with NetworkMode=auto.
	TapName     string `json:"tap_name,omitempty"`
	StaticIP    string `json:"static_ip,omitempty"`
	Gateway     string `json:"gateway,omitempty"`
	NetworkMode string `json:"network_mode,omitempty"`
	// Mounts: reserved for future virtio-fs rebind once FUSE-session
	// migration is available. Today /clone rejects VMs with live
	// virtio-fs mounts at the API (400) to avoid silent hangs, so this
	// field is validated but not acted on.
	Mounts []container.Mount `json:"mounts,omitempty"`
	// ExecEnabled for the clone (the source's exec is not forwarded).
	ExecEnabled bool `json:"exec_enabled,omitempty"`
	// Metadata merged onto the clone's VMInfo.
	Metadata map[string]string `json:"metadata,omitempty"`
}

type VMInfo struct {
	ID          string            `json:"id"`
	State       string            `json:"state"`
	Uptime      string            `json:"uptime"`
	MemMB       uint64            `json:"mem_mb"`
	Arch        string            `json:"arch,omitempty"`
	Kernel      string            `json:"kernel"`
	Events      []vmm.Event       `json:"events"`
	Devices     []vmm.DeviceInfo  `json:"devices,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	TapName     string            `json:"tap_name,omitempty"`
	GuestIP     string            `json:"guest_ip,omitempty"`
	Gateway     string            `json:"gateway,omitempty"`
	NetworkMode string            `json:"network_mode,omitempty"`
}

type APIError struct {
	FaultMessage string `json:"fault_message"`
}

type ExecRequest struct {
	Command []string `json:"command,omitempty"`
	Columns int      `json:"columns,omitempty"`
	Rows    int      `json:"rows,omitempty"`
	// Stdin is fed to the guest process on stdin (one-shot exec only).
	Stdin string `json:"stdin,omitempty"`
	// Env is appended to the guest process environment as KEY=VALUE entries.
	Env []string `json:"env,omitempty"`
	// WorkDir overrides the guest working directory for this single exec.
	WorkDir string `json:"workdir,omitempty"`
}

type ExecResponse struct {
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode int    `json:"exit_code"`
}

type vmEntry struct {
	apiID         string
	handle        vmm.Handle
	local         *vmm.VM
	kind          string
	metadata      map[string]string
	pendingCfg    vmm.Config
	pendingState  string
	pendingEvents []vmm.Event
	socketPath    string
	workerPID     int
	jailRoot      string
	runDir        string
	bundleDir     string
	isRoot        bool
	createdAt     time.Time
	lastEventAt   time.Time
	cleanup       func()
}

type composeStack struct {
	network *stacknet.Manager
	vmIDs   map[string]struct{}
}

type rateLimiterUpdater interface {
	UpdateNetRateLimiter(*vmm.RateLimiterConfig) error
	UpdateNetRateLimiters(rx, tx *vmm.RateLimiterConfig) error
	UpdateBlockRateLimiter(*vmm.RateLimiterConfig) error
	UpdateRNGRateLimiter(*vmm.RateLimiterConfig) error
}

type balloonUpdater interface {
	vmm.BalloonController
}

type memoryHotplugUpdater interface {
	vmm.MemoryHotplugController
}

// New creates and configures the API server.
func New() *Server {
	return NewWithOptions(Options{})
}

func NewWithOptions(opts Options) *Server {
	mode, err := normalizeX86BootMode(opts.DefaultX86Boot, "")
	if err != nil {
		mode = vmm.X86BootAuto
	}
	s := &Server{
		vms:                 make(map[string]*vmEntry),
		vmDirs:              make(map[string]string),
		migrationSessions:   make(map[string]string),
		composeStacks:       make(map[string]*composeStack),
		defaultX86Boot:      mode,
		jailerMode:          opts.JailerMode,
		jailerBinary:        opts.JailerBinary,
		vmmBinary:           opts.VMMBinary,
		chrootBaseDir:       opts.ChrootBaseDir,
		stateDir:            opts.StateDir,
		cacheDir:            opts.CacheDir,
		uid:                 opts.UID,
		gid:                 opts.GID,
		runFn:               opts.RunFn,
		buildFn:             opts.BuildFn,
		launchVMMFn:         opts.LaunchVMMFn,
		restoreVMMFn:        opts.RestoreVMMFn,
		reattachVMMFn:       opts.ReattachVMMFn,
		authToken:           strings.TrimSpace(opts.AuthToken),
		trustedKernelDirs:   normalizeTrustedDirs(opts.TrustedKernelDirs),
		trustedWorkDirs:     normalizeTrustedDirs(opts.TrustedWorkDirs),
		trustedSnapshotDirs: normalizeTrustedDirs(opts.TrustedSnapshotDirs),
	}
	if s.runFn == nil {
		s.runFn = container.Run
	}
	if s.buildFn == nil {
		s.buildFn = container.Build
	}
	r := chi.NewRouter()
	r.Use(gclog.AccessLogMiddleware("api"))
	r.Use(middleware.Recoverer)
	r.Use(s.authMiddleware)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Server", "gocracker/0.2.0")
			next.ServeHTTP(w, r)
		})
	})

	// ---- Firecracker-compatible endpoints ----
	r.Get("/", s.handleInstanceInfo)
	r.Put("/boot-source", s.handleBootSource)
	r.Put("/machine-config", s.handleMachineConfig)
	r.Get("/balloon", s.handleBalloonGet)
	r.Put("/balloon", s.handleBalloonPut)
	r.Patch("/balloon", s.handleBalloonPatch)
	r.Get("/balloon/statistics", s.handleBalloonStatsGet)
	r.Patch("/balloon/statistics", s.handleBalloonStatsPatch)
	r.Get("/hotplug/memory", s.handleMemoryHotplugGet)
	r.Put("/hotplug/memory", s.handleMemoryHotplugPut)
	r.Patch("/hotplug/memory", s.handleMemoryHotplugPatch)
	r.Put("/drives/{drive_id}", s.handleDrive)
	r.Put("/network-interfaces/{iface_id}", s.handleNetworkIface)
	r.Put("/actions", s.handleAction)

	// ---- Extended endpoints ----

	// POST /run — build + boot in one call
	r.Post("/run", s.handleRun)

	// GET  /vms                    — list all VMs
	// GET  /vms/{id}              — VM info with events + devices
	// POST /vms/{id}/stop         — stop VM
	// POST /vms/{id}/migrate      — live-migrate VM to another gocracker API server
	// POST /vms/{id}/snapshot     — take snapshot
	// GET  /vms/{id}/events       — polling event log (?since=RFC3339)
	// GET  /vms/{id}/events/stream — SSE event stream
	// GET  /vms/{id}/logs         — UART console output
	r.Get("/vms", s.handleListVMs)
	r.Get("/vms/{id}", s.handleGetVM)
	r.Post("/vms/{id}/stop", s.handleStopVM)
	r.Post("/vms/{id}/pause", s.handleVMPause)
	r.Post("/vms/{id}/resume", s.handleVMResume)
	r.Post("/vms/{id}/clone", s.handleVMClone)
	r.Post("/vms/{id}/migrate", s.handleMigrateVM)
	r.Post("/vms/{id}/snapshot", s.handleSnapshot)
	r.Get("/vms/{id}/events", s.handleVMEvents)
	r.Get("/vms/{id}/events/stream", s.handleVMEventsStream)
	r.Get("/vms/{id}/logs", s.handleVMLogs)
	r.Post("/vms/{id}/exec", s.handleVMExec)
	r.Post("/vms/{id}/exec/stream", s.handleVMExecStream)
	r.Get("/vms/{id}/vsock/connect", s.handleVMVsockConnect)
	r.Put("/vms/{id}/rate-limiters/net", s.handleVMNetRateLimiter)
	r.Put("/vms/{id}/rate-limiters/block", s.handleVMBlockRateLimiter)
	r.Put("/vms/{id}/rate-limiters/rng", s.handleVMRNGRateLimiter)
	r.Post("/migrations/load", s.handleMigrationLoad)
	r.Post("/migrations/prepare", s.handleMigrationPrepare)
	r.Post("/migrations/finalize", s.handleMigrationFinalize)
	r.Post("/migrations/abort", s.handleMigrationAbort)

	// POST /build — build image only (no boot)
	r.Post("/build", s.handleBuild)

	s.router = r

	// Periodic cleanup of stopped VMs
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			s.cleanStopped()
		}
	}()

	s.loadPersistedWorkers()

	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

func (s *Server) ListenAndServe(addr string) error {
	gclog.API.Info("listening", "addr", addr)
	return http.ListenAndServe(addr, s)
}

func (s *Server) ListenUnix(path string) error {
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	gclog.API.Info("unix socket", "path", path)
	return http.Serve(ln, s)
}

// ---- Firecracker-compatible handlers ----

func (s *Server) handleInstanceInfo(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	running := 0
	for _, v := range s.vms {
		if v == nil || v.handle == nil {
			continue
		}
		if v.handle.State() == vmm.StateRunning {
			running++
		}
	}
	s.mu.RUnlock()
	json.NewEncoder(w).Encode(InstanceInfo{
		ID:         "gocracker-0",
		State:      fmt.Sprintf("running=%d", running),
		AppName:    "gocracker",
		VMMVersion: "0.2.0",
		HostArch:   runtime.GOARCH,
	})
}

func (s *Server) handleBootSource(w http.ResponseWriter, r *http.Request) {
	if err := s.rejectIfRootStarted(); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, 409), err.Error())
		return
	}
	var v BootSource
	if err := decodeJSONStrict(r, &v); err != nil {
		apiErr(w, 400, err.Error())
		return
	}
	if err := firecrackerapi.ValidateBootSource(firecrackerapi.BootSource{
		KernelImagePath: v.KernelImagePath,
		BootArgs:        v.BootArgs,
		InitrdPath:      v.InitrdPath,
		X86Boot:         v.X86Boot,
	}); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, 400), err.Error())
		return
	}
	if err := s.validateKernelPathForServer(v.KernelImagePath); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(v.InitrdPath) != "" {
		if err := s.validateWorkPathForServer(v.InitrdPath, "initrd_path"); err != nil {
			apiErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	s.mu.Lock()
	s.preboot.bootSource = &v
	s.mu.Unlock()
	w.WriteHeader(204)
}

func (s *Server) handleMachineConfig(w http.ResponseWriter, r *http.Request) {
	if err := s.rejectIfRootStarted(); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, 409), err.Error())
		return
	}
	var v MachineConfig
	if err := decodeJSONStrict(r, &v); err != nil {
		apiErr(w, 400, err.Error())
		return
	}
	if err := firecrackerapi.ValidateMachineConfig(firecrackerapi.MachineConfig{
		VcpuCount:  v.VcpuCount,
		MemSizeMib: v.MemSizeMib,
	}); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, 400), err.Error())
		return
	}
	s.mu.Lock()
	s.preboot.machineConf = &v
	s.mu.Unlock()
	w.WriteHeader(204)
}

func (s *Server) handleBalloonGet(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	rootID := s.rootVMID
	prebootBalloon := s.preboot.balloon
	entry := s.vms[rootID]
	s.mu.RUnlock()
	if rootID == "" {
		if prebootBalloon == nil {
			apiErr(w, http.StatusBadRequest, "balloon is not configured")
			return
		}
		_ = json.NewEncoder(w).Encode(prebootBalloon)
		return
	}
	if entry == nil || entry.isPending() {
		apiErr(w, http.StatusConflict, "instance is still starting")
		return
	}
	controller, ok := entry.handle.(balloonUpdater)
	if !ok {
		apiErr(w, http.StatusConflict, "virtio-balloon is not configured")
		return
	}
	cfg, err := controller.GetBalloonConfig()
	if err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = json.NewEncoder(w).Encode(Balloon{
		AmountMib:             cfg.AmountMiB,
		DeflateOnOOM:          cfg.DeflateOnOOM,
		StatsPollingIntervalS: cfg.StatsPollingIntervalS,
	})
}

func (s *Server) handleBalloonPut(w http.ResponseWriter, r *http.Request) {
	if err := s.rejectIfRootStarted(); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, 409), err.Error())
		return
	}
	var v Balloon
	if err := decodeJSONStrict(r, &v); err != nil {
		apiErr(w, 400, err.Error())
		return
	}
	if err := firecrackerapi.ValidateBalloon(firecrackerapi.Balloon{
		AmountMib:             v.AmountMib,
		DeflateOnOOM:          v.DeflateOnOOM,
		StatsPollingIntervalS: v.StatsPollingIntervalS,
		FreePageHinting:       v.FreePageHinting,
		FreePageReporting:     v.FreePageReporting,
	}); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, 400), err.Error())
		return
	}
	s.mu.Lock()
	s.preboot.balloon = &v
	s.mu.Unlock()
	w.WriteHeader(204)
}

func (s *Server) handleBalloonPatch(w http.ResponseWriter, r *http.Request) {
	var v BalloonUpdate
	if err := decodeJSONStrict(r, &v); err != nil {
		apiErr(w, 400, err.Error())
		return
	}
	if err := firecrackerapi.ValidateBalloonUpdate(firecrackerapi.BalloonUpdate{AmountMib: v.AmountMib}); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, 400), err.Error())
		return
	}
	s.mu.Lock()
	if s.rootVMID == "" {
		if s.preboot.balloon == nil {
			s.mu.Unlock()
			apiErr(w, http.StatusBadRequest, "balloon is not configured")
			return
		}
		s.preboot.balloon.AmountMib = v.AmountMib
		s.mu.Unlock()
		w.WriteHeader(204)
		return
	}
	entry := s.vms[s.rootVMID]
	s.mu.Unlock()
	if entry == nil || entry.isPending() {
		apiErr(w, http.StatusConflict, "instance is still starting")
		return
	}
	controller, ok := entry.handle.(balloonUpdater)
	if !ok {
		apiErr(w, http.StatusConflict, "virtio-balloon is not configured")
		return
	}
	if err := controller.UpdateBalloon(vmm.BalloonUpdate{AmountMiB: v.AmountMib}); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(204)
}

func (s *Server) handleBalloonStatsGet(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	rootID := s.rootVMID
	prebootBalloon := s.preboot.balloon
	entry := s.vms[rootID]
	s.mu.RUnlock()
	if rootID == "" {
		if prebootBalloon == nil || prebootBalloon.StatsPollingIntervalS == 0 {
			apiErr(w, http.StatusBadRequest, "balloon statistics are not enabled")
			return
		}
		_ = json.NewEncoder(w).Encode(vmm.BalloonStats{
			TargetPages: uint64(prebootBalloon.AmountMib) * 256,
			ActualPages: 0,
			TargetMiB:   prebootBalloon.AmountMib,
			ActualMiB:   0,
		})
		return
	}
	if entry == nil || entry.isPending() {
		apiErr(w, http.StatusConflict, "instance is still starting")
		return
	}
	controller, ok := entry.handle.(balloonUpdater)
	if !ok {
		apiErr(w, http.StatusConflict, "virtio-balloon is not configured")
		return
	}
	stats, err := controller.GetBalloonStats()
	if err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = json.NewEncoder(w).Encode(stats)
}

func (s *Server) handleBalloonStatsPatch(w http.ResponseWriter, r *http.Request) {
	var v BalloonStatsUpdate
	if err := decodeJSONStrict(r, &v); err != nil {
		apiErr(w, 400, err.Error())
		return
	}
	if err := firecrackerapi.ValidateBalloonStatsUpdate(firecrackerapi.BalloonStatsUpdate{
		StatsPollingIntervalS: v.StatsPollingIntervalS,
	}); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, 400), err.Error())
		return
	}
	s.mu.Lock()
	if s.rootVMID == "" {
		if s.preboot.balloon == nil {
			s.mu.Unlock()
			apiErr(w, http.StatusBadRequest, "balloon is not configured")
			return
		}
		s.preboot.balloon.StatsPollingIntervalS = v.StatsPollingIntervalS
		s.mu.Unlock()
		w.WriteHeader(204)
		return
	}
	entry := s.vms[s.rootVMID]
	s.mu.Unlock()
	if entry == nil || entry.isPending() {
		apiErr(w, http.StatusConflict, "instance is still starting")
		return
	}
	controller, ok := entry.handle.(balloonUpdater)
	if !ok {
		apiErr(w, http.StatusConflict, "virtio-balloon is not configured")
		return
	}
	if err := controller.UpdateBalloonStats(vmm.BalloonStatsUpdate{
		StatsPollingIntervalS: v.StatsPollingIntervalS,
	}); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(204)
}

func (s *Server) handleMemoryHotplugGet(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	rootID := s.rootVMID
	prebootCfg := cloneMemoryHotplug(s.preboot.memoryHotplug)
	prebootRequested := s.preboot.memoryHotplugRequestedMiB
	entry := s.vms[rootID]
	s.mu.RUnlock()
	if rootID == "" {
		if prebootCfg == nil {
			apiErr(w, http.StatusBadRequest, "memory hotplug is not configured")
			return
		}
		_ = json.NewEncoder(w).Encode(MemoryHotplugStatus{
			TotalSizeMiB:     prebootCfg.TotalSizeMiB,
			SlotSizeMiB:      prebootCfg.SlotSizeMiB,
			BlockSizeMiB:     prebootCfg.BlockSizeMiB,
			PluggedSizeMiB:   0,
			RequestedSizeMiB: prebootRequested,
		})
		return
	}
	if entry == nil || entry.isPending() {
		apiErr(w, http.StatusConflict, "instance is still starting")
		return
	}
	controller, ok := entry.handle.(memoryHotplugUpdater)
	if !ok {
		apiErr(w, http.StatusConflict, "memory hotplug is not configured")
		return
	}
	status, err := controller.GetMemoryHotplug()
	if err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = json.NewEncoder(w).Encode(status)
}

func (s *Server) handleMemoryHotplugPut(w http.ResponseWriter, r *http.Request) {
	var v MemoryHotplugConfig
	if err := decodeJSONStrict(r, &v); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := firecrackerapi.ValidateMemoryHotplugConfig(firecrackerapi.MemoryHotplugConfig{
		TotalSizeMib: v.TotalSizeMiB,
		SlotSizeMib:  v.SlotSizeMiB,
		BlockSizeMib: v.BlockSizeMiB,
	}); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, http.StatusBadRequest), err.Error())
		return
	}
	s.mu.Lock()
	s.preboot.memoryHotplug = cloneMemoryHotplug(&v)
	s.preboot.memoryHotplugRequestedMiB = 0
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMemoryHotplugPatch(w http.ResponseWriter, r *http.Request) {
	var v MemoryHotplugSizeUpdate
	if err := decodeJSONStrict(r, &v); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := firecrackerapi.ValidateMemoryHotplugSizeUpdate(firecrackerapi.MemoryHotplugSizeUpdate{
		RequestedSizeMib: v.RequestedSizeMiB,
	}); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, http.StatusBadRequest), err.Error())
		return
	}
	s.mu.Lock()
	if s.rootVMID == "" {
		if s.preboot.memoryHotplug == nil {
			s.mu.Unlock()
			apiErr(w, http.StatusBadRequest, "memory hotplug is not configured")
			return
		}
		if v.RequestedSizeMiB > s.preboot.memoryHotplug.TotalSizeMiB {
			s.mu.Unlock()
			apiErr(w, http.StatusBadRequest, "requested_size_mib exceeds total_size_mib")
			return
		}
		if s.preboot.memoryHotplug.BlockSizeMiB > 0 && v.RequestedSizeMiB%s.preboot.memoryHotplug.BlockSizeMiB != 0 {
			s.mu.Unlock()
			apiErr(w, http.StatusBadRequest, "requested_size_mib must be aligned to block_size_mib")
			return
		}
		s.preboot.memoryHotplugRequestedMiB = v.RequestedSizeMiB
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	entry := s.vms[s.rootVMID]
	s.mu.Unlock()
	if entry == nil || entry.isPending() {
		apiErr(w, http.StatusConflict, "instance is still starting")
		return
	}
	controller, ok := entry.handle.(memoryHotplugUpdater)
	if !ok {
		apiErr(w, http.StatusConflict, "memory hotplug is not configured")
		return
	}
	if err := controller.UpdateMemoryHotplug(vmm.MemoryHotplugSizeUpdate{RequestedSizeMiB: v.RequestedSizeMiB}); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDrive(w http.ResponseWriter, r *http.Request) {
	if err := s.rejectIfRootStarted(); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, 409), err.Error())
		return
	}
	id := chi.URLParam(r, "drive_id")
	var v Drive
	if err := decodeJSONStrict(r, &v); err != nil {
		apiErr(w, 400, err.Error())
		return
	}
	v.DriveID = id
	if err := firecrackerapi.ValidateDrive(firecrackerapi.Drive{
		DriveID:      v.DriveID,
		PathOnHost:   v.PathOnHost,
		IsRootDevice: v.IsRootDevice,
	}); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, 400), err.Error())
		return
	}
	s.mu.Lock()
	s.preboot.drives = upsertDrive(s.preboot.drives, v)
	s.mu.Unlock()
	w.WriteHeader(204)
}

func (s *Server) handleNetworkIface(w http.ResponseWriter, r *http.Request) {
	if err := s.rejectIfRootStarted(); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, 409), err.Error())
		return
	}
	id := chi.URLParam(r, "iface_id")
	var v NetworkInterface
	if err := decodeJSONStrict(r, &v); err != nil {
		apiErr(w, 400, err.Error())
		return
	}
	v.IfaceID = id
	if err := firecrackerapi.ValidateNetworkInterface(firecrackerapi.NetworkInterface{
		IfaceID:     v.IfaceID,
		HostDevName: v.HostDevName,
		GuestMAC:    v.GuestMAC,
	}); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, 400), err.Error())
		return
	}
	s.mu.Lock()
	s.preboot.netIfaces = upsertNetworkInterface(s.preboot.netIfaces, v)
	s.mu.Unlock()
	w.WriteHeader(204)
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	var a Action
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		apiErr(w, 400, err.Error())
		return
	}
	switch a.ActionType {
	case "InstanceStart":
		if err := s.startPrebootVM(); err != nil {
			apiErr(w, firecrackerapi.StatusCode(err, 400), err.Error())
			return
		}
		w.WriteHeader(204)
	case "InstanceStop":
		if err := s.stopRootVM(); err != nil {
			apiErr(w, firecrackerapi.StatusCode(err, 400), err.Error())
			return
		}
		w.WriteHeader(204)
	default:
		apiErr(w, 400, "unknown action: "+a.ActionType)
	}
}

func (s *Server) startPrebootVM() error {
	cfg, err := s.buildPrebootVMConfig()
	if err != nil {
		return err
	}
	s.mu.RLock()
	requestedHotplugMiB := s.preboot.memoryHotplugRequestedMiB
	s.mu.RUnlock()
	handle, cleanup, err := s.launchManagedVMM(cfg)
	if err != nil {
		return err
	}
	if requestedHotplugMiB > 0 {
		controller, ok := handle.(memoryHotplugUpdater)
		if !ok {
			handle.Stop()
			cleanup()
			return fmt.Errorf("memory hotplug is not configured")
		}
		if err := controller.UpdateMemoryHotplug(vmm.MemoryHotplugSizeUpdate{RequestedSizeMiB: requestedHotplugMiB}); err != nil {
			handle.Stop()
			cleanup()
			return fmt.Errorf("apply preboot memory hotplug target: %w", err)
		}
	}
	entry := s.newVMEntry(handle, cleanup)
	entry.isRoot = true
	entry.kind = "firecracker-root"
	s.registerVMEntry(handle.ID(), entry)
	return nil
}

func cloneAPILimiter(cfg *vmm.RateLimiterConfig) *vmm.RateLimiterConfig {
	if cfg == nil {
		return nil
	}
	cloned := *cfg
	return &cloned
}

// ---- Extended handlers ----

// POST /run  — build + boot in one call
func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	var req RunRequest
	if err := decodeJSONStrict(r, &req); err != nil {
		apiErr(w, 400, err.Error())
		return
	}
	if req.KernelPath == "" {
		apiErr(w, 400, "kernel_path is required")
		return
	}
	if req.Image == "" && req.Dockerfile == "" {
		apiErr(w, 400, "exactly one of image or dockerfile is required")
		return
	}
	if req.Image != "" && req.Dockerfile != "" {
		apiErr(w, 400, "specify image or dockerfile, not both")
		return
	}
	if err := s.validateKernelPathForServer(req.KernelPath); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Dockerfile != "" {
		if err := s.validateWorkPathForServer(req.Dockerfile, "dockerfile"); err != nil {
			apiErr(w, http.StatusBadRequest, err.Error())
			return
		}
		contextPath := req.Context
		if strings.TrimSpace(contextPath) == "" {
			contextPath = filepath.Dir(req.Dockerfile)
		}
		if err := s.validateWorkPathForServer(contextPath, "context"); err != nil {
			apiErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if strings.TrimSpace(req.SnapshotDir) != "" {
		if err := s.validateSnapshotPathForServer(req.SnapshotDir, true); err != nil {
			apiErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.Balloon != nil {
		if err := firecrackerapi.ValidateBalloon(firecrackerapi.Balloon{
			AmountMib:             req.Balloon.AmountMib,
			DeflateOnOOM:          req.Balloon.DeflateOnOOM,
			StatsPollingIntervalS: req.Balloon.StatsPollingIntervalS,
			FreePageHinting:       req.Balloon.FreePageHinting,
			FreePageReporting:     req.Balloon.FreePageReporting,
		}); err != nil {
			apiErr(w, firecrackerapi.StatusCode(err, http.StatusBadRequest), err.Error())
			return
		}
		if req.Balloon.AmountMib > req.MemMB && req.MemMB > 0 {
			apiErr(w, http.StatusBadRequest, "balloon amount_mib exceeds mem_mb")
			return
		}
	}
	if req.MemoryHotplug != nil {
		if err := firecrackerapi.ValidateMemoryHotplugConfig(firecrackerapi.MemoryHotplugConfig{
			TotalSizeMib: req.MemoryHotplug.TotalSizeMiB,
			SlotSizeMib:  req.MemoryHotplug.SlotSizeMiB,
			BlockSizeMib: req.MemoryHotplug.BlockSizeMiB,
		}); err != nil {
			apiErr(w, firecrackerapi.StatusCode(err, http.StatusBadRequest), err.Error())
			return
		}
	}
	if err := validateRequestedArch(req.Arch); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateNetworkMode(req.NetworkMode, req.StaticIP, req.Gateway); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// Generate ID before goroutine so response matches the actual VM
	id := fmt.Sprintf("gc-%d", time.Now().UnixNano()%100000)

	// Deep-copy slices to avoid race with caller
	cmd := make([]string, len(req.Cmd))
	copy(cmd, req.Cmd)
	ep := make([]string, len(req.Entrypoint))
	copy(ep, req.Entrypoint)
	env := make([]string, len(req.Env))
	copy(env, req.Env)
	buildArgs := make(map[string]string, len(req.BuildArgs))
	for k, v := range req.BuildArgs {
		buildArgs[k] = v
	}

	opts := container.RunOptions{
		ID:            id,
		Image:         req.Image,
		Dockerfile:    req.Dockerfile,
		Context:       req.Context,
		CPUs:          req.VcpuCount,
		MemMB:         req.MemMB,
		Arch:          req.Arch,
		KernelPath:    req.KernelPath,
		TapName:       req.TapName,
		NetworkMode:   normalizeNetworkMode(req.NetworkMode),
		X86Boot:       s.defaultX86Boot,
		Cmd:           cmd,
		Entrypoint:    ep,
		Env:           env,
		Hosts:         append([]string{}, req.Hosts...),
		WorkDir:       req.WorkDir,
		PID1Mode:      req.PID1Mode,
		BuildArgs:     buildArgs,
		DiskSizeMB:    req.DiskSizeMB,
		Mounts:        append([]container.Mount(nil), req.Mounts...),
		JailerMode:    s.jailerMode,
		SnapshotDir:   req.SnapshotDir,
		StaticIP:      req.StaticIP,
		Gateway:       req.Gateway,
		CacheDir:      req.CacheDir,
		Metadata:      cloneMetadata(req.Metadata),
		ExecEnabled:   req.ExecEnabled,
		MemoryHotplug: cloneMemoryHotplug(req.MemoryHotplug),
	}
	if req.Balloon != nil {
		opts.Balloon = &vmm.BalloonConfig{
			AmountMiB:             req.Balloon.AmountMib,
			DeflateOnOOM:          req.Balloon.DeflateOnOOM,
			StatsPollingIntervalS: req.Balloon.StatsPollingIntervalS,
		}
	}
	if len(req.Drives) > 0 {
		seen := map[string]struct{}{}
		opts.Drives = make([]vmm.DriveConfig, 0, len(req.Drives))
		for _, drive := range req.Drives {
			if err := firecrackerapi.ValidateDrive(firecrackerapi.Drive{
				DriveID:      drive.DriveID,
				PathOnHost:   drive.PathOnHost,
				IsRootDevice: drive.IsRootDevice,
			}); err != nil {
				apiErr(w, firecrackerapi.StatusCode(err, http.StatusBadRequest), err.Error())
				return
			}
			if drive.IsRootDevice {
				apiErr(w, http.StatusBadRequest, "run.drives only supports additional non-root drives")
				return
			}
			if _, ok := seen[drive.DriveID]; ok {
				apiErr(w, http.StatusBadRequest, fmt.Sprintf("duplicate drive_id %q", drive.DriveID))
				return
			}
			seen[drive.DriveID] = struct{}{}
			opts.Drives = append(opts.Drives, vmm.DriveConfig{
				ID:          drive.DriveID,
				Path:        drive.PathOnHost,
				Root:        false,
				ReadOnly:    drive.IsReadOnly,
				RateLimiter: cloneAPILimiter(drive.RateLimiter),
			})
		}
	}
	if req.X86Boot != "" {
		mode, err := normalizeX86BootMode(s.defaultX86Boot, req.X86Boot)
		if err != nil {
			apiErr(w, 400, err.Error())
			return
		}
		opts.X86Boot = mode
	}
	if opts.CacheDir == "" {
		opts.CacheDir = s.cacheDir
	}
	opts.JailerBinary = s.jailerBinary
	opts.VMMBinary = s.vmmBinary
	opts.ChrootBase = s.chrootBaseDir
	opts.UID = s.uid
	opts.GID = s.gid

	// Opt-in synchronous path: used by the snapshot-restore fast path so
	// the caller can skip the starting→running poll round-trip. Accept
	// either "wait=true" in the JSON body or the "wait=true" query param.
	waitSync := req.Wait
	if !waitSync {
		if v := strings.TrimSpace(r.URL.Query().Get("wait")); v != "" {
			if b, err := strconv.ParseBool(v); err == nil {
				waitSync = b
			}
		}
	}
	// wait=true runs the full runFn inline and returns once state="running".
	// For snapshot_dir this is typically single-digit ms; for a fresh boot
	// this blocks for the kernel boot duration (seconds). Callers that need
	// the cold-boot fast-return behaviour can leave wait=false and poll
	// GET /vms/{id}.
	if waitSync {
		result, err := s.runFn(opts)
		if err != nil {
			gclog.API.Error("run failed", "error", err)
			apiErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := s.attachComposeStackResources(id, opts, result); err != nil {
			gclog.API.Error("compose network attach failed", "id", id, "error", err)
			result.VM.Stop()
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
			_ = result.VM.WaitStopped(stopCtx)
			stopCancel()
			result.Close()
			apiErr(w, http.StatusInternalServerError, fmt.Sprintf("attach compose network: %v", err))
			return
		}
		entry := s.newVMEntry(result.VM, result.Close)
		entry.metadata = mergeMetadata(entry.metadata, map[string]string{
			"guest_ip":     result.GuestIP,
			"gateway":      result.Gateway,
			"tap_name":     result.TapName,
			"disk_path":    result.DiskPath,
			"network_mode": opts.NetworkMode,
		})
		s.registerVMEntry(result.ID, entry)
		json.NewEncoder(w).Encode(RunResponse{
			ID:                   result.ID,
			State:                "running",
			Message:              "VM is running",
			TapName:              result.TapName,
			GuestIP:              result.GuestIP,
			Gateway:              result.Gateway,
			NetworkMode:          opts.NetworkMode,
			RestoredFromSnapshot: strings.TrimSpace(opts.SnapshotDir) != "",
		})
		return
	}

	// Run asynchronously so the HTTP response returns immediately
	go func() {
		result, err := s.runFn(opts)
		if err != nil {
			gclog.API.Error("run failed", "error", err)
			s.markPendingVMStartFailed(id, err)
			return
		}
		if err := s.attachComposeStackResources(id, opts, result); err != nil {
			gclog.API.Error("compose network attach failed", "id", id, "error", err)
			result.VM.Stop()
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
			_ = result.VM.WaitStopped(stopCtx)
			stopCancel()
			result.Close()
			s.markPendingVMStartFailed(id, fmt.Errorf("attach compose network: %w", err))
			return
		}
		entry := s.newVMEntry(result.VM, result.Close)
		entry.metadata = mergeMetadata(entry.metadata, map[string]string{
			"guest_ip":     result.GuestIP,
			"gateway":      result.Gateway,
			"tap_name":     result.TapName,
			"disk_path":    result.DiskPath,
			"network_mode": opts.NetworkMode,
		})
		s.registerVMEntry(result.ID, entry)
	}()

	s.registerVMEntry(id, newPendingVMEntry(id, opts))

	json.NewEncoder(w).Encode(RunResponse{
		ID:          id,
		State:       "starting",
		Message:     "VM is booting",
		NetworkMode: opts.NetworkMode,
	})
}

// POST /build — build rootfs+disk only, don't boot
func (s *Server) handleBuild(w http.ResponseWriter, r *http.Request) {
	var req RunRequest
	if err := decodeJSONStrict(r, &req); err != nil {
		apiErr(w, 400, err.Error())
		return
	}
	if req.Image == "" && req.Dockerfile == "" {
		apiErr(w, 400, "exactly one of image or dockerfile is required")
		return
	}
	if req.Image != "" && req.Dockerfile != "" {
		apiErr(w, 400, "specify image or dockerfile, not both")
		return
	}
	if req.Dockerfile != "" {
		if err := s.validateWorkPathForServer(req.Dockerfile, "dockerfile"); err != nil {
			apiErr(w, http.StatusBadRequest, err.Error())
			return
		}
		contextPath := req.Context
		if strings.TrimSpace(contextPath) == "" {
			contextPath = filepath.Dir(req.Dockerfile)
		}
		if err := s.validateWorkPathForServer(contextPath, "context"); err != nil {
			apiErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	go func() {
		result, err := s.buildFn(container.BuildOptions{
			Image:        req.Image,
			Dockerfile:   req.Dockerfile,
			Context:      req.Context,
			DiskSizeMB:   req.DiskSizeMB,
			BuildArgs:    req.BuildArgs,
			CacheDir:     firstNonEmpty(req.CacheDir, s.cacheDir),
			JailerMode:   s.jailerMode,
			JailerBinary: s.jailerBinary,
			WorkerBinary: s.vmmBinary,
			ChrootBase:   s.chrootBaseDir,
			UID:          s.uid,
			GID:          s.gid,
		})
		if err != nil {
			gclog.API.Error("build failed", "error", err)
			return
		}
		gclog.API.Info("build complete", "disk", result.DiskPath)
	}()

	json.NewEncoder(w).Encode(map[string]string{"status": "building"})
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

const composePortMappingsMetadataKey = "gocracker_internal_compose_ports"

func isComposeVMMetadata(metadata map[string]string) bool {
	return strings.EqualFold(strings.TrimSpace(metadata["orchestrator"]), "compose") &&
		strings.TrimSpace(metadata["stack_name"]) != ""
}

func decodeComposePortMappings(metadata map[string]string) ([]stacknet.PortMapping, error) {
	raw := strings.TrimSpace(metadata[composePortMappingsMetadataKey])
	if raw == "" {
		return nil, nil
	}
	var mappings []stacknet.PortMapping
	if err := json.Unmarshal([]byte(raw), &mappings); err != nil {
		return nil, fmt.Errorf("decode compose port mappings: %w", err)
	}
	return mappings, nil
}

func publicMetadata(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		if strings.HasPrefix(key, "gocracker_internal_") {
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *Server) ensureComposeStackNetwork(stackName, guestCIDR, gateway string) (*stacknet.Manager, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.composeStacks[stackName]; existing != nil && existing.network != nil {
		return existing.network, false, nil
	}
	var subnet *net.IPNet
	if strings.TrimSpace(guestCIDR) != "" {
		_, parsed, err := net.ParseCIDR(strings.TrimSpace(guestCIDR))
		if err != nil {
			return nil, false, fmt.Errorf("parse compose guest CIDR %q: %w", guestCIDR, err)
		}
		subnet = parsed
	}
	var gatewayIP net.IP
	if strings.TrimSpace(gateway) != "" {
		gatewayIP = net.ParseIP(strings.TrimSpace(gateway))
		if gatewayIP == nil {
			return nil, false, fmt.Errorf("parse compose gateway %q", gateway)
		}
	}
	manager, err := stacknet.New(stackName, subnet, gatewayIP)
	if err != nil {
		return nil, false, err
	}
	s.composeStacks[stackName] = &composeStack{
		network: manager,
		vmIDs:   map[string]struct{}{},
	}
	return manager, true, nil
}

func (s *Server) attachComposeStackResources(id string, opts container.RunOptions, result *container.RunResult) error {
	if !isComposeVMMetadata(opts.Metadata) {
		return nil
	}
	stackName := strings.TrimSpace(opts.Metadata["stack_name"])
	serviceName := strings.TrimSpace(opts.Metadata["service_name"])
	if stackName == "" {
		return nil
	}
	manager, created, err := s.ensureComposeStackNetwork(stackName, firstNonEmpty(opts.StaticIP, opts.Metadata["guest_ip"]), firstNonEmpty(opts.Gateway, opts.Metadata["gateway"]))
	if err != nil {
		return err
	}
	if strings.TrimSpace(result.TapName) == "" {
		return fmt.Errorf("compose service %s missing tap name", serviceName)
	}
	if strings.TrimSpace(result.GuestIP) == "" {
		return fmt.Errorf("compose service %s missing guest IP", serviceName)
	}
	if err := manager.AttachTap(result.TapName); err != nil {
		if created {
			s.releaseComposeStackResources(id, opts.Metadata)
		}
		return err
	}
	mappings, err := decodeComposePortMappings(opts.Metadata)
	if err != nil {
		if created {
			s.releaseComposeStackResources(id, opts.Metadata)
		}
		return err
	}
	if err := manager.AddPortForwardMappings(serviceName, result.GuestIP, mappings); err != nil {
		if created {
			s.releaseComposeStackResources(id, opts.Metadata)
		}
		return err
	}
	s.mu.Lock()
	stack := s.composeStacks[stackName]
	if stack == nil {
		stack = &composeStack{network: manager, vmIDs: map[string]struct{}{}}
		s.composeStacks[stackName] = stack
	}
	stack.vmIDs[id] = struct{}{}
	s.mu.Unlock()
	return nil
}

func (s *Server) releaseComposeStackResources(id string, metadata map[string]string) {
	if !isComposeVMMetadata(metadata) {
		return
	}
	var manager *stacknet.Manager
	s.mu.Lock()
	manager = s.releaseComposeStackResourcesLocked(id, metadata)
	s.mu.Unlock()
	if manager != nil {
		manager.Close()
	}
}

func (s *Server) releaseComposeStackResourcesLocked(id string, metadata map[string]string) *stacknet.Manager {
	if !isComposeVMMetadata(metadata) {
		return nil
	}
	stackName := strings.TrimSpace(metadata["stack_name"])
	if stackName == "" {
		return nil
	}
	stack := s.composeStacks[stackName]
	if stack == nil {
		return nil
	}
	delete(stack.vmIDs, id)
	if len(stack.vmIDs) == 0 {
		manager := stack.network
		delete(s.composeStacks, stackName)
		return manager
	}
	return nil
}

// cleanStopped removes VMs that have stopped from the map.
func (s *Server) cleanStopped() {
	s.mu.Lock()
	var composeManagers []*stacknet.Manager
	for id, v := range s.vms {
		if v == nil || v.handle == nil {
			continue
		}
		if v.handle.State() == vmm.StateStopped {
			if manager := s.releaseComposeStackResourcesLocked(id, v.metadata); manager != nil {
				composeManagers = append(composeManagers, manager)
			}
			if v.cleanup != nil {
				v.cleanup()
			}
			if s.rootVMID == id {
				s.rootVMID = ""
			}
			delete(s.vms, id)
			if dir := s.vmDirs[id]; dir != "" {
				delete(s.vmDirs, id)
				_ = os.RemoveAll(dir)
			}
			s.removePersistedWorkerRecord(id)
		}
	}
	s.mu.Unlock()
	for _, manager := range composeManagers {
		manager.Close()
	}
}

func (s *Server) newVMEntry(handle vmm.Handle, cleanup func()) *vmEntry {
	entry := &vmEntry{
		handle:      handle,
		kind:        "local",
		metadata:    cloneMetadata(handle.VMConfig().Metadata),
		createdAt:   time.Now(),
		lastEventAt: time.Now(),
		cleanup:     cleanup,
	}
	if localVM, ok := handle.(*vmm.VM); ok {
		entry.local = localVM
	}
	if workerHandle, ok := handle.(vmm.WorkerBacked); ok {
		meta := workerHandle.WorkerMetadata()
		if meta.Kind != "" {
			entry.kind = meta.Kind
		}
		entry.socketPath = meta.SocketPath
		entry.workerPID = meta.WorkerPID
		entry.jailRoot = meta.JailRoot
		entry.runDir = meta.RunDir
		if !meta.CreatedAt.IsZero() {
			entry.createdAt = meta.CreatedAt
		}
	}
	if events := handle.Events().Events(time.Time{}); len(events) > 0 {
		entry.lastEventAt = events[len(events)-1].Time
	}
	return entry
}

func newPendingVMEntry(id string, opts container.RunOptions) *vmEntry {
	now := time.Now()
	cfg := vmm.Config{
		ID:            id,
		MemMB:         opts.MemMB,
		Arch:          opts.Arch,
		KernelPath:    opts.KernelPath,
		TapName:       opts.TapName,
		Metadata:      cloneMetadata(opts.Metadata),
		Balloon:       cloneBalloonConfig(opts.Balloon),
		MemoryHotplug: cloneMemoryHotplug(opts.MemoryHotplug),
	}
	return &vmEntry{
		apiID:        id,
		kind:         "pending",
		metadata:     cloneMetadata(opts.Metadata),
		pendingCfg:   cfg,
		pendingState: "starting",
		pendingEvents: []vmm.Event{
			{Time: now, Type: vmm.EventCreated, Message: "VM accepted by API"},
			{Time: now, Type: vmm.EventStarting, Message: "VM is booting"},
		},
		createdAt:   now,
		lastEventAt: now,
	}
}

func (entry *vmEntry) isPending() bool {
	return entry != nil && entry.handle == nil && strings.TrimSpace(entry.pendingState) != ""
}

func (s *Server) markPendingVMStartFailed(id string, err error) {
	if id == "" || err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.vms[id]
	if !ok || !entry.isPending() {
		return
	}
	entry.pendingState = vmm.StateStopped.String()
	entry.pendingEvents = append(entry.pendingEvents, vmm.Event{
		Time:    time.Now(),
		Type:    vmm.EventError,
		Message: err.Error(),
	}, vmm.Event{
		Time:    time.Now(),
		Type:    vmm.EventStopped,
		Message: "VM failed before registration",
	})
}

func normalizeX86BootMode(defaultMode vmm.X86BootMode, raw string) (vmm.X86BootMode, error) {
	mode := strings.TrimSpace(raw)
	if mode == "" {
		if defaultMode == "" {
			return vmm.X86BootAuto, nil
		}
		return defaultMode, nil
	}
	switch vmm.X86BootMode(mode) {
	case vmm.X86BootAuto, vmm.X86BootACPI, vmm.X86BootLegacy:
		return vmm.X86BootMode(mode), nil
	default:
		return "", fmt.Errorf("invalid x86 boot mode %q", raw)
	}
}

func decodeJSONStrict(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func upsertDrive(drives []Drive, drive Drive) []Drive {
	for i := range drives {
		if drives[i].DriveID == drive.DriveID {
			drives[i] = drive
			return drives
		}
	}
	return append(drives, drive)
}

func upsertNetworkInterface(ifaces []NetworkInterface, iface NetworkInterface) []NetworkInterface {
	for i := range ifaces {
		if ifaces[i].IfaceID == iface.IfaceID {
			ifaces[i] = iface
			return ifaces
		}
	}
	return append(ifaces, iface)
}

func (s *Server) handleListVMs(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var list []VMInfo
	for _, v := range s.vms {
		info := s.buildVMInfo(v)
		if !matchesVMFilters(info, r) {
			continue
		}
		list = append(list, info)
	}
	if list == nil {
		list = []VMInfo{}
	}
	json.NewEncoder(w).Encode(list)
}

func (s *Server) handleGetVM(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	s.mu.RLock()
	v, ok := s.vms[id]
	s.mu.RUnlock()
	if !ok {
		apiErr(w, 404, "VM not found")
		return
	}
	json.NewEncoder(w).Encode(s.buildVMInfo(v))
}

func (s *Server) buildVMInfo(entry *vmEntry) VMInfo {
	if entry == nil {
		return VMInfo{}
	}
	if entry.isPending() {
		events := append([]vmm.Event(nil), entry.pendingEvents...)
		if len(events) > 50 {
			events = events[len(events)-50:]
		}
		metadata := publicMetadata(cloneMetadata(entry.pendingCfg.Metadata))
		if metadata == nil && len(entry.metadata) > 0 {
			metadata = map[string]string{}
		}
		for key, value := range entry.metadata {
			if strings.HasPrefix(key, "gocracker_internal_") {
				continue
			}
			metadata[key] = value
		}
		return VMInfo{
			ID:          entry.apiID,
			State:       entry.pendingState,
			Uptime:      time.Since(entry.createdAt).Round(time.Second).String(),
			MemMB:       entry.pendingCfg.MemMB,
			Arch:        defaultVMArch(entry.pendingCfg.Arch),
			Kernel:      entry.pendingCfg.KernelPath,
			Events:      events,
			Metadata:    metadata,
			TapName:     metadata["tap_name"],
			GuestIP:     metadata["guest_ip"],
			Gateway:     metadata["gateway"],
			NetworkMode: metadata["network_mode"],
		}
	}
	cfg := entry.handle.VMConfig()
	events := entry.handle.Events().Events(time.Time{})
	// Return last 50 events in the info response
	if len(events) > 50 {
		events = events[len(events)-50:]
	}
	metadata := publicMetadata(cloneMetadata(cfg.Metadata))
	if metadata == nil && len(entry.metadata) > 0 {
		metadata = map[string]string{}
	}
	for key, value := range entry.metadata {
		if strings.HasPrefix(key, "gocracker_internal_") {
			continue
		}
		metadata[key] = value
	}
	id := entry.handle.ID()
	if strings.TrimSpace(entry.apiID) != "" {
		id = entry.apiID
	}
	return VMInfo{
		ID:          id,
		State:       entry.handle.State().String(),
		Uptime:      entry.handle.Uptime().Round(time.Second).String(),
		MemMB:       cfg.MemMB,
		Arch:        defaultVMArch(cfg.Arch),
		Kernel:      cfg.KernelPath,
		Events:      events,
		Devices:     entry.handle.DeviceList(),
		Metadata:    metadata,
		TapName:     metadata["tap_name"],
		GuestIP:     metadata["guest_ip"],
		Gateway:     metadata["gateway"],
		NetworkMode: metadata["network_mode"],
	}
}

func defaultVMArch(raw string) string {
	if strings.TrimSpace(raw) != "" {
		return raw
	}
	return runtime.GOARCH
}

func validateRequestedArch(raw string) error {
	arch := strings.TrimSpace(raw)
	if arch == "" {
		return nil
	}
	switch vmm.MachineArch(arch) {
	case vmm.ArchAMD64, vmm.ArchARM64:
	default:
		return fmt.Errorf("invalid arch %q", raw)
	}
	if arch != runtime.GOARCH {
		return fmt.Errorf("arch %q is not compatible with host arch %q (same-arch only)", arch, runtime.GOARCH)
	}
	return nil
}

func validateNetworkMode(mode, staticIP, gateway string) error {
	switch strings.TrimSpace(strings.ToLower(mode)) {
	case "", "none":
		return nil
	case container.NetworkModeAuto:
		if strings.TrimSpace(staticIP) != "" || strings.TrimSpace(gateway) != "" {
			return fmt.Errorf("network_mode=auto is exclusive with explicit static_ip/gateway")
		}
		return nil
	default:
		return fmt.Errorf("invalid network_mode %q (want \"\"|\"none\"|\"auto\")", mode)
	}
}

// normalizeNetworkMode folds "none" back to "" so downstream code only has to
// distinguish "" (explicit/none) from "auto".
func normalizeNetworkMode(mode string) string {
	switch strings.TrimSpace(strings.ToLower(mode)) {
	case container.NetworkModeAuto:
		return container.NetworkModeAuto
	default:
		return ""
	}
}

// cloneTapName derives a unique tap name for a cloned VM, within the 15-char
// Linux IFNAMSIZ limit. Source and clone share the snapshot's MAC + guest IP;
// only the host-side tap name differs. Clones without host-side network
// routing still boot fine — the exec agent, disk, and virtiofs work — they
// just cannot reach the outside world until the caller supplies static_ip /
// gateway or network_mode=auto.
func cloneTapName(newID string) string {
	// newID is "gc-<12-digits>"; "tclone-<N>" fits under 15 chars when N is
	// the last 6 digits of the ID (monotonic per-second in practice).
	suffix := newID
	if strings.HasPrefix(suffix, "gc-") {
		suffix = suffix[3:]
	}
	if len(suffix) > 6 {
		suffix = suffix[len(suffix)-6:]
	}
	name := "tclone-" + suffix
	if len(name) > 15 {
		name = name[:15]
	}
	return name
}

func matchesVMFilters(info VMInfo, r *http.Request) bool {
	filters := map[string]string{
		"stack":        "stack_name",
		"service":      "service_name",
		"orchestrator": "orchestrator",
	}
	for queryKey, metadataKey := range filters {
		want := strings.TrimSpace(r.URL.Query().Get(queryKey))
		if want == "" {
			continue
		}
		if got := strings.TrimSpace(info.Metadata[metadataKey]); got != want {
			return false
		}
	}
	return true
}

func cloneMetadata(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
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

func cloneMemoryHotplug(cfg *vmm.MemoryHotplugConfig) *vmm.MemoryHotplugConfig {
	if cfg == nil {
		return nil
	}
	cloned := *cfg
	return &cloned
}

func mergeMetadata(dst map[string]string, values map[string]string) map[string]string {
	if dst == nil {
		dst = map[string]string{}
	}
	for key, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		dst[key] = value
	}
	if len(dst) == 0 {
		return nil
	}
	return dst
}

func (s *Server) handleVMEvents(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	s.mu.RLock()
	v, ok := s.vms[id]
	s.mu.RUnlock()
	if !ok {
		apiErr(w, 404, "VM not found")
		return
	}
	if v.isPending() {
		var since time.Time
		if q := r.URL.Query().Get("since"); q != "" {
			t, err := time.Parse(time.RFC3339, q)
			if err != nil {
				apiErr(w, 400, "invalid since: "+err.Error())
				return
			}
			since = t
		}
		var out []vmm.Event
		for _, ev := range v.pendingEvents {
			if since.IsZero() || ev.Time.After(since) {
				out = append(out, ev)
			}
		}
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	var since time.Time
	if q := r.URL.Query().Get("since"); q != "" {
		t, err := time.Parse(time.RFC3339, q)
		if err != nil {
			apiErr(w, 400, "invalid since: "+err.Error())
			return
		}
		since = t
	}
	json.NewEncoder(w).Encode(v.handle.Events().Events(since))
}

func (s *Server) handleVMEventsStream(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	s.mu.RLock()
	v, ok := s.vms[id]
	s.mu.RUnlock()
	if !ok {
		apiErr(w, 404, "VM not found")
		return
	}
	if v.isPending() {
		apiErr(w, http.StatusConflict, "VM is still starting")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		apiErr(w, 500, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, unsub := v.handle.Events().Subscribe()
	defer unsub()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func (s *Server) lookupRateLimiterUpdater(id string) (rateLimiterUpdater, bool, error) {
	s.mu.RLock()
	entry, ok := s.vms[id]
	s.mu.RUnlock()
	if !ok {
		return nil, false, nil
	}
	if entry.isPending() {
		return nil, true, fmt.Errorf("VM is still starting")
	}
	updater, ok := entry.handle.(rateLimiterUpdater)
	if !ok {
		return nil, true, fmt.Errorf("rate limiter update is not supported by this VM backend")
	}
	return updater, true, nil
}

func (s *Server) handleVMNetRateLimiter(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	updater, found, err := s.lookupRateLimiterUpdater(id)
	if err != nil {
		apiErr(w, http.StatusConflict, err.Error())
		return
	}
	if !found {
		apiErr(w, http.StatusNotFound, "VM not found")
		return
	}
	// Accept either the legacy single-bucket shape (TokenBucketConfig
	// fields directly) OR the Firecracker-parity {rx_rate_limiter, tx_rate_limiter}
	// envelope. We read the body once and try both.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var envelope struct {
		Rx *vmm.RateLimiterConfig `json:"rx_rate_limiter,omitempty"`
		Tx *vmm.RateLimiterConfig `json:"tx_rate_limiter,omitempty"`
	}
	_ = json.Unmarshal(body, &envelope)
	if envelope.Rx != nil || envelope.Tx != nil {
		if err := updater.UpdateNetRateLimiters(envelope.Rx, envelope.Tx); err != nil {
			apiErr(w, http.StatusBadRequest, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var cfg vmm.RateLimiterConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := updater.UpdateNetRateLimiter(&cfg); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleVMBlockRateLimiter(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	updater, found, err := s.lookupRateLimiterUpdater(id)
	if err != nil {
		apiErr(w, http.StatusConflict, err.Error())
		return
	}
	if !found {
		apiErr(w, http.StatusNotFound, "VM not found")
		return
	}
	var cfg vmm.RateLimiterConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := updater.UpdateBlockRateLimiter(&cfg); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleVMRNGRateLimiter(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	updater, found, err := s.lookupRateLimiterUpdater(id)
	if err != nil {
		apiErr(w, http.StatusConflict, err.Error())
		return
	}
	if !found {
		apiErr(w, http.StatusNotFound, "VM not found")
		return
	}
	var cfg vmm.RateLimiterConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := updater.UpdateRNGRateLimiter(&cfg); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleVMLogs(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	s.mu.RLock()
	v, ok := s.vms[id]
	s.mu.RUnlock()
	if !ok {
		apiErr(w, 404, "VM not found")
		return
	}
	if v.isPending() {
		apiErr(w, http.StatusConflict, "VM is still starting")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(v.handle.ConsoleOutput())
}

func (s *Server) handleVMVsockConnect(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	portValue := strings.TrimSpace(r.URL.Query().Get("port"))
	if portValue == "" {
		apiErr(w, http.StatusBadRequest, "port is required")
		return
	}
	var rawPort uint64
	if _, err := fmt.Sscanf(portValue, "%d", &rawPort); err != nil || rawPort > uint64(^uint32(0)) {
		apiErr(w, http.StatusBadRequest, "invalid port")
		return
	}
	s.mu.RLock()
	v, ok := s.vms[id]
	s.mu.RUnlock()
	if !ok {
		apiErr(w, http.StatusNotFound, "VM not found")
		return
	}
	if v.isPending() {
		apiErr(w, http.StatusConflict, "VM is still starting")
		return
	}
	dialer, ok := v.handle.(vmm.VsockDialer)
	if !ok {
		apiErr(w, http.StatusConflict, "virtio-vsock is not configured")
		return
	}
	guestConn, err := dialer.DialVsock(uint32(rawPort))
	if err != nil {
		apiErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := upgradeAndProxyVMConn(w, guestConn); err != nil {
		_ = guestConn.Close()
		apiErr(w, http.StatusInternalServerError, err.Error())
	}
}

func (s *Server) handleStopVM(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	s.mu.RLock()
	v, ok := s.vms[id]
	s.mu.RUnlock()
	if !ok {
		apiErr(w, 404, "VM not found")
		return
	}
	if v.isPending() {
		apiErr(w, http.StatusConflict, "VM is still starting")
		return
	}
	v.handle.Stop()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = v.handle.WaitStopped(ctx)
		s.cleanStopped()
	}()
	w.WriteHeader(204)
}

func (s *Server) handleVMPause(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	s.mu.RLock()
	v, ok := s.vms[id]
	s.mu.RUnlock()
	if !ok {
		apiErr(w, http.StatusNotFound, "VM not found")
		return
	}
	if v.isPending() {
		apiErr(w, http.StatusConflict, "VM is still starting")
		return
	}
	if err := v.handle.Pause(); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleVMResume(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	s.mu.RLock()
	v, ok := s.vms[id]
	s.mu.RUnlock()
	if !ok {
		apiErr(w, http.StatusNotFound, "VM not found")
		return
	}
	if v.isPending() {
		apiErr(w, http.StatusConflict, "VM is still starting")
		return
	}
	if err := v.handle.Resume(); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleVMClone snapshots the source VM and restores it as a new VM on the
// same server in one HTTP call. The source stays running; the clone gets a
// fresh ID. Callers can override tap/ip/gateway/mounts/network_mode to
// differentiate the clone from its template.
func (s *Server) handleVMClone(w http.ResponseWriter, r *http.Request) {
	srcID := chi.URLParam(r, "id")
	var req CloneRequest
	if err := decodeJSONStrict(r, &req); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateNetworkMode(req.NetworkMode, req.StaticIP, req.Gateway); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}

	s.mu.RLock()
	src, ok := s.vms[srcID]
	s.mu.RUnlock()
	if !ok {
		apiErr(w, http.StatusNotFound, "VM not found")
		return
	}
	if src.isPending() {
		apiErr(w, http.StatusConflict, "source VM is still starting")
		return
	}

	// Decide on the snapshot dir. If the caller did not provide one, use a
	// scratch dir under /tmp and delete it after the clone returns.
	snapDir := strings.TrimSpace(req.SnapshotDir)
	retainSnap := snapDir != ""
	if !retainSnap {
		tmp, err := os.MkdirTemp("", "gocracker-clone-*")
		if err != nil {
			apiErr(w, http.StatusInternalServerError, fmt.Sprintf("create clone tmpdir: %v", err))
			return
		}
		snapDir = tmp
		defer os.RemoveAll(tmp)
	} else {
		if err := s.validateSnapshotPathForServer(snapDir, false); err != nil {
			apiErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	srcCfg := src.handle.VMConfig()
	// virtio-fs mounts cannot currently round-trip through snapshot +
	// restore: the Linux driver's virtqueue index is frozen at snapshot
	// time and the fresh virtiofsd on restore starts at index 0, which
	// trips "requests.0:id 0 is not a head!" on first FUSE op. Until
	// virtiofsd grows FUSE-session migration (or we teach the guest
	// driver to fully re-init on a specific host signal), block clone of
	// a VM with live virtio-fs mounts instead of shipping an endpoint
	// that silently hangs the caller.
	if len(srcCfg.SharedFS) > 0 {
		apiErr(w, http.StatusBadRequest, "cloning a VM with active virtio-fs mounts is not supported: the Linux virtio-fs driver's queue state cannot be migrated to a fresh virtiofsd. Snapshot after umount, or use virtio-blk for per-sandbox data.")
		return
	}
	if _, err := src.handle.TakeSnapshot(snapDir); err != nil {
		apiErr(w, http.StatusInternalServerError, fmt.Sprintf("snapshot source: %v", err))
		return
	}
	newID := fmt.Sprintf("gc-%d", time.Now().UnixNano()%100000)
	// A clone running alongside its source cannot share the source's TAP —
	// the TUN/TAP device is exclusive to one opener. When the caller did not
	// supply tap_name/network_mode, mint a per-clone name derived from the
	// new VM ID so restore does not hit TUNSETIFF EBUSY against the source.
	tapName := strings.TrimSpace(req.TapName)
	if tapName == "" && normalizeNetworkMode(req.NetworkMode) == "" && strings.TrimSpace(srcCfg.TapName) != "" {
		tapName = cloneTapName(newID)
	}
	opts := container.RunOptions{
		ID:           newID,
		KernelPath:   srcCfg.KernelPath,
		TapName:      tapName,
		NetworkMode:  normalizeNetworkMode(req.NetworkMode),
		SnapshotDir:  snapDir,
		StaticIP:     req.StaticIP,
		Gateway:      req.Gateway,
		Mounts:       append([]container.Mount(nil), req.Mounts...),
		ExecEnabled:  req.ExecEnabled,
		Metadata:     cloneMetadata(req.Metadata),
		JailerMode:   s.jailerMode,
		JailerBinary: s.jailerBinary,
		VMMBinary:    s.vmmBinary,
		ChrootBase:   s.chrootBaseDir,
		UID:          s.uid,
		GID:          s.gid,
		CacheDir:     s.cacheDir,
		X86Boot:      s.defaultX86Boot,
	}

	result, err := s.runFn(opts)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, fmt.Sprintf("restore clone: %v", err))
		return
	}
	if err := s.attachComposeStackResources(newID, opts, result); err != nil {
		result.VM.Stop()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = result.VM.WaitStopped(stopCtx)
		stopCancel()
		result.Close()
		apiErr(w, http.StatusInternalServerError, fmt.Sprintf("attach compose network: %v", err))
		return
	}
	entry := s.newVMEntry(result.VM, result.Close)
	entry.metadata = mergeMetadata(entry.metadata, map[string]string{
		"guest_ip":     result.GuestIP,
		"gateway":      result.Gateway,
		"tap_name":     result.TapName,
		"disk_path":    result.DiskPath,
		"network_mode": opts.NetworkMode,
		"cloned_from":  srcID,
	})
	s.registerVMEntry(result.ID, entry)

	// The clone inherits frozen network/FUSE state from the snapshot. For
	// either to actually work, the guest has to be walked into the new
	// world: eth0 re-IP'd onto the fresh tap/subnet, and each virtio-fs
	// target re-mounted against the new virtiofsd. Both require exec; we
	// fail the clone loudly if the caller skipped exec_enabled, so they
	// don't end up with a silently-broken VM.
	if opts.NetworkMode == container.NetworkModeAuto {
		if !req.ExecEnabled {
			_ = s.stopAndUnregisterVM(result.ID)
			apiErr(w, http.StatusBadRequest, "clone with network_mode=auto requires exec_enabled=true (the clone needs to rewrite guest networking)")
			return
		}
		if err := s.execGuestReIP(result.ID, result.GuestIP, result.Gateway); err != nil {
			_ = s.stopAndUnregisterVM(result.ID)
			apiErr(w, http.StatusInternalServerError, fmt.Sprintf("post-restore re-IP: %v", err))
			return
		}
	}

	_ = json.NewEncoder(w).Encode(RunResponse{
		ID:                   result.ID,
		State:                "running",
		Message:              fmt.Sprintf("cloned from %s", srcID),
		TapName:              result.TapName,
		GuestIP:              result.GuestIP,
		Gateway:              result.Gateway,
		NetworkMode:          opts.NetworkMode,
		RestoredFromSnapshot: true,
	})
}

// execGuestReIP plumbs new IP+gateway into a freshly-restored guest by
// running a small iproute2 script over the exec agent. Idempotent (uses
// `ip addr replace` and `ip route replace`); a guest without `ip` is a
// no-op. Designed for the sandbox-clone flow where the guest kernel was
// frozen with the template's old IP and we want it to use the per-clone
// allocation.
func (s *Server) execGuestReIP(id, guestIP, gateway string) error {
	if strings.TrimSpace(guestIP) == "" || strings.TrimSpace(gateway) == "" {
		return nil
	}
	cidr := guestIP
	if !strings.Contains(cidr, "/") {
		cidr = guestIP + "/30"
	}
	script := fmt.Sprintf(`set -e
if ! command -v ip >/dev/null 2>&1; then exit 0; fi
ip link set eth0 down 2>/dev/null || true
ip addr flush dev eth0 2>/dev/null || true
ip addr add %s dev eth0
ip link set eth0 up
ip route replace default via %s
`, cidr, gateway)
	return s.execGuestScript(id, script)
}


// execGuestScript runs a shell script inside the guest synchronously via the
// exec agent. Retries the vsock dial with exponential backoff up to 30 s:
// immediately after a snapshot restore the guest exec agent's listener may
// not have re-bound yet (the TRANSPORT_RESET event propagates on the first
// kvm_run, the guest kernel sees ECONNRESET on its AF_VSOCK accept fd, and
// the agent has to re-`listen` — all of which needs vCPU cycles). The
// remaining RPC shares the outer deadline so slow dials don't collapse
// the I/O budget to nothing.
func (s *Server) execGuestScript(id, script string) error {
	s.mu.RLock()
	v, ok := s.vms[id]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("vm %s not registered", id)
	}
	if v.isPending() {
		return fmt.Errorf("vm %s still pending", id)
	}
	dialer, ok := v.handle.(vmm.VsockDialer)
	if !ok {
		return fmt.Errorf("vm %s does not expose vsock", id)
	}
	cfg := v.handle.VMConfig()
	if cfg.Exec == nil || !cfg.Exec.Enabled {
		return fmt.Errorf("vm %s does not have exec enabled", id)
	}
	port := cfg.Exec.VsockPort
	if port == 0 {
		port = 1056
	}

	var conn net.Conn
	var lastErr error
	deadline := time.Now().Add(30 * time.Second)
	backoff := 25 * time.Millisecond
	const maxBackoff = 500 * time.Millisecond
	for {
		c, err := dialer.DialVsock(port)
		if err == nil {
			conn = c
			break
		}
		lastErr = err
		if time.Now().After(deadline) {
			return fmt.Errorf("dial exec agent after %s: %w", 30*time.Second, lastErr)
		}
		time.Sleep(backoff)
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
	defer conn.Close()
	// Cap the RPC deadline at what's left of the outer deadline, with a
	// 2 s floor so a dial that consumed most of the 30 s still has room
	// to write+read a short JSON blob.
	rpcDeadline := deadline
	if remaining := time.Until(deadline); remaining < 2*time.Second {
		rpcDeadline = time.Now().Add(2 * time.Second)
	}
	if err := conn.SetDeadline(rpcDeadline); err != nil {
		return err
	}
	req := guestexec.Request{
		Mode:    guestexec.ModeExec,
		Command: []string{"/bin/sh", "-lc", script},
	}
	if err := guestexec.Encode(conn, req); err != nil {
		return fmt.Errorf("write exec request: %w", err)
	}
	var resp guestexec.Response
	if err := guestexec.Decode(conn, &resp); err != nil {
		return fmt.Errorf("read exec response: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("exec failed: %s", resp.Error)
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("exec exit=%d stdout=%q stderr=%q", resp.ExitCode, resp.Stdout, resp.Stderr)
	}
	return nil
}

// stopAndUnregisterVM best-effort tears down a VM we partially set up: used
// when a post-restore step (re-IP, remount) fails and we need to avoid
// leaking half-broken VMs in the registry. The registry entry is removed
// whether or not the stop RPC succeeds so a retry with the same API ID is
// not blocked.
func (s *Server) stopAndUnregisterVM(id string) error {
	s.mu.Lock()
	v, ok := s.vms[id]
	if ok {
		delete(s.vms, id)
	}
	s.mu.Unlock()
	if !ok || v == nil {
		return nil
	}
	if v.isPending() {
		return nil
	}
	v.handle.Stop()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	err := v.handle.WaitStopped(stopCtx)
	stopCancel()
	if v.cleanup != nil {
		v.cleanup()
	}
	return err
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req SnapshotRequest
	if err := decodeJSONStrict(r, &req); err != nil {
		apiErr(w, 400, err.Error())
		return
	}
	if req.DestDir == "" {
		req.DestDir = fmt.Sprintf("/tmp/gocracker-snapshots/%s-%d", id, time.Now().Unix())
	}
	if err := s.validateSnapshotPathForServer(req.DestDir, false); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.mu.RLock()
	v, ok := s.vms[id]
	s.mu.RUnlock()
	if !ok {
		apiErr(w, 404, "VM not found")
		return
	}
	if v.isPending() {
		apiErr(w, http.StatusConflict, "VM is still starting")
		return
	}
	snap, err := v.handle.TakeSnapshot(req.DestDir)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	json.NewEncoder(w).Encode(snap)
}

func upgradeAndProxyVMConn(w http.ResponseWriter, guestConn net.Conn) error {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return fmt.Errorf("http hijacking is not supported")
	}
	clientConn, rw, err := hj.Hijack()
	if err != nil {
		return err
	}
	if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: vsock\r\n\r\n"); err != nil {
		_ = clientConn.Close()
		return err
	}
	if err := rw.Flush(); err != nil {
		_ = clientConn.Close()
		return err
	}
	go proxyVMConnPair(clientConn, guestConn)
	return nil
}

func proxyVMConnPair(clientConn, guestConn net.Conn) {
	var once sync.Once
	closeBoth := func() {
		_ = clientConn.Close()
		_ = guestConn.Close()
	}
	go func() {
		_, _ = io.Copy(guestConn, clientConn)
		once.Do(closeBoth)
	}()
	go func() {
		_, _ = io.Copy(clientConn, guestConn)
		once.Do(closeBoth)
	}()
}

func apiErr(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(APIError{FaultMessage: msg})
}
