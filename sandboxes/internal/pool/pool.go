//go:build linux

// Package pool manages a warm pool of pre-booted sandbox VMs so that
// Lease-based creates avoid the 200-500 ms cold-boot tax.
//
// Semantics (mirrored from feat/sandboxes-v2 pool manager f13c464, but
// rebuilt on the current pkg/container.Run + pkg/vmm stack — v2's
// runtimeclient / store / model layers are not dragged in):
//
//   - Per-template pool. Each template (image + kernel + cmd) has its
//     own refiller goroutine, its own MinPaused/MaxPaused invariant,
//     and its own failure backoff state.
//   - Sandboxes sit in StatePaused: cold-booted, then vm.Pause() so
//     they hold no vCPU time. Lease resumes + SetNetwork → StateLeased.
//   - Release tears the VM down (no re-pausing — warm-cache paths
//     restore from a clean snapshot for the NEXT lease, not this one).
//   - GlobalInflightBudget caps concurrent cold-boots across all
//     templates to avoid thundering-herd on burst-start of N pools.
//
// This file (slice 1) is the API surface + state machine only. The
// refiller, reapDead, event-driven refill, and IP allocator land in
// follow-up slices, each with its own tests and a hard gate before
// the next one starts (see feedback_foundation_before_features).
package pool

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	gclog "github.com/gocracker/gocracker/internal/log"
	"github.com/gocracker/gocracker/pkg/container"
)

// State is the lifecycle of a pooled sandbox. A VM can only transition
// through these states in the documented direction — no "unpause" flow:
// a leased VM is never returned to the pool because its dirty state
// (files, network config, processes) would leak into the next lessee.
//
//	creating → paused → leased → stopped
//	creating → error
//	paused → stopped  (reapDead / shutdown)
type State string

const (
	// StateCreating: cold-boot in flight. Not yet usable; counted
	// against GlobalInflightBudget.
	StateCreating State = "creating"
	// StateHot: booted and KEPT RUNNING (no Pause). Acquire hands
	// these back without a Resume step. Reserved for templates where
	// the SetNetwork round-trip isn't needed OR where the caller
	// explicitly opted into the lower-latency tier (plan §5 step 3:
	// "MaxHot=2 solo para templates con startup pesado que no
	// quieran pagar el SetNetwork"). Slice picks Hot before Paused.
	StateHot State = "hot"
	// StatePaused: booted + vm.Pause()'d. Eligible for Acquire.
	StatePaused State = "paused"
	// StateLeased: Acquire returned this to a caller. Not eligible
	// for refill; caller must Release.
	StateLeased State = "leased"
	// StateStopped: VM has been torn down. Terminal; entry stays in
	// the pool map only until the reaper sweeps it.
	StateStopped State = "stopped"
	// StateError: cold-boot failed. Terminal. Counted against the
	// template's ConsecutiveFailure counter; refiller backs off.
	StateError State = "error"
)

