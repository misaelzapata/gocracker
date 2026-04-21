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
	"errors"
	"fmt"
	"sync"
	"time"

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
// Defaults (matching PLAN_SANDBOXD Fase 5 step 3) are applied by
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
}

// defaultsApplied returns a copy of cfg with zero-valued fields set
// to the Fase 5 defaults. Kept separate from the struct literal so
// tests can assert "I passed 0, the pool used the default".
func (c Config) defaultsApplied() Config {
	if c.MinPaused == 0 {
		c.MinPaused = 4
	}
	if c.MaxPaused == 0 {
		c.MaxPaused = 8
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
	// result holds the live container.RunResult for paused/leased VMs.
	// Released to nil when the entry transitions to Stopped.
	result *container.RunResult
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
	cfg Config

	mu      sync.Mutex
	entries map[string]*Entry
}

// NewPool applies defaults, validates, and returns a zero-entry pool.
// No goroutines are spawned yet — this keeps slice 1 synchronous.
func NewPool(cfg Config) (*Pool, error) {
	cfg = cfg.defaultsApplied()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Pool{
		cfg:     cfg,
		entries: map[string]*Entry{},
	}, nil
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
// Slice-1-only API: in slice 2 the refiller will be the sole writer.
// Tests use this to exercise Acquire/Release without standing up real
// VMs.
func (p *Pool) AddPaused(id string, result *container.RunResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries[id] = &Entry{
		ID:        id,
		State:     StatePaused,
		CreatedAt: time.Now(),
		result:    result,
	}
}

// Acquire returns the oldest paused entry, transitioning it to
// StateLeased. Returns ErrPoolEmpty when no paused entry is
// available. The returned Entry is a snapshot — callers must use
// entry.ID for subsequent Release calls, not retained pointers.
//
// Picking the OLDEST paused entry (FIFO) matches the v2 behavior and
// keeps long-idle VMs exercised, which surfaces any "paused sandbox
// rots after N hours" bugs early instead of hiding them under a LIFO
// that only serves the freshest entries.
func (p *Pool) Acquire() (Entry, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var oldest *Entry
	for _, e := range p.entries {
		if e.State != StatePaused {
			continue
		}
		if oldest == nil || e.CreatedAt.Before(oldest.CreatedAt) {
			oldest = e
		}
	}
	if oldest == nil {
		return Entry{}, ErrPoolEmpty
	}
	oldest.State = StateLeased
	oldest.LeasedAt = time.Now()
	return *oldest, nil
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
	e.State = StateStopped
	rr := e.result
	e.result = nil
	return rr, nil
}
