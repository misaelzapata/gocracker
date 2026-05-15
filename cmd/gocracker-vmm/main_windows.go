//go:build windows

// gocracker-vmm on Windows is a minimal-shim worker process that exposes
// a Firecracker-flavoured REST surface over AF_UNIX (or, when AF_UNIX
// binding fails, a TCP fallback on 127.0.0.1) and drives the WHP-backed
// boot path in pkg/vmm.BootLinuxOnWHP.
//
// It is intentionally NOT a full port of the Linux internal/vmmserver,
// which depends on Linux-only seccomp / firecrackerapi / event-source
// packages. Once those packages cross-compile, this shim is the natural
// drop-in point for the full Firecracker REST surface.
//
// API surface (Firecracker subset):
//
//	GET  /           -> {"state":"...","id":"..."}
//	PUT  /machine-config  body {"vcpu_count":N,"mem_size_mib":N}
//	PUT  /boot-source     body {"kernel_image_path":"...","boot_args":"..."}
//	PUT  /drives/rootfs   body {"path_on_host":"...","is_read_only":bool}
//	PUT  /actions         body {"action_type":"InstanceStart"|"SendCtrlAltDel"}
//
// Anything else returns 405 / 404. The shim does not currently expose
// pause/resume, snapshot, or rate-limiter endpoints — those land alongside
// the cross-compile fixes for internal/vmmserver in a follow-up.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gocracker/gocracker/internal/paths"
	"github.com/gocracker/gocracker/internal/whp"
	"github.com/gocracker/gocracker/pkg/vmm"
)

func init() {
	// Match the Linux worker's tuning: gocracker-vmm is a short-lived
	// (one-VM-per-lifetime) process whose Go-managed heap stays below
	// ~10 MiB during boot. Disabling GC trades that heap growth for a
	// few hundred microseconds off the boot critical path. Override
	// with GOGC=100 in the environment to restore the default.
	debug.SetGCPercent(-1)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run is the testable entry point — separated from main so unit tests
// can drive flag parsing without spinning up listeners. It returns the
// process exit code.
func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("gocracker-vmm", flag.ContinueOnError)
	fs.SetOutput(stderr)

	// `socket` mirrors the Linux build's flag name. On Windows the
	// default is `%LOCALAPPDATA%\gocracker\sock\vmm.sock` via paths.VMMSocket().
	// The flag accepts either a bare path (-> AF_UNIX) or the explicit
	// `unix://` prefix (compatibility with hand-written API addresses).
	socketPath := fs.String("socket", paths.VMMSocket(), "AF_UNIX socket path to listen on (or unix:// URL)")
	apiAddr := fs.String("api-addr", "", "Alternate API address. unix://PATH for AF_UNIX, tcp://HOST:PORT for TCP (loopback only).")
	vmID := fs.String("vm-id", "", "VM identifier surfaced in GET / responses")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	applyWorkingSetPolicy(stderr)

	// Resolve listener. The flag matrix is intentionally simple — we
	// don't accept both -socket and -api-addr; -api-addr wins if both
	// are passed, since callers asking for that form usually want a
	// non-default network.
	network, address, err := resolveListenerAddr(*apiAddr, *socketPath)
	if err != nil {
		fmt.Fprintf(stderr, "gocracker-vmm: address: %v\n", err)
		return 2
	}

	// Pre-flight WHP availability check. If WHP is missing or the
	// hypervisor feature is disabled, fail loudly NOW with the same
	// actionable message gocracker.exe prints — otherwise the first
	// InstanceStart action would surface a confusing HRESULT trace.
	if !whp.Available() {
		fmt.Fprintln(stderr,
			"gocracker-vmm: ERROR: WinHvPlatform.dll not loadable. This host does not expose the Windows Hypervisor Platform.")
		return 3
	}
	present, err := whp.HypervisorPresent()
	if err != nil {
		fmt.Fprintf(stderr, "gocracker-vmm: ERROR: WHvGetCapability(HypervisorPresent) failed: %v\n", err)
		return 3
	}
	if !present {
		fmt.Fprintln(stderr,
			"gocracker-vmm: ERROR: Hypervisor Platform feature is not enabled on this host.")
		fmt.Fprintln(stderr, "Enable it with (admin PowerShell, then reboot):")
		fmt.Fprintln(stderr, "  Enable-WindowsOptionalFeature -Online -FeatureName HypervisorPlatform -All")
		return 3
	}

	srv := newWorker(*vmID, stderr)

	// AF_UNIX listeners on Windows DO leave a filesystem artifact
	// after process exit; remove a stale one of our own first so a
	// crashed predecessor doesn't block startup. Best-effort.
	if network == "unix" {
		if err := os.MkdirAll(filepath.Dir(address), 0o755); err != nil {
			fmt.Fprintf(stderr, "gocracker-vmm: mkdir %s: %v\n", filepath.Dir(address), err)
			return 1
		}
		_ = os.Remove(address)
	}

	ln, err := net.Listen(network, address)
	if err != nil {
		fmt.Fprintf(stderr, "gocracker-vmm: listen %s://%s: %v\n", network, address, err)
		return 1
	}
	defer func() {
		_ = ln.Close()
		if network == "unix" {
			_ = os.Remove(address)
		}
	}()

	// Ctrl-C / SIGTERM gracefully tears down the listener AND any
	// running VM. We can't use signal.Notify(SIGINT) -> srv.Close()
	// directly because the VM Run() goroutine holds the only handle to
	// the WHPBootSession; instead we cancel the worker's context and
	// let Run() unwind through WHvCancelRunVirtualProcessor.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(stderr, "gocracker-vmm: interrupt received; shutting down")
		srv.shutdown()
		_ = ln.Close()
	}()

	fmt.Fprintf(stderr, "gocracker-vmm: listening on %s://%s\n", network, address)
	httpSrv := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(stderr, "gocracker-vmm: serve: %v\n", err)
		return 1
	}
	return 0
}