// Config is the tunable policy knobs for a single template's pool.
// All zero-value-friendly: a Config{} produces a 0-element pool that
// refuses Acquire. Callers set the fields they care about.
//
// Defaults are applied by
// NewPool — explicit zero values are preserved, only unset fields
// get defaulted.
type Config struct {
	// TemplateID uniquely names this pool. Distinct templates that
	// share an image but differ in kernel/cmd/mem MUST use different
	// IDs or they'll cross-contaminate the refill counter.
	TemplateID string

	// RunOptions is the container.RunOptions fed to every cold-boot
	// in this pool. TemplateID is baked into cacheability via the
	// underlying artifact key (image+kernel+cmd+mem+vcpus+arch).
	RunOptions container.RunOptions

	// MinPaused is the low-water mark. The refiller creates new
	// sandboxes whenever len(paused) drops below this number.
	// Default: 4.
	MinPaused int
	// MaxPaused is the high-water mark. The refiller stops creating
	// above this number. Default: 8.
	MaxPaused int

	// ReplenishParallelism caps in-flight creates FOR THIS TEMPLATE.
	// The global cap across all templates is enforced separately via
	// the Manager's GlobalInflightBudget. Default: 2.
	ReplenishParallelism int

	// ConsecutiveFailureThreshold is how many back-to-back cold-boot
	// errors trip the backoff. Default: 3.
	ConsecutiveFailureThreshold int
	// Cooldown is how long the refiller waits after tripping the
	// failure threshold before attempting another create. Default:
	// 60 s.
	Cooldown time.Duration

	// ReconcileInterval is how often the refiller loop polls the
	// MinPaused invariant in the absence of TriggerReconcile events.
	// Default: 50 ms. The original 500 ms default was a hangover
	// from before the event-driven AcquireWait wakeup landed; under
	// burst load (50 concurrent leases against an empty pool) that
	// half-second tick added up to a full poll period of pure wait.
	// 50 ms is fast enough that even adversarial loads converge
	// without manual TriggerReconcile prods, and the refiller is
	// idempotent so the extra ticks cost almost nothing when the
	// pool is at its target size.
	ReconcileInterval time.Duration

	// ReapInterval is how often the reaper scans paused entries for
	// dead VMs (kernel panic, OOM, manual kill -9). Plan §5 step 5
	// targets <10 s detection; default 5 s comfortably hits that.
	// Tests shorten to surface dead VMs within their ~1 s timeouts.
	ReapInterval time.Duration

	// MinHot / MaxHot size the always-running tier (plan §5 step 3).
	// Hot entries are booted but NEVER paused — Acquire skips Resume
	// entirely for them, so leases are a few µs of state transition.
	// SetNetwork still runs since each lease gets its own IP.
	//
	// MaxHot=0 (default) disables the hot tier — the pool is
	// paused-only, which is what 99% of templates want. Turn it on
	// for templates where startup cost matters AND the extra RSS
	// (a hot VM holds full guest memory, a paused one sits at the
	// kernel's idle footprint) is acceptable.
	MinHot int
	MaxHot int
}

// defaultsApplied returns a copy of cfg with zero-valued fields set
// to the Fase 5 defaults. The MinPaused/MaxPaused pair is treated as
// a unit — if BOTH are zero we apply the Fase 5 defaults; if EITHER
// is non-zero we honor the explicit values (a caller passing
// MinPaused=0, MaxPaused=8 wants "no warm minimum, cap at 8" — not
// the default 4/8 — and tests rely on MinPaused=0 to disable the
// refiller's create loop entirely).
func (c Config) defaultsApplied() Config {
	if c.MinPaused == 0 && c.MaxPaused == 0 {
		c.MinPaused = 4
		c.MaxPaused = 8
	}
	// MaxPaused unset but MinPaused set: pull MaxPaused up so the
	// pair stays consistent (Validate rejects MaxPaused < MinPaused).
	// Picks the larger of the Fase 5 default (8) and 2× MinPaused so
	// generous MinPaused configs still get headroom.
	if c.MaxPaused == 0 && c.MinPaused > 0 {
		c.MaxPaused = 8
		if 2*c.MinPaused > c.MaxPaused {
			c.MaxPaused = 2 * c.MinPaused
		}
	}
	if c.ReplenishParallelism == 0 {
		c.ReplenishParallelism = 2
	}
	if c.ConsecutiveFailureThreshold == 0 {
		c.ConsecutiveFailureThreshold = 3
	}
	if c.Cooldown == 0 {
		c.Cooldown = 60 * time.Second
	}
	if c.ReconcileInterval == 0 {
		c.ReconcileInterval = 50 * time.Millisecond
	}
	if c.ReapInterval == 0 {
		c.ReapInterval = 5 * time.Second
	}
	return c
}

// Validate checks the Config is internally consistent. Returned
// errors are informative — the Manager surfaces them on register.
func (c Config) Validate() error {
	if c.TemplateID == "" {
		return errors.New("pool: TemplateID required")
	}
	if c.RunOptions.KernelPath == "" {
		return errors.New("pool: RunOptions.KernelPath required")
	}
	if c.RunOptions.Image == "" && c.RunOptions.Dockerfile == "" {
		return errors.New("pool: RunOptions.Image or RunOptions.Dockerfile required")
	}
	applied := c.defaultsApplied()
	if applied.MinPaused < 0 {
		return fmt.Errorf("pool: MinPaused=%d must be ≥0", applied.MinPaused)
	}
	if applied.MaxPaused < applied.MinPaused {
		return fmt.Errorf("pool: MaxPaused=%d must be ≥ MinPaused=%d", applied.MaxPaused, applied.MinPaused)
	}
	if applied.ReplenishParallelism <= 0 {
		return fmt.Errorf("pool: ReplenishParallelism=%d must be >0", applied.ReplenishParallelism)
	}
	return nil
}

