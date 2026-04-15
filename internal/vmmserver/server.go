package vmmserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gocracker/gocracker/internal/firecrackerapi"
	"github.com/gocracker/gocracker/internal/log"
	"github.com/gocracker/gocracker/internal/seccomp"
	"github.com/gocracker/gocracker/pkg/vmm"
)

// VM abstracts the vmm.VM surface used by the worker.
type VM interface {
	Start() error
	Stop()
	TakeSnapshot(string) (*vmm.Snapshot, error)
	State() vmm.State
	ID() string
	Uptime() time.Duration
	Events() vmm.EventSource
	VMConfig() vmm.Config
	DeviceList() []vmm.DeviceInfo
	ConsoleOutput() []byte
	FirstOutputAt() time.Time
}

// VMFactory creates a VM for the worker.
type VMFactory func(vmm.Config) (VM, error)

// Options configures the worker server.
type Options struct {
	DefaultX86Boot vmm.X86BootMode
	Factory        VMFactory
	VMID           string
}

type prebootConfig struct {
	bootSource                *BootSource
	machineCfg                *MachineConfig
	balloon                   *Balloon
	memoryHotplug             *vmm.MemoryHotplugConfig
	memoryHotplugRequestedMiB uint64
	drives                    []Drive
	netIfaces                 []NetworkInterface
	sharedFS                  []SharedFS
}

// SharedFS describes a virtio-fs export to advertise to the guest. The Source
// is a host directory; Tag is the mount tag the guest should use to mount it.
// SocketPath, when set, points to an existing virtiofsd unix socket the VMM
// should attach to instead of spawning its own virtiofsd process.
type SharedFS struct {
	Tag        string `json:"tag"`
	Source     string `json:"source"`
	SocketPath string `json:"socket_path,omitempty"`
}

// Server is a one-VM-per-process Firecracker-like API worker.
type Server struct {
	mu      sync.RWMutex
	router  chi.Router
	vm      VM
	started bool

	defaultX86Boot vmm.X86BootMode
	factory        VMFactory
	vmID           string
	preboot        prebootConfig
}

// BootSource matches the Firecracker boot-source payload.
type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
	InitrdPath      string `json:"initrd_path,omitempty"`
	X86Boot         string `json:"x86_boot,omitempty"`
}

// MachineConfig matches the Firecracker machine-config payload.
type MachineConfig struct {
	VcpuCount      int                    `json:"vcpu_count"`
	MemSizeMib     int                    `json:"mem_size_mib"`
	RNGRateLimiter *vmm.RateLimiterConfig `json:"rng_rate_limiter,omitempty"`
	VsockEnabled   bool                   `json:"vsock_enabled,omitempty"`
	VsockGuestCID  uint32                 `json:"vsock_guest_cid,omitempty"`
	ExecEnabled    bool                   `json:"exec_enabled,omitempty"`
	ExecVsockPort  uint32                 `json:"exec_vsock_port,omitempty"`
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

// Drive matches the Firecracker drive payload.
type Drive struct {
	DriveID      string                 `json:"drive_id"`
	PathOnHost   string                 `json:"path_on_host"`
	IsRootDevice bool                   `json:"is_root_device"`
	IsReadOnly   bool                   `json:"is_read_only"`
	RateLimiter  *vmm.RateLimiterConfig `json:"rate_limiter,omitempty"`
}

// NetworkInterface matches the Firecracker network-interface payload.
type NetworkInterface struct {
	IfaceID     string                 `json:"iface_id"`
	HostDevName string                 `json:"host_dev_name"`
	GuestMAC    string                 `json:"guest_mac,omitempty"`
	RateLimiter *vmm.RateLimiterConfig `json:"rate_limiter,omitempty"`
}

// Action matches the Firecracker actions payload.
type Action struct {
	ActionType string `json:"action_type"`
}

// InstanceInfo mirrors the root info response used by the existing API.
type InstanceInfo struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	AppName    string `json:"app_name"`
	VMMVersion string `json:"vmm_version"`
}

// APIError is the error envelope returned by the worker.
type APIError struct {
	FaultMessage string `json:"fault_message"`
}

type VMInfo struct {
	ID             string           `json:"id"`
	State          string           `json:"state"`
	Uptime         string           `json:"uptime"`
	MemMB          uint64           `json:"mem_mb"`
	Kernel         string           `json:"kernel"`
	Events         []vmm.Event      `json:"events"`
	Devices        []vmm.DeviceInfo `json:"devices,omitempty"`
	// FirstOutputAt is the wall-clock time at which the guest first
	// transmitted a byte on the serial console. Populated from the VMM's
	// UART as soon as the guest prints anything; zero until then.
	FirstOutputAt time.Time `json:"first_output_at,omitempty"`
}

// ConfigureAndStartRequest bundles all pre-boot configuration with InstanceStart.
type ConfigureAndStartRequest struct {
	BootSource        BootSource          `json:"boot_source"`
	MachineConfig     *MachineConfig      `json:"machine_config,omitempty"`
	Balloon           *Balloon            `json:"balloon,omitempty"`
	MemoryHotplug     *MemoryHotplugConfig `json:"memory_hotplug,omitempty"`
	Drives            []Drive             `json:"drives,omitempty"`
	NetworkInterfaces []NetworkInterface  `json:"network_interfaces,omitempty"`
	SharedFS          []SharedFS          `json:"shared_fs,omitempty"`
}

type SnapshotRequest struct {
	DestDir string `json:"dest_dir"`
}

type RestoreRequest struct {
	SnapshotDir string `json:"snapshot_dir"`
	TapName     string `json:"tap_name,omitempty"`
	VcpuCount   int    `json:"vcpu_count,omitempty"`
	X86Boot     string `json:"x86_boot,omitempty"`
	Resume      bool   `json:"resume"`
}

