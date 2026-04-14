// Package warmpool maintains a pool of pre-spawned, snapshot-restored VMM
// worker processes so `gocracker run` can hand back a live microVM in a few
// milliseconds instead of paying the ~250 ms cold-boot cost every time.
//
// The pool is keyed by the warmcache key (pkg/warmcache) — one entry per
// (image, kernel, cmdline, mem, vCPUs, arch) tuple. An entry is a small
// ring of "warmed workers", each a long-running gocracker-vmm subprocess
// that has already restored the snapshot and is ready to serve a request.
//
// Design decisions worth calling out:
//
//   - The pool depends on a Spawner interface (not on internal/worker
//     directly). This keeps the package unit-testable without a live KVM
//     host — tests inject a fake spawner that returns paper Handles. The
//     production wiring (in a future commit) supplies a spawner that
//     calls worker.LaunchRestoredVMMWithResume and bridges the handle.
//
//   - Release kills the worker. We intentionally do NOT return the worker
//     to the pool for reuse across tenants — every Acquire gets a fresh
//     process with its own COW copy of guest memory. Refill happens in
//     the background so the next Acquire is fast, but the pool still
//     only holds workers that have never served a request.
//
//   - Acquire is non-blocking. On an empty pool, Acquire returns
//     (nil, false, nil) and callers fall through to the cold-boot path.
//     We do NOT spawn on demand — that would make Acquire slow exactly
//     when the pool already failed to pre-warm, which is the worst time
//     to pay that cost. Callers that want "always fast" should call
//     EnsureRefill beforehand.
package warmpool

import (
	"errors"
	"sync"
	"time"
)

// Worker is the minimum surface the pool needs to manage a warmed-up
// microVM. Concrete implementations wrap internal/worker.remoteVM or
// any other vmm.Handle-compatible type. Close must fully terminate the
// process and reclaim its runtime files — it is called on Release and
// on Close() of the pool as a whole.
type Worker interface {
	ID() string
	Close() error
}

// Spawner produces a fresh warmed worker for the given key + snapshot dir.
// It blocks until the worker has loaded the snapshot and is ready to
// serve a request (Paused or Running — implementations decide, but the
// pool treats the returned worker as ready-to-Acquire). The pool calls
// Spawner from background goroutines only; callers should assume
// concurrent invocation across keys.
type Spawner func(key, snapshotDir string) (Worker, error)

// Pool is the warm-worker pool. Safe for concurrent use. The zero value
// is not usable; call New.
type Pool struct {
	mu     sync.Mutex
	target int                  // desired warmed workers per key
	spawn  Spawner              // how to produce a new warmed worker
	now    func() time.Time     // injectable clock for tests
	pools  map[string]*keyState // key -> state
}

type keyState struct {
	snapshotDir string
	workers     []Worker // ready-to-Acquire; pop from the end
	lastUse     time.Time
	// inflightRefills counts Spawner calls currently running for this
	// key. We cap parallel refills at target so we don't create a
	// thundering-herd of subprocesses during warm-up.
	inflightRefills int
}

// Options configures Pool. Target is the number of warmed workers the
// pool tries to keep per key; below this, Refill/EnsureRefill will
// spawn more in the background. Clock is optional; if nil, time.Now
// is used.
type Options struct {
	Target int
	Spawn  Spawner
	Clock  func() time.Time
}

// New constructs a Pool from Options. Returns an error if mandatory
// fields (Target>0, Spawn non-nil) are missing.
func New(opts Options) (*Pool, error) {
	if opts.Target <= 0 {
		return nil, errors.New("warmpool: Target must be >0")
	}
	if opts.Spawn == nil {
		return nil, errors.New("warmpool: Spawn is required")
	}
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Pool{
		target: opts.Target,
		spawn:  opts.Spawn,
		now:    clock,
		pools:  make(map[string]*keyState),
	}, nil
}

// Acquire takes a warmed worker for key if one is available. Returns
// (nil, false, nil) on an empty pool for that key — callers fall
// through to cold boot. Acquire is non-blocking and never calls Spawn.
// After a successful Acquire, Refill is kicked off in the background
// so the next request sees a fresh warm worker waiting.
//
// snapshotDir is the path the pool will pass to Spawn on future refills
// for this key. It is only read on refills, not on the current Acquire
// (the existing warmed workers already hold an open fd or mmap to the
// snapshot file they were spawned from). Passing an empty snapshotDir
// is allowed if refills for this key aren't desired.
func (p *Pool) Acquire(key, snapshotDir string) (Worker, bool, error) {
	p.mu.Lock()
	st, ok := p.pools[key]
	if !ok || len(st.workers) == 0 {
		// Remember the snapshotDir so a follow-up EnsureRefill knows
		// where to spawn from, even on a cold cache-miss.
		if snapshotDir != "" {
			if st == nil {
				st = &keyState{snapshotDir: snapshotDir}
				p.pools[key] = st
			} else if st.snapshotDir == "" {
				st.snapshotDir = snapshotDir
			}
		}
		p.mu.Unlock()
		return nil, false, nil
	}
	// Pop the newest warmed worker (LIFO is fine — all are equivalent).
	n := len(st.workers)
	w := st.workers[n-1]
	st.workers[n-1] = nil
	st.workers = st.workers[:n-1]
	st.lastUse = p.now()
	if snapshotDir != "" && st.snapshotDir == "" {
		st.snapshotDir = snapshotDir
	}
	p.mu.Unlock()

	// Kick a background refill so the next Acquire also hits.
	go p.refillOne(key)

	return w, true, nil
}