// Entry is one pooled sandbox. Exported for expvar / debug endpoints
// and for tests. Lifecycle is owned by Pool — callers never construct
// Entry directly.
type Entry struct {
	ID        string
	State     State
	CreatedAt time.Time
	LeasedAt  time.Time
	// UDSPath is the HOST-VISIBLE Firecracker-style UDS for this VM's
	// toolbox agent. Used by the lease path to issue SetNetwork and
	// by callers to stream exec/files via internal/toolbox/client.
	UDSPath string
	// GuestIP is set on Lease when SetNetwork succeeded. Empty for
	// paused entries.
	GuestIP string
	// result holds the live container.RunResult for paused/leased VMs.
	// Released to nil when the entry transitions to Stopped.
	result *container.RunResult
	// resumer is the minimal vm-handle subset the lease path calls
	// (Resume). Kept as its own field so tests can inject a fake
	// without implementing the full vmm.Handle interface; production
	// sets it to result.VM which satisfies Resumer implicitly.
	resumer Resumer
	// stater is the minimal vm-handle subset the reaper polls.
	// Nil = reaper skips this entry (test entries created without a
	// stater are immortal — useful for bench code that never wants
	// the reaper interfering).
	stater Stater
	// lastError is the error that pushed this entry into StateError.
	// Drained by the refiller's failure-backoff logic.
	lastError error
}

// ErrPoolEmpty is returned by Acquire when no paused sandbox is
// available. The refiller is asynchronous; callers that need to wait
// for replenishment use AcquireWait (slice 2+) instead.
var ErrPoolEmpty = errors.New("pool: no paused sandbox available")

// ErrNotFound is returned by Release when the ID is not in the pool.
// Typically means a double-release or a use-after-stop.
var ErrNotFound = errors.New("pool: entry not found")

// ErrNotLeased is returned by Release when the entry exists but is
// not in StateLeased. Guards against double-release racing with a
// reaper that has already transitioned the entry to StateStopped.
var ErrNotLeased = errors.New("pool: entry not in leased state")

// Pool owns a set of pre-booted sandboxes for a single template.
// Thread-safe: Acquire, Release, and (once wired in slice 2) the
// refiller all hold p.mu for any read/write of the entries map.
//
// This slice's Pool does NOT refill itself — callers must push entries
// in via AddPaused (tests) to populate it. Wiring the refiller onto
// pkg/container.Run lands in slice 2.
type Pool struct {
	cfg       Config
	booter    Booter    // nil until Start; tests inject via NewPoolWithBooter.
	networker Networker // nil = lease skips SetNetwork; tests inject via SetNetworker.

	mu      sync.Mutex
	entries map[string]*Entry

	// availableHot and availablePaused are FIFO buckets of entries
	// currently eligible for Acquire. Head = oldest, tail = newest.
	// They mirror entries[*].State and are maintained in lockstep with
	// every state transition. Acquire pops the head (preferring hot
	// over paused), turning the previous O(n) entries-map scan into
	// an O(1) lookup. Under burst load (50 concurrent Acquires
	// against a pool with 70 entries) this drops the held-lock work
	// per Acquire from ~hundreds of µs to a few µs — see
	// BenchmarkPoolAcquire_Many for the regression guard.
	availableHot    *list.List
	availablePaused *list.List
	// availablePos maps entry ID → its element in whichever bucket
	// the entry currently lives in. Used for O(1) removal on every
	// non-hot/paused state transition (Lease, Stop, Release).
	availablePos map[string]*list.Element

	// Refiller state (slice 2). All mutated under p.mu.
	started             bool
	inflight            int // legacy total; kept for ABI compat with Inflight()
	inflightHot         int
	inflightPaused      int
	consecutiveFailures int
	cooldownUntil       time.Time
	lastBootError       error

	// globalBudget caps cross-template concurrent cold-boots. Nil =
	// unlimited (per-template ReplenishParallelism is the only
	// ceiling). Set via SetGlobalBudget after NewPool; Manager-level
	// integrators share one Budget across every pool they register.
	globalBudget *GlobalInflightBudget

	// Lifecycle. ctx/cancel are set by Start, cleared by Stop. wg
	// tracks the refiller goroutine + every in-flight Booter call so
	// Stop blocks until all of them finish. triggerCh is a buffered
	// (size 1) channel so TriggerReconcile never blocks.
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	triggerCh chan struct{}
	// warmAvailableCh is closed-and-replaced every time the refiller
	// successfully adds a paused entry. AcquireWait selects on it to
	// wake up as soon as new warm capacity lands, rather than polling
	// the entries map. Guarded by p.mu.
	warmAvailableCh chan struct{}
}

