package warmpool

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeWorker is a trivial Worker that records Close calls so tests can
// verify release/prune/Close behaviour without touching KVM.
type fakeWorker struct {
	id     string
	closed atomic.Bool
}

func (f *fakeWorker) ID() string { return f.id }
func (f *fakeWorker) Close() error {
	f.closed.Store(true)
	return nil
}

// countingSpawner hands out fake workers and counts how many times it
// was called per key. It also exposes a blocking knob so tests can
// inspect the pool mid-spawn.
type countingSpawner struct {
	mu    sync.Mutex
	calls map[string]int
	block chan struct{} // if non-nil, Spawn waits on it before returning
	fail  bool
}

func newCountingSpawner() *countingSpawner {
	return &countingSpawner{calls: make(map[string]int)}
}

func (c *countingSpawner) spawn(key, dir string) (Worker, error) {
	c.mu.Lock()
	c.calls[key]++
	n := c.calls[key]
	block := c.block
	fail := c.fail
	c.mu.Unlock()

	if block != nil {
		<-block
	}
	if fail {
		return nil, errors.New("spawn failed")
	}
	return &fakeWorker{id: fmt.Sprintf("%s-%d", key, n)}, nil
}

func (c *countingSpawner) countFor(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[key]
}

// waitFor polls condition until it becomes true or d elapses. Keeps the
// tests free of sleeps that guess at goroutine scheduling.
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met after %s", d)
}

func TestNew_Validation(t *testing.T) {
	if _, err := New(Options{Target: 0, Spawn: func(string, string) (Worker, error) { return nil, nil }}); err == nil {
		t.Error("Target=0 should error")
	}
	if _, err := New(Options{Target: 2}); err == nil {
		t.Error("nil Spawn should error")
	}
}

func TestAcquire_EmptyPoolReturnsFalse(t *testing.T) {
	cs := newCountingSpawner()
	p, err := New(Options{Target: 2, Spawn: cs.spawn})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	w, ok, err := p.Acquire("k", "/snap/k")
	if err != nil || ok || w != nil {
		t.Fatalf("empty pool Acquire: w=%v ok=%v err=%v", w, ok, err)
	}
	// Acquire on an empty pool MUST NOT spawn (that defeats the point —
	// the caller would rather cold-boot than wait for a synchronous spawn).
	time.Sleep(20 * time.Millisecond)
	if c := cs.countFor("k"); c != 0 {
		t.Fatalf("Acquire triggered %d spawns; must be 0", c)
	}
}

func TestEnsureRefill_FillsToTarget(t *testing.T) {
	cs := newCountingSpawner()
	p, err := New(Options{Target: 3, Spawn: cs.spawn})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	p.EnsureRefill("k", "/snap/k")
	waitFor(t, time.Second, func() bool { return p.Size("k") == 3 })

	if cs.countFor("k") != 3 {
		t.Fatalf("expected 3 spawns, got %d", cs.countFor("k"))
	}
	// Calling EnsureRefill again on a full pool must be a no-op.
	p.EnsureRefill("k", "/snap/k")
	time.Sleep(20 * time.Millisecond)
	if cs.countFor("k") != 3 {
		t.Fatalf("extra spawns on full pool: %d", cs.countFor("k"))
	}
}

func TestAcquire_PopsAndAutoRefills(t *testing.T) {
	cs := newCountingSpawner()
	p, err := New(Options{Target: 2, Spawn: cs.spawn})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	p.EnsureRefill("k", "/snap/k")
	waitFor(t, time.Second, func() bool { return p.Size("k") == 2 })

	w, ok, err := p.Acquire("k", "/snap/k")
	if err != nil || !ok || w == nil {
		t.Fatalf("Acquire failed: w=%v ok=%v err=%v", w, ok, err)
	}
	// Pool should be refilling back to 2 in the background.
	waitFor(t, time.Second, func() bool { return p.Size("k") == 2 })
	if cs.countFor("k") != 3 {
		t.Fatalf("expected 3 spawns total after 1 Acquire, got %d", cs.countFor("k"))
	}
	if fw, ok := w.(*fakeWorker); !ok || fw.closed.Load() {
		t.Fatal("Acquired worker should not be Closed")
	}
}

func TestRelease_ClosesWorker(t *testing.T) {
	cs := newCountingSpawner()
	p, err := New(Options{Target: 1, Spawn: cs.spawn})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	p.EnsureRefill("k", "/snap/k")
	waitFor(t, time.Second, func() bool { return p.Size("k") == 1 })

	w, _, _ := p.Acquire("k", "/snap/k")
	if err := p.Release(w); err != nil {
		t.Fatalf("Release err: %v", err)
	}
	if !w.(*fakeWorker).closed.Load() {
		t.Fatal("Release should Close the worker")
	}
	if err := p.Release(nil); err != nil {
		t.Fatalf("Release(nil) should be a no-op, got %v", err)
	}
}

