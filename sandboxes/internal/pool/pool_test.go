package pool

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gocracker/gocracker/pkg/container"
)

// baseCfg returns a Config with just enough set for Validate to pass.
// Tests extend this rather than spelling out every field.
func baseCfg() Config {
	return Config{
		TemplateID: "tmpl-test",
		RunOptions: container.RunOptions{
			Image:      "alpine:3.20",
			KernelPath: "/tmp/nonexistent/kernel",
		},
	}
}

func TestConfig_DefaultsApplied(t *testing.T) {
	got := Config{TemplateID: "x"}.defaultsApplied()
	if got.MinPaused != 4 || got.MaxPaused != 8 {
		t.Errorf("MinPaused/MaxPaused defaults = %d/%d, want 4/8", got.MinPaused, got.MaxPaused)
	}
	if got.ReplenishParallelism != 2 {
		t.Errorf("ReplenishParallelism default = %d, want 2", got.ReplenishParallelism)
	}
	if got.ConsecutiveFailureThreshold != 3 {
		t.Errorf("ConsecutiveFailureThreshold default = %d, want 3", got.ConsecutiveFailureThreshold)
	}
	if got.Cooldown != 60*time.Second {
		t.Errorf("Cooldown default = %v, want 60s", got.Cooldown)
	}
}

func TestConfig_DefaultsPreserveExplicitValues(t *testing.T) {
	got := Config{
		TemplateID:                  "x",
		MinPaused:                   1,
		MaxPaused:                   3,
		ReplenishParallelism:        5,
		ConsecutiveFailureThreshold: 9,
		Cooldown:                    time.Second,
	}.defaultsApplied()
	if got.MinPaused != 1 || got.MaxPaused != 3 {
		t.Errorf("explicit Min/Max clobbered: %d/%d", got.MinPaused, got.MaxPaused)
	}
	if got.ReplenishParallelism != 5 {
		t.Errorf("explicit ReplenishParallelism clobbered: %d", got.ReplenishParallelism)
	}
	if got.ConsecutiveFailureThreshold != 9 {
		t.Errorf("explicit FailureThreshold clobbered: %d", got.ConsecutiveFailureThreshold)
	}
	if got.Cooldown != time.Second {
		t.Errorf("explicit Cooldown clobbered: %v", got.Cooldown)
	}
}

func TestConfig_ValidateRejectsMissing(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"no TemplateID", Config{RunOptions: container.RunOptions{Image: "i", KernelPath: "k"}}, "TemplateID required"},
		{"no KernelPath", Config{TemplateID: "t", RunOptions: container.RunOptions{Image: "i"}}, "KernelPath required"},
		{"no Image + no Dockerfile", Config{TemplateID: "t", RunOptions: container.RunOptions{KernelPath: "k"}}, "Image or RunOptions.Dockerfile required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestConfig_ValidateRejectsInconsistentBounds(t *testing.T) {
	cfg := baseCfg()
	cfg.MinPaused = 5
	cfg.MaxPaused = 2 // max < min
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for MaxPaused < MinPaused")
	}
	cfg.MinPaused = -1
	cfg.MaxPaused = 3
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for negative MinPaused")
	}
}