// NewPool applies defaults, validates, and returns a zero-entry pool.
// No goroutines are spawned yet — this keeps slice 1 synchronous.
func NewPool(cfg Config) (*Pool, error) {
	cfg = cfg.defaultsApplied()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Pool{
		cfg:             cfg,
		entries:         map[string]*Entry{},
		availableHot:    list.New(),
		availablePaused: list.New(),
		availablePos:    map[string]*list.Element{},
		warmAvailableCh: make(chan struct{}),
	}, nil
}

// enqueueAvailableLocked adds the entry to the FIFO bucket matching its
// current State (hot or paused). No-op for any other state. Idempotent
// w.r.t. an already-queued entry. Caller must hold p.mu.
func (p *Pool) enqueueAvailableLocked(e *Entry) {
	if e == nil {
		return
	}
	if _, already := p.availablePos[e.ID]; already {
		return
	}
	var bucket *list.List
	switch e.State {
	case StateHot:
		bucket = p.availableHot
	case StatePaused:
		bucket = p.availablePaused
	default:
		return
	}
	p.availablePos[e.ID] = bucket.PushBack(e)
}

// dequeueAvailableLocked removes the entry from whichever bucket it's
// currently in, if any. Safe to call on entries that aren't queued.
// Caller must hold p.mu.
func (p *Pool) dequeueAvailableLocked(e *Entry) {
	if e == nil {
		return
	}
	elem, ok := p.availablePos[e.ID]
	if !ok {
		return
	}
	delete(p.availablePos, e.ID)
	switch e.State {
	case StateHot:
		p.availableHot.Remove(elem)
	case StatePaused:
		p.availablePaused.Remove(elem)
	default:
		// State changed since we enqueued — fall back to a slow but
		// correct removal: try both buckets.
		for _, b := range []*list.List{p.availableHot, p.availablePaused} {
			for x := b.Front(); x != nil; x = x.Next() {
				if x == elem {
					b.Remove(elem)
					return
				}
			}
		}
	}
}

// signalWarmAvailableLocked closes the current warmAvailableCh and
// installs a fresh one. Must be called with p.mu held. The
// close-and-replace pattern is the standard Go idiom for "broadcast
// once" — every goroutine currently selecting on the channel wakes
// up exactly once, and subsequent selects observe the new (open)
// channel until the next refill lands.
func (p *Pool) signalWarmAvailableLocked() {
	close(p.warmAvailableCh)
	p.warmAvailableCh = make(chan struct{})
}

// Config returns a copy of the pool's configuration. Exposed so
// tests and the Manager can introspect defaults without peeking at
// the unexported field.
func (p *Pool) Config() Config {
	return p.cfg
}

// Snapshot returns a copy of every entry in the pool sorted by
// CreatedAt ascending. Safe to iterate without holding any lock.
func (p *Pool) Snapshot() []Entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Entry, 0, len(p.entries))
	for _, e := range p.entries {
		out = append(out, *e)
	}
	return out
}

// CountByState returns how many entries are currently in each state.
// Zero-counts are omitted. O(n) scan under p.mu.
func (p *Pool) CountByState() map[State]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	counts := map[State]int{}
	for _, e := range p.entries {
		counts[e.State]++
	}
	return counts
}

