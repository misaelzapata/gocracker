package pool

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gocracker/gocracker/pkg/container"
)

// fakeStater satisfies Stater. The pool's reaper polls Stopped(); set
// stopped=1 (atomically) to simulate a kernel panic / OOM / kill -9.
type fakeStater struct {
	stopped int32 // atomic; non-zero = dead
}

func (f *fakeStater) Stopped() bool { return atomic.LoadInt32(&f.stopped) != 0 }
func (f *fakeStater) markDead()      { atomic.StoreInt32(&f.stopped, 1) }

// reaperCfg builds a Config tuned for sub-second reaping.
func reaperCfg() Config {
	return Config{
		TemplateID: "tmpl-reaper",
		RunOptions: container.RunOptions{
			Image:      "alpine:3.20",
			KernelPath: "/k",
		},
		MinPaused:                   0,
		MaxPaused:                   8,
		ReplenishParallelism:        2,
		ConsecutiveFailureThreshold: 3,
		Cooldown:                    100 * time.Millisecond,
		ReconcileInterval:           20 * time.Millisecond,
		// Tight reap interval so the test sees the dead VM within ~50ms.
		ReapInterval: 20 * time.Millisecond,
	}
}

func TestReaper_RemovesDeadPausedEntry(t *testing.T) {
	p, _ := NewPoolWithBooter(reaperCfg(), &fakeBooter{})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { p.Stop(); p.DrainPaused() }()

	stater := &fakeStater{}
	p.AddPaused("a", nil, "", &fakeResumer{}, stater)

	// Sanity: alive at t=0.
	if got := p.CountByState()[StatePaused]; got != 1 {
		t.Fatalf("pre-kill paused=%d, want 1", got)
	}

	// Kill the VM.
	stater.markDead()

	// Within ~3 reap ticks (60ms) the entry should transition.
	if !waitForCondition(500*time.Millisecond, func() bool {
		return p.CountByState()[StatePaused] == 0 && p.CountByState()[StateStopped] == 1
	}) {
		t.Fatalf("dead entry not reaped; counts=%v", p.CountByState())
	}
}

func TestReaper_LeavesAliveEntriesAlone(t *testing.T) {
	p, _ := NewPoolWithBooter(reaperCfg(), &fakeBooter{})
	_ = p.Start(context.Background())
	defer func() { p.Stop(); p.DrainPaused() }()

	for i := 0; i < 3; i++ {
		stater := &fakeStater{} // never marked dead
		p.AddPaused(fakeID(int64(i)), nil, "", &fakeResumer{}, stater)
	}
	// Wait through several reap ticks.
	time.Sleep(150 * time.Millisecond)
	if got := p.CountByState()[StatePaused]; got != 3 {
		t.Errorf("alive entries reaped: paused=%d, want 3", got)
	}
}

func TestReaper_LeavesLeasedEntriesAlone(t *testing.T) {
	p, _ := NewPoolWithBooter(reaperCfg(), &fakeBooter{})
	_ = p.Start(context.Background())
	defer func() { p.Stop(); p.DrainPaused() }()

	stater := &fakeStater{}
	p.AddPaused("a", nil, "", &fakeResumer{}, stater)
	if _, err := p.Acquire(context.Background(), LeaseSpec{}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// Now the entry is leased. Mark the VM dead — reaper must leave
	// it alone (lease lifecycle belongs to the caller).
	stater.markDead()
	time.Sleep(150 * time.Millisecond)
	if got := p.CountByState()[StateLeased]; got != 1 {
		t.Errorf("leased entry was reaped: leased=%d, want 1", got)
	}
}

func TestReaper_NilStaterIsImmortal(t *testing.T) {
	p, _ := NewPoolWithBooter(reaperCfg(), &fakeBooter{})
	_ = p.Start(context.Background())
	defer func() { p.Stop(); p.DrainPaused() }()

	p.AddPaused("a", nil, "", &fakeResumer{}, nil)
	time.Sleep(150 * time.Millisecond)
	if got := p.CountByState()[StatePaused]; got != 1 {
		t.Errorf("nil-Stater entry reaped: paused=%d, want 1 (immortal)", got)
	}
}

// TestReaper_TriggersRefill: after reaping, the refiller should
// notice the gap and replenish toward MinPaused. Combines reaper +
// refiller — confirms the TriggerReconcile bridge between them works.
func TestReaper_TriggersRefill(t *testing.T) {
	cfg := reaperCfg()
	cfg.MinPaused = 2
	cfg.MaxPaused = 4
	booter := &fakeBooter{}
	p, _ := NewPoolWithBooter(cfg, booter)
	_ = p.Start(context.Background())
	defer func() { p.Stop(); p.DrainPaused() }()

	// Wait for refiller to reach MinPaused via cold-boot.
	if !waitForCondition(1*time.Second, func() bool {
		return p.CountByState()[StatePaused] >= 2
	}) {
		t.Fatalf("initial fill failed; counts=%v", p.CountByState())
	}

	// Inject a stater on one of the paused entries so we can kill it.
	// (fakeBooter doesn't supply staters, so we patch in via pool internals.)
	stater := &fakeStater{}
	p.mu.Lock()
	for _, e := range p.entries {
		if e.State == StatePaused {
			e.stater = stater
			break
		}
	}
	p.mu.Unlock()

	preBoots := atomic.LoadInt32(&booter.boots)
	stater.markDead()

	// Within a few reap+refill cycles, the dead entry is replaced and
	// total paused returns to MinPaused. Boots count must advance.
	if !waitForCondition(1*time.Second, func() bool {
		return atomic.LoadInt32(&booter.boots) > preBoots &&
			p.CountByState()[StatePaused] >= 2
	}) {
		t.Fatalf("reap-then-refill loop didn't run; boots=%d preBoots=%d counts=%v",
			atomic.LoadInt32(&booter.boots), preBoots, p.CountByState())
	}
}

func TestReaper_ConfigDefaultIs5s(t *testing.T) {
	got := Config{TemplateID: "x"}.defaultsApplied()
	if got.ReapInterval != 5*time.Second {
		t.Errorf("ReapInterval default = %v, want 5s", got.ReapInterval)
	}
}
