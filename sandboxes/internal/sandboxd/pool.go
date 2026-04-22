// Pool integration for sandboxd (Fase 5 slice 7). Wires a per-template
// warm pool to the HTTP control plane so callers can:
//
//   POST /pools                  — register a template + start its pool
//   GET  /pools                  — list registered pools + counts
//   POST /sandboxes/lease        — acquire a warm sandbox (blocks)
//
// Lease vs cold-boot: /sandboxes (existing) always cold-boots a fresh
// VM via container.Run, taking 200-500 ms. /sandboxes/lease pulls a
// pre-paused VM from the named pool, applies an IP, and returns in
// the 15-20 ms range (3 ms restore + ~15 ms SetNetwork). Both routes
// produce the same Sandbox shape so client code only needs the
// fast-path opt-in.
package sandboxd

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gocracker/gocracker/pkg/container"
	"github.com/gocracker/gocracker/pkg/vmm"
	"github.com/gocracker/gocracker/sandboxes/internal/pool"
)

// PoolRegistration is the persisted-or-not state of a single pool.
// Manager.pools holds these; Server exposes them via /pools routes.
// One *pool.Pool per registration — pool config is immutable after
// register.
type PoolRegistration struct {
	TemplateID string             `json:"template_id"`
	Image      string             `json:"image"`
	KernelPath string             `json:"kernel_path"`
	MemMB      uint64             `json:"mem_mb,omitempty"`
	CPUs       int                `json:"cpus,omitempty"`
	JailerMode string             `json:"jailer_mode,omitempty"`
	MinPaused  int                `json:"min_paused,omitempty"`
	MaxPaused  int                `json:"max_paused,omitempty"`
	Counts     map[pool.State]int `json:"counts,omitempty"`

	pool *pool.Pool `json:"-"`
}

// CreatePoolRequest is the POST /pools payload. A subset of
// container.RunOptions (the user-relevant warm-cache surface) plus
// pool-policy knobs. Anything not set falls through to the pool
// package's Fase 5 defaults.
type CreatePoolRequest struct {
	TemplateID string `json:"template_id"`           // required, unique per Manager
	Image      string `json:"image"`                 // OCI ref (or Dockerfile)
	Dockerfile string `json:"dockerfile,omitempty"`  // alternative to Image
	Context    string `json:"context,omitempty"`     // build context for Dockerfile
	KernelPath string `json:"kernel_path"`           // required
	MemMB      uint64 `json:"mem_mb,omitempty"`      // default 256
	CPUs       int    `json:"cpus,omitempty"`        // default 1
	JailerMode string `json:"jailer_mode,omitempty"` // "on"|"off", default "off"

	MinPaused            int `json:"min_paused,omitempty"`
	MaxPaused            int `json:"max_paused,omitempty"`
	ReplenishParallelism int `json:"replenish_parallelism,omitempty"`
}

// LeaseSandboxRequest is the POST /sandboxes/lease payload. Blocks
// (server-side) until a paused sandbox is available or Timeout
// elapses. The server also caps Timeout at a hard ceiling (60 s) to
// prevent a misbehaving client from holding an HTTP connection
// forever.
type LeaseSandboxRequest struct {
	TemplateID string        `json:"template_id"`
	Timeout    time.Duration `json:"timeout,omitempty"` // default 5s
}

// ErrPoolAlreadyRegistered is returned by RegisterPool when the
// caller passed a TemplateID that's already in the registry. Pools
// are register-once: change the template by Unregister then
// Register fresh.
var ErrPoolAlreadyRegistered = errors.New("sandboxd: pool already registered")

// ErrPoolNotFound is returned by Lease when the requested TemplateID
// has no registered pool.
var ErrPoolNotFound = errors.New("sandboxd: pool not found")

// poolManager holds the per-Manager pool registry, IP allocator,
// and the shared Networker. Embedded into Manager via composition;
// kept in its own struct so the file boundary isolates Fase 5
// surface from the cold-boot Lifecycle methods.
type poolManager struct {
	mu      sync.Mutex
	pools   map[string]*PoolRegistration
	ipAlloc *pool.IPAllocator
	netter  pool.Networker
}

// initPoolManagerLocked is called once by Manager.ensurePoolManager
// to lazily allocate the registry on first use. Idempotent.
func (m *Manager) ensurePoolManager() *poolManager {
	m.poolInit.Do(func() {
		ipa, err := pool.NewIPAllocator("198.19.0.0/16", 30)
		if err != nil {
			// 198.19.0.0/16 is a literal constant; an error here is
			// a programming bug, not runtime. Panic so it shows up
			// in tests immediately.
			panic(fmt.Sprintf("sandboxd: default IP allocator init: %v", err))
		}
		m.poolMgr = &poolManager{
			pools:   map[string]*PoolRegistration{},
			ipAlloc: ipa,
			netter:  pool.NewToolboxNetworker(),
		}
	})
	return m.poolMgr
}