// AddPaused inserts an already-booted-and-paused entry into the pool.
// Test-only API (production goes through the refiller); kept exported
// because lease/refiller tests across the package need it. udsPath
// surfaces on the returned Lease; resumer is invoked on Acquire so
// tests can assert Resume() is called without standing up a VM.
// stater is polled by the reaper — pass nil to opt out of reap (the
// entry is treated as immortal).
func (p *Pool) AddPaused(id string, result *container.RunResult, udsPath string, resumer Resumer, stater Stater) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := &Entry{
		ID:        id,
		State:     StatePaused,
		CreatedAt: time.Now(),
		UDSPath:   udsPath,
		result:    result,
		resumer:   resumer,
		stater:    stater,
	}
	p.entries[id] = e
	p.enqueueAvailableLocked(e)
}

// LeaseSpec configures what the pool does to a paused sandbox on the
// way out the door. All fields optional — zero-value LeaseSpec means
// "just flip the state to leased" (slice 1 behavior; tests use this).
type LeaseSpec struct {
	// IP / Gateway / MAC / Interface are passed through to the
	// guest's toolbox agent via Networker.SetNetwork. Empty IP =
	// skip SetNetwork entirely (e.g. the caller wants to own IP
	// assignment themselves). Gateway / MAC / Interface are
	// no-ops unless IP is set.
	IP        string
	Gateway   string
	MAC       string
	Interface string

	// ResumeTimeout caps the vm.Resume() step. Default 2 s.
	ResumeTimeout time.Duration
	// SetNetworkTimeout caps the Networker.SetNetwork step. Default
	// 2 s. Plan §5 target is 15 ms, so 2 s leaves two orders of
	// magnitude of slack for pathological runs without stalling the
	// caller indefinitely.
	SetNetworkTimeout time.Duration

	// CodeDisks are virtio-blk drives the caller wants attached at
	// lease time — Phase 3 of code-disk-attach. The wire shape is
	// here so sandboxd / SDKs can route per-lease code disks to the
	// pool, but runtime application is **not yet implemented**: the
	// pool's existing warm-resume model gives out already-restored
	// VMs, and gocracker has no virtio-blk hot-plug path to attach
	// new drives to a running guest. Today the field is accepted and
	// preserved (so callers can wire it through SDKs without a future
	// API break) but is a no-op on Acquire — making it functional
	// requires a restore-on-demand pool mode that re-restores the
	// snapshot at lease time with these drives merged in. Tracked in
	// docs/design/code-disk-attach.md.
	CodeDisks []container.CodeDisk
}

// Lease is what Acquire returns on success. Callers stash lease.ID
// to call Release; lease.UDSPath to talk to the guest's toolbox
// agent; lease.GuestIP when SetNetwork actually applied one.
type Lease struct {
	ID       string
	UDSPath  string
	GuestIP  string
	LeasedAt time.Time
}

// Networker applies guest network config. In production the
// containerNetworker wraps internal/toolbox/client.SetNetwork; tests
// inject a fake.
type Networker interface {
	SetNetwork(ctx context.Context, udsPath, ip, gateway, mac, iface string) error
}

// Resumer is the minimal subset of vmm.Handle the lease path needs.
// Kept as its own interface so tests can inject a fake Resumer
// without standing up a real VMM.
type Resumer interface {
	Resume() error
}

// Stater is the minimal subset of vmm.Handle the reaper needs to
// detect dead VMs (kernel panic, OOM, manual kill -9 of the worker).
// Returns true once the VM has terminated and is no longer usable —
// the reaper transitions such entries to StateStopped and triggers a
// refill so the dead slot gets replaced.
//
// Production wires this via a thin adapter over vmm.Handle.State():
// `s.h.State() == vmm.StateStopped` (see containerBooter). Tests
// inject a fake Stater that flips on a channel/atomic so they don't
// have to mock the full vmm package.
type Stater interface {
	Stopped() bool
}

// SetNetworker configures a pool-wide Networker used on every Lease.
// Must be called before Start. Nil is fine — Lease just skips the
// SetNetwork step (useful when the caller owns IP assignment out-of-
// band, e.g. early demos before the IP allocator from slice 4 lands).
func (p *Pool) SetNetworker(n Networker) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.networker = n
}