// restoreResponse is the minimal payload returned by POST /restore. It stays
// deliberately small (≤60 bytes JSON) so the hot snapshot-resume path does
// not pay to serialise Events/Devices. Callers that need the full view should
// issue a follow-up GET /vm.
type restoreResponse struct {
	ID    string `json:"id"`
	State string `json:"state"`
	MemMB uint64 `json:"mem_mb"`
}

// New creates a server with default options.
func New() *Server {
	return NewWithOptions(Options{})
}

// NewWithOptions creates a server with custom VM factory or default x86 boot mode.
func NewWithOptions(opts Options) *Server {
	mode, err := normalizeX86BootMode(opts.DefaultX86Boot, "")
	if err != nil {
		mode = vmm.X86BootAuto
	}
	s := &Server{
		defaultX86Boot: mode,
		factory:        opts.Factory,
		vmID:           strings.TrimSpace(opts.VMID),
	}
	if s.factory == nil {
		s.factory = defaultFactory
	}

	r := chi.NewRouter()
	r.Use(log.AccessLogMiddleware("api"))
	r.Use(middleware.Recoverer)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Server", "gocracker-vmm/0.1.0")
			next.ServeHTTP(w, r)
		})
	})

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
	r.Put("/shared-fs/{tag}", s.handleSharedFS)
	r.Put("/rate-limiters/net", s.handleNetRateLimiter)
	r.Put("/rate-limiters/block", s.handleBlockRateLimiter)
	r.Put("/rate-limiters/rng", s.handleRNGRateLimiter)
	r.Put("/actions", s.handleAction)
	r.Get("/vm", s.handleVMInfo)
	r.Get("/vsock/connect", s.handleVsockConnect)
	r.Get("/events", s.handleEvents)
	r.Get("/logs", s.handleLogs)
	r.Post("/snapshot", s.handleSnapshot)
	r.Post("/restore", s.handleRestore)
	r.Post("/migrations/prepare", s.handleMigrationPrepare)
	r.Post("/migrations/finalize", s.handleMigrationFinalize)
	r.Post("/migrations/reset", s.handleMigrationReset)
	r.Put("/configure-and-start", s.handleConfigureAndStart)

	s.router = r
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

// ListenUnix serves the API on a Unix socket.
func (s *Server) ListenUnix(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := seccomp.InstallWorkerProcessProfile(); err != nil {
		_ = ln.Close()
		return fmt.Errorf("install worker seccomp profile: %w", err)
	}
	log.API.Info("unix socket", "path", path)
	return http.Serve(ln, s)
}

// Close stops the VM, if any.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vm != nil {
		s.vm.Stop()
	}
}

func (s *Server) handleInstanceInfo(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	running := 0
	if s.vm != nil && s.vm.State() == vmm.StateRunning {
		running = 1
	}
	s.mu.RUnlock()
	_ = json.NewEncoder(w).Encode(InstanceInfo{
		ID:         "gocracker-0",
		State:      fmt.Sprintf("running=%d", running),
		AppName:    "gocracker",
		VMMVersion: "0.1.0",
	})
}

func (s *Server) handleVsockConnect(w http.ResponseWriter, r *http.Request) {
	port, err := parseUint32Value(r.URL.Query().Get("port"))
	if err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}

	s.mu.RLock()
	vm := s.vm
	started := s.started
	s.mu.RUnlock()
	if vm == nil || !started {
		apiErr(w, http.StatusConflict, "vm is not running")
		return
	}
	dialer, ok := vm.(vmm.VsockDialer)
	if !ok {
		apiErr(w, http.StatusConflict, "virtio-vsock is not configured")
		return
	}
	guestConn, err := dialer.DialVsock(port)
	if err != nil {
		apiErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := upgradeAndProxy(w, guestConn); err != nil {
		_ = guestConn.Close()
		apiErr(w, http.StatusInternalServerError, err.Error())
	}
}