func TestEnsureRefill_CapsInflight(t *testing.T) {
	cs := newCountingSpawner()
	cs.block = make(chan struct{})
	p, err := New(Options{Target: 3, Spawn: cs.spawn})
	if err != nil {
		t.Fatal(err)
	}

	// Fire EnsureRefill repeatedly while spawns are blocked. We must
	// never exceed Target=3 parallel Spawn calls.
	for i := 0; i < 10; i++ {
		p.EnsureRefill("k", "/snap/k")
	}
	// Give goroutines a moment to queue up.
	time.Sleep(30 * time.Millisecond)
	if c := cs.countFor("k"); c != 3 {
		t.Fatalf("expected exactly 3 inflight spawns, got %d", c)
	}

	close(cs.block) // let them finish
	waitFor(t, time.Second, func() bool { return p.Size("k") == 3 })
	p.Close()
}

func TestSpawnFailure_DecrementsInflight(t *testing.T) {
	cs := newCountingSpawner()
	cs.fail = true
	p, err := New(Options{Target: 2, Spawn: cs.spawn})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	p.EnsureRefill("k", "/snap/k")
	waitFor(t, time.Second, func() bool { return cs.countFor("k") >= 2 })
	// After failures settle, Size must be 0 AND inflightRefills must
	// have dropped back so a subsequent EnsureRefill retries.
	time.Sleep(30 * time.Millisecond)
	if p.Size("k") != 0 {
		t.Fatalf("failed spawns must not leak workers")
	}
	cs.mu.Lock()
	cs.fail = false
	cs.mu.Unlock()
	p.EnsureRefill("k", "/snap/k")
	waitFor(t, time.Second, func() bool { return p.Size("k") == 2 })
}

func TestPrune_RemovesStaleKeys(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	cs := newCountingSpawner()
	p, err := New(Options{Target: 1, Spawn: cs.spawn, Clock: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	p.EnsureRefill("old", "/snap/old")
	p.EnsureRefill("new", "/snap/new")
	waitFor(t, time.Second, func() bool { return p.Size("old") == 1 && p.Size("new") == 1 })

	// "new" was just used. "old" is about to be stale.
	_, _, _ = p.Acquire("new", "/snap/new")

	// Advance clock 10 min. "new" was used at t+0; "old" has Zero()
	// lastUse so it pruned immediately by design.
	clock.advance(10 * time.Minute)
	removed := p.Prune(5 * time.Minute)
	if removed == 0 {
		t.Fatal("expected at least 1 prune")
	}
	if p.Size("old") != 0 {
		t.Errorf("old key should be evicted, got %d", p.Size("old"))
	}
}

func TestClose_TerminatesAllWorkers(t *testing.T) {
	cs := newCountingSpawner()
	p, err := New(Options{Target: 2, Spawn: cs.spawn})
	if err != nil {
		t.Fatal(err)
	}
	p.EnsureRefill("a", "/a")
	p.EnsureRefill("b", "/b")
	waitFor(t, time.Second, func() bool { return p.Size("a") == 2 && p.Size("b") == 2 })

	// Grab references to every worker so we can assert they're Closed.
	p.mu.Lock()
	var workers []*fakeWorker
	for _, st := range p.pools {
		for _, w := range st.workers {
			workers = append(workers, w.(*fakeWorker))
		}
	}
	p.mu.Unlock()

	if err := p.Close(); err != nil {
		t.Fatalf("Close err: %v", err)
	}
	for _, w := range workers {
		if !w.closed.Load() {
			t.Errorf("worker %s not closed", w.ID())
		}
	}
	// Post-close, Acquire on any key misses.
	if _, ok, _ := p.Acquire("a", "/a"); ok {
		t.Error("Acquire after Close should miss")
	}
}

func TestClose_DrainsInflightSpawns(t *testing.T) {
	// Workers returned by a Spawn that finishes after Close() must be
	// Close()d by spawnOne; otherwise they leak.
	cs := newCountingSpawner()
	cs.block = make(chan struct{})
	p, err := New(Options{Target: 2, Spawn: cs.spawn})
	if err != nil {
		t.Fatal(err)
	}
	p.EnsureRefill("k", "/snap/k")
	time.Sleep(20 * time.Millisecond) // spawns are now in flight & blocked

	_ = p.Close()
	close(cs.block) // unblock late spawns

	// After drain, any workers the spawner still produced must be
	// Close()d automatically. countingSpawner doesn't record closes,
	// so we assert via the absence of leaked Size.
	time.Sleep(20 * time.Millisecond)
	if p.Size("k") != 0 {
		t.Fatalf("post-close pool should be empty, got %d", p.Size("k"))
	}
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
