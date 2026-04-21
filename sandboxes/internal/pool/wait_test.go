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

// waitCfg builds a Config tuned for sub-second AcquireWait turnaround.
// MinPaused>0 so the refiller produces entries; ReconcileInterval
// short so the periodic safety net fires fast (event channel should
// beat it but we want the test to finish if it doesn't).
func waitCfg() Config {
	return Config{
		TemplateID: "tmpl-wait",
		RunOptions: container.RunOptions{
			Image:      "alpine:3.20",
			KernelPath: "/k",
		},
		MinPaused:                   2,
		MaxPaused:                   4,
		ReplenishParallelism:        2,
		ConsecutiveFailureThreshold: 3,
		Cooldown:                    100 * time.Millisecond,
		ReconcileInterval:           50 * time.Millisecond,
		ReapInterval:                10 * time.Second, // not exercising reap here
	}
}

func TestAcquireWait_FastPathHitsImmediately(t *testing.T) {
	p, _ := NewPool(baseCfg())
	p.AddPaused("a", nil, "/u", &fakeResumer{}, nil)

	start := time.Now()
	lease, err := p.AcquireWait(context.Background(), LeaseSpec{}, 1*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("AcquireWait: %v", err)
	}
	if lease.ID != "a" {
		t.Errorf("lease.ID = %q, want a", lease.ID)
	}
	// Fast path should be sub-millisecond (no channel wait).
	if elapsed > 10*time.Millisecond {
		t.Errorf("fast-path AcquireWait took %v, want <10ms", elapsed)
	}
}

func TestAcquireWait_BlocksUntilRefill(t *testing.T) {
	booter := &fakeBooter{delay: 100 * time.Millisecond}
	p, _ := NewPoolWithBooter(waitCfg(), booter)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { p.Stop(); p.DrainPaused() }()

	// Wait for refiller to land MinPaused entries.
	if !waitForCondition(1*time.Second, func() bool {
		return p.CountByState()[StatePaused] >= 2
	}) {
		t.Fatalf("initial fill failed")
	}

	// Drain the pool by Acquiring all entries.
	for i := 0; i < 2; i++ {
		if _, err := p.Acquire(context.Background(), LeaseSpec{}); err != nil {
			t.Fatalf("drain Acquire #%d: %v", i, err)
		}
	}
	// Pool now empty. AcquireWait should block until refiller (with
	// 100 ms boot delay) lands a new entry.
	start := time.Now()
	lease, err := p.AcquireWait(context.Background(), LeaseSpec{}, 1*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("AcquireWait: %v (elapsed %v)", err, elapsed)
	}
	if lease.ID == "" {
		t.Errorf("lease.ID empty")
	}
	// Must have actually waited (boot delay 100ms) but not too long.
	if elapsed < 50*time.Millisecond {
		t.Errorf("AcquireWait returned in %v — refiller didn't actually have to boot?", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("AcquireWait took %v — slower than expected", elapsed)
	}
}

func TestAcquireWait_TimeoutReturnsErrPoolEmpty(t *testing.T) {
	// MinPaused=0 so the refiller never fills.
	cfg := waitCfg()
	cfg.MinPaused = 0
	cfg.MaxPaused = 0  // both zero would re-default; keep MaxPaused=0 explicit
	cfg.ReconcileInterval = 100 * time.Millisecond

	// Use a Booter that always errors so even if the pool tried to
	// refill, nothing lands.
	flip := &flipBooter{threshold: 9999}
	p, _ := NewPoolWithBooter(cfg, flip)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { p.Stop(); p.DrainPaused() }()

	start := time.Now()
	_, err := p.AcquireWait(context.Background(), LeaseSpec{}, 100*time.Millisecond)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrPoolEmpty) {
		t.Fatalf("err = %v, want ErrPoolEmpty", err)
	}
	if elapsed < 90*time.Millisecond {
		t.Errorf("AcquireWait returned in %v — should respect 100ms timeout", elapsed)
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("AcquireWait took %v — overshot 100ms timeout by too much", elapsed)
	}
}