func (s *Server) handleBootSource(w http.ResponseWriter, r *http.Request) {
	if err := s.rejectIfStarted(); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, http.StatusConflict), err.Error())
		return
	}
	var v BootSource
	if err := decodeJSONStrict(r, &v); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := firecrackerapi.ValidateBootSource(firecrackerapi.BootSource{
		KernelImagePath: v.KernelImagePath,
		BootArgs:        v.BootArgs,
		InitrdPath:      v.InitrdPath,
		X86Boot:         v.X86Boot,
	}); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, http.StatusBadRequest), err.Error())
		return
	}
	s.mu.Lock()
	s.preboot.bootSource = &v
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMachineConfig(w http.ResponseWriter, r *http.Request) {
	if err := s.rejectIfStarted(); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, http.StatusConflict), err.Error())
		return
	}
	var v MachineConfig
	if err := decodeJSONStrict(r, &v); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := firecrackerapi.ValidateMachineConfig(firecrackerapi.MachineConfig{
		VcpuCount:  v.VcpuCount,
		MemSizeMib: v.MemSizeMib,
	}); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, http.StatusBadRequest), err.Error())
		return
	}
	s.mu.Lock()
	s.preboot.machineCfg = &v
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleBalloonGet(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	vm := s.vm
	started := s.started
	prebootBalloon := s.preboot.balloon
	s.mu.RUnlock()
	if !started {
		if prebootBalloon == nil {
			apiErr(w, http.StatusBadRequest, "balloon is not configured")
			return
		}
		_ = json.NewEncoder(w).Encode(prebootBalloon)
		return
	}
	controller, ok := vm.(vmm.BalloonController)
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
	if err := s.rejectIfStarted(); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, http.StatusConflict), err.Error())
		return
	}
	var v Balloon
	if err := decodeJSONStrict(r, &v); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := firecrackerapi.ValidateBalloon(firecrackerapi.Balloon{
		AmountMib:             v.AmountMib,
		DeflateOnOOM:          v.DeflateOnOOM,
		StatsPollingIntervalS: v.StatsPollingIntervalS,
		FreePageHinting:       v.FreePageHinting,
		FreePageReporting:     v.FreePageReporting,
	}); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, http.StatusBadRequest), err.Error())
		return
	}
	s.mu.Lock()
	s.preboot.balloon = &v
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleBalloonPatch(w http.ResponseWriter, r *http.Request) {
	var v BalloonUpdate
	if err := decodeJSONStrict(r, &v); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := firecrackerapi.ValidateBalloonUpdate(firecrackerapi.BalloonUpdate{AmountMib: v.AmountMib}); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, http.StatusBadRequest), err.Error())
		return
	}
	s.mu.Lock()
	if !s.started {
		if s.preboot.balloon == nil {
			s.mu.Unlock()
			apiErr(w, http.StatusBadRequest, "balloon is not configured")
			return
		}
		s.preboot.balloon.AmountMib = v.AmountMib
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	vm := s.vm
	s.mu.Unlock()
	controller, ok := vm.(vmm.BalloonController)
	if !ok {
		apiErr(w, http.StatusConflict, "virtio-balloon is not configured")
		return
	}
	if err := controller.UpdateBalloon(vmm.BalloonUpdate{AmountMiB: v.AmountMib}); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleBalloonStatsGet(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	vm := s.vm
	started := s.started
	prebootBalloon := s.preboot.balloon
	s.mu.RUnlock()
	if !started {
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
	controller, ok := vm.(vmm.BalloonController)
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
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := firecrackerapi.ValidateBalloonStatsUpdate(firecrackerapi.BalloonStatsUpdate{
		StatsPollingIntervalS: v.StatsPollingIntervalS,
	}); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, http.StatusBadRequest), err.Error())
		return
	}
	s.mu.Lock()
	if !s.started {
		if s.preboot.balloon == nil {
			s.mu.Unlock()
			apiErr(w, http.StatusBadRequest, "balloon is not configured")
			return
		}
		s.preboot.balloon.StatsPollingIntervalS = v.StatsPollingIntervalS
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	vm := s.vm
	s.mu.Unlock()
	controller, ok := vm.(vmm.BalloonController)
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
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMemoryHotplugGet(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	started := s.started
	prebootCfg := cloneMemoryHotplug(s.preboot.memoryHotplug)
	prebootRequested := s.preboot.memoryHotplugRequestedMiB
	vm := s.vm
	s.mu.RUnlock()
	if !started {
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
	controller, ok := vm.(vmm.MemoryHotplugController)
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
	if !s.started {
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
	vm := s.vm
	s.mu.Unlock()
	controller, ok := vm.(vmm.MemoryHotplugController)
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
	if err := s.rejectIfStarted(); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, http.StatusConflict), err.Error())
		return
	}
	id := chi.URLParam(r, "drive_id")
	var v Drive
	if err := decodeJSONStrict(r, &v); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	v.DriveID = id
	if err := firecrackerapi.ValidateDrive(firecrackerapi.Drive{
		DriveID:      v.DriveID,
		PathOnHost:   v.PathOnHost,
		IsRootDevice: v.IsRootDevice,
	}); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, http.StatusBadRequest), err.Error())
		return
	}
	s.mu.Lock()
	s.preboot.drives = upsertDrive(s.preboot.drives, v)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleNetworkIface(w http.ResponseWriter, r *http.Request) {
	if err := s.rejectIfStarted(); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, http.StatusConflict), err.Error())
		return
	}
	id := chi.URLParam(r, "iface_id")
	var v NetworkInterface
	if err := decodeJSONStrict(r, &v); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	v.IfaceID = id
	if err := firecrackerapi.ValidateNetworkInterface(firecrackerapi.NetworkInterface{
		IfaceID:     v.IfaceID,
		HostDevName: v.HostDevName,
		GuestMAC:    v.GuestMAC,
	}); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, http.StatusBadRequest), err.Error())
		return
	}
	s.mu.Lock()
	s.preboot.netIfaces = upsertNetworkInterface(s.preboot.netIfaces, v)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSharedFS(w http.ResponseWriter, r *http.Request) {
	if err := s.rejectIfStarted(); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, http.StatusConflict), err.Error())
		return
	}
	tag := chi.URLParam(r, "tag")
	var v SharedFS
	if err := decodeJSONStrict(r, &v); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	v.Tag = tag
	if strings.TrimSpace(v.Source) == "" {
		apiErr(w, http.StatusBadRequest, "source is required")
		return
	}
	s.mu.Lock()
	s.preboot.sharedFS = upsertSharedFS(s.preboot.sharedFS, v)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func upsertSharedFS(list []SharedFS, v SharedFS) []SharedFS {
	for i := range list {
		if list[i].Tag == v.Tag {
			list[i] = v
			return list
		}
	}
	return append(list, v)
}

