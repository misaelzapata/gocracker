//go:build windows

package vmm

import (
	"sync"
	"time"
)

// HPET (High Precision Event Timer) emulation at MMIO 0xFED00000.
// Provides a 10 MHz 64-bit free-running counter plus 3 timers. Linux's
// hpet_clocksource_register / hpet_enable uses this to derive a TSC
// frequency without the PIT-based calibration loop that doesn't
// converge against our software PIT — once HPET is exposed via the
// ACPI HPET table, Linux skips PIT TSC calibration and the
// `tsc_early_khz` cmdline workaround can be dropped.
//
// Port of node-vmm/native/whp/devices/hpet.cc, simplified:
//   * 3 timers (the spec minimum Linux exercises).
//   * Legacy mode (CFG bit 1) routes timer 0 → IRQ 0 (PIT pin) and
//     timer 1 → IRQ 8 (RTC pin).
//   * FSB/MSI not exposed; Linux always goes through PIC/IOAPIC.

const (
	hpetMMIOBase uint64 = 0xFED00000
	hpetMMIOSize uint64 = 0x400

	hpetClockPeriodNs uint64 = 100                   // 10 MHz
	hpetClockPeriodFs uint64 = hpetClockPeriodNs * 1_000_000

	hpetNumTimers      = 3
	hpetTimer0IRQ      = 0
	hpetTimer1IRQ      = 8
	hpetIntCapMask     = 1 << hpetTimer0IRQ // both legacy lines exposed via IRQ 0/8 caps

	hpetCfgEnable = 0x001
	hpetCfgLegacy = 0x002
	hpetCfgWriteMask = 0x003

	hpetTimerTypeLevel    = 0x002
	hpetTimerEnable       = 0x004
	hpetTimerPeriodic     = 0x008
	hpetTimerPeriodicCap  = 0x010
	hpetTimerSizeCap      = 0x020
	hpetTimerSetVal       = 0x040
	hpetTimer32Bit        = 0x100
	hpetTimerIntRouteMask = 0x3E00
	hpetTimerCfgWriteMask = 0x7F4E
	hpetTimerIntRouteShift = 9
	hpetTimerIntRouteCapShift = 32
)

// hpetCapabilities encodes vendor "Intel" (0x8086) + numTimers - 1 in
// bits 8..12 + 64-bit support (bit 13) + clock period in femtoseconds
// in bits 32..63.
var hpetCapabilities uint64 = 0x8086A001 |
	(uint64(hpetNumTimers-1) << 8) |
	(hpetClockPeriodFs << 32)

type hpetTimer struct {
	config uint64
	cmp    uint64 // 32-bit view (what the guest sees)
	cmp64  uint64 // 64-bit view used by the scheduler
	period uint64
	armed  bool
}

// HPET is the device. Construct with NewHPET; mount via boot_whp_windows.go's
// MMIO dispatch table. raiseIRQ is the upcall fired when a timer expires.
type HPET struct {
	mu                sync.Mutex
	timers            [hpetNumTimers]hpetTimer
	assertedIRQ       [hpetNumTimers]uint8 // last IRQ pin asserted per timer
	assertedIRQValid  [hpetNumTimers]bool
	config            uint64
	isr               uint64
	counterBase       uint64
	counterStartedAt  time.Time
	lastCounter       uint64 // monotonic floor — every read ≥ this+1
	raiseIRQ          func(uint8)
	stop              chan struct{}
	wakeup            chan struct{}
	tickerRunning     bool
}

// NewHPET returns a fresh HPET with the counter stopped. The internal
// scheduler goroutine starts on the first config-enable write.
func NewHPET(raiseIRQ func(uint8)) *HPET {
	h := &HPET{
		raiseIRQ: raiseIRQ,
		stop:     make(chan struct{}),
		wakeup:   make(chan struct{}, 1),
	}
	for i := range h.timers {
		h.timers[i].cmp = 0xFFFFFFFFFFFFFFFF
		h.timers[i].cmp64 = 0xFFFFFFFFFFFFFFFF
		h.timers[i].config = hpetTimerPeriodicCap | hpetTimerSizeCap |
			(uint64(hpetIntCapMask) << hpetTimerIntRouteCapShift)
	}
	return h
}

// HandlesAddr reports whether the address falls in our MMIO window.
func (h *HPET) HandlesAddr(addr uint64) bool {
	return addr >= hpetMMIOBase && addr < hpetMMIOBase+hpetMMIOSize
}

// MmioBase / MmioEnd mirror VirtioBlk's shape for dispatch parity.
func (h *HPET) MmioBase() uint64 { return hpetMMIOBase }
func (h *HPET) MmioEnd() uint64  { return hpetMMIOBase + hpetMMIOSize }

// Close stops the internal ticker. Idempotent.
func (h *HPET) Close() error {
	h.mu.Lock()
	running := h.tickerRunning
	h.mu.Unlock()
	if running {
		select {
		case <-h.stop:
		default:
			close(h.stop)
		}
	}
	return nil
}

