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
	"github.com/gocracker/gocracker/pkg/warmcache"
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
			// Lease path SetNetwork configures eth0 post-restore —
			// the pool needs a TAP + eth0 available on every booted
			// VM. network_mode=auto gives us exactly that (host-side
			// TAP + guest-side eth0); anything else (none / manual)
			// would fail SetNetwork with "Link not found".
			NetworkMode:  "auto",
			ExecEnabled:  true, // pooled sandboxes always have toolbox running
			VsockUDSPath: poolVsockUDSPath(req.TemplateID, jailerMode),
			VMMBinary:    m.VMMBinary,
			JailerBinary: m.JailerBinary,
			// WarmCapture is the whole point of the pool: the FIRST
			// boot pays the cold-boot cost (~200-500 ms) AND captures
			// a snapshot. Every subsequent boot via container.Run
			// hits warmcache.Lookup → restore in ~3 ms instead of
			// re-cold-booting. Without this, refill mid-burst pays
			// the full cold tax and p95 spikes 100-500 ms — exactly
			// the regression PLAN §5 was avoiding.
			WarmCapture:     true,
			InteractiveExec: true, // CMD-agnostic snapshot — any LeaseSpec.Cmd works
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

	// Pre-warm the warmcache snapshot. Without this, the FIRST N
	// pool boots all race as cold-boots (each not yet seeing the
	// other's not-yet-written snapshot), and we lose the entire
	// point of WarmCapture for pool refill. One synchronous warm-up
	// boot here populates the cache; once container.Run returns
	// and WarmDone fires, every subsequent container.Run for this
	// template hits the cache and restores in ~3-5 ms instead of
	// re-cold-booting in ~200-500 ms.
	//
	// Cost: one cold-boot + snapshot capture (~3 s) at register
	// time. Benefit: N×~200 ms saved on every pool fill thereafter.
	// Net win at any pool size > 0.
	if err := prewarmSnapshot(ctx, cfg.RunOptions); err != nil {
		p.Stop()
		return PoolRegistration{}, fmt.Errorf("sandboxd: prewarm snapshot: %w", err)
	}

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

// Shutdown stops every registered pool and drains their paused VMs.
// Wired into sandboxd's main() signal-handling so Ctrl-C / SIGTERM
// doesn't orphan running VMs (the refiller goroutines die with the
// process but their booted KVM children would survive otherwise).
//
// Safe to call multiple times; second call is a no-op because pools
// are removed from the registry on first iteration.
func (m *Manager) Shutdown(ctx context.Context) {
	if m.poolMgr == nil {
		return
	}
	pm := m.poolMgr
	pm.mu.Lock()
	regs := make([]*PoolRegistration, 0, len(pm.pools))
	for _, r := range pm.pools {
		regs = append(regs, r)
	}
	pm.pools = map[string]*PoolRegistration{}
	pm.mu.Unlock()
	for _, r := range regs {
		r.pool.Stop()
		r.pool.DrainPaused()
	}
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

// prewarmSnapshot does one synchronous cold-boot with WarmCapture
// so the warmcache snapshot exists on disk before the pool refiller
// starts. After this returns, every subsequent container.Run with
// the same RunOptions hits warmcache.Lookup → restore (~3-5 ms)
// instead of re-cold-booting.
//
// The booted VM is closed immediately — we only care about the
// side-effect of populating the cache. Failure is non-fatal at the
// VM-handle level (Close errors are best-effort) but IS fatal at the
// snapshot level (caller propagates).
func prewarmSnapshot(ctx context.Context, opts container.RunOptions) error {
	// If a snapshot already exists in the cache for this opts, we
	// can skip the cold-boot entirely — container.Run on the next
	// pool boot will hit it.
	key, ok := container.ComputeWarmCacheKey(opts)
	if ok {
		if _, hit := warmcacheLookup(key); hit {
			return nil
		}
	}
	result, err := container.Run(opts)
	if err != nil {
		return err
	}
	defer result.Close()
	defer result.VM.Stop()
	if result.WarmDone != nil {
		select {
		case <-result.WarmDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// warmcacheLookup is a thin wrapper so prewarmSnapshot can ask
// "does this template's snapshot already exist on disk?" without
// a full container.Run round-trip.
func warmcacheLookup(key string) (string, bool) {
	return warmcache.Lookup(warmcache.DefaultRoot(), key)
}

// poolVsockUDSPath picks the per-template UDS template path. For
// jailer-on we use the /worker/-prefixed path: each VM has its own
// runDir bind-mounted at /worker, so the same configured path
// resolves to a different host-side socket per VM. The pool's
// containerBooter calls ResolveWorkerHostSidePath after each boot to
// rewrite this internal path into the host-dialable one (lives on
// BootResult.UDSPath, then surfaces on Lease.UDSPath).
//
// For jailer-off we return "" and the booter generates a unique
// /tmp/gc-pool-<pid>-<ns>.sock per Boot — direct host paths can't
// be shared across pooled VMs without colliding at bind().
func poolVsockUDSPath(templateID, jailerMode string) string {
	if jailerMode == container.JailerModeOn {
		return "/worker/" + templateID + ".sock"
	}
	return ""
}

