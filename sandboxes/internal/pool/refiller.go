package pool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/gocracker/gocracker/pkg/container"
	"github.com/gocracker/gocracker/pkg/vmm"
)

// BootResult is what Booter hands the refiller on a successful boot.
// Result owns the VM teardown surface (callers run Result.Close() at
// shutdown); UDSPath is the HOST-VISIBLE toolbox UDS for this VM
// (ResolveWorkerHostSidePath'd for jailer-on); Resumer is the minimal
// VM handle the lease path calls for vm.Resume() (typically
// Result.VM which satisfies Resumer via vmm.Handle's Resume());
// Stater is what the reaper polls to detect dead VMs.
//
// Tests can leave Result=nil and still exercise the lease path by
// providing Resumer — teardown just skips when result is nil.
type BootResult struct {
	Result  *container.RunResult
	UDSPath string
	Resumer Resumer
	Stater  Stater
}

// Booter cold-boots a new sandbox for the pool. The default
// implementation (containerBooter) wraps pkg/container.Run + vm.Pause,
// but tests inject fakes via NewPoolWithBooter so they can exercise
// the refiller without standing up real VMs.
//
// Boot MUST be safe for concurrent calls — the refiller invokes up to
// ReplenishParallelism in parallel. Boot SHOULD respect ctx and exit
// promptly when it's canceled (Pool.Stop waits on in-flight creates).
//
// On success Boot returns a PAUSED sandbox ready for Acquire. On
// failure Boot returns an error and the pool tracks it against
// ConsecutiveFailureThreshold.
type Booter interface {
	Boot(ctx context.Context) (*BootResult, error)
}

// containerBooter is the production Booter: cold-boots via
// container.Run and pauses the resulting VM. Any failure after Run
// returns (e.g. Pause() itself fails) tears the VM back down so
// we don't leak a running VM that the pool has forgotten.
type containerBooter struct {
	opts container.RunOptions
}

func (b containerBooter) Boot(ctx context.Context) (*BootResult, error) {
	// container.Run is synchronous and does its own context handling
	// internally. The ctx passed here is checked AFTER Run returns so
	// shutdown cancellation immediately tears down anything that just
	// finished booting.
	//
	// Per-boot UDSPath: for jailer-off the VsockUDSPath is a direct
	// host path, so every VM in the pool needs a unique one (a
	// shared path would either collide at bind() or cross-wire
	// clients into the wrong guest). For jailer-on the configured
	// path is relative to /worker which is per-VM bind-mounted, so
	// it's already unique — no mutation needed.
	opts := b.opts
	if opts.VsockUDSPath == "" && opts.JailerMode != container.JailerModeOn {
		opts.VsockUDSPath = fmt.Sprintf("/tmp/gc-pool-%d-%d.sock", os.Getpid(), time.Now().UnixNano())
	}
	result, err := container.Run(opts)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		result.Close()
		return nil, fmt.Errorf("pool: booter canceled post-boot: %w", err)
	}
	// Drain WarmCapture's background snapshot writer BEFORE pausing.
	// container.Run with WarmCapture=true returns immediately after
	// boot but does the heavy snapshot.json + mem.bin write in a
	// goroutine; subsequent Booter.Boot calls would race and miss
	// the warmcache.Lookup hit, falling back to cold-boot. Waiting
	// here costs ~50-200 ms on the FIRST boot of a template
	// (one-time tax) and zero on every subsequent one. Net effect:
	// the entire pool warms via restore-from-snapshot instead of
	// N parallel cold-boots.
	if result.WarmDone != nil {
		select {
		case <-result.WarmDone:
		case <-ctx.Done():
			result.Close()
			return nil, fmt.Errorf("pool: booter canceled waiting for warm capture: %w", ctx.Err())
		}
	}
	if err := result.VM.Pause(); err != nil {
		result.Close()
		return nil, fmt.Errorf("pool: vm.Pause after boot: %w", err)
	}
	// UDSPath resolution is left to slice 7's HTTP integrator
	// (sandboxd knows about ResolveWorkerHostSidePath + the jailer
	// mount conventions). Here we just pass the internal path
	// through; sandboxd will unwrap it on lease.
	return &BootResult{
		Result:  result,
		UDSPath: opts.VsockUDSPath,
		Resumer: result.VM,
		Stater:  vmHandleStater{h: result.VM},
	}, nil
}

// vmHandleStater adapts vmm.Handle to the pool's Stater interface.
// Returns true once the VM has transitioned to vmm.StateStopped —
// kernel panic, OOM, manual kill of the worker, or any other
// non-recoverable terminal state.
type vmHandleStater struct {
	h vmm.Handle
}

func (s vmHandleStater) Stopped() bool {
	if s.h == nil {
		return false
	}
	return s.h.State() == vmm.StateStopped
}

// NewPoolWithBooter is NewPool with an explicit Booter. Used by tests
// that need a FakeBooter; production code calls NewPool which wires
// the default containerBooter from cfg.RunOptions.
func NewPoolWithBooter(cfg Config, booter Booter) (*Pool, error) {
	p, err := NewPool(cfg)
	if err != nil {
		return nil, err
	}
	if booter == nil {
		return nil, errors.New("pool: nil Booter")
	}
	p.booter = booter
	return p, nil
}