func (s *Server) handleNetRateLimiter(w http.ResponseWriter, r *http.Request) {
	var cfg vmm.RateLimiterConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.updateNetRateLimiter(&cfg); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleBlockRateLimiter(w http.ResponseWriter, r *http.Request) {
	var cfg vmm.RateLimiterConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.updateBlockRateLimiter(&cfg); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRNGRateLimiter(w http.ResponseWriter, r *http.Request) {
	var cfg vmm.RateLimiterConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.updateRNGRateLimiter(&cfg); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	var a Action
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	switch a.ActionType {
	case "InstanceStart":
		if err := s.startPrebootVM(); err != nil {
			apiErr(w, firecrackerapi.StatusCode(err, http.StatusBadRequest), err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case "InstanceStop":
		if err := s.stopVM(); err != nil {
			apiErr(w, firecrackerapi.StatusCode(err, http.StatusBadRequest), err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		apiErr(w, http.StatusBadRequest, "unknown action: "+a.ActionType)
	}
}

// handleConfigureAndStart applies all pre-boot configuration and starts the VM.
func (s *Server) handleConfigureAndStart(w http.ResponseWriter, r *http.Request) {
	if err := s.rejectIfStarted(); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, http.StatusConflict), err.Error())
		return
	}
	var req ConfigureAndStartRequest
	if err := decodeJSONStrict(r, &req); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// Validate boot source (required).
	if err := firecrackerapi.ValidateBootSource(firecrackerapi.BootSource{
		KernelImagePath: req.BootSource.KernelImagePath,
		BootArgs:        req.BootSource.BootArgs,
		InitrdPath:      req.BootSource.InitrdPath,
		X86Boot:         req.BootSource.X86Boot,
	}); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, http.StatusBadRequest), err.Error())
		return
	}

	// Validate machine config (optional but usually present).
	if req.MachineConfig != nil {
		if err := firecrackerapi.ValidateMachineConfig(firecrackerapi.MachineConfig{
			VcpuCount:  req.MachineConfig.VcpuCount,
			MemSizeMib: req.MachineConfig.MemSizeMib,
		}); err != nil {
			apiErr(w, firecrackerapi.StatusCode(err, http.StatusBadRequest), err.Error())
			return
		}
	}

	// Validate balloon (optional).
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
	}

	// Validate memory hotplug (optional).
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

	// Validate drives.
	for i := range req.Drives {
		if err := firecrackerapi.ValidateDrive(firecrackerapi.Drive{
			DriveID:      req.Drives[i].DriveID,
			PathOnHost:   req.Drives[i].PathOnHost,
			IsRootDevice: req.Drives[i].IsRootDevice,
		}); err != nil {
			apiErr(w, firecrackerapi.StatusCode(err, http.StatusBadRequest), err.Error())
			return
		}
	}

	// Validate network interfaces.
	for i := range req.NetworkInterfaces {
		if err := firecrackerapi.ValidateNetworkInterface(firecrackerapi.NetworkInterface{
			IfaceID:     req.NetworkInterfaces[i].IfaceID,
			HostDevName: req.NetworkInterfaces[i].HostDevName,
			GuestMAC:    req.NetworkInterfaces[i].GuestMAC,
		}); err != nil {
			apiErr(w, firecrackerapi.StatusCode(err, http.StatusBadRequest), err.Error())
			return
		}
	}

	// Validate shared FS entries.
	for i := range req.SharedFS {
		if strings.TrimSpace(req.SharedFS[i].Source) == "" {
			apiErr(w, http.StatusBadRequest, "source is required for shared_fs entry")
			return
		}
	}

	// All validation passed — apply config under a single lock.
	s.mu.Lock()
	s.preboot.bootSource = &req.BootSource
	if req.MachineConfig != nil {
		s.preboot.machineCfg = req.MachineConfig
	}
	if req.Balloon != nil {
		s.preboot.balloon = req.Balloon
	}
	if req.MemoryHotplug != nil {
		s.preboot.memoryHotplug = cloneMemoryHotplug(req.MemoryHotplug)
		s.preboot.memoryHotplugRequestedMiB = 0
	}
	for _, d := range req.Drives {
		s.preboot.drives = upsertDrive(s.preboot.drives, d)
	}
	for _, ni := range req.NetworkInterfaces {
		s.preboot.netIfaces = upsertNetworkInterface(s.preboot.netIfaces, ni)
	}
	for _, fs := range req.SharedFS {
		s.preboot.sharedFS = upsertSharedFS(s.preboot.sharedFS, fs)
	}
	s.mu.Unlock()

	// Start the VM.
	if err := s.startPrebootVM(); err != nil {
		apiErr(w, firecrackerapi.StatusCode(err, http.StatusBadRequest), err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	vm, err := s.currentVM()
	if err != nil {
		apiErr(w, http.StatusNotFound, err.Error())
		return
	}
	var since time.Time
	if q := r.URL.Query().Get("since"); q != "" {
		t, err := time.Parse(time.RFC3339, q)
		if err != nil {
			apiErr(w, http.StatusBadRequest, "invalid since: "+err.Error())
			return
		}
		since = t
	}
	_ = json.NewEncoder(w).Encode(vm.Events().Events(since))
}

func (s *Server) handleVMInfo(w http.ResponseWriter, r *http.Request) {
	vm, err := s.currentVM()
	if err != nil {
		apiErr(w, http.StatusNotFound, err.Error())
		return
	}
	cfg := vm.VMConfig()
	_ = json.NewEncoder(w).Encode(VMInfo{
		ID:            vm.ID(),
		State:         vm.State().String(),
		Uptime:        vm.Uptime().String(),
		MemMB:         cfg.MemMB,
		Kernel:        cfg.KernelPath,
		Events:        vm.Events().Events(time.Time{}),
		Devices:       vm.DeviceList(),
		FirstOutputAt: vm.FirstOutputAt(),
	})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	vm, err := s.currentVM()
	if err != nil {
		apiErr(w, http.StatusNotFound, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(vm.ConsoleOutput())
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	vm, err := s.currentVM()
	if err != nil {
		apiErr(w, http.StatusNotFound, err.Error())
		return
	}
	var req SnapshotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.DestDir) == "" {
		apiErr(w, http.StatusBadRequest, "dest_dir is required")
		return
	}
	snap, err := vm.TakeSnapshot(req.DestDir)
	if err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = json.NewEncoder(w).Encode(snap)
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	if err := s.rejectIfStarted(); err != nil {
		apiErr(w, http.StatusConflict, err.Error())
		return
	}
	var req RestoreRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.SnapshotDir) == "" {
		apiErr(w, http.StatusBadRequest, "snapshot_dir is required")
		return
	}
	mode, err := normalizeX86BootMode(s.defaultX86Boot, req.X86Boot)
	if err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	vm, err := vmm.RestoreFromSnapshotWithOptions(req.SnapshotDir, vmm.RestoreOptions{
		OverrideTap:     req.TapName,
		OverrideVCPUs:   req.VcpuCount,
		OverrideX86Boot: mode,
	})
	if err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Resume {
		if err := vm.Start(); err != nil {
			vm.Stop()
			apiErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	s.mu.Lock()
	s.vm = vm
	s.started = true
	s.mu.Unlock()
	// Minimal response payload. The full VMInfo with Events + DeviceList
	// ran ~0.4 ms of JSON encoding plus 1–2 KB of socket write per restore —
	// visible at this end of the latency budget. Callers that need the full
	// view can issue a GET /vm afterwards; they rarely do, because the VM ID
	// is stable and Events can be streamed over SSE.
	cfg := vm.VMConfig()
	_ = json.NewEncoder(w).Encode(restoreResponse{
		ID:    vm.ID(),
		State: vm.State().String(),
		MemMB: cfg.MemMB,
	})
}

func (s *Server) handleMigrationPrepare(w http.ResponseWriter, r *http.Request) {
	vm, err := s.currentVM()
	if err != nil {
		apiErr(w, http.StatusConflict, err.Error())
		return
	}
	var req SnapshotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.DestDir == "" {
		apiErr(w, http.StatusBadRequest, "dest_dir is required")
		return
	}
	migrator, ok := vm.(migrationCapable)
	if !ok {
		apiErr(w, http.StatusConflict, "migration prepare is not supported by this VM backend")
		return
	}
	if err := migrator.PrepareMigrationBundle(req.DestDir); err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMigrationFinalize(w http.ResponseWriter, r *http.Request) {
	vm, err := s.currentVM()
	if err != nil {
		apiErr(w, http.StatusConflict, err.Error())
		return
	}
	var req SnapshotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.DestDir == "" {
		apiErr(w, http.StatusBadRequest, "dest_dir is required")
		return
	}
	migrator, ok := vm.(migrationCapable)
	if !ok {
		apiErr(w, http.StatusConflict, "migration finalize is not supported by this VM backend")
		return
	}
	snap, patches, err := migrator.FinalizeMigrationBundle(req.DestDir)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = json.NewEncoder(w).Encode(struct {
		Snapshot *vmm.Snapshot          `json:"snapshot"`
		Patches  *vmm.MigrationPatchSet `json:"patches,omitempty"`
	}{Snapshot: snap, Patches: patches})
}

func (s *Server) handleMigrationReset(w http.ResponseWriter, r *http.Request) {
	vm, err := s.currentVM()
	if err != nil {
		apiErr(w, http.StatusConflict, err.Error())
		return
	}
	migrator, ok := vm.(migrationCapable)
	if !ok {
		apiErr(w, http.StatusConflict, "migration reset is not supported by this VM backend")
		return
	}
	if err := migrator.ResetMigrationTracking(); err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) updateNetRateLimiter(cfg *vmm.RateLimiterConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started && s.vm != nil {
		updater, ok := s.vm.(rateLimiterCapable)
		if !ok {
			return fmt.Errorf("rate limiter update is not supported by this VM backend")
		}
		return updater.UpdateNetRateLimiter(cloneVMLimiter(cfg))
	}
	if len(s.preboot.netIfaces) == 0 {
		return fmt.Errorf("network-interface not configured")
	}
	s.preboot.netIfaces[0].RateLimiter = cloneVMLimiter(cfg)
	return nil
}

func (s *Server) updateBlockRateLimiter(cfg *vmm.RateLimiterConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started && s.vm != nil {
		updater, ok := s.vm.(rateLimiterCapable)
		if !ok {
			return fmt.Errorf("rate limiter update is not supported by this VM backend")
		}
		return updater.UpdateBlockRateLimiter(cloneVMLimiter(cfg))
	}
	for i := range s.preboot.drives {
		if s.preboot.drives[i].IsRootDevice {
			s.preboot.drives[i].RateLimiter = cloneVMLimiter(cfg)
			return nil
		}
	}
	return fmt.Errorf("root drive not configured")
}

func (s *Server) updateRNGRateLimiter(cfg *vmm.RateLimiterConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started && s.vm != nil {
		updater, ok := s.vm.(rateLimiterCapable)
		if !ok {
			return fmt.Errorf("rate limiter update is not supported by this VM backend")
		}
		return updater.UpdateRNGRateLimiter(cloneVMLimiter(cfg))
	}
	if s.preboot.machineCfg == nil {
		s.preboot.machineCfg = &MachineConfig{}
	}
	s.preboot.machineCfg.RNGRateLimiter = cloneVMLimiter(cfg)
	return nil
}

func cloneVMLimiter(cfg *vmm.RateLimiterConfig) *vmm.RateLimiterConfig {
	if cfg == nil {
		return nil
	}
	cloned := *cfg
	return &cloned
}

func cloneMemoryHotplug(cfg *vmm.MemoryHotplugConfig) *vmm.MemoryHotplugConfig {
	if cfg == nil {
		return nil
	}
	cloned := *cfg
	return &cloned
}

func (s *Server) startPrebootVM() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return firecrackerapi.InvalidStatef("instance already started")
	}
	spec := firecrackerapi.PrebootConfig{
		DefaultVCPUs: 1,
		DefaultMemMB: 128,
	}
	if s.preboot.bootSource != nil {
		spec.BootSource = &firecrackerapi.BootSource{
			KernelImagePath: s.preboot.bootSource.KernelImagePath,
			BootArgs:        s.preboot.bootSource.BootArgs,
			InitrdPath:      s.preboot.bootSource.InitrdPath,
			X86Boot:         s.preboot.bootSource.X86Boot,
		}
	}
	if s.preboot.machineCfg != nil {
		spec.MachineCfg = &firecrackerapi.MachineConfig{
			VcpuCount:  s.preboot.machineCfg.VcpuCount,
			MemSizeMib: s.preboot.machineCfg.MemSizeMib,
		}
	}
	if s.preboot.balloon != nil {
		spec.Balloon = &firecrackerapi.Balloon{
			AmountMib:             s.preboot.balloon.AmountMib,
			DeflateOnOOM:          s.preboot.balloon.DeflateOnOOM,
			StatsPollingIntervalS: s.preboot.balloon.StatsPollingIntervalS,
			FreePageHinting:       s.preboot.balloon.FreePageHinting,
			FreePageReporting:     s.preboot.balloon.FreePageReporting,
		}
	}
	if s.preboot.memoryHotplug != nil {
		spec.MemoryHotplug = &firecrackerapi.MemoryHotplugConfig{
			TotalSizeMib: s.preboot.memoryHotplug.TotalSizeMiB,
			SlotSizeMib:  s.preboot.memoryHotplug.SlotSizeMiB,
			BlockSizeMib: s.preboot.memoryHotplug.BlockSizeMiB,
		}
	}
	spec.Drives = make([]firecrackerapi.Drive, 0, len(s.preboot.drives))
	for _, drive := range s.preboot.drives {
		spec.Drives = append(spec.Drives, firecrackerapi.Drive{
			DriveID:      drive.DriveID,
			PathOnHost:   drive.PathOnHost,
			IsRootDevice: drive.IsRootDevice,
		})
	}
	spec.NetIfaces = make([]firecrackerapi.NetworkInterface, 0, len(s.preboot.netIfaces))
	for _, iface := range s.preboot.netIfaces {
		spec.NetIfaces = append(spec.NetIfaces, firecrackerapi.NetworkInterface{
			IfaceID:     iface.IfaceID,
			HostDevName: iface.HostDevName,
			GuestMAC:    iface.GuestMAC,
		})
	}
	if err := firecrackerapi.ValidatePrebootForStart(spec); err != nil {
		return err
	}
	requestedHotplugMiB := s.preboot.memoryHotplugRequestedMiB

	mode, err := normalizeX86BootMode(s.defaultX86Boot, s.preboot.bootSource.X86Boot)
	if err != nil {
		return err
	}
	cfg := vmm.Config{
		ID:         s.vmConfigID(),
		KernelPath: s.preboot.bootSource.KernelImagePath,
		Cmdline:    s.preboot.bootSource.BootArgs,
		InitrdPath: s.preboot.bootSource.InitrdPath,
		VCPUs:      1,
		MemMB:      128,
		X86Boot:    mode,
	}
	if s.preboot.machineCfg != nil {
		if s.preboot.machineCfg.MemSizeMib > 0 {
			cfg.MemMB = uint64(s.preboot.machineCfg.MemSizeMib)
		}
		if s.preboot.machineCfg.VcpuCount > 0 {
			cfg.VCPUs = s.preboot.machineCfg.VcpuCount
		}
		cfg.RNGRateLimiter = cloneVMLimiter(s.preboot.machineCfg.RNGRateLimiter)
		if s.preboot.machineCfg.VsockEnabled {
			cfg.Vsock = &vmm.VsockConfig{
				Enabled:  true,
				GuestCID: s.preboot.machineCfg.VsockGuestCID,
			}
		}
		if s.preboot.machineCfg.ExecEnabled {
			cfg.Exec = &vmm.ExecConfig{
				Enabled:   true,
				VsockPort: s.preboot.machineCfg.ExecVsockPort,
			}
		}
	}
	if s.preboot.balloon != nil {
		cfg.Balloon = &vmm.BalloonConfig{
			AmountMiB:             s.preboot.balloon.AmountMib,
			DeflateOnOOM:          s.preboot.balloon.DeflateOnOOM,
			StatsPollingIntervalS: s.preboot.balloon.StatsPollingIntervalS,
		}
	}
	if s.preboot.memoryHotplug != nil {
		cfg.MemoryHotplug = &vmm.MemoryHotplugConfig{
			TotalSizeMiB: s.preboot.memoryHotplug.TotalSizeMiB,
			SlotSizeMiB:  s.preboot.memoryHotplug.SlotSizeMiB,
			BlockSizeMiB: s.preboot.memoryHotplug.BlockSizeMiB,
		}
	}
	if len(s.preboot.drives) > 1 {
		cfg.Drives = make([]vmm.DriveConfig, 0, len(s.preboot.drives))
	}
	for _, d := range s.preboot.drives {
		driveCfg := vmm.DriveConfig{
			ID:          d.DriveID,
			Path:        d.PathOnHost,
			Root:        d.IsRootDevice,
			ReadOnly:    d.IsReadOnly,
			RateLimiter: cloneVMLimiter(d.RateLimiter),
		}
		if d.IsRootDevice {
			cfg.DiskImage = d.PathOnHost
			cfg.DiskRO = d.IsReadOnly
			cfg.BlockRateLimiter = cloneVMLimiter(d.RateLimiter)
		}
		if cfg.Drives != nil {
			cfg.Drives = append(cfg.Drives, driveCfg)
		}
	}
	if len(s.preboot.netIfaces) > 0 {
		cfg.TapName = s.preboot.netIfaces[0].HostDevName
		cfg.NetRateLimiter = cloneVMLimiter(s.preboot.netIfaces[0].RateLimiter)
		if mac := strings.TrimSpace(s.preboot.netIfaces[0].GuestMAC); mac != "" {
			hw, err := net.ParseMAC(mac)
			if err != nil {
				return fmt.Errorf("parse guest_mac: %w", err)
			}
			cfg.MACAddr = hw
		}
	}
	if len(s.preboot.sharedFS) > 0 {
		cfg.SharedFS = make([]vmm.SharedFSConfig, 0, len(s.preboot.sharedFS))
		for _, fs := range s.preboot.sharedFS {
			cfg.SharedFS = append(cfg.SharedFS, vmm.SharedFSConfig{
				Tag:        fs.Tag,
				Source:     fs.Source,
				SocketPath: fs.SocketPath,
			})
		}
	}

	vm, err := s.factory(cfg)
	if err != nil {
		return err
	}
	s.vm = vm
	s.started = true
	if err := vm.Start(); err != nil {
		return err
	}
	if requestedHotplugMiB > 0 {
		controller, ok := vm.(vmm.MemoryHotplugController)
		if !ok {
			vm.Stop()
			s.vm = nil
			s.started = false
			return fmt.Errorf("memory hotplug is not configured")
		}
		if err := controller.UpdateMemoryHotplug(vmm.MemoryHotplugSizeUpdate{RequestedSizeMiB: requestedHotplugMiB}); err != nil {
			vm.Stop()
			s.vm = nil
			s.started = false
			return fmt.Errorf("apply preboot memory hotplug target: %w", err)
		}
	}
	return nil
}

func (s *Server) vmConfigID() string {
	if id := strings.TrimSpace(s.vmID); id != "" {
		return id
	}
	return "root-vm"
}

func (s *Server) stopVM() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vm == nil {
		return firecrackerapi.InvalidStatef("instance not started")
	}
	s.vm.Stop()
	return nil
}

func (s *Server) currentVM() (VM, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.vm == nil {
		return nil, fmt.Errorf("instance not started")
	}
	return s.vm, nil
}

type migrationCapable interface {
	PrepareMigrationBundle(string) error
	FinalizeMigrationBundle(string) (*vmm.Snapshot, *vmm.MigrationPatchSet, error)
	ResetMigrationTracking() error
}

type rateLimiterCapable interface {
	UpdateNetRateLimiter(*vmm.RateLimiterConfig) error
	UpdateBlockRateLimiter(*vmm.RateLimiterConfig) error
	UpdateRNGRateLimiter(*vmm.RateLimiterConfig) error
}

func (s *Server) rejectIfStarted() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.started {
		return firecrackerapi.InvalidStatef("instance already started")
	}
	return nil
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

func defaultFactory(cfg vmm.Config) (VM, error) {
	if cfg.ConsoleOut == nil {
		cfg.ConsoleOut = io.Discard
	}
	if cfg.ConsoleIn == nil {
		cfg.ConsoleIn = bytes.NewReader(nil)
	}
	return vmm.New(cfg)
}

type Client struct {
	httpClient *http.Client
	baseURL    string
	socketPath string
}

func NewClient(socketPath string) *Client {
	transport := &http.Transport{}
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socketPath)
	}
	return &Client{
		httpClient: &http.Client{Transport: transport},
		baseURL:    "http://unix",
		socketPath: socketPath,
	}
}