// ReadMMIO services a guest read; returns the requested u32 slice of
// the 64-bit register at `addr`. Length 1/2/4 supported; length 8 not
// supported (split into two 4-byte reads, which Linux always does).
func (h *HPET) ReadMMIO(addr uint64, length uint32) uint32 {
	if length == 0 || length > 8 {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	off := addr - hpetMMIOBase
	shift := uint32((off & 4) * 8)
	value := h.readReg(off &^ 4)
	return uint32(value >> shift)
}

// WriteMMIO services a guest write. Maps to the corresponding 64-bit
// register, applying only the requested bits via shift+mask.
func (h *HPET) WriteMMIO(addr uint64, length uint32, value uint32) {
	if length == 0 || length > 8 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	off := addr - hpetMMIOBase
	shift := uint32((off & 4) * 8)
	bits := uint32(length * 8)
	if bits > 64-shift {
		bits = 64 - shift
	}
	h.writeReg(off&^4, shift, bits, uint64(value))
	// Wake the scheduler goroutine since timer state may have changed.
	if h.tickerRunning {
		select {
		case h.wakeup <- struct{}{}:
		default:
		}
	}
}

func (h *HPET) readReg(off uint64) uint64 {
	if off <= 0xFF {
		switch off {
		case 0x000:
			return hpetCapabilities
		case 0x010:
			return h.config
		case 0x020:
			return h.isr
		case 0x0F0:
			return h.counterLocked()
		default:
			return 0
		}
	}
	if off < 0x100 {
		return 0
	}
	timerID := int((off - 0x100) / 0x20)
	if timerID >= len(h.timers) {
		return 0
	}
	t := &h.timers[timerID]
	switch off & 0x18 {
	case 0x00:
		return t.config
	case 0x08:
		return t.cmp
	default:
		return 0
	}
}

func (h *HPET) writeReg(off uint64, shift, bits uint32, value uint64) {
	if off <= 0xFF {
		switch off {
		case 0x010:
			h.writeConfig(shift, bits, value)
		case 0x020:
			clear := depositBits(0, shift, bits, value)
			cleared := h.isr & clear
			h.isr &^= clear
			h.deassertClearedIRQs(cleared)
		case 0x0F0:
			if h.config&hpetCfgEnable == 0 {
				h.counterBase = depositBits(h.counterBase, shift, bits, value)
			}
		}
		return
	}
	if off < 0x100 {
		return
	}
	timerID := int((off - 0x100) / 0x20)
	if timerID >= len(h.timers) {
		return
	}
	switch off & 0x18 {
	case 0x00:
		h.writeTimerConfig(timerID, shift, bits, value)
	case 0x08:
		h.writeTimerCmp(timerID, shift, bits, value)
	default:
		// FSB/MSI routing intentionally unsupported.
	}
}

func (h *HPET) writeConfig(shift, bits uint32, value uint64) {
	old := h.config
	next := depositBits(old, shift, bits, value)
	next = (next & hpetCfgWriteMask) | (old &^ hpetCfgWriteMask)
	wasEnabled := old&hpetCfgEnable != 0
	willEnable := next&hpetCfgEnable != 0
	if wasEnabled && !willEnable {
		h.counterBase = h.counterLocked()
	}
	h.config = next
	if !wasEnabled && willEnable {
		h.counterStartedAt = time.Now()
		if !h.tickerRunning {
			h.tickerRunning = true
			go h.run()
		}
	}
	if willEnable {
		for i := range h.timers {
			h.armTimerLocked(i)
		}
	} else {
		for i := range h.timers {
			h.timers[i].armed = false
		}
		cleared := h.isr
		h.isr = 0
		h.deassertClearedIRQs(cleared)
	}
}

func (h *HPET) writeTimerConfig(id int, shift, bits uint32, value uint64) {
	t := &h.timers[id]
	old := t.config
	next := depositBits(old, shift, bits, value)
	next = (next & hpetTimerCfgWriteMask) | (old &^ hpetTimerCfgWriteMask)
	t.config = next
	if t.config&hpetTimer32Bit != 0 {
		t.cmp = uint64(uint32(t.cmp))
		t.period = uint64(uint32(t.period))
	}
	h.armTimerLocked(id)
}

func (h *HPET) writeTimerCmp(id int, shift, bits uint32, value uint64) {
	t := &h.timers[id]
	if t.config&hpetTimer32Bit != 0 {
		if shift != 0 {
			return
		}
		bits = 64
		value = uint64(uint32(value))
	}
	if t.config&hpetTimerPeriodic == 0 || t.config&hpetTimerSetVal != 0 {
		t.cmp = depositBits(t.cmp, shift, bits, value)
	}
	if t.config&hpetTimerPeriodic != 0 {
		t.period = depositBits(t.period, shift, bits, value)
	}
	t.config &^= hpetTimerSetVal
	h.armTimerLocked(id)
}

func (h *HPET) armTimerLocked(id int) {
	t := &h.timers[id]
	t.armed = false
	if h.config&hpetCfgEnable == 0 || t.config&hpetTimerEnable == 0 {
		return
	}
	now := h.counterLocked()
	if t.config&hpetTimer32Bit != 0 {
		t.cmp64 = (now &^ 0xFFFFFFFF) | uint64(uint32(t.cmp))
		if int64(t.cmp64-now) < 0 {
			t.cmp64 += 0x100000000
		}
	} else {
		t.cmp64 = t.cmp
	}
	t.armed = true
}

func (h *HPET) counterLocked() uint64 {
	if h.config&hpetCfgEnable == 0 {
		return h.counterBase
	}
	elapsed := time.Since(h.counterStartedAt)
	var val uint64
	if elapsed <= 0 {
		val = h.counterBase
	} else {
		val = h.counterBase + uint64(elapsed.Nanoseconds())/hpetClockPeriodNs
	}
	// Linux's hpet_clocksource_register reads the counter twice with a
	// udelay(200) between them and disables HPET if the value didn't
	// move. Our host time resolution is fine for that — but if the
	// guest's udelay returns instantly (TSC unstable during early
	// boot), both reads can land in the same 100 ns window. Force the
	// counter to strictly increase between reads so the kernel's
	// "counter not counting" check always passes.
	if val <= h.lastCounter {
		val = h.lastCounter + 1
	}
	h.lastCounter = val
	return val
}

func (h *HPET) routeForTimer(id int) uint8 {
	t := &h.timers[id]
	if h.config&hpetCfgLegacy != 0 && id <= 1 {
		if id == 0 {
			return hpetTimer0IRQ
		}
		return hpetTimer1IRQ
	}
	pin := uint8((t.config & hpetTimerIntRouteMask) >> hpetTimerIntRouteShift)
	return pin
}

func (h *HPET) deassertClearedIRQs(cleared uint64) {
	if h.raiseIRQ == nil {
		return
	}
	for i := 0; i < len(h.timers); i++ {
		if cleared&(uint64(1)<<i) == 0 {
			continue
		}
		if !h.assertedIRQValid[i] {
			continue
		}
		// Level-triggered de-assert: re-raise with low (the boot session
		// treats every raiseIRQ as an edge, so de-assert just clears
		// our tracking — no second WHvRequestInterrupt fires).
		h.assertedIRQValid[i] = false
	}
}

// run is the timer scheduler goroutine. Wakes near the next earliest
// armed-timer expiry, then fires raiseIRQ for each timer that crossed
// its comparator. Periodic timers re-arm; one-shot timers disarm.
func (h *HPET) run() {
	for {
		h.mu.Lock()
		if h.config&hpetCfgEnable == 0 {
			h.mu.Unlock()
			select {
			case <-h.stop:
				return
			case <-h.wakeup:
				continue
			case <-time.After(50 * time.Millisecond):
				continue
			}
		}

		now := h.counterLocked()
		var earliest uint64 = ^uint64(0)
		for i := range h.timers {
			t := &h.timers[i]
			if !t.armed || t.config&hpetTimerEnable == 0 {
				continue
			}
			if t.cmp64 < earliest {
				earliest = t.cmp64
			}
		}

		var sleepFor time.Duration
		if earliest == ^uint64(0) {
			sleepFor = 50 * time.Millisecond
		} else if earliest > now {
			sleepFor = time.Duration((earliest-now)*hpetClockPeriodNs) * time.Nanosecond
			if sleepFor > 100*time.Millisecond {
				sleepFor = 100 * time.Millisecond
			}
		} else {
			sleepFor = 0
		}
		h.mu.Unlock()

		if sleepFor > 0 {
			select {
			case <-h.stop:
				return
			case <-h.wakeup:
				continue
			case <-time.After(sleepFor):
			}
		}

		h.mu.Lock()
		if h.config&hpetCfgEnable == 0 {
			h.mu.Unlock()
			continue
		}
		now = h.counterLocked()
		for i := range h.timers {
			t := &h.timers[i]
			if !t.armed || t.config&hpetTimerEnable == 0 {
				continue
			}
			if int64(t.cmp64-now) > 0 {
				continue
			}
			irq := h.routeForTimer(i)
			if t.config&hpetTimerTypeLevel != 0 {
				h.isr |= uint64(1) << i
				h.assertedIRQ[i] = irq
				h.assertedIRQValid[i] = true
			}
			raiser := h.raiseIRQ
			h.mu.Unlock()
			if raiser != nil {
				raiser(irq)
			}
			h.mu.Lock()
			if t.config&hpetTimerPeriodic != 0 && t.period != 0 {
				for int64(t.cmp64-now) <= 0 {
					t.cmp64 += t.period
				}
				if t.config&hpetTimer32Bit != 0 {
					t.cmp = uint64(uint32(t.cmp64))
				} else {
					t.cmp = t.cmp64
				}
			} else {
				t.armed = false
			}
		}
		h.mu.Unlock()
	}
}

// depositBits returns `prev` with `bits` starting at `shift` replaced
// by the corresponding bits of `value`. Mirrors qemu's deposit32/64.
func depositBits(prev uint64, shift, bits uint32, value uint64) uint64 {
	mask := uint64(0)
	if bits >= 64 {
		mask = ^uint64(0)
	} else {
		mask = (uint64(1) << bits) - 1
	}
	return (prev &^ (mask << shift)) | ((value & mask) << shift)
}
