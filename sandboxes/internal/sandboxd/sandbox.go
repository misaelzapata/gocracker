package sandboxd

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"time"

	"github.com/gocracker/gocracker/pkg/container"
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
}

// Create cold-boots a fresh sandbox VM. Blocks until container.Run
// returns (typically <100 ms for warm-cached images, ~1-3s for cold
// pulls). On success the sandbox is in StateReady; on failure it's
// StateError with the cause and the partial RunResult cleaned up.
func (m *Manager) Create(req CreateSandboxRequest) (*Sandbox, error) {
	if req.Image == "" {
		return nil, fmt.Errorf("%w: image is required", ErrInvalidRequest)
	}
	if req.KernelPath == "" {
		return nil, fmt.Errorf("%w: kernel_path is required", ErrInvalidRequest)
	}
	id := newSandboxID()
	udsPath := filepath.Join(m.StateDir, "sandboxes", id+".sock")

	sb := &Sandbox{
		ID:        id,
		State:     StateCreating,
		Image:     req.Image,
		UDSPath:   udsPath,
		CreatedAt: time.Now().UTC(),
	}
	if err := m.Store.Add(sb); err != nil {
		return nil, err
	}

	opts := container.RunOptions{
		Image:        req.Image,
		KernelPath:   req.KernelPath,
		MemMB:        defaultUint64(req.MemMB, 256),
		CPUs:         defaultInt(req.CPUs, 1),
		Cmd:          req.Cmd,
		Env:          req.Env,
		WorkDir:      req.WorkDir,
		NetworkMode:  defaultString(req.NetworkMode, "auto"),
		JailerMode:   defaultString(req.JailerMode, container.JailerModeOff),
		ExecEnabled:  true, // sandboxes always run with the exec agent (and now toolbox) idle
		VsockUDSPath: udsPath,
	}

	result, err := container.Run(opts)
	if err != nil {
		m.Store.Update(id, func(s *Sandbox) {
			s.State = StateError
			s.Error = err.Error()
		})
		return sb, fmt.Errorf("sandboxd create: container run: %w", err)
	}

	m.Store.Update(id, func(s *Sandbox) {
		s.State = StateReady
		s.runResult = result
		s.RuntimeID = result.ID
		s.GuestIP = result.GuestIP
	})

	// Re-read to return the updated copy.
	updated, _ := m.Store.Get(id)
	return updated, nil
}

// Delete stops the sandbox VM, removes it from the store, and
// returns nil on success. If the sandbox doesn't exist, returns a
// NotFound-style error the server maps to 404. Idempotent on the
// store side — calling Delete twice for the same id returns the
// not-found error on the second call.
func (m *Manager) Delete(id string) error {
	sb, ok := m.Store.Get(id)
	if !ok {
		return ErrSandboxNotFound
	}
	m.Store.Update(id, func(s *Sandbox) {
		s.State = StateStopping
	})
	if sb.runResult != nil {
		// Stop the VM and drain any background work (warm-cache goroutine, etc.)
		if sb.runResult.VM != nil {
			sb.runResult.VM.Stop()
		}
		sb.runResult.Close()
	}
	m.Store.Remove(id)
	return nil
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
func newSandboxID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
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