// Start launches the refiller goroutine and returns immediately. The
// refiller runs until ctx is canceled or Stop is called. Calling
// Start twice on the same Pool returns an error — the pool tracks a
// single lifecycle, not a resumable one.
//
// After Start, the pool continuously works to maintain
// len(paused) + inflight ≥ MinPaused, capped at MaxPaused total and
// ReplenishParallelism concurrent cold-boots.
func (p *Pool) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return errors.New("pool: already started")
	}
	if p.booter == nil {
		// Default: cold-boot via container.Run using cfg.RunOptions.
		// Wired lazily here rather than in NewPool so tests that use
		// AddPaused directly don't need to provide a Booter.
		p.booter = containerBooter{opts: p.cfg.RunOptions}
	}
	p.started = true
	p.ctx, p.cancel = context.WithCancel(ctx)
	p.triggerCh = make(chan struct{}, 1)
	p.mu.Unlock()

	p.wg.Add(2)
	go p.refillLoop()
	go p.reapLoop()
	return nil
}

// Stop cancels the refiller context and waits for every in-flight
// create goroutine to finish. Paused entries remain in the pool map —
// the caller is responsible for draining them via Acquire/Release or
// calling DrainPaused to tear them all down.
//
// Safe to call multiple times; subsequent calls are no-ops.
func (p *Pool) Stop() {
	p.mu.Lock()
	if !p.started || p.cancel == nil {
		p.mu.Unlock()
		return
	}
	cancel := p.cancel
	p.cancel = nil
	p.mu.Unlock()
	cancel()
	p.wg.Wait()
}

// TriggerReconcile wakes the refiller loop immediately, bypassing the
// periodic ticker. Non-blocking: if the channel is already full, the
// wake is coalesced with the pending one. Slice 6 wires this to
// Acquire/Release so lease traffic drives refill event-style.
func (p *Pool) TriggerReconcile() {
	p.mu.Lock()
	ch := p.triggerCh
	p.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
		// already a pending wake — nothing to do.
	}
}

// DrainPaused tears down every paused entry in the pool and removes
// them from the map. Called by integrators during shutdown AFTER
// Stop so the refiller isn't racing us. Returns the number of entries
// drained. Leased entries are left untouched — caller must Release
// them first.
func (p *Pool) DrainPaused() int {
	p.mu.Lock()
	toClose := make([]*container.RunResult, 0)
	for id, e := range p.entries {
		if e.State == StatePaused && e.result != nil {
			toClose = append(toClose, e.result)
			e.result = nil
			e.State = StateStopped
			_ = id
		}
	}
	p.mu.Unlock()
	for _, rr := range toClose {
		rr.Close()
	}
	return len(toClose)
}

// refillLoop is the main refiller goroutine. It wakes on a periodic
// ticker (reconcileInterval) and on any TriggerReconcile, then tops
// the paused pool up toward MinPaused without exceeding MaxPaused or
// ReplenishParallelism in-flight. Honors the failure-backoff window:
// while p.cooldownUntil is in the future, no new creates are launched.
func (p *Pool) refillLoop() {
	defer p.wg.Done()

	ticker := time.NewTicker(p.cfg.ReconcileInterval)
	defer ticker.Stop()

	// Kick off an initial reconcile before the first tick so the pool
	// starts warming up at t=0 instead of at t=reconcileInterval.
	p.reconcile()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.reconcile()
		case <-p.triggerCh:
			p.reconcile()
		}
	}
}

// reconcile is one pass of the refiller logic. Holds p.mu only to
// read/mutate counters + launch goroutines — the launched goroutines
// do container.Run WITHOUT holding p.mu (Run blocks for ~200-500 ms).
func (p *Pool) reconcile() {
	p.mu.Lock()
	if !p.started {
		p.mu.Unlock()
		return
	}
	// Count live (paused or creating-in-flight) slots.
	paused := 0
	for _, e := range p.entries {
		if e.State == StatePaused {
			paused++
		}
	}
	live := paused + p.inflight

	// Respect cooldown: if we're inside a failure backoff, skip.
	if now := time.Now(); now.Before(p.cooldownUntil) {
		p.mu.Unlock()
		return
	}

	// Slots we should launch this pass.
	wantMore := p.cfg.MinPaused - live
	if wantMore <= 0 {
		p.mu.Unlock()
		return
	}
	// Cap by per-template parallelism.
	canLaunch := p.cfg.ReplenishParallelism - p.inflight
	if canLaunch <= 0 {
		p.mu.Unlock()
		return
	}
	// Cap by MaxPaused so we never overshoot.
	roomToMax := p.cfg.MaxPaused - live
	if roomToMax < wantMore {
		wantMore = roomToMax
	}
	if canLaunch < wantMore {
		wantMore = canLaunch
	}
	if wantMore <= 0 {
		p.mu.Unlock()
		return
	}

	// Reserve inflight slots BEFORE unlocking so a concurrent
	// reconcile (shouldn't happen — single refiller goroutine — but
	// belt + suspenders for future Manager callers of reconcile)
	// can't double-launch.
	p.inflight += wantMore
	ctx := p.ctx
	booter := p.booter
	p.mu.Unlock()

	for i := 0; i < wantMore; i++ {
		p.wg.Add(1)
		go p.runCreate(ctx, booter)
	}
}

