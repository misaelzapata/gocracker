package vmm

import (
	"testing"
	"time"
)

// TestPauseResumeWakeupLatency proves that Resume wakes a goroutine
// parked in waitIfPaused in well under 5 ms. The previous polling
// implementation always took ≥10 ms because of the time.Sleep(10ms) tick;
// the cond.Broadcast wake-up is goroutine-handoff time (typically tens of
// microseconds even on busy CI runners). The 5 ms budget is loose; the
// BenchmarkPauseResumeWakeup gives the precise number.
func TestPauseResumeWakeupLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping latency test in -short")
	}

	const cycles = 50
	var worst time.Duration
	for i := 0; i < cycles; i++ {
		vm := &VM{
			state:       StatePaused,
			events:      NewEventLog(),
			pausedVCPUs: make(map[int]struct{}),
		}
		started := make(chan struct{})
		woken := make(chan time.Time, 1)

		go func() {
			vm.mu.Lock()
			cond := vm.ensurePauseCondLocked()
			vm.pausedVCPUs[0] = struct{}{}
			close(started)
			for vm.state == StatePaused {
				cond.Wait()
			}
			delete(vm.pausedVCPUs, 0)
			woken <- time.Now()
			vm.mu.Unlock()
		}()

		<-started
		// Give the goroutine a moment to actually park on cond.Wait. Without
		// this the Broadcast may fire before the Wait registers and we'd
		// deadlock — the same constraint a real waitIfPaused has.
		time.Sleep(500 * time.Microsecond)

		t0 := time.Now()
		if err := vm.Resume(); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		select {
		case at := <-woken:
			lat := at.Sub(t0)
			if lat > worst {
				worst = lat
			}
		case <-time.After(200 * time.Millisecond):
			t.Fatalf("cycle %d: goroutine did not wake within 200 ms — cond.Broadcast regression", i)
		}
	}

	t.Logf("worst-case Resume wake-up over %d cycles: %v", cycles, worst)
	if worst > 5*time.Millisecond {
		t.Fatalf("Resume wake-up worst case %v exceeds budget 5ms", worst)
	}
}

// BenchmarkPauseResumeWakeup measures Resume → waiter-wake handoff in a
// tight loop. Sample run before the cond refactor was bounded by
// time.Sleep(10ms); after the refactor we expect tens of microseconds.
//
//	go test -run=^$ -bench=BenchmarkPauseResumeWakeup -count=5 ./pkg/vmm
func BenchmarkPauseResumeWakeup(b *testing.B) {
	vm := &VM{
		events:      NewEventLog(),
		pausedVCPUs: make(map[int]struct{}),
	}
	vm.mu.Lock()
	vm.ensurePauseCondLocked()
	vm.mu.Unlock()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		vm.mu.Lock()
		vm.state = StatePaused
		vm.mu.Unlock()

		started := make(chan struct{})
		woken := make(chan struct{})
		go func() {
			vm.mu.Lock()
			cond := vm.ensurePauseCondLocked()
			vm.pausedVCPUs[0] = struct{}{}
			close(started)
			for vm.state == StatePaused {
				cond.Wait()
			}
			delete(vm.pausedVCPUs, 0)
			vm.mu.Unlock()
			close(woken)
		}()

		<-started
		// Tiny yield so the goroutine reaches cond.Wait. We can't avoid
		// this in a benchmark — measuring without it would deadlock
		// occasionally on slow runners. The cost is amortized into the
		// timer setup, not the cond wake-up itself.
		time.Sleep(50 * time.Microsecond)

		_ = vm.Resume()
		<-woken
	}
}