// Release terminates a worker that was previously returned by Acquire.
// We never put the worker back in the pool — a freshly-spawned one
// takes its place. Release is a pure convenience for callers; ignoring
// it and calling w.Close() directly is equivalent.
func (p *Pool) Release(w Worker) error {
	if w == nil {
		return nil
	}
	return w.Close()
}

// EnsureRefill schedules background Spawn calls for key until the pool
// holds target warmed workers. Safe to call repeatedly; parallel
// refills are capped at target so overlapping callers don't stampede.
// The snapshotDir is stored on the first call and reused on subsequent
// refills.
func (p *Pool) EnsureRefill(key, snapshotDir string) {
	p.mu.Lock()
	st, ok := p.pools[key]
	if !ok {
		st = &keyState{snapshotDir: snapshotDir}
		p.pools[key] = st
	} else if snapshotDir != "" && st.snapshotDir == "" {
		st.snapshotDir = snapshotDir
	}
	needed := p.target - len(st.workers) - st.inflightRefills
	if needed <= 0 {
		p.mu.Unlock()
		return
	}
	st.inflightRefills += needed
	dir := st.snapshotDir
	p.mu.Unlock()

	for i := 0; i < needed; i++ {
		go p.spawnOne(key, dir)
	}
}

// refillOne fires exactly one spawn (used after an Acquire drains one
// worker from the ring).
func (p *Pool) refillOne(key string) {
	p.mu.Lock()
	st, ok := p.pools[key]
	if !ok || st.snapshotDir == "" {
		p.mu.Unlock()
		return
	}
	if len(st.workers)+st.inflightRefills >= p.target {
		p.mu.Unlock()
		return
	}
	st.inflightRefills++
	dir := st.snapshotDir
	p.mu.Unlock()
	p.spawnOne(key, dir)
}

func (p *Pool) spawnOne(key, dir string) {
	w, err := p.spawn(key, dir)

	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.pools[key]
	if !ok {
		// Pool was Close()d while we were spawning.
		if err == nil && w != nil {
			_ = w.Close()
		}
		return
	}
	st.inflightRefills--
	if err != nil || w == nil {
		return
	}
	// If the pool is already at target (e.g. EnsureRefill fired us
	// alongside a returning Release), discard the extra.
	if len(st.workers) >= p.target {
		_ = w.Close()
		return
	}
	st.workers = append(st.workers, w)
}

// Prune removes pool entries for keys not used in the last maxAge and
// returns the number of workers terminated. An entry with no Acquire
// history uses its creation time as lastUse. Prune does NOT spawn new
// workers or cancel in-flight refills — those finish and are then
// discarded by spawnOne when it sees the key has been evicted.
func (p *Pool) Prune(maxAge time.Duration) int {
	if maxAge <= 0 {
		return 0
	}
	cutoff := p.now().Add(-maxAge)
	var toClose []Worker

	p.mu.Lock()
	for key, st := range p.pools {
		ref := st.lastUse
		if ref.IsZero() {
			// No Acquire has ever happened; treat as stale the moment
			// any workers have been sitting around longer than maxAge.
			// Using the zero time means the entry will be pruned on
			// the first Prune after maxAge — acceptable.
			ref = cutoff.Add(-time.Second)
		}
		if ref.Before(cutoff) {
			toClose = append(toClose, st.workers...)
			delete(p.pools, key)
		}
	}
	p.mu.Unlock()

	for _, w := range toClose {
		_ = w.Close()
	}
	return len(toClose)
}

// Size reports how many warmed workers are currently sitting in the
// pool for key (0 if the key is unknown). Intended for metrics and
// tests; production Acquire/Release calls do not consult it.
func (p *Pool) Size(key string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.pools[key]
	if !ok {
		return 0
	}
	return len(st.workers)
}

// Close terminates every warmed worker across all keys. In-flight
// Spawn calls are left to finish on their own; their returned workers
// will be Close()d by spawnOne when it sees the key entry gone.
func (p *Pool) Close() error {
	p.mu.Lock()
	var toClose []Worker
	for _, st := range p.pools {
		toClose = append(toClose, st.workers...)
	}
	p.pools = map[string]*keyState{}
	p.mu.Unlock()

	var firstErr error
	for _, w := range toClose {
		if err := w.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