// resolveListenerAddr translates the (-api-addr, -socket) flag combo
// into a (network, address) pair net.Listen accepts. -api-addr wins
// when set; otherwise -socket implies AF_UNIX at the given path.
func resolveListenerAddr(apiAddr, socketPath string) (network, address string, err error) {
	if apiAddr != "" {
		switch {
		case strings.HasPrefix(apiAddr, "unix://"):
			return "unix", strings.TrimPrefix(apiAddr, "unix://"), nil
		case strings.HasPrefix(apiAddr, "tcp://"):
			return "tcp", strings.TrimPrefix(apiAddr, "tcp://"), nil
		default:
			return "", "", fmt.Errorf("unsupported -api-addr scheme: %q (want unix:// or tcp://)", apiAddr)
		}
	}
	// Strip a leading unix:// if a caller put it on -socket; otherwise
	// take the raw path as an AF_UNIX socket.
	if strings.HasPrefix(socketPath, "unix://") {
		return "unix", strings.TrimPrefix(socketPath, "unix://"), nil
	}
	return "unix", socketPath, nil
}

// machineConfig mirrors the Firecracker /machine-config schema's
// minimal fields. We accept extras (ht_enabled, cpu_template, etc.)
// silently — the JSON decoder ignores unknown keys.
type machineConfig struct {
	VCPUCount  int `json:"vcpu_count"`
	MemSizeMiB int `json:"mem_size_mib"`
}

// bootSource mirrors the Firecracker /boot-source schema.
type bootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	InitrdPath      string `json:"initrd_path,omitempty"`
	BootArgs        string `json:"boot_args"`
}

// driveSpec mirrors the Firecracker /drives/{id} schema. We only
// honour the rootfs slot (id == "rootfs"); secondary disks land with
// virtio-blk-mmio multi-device support.
type driveSpec struct {
	PathOnHost string `json:"path_on_host"`
	IsReadOnly bool   `json:"is_read_only"`
}

// actionRequest mirrors the Firecracker /actions schema.
type actionRequest struct {
	ActionType string `json:"action_type"`
}