// reapLoop is the dead-VM watcher. Periodically scans paused entries,
// asks each entry's Stater whether the VM is still alive, and
// transitions any dead entries to StateStopped. Triggers reconcile
// after a sweep so the refiller replaces what we reaped.
//
// Only PAUSED entries are reaped — leased entries belong to the caller
// and we don't second-guess their lifecycle (a leased VM that died
// surfaces via the caller's exec/health checks). Entries with nil
// Stater are skipped (test-only AddPaused calls without a Stater).
func (p *Pool) reapLoop() {
	defer p.wg.Done()
	ticker := time.NewTicker(p.cfg.ReapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			if p.reapOnce() > 0 {
				p.TriggerReconcile()
			}
		}
	}
}

// reapOnce sweeps once and returns the count of entries reaped.
// Closing result.Close() happens OUTSIDE p.mu — Close can block
// (Stop a worker subprocess) and holding the pool lock through it
// would stall every Acquire.
func (p *Pool) reapOnce() int {
	type doomed struct {
		id     string
		result *container.RunResult
	}
	var dead []doomed

	p.mu.Lock()
	for id, e := range p.entries {
		if e.State != StatePaused {
			continue
		}
		if e.stater == nil || !e.stater.Stopped() {
			continue
		}
		e.State = StateStopped
		dead = append(dead, doomed{id: id, result: e.result})
		e.result = nil
	}
	p.mu.Unlock()

	for _, d := range dead {
		if d.result != nil {
			d.result.Close()
		}
	}
	return len(dead)
}

// runCreate is one cold-boot-and-pause attempt. It decrements
// p.inflight on exit and either adds a paused entry (on success) or
// bumps the failure counter (on error). Also resets the failure
// counter + cooldown on a successful boot.
func (p *Pool) runCreate(ctx context.Context, booter Booter) {
	defer p.wg.Done()
	bootResult, err := booter.Boot(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inflight--
	if err != nil {
		p.consecutiveFailures++
		p.lastBootError = err
		if p.consecutiveFailures >= p.cfg.ConsecutiveFailureThreshold {
			p.cooldownUntil = time.Now().Add(p.cfg.Cooldown)
		}
		return
	}
	// Boot raced shutdown: tear the VM back down instead of parking
	// it in the map where no one will Release it.
	if ctx.Err() != nil {
		p.mu.Unlock()
		if bootResult != nil && bootResult.Result != nil {
			bootResult.Result.Close()
		}
		p.mu.Lock()
		return
	}
	id := ""
	if bootResult.Result != nil {
		id = bootResult.Result.ID
	}
	if id == "" {
		id = fmt.Sprintf("pool-%d", time.Now().UnixNano())
	}
	p.entries[id] = &Entry{
		ID:        id,
		State:     StatePaused,
		CreatedAt: time.Now(),
		UDSPath:   bootResult.UDSPath,
		result:    bootResult.Result,
		resumer:   bootResult.Resumer,
		stater:    bootResult.Stater,
	}
	p.consecutiveFailures = 0
	p.cooldownUntil = time.Time{}
	// Wake any AcquireWait callers parked on warmAvailableCh — this
	// is the event-driven side of slice 6's refill path. Polling the
	// map every N ms wastes cycles AND adds latency to a burst of
	// concurrent Acquires; broadcasting on each successful boot lets
	// every parked caller race for the new entry immediately.
	p.signalWarmAvailableLocked()
}

// Inflight returns the current count of in-flight cold-boots. Used
// by tests and expvar gauges in slice 7. Protected by p.mu.
func (p *Pool) Inflight() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inflight
}

// ConsecutiveFailures returns the current failure-run length. Resets
// to 0 on any successful boot. Used by tests to assert backoff
// behavior.
func (p *Pool) ConsecutiveFailures() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.consecutiveFailures
}

// CooldownUntil returns the time at which the cooldown window ends
// (zero time when no cooldown is active). Used by tests to assert
// the pool enters backoff after ConsecutiveFailureThreshold errors.
func (p *Pool) CooldownUntil() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cooldownUntil
}

// LastBootError returns the most recent Boot error, or nil if the
// last attempt succeeded. Used by integrators to surface refill
// failures to operators.
func (p *Pool) LastBootError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastBootError
}

// waitForCondition is a test helper — poll-until-condition with a
// timeout. Kept here (not in pool_test.go) because refiller tests use
// it against both Pool internals and Pool behavior.
func waitForCondition(timeout time.Duration, check func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return check()
}

// assert the helper isn't dead code without any test yet.
var _ = waitForCondition
var _ sync.Mutex // keep sync import used when tests live in another file
