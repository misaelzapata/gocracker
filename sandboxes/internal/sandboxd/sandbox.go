package sandboxd

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gocracker/gocracker/pkg/container"
	"github.com/gocracker/gocracker/pkg/vmm"
)

// Manager owns the sandbox lifecycle. It writes to the Store, calls
// pkg/container for cold boot, and tears VMs down on Delete. Future
// slices add the warm pool, IP allocator, and toolbox client wiring
// — those will hook in via additional Manager methods, not by
// rewriting Create.
//
// Per Fase 4 slice 1 scope: only cold boot. Each Create call goes
// straight to container.Run with the user's options. The UDS path
// is auto-generated under StateDir so each sandbox has its own
// dialable surface for the next slices (process/execute, files).
type Manager struct {
	Store    *Store
	StateDir string // base for per-sandbox runtime state (UDS sockets, etc.)

	// VMMBinary / JailerBinary let sandboxd spawn workers via the
	// main gocracker binary instead of re-exec-ing itself (it has no
	// worker/jailer subcommands). Empty = fall back to the internal/
	// worker resolver (os.Executable()), which only works when
	// running as part of the gocracker binary proper.
	VMMBinary    string
	JailerBinary string
}

// Create cold-boots a fresh sandbox VM. Blocks until container.Run
// returns (typically <100 ms for warm-cached images, ~1-3s for cold
// pulls). On success the sandbox is in StateReady; on failure it's
// StateError with the cause and the partial RunResult cleaned up.
func (m *Manager) Create(req CreateSandboxRequest) (Sandbox, error) {
	normalized, err := validateCreateRequest(req)
	if err != nil {
		return Sandbox{}, err
	}
	id := newSandboxID()
	// For jailer-on we can't just put the UDS under StateDir: the jail
	// prefix (~65 chars) plus <StateDir>/sandboxes/<id>.sock blows past
	// the sockaddr_un 108-byte limit. /worker is already bind-mounted
	// rw from the worker's runDir into the chroot, so putting the UDS
	// at /worker/<id>.sock keeps both the internal path short (~24
	// chars) AND the host-side path short (runDir is ~40 chars, total
	// ~60 chars). ResolveWorkerHostSidePath handles the translation.
	// Jailer-off keeps StateDir since there's no chroot layering.
	var udsPath string
	if normalized.JailerMode == container.JailerModeOn {
		udsPath = "/worker/sb-" + id[3:] + ".sock" // id already has "sb-" prefix; keep total short
	} else {
		udsPath = filepath.Join(m.StateDir, "sandboxes", id+".sock")
	}

	sb := &Sandbox{
		ID:        id,
		State:     StateCreating,
		Image:     normalized.Image,
		UDSPath:   udsPath,
		CreatedAt: time.Now().UTC(),
	}
	if err := m.Store.Add(sb); err != nil {
		return Sandbox{}, err
	}

	opts := container.RunOptions{
		Image:        normalized.Image,
		KernelPath:   normalized.KernelPath,
		MemMB:        defaultUint64(normalized.MemMB, 256),
		CPUs:         defaultInt(normalized.CPUs, 1),
		Cmd:          normalized.Cmd,
		Env:          normalized.Env,
		WorkDir:      normalized.WorkDir,
		NetworkMode:  normalized.NetworkMode,
		JailerMode:   normalized.JailerMode,
		ExecEnabled:  true, // sandboxes always run with the exec agent (and now toolbox) idle
		VsockUDSPath: udsPath,
		VMMBinary:    m.VMMBinary,
		JailerBinary: m.JailerBinary,
	}

	result, runErr := container.Run(opts)
	if runErr != nil {
		m.Store.Update(id, func(s *Sandbox) {
			s.State = StateError
			s.Error = runErr.Error()
		})
		snap, _ := m.Store.Get(id)
		return snap, fmt.Errorf("sandboxd create: container run: %w", runErr)
	}

	// Translate the guest-internal UDS path into the host-visible one.
	// Jailer-on: the VMM binds under /worker which is bind-mounted from
	// runDir, so ResolveWorkerHostSidePath rewrites /worker/... to the
	// host RunDir (stays short — critical for the 108-byte sockaddr_un
	// limit). Jailer-off: no chroot, no translation.
	hostUDSPath := udsPath
	if wh, ok := result.VM.(vmm.WorkerBacked); ok {
		hostUDSPath = vmm.ResolveWorkerHostSidePath(wh.WorkerMetadata(), udsPath)
	}

	m.Store.Update(id, func(s *Sandbox) {
		s.State = StateReady
		s.runResult = result
		s.RuntimeID = result.ID
		s.GuestIP = result.GuestIP
		s.UDSPath = hostUDSPath
	})

	updated, _ := m.Store.Get(id)
	return updated, nil
}