func (c *Client) SetBootSource(ctx context.Context, body BootSource) error {
	return c.putJSON(ctx, http.MethodPut, "/boot-source", body)
}

func (c *Client) SetMachineConfig(ctx context.Context, body MachineConfig) error {
	return c.putJSON(ctx, http.MethodPut, "/machine-config", body)
}

func (c *Client) GetBalloon(ctx context.Context) (Balloon, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/balloon", nil)
	if err != nil {
		return Balloon{}, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Balloon{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return Balloon{}, decodeAPIError(resp.Body, resp.StatusCode)
	}
	var out Balloon
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Balloon{}, err
	}
	return out, nil
}

func (c *Client) SetBalloon(ctx context.Context, body Balloon) error {
	return c.putJSON(ctx, http.MethodPut, "/balloon", body)
}

func (c *Client) PatchBalloon(ctx context.Context, body BalloonUpdate) error {
	return c.putJSON(ctx, http.MethodPatch, "/balloon", body)
}

func (c *Client) GetBalloonStats(ctx context.Context) (vmm.BalloonStats, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/balloon/statistics", nil)
	if err != nil {
		return vmm.BalloonStats{}, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return vmm.BalloonStats{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return vmm.BalloonStats{}, decodeAPIError(resp.Body, resp.StatusCode)
	}
	var out vmm.BalloonStats
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return vmm.BalloonStats{}, err
	}
	return out, nil
}