// Acquire picks the oldest paused entry, Resume()s the VM, optionally
// SetNetwork()s the guest per spec, and returns a Lease. Returns
// ErrPoolEmpty when no paused entry is available.
//
// Picking the OLDEST paused entry (FIFO) keeps long-idle VMs
// exercised, surfacing any "paused sandbox rots after N hours" bug
// early instead of hiding it behind a LIFO that only serves the
// freshest entries.
//
// On Resume failure the entry is transitioned to StateStopped and
// the underlying RunResult is returned via the error path so the
// caller can tear it down — a VM that can't Resume is unusable and
// leaving it in the pool would just make the next Acquire pick the
// same broken entry.
func (p *Pool) Acquire(ctx context.Context, spec LeaseSpec) (Lease, error) {
	if spec.ResumeTimeout == 0 {
		spec.ResumeTimeout = 2 * time.Second
	}
	if spec.SetNetworkTimeout == 0 {
		spec.SetNetworkTimeout = 2 * time.Second
	}

	p.mu.Lock()
	// O(1) selection: hot bucket wins (lease cost is lower — no
	// Resume). Within each bucket the head is the oldest entry —
	// enqueueAvailableLocked PushBacks at boot time, so FIFO ordering
	// keeps long-idle VMs exercised. The previous O(n) scan over the
	// entries map showed up under burst load (50 concurrent Acquires
	// vs ~70-entry pool added ~700 µs of held-lock time per call).
	var elem *list.Element
	if elem = p.availableHot.Front(); elem == nil {
		elem = p.availablePaused.Front()
	}
	if elem == nil {
		p.mu.Unlock()
		return Lease{}, ErrPoolEmpty
	}
	oldest := elem.Value.(*Entry)
	wasHot := oldest.State == StateHot
	p.dequeueAvailableLocked(oldest)
	oldest.State = StateLeased
	metricLeases.Add(1)
	p.publishGaugesLocked()
	oldest.LeasedAt = time.Now()
	lease := Lease{
		ID:       oldest.ID,
		UDSPath:  oldest.UDSPath,
		GuestIP:  oldest.GuestIP, // entry's baked-in IP (from refill cold-boot)
		LeasedAt: oldest.LeasedAt,
	}
	resumer := oldest.resumer
	networker := p.networker
	p.mu.Unlock()

	// Resume OUTSIDE p.mu: vm.Resume walks all vCPUs + re-arms
	// device state; it's fast but non-trivial, and holding the
	// pool lock across it would serialise every Acquire — exactly
	// the concurrency we're trying to enable.
	//
	// Hot tier skips Resume entirely — the VM is already running.
	var resumeMs, setnetMs int64
	if resumer != nil && !wasHot {
		t0 := time.Now()
		resumeCtx, cancel := context.WithTimeout(ctx, spec.ResumeTimeout)
		if err := resumeWithContext(resumeCtx, resumer); err != nil {
			cancel()
			p.markStoppedAndReturn(lease.ID)
			return Lease{}, fmt.Errorf("pool: resume %s: %w", lease.ID, err)
		}
		cancel()
		resumeMs = time.Since(t0).Milliseconds()
	}

	// SetNetwork: only when caller explicitly provides a DIFFERENT IP.
	// Warm leases normally pass spec.IP="" and reuse the entry's
	// baked-in IP (from cold-boot's hostnet.NewAuto), which avoids a
	// 15–20 ms host↔guest netlink round trip on the hot path. Callers
	// that need a specific IP (e.g. deterministic test fixtures) still
	// go through the slower path by setting spec.IP.
	if spec.IP != "" && networker != nil && lease.UDSPath != "" {
		t1 := time.Now()
		netCtx, cancel := context.WithTimeout(ctx, spec.SetNetworkTimeout)
		err := networker.SetNetwork(netCtx, lease.UDSPath, spec.IP, spec.Gateway, spec.MAC, spec.Interface)
		cancel()
		setnetMs = time.Since(t1).Milliseconds()
		if err != nil {
			p.markStoppedAndReturn(lease.ID)
			return Lease{}, fmt.Errorf("pool: setnetwork %s: %w", lease.ID, err)
		}
		p.mu.Lock()
		if e, ok := p.entries[lease.ID]; ok {
			e.GuestIP = spec.IP
		}
		p.mu.Unlock()
		lease.GuestIP = spec.IP
	}
	// The lease-timing log is informational; emit it async so the
	// underlying logger's syscall (write to stderr/file) cannot show up
	// on the request goroutine's hot path. Affects p99 jitter under
	// burst load. Goroutine spawn is ~µs and the snapshot of values
	// captured below makes it safe to read after we return.
	go func(id string, resumeMs, setnetMs int64, guestIP string) {
		gclog.VMM.Info("lease timing", "id", id, "resume_ms", resumeMs, "setnet_ms", setnetMs, "guest_ip", guestIP)
	}(lease.ID, resumeMs, setnetMs, lease.GuestIP)

	return lease, nil
}

