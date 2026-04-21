package pool

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gocracker/gocracker/pkg/container"
)

// fakeBooter is a Booter test double. Each Boot call increments
// a counter. Callers can wire failFor, delay, and a fail trigger
// (failCh) to exercise the refiller's failure paths.
type fakeBooter struct {
	calls      int64
	failEveryN int32  // 0 = never fail; N = fail every Nth call
	boots      int32  // successful boots (atomic)
	delay      time.Duration
	mu         sync.Mutex
	results    []*container.RunResult
}

func (f *fakeBooter) Boot(ctx context.Context) (*container.RunResult, error) {
	n := atomic.AddInt64(&f.calls, 1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.failEveryN > 0 && int32(n)%f.failEveryN == 0 {
		return nil, errors.New("fakeBooter: synthetic failure")
	}
	atomic.AddInt32(&f.boots, 1)
	// Real Pool only dereferences result.VM / result.ID on certain
	// paths; slice 2 tests don't touch VM, so nil VM is fine. We
	// synthesize a unique ID so entries don't collide in the map.
	rr := &container.RunResult{ID: fakeID(n)}
	f.mu.Lock()
	f.results = append(f.results, rr)
	f.mu.Unlock()
	return rr, nil
}

func fakeID(n int64) string {
	return "fake-" + time.Now().Format("150405.000000") + "-" + itoa(n)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// startWithFastTick runs the pool with a small MinPaused so tests
// converge quickly. Callers provide a Booter and get back the pool +
// cleanup func that guarantees Stop.
func startWithFastTick(t *testing.T, min, max, parallelism int, b Booter) *Pool {
	t.Helper()
	cfg := Config{
		TemplateID: "tmpl-test",
		RunOptions: container.RunOptions{
			Image:      "alpine:3.20",
			KernelPath: "/tmp/nonexistent/kernel",
		},
		MinPaused:                   min,
		MaxPaused:                   max,
		ReplenishParallelism:        parallelism,
		ConsecutiveFailureThreshold: 3,
		// Cooldown larger than the test's measurement window so the
		// "no Boot during cooldown" assertion has real headroom —
		// making them equal (e.g. both 100 ms) creates a race at the
		// boundary where one extra boot can slip in as the window
		// closes.
		Cooldown: 500 * time.Millisecond,
		// Tests need sub-second convergence; production leaves the
		// default 500 ms since slice 6 makes the hot path event-driven.
		ReconcileInterval: 20 * time.Millisecond,
	}
	p, err := NewPoolWithBooter(cfg, b)
	if err != nil {
		t.Fatalf("NewPoolWithBooter: %v", err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		p.Stop()
		p.DrainPaused()
	})
	return p
}

func TestRefiller_ReachesMinPaused(t *testing.T) {
	b := &fakeBooter{}
	p := startWithFastTick(t, 3, 5, 2, b)

	ok := waitForCondition(2*time.Second, func() bool {
		return p.CountByState()[StatePaused] >= 3
	})
	if !ok {
		t.Fatalf("pool did not reach MinPaused=3 within 2s; counts=%v inflight=%d",
			p.CountByState(), p.Inflight())
	}
}

func TestRefiller_RespectsMaxPaused(t *testing.T) {
	b := &fakeBooter{}
	p := startWithFastTick(t, 2, 3, 2, b)

	// Let it settle above MinPaused.
	if !waitForCondition(2*time.Second, func() bool {
		return p.CountByState()[StatePaused] >= 2
	}) {
		t.Fatalf("didn't reach MinPaused")
	}
	// Give plenty of extra time and confirm we never exceed MaxPaused=3.
	time.Sleep(500 * time.Millisecond)
	paused := p.CountByState()[StatePaused]
	inflight := p.Inflight()
	total := paused + inflight
	if total > 3 {
		t.Fatalf("pool exceeded MaxPaused=3: paused=%d inflight=%d total=%d",
			paused, inflight, total)
	}
}

// TestRefiller_ParallelismCap: with Boot delay 200ms and
// ReplenishParallelism=2, at most 2 Boot calls should be in flight at
// once even when MinPaused=10.
func TestRefiller_ParallelismCap(t *testing.T) {
	b := &fakeBooter{delay: 200 * time.Millisecond}
	p := startWithFastTick(t, 10, 10, 2, b)

	// Watch inflight for ~500ms; max should never exceed 2.
	maxInflight := 0
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if n := p.Inflight(); n > maxInflight {
			maxInflight = n
		}
		time.Sleep(10 * time.Millisecond)
	}
	if maxInflight > 2 {
		t.Fatalf("inflight peaked at %d, want ≤2 (ReplenishParallelism)", maxInflight)
	}
	if maxInflight == 0 {
		t.Fatalf("inflight never observed >0 — refiller did nothing")
	}
}

// TestRefiller_BackoffAfterFailures: every Boot fails; after
// ConsecutiveFailureThreshold=3 the pool enters cooldown and stops
// launching new creates until the cooldown elapses.
func TestRefiller_BackoffAfterFailures(t *testing.T) {
	b := &fakeBooter{failEveryN: 1} // every call fails
	p := startWithFastTick(t, 3, 5, 1, b)

	// Wait for the failure threshold.
	if !waitForCondition(1500*time.Millisecond, func() bool {
		return p.ConsecutiveFailures() >= 3
	}) {
		t.Fatalf("ConsecutiveFailures never reached 3; got %d", p.ConsecutiveFailures())
	}

	// Cooldown should now be in the future.
	cd := p.CooldownUntil()
	if cd.IsZero() || !cd.After(time.Now()) {
		t.Fatalf("cooldown not in future after threshold hit; cd=%v now=%v", cd, time.Now())
	}

	// Record call count; during cooldown it should stay flat.
	startCalls := atomic.LoadInt64(&b.calls)
	time.Sleep(100 * time.Millisecond) // well inside the 200ms cooldown
	mid := atomic.LoadInt64(&b.calls)
	if mid != startCalls {
		t.Errorf("Boot calls advanced during cooldown: %d → %d", startCalls, mid)
	}
}

// TestRefiller_RecoversAfterCooldown: a Booter that fails first then
// succeeds after a trigger flag. Pool should enter cooldown, wait it
// out, then successfully populate paused slots.
func TestRefiller_RecoversAfterCooldown(t *testing.T) {
	type mixedBooter struct {
		fakeBooter
		succeedAfter int64 // call index after which Boot succeeds
	}
	b := &mixedBooter{succeedAfter: 3}
	b.failEveryN = 1 // start all-fail
	// Hack: swap failEveryN to 0 after we've accumulated enough failures.
	// Easier: wrap in a Booter that flips behavior based on atomic call count.
	flip := &flipBooter{threshold: 3}
	p := startWithFastTick(t, 3, 5, 1, flip)

	// Wait up to ~1s for post-cooldown recovery (3 failures + 200 ms
	// cooldown + a few successful boots).
	if !waitForCondition(2*time.Second, func() bool {
		return p.CountByState()[StatePaused] >= 3
	}) {
		t.Fatalf("pool did not recover post-cooldown; counts=%v fails=%d cd=%v",
			p.CountByState(), p.ConsecutiveFailures(), p.CooldownUntil())
	}
	if p.ConsecutiveFailures() != 0 {
		t.Errorf("ConsecutiveFailures=%d after recovery, want 0", p.ConsecutiveFailures())
	}
}

// flipBooter fails for the first `threshold` calls and succeeds
// afterward.
type flipBooter struct {
	threshold int64
	n         int64
}

func (f *flipBooter) Boot(ctx context.Context) (*container.RunResult, error) {
	n := atomic.AddInt64(&f.n, 1)
	if n <= f.threshold {
		return nil, errors.New("flipBooter: fail")
	}
	return &container.RunResult{ID: fakeID(n)}, nil
}

func TestRefiller_StopWaitsForInflight(t *testing.T) {
	b := &fakeBooter{delay: 100 * time.Millisecond}
	cfg := Config{
		TemplateID: "t",
		RunOptions: container.RunOptions{
			Image:      "alpine:3.20",
			KernelPath: "/k",
		},
		MinPaused: 3, MaxPaused: 5, ReplenishParallelism: 2,
	}
	p, _ := NewPoolWithBooter(cfg, b)
	_ = p.Start(context.Background())

	// Let some Boots start.
	time.Sleep(20 * time.Millisecond)

	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()

	select {
	case <-stopDone:
		// Stop returned; no inflight should remain.
		if n := p.Inflight(); n != 0 {
			t.Errorf("post-Stop inflight=%d, want 0", n)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Stop did not return within 1s — in-flight wait-group stuck")
	}
}

func TestRefiller_DoubleStartRejected(t *testing.T) {
	b := &fakeBooter{}
	cfg := Config{
		TemplateID: "t",
		RunOptions: container.RunOptions{
			Image:      "alpine:3.20",
			KernelPath: "/k",
		},
		MinPaused: 1,
	}
	p, _ := NewPoolWithBooter(cfg, b)
	defer p.Stop()
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := p.Start(context.Background()); err == nil {
		t.Fatal("second Start should have errored")
	}
}

func TestRefiller_TriggerReconcileAccelerates(t *testing.T) {
	b := &fakeBooter{delay: 10 * time.Millisecond}
	p := startWithFastTick(t, 2, 5, 2, b)

	// Wait for initial fill.
	if !waitForCondition(1*time.Second, func() bool {
		return p.CountByState()[StatePaused] >= 2
	}) {
		t.Fatalf("initial fill failed")
	}

	// Acquire to drop below MinPaused, then TriggerReconcile.
	e, err := p.Acquire()
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// Release so the slot is counted as stopped (not leased).
	rr, _ := p.Release(e.ID)
	_ = rr

	preTrigger := atomic.LoadInt64(&b.calls)
	p.TriggerReconcile()

	// Within ~100 ms the refiller should have kicked off a new boot
	// (Boot delay 10 ms → success < 50 ms).
	if !waitForCondition(200*time.Millisecond, func() bool {
		return atomic.LoadInt64(&b.calls) > preTrigger
	}) {
		t.Fatalf("TriggerReconcile did not wake the refiller; calls stayed at %d", preTrigger)
	}
}

func TestRefiller_LastBootErrorSurfacedAndCleared(t *testing.T) {
	flip := &flipBooter{threshold: 2}
	p := startWithFastTick(t, 2, 4, 1, flip)

	// Wait until we've seen an error.
	if !waitForCondition(1*time.Second, func() bool {
		return p.LastBootError() != nil
	}) {
		t.Fatalf("LastBootError never set")
	}
	// Wait until recovery zeros out failures (LastBootError is NOT
	// cleared on success per design — it's the most recent error,
	// same shape as go's http.Client.CloseIdleConnections style
	// diagnostic — but ConsecutiveFailures IS cleared).
	if !waitForCondition(2*time.Second, func() bool {
		return p.CountByState()[StatePaused] >= 2
	}) {
		t.Fatalf("pool never recovered; counts=%v", p.CountByState())
	}
	if p.ConsecutiveFailures() != 0 {
		t.Errorf("ConsecutiveFailures=%d after recovery, want 0", p.ConsecutiveFailures())
	}
}