func (c *Client) PatchBalloonStats(ctx context.Context, body BalloonStatsUpdate) error {
	return c.putJSON(ctx, http.MethodPatch, "/balloon/statistics", body)
}

func (c *Client) GetMemoryHotplug(ctx context.Context) (MemoryHotplugStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/hotplug/memory", nil)
	if err != nil {
		return MemoryHotplugStatus{}, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return MemoryHotplugStatus{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return MemoryHotplugStatus{}, decodeAPIError(resp.Body, resp.StatusCode)
	}
	var out MemoryHotplugStatus
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return MemoryHotplugStatus{}, err
	}
	return out, nil
}

func (c *Client) SetMemoryHotplug(ctx context.Context, body MemoryHotplugConfig) error {
	return c.putJSON(ctx, http.MethodPut, "/hotplug/memory", body)
}

func (c *Client) PatchMemoryHotplug(ctx context.Context, body MemoryHotplugSizeUpdate) error {
	return c.putJSON(ctx, http.MethodPatch, "/hotplug/memory", body)
}

func (c *Client) SetDrive(ctx context.Context, driveID string, body Drive) error {
	return c.putJSON(ctx, http.MethodPut, "/drives/"+driveID, body)
}

func (c *Client) SetNetworkInterface(ctx context.Context, ifaceID string, body NetworkInterface) error {
	return c.putJSON(ctx, http.MethodPut, "/network-interfaces/"+ifaceID, body)
}