func TestConfig_DockerfileAlsoValid(t *testing.T) {
	cfg := Config{
		TemplateID: "t",
		RunOptions: container.RunOptions{
			Dockerfile: "/some/Dockerfile",
			KernelPath: "/k",
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Dockerfile-only cfg should validate, got: %v", err)
	}
}

func TestNewPool_EmptyStateAfterCreate(t *testing.T) {
	p, err := NewPool(baseCfg())
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if snap := p.Snapshot(); len(snap) != 0 {
		t.Errorf("fresh pool should have 0 entries, got %d", len(snap))
	}
	if counts := p.CountByState(); len(counts) != 0 {
		t.Errorf("fresh pool counts should be empty, got %v", counts)
	}
}

func TestNewPool_PropagatesValidateError(t *testing.T) {
	if _, err := NewPool(Config{}); err == nil {
		t.Fatal("NewPool({}) should fail validation")
	}
}

func TestAcquire_EmptyReturnsErrPoolEmpty(t *testing.T) {
	p, _ := NewPool(baseCfg())
	_, err := p.Acquire(context.Background(), LeaseSpec{})
	if !errors.Is(err, ErrPoolEmpty) {
		t.Fatalf("Acquire on empty = %v, want ErrPoolEmpty", err)
	}
}

func TestAcquire_TransitionsPausedToLeased(t *testing.T) {
	p, _ := NewPool(baseCfg())
	p.AddPaused("a", nil, "", nil)
	if got := p.CountByState()[StatePaused]; got != 1 {
		t.Fatalf("pre-Acquire paused=%d, want 1", got)
	}
	lease, err := p.Acquire(context.Background(), LeaseSpec{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if lease.ID != "a" {
		t.Errorf("got %+v, want id=a", lease)
	}
	if got := p.CountByState()[StateLeased]; got != 1 {
		t.Errorf("post-Acquire leased=%d, want 1", got)
	}
	if got := p.CountByState()[StatePaused]; got != 0 {
		t.Errorf("post-Acquire paused=%d, want 0", got)
	}
}

func TestAcquire_PicksOldestPaused(t *testing.T) {
	p, _ := NewPool(baseCfg())
	p.AddPaused("new", nil, "", nil)
	time.Sleep(10 * time.Millisecond) // ensure distinct CreatedAt
	p.AddPaused("newer", nil, "", nil)
	// Inject an even-older entry by hand to assert ordering.
	p.mu.Lock()
	p.entries["oldest"] = &Entry{
		ID:        "oldest",
		State:     StatePaused,
		CreatedAt: time.Now().Add(-time.Hour),
	}
	p.mu.Unlock()
	e, err := p.Acquire(context.Background(), LeaseSpec{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if e.ID != "oldest" {
		t.Errorf("Acquire picked %s, want oldest", e.ID)
	}
}

func TestAcquire_SkipsLeasedAndStopped(t *testing.T) {
	p, _ := NewPool(baseCfg())
	p.mu.Lock()
	p.entries["leased"] = &Entry{ID: "leased", State: StateLeased, CreatedAt: time.Now().Add(-time.Hour)}
	p.entries["stopped"] = &Entry{ID: "stopped", State: StateStopped, CreatedAt: time.Now().Add(-time.Hour)}
	p.entries["paused"] = &Entry{ID: "paused", State: StatePaused, CreatedAt: time.Now()}
	p.mu.Unlock()
	e, err := p.Acquire(context.Background(), LeaseSpec{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if e.ID != "paused" {
		t.Errorf("Acquire picked %s, want paused (only eligible)", e.ID)
	}
}

func TestRelease_TransitionsLeasedToStopped(t *testing.T) {
	p, _ := NewPool(baseCfg())
	p.AddPaused("a", nil, "", nil)
	_, _ = p.Acquire(context.Background(), LeaseSpec{})
	rr, err := p.Release("a")
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if rr != nil {
		t.Errorf("Release returned result=%v, want nil (AddPaused passed nil)", rr)
	}
	if got := p.CountByState()[StateStopped]; got != 1 {
		t.Errorf("post-Release stopped=%d, want 1", got)
	}
}

func TestRelease_UnknownIDReturnsErrNotFound(t *testing.T) {
	p, _ := NewPool(baseCfg())
	_, err := p.Release("ghost")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Release(ghost) = %v, want ErrNotFound", err)
	}
}

func TestRelease_PausedReturnsErrNotLeased(t *testing.T) {
	p, _ := NewPool(baseCfg())
	p.AddPaused("a", nil, "", nil)
	_, err := p.Release("a")
	if !errors.Is(err, ErrNotLeased) {
		t.Fatalf("Release(paused) = %v, want ErrNotLeased", err)
	}
}

func TestRelease_DoubleReleaseIsRejected(t *testing.T) {
	p, _ := NewPool(baseCfg())
	p.AddPaused("a", nil, "", nil)
	_, _ = p.Acquire(context.Background(), LeaseSpec{})
	if _, err := p.Release("a"); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	if _, err := p.Release("a"); !errors.Is(err, ErrNotLeased) {
		t.Fatalf("double Release = %v, want ErrNotLeased", err)
	}
}

// TestConcurrentAcquire: N goroutines race for N-1 paused entries.
// Exactly one should get ErrPoolEmpty; the others each get a distinct ID.
// Catches any under-the-hood lock skew before slice 2 adds more writers.
func TestConcurrentAcquire(t *testing.T) {
	p, _ := NewPool(baseCfg())
	for i := 0; i < 9; i++ {
		p.AddPaused(string(rune('a'+i)), nil, "", nil)
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		acquired = map[string]int{}
		empties  int
	)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, err := p.Acquire(context.Background(), LeaseSpec{})
			mu.Lock()
			defer mu.Unlock()
			if errors.Is(err, ErrPoolEmpty) {
				empties++
				return
			}
			if err != nil {
				t.Errorf("unexpected err: %v", err)
				return
			}
			acquired[e.ID]++
		}()
	}
	wg.Wait()

	if empties != 1 {
		t.Errorf("got %d empties, want exactly 1", empties)
	}
	if len(acquired) != 9 {
		t.Errorf("got %d distinct acquired, want 9", len(acquired))
	}
	for id, n := range acquired {
		if n != 1 {
			t.Errorf("id %q acquired %d times, want 1 (double-lease)", id, n)
		}
	}
}