// resumeWithContext runs vm.Resume() in a goroutine so the caller's
// ResumeTimeout actually caps it — Resume itself has no ctx param.
// The goroutine is fire-and-forget on timeout; Resume is idempotent
// so a late completion doesn't wedge state (and we've already marked
// the VM as unusable).
func resumeWithContext(ctx context.Context, vm Resumer) error {
	done := make(chan error, 1)
	go func() {
		done <- vm.Resume()
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// markStoppedAndReturn transitions lease.ID to StateStopped under
// p.mu. Called from Acquire error paths so the poisoned entry is
// removed from the pool's paused population.
func (p *Pool) markStoppedAndReturn(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[id]; ok {
		// dequeue is a no-op if the entry was already in StateLeased
		// (the typical Acquire-error path) — buckets only hold
		// Hot/Paused entries.
		p.dequeueAvailableLocked(e)
		e.State = StateStopped
	}
}

// AcquireWait is Acquire that blocks until a paused entry becomes
// available, ctx is canceled, or timeout elapses. The wait listens
// on p.warmAvailableCh which the refiller closes-and-replaces on
// every successful boot — sub-millisecond wake latency, no polling.
//
// Use over Acquire when the caller cannot tolerate ErrPoolEmpty (the
// HTTP /sandboxes/lease handler in slice 7 is the canonical user).
// Eager-refill: the refiller is woken via TriggerReconcile every time
// AcquireWait observes an empty pool, so even a cold burst converges
// to the steady state quickly without manual prodding.
func (p *Pool) AcquireWait(ctx context.Context, spec LeaseSpec, timeout time.Duration) (Lease, error) {
	deadline := time.Now().Add(timeout)
	for {
		// Try a non-blocking Acquire first. The fast path avoids the
		// channel handshake when the pool is steadily warm.
		lease, err := p.Acquire(ctx, spec)
		if err == nil {
			// Eager-refill: replenish the slot we just consumed.
			// Slice 7 will plug the IP allocator + Networker into
			// LeaseSpec; here we only need the wake.
			p.TriggerReconcile()
			return lease, nil
		}
		if !errors.Is(err, ErrPoolEmpty) {
			return Lease{}, err
		}

		// Pool empty. Wake the refiller (in case the periodic ticker
		// hasn't fired yet) and park on the broadcast channel.
		p.TriggerReconcile()

		p.mu.Lock()
		ch := p.warmAvailableCh
		p.mu.Unlock()

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return Lease{}, fmt.Errorf("pool: AcquireWait timeout after %s: %w", timeout, ErrPoolEmpty)
		}
		select {
		case <-ch:
			// Refill landed; loop and try Acquire again. The new
			// entry may have been picked off by another waiting
			// goroutine in the race; that's fine, we retry.
		case <-ctx.Done():
			return Lease{}, ctx.Err()
		case <-time.After(remaining):
			return Lease{}, fmt.Errorf("pool: AcquireWait timeout after %s: %w", timeout, ErrPoolEmpty)
		}
	}
}

// Release transitions the entry to StateStopped and returns its
// underlying RunResult so the caller can shut down the VM. The pool
// does not call result.Close() itself — teardown can block and
// holding p.mu through it would serialize every Acquire/Release.
//
// Returns ErrNotFound if the id is unknown (double-release or pre-
// reaped), ErrNotLeased if the entry exists but isn't leased.
func (p *Pool) Release(id string) (*container.RunResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[id]
	if !ok {
		return nil, ErrNotFound
	}
	if e.State != StateLeased {
		return nil, ErrNotLeased
	}
	// Leased entries aren't in any availability bucket; the dequeue is
	// a defensive no-op.
	p.dequeueAvailableLocked(e)
	e.State = StateStopped
	rr := e.result
	e.result = nil
	return rr, nil
}