func (c *Client) SetSharedFS(ctx context.Context, tag string, body SharedFS) error {
	return c.putJSON(ctx, http.MethodPut, "/shared-fs/"+tag, body)
}

func (c *Client) SetNetRateLimiter(ctx context.Context, body vmm.RateLimiterConfig) error {
	return c.putJSON(ctx, http.MethodPut, "/rate-limiters/net", body)
}

func (c *Client) SetBlockRateLimiter(ctx context.Context, body vmm.RateLimiterConfig) error {
	return c.putJSON(ctx, http.MethodPut, "/rate-limiters/block", body)
}

func (c *Client) SetRNGRateLimiter(ctx context.Context, body vmm.RateLimiterConfig) error {
	return c.putJSON(ctx, http.MethodPut, "/rate-limiters/rng", body)
}

func (c *Client) Start(ctx context.Context) error {
	return c.putJSON(ctx, http.MethodPut, "/actions", Action{ActionType: "InstanceStart"})
}

// ConfigureAndStart configures and starts the VM in a single RPC.
func (c *Client) ConfigureAndStart(ctx context.Context, req ConfigureAndStartRequest) error {
	return c.putJSON(ctx, http.MethodPut, "/configure-and-start", req)
}

func (c *Client) GetInfo(ctx context.Context) (VMInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/vm", nil)
	if err != nil {
		return VMInfo{}, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return VMInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return VMInfo{}, decodeAPIError(resp.Body, resp.StatusCode)
	}
	var info VMInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return VMInfo{}, err
	}
	return info, nil
}