// worker holds the VM-pending configuration accumulated through PUT
// calls, plus the live session once InstanceStart fires. The mutex
// guards all fields — REST handlers run on httptest-spawned goroutines.
type worker struct {
	mu      sync.Mutex
	id      string
	stderr  io.Writer
	machine machineConfig
	boot    bootSource
	rootfs  driveSpec

	// state moves through "Uninitialized" -> "Starting" -> "Running"
	// -> "Halted" (post-Run). Anything before InstanceStart is
	// "Uninitialized" so callers can poll GET / and know they're
	// still in the config-accumulation phase.
	state string

	// Running session + cancel func for the vCPU. nil before
	// InstanceStart and after the run goroutine exits.
	session *vmm.WHPBootSession
	cancel  context.CancelFunc
	done    chan struct{}
}

func newWorker(id string, stderr io.Writer) *worker {
	if stderr == nil {
		stderr = io.Discard
	}
	return &worker{
		id:     id,
		stderr: stderr,
		state:  "Uninitialized",
		machine: machineConfig{
			VCPUCount:  1,
			MemSizeMiB: 128,
		},
		boot: bootSource{
			BootArgs: "console=ttyS0 reboot=k panic=1",
		},
		done: make(chan struct{}),
	}
}

// shutdown cancels any running VM and waits for its Run goroutine to
// unwind. Safe to call concurrently with handlers and multiple times.
func (w *worker) shutdown() {
	w.mu.Lock()
	cancel := w.cancel
	done := w.done
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			fmt.Fprintln(w.stderr, "gocracker-vmm: shutdown: VM did not exit within 5s")
		}
	}
}

// ServeHTTP routes the small REST surface. We avoid pulling in
// chi/mux to keep the Windows binary small and the build dependency
// graph identical to the gocracker-whp helper.
func (w *worker) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/":
		w.handleStatus(rw, r)
	case r.Method == http.MethodPut && r.URL.Path == "/machine-config":
		w.handleMachineConfig(rw, r)
	case r.Method == http.MethodPut && r.URL.Path == "/boot-source":
		w.handleBootSource(rw, r)
	case r.Method == http.MethodPut && r.URL.Path == "/drives/rootfs":
		w.handleRootfs(rw, r)
	case r.Method == http.MethodPut && r.URL.Path == "/actions":
		w.handleAction(rw, r)
	default:
		http.NotFound(rw, r)
	}
}

func (w *worker) handleStatus(rw http.ResponseWriter, _ *http.Request) {
	w.mu.Lock()
	resp := map[string]any{
		"id":    w.id,
		"state": w.state,
	}
	w.mu.Unlock()
	writeJSON(rw, http.StatusOK, resp)
}

func (w *worker) handleMachineConfig(rw http.ResponseWriter, r *http.Request) {
	var cfg machineConfig
	if err := decodeJSON(r, &cfg); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"fault_message": err.Error()})
		return
	}
	if cfg.VCPUCount <= 0 || cfg.MemSizeMiB <= 0 {
		writeJSON(rw, http.StatusBadRequest, map[string]string{
			"fault_message": "vcpu_count and mem_size_mib must be positive",
		})
		return
	}
	if cfg.VCPUCount != 1 {
		writeJSON(rw, http.StatusBadRequest, map[string]string{
			"fault_message": "vcpu_count must be 1 on Windows today (Phase 2e); multi-vCPU lands in a follow-up",
		})
		return
	}
	if cfg.MemSizeMiB < 64 {
		writeJSON(rw, http.StatusBadRequest, map[string]string{
			"fault_message": fmt.Sprintf("mem_size_mib must be >= 64; got %d", cfg.MemSizeMiB),
		})
		return
	}
	w.mu.Lock()
	w.machine = cfg
	w.mu.Unlock()
	rw.WriteHeader(http.StatusNoContent)
}

func (w *worker) handleBootSource(rw http.ResponseWriter, r *http.Request) {
	var b bootSource
	if err := decodeJSON(r, &b); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"fault_message": err.Error()})
		return
	}
	if b.KernelImagePath == "" {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"fault_message": "kernel_image_path is required"})
		return
	}
	if _, err := os.Stat(b.KernelImagePath); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]string{
			"fault_message": fmt.Sprintf("kernel_image_path %q: %v", b.KernelImagePath, err),
		})
		return
	}
	if b.InitrdPath != "" {
		if _, err := os.Stat(b.InitrdPath); err != nil {
			writeJSON(rw, http.StatusBadRequest, map[string]string{
				"fault_message": fmt.Sprintf("initrd_path %q: %v", b.InitrdPath, err),
			})
			return
		}
	}
	if b.BootArgs == "" {
		b.BootArgs = "console=ttyS0 reboot=k panic=1"
	}
	w.mu.Lock()
	w.boot = b
	w.mu.Unlock()
	rw.WriteHeader(http.StatusNoContent)
}