// Delete stops the sandbox VM, removes it from the store, and
// returns nil on success. Idempotent on the store side — calling
// Delete twice returns the not-found error the second time.
//
// We read sb.runResult INSIDE the Store.Update closure (which holds
// both the store lock and sb.mu) so a concurrent Create-in-flight
// can't race us on the pointer assignment. The VM teardown itself
// runs outside the lock — VM.Stop + RunResult.Close can block for
// seconds and we don't want to stall other Store callers on it.
func (m *Manager) Delete(id string) error {
	var rr *container.RunResult
	found := m.Store.Update(id, func(s *Sandbox) {
		rr = s.runResult
		s.runResult = nil
		s.State = StateStopping
	})
	if !found {
		return ErrSandboxNotFound
	}
	if rr != nil {
		if rr.VM != nil {
			rr.VM.Stop()
		}
		rr.Close()
	}
	m.Store.Remove(id)
	return nil
}

// validateCreateRequest canonicalizes the user input, runs the
// cheap/synchronous validations (path exists + mode/jailer enums)
// and returns a copy that's safe to pass into container.Run. Every
// validation failure wraps ErrInvalidRequest so the HTTP layer maps
// it to 400 — avoids the previous situation where a bad kernel_path
// or garbage jailer_mode silently turned into a runtime 500.
func validateCreateRequest(req CreateSandboxRequest) (CreateSandboxRequest, error) {
	if req.Image == "" {
		return req, fmt.Errorf("%w: image is required", ErrInvalidRequest)
	}
	if req.KernelPath == "" {
		return req, fmt.Errorf("%w: kernel_path is required", ErrInvalidRequest)
	}
	info, err := os.Stat(req.KernelPath)
	if err != nil {
		return req, fmt.Errorf("%w: kernel_path %q: %v", ErrInvalidRequest, req.KernelPath, err)
	}
	if info.IsDir() {
		return req, fmt.Errorf("%w: kernel_path %q is a directory", ErrInvalidRequest, req.KernelPath)
	}
	// network_mode: accept "" (default "auto"), "auto", or "none" / "off" (disable).
	switch req.NetworkMode {
	case "":
		req.NetworkMode = "auto"
	case "auto":
		// keep
	case "none", "off":
		req.NetworkMode = ""
	default:
		return req, fmt.Errorf("%w: network_mode %q (expected \"\", \"auto\", or \"none\")", ErrInvalidRequest, req.NetworkMode)
	}
	// jailer_mode: "" defaults to off; only on/off accepted — container.jailerEnabled
	// treats unknown values as jailer-ON, which would silently flip privilege mode.
	switch req.JailerMode {
	case "":
		req.JailerMode = container.JailerModeOff
	case container.JailerModeOn, container.JailerModeOff:
		// keep
	default:
		return req, fmt.Errorf("%w: jailer_mode %q (expected \"on\" or \"off\")", ErrInvalidRequest, req.JailerMode)
	}
	return req, nil
}

// ErrSandboxNotFound is returned by Manager.Delete and Manager.Get
// when the supplied id has no live sandbox. Server-side this maps
// to HTTP 404.
var ErrSandboxNotFound = fmt.Errorf("sandbox not found")

// ErrInvalidRequest flags Create-time validation failures that
// clients should fix rather than retry. The server maps this to
// HTTP 400 via errors.Is so callers see the right status.
var ErrInvalidRequest = fmt.Errorf("invalid request")

// newSandboxID returns a 12-hex-char unique id (sb-XXXXXX format)
// — long enough to avoid collisions in any realistic per-host
// load, short enough to type. Format kept identical across slices
// so logs and external tooling don't churn.
//
// If crypto/rand.Read fails (shouldn't on Linux with getrandom, but
// possible under exotic conditions like SELinux denying the syscall)
// we salt the zero bytes with time.Now().UnixNano() so IDs stay
// unique per-call instead of collapsing to "sb-000000000000" on
// every create. Callers that need cryptographic randomness should
// source it elsewhere — this is a collision-avoidance token, not a
// secret.
func newSandboxID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		nanos := uint64(time.Now().UnixNano())
		for i := 0; i < 6; i++ {
			b[i] = byte(nanos >> (uint(i) * 8))
		}
	}
	return "sb-" + hex.EncodeToString(b[:])
}

func defaultUint64(v, dflt uint64) uint64 {
	if v == 0 {
		return dflt
	}
	return v
}

func defaultInt(v, dflt int) int {
	if v == 0 {
		return dflt
	}
	return v
}

func defaultString(v, dflt string) string {
	if v == "" {
		return dflt
	}
	return v
}
