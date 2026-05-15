//go:build windows

package vmm

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestHPETCapabilitiesReg covers the static capabilities register —
// vendor "Intel" (0x8086), numTimers-1 in bits 8..12, clock period in
// femtoseconds in bits 32..63.
func TestHPETCapabilitiesReg(t *testing.T) {
	h := NewHPET(nil)
	low := h.ReadMMIO(hpetMMIOBase, 4)
	hi := h.ReadMMIO(hpetMMIOBase+4, 4)
	cap := uint64(hi)<<32 | uint64(low)

	// Low 16 = 0xA001 (rev) | numTimers-1<<8. For numTimers=3 → 0xA201.
	wantLow16 := uint64(0xA001) | uint64(hpetNumTimers-1)<<8
	if cap&0xFFFF != wantLow16 {
		t.Errorf("HPET capabilities low 16 = %#x; want %#x", cap&0xFFFF, wantLow16)
	}
	if (cap>>16)&0xFFFF != 0x8086 {
		t.Errorf("HPET capabilities vendor = %#x; want 0x8086 (Intel)", (cap>>16)&0xFFFF)
	}
	if got := (cap >> 8) & 0x1F; got != uint64(hpetNumTimers-1) {
		t.Errorf("HPET capabilities numTimers-1 = %d; want %d", got, hpetNumTimers-1)
	}
	if got := cap >> 32; got != hpetClockPeriodFs {
		t.Errorf("HPET clock period = %d fs; want %d", got, hpetClockPeriodFs)
	}
}

// TestHPETCounterAdvancesAfterEnable verifies the main counter starts
// at 0, stays at 0 until the config-enable bit goes high, then advances.
func TestHPETCounterAdvancesAfterEnable(t *testing.T) {
	h := NewHPET(nil)
	defer h.Close()

	if v := h.ReadMMIO(hpetMMIOBase+0xF0, 4); v != 0 {
		t.Errorf("counter before enable = %d; want 0", v)
	}

	// Enable counter.
	h.WriteMMIO(hpetMMIOBase+0x10, 4, hpetCfgEnable)
	time.Sleep(2 * time.Millisecond) // 2 ms = 20 000 HPET ticks

	v := h.ReadMMIO(hpetMMIOBase+0xF0, 4)
	if v < 10_000 || v > 100_000 {
		t.Errorf("counter after 2 ms = %d; expected ~20 000 ticks (10 000..100 000)", v)
	}
}

// TestHPETPeriodicTimerFires arms timer 0 in legacy + periodic mode and
// checks that raiseIRQ(0) fires within a generous wall-clock window.
func TestHPETPeriodicTimerFires(t *testing.T) {
	var hits atomic.Int32
	h := NewHPET(func(irq uint8) {
		if irq == 0 {
			hits.Add(1)
		}
	})
	defer h.Close()

	// Enable counter + legacy replacement (timer 0 → IRQ 0).
	h.WriteMMIO(hpetMMIOBase+0x10, 4, hpetCfgEnable|hpetCfgLegacy)

	// Arm timer 0 as periodic, enable IRQ, period of 1 ms (10 000 ticks).
	cfg := uint32(hpetTimerEnable | hpetTimerPeriodic | hpetTimerSetVal | hpetTimer32Bit)
	h.WriteMMIO(hpetMMIOBase+0x100, 4, cfg)
	h.WriteMMIO(hpetMMIOBase+0x108, 4, 10_000)

	// Wait up to 200 ms for at least 3 ticks. Allows for slow host
	// scheduling on CI runners.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if hits.Load() >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := hits.Load(); got < 3 {
		t.Errorf("HPET timer 0 fired %d times in 200 ms; expected ≥ 3", got)
	}
}

// TestHPETWriteIgnoredOutOfRange confirms that out-of-range writes do
// not crash and don't change observable state.
func TestHPETWriteIgnoredOutOfRange(t *testing.T) {
	h := NewHPET(nil)
	defer h.Close()
	h.WriteMMIO(hpetMMIOBase+0x3FC, 4, 0xDEADBEEF) // last word, no register here
	h.WriteMMIO(hpetMMIOBase+0x500, 4, 0xCAFEBABE) // way past size — HandlesAddr false
	// No assert beyond "doesn't panic".
}