func TestAcquireWait_RespectsContextCancel(t *testing.T) {
	cfg := waitCfg()
	cfg.MinPaused = 0
	cfg.MaxPaused = 0
	flip := &flipBooter{threshold: 9999}
	p, _ := NewPoolWithBooter(cfg, flip)
	_ = p.Start(context.Background())
	defer func() { p.Stop(); p.DrainPaused() }()

	ctx, cancel := context.WithCancel(context.Background())
	doneCh := make(chan error, 1)
	go func() {
		_, err := p.AcquireWait(ctx, LeaseSpec{}, 5*time.Second)
		doneCh <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-doneCh:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("AcquireWait did not return after ctx cancel")
	}
}

// TestAcquireWait_BurstConcurrent: 10 goroutines AcquireWait against a
// pool that boots slowly. Refiller produces entries one by one;
// each goroutine should pick up exactly one. None should starve.
func TestAcquireWait_BurstConcurrent(t *testing.T) {
	const concurrency = 10
	cfg := waitCfg()
	cfg.MinPaused = concurrency
	cfg.MaxPaused = concurrency
	cfg.ReplenishParallelism = 4
	booter := &fakeBooter{delay: 30 * time.Millisecond}
	p, _ := NewPoolWithBooter(cfg, booter)
	_ = p.Start(context.Background())
	defer func() { p.Stop(); p.DrainPaused() }()

	var wg sync.WaitGroup
	leases := make([]Lease, concurrency)
	errs := make([]error, concurrency)
	start := time.Now()
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			leases[i], errs[i] = p.AcquireWait(context.Background(), LeaseSpec{}, 3*time.Second)
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	seen := map[string]int{}
	for i, e := range errs {
		if e != nil {
			t.Errorf("goroutine %d: %v", i, e)
			continue
		}
		seen[leases[i].ID]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("lease %q handed out %d times, want 1", id, n)
		}
	}
	if len(seen) != concurrency {
		t.Errorf("got %d distinct leases, want %d", len(seen), concurrency)
	}
	t.Logf("burst-%d AcquireWait elapsed=%v boots=%d",
		concurrency, elapsed, atomic.LoadInt32(&booter.boots))
}

// TestEagerRefill_AcquireTriggersReconcile: a successful Acquire
// should TriggerReconcile so the refiller starts replacing the
// consumed slot without waiting for the next periodic tick.
func TestEagerRefill_AcquireTriggersReconcile(t *testing.T) {
	booter := &fakeBooter{}
	cfg := waitCfg()
	cfg.ReconcileInterval = 5 * time.Second // periodic tick is far away
	p, _ := NewPoolWithBooter(cfg, booter)
	_ = p.Start(context.Background())
	defer func() { p.Stop(); p.DrainPaused() }()

	// Wait for initial fill (refiller's first reconcile fires at t=0).
	if !waitForCondition(1*time.Second, func() bool {
		return p.CountByState()[StatePaused] >= 2
	}) {
		t.Fatalf("initial fill failed")
	}

	preBoots := atomic.LoadInt32(&booter.boots)

	// Acquire (without Wait) — should fire TriggerReconcile via
	// AcquireWait? No, plain Acquire doesn't trigger. Use AcquireWait
	// to exercise the eager-refill path.
	if _, err := p.AcquireWait(context.Background(), LeaseSpec{}, 1*time.Second); err != nil {
		t.Fatalf("AcquireWait: %v", err)
	}

	// Within ~50 ms the refiller should have started a new boot
	// (no waiting for the 5-second periodic tick).
	if !waitForCondition(200*time.Millisecond, func() bool {
		return atomic.LoadInt32(&booter.boots) > preBoots
	}) {
		t.Fatalf("eager refill didn't fire; boots=%d preBoots=%d",
			atomic.LoadInt32(&booter.boots), preBoots)
	}
}