// RegisterPool starts a warm pool for the given template. Returns
// the registration on success; ErrPoolAlreadyRegistered if the ID
// is taken. Pool refiller goroutines start immediately and run
// until UnregisterPool or process exit.
func (m *Manager) RegisterPool(ctx context.Context, req CreatePoolRequest) (PoolRegistration, error) {
	if req.TemplateID == "" {
		return PoolRegistration{}, fmt.Errorf("%w: template_id required", ErrInvalidRequest)
	}
	if req.KernelPath == "" {
		return PoolRegistration{}, fmt.Errorf("%w: kernel_path required", ErrInvalidRequest)
	}
	if req.Image == "" && req.Dockerfile == "" {
		return PoolRegistration{}, fmt.Errorf("%w: image or dockerfile required", ErrInvalidRequest)
	}
	jailerMode := req.JailerMode
	switch jailerMode {
	case "":
		jailerMode = container.JailerModeOff
	case container.JailerModeOn, container.JailerModeOff:
	default:
		return PoolRegistration{}, fmt.Errorf("%w: jailer_mode %q (expected on|off)", ErrInvalidRequest, jailerMode)
	}

	pm := m.ensurePoolManager()
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if _, exists := pm.pools[req.TemplateID]; exists {
		return PoolRegistration{}, ErrPoolAlreadyRegistered
	}

	cfg := pool.Config{
		TemplateID: req.TemplateID,
		RunOptions: container.RunOptions{
			Image:        req.Image,
			Dockerfile:   req.Dockerfile,
			Context:      req.Context,
			KernelPath:   req.KernelPath,
			MemMB:        defaultUint64(req.MemMB, 256),
			CPUs:         defaultInt(req.CPUs, 1),
			JailerMode:   jailerMode,
			ExecEnabled:  true, // pooled sandboxes always have toolbox running
			VsockUDSPath: poolVsockUDSPath(req.TemplateID, jailerMode),
			VMMBinary:    m.VMMBinary,
			JailerBinary: m.JailerBinary,
		},
		MinPaused:            req.MinPaused,
		MaxPaused:            req.MaxPaused,
		ReplenishParallelism: req.ReplenishParallelism,
	}
	p, err := pool.NewPool(cfg)
	if err != nil {
		return PoolRegistration{}, fmt.Errorf("sandboxd: new pool: %w", err)
	}
	p.SetNetworker(pm.netter)
	if err := p.Start(ctx); err != nil {
		return PoolRegistration{}, fmt.Errorf("sandboxd: start pool: %w", err)
	}

	reg := &PoolRegistration{
		TemplateID: req.TemplateID,
		Image:      req.Image,
		KernelPath: req.KernelPath,
		MemMB:      cfg.RunOptions.MemMB,
		CPUs:       cfg.RunOptions.CPUs,
		JailerMode: jailerMode,
		MinPaused:  cfg.MinPaused,
		MaxPaused:  cfg.MaxPaused,
		pool:       p,
	}
	pm.pools[req.TemplateID] = reg
	return *reg, nil
}

// UnregisterPool stops the named pool and tears down all its paused
// VMs. Leased VMs (already handed to callers) are NOT touched —
// callers must Release them via DELETE /sandboxes/{id} as usual.
// Returns ErrPoolNotFound if the template isn't registered.
func (m *Manager) UnregisterPool(templateID string) error {
	pm := m.ensurePoolManager()
	pm.mu.Lock()
	reg, ok := pm.pools[templateID]
	if !ok {
		pm.mu.Unlock()
		return ErrPoolNotFound
	}
	delete(pm.pools, templateID)
	pm.mu.Unlock()
	reg.pool.Stop()
	reg.pool.DrainPaused()
	return nil
}

// ListPools returns a snapshot of every registered pool with live
// counts. Safe to call without holding the Manager lock; pool count
// reads take p.mu internally.
func (m *Manager) ListPools() []PoolRegistration {
	pm := m.ensurePoolManager()
	pm.mu.Lock()
	defer pm.mu.Unlock()
	out := make([]PoolRegistration, 0, len(pm.pools))
	for _, r := range pm.pools {
		snap := *r
		snap.Counts = r.pool.CountByState()
		snap.pool = nil // don't leak the pointer through JSON
		out = append(out, snap)
	}
	return out
}

