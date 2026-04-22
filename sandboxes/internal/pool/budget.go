package pool

import "sync"

// GlobalInflightBudget caps cross-template concurrent cold-boots.
// Without it, N templates each at ReplenishParallelism=2 can run
// 2N parallel cold-boots and overwhelm the disk / artifact cache.
// Plan §5 step 4: "GlobalInflightBudget = 8".
//
// Semantics: a pool's reconcile() MUST acquire one budget slot per
// in-flight boot it launches, and release one when that boot
// finishes (whether success or error). The budget doesn't block —
// if remaining capacity is 0 the caller simply launches nothing
// this pass and the periodic reconcile retries next tick (or
// TriggerReconcile fires when another pool releases a slot).
type GlobalInflightBudget struct {
	mu       sync.Mutex
	capacity int
	inUse    int
	// waiters are pools parked for a slot. Triggered on Release so
	// they can reconcile immediately instead of waiting for the
	// next tick. Slice-7 opt-in; slice 5 core just uses polling.
	waiters []*Pool
}

// NewGlobalInflightBudget builds a budget with the given capacity.
// capacity ≤ 0 is treated as unlimited (never blocks).
func NewGlobalInflightBudget(capacity int) *GlobalInflightBudget {
	return &GlobalInflightBudget{capacity: capacity}
}

// remainingLocked returns how many slots are free. Caller owns g.mu.
// Exposed lowercase so only package-internal callers use the
// lock-skipping variant; external code calls Remaining().
func (g *GlobalInflightBudget) remainingLocked() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.capacity <= 0 {
		return 1 << 30 // unbounded
	}
	r := g.capacity - g.inUse
	if r < 0 {
		return 0
	}
	return r
}

// acquireLocked reserves n slots. Caller is responsible for
// verifying n ≤ remainingLocked() first. Separating the check from
// the acquire lets reconcile pick between "launch some of what I
// wanted" vs "launch none" without intermediate state.
func (g *GlobalInflightBudget) acquireLocked(n int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.inUse += n
}

// releaseLocked frees n slots + wakes any parked waiters. Called
// from runCreate's exit path.
func (g *GlobalInflightBudget) releaseLocked(n int) {
	g.mu.Lock()
	freed := g.inUse - n
	if freed < 0 {
		freed = 0
	}
	g.inUse = freed
	waiters := g.waiters
	g.waiters = nil
	g.mu.Unlock()
	for _, w := range waiters {
		w.TriggerReconcile()
	}
}

// Remaining returns the public slot count. O(lock-hop).
func (g *GlobalInflightBudget) Remaining() int {
	if g == nil {
		return 1 << 30
	}
	return g.remainingLocked()
}

// InUse returns the count of slots currently held.
func (g *GlobalInflightBudget) InUse() int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inUse
}

// Capacity returns the budget's max slot count. 0 / negative mean
// unlimited.
func (g *GlobalInflightBudget) Capacity() int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.capacity
}
