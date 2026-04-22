package pool

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gocracker/gocracker/pkg/container"
)

func TestAcquire_PrefersHotOverPaused(t *testing.T) {
	p, _ := NewPool(baseCfg())
	// Inject one paused + one hot entry manually (hot first created).
	p.mu.Lock()
	p.entries["hot-a"] = &Entry{
		ID: "hot-a", State: StateHot,
		CreatedAt: time.Now().Add(-time.Hour),
		resumer:   &fakeResumer{},
	}
	p.entries["paused-b"] = &Entry{
		ID: "paused-b", State: StatePaused,
		CreatedAt: time.Now().Add(-2 * time.Hour), // OLDER, would win FIFO
		resumer:   &fakeResumer{},
	}
	p.mu.Unlock()

	lease, err := p.Acquire(context.Background(), LeaseSpec{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// Hot wins over paused even though paused is older — tier
	// preference takes precedence over FIFO.
	if lease.ID != "hot-a" {
		t.Errorf("picked %s, want hot-a (hot tier preferred)", lease.ID)
	}
}

func TestAcquire_HotSkipsResume(t *testing.T) {
	p, _ := NewPool(baseCfg())
	r := &fakeResumer{}
	p.mu.Lock()
	p.entries["h"] = &Entry{
		ID: "h", State: StateHot, CreatedAt: time.Now(), resumer: r,
	}
	p.mu.Unlock()

	if _, err := p.Acquire(context.Background(), LeaseSpec{}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if atomic.LoadInt32(&r.calls) != 0 {
		t.Errorf("hot tier Acquire called Resume %d times, want 0", r.calls)
	}
}

func TestRefiller_MaintainsHotInvariant(t *testing.T) {
	cfg := Config{
		TemplateID: "t",
		RunOptions: container.RunOptions{
			Image: "alpine:3.20", KernelPath: "/k",
		},
		MinHot: 2, MaxHot: 2,
		MinPaused: 0, MaxPaused: 0,
		ReplenishParallelism:        2,
		ConsecutiveFailureThreshold: 3,
		Cooldown:                    100 * time.Millisecond,
		ReconcileInterval:           20 * time.Millisecond,
	}
	b := &fakeBooter{}
	p, _ := NewPoolWithBooter(cfg, b)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { p.Stop(); p.DrainPaused() }()

	if !waitForCondition(2*time.Second, func() bool {
		return p.CountByState()[StateHot] >= 2
	}) {
		t.Fatalf("pool didn't reach MinHot=2; counts=%v", p.CountByState())
	}
}

// TestGlobalBudget_CapsCrossPoolInflight: two pools sharing a
// budget=2 cap their combined in-flight cold-boots at 2 even with
// each pool's ReplenishParallelism=2.
func TestGlobalBudget_CapsCrossPoolInflight(t *testing.T) {
	budget := NewGlobalInflightBudget(2)

	cfg := Config{
		TemplateID: "t",
		RunOptions: container.RunOptions{
			Image: "alpine:3.20", KernelPath: "/k",
		},
		MinPaused: 8, MaxPaused: 8, ReplenishParallelism: 4,
		Cooldown: 100 * time.Millisecond, ReconcileInterval: 20 * time.Millisecond,
	}
	b1 := &fakeBooter{delay: 200 * time.Millisecond}
	b2 := &fakeBooter{delay: 200 * time.Millisecond}

	cfg.TemplateID = "t1"
	p1, _ := NewPoolWithBooter(cfg, b1)
	p1.SetGlobalBudget(budget)

	cfg.TemplateID = "t2"
	p2, _ := NewPoolWithBooter(cfg, b2)
	p2.SetGlobalBudget(budget)

	_ = p1.Start(context.Background())
	_ = p2.Start(context.Background())
	defer func() { p1.Stop(); p2.Stop(); p1.DrainPaused(); p2.DrainPaused() }()

	// Watch combined inflight; must never exceed budget capacity.
	maxInflight := 0
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		n := p1.Inflight() + p2.Inflight()
		if n > maxInflight {
			maxInflight = n
		}
		time.Sleep(5 * time.Millisecond)
	}
	if maxInflight > 2 {
		t.Fatalf("combined inflight peaked at %d, want ≤2 (GlobalInflightBudget)", maxInflight)
	}
	if maxInflight == 0 {
		t.Fatal("budget never saw an in-flight — test setup wrong?")
	}
}

func TestGlobalBudget_ReleaseWakesWaiters(t *testing.T) {
	budget := NewGlobalInflightBudget(1)
	// Acquire the only slot manually.
	budget.acquireLocked(1)
	if budget.Remaining() != 0 {
		t.Fatal("budget not full after acquire")
	}
	// Release — just asserting the call doesn't deadlock. Wake-up
	// mechanic is exercised in the cross-pool test above.
	budget.releaseLocked(1)
	if budget.Remaining() != 1 {
		t.Errorf("remaining=%d after release, want 1", budget.Remaining())
	}
}

func TestGlobalBudget_UnlimitedWhenNil(t *testing.T) {
	var g *GlobalInflightBudget
	if g.Capacity() != 0 {
		t.Errorf("nil Capacity=%d, want 0", g.Capacity())
	}
	if g.InUse() != 0 {
		t.Errorf("nil InUse=%d, want 0", g.InUse())
	}
	if g.Remaining() < 1<<20 {
		t.Errorf("nil Remaining=%d, want effectively unlimited", g.Remaining())
	}
}

// TestExpvarMetrics_ExportsBootCounters: refiller increments boot
// attempts / successes; gauges reflect state. Can't reset global
// expvars between tests, so we only assert deltas.
func TestExpvarMetrics_ExportsBootCounters(t *testing.T) {
	before := metricBootAttempts.Value()
	successBefore := metricBootSuccesses.Value()

	b := &fakeBooter{}
	cfg := baseCfg()
	cfg.MinPaused = 2
	cfg.MaxPaused = 2
	cfg.ReconcileInterval = 20 * time.Millisecond
	p, _ := NewPoolWithBooter(cfg, b)
	_ = p.Start(context.Background())
	defer func() { p.Stop(); p.DrainPaused() }()

	if !waitForCondition(2*time.Second, func() bool {
		return p.CountByState()[StatePaused] >= 2
	}) {
		t.Fatalf("pool didn't fill; counts=%v", p.CountByState())
	}

	if metricBootAttempts.Value() < before+2 {
		t.Errorf("boot_attempts_total didn't advance: before=%d after=%d", before, metricBootAttempts.Value())
	}
	if metricBootSuccesses.Value() < successBefore+2 {
		t.Errorf("boot_successes_total didn't advance: before=%d after=%d", successBefore, metricBootSuccesses.Value())
	}
	if metricGaugePaused.Value() < 2 {
		t.Errorf("paused gauge = %d, want ≥2", metricGaugePaused.Value())
	}
}

// Avoid unused import in the file when this sync isn't otherwise referenced.
var _ = sync.Mutex{}
