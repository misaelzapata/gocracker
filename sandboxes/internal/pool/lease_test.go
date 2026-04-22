package pool

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeResumer satisfies Resumer. Records call count, optional delay,
// optional error to simulate vm.Resume() failures.
type fakeResumer struct {
	calls   int32
	delay   time.Duration
	failErr error
}

func (f *fakeResumer) Resume() error {
	atomic.AddInt32(&f.calls, 1)
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	return f.failErr
}

// fakeNetworker satisfies Networker; records the last call for
// assertion and honors delay/failErr to exercise error + timeout paths.
type fakeNetworker struct {
	mu      sync.Mutex
	calls   int32
	lastUDS string
	lastIP  string
	lastGw  string
	lastMAC string
	lastIf  string
	delay   time.Duration
	failErr error
}

func (f *fakeNetworker) SetNetwork(ctx context.Context, udsPath, ip, gateway, mac, iface string) error {
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	f.lastUDS, f.lastIP, f.lastGw, f.lastMAC, f.lastIf = udsPath, ip, gateway, mac, iface
	f.mu.Unlock()
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.failErr
}

func TestAcquire_ResumesVM_OnSuccess(t *testing.T) {
	p, _ := NewPool(baseCfg())
	r := &fakeResumer{}
	p.AddPaused("a", nil, "/tmp/fake.sock", r, nil)

	lease, err := p.Acquire(context.Background(), LeaseSpec{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if lease.ID != "a" || lease.UDSPath != "/tmp/fake.sock" {
		t.Errorf("lease = %+v, want id=a uds=/tmp/fake.sock", lease)
	}
	if atomic.LoadInt32(&r.calls) != 1 {
		t.Errorf("Resume calls = %d, want 1", r.calls)
	}
}

func TestAcquire_ResumeFailure_MarksStopped(t *testing.T) {
	p, _ := NewPool(baseCfg())
	r := &fakeResumer{failErr: errors.New("resume boom")}
	p.AddPaused("a", nil, "/tmp/fake.sock", r, nil)

	_, err := p.Acquire(context.Background(), LeaseSpec{})
	if err == nil || !strings.Contains(err.Error(), "resume") {
		t.Fatalf("Acquire err = %v, want resume-related", err)
	}
	// Entry should have transitioned to stopped so a retrying caller
	// doesn't pick the same broken VM.
	if got := p.CountByState()[StateStopped]; got != 1 {
		t.Errorf("post-failure stopped=%d, want 1", got)
	}
	if got := p.CountByState()[StatePaused]; got != 0 {
		t.Errorf("post-failure paused=%d, want 0 (broken VM requeued)", got)
	}
}

func TestAcquire_ResumeTimeout_UsesContext(t *testing.T) {
	p, _ := NewPool(baseCfg())
	r := &fakeResumer{delay: 500 * time.Millisecond}
	p.AddPaused("a", nil, "", r, nil)

	start := time.Now()
	_, err := p.Acquire(context.Background(), LeaseSpec{ResumeTimeout: 30 * time.Millisecond})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Acquire should fail on resume timeout")
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("Acquire took %v, want <100ms (deadline 30ms)", elapsed)
	}
}

func TestAcquire_SetNetwork_OnlyWhenIPProvided(t *testing.T) {
	p, _ := NewPool(baseCfg())
	n := &fakeNetworker{}
	p.SetNetworker(n)

	p.AddPaused("a", nil, "/tmp/a.sock", &fakeResumer{}, nil)
	// No IP → no SetNetwork call.
	if _, err := p.Acquire(context.Background(), LeaseSpec{}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if atomic.LoadInt32(&n.calls) != 0 {
		t.Errorf("SetNetwork called %d times with empty IP, want 0", n.calls)
	}

	// Add a second entry and Acquire with IP.
	p.AddPaused("b", nil, "/tmp/b.sock", &fakeResumer{}, nil)
	lease, err := p.Acquire(context.Background(), LeaseSpec{
		IP:        "198.19.100.2/30",
		Gateway:   "198.19.100.1",
		MAC:       "02:00:00:00:00:02",
		Interface: "eth0",
	})
	if err != nil {
		t.Fatalf("Acquire w/ IP: %v", err)
	}
	if lease.GuestIP != "198.19.100.2/30" {
		t.Errorf("lease.GuestIP = %q, want 198.19.100.2/30", lease.GuestIP)
	}
	if atomic.LoadInt32(&n.calls) != 1 {
		t.Errorf("SetNetwork calls = %d, want 1", n.calls)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.lastUDS != "/tmp/b.sock" || n.lastIP != "198.19.100.2/30" ||
		n.lastGw != "198.19.100.1" || n.lastMAC != "02:00:00:00:00:02" ||
		n.lastIf != "eth0" {
		t.Errorf("Networker got wrong args: uds=%q ip=%q gw=%q mac=%q iface=%q",
			n.lastUDS, n.lastIP, n.lastGw, n.lastMAC, n.lastIf)
	}
}

func TestAcquire_SetNetworkFailure_MarksStopped(t *testing.T) {
	p, _ := NewPool(baseCfg())
	r := &fakeResumer{}
	n := &fakeNetworker{failErr: errors.New("netlink sad")}
	p.AddPaused("a", nil, "/tmp/a.sock", r, nil)
	p.SetNetworker(n)

	_, err := p.Acquire(context.Background(), LeaseSpec{IP: "198.19.100.2/30"})
	if err == nil || !strings.Contains(err.Error(), "setnetwork") {
		t.Fatalf("Acquire err = %v, want setnetwork-related", err)
	}
	if got := p.CountByState()[StateStopped]; got != 1 {
		t.Errorf("stopped=%d, want 1 (failed lease should poison the entry)", got)
	}
	// Resume DID happen (the VM was woken up) — we're not testing
	// rollback, just that the entry is off-pool so a retry won't
	// pick it again.
	if atomic.LoadInt32(&r.calls) != 1 {
		t.Errorf("Resume calls=%d, want 1 (happened before SetNetwork failure)", r.calls)
	}
}

func TestAcquire_NoNetworker_SkipsSetNetworkSilently(t *testing.T) {
	p, _ := NewPool(baseCfg())
	r := &fakeResumer{}
	p.AddPaused("a", nil, "/tmp/a.sock", r, nil)
	// Networker intentionally NOT set.

	lease, err := p.Acquire(context.Background(), LeaseSpec{IP: "198.19.100.2/30"})
	if err != nil {
		t.Fatalf("Acquire should succeed without Networker; got %v", err)
	}
	if lease.GuestIP != "" {
		t.Errorf("lease.GuestIP = %q, want empty (no Networker applied anything)", lease.GuestIP)
	}
}

func TestAcquire_SetNetworkTimeout_Respected(t *testing.T) {
	p, _ := NewPool(baseCfg())
	r := &fakeResumer{}
	n := &fakeNetworker{delay: 500 * time.Millisecond}
	p.AddPaused("a", nil, "/tmp/a.sock", r, nil)
	p.SetNetworker(n)

	start := time.Now()
	_, err := p.Acquire(context.Background(), LeaseSpec{
		IP:                "198.19.100.2/30",
		SetNetworkTimeout: 30 * time.Millisecond,
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Acquire should fail on SetNetwork timeout")
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("Acquire took %v, want <100ms (timeout 30ms)", elapsed)
	}
}

func TestAcquire_NoResumer_SkipsResumeSilently(t *testing.T) {
	p, _ := NewPool(baseCfg())
	// AddPaused without a resumer — tests that don't care about
	// Resume should still be able to exercise the lease path.
	p.AddPaused("a", nil, "/tmp/a.sock", nil, nil)

	lease, err := p.Acquire(context.Background(), LeaseSpec{})
	if err != nil {
		t.Fatalf("Acquire with nil resumer should succeed; got %v", err)
	}
	if lease.ID != "a" {
		t.Errorf("lease.ID = %q, want a", lease.ID)
	}
}

// BenchmarkAcquire_Lease measures pool overhead on the happy path
// (Resume + SetNetwork with fakes). Plan §5 target on real warm-cache
// is <15 ms per lease (3 ms restore + 15 ms SetNetwork budget — the
// budget subsumes SetNetwork plus pool overhead). Anything above a
// few microseconds here would surface lock contention before the real
// network adds its 15 ms and mask the regression. Pre-populate N+10
// paused entries so the bench never hits ErrPoolEmpty.
func BenchmarkAcquire_Lease(b *testing.B) {
	p, _ := NewPool(baseCfg())
	n := &fakeNetworker{}
	p.SetNetworker(n)
	for i := 0; i < b.N+10; i++ {
		p.AddPaused(benchID(i), nil, "/tmp/b.sock", &fakeResumer{}, nil)
	}

	ctx := context.Background()
	spec := LeaseSpec{IP: "198.19.100.2/30", Gateway: "198.19.100.1"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := p.Acquire(ctx, spec); err != nil {
			b.Fatalf("Acquire #%d: %v", i, err)
		}
	}
}

func benchID(i int) string { return "bench-" + itoa(int64(i)) }
