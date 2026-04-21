// Package sandboxd is the gocracker sandbox control plane — a thin
// HTTP service on top of pkg/container + pkg/vmm + the toolbox
// agent client. It owns the sandbox lifecycle (create / get / list
// / delete) and is the place where higher-level concerns (warm
// pool, IP allocation, templates, preview URLs) plug in over time.
//
// Fase 4 slice 1 (this slice) ships the minimum: cold-boot via
// container.Run, persisted store, REST routes for CRUD. Exec, files,
// events SSE, warm pool, IP pool, templates land in subsequent
// slices on the same branch.
package sandboxd

import (
	"sync"
	"time"

	"github.com/gocracker/gocracker/pkg/container"
)

// State is the lifecycle stage of a sandbox.
type State string

const (
	StateCreating State = "creating"
	StateReady    State = "ready"
	StateStopping State = "stopping"
	StateStopped  State = "stopped"
	StateError    State = "error"
)

// Sandbox is the in-memory representation of a single sandbox VM.
// Not safe for concurrent mutation by multiple goroutines without
// holding the parent Store's lock — Store.Update is the only
// supported way to change fields.
type Sandbox struct {
	ID        string    `json:"id"`
	State     State     `json:"state"`
	Image     string    `json:"image"`
	UDSPath   string    `json:"uds_path,omitempty"` // host path to the toolbox UDS
	GuestIP   string    `json:"guest_ip,omitempty"` // primary IP from container.Run, if set
	RuntimeID string    `json:"runtime_id,omitempty"` // gocracker VM ID for cross-referencing logs
	CreatedAt time.Time `json:"created_at"`
	Error     string    `json:"error,omitempty"`

	// runResult is intentionally not serialized — it carries an
	// open VM handle that only makes sense in-process. Persistence
	// of stopped sandboxes drops this field on load.
	runResult *container.RunResult `json:"-"`

	// mu guards mutable fields above when callers hold a Sandbox
	// pointer outside the Store's lock (e.g. during async ops like
	// stop). Most mutation should still flow through Store.
	mu sync.Mutex `json:"-"`
}

// CreateSandboxRequest is the POST /sandboxes payload. Mirrors the
// subset of container.RunOptions that makes sense for sandbox
// orchestration — internal flags like InteractiveExec / WarmCapture
// are derived inside sandboxd, not exposed here.
type CreateSandboxRequest struct {
	Image       string   `json:"image"`           // OCI ref, required
	KernelPath  string   `json:"kernel_path"`     // absolute path, required
	MemMB       uint64   `json:"mem_mb,omitempty"` // default 256
	CPUs        int      `json:"cpus,omitempty"`   // default 1
	Cmd         []string `json:"cmd,omitempty"`    // optional override
	Env         []string `json:"env,omitempty"`    // KEY=VALUE
	WorkDir     string   `json:"workdir,omitempty"`
	NetworkMode string   `json:"network_mode,omitempty"` // "" | "auto"
	JailerMode  string   `json:"jailer_mode,omitempty"`  // "on" | "off" — default off for now
}

// CreateSandboxResponse is what POST /sandboxes returns — same shape
// as Sandbox but explicit so future fields don't accidentally leak
// internal model state.
type CreateSandboxResponse struct {
	Sandbox *Sandbox `json:"sandbox"`
}

// ListSandboxesResponse wraps a slice for forward-compat with paging.
type ListSandboxesResponse struct {
	Sandboxes []*Sandbox `json:"sandboxes"`
}
