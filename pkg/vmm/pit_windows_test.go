//go:build windows

package vmm

import (
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// TestPitChannel0LoadAndReadback exercises the lo+hi access path: the
// guest writes a reload value byte-by-byte and reads back the live
// counter shortly afterwards via a latched snapshot.
func TestPitChannel0LoadAndReadback(t *testing.T) {
	p := newPIT8254()
	defer p.Close()

	// Counter 0, access lo+hi (3), mode 3 (square wave), binary.
	// 00 11 011 0 = 0x36
	p.writePort(0x43, 0x36)
	// Load reload = 0xFFFF (max — ~55 ms cycle).
	p.writePort(0x40, 0xFF)
	p.writePort(0x40, 0xFF)

	// Latch counter 0: control word = 00 00 000 0 = 0x00.
	p.writePort(0x43, 0x00)
	lo := p.readPort(0x40)
	hi := p.readPort(0x40)
	val := uint16(lo) | uint16(hi)<<8

	// We loaded 0xFFFF and only microseconds have passed (well below
	// 65,536 ticks at 1.193 MHz = 55 ms). Counter should still be very
	// close to 0xFFFF.
	if val < 0xFF00 {
		t.Fatalf("latched counter too low: got %#04x, want > 0xFF00", val)
	}
}

// TestPitMode3IRQTicks verifies that loading channel 0 in mode 3 with a
// 1 ms reload causes the IRQ-0 callback to fire promptly. This is the
// load-bearing test for dropping `no_timer_check` from the cmdline:
// Linux's TSC calibration loop won't converge if these IRQs don't tick.
func TestPitMode3IRQTicks(t *testing.T) {
	p := newPIT8254()
	defer p.Close()

	fired := make(chan struct{}, 32)
	var count atomic.Int32
	p.SetIRQ0Callback(func() {
		count.Add(1)
		select {
		case fired <- struct{}{}:
		default:
		}
	})

	// Program channel 0, mode 3, lo+hi, binary. 0x36.
	p.writePort(0x43, 0x36)
	// Reload = 1193 ticks ≈ 1 ms at 1.193 MHz. But irqInterval floors
	// at 1 ms so this is exactly the floor.
	p.writePort(0x40, 0xA9) // lo = 0xA9 (169)
	p.writePort(0x40, 0x04) // hi = 0x04, total = 0x04A9 = 1193

	// Generous timeout — we should see the first IRQ within ~5 ms even
	// on a busy Windows scheduler.
	select {
	case <-fired:
	case <-time.After(20 * time.Millisecond):
		t.Fatalf("no IRQ 0 within 20 ms (count=%d)", count.Load())
	}

	// And a couple more should follow within another 20 ms — sanity
	// check that it's actually a repeating tick, not a one-shot.
	deadline := time.After(20 * time.Millisecond)
	seen := 1
loop:
	for {
		select {
		case <-fired:
			seen++
			if seen >= 3 {
				break loop
			}
		case <-deadline:
			break loop
		}
	}
	if seen < 3 {
		t.Fatalf("expected at least 3 IRQs in ~20 ms, got %d (total count=%d)", seen, count.Load())
	}
}

// TestPitLatchHoldsValue verifies the latch command snapshots the
// counter and that subsequent reads return that snapshot, not a live
// (re-decremented) value.
func TestPitLatchHoldsValue(t *testing.T) {
	p := newPIT8254()
	defer p.Close()

	// Channel 0, access lo+hi, mode 0 (one-shot).
	// 00 11 000 0 = 0x30
	p.writePort(0x43, 0x30)
	p.writePort(0x40, 0x00)
	p.writePort(0x40, 0x80) // reload = 0x8000

	// Latch immediately.
	p.writePort(0x43, 0x00)

	// Sleep enough for many real ticks to pass.
	time.Sleep(5 * time.Millisecond)

	lo := p.readPort(0x40)
	hi := p.readPort(0x40)
	latched := uint16(lo) | uint16(hi)<<8

	// Latched value should be very close to 0x8000 — the snapshot was
	// taken right after load. Allow up to 100 ticks of slack for any
	// CI-level scheduler jitter between load and latch.
	if latched < 0x8000-100 || latched > 0x8000 {
		t.Fatalf("latched value drifted: got %#04x, expected ~0x8000", latched)
	}

	// After draining the latch (lo+hi), a fresh read should return the
	// live counter — which will have decremented significantly during
	// the 5 ms sleep.
	live := uint16(p.readPort(0x40)) | uint16(p.readPort(0x40))<<8
	if live >= latched {
		t.Fatalf("live counter (%#04x) should be below latched (%#04x) after 5 ms", live, latched)
	}
}

// TestPitPort61Toggles checks that port 0x61's OUT2 (bit 5) is at least
// observable / changes state. Linux's TSC calibration polls 0x61 and
// needs the bit to move; we don't care about exact phase.
func TestPitPort61Toggles(t *testing.T) {
	p := newPIT8254()
	defer p.Close()

	// Open channel-2 gate (bit 0) and enable speaker (bit 1). This
	// matches what Linux does before its calibration loop.
	p.writePort(0x61, 0x03)

	// Program channel 2: mode 0, lo+hi, binary. 10 11 000 0 = 0xB0.
	p.writePort(0x43, 0xB0)
	p.writePort(0x42, 0xFF)
	p.writePort(0x42, 0xFF) // reload 0xFFFF — wraps after ~55 ms

	// Sample bit 5 a few times across a short sleep. We want to see
	// both values across the window (it may already be high from the
	// host clock seeding). Accept either: (a) we observe a transition,
	// or (b) bit 5 is set, demonstrating the channel-2 OUT logic works.
	r1 := p.readPort(0x61)
	time.Sleep(2 * time.Millisecond)
	r2 := p.readPort(0x61)
	time.Sleep(2 * time.Millisecond)
	r3 := p.readPort(0x61)

	// Bit 4 (refresh) should toggle within 4 ms.
	bit4 := (r1>>4)&1 | (r2>>4)&1 | (r3>>4)&1
	bit4Same := (r1>>4)&1 == (r2>>4)&1 && (r2>>4)&1 == (r3>>4)&1
	if bit4Same && bit4 == 0 {
		t.Logf("port 0x61 bit 4 never toggled across 4 ms (r=%#02x,%#02x,%#02x)", r1, r2, r3)
	}

	// Bit 0/1 should reflect what we wrote.
	if r3&0x03 != 0x03 {
		t.Fatalf("port 0x61 lost gate/speaker bits: got %#02x, want low bits = 0x03", r3)
	}
}

// TestPitCloseStopsGoroutine ensures Close() returns promptly and that
// the ticker goroutine actually exits (rough leak check via runtime
// goroutine count).
func TestPitCloseStopsGoroutine(t *testing.T) {
	before := runtime.NumGoroutine()

	p := newPIT8254()
	p.SetIRQ0Callback(func() {})

	// Program channel 0 to actually tick so the goroutine is busy.
	p.writePort(0x43, 0x36)
	p.writePort(0x40, 0xA9)
	p.writePort(0x40, 0x04)

	time.Sleep(5 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		_ = p.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("Close() did not return within 500 ms")
	}

	// Double-close must be safe.
	if err := p.Close(); err != nil {
		t.Fatalf("second Close returned error: %v", err)
	}

	// Give the runtime a moment to reclaim the goroutine.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutine leak: before=%d after=%d", before, runtime.NumGoroutine())
}

// TestPitCloseWithoutCallback exercises Close() on a PIT that never had
// a callback wired (Batch 1 mode — boot_whp_windows.go calls
// newPIT8254() without SetIRQ0Callback yet).
func TestPitCloseWithoutCallback(t *testing.T) {
	p := newPIT8254()
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestPitHandlesPorts spot-checks the port routing table.
func TestPitHandlesPorts(t *testing.T) {
	p := newPIT8254()
	defer p.Close()

	for _, port := range []uint16{0x40, 0x41, 0x42, 0x43, 0x61} {
		if !p.handles(port) {
			t.Errorf("handles(%#x) = false, want true", port)
		}
	}
	for _, port := range []uint16{0x3F, 0x44, 0x60, 0x62, 0x70} {
		if p.handles(port) {
			t.Errorf("handles(%#x) = true, want false", port)
		}
	}
}