func (c *Client) GetEvents(ctx context.Context, since time.Time) ([]vmm.Event, error) {
	url := c.baseURL + "/events"
	if !since.IsZero() {
		url += "?since=" + since.Format(time.RFC3339)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, decodeAPIError(resp.Body, resp.StatusCode)
	}
	var events []vmm.Event
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		return nil, err
	}
	return events, nil
}

func (c *Client) GetLogs(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/logs", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, decodeAPIError(resp.Body, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func (c *Client) DialVsock(ctx context.Context, port uint32) (net.Conn, error) {
	if c.socketPath == "" {
		return nil, fmt.Errorf("worker socket path is not configured")
	}
	var d net.Dialer
	rawConn, err := d.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return nil, err
	}
	return upgradeClientConn(rawConn, fmt.Sprintf("/vsock/connect?port=%d", port))
}

func (c *Client) Stop(ctx context.Context) error {
	return c.putJSON(ctx, http.MethodPut, "/actions", Action{ActionType: "InstanceStop"})
}

func (c *Client) Snapshot(ctx context.Context, reqBody SnapshotRequest) (*vmm.Snapshot, error) {
	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/snapshot", strings.NewReader(string(data)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, decodeAPIError(resp.Body, resp.StatusCode)
	}
	var snap vmm.Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

func (c *Client) Restore(ctx context.Context, reqBody RestoreRequest) (VMInfo, error) {
	var info VMInfo
	if err := c.doJSON(ctx, http.MethodPost, "/restore", reqBody, &info); err != nil {
		return VMInfo{}, err
	}
	return info, nil
}

func (c *Client) PrepareMigrationBundle(ctx context.Context, reqBody SnapshotRequest) error {
	return c.doJSON(ctx, http.MethodPost, "/migrations/prepare", reqBody, nil)
}

func (c *Client) FinalizeMigrationBundle(ctx context.Context, reqBody SnapshotRequest) (*vmm.Snapshot, *vmm.MigrationPatchSet, error) {
	var resp struct {
		Snapshot *vmm.Snapshot          `json:"snapshot"`
		Patches  *vmm.MigrationPatchSet `json:"patches,omitempty"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/migrations/finalize", reqBody, &resp); err != nil {
		return nil, nil, err
	}
	return resp.Snapshot, resp.Patches, nil
}

func (c *Client) ResetMigrationTracking(ctx context.Context) error {
	return c.doJSON(ctx, http.MethodPost, "/migrations/reset", nil, nil)
}

func (c *Client) putJSON(ctx context.Context, method, path string, body any) error {
	return c.doJSON(ctx, method, path, body, nil)
}

func (c *Client) doJSON(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = strings.NewReader(string(data))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return decodeAPIError(resp.Body, resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func decodeAPIError(body io.Reader, status int) error {
	var apiErr APIError
	if err := json.NewDecoder(body).Decode(&apiErr); err == nil && apiErr.FaultMessage != "" {
		return fmt.Errorf("worker status %d: %s", status, apiErr.FaultMessage)
	}
	data, _ := io.ReadAll(body)
	return fmt.Errorf("worker status %d: %s", status, strings.TrimSpace(string(data)))
}

func parseUint32Value(raw string) (uint32, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("port is required")
	}
	var parsed uint64
	if _, err := fmt.Sscanf(raw, "%d", &parsed); err != nil {
		return 0, fmt.Errorf("invalid uint32 value %q", raw)
	}
	if parsed > uint64(^uint32(0)) {
		return 0, fmt.Errorf("value %q exceeds uint32", raw)
	}
	return uint32(parsed), nil
}

func upgradeAndProxy(w http.ResponseWriter, guestConn net.Conn) error {
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
	go proxyConnPair(clientConn, guestConn)
	return nil
}

func upgradeClientConn(rawConn net.Conn, path string) (net.Conn, error) {
	if _, err := fmt.Fprintf(rawConn, "GET %s HTTP/1.1\r\nHost: unix\r\nConnection: Upgrade\r\nUpgrade: vsock\r\n\r\n", path); err != nil {
		_ = rawConn.Close()
		return nil, err
	}
	reader := bufio.NewReader(rawConn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		_ = rawConn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		return nil, decodeAPIError(resp.Body, resp.StatusCode)
	}
	return &bufferedConn{Conn: rawConn, reader: reader}, nil
}

func proxyConnPair(clientConn, guestConn net.Conn) {
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

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	if c.reader != nil && c.reader.Buffered() > 0 {
		return c.reader.Read(p)
	}
	return c.Conn.Read(p)
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

func apiErr(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(APIError{FaultMessage: msg})
}