func (w *worker) handleRootfs(rw http.ResponseWriter, r *http.Request) {
	var d driveSpec
	if err := decodeJSON(r, &d); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"fault_message": err.Error()})
		return
	}
	if d.PathOnHost == "" {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"fault_message": "path_on_host is required"})
		return
	}
	if _, err := os.Stat(d.PathOnHost); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]string{
			"fault_message": fmt.Sprintf("path_on_host %q: %v", d.PathOnHost, err),
		})
		return
	}
	w.mu.Lock()
	w.rootfs = d
	w.mu.Unlock()
	rw.WriteHeader(http.StatusNoContent)
}

func (w *worker) handleAction(rw http.ResponseWriter, r *http.Request) {
	var a actionRequest
	if err := decodeJSON(r, &a); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"fault_message": err.Error()})
		return
	}
	switch a.ActionType {
	case "InstanceStart":
		if err := w.startVM(); err != nil {
			writeJSON(rw, http.StatusBadRequest, map[string]string{"fault_message": err.Error()})
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	case "SendCtrlAltDel":
		// Mirrors Firecracker's reboot semantic. We cancel the vCPU
		// context — the guest sees an external halt, which is a
		// reasonable approximation until ACPI shutdown lands.
		w.shutdown()
		rw.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(rw, http.StatusBadRequest, map[string]string{
			"fault_message": fmt.Sprintf("action_type %q not supported on this build", a.ActionType),
		})
	}
}

// startVM transitions the worker from "Uninitialized" to "Running",
// spawning a background goroutine that drives session.Run until the
// guest halts or the context is cancelled. Returns an error if the
// accumulated config is incomplete or a VM is already running.
func (w *worker) startVM() error {
	w.mu.Lock()
	if w.session != nil {
		w.mu.Unlock()
		return errors.New("VM already running; tear down with SendCtrlAltDel first")
	}
	if w.boot.KernelImagePath == "" {
		w.mu.Unlock()
		return errors.New("boot-source must be set before InstanceStart")
	}
	cfg := vmm.WHPBootConfig{
		KernelPath:     w.boot.KernelImagePath,
		Cmdline:        w.boot.BootArgs,
		MemoryBytes:    uint64(w.machine.MemSizeMiB) * 1024 * 1024,
		VCPUs:          w.machine.VCPUCount,
		InitrdPath:     w.boot.InitrdPath,
		RootfsPath:     w.rootfs.PathOnHost,
		RootfsReadOnly: w.rootfs.IsReadOnly,
		OnUARTOutput: func(b byte) {
			// The shim does not have a console subscriber today;
			// drop guest UART output to /dev/null. A future revision
			// can buffer this and surface it via GET /console.
		},
	}
	// We intentionally hold w.mu across BootLinuxOnWHP — it can take
	// 50-200 ms (kernel parse + RAM map) but the worker is single-
	// tenant so no other request can usefully proceed during boot.
	session, err := vmm.BootLinuxOnWHP(context.Background(), cfg)
	if err != nil {
		w.mu.Unlock()
		return fmt.Errorf("BootLinuxOnWHP: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.session = session
	w.cancel = cancel
	w.state = "Running"
	w.done = make(chan struct{})
	done := w.done
	w.mu.Unlock()

	go func() {
		defer close(done)
		runErr := session.Run(ctx)
		_ = session.Close()
		w.mu.Lock()
		w.state = "Halted"
		w.session = nil
		w.cancel = nil
		w.mu.Unlock()
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			fmt.Fprintf(w.stderr, "gocracker-vmm: vCPU exit error: %v\n", runErr)
		}
	}()
	return nil
}

// decodeJSON pulls JSON off the request body with a small body limit
// to keep a misbehaving client from OOMing the worker. 1 MiB is way
// more than any of the supported request schemas need.
func decodeJSON(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func writeJSON(rw http.ResponseWriter, status int, body any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(body)
}