// LeaseSandbox pulls a paused sandbox from the named pool, allocates
// an IP, and applies the network config via toolbox SetNetwork. The
// returned Sandbox carries the host-visible UDSPath and the assigned
// GuestIP so the caller can immediately exec into it.
//
// On any failure between Acquire and a healthy lease (Resume error,
// SetNetwork error, IP exhaustion) the IP is freed and the partial
// VM is torn down — the caller never gets a half-configured handle.
func (m *Manager) LeaseSandbox(ctx context.Context, req LeaseSandboxRequest) (Sandbox, error) {
	if req.TemplateID == "" {
		return Sandbox{}, fmt.Errorf("%w: template_id required", ErrInvalidRequest)
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if timeout > 60*time.Second {
		timeout = 60 * time.Second
	}

	pm := m.ensurePoolManager()
	pm.mu.Lock()
	reg, ok := pm.pools[req.TemplateID]
	pm.mu.Unlock()
	if !ok {
		return Sandbox{}, ErrPoolNotFound
	}

	addr, err := pm.ipAlloc.Allocate()
	if err != nil {
		return Sandbox{}, fmt.Errorf("sandboxd: ip allocate: %w", err)
	}
	spec := pool.LeaseSpec{
		IP:        addr.IP,
		Gateway:   addr.Gateway,
		MAC:       addr.MAC,
		Interface: "eth0",
	}
	lease, err := reg.pool.AcquireWait(ctx, spec, timeout)
	if err != nil {
		pm.ipAlloc.Free(addr.Slot)
		return Sandbox{}, fmt.Errorf("sandboxd: lease: %w", err)
	}

	sb := &Sandbox{
		ID:        lease.ID,
		State:     StateReady,
		Image:     reg.Image,
		UDSPath:   lease.UDSPath,
		GuestIP:   addr.IP,
		RuntimeID: lease.ID,
		CreatedAt: lease.LeasedAt,
	}
	if err := m.Store.Add(sb); err != nil {
		// ID collision (shouldn't happen — pool IDs are gc-N hex).
		// Free the IP, return; the leased VM stays in the pool's
		// leased state and will be reaped on UnregisterPool.
		pm.ipAlloc.Free(addr.Slot)
		return Sandbox{}, fmt.Errorf("sandboxd: store add: %w", err)
	}
	// Stash the lease metadata so DELETE can route to Pool.Release.
	m.Store.Update(lease.ID, func(s *Sandbox) {
		s.poolTemplateID = req.TemplateID
		s.poolIPSlot = addr.Slot
	})
	updated, _ := m.Store.Get(lease.ID)
	return updated, nil
}

// ReleaseLeased tears down a leased sandbox: calls Pool.Release on
// the pool, frees the IP, removes from the store. Used by the
// DELETE /sandboxes/{id} handler when the sandbox originated from
// a lease (not a cold boot). Returns nil on success;
// ErrSandboxNotFound when the id is unknown.
func (m *Manager) ReleaseLeased(id string) error {
	sb, ok := m.Store.Get(id)
	if !ok {
		return ErrSandboxNotFound
	}
	if sb.poolTemplateID == "" {
		// Not a leased sandbox; caller should use Delete instead.
		return fmt.Errorf("sandboxd: %s is not a leased sandbox", id)
	}
	pm := m.ensurePoolManager()
	pm.mu.Lock()
	reg, ok := pm.pools[sb.poolTemplateID]
	pm.mu.Unlock()
	if !ok {
		// Pool unregistered after lease; just clean up the store.
		_, _ = m.Store.Remove(id)
		return nil
	}
	rr, relErr := reg.pool.Release(id)
	if rr != nil {
		rr.Close()
	}
	pm.ipAlloc.Free(sb.poolIPSlot)
	_, _ = m.Store.Remove(id)
	return relErr
}

// poolVsockUDSPath picks the per-template UDS template path. For
// jailer-on we use the /worker/-prefixed path that ResolveWorkerHostSidePath
// rewrites to the runDir on the host (avoids the 108-byte
// sockaddr_un limit, see commit a45700a). For jailer-off we let the
// runtime pick its own default.
//
// NB: the pool boots N VMs from the same RunOptions, so the UDSPath
// here MUST be the same for every boot — each booted VM gets its own
// runDir, and ResolveWorkerHostSidePath translates per-VM. Slice 8
// will revisit if pool-bench surfaces collisions; the expected
// behavior is that internal/worker/vmm.go gives every VM a unique
// runDir and the bind-mount keeps the per-VM .sock files distinct.
func poolVsockUDSPath(templateID, jailerMode string) string {
	if jailerMode == container.JailerModeOn {
		return "/worker/" + templateID + ".sock"
	}
	return ""
}

// resolveLeaseUDSPath translates the pool's internal UDSPath into the
// host-visible one for the leased sandbox. Wrapped here so the lease
// path doesn't have to know the vmm.WorkerBacked dance directly.
//
// Currently unused — the pool's BootResult.UDSPath is already the
// internal path, and slice 7 sets the lease's UDSPath to it as-is
// for jailer-off. Slice 8's pool-bench will exercise jailer-on and
// surface whether we need full translation here.
func resolveLeaseUDSPath(internal string, _ vmm.WorkerMetadata) string {
	return internal
}
