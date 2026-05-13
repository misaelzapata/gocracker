//go:build windows

package vmm

import (
	"sync"
	"sync/atomic"
	"time"
)

// pit8254 is an emulation of the Intel 8254 Programmable Interval Timer.
// Ported from node-vmm/native/whp/devices/pit.cc (~182 LoC C++) with
// adaptations for Go and an internal IRQ-0 ticker goroutine that lets
// Linux complete its real TSC calibration loop (pit_hpet_ptimer_calibrate_cpu).
//
// Crystal frequency: 1,193,182 Hz (~1.193 MHz).
//
// Ports:
//   - 0x40: channel 0 data (timer IRQ 0)
//   - 0x41: channel 1 data (legacy DRAM refresh — present for compatibility)
//   - 0x42: channel 2 data (PC speaker)
//   - 0x43: command/mode register
//   - 0x61: NMI/speaker control. Bit 0 = gate2, bit 1 = speaker enable,
//     bit 4 = refresh toggle, bit 5 = OUT2 readback.
//
// Channel modes implemented:
//   - mode 0 (interrupt-on-terminal-count): counter decrements once.
//   - mode 2 (rate generator): counter wraps continuously.
//   - mode 3 (square wave): counter halves the input frequency, toggling
//     OUT. Linux's pit_hpet_ptimer_calibrate_cpu loads channel 0 in this
//     mode (LATCH = PIT_TICK_RATE / HZ) and clocks IRQ 0 on each wrap.
//
// Channel 0 OUT → IRQ 0. When the channel-0 counter wraps, the ticker
// goroutine invokes raiseIRQ0 (set via SetIRQ0Callback). Channel 2 OUT
// is reflected in port 0x61 bit 5 so the kernel's speaker-gated TSC
// calibration loop sees motion.
//
// The ticker only runs when a callback has been registered, so the
// zero-value/no-callback case remains side-effect free.
const pitFrequencyHz = 1193182

type pit8254 struct {
	mu       sync.Mutex
	channels [3]pitChannel
	port61   byte // NMI/speaker control register

	// IRQ-0 callback (channel 0 OUT line). nil until SetIRQ0Callback.
	raiseIRQ0 func()

	// Lifecycle for the internal ticker goroutine.
	tickerStarted atomic.Bool
	closed        atomic.Bool
	stop          chan struct{}
	done          chan struct{}
}

type pitChannel struct {
	reload      uint16
	latch       uint16
	latchValid  bool
	access      uint8 // 1=lobyte only, 2=hibyte only, 3=lo+hi
	mode        uint8 // 0..5
	writePhase  uint8 // 0 = expecting lobyte, 1 = expecting hibyte
	readPhase   uint8 // 0 = next read = lobyte, 1 = next read = hibyte
	start       time.Time
	nextIRQ     time.Time
	irqEnabled  bool
	gateOn      bool // channel 2 gate (port 0x61 bit 0)
}

// newPIT8254 constructs a PIT with no IRQ callback wired. The Batch 2
// integrator (boot_whp_windows.go) calls SetIRQ0Callback to attach
// s.raiseIRQ(0) after construction; until then no goroutine runs and
// the device is purely passive.
func newPIT8254() *pit8254 {
	p := &pit8254{
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	now := time.Now()
	for i := range p.channels {
		p.channels[i].start = now
		p.channels[i].access = 3
		p.channels[i].mode = 3
	}
	p.channels[0].irqEnabled = true
	return p
}

// SetIRQ0Callback wires the channel-0 OUT line to the given function and
// starts the internal ticker goroutine (idempotent). Pass nil to leave
// the PIT silent. Called once by the boot session integrator with
// func() { session.raiseIRQ(0) }.
func (p *pit8254) SetIRQ0Callback(fn func()) {
	p.mu.Lock()
	p.raiseIRQ0 = fn
	if fn != nil {
		now := time.Now()
		p.channels[0].nextIRQ = now.Add(p.irqInterval(&p.channels[0]))
	}
	p.mu.Unlock()
	if fn != nil && p.tickerStarted.CompareAndSwap(false, true) {
		go p.run()
	}
}

// Close stops the ticker goroutine cleanly. Safe to call even if the
// ticker was never started, and safe to call multiple times.
func (p *pit8254) Close() error {
	if !p.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(p.stop)
	if p.tickerStarted.Load() {
		<-p.done
	}
	return nil
}

// run is the channel-0 ticker. Wakes near each scheduled IRQ moment,
// catches up missed ticks, then sleeps until the next due time.
func (p *pit8254) run() {
	defer close(p.done)
	for {
		p.mu.Lock()
		fn := p.raiseIRQ0
		ch := &p.channels[0]
		interval := p.irqInterval(ch)
		next := ch.nextIRQ
		enabled := ch.irqEnabled
		p.mu.Unlock()

		if fn == nil || !enabled {
			// Sleep a coarse amount; SetIRQ0Callback re-arms us.
			select {
			case <-p.stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			continue
		}

		now := time.Now()
		wait := time.Until(next)
		if wait > 0 {
			select {
			case <-p.stop:
				return
			case <-time.After(wait):
			}
		}

		// Coalesce missed ticks (host scheduler hiccup, large interval).
		fire := false
		now = time.Now()
		p.mu.Lock()
		ch = &p.channels[0]
		if ch.irqEnabled {
			for !now.Before(ch.nextIRQ) {
				ch.nextIRQ = ch.nextIRQ.Add(interval)
				fire = true
			}
		}
		fn = p.raiseIRQ0
		p.mu.Unlock()

		if fire && fn != nil {
			fn()
		}
	}
}

// divisor returns the effective divisor for the channel. Reload 0 means
// 65,536 ticks per cycle (full 16-bit wrap).
func divisor(c *pitChannel) uint32 {
	if c.reload == 0 {
		return 65536
	}
	return uint32(c.reload)
}

// irqInterval returns the wall-clock period between IRQ 0 edges for the
// given channel-0 configuration. Floored at 1 ms to keep the host from
// melting if the kernel ever loads an absurdly small reload.
func (p *pit8254) irqInterval(c *pitChannel) time.Duration {
	div := uint64(divisor(c))
	nanos := (div * 1_000_000_000) / pitFrequencyHz
	if nanos < 1_000_000 {
		nanos = 1_000_000
	}
	return time.Duration(nanos)
}

// currentCount returns the channel counter value right now, accounting
// for elapsed wall time since reload. Mirrors QEMU's pit_get_count: for
// modes 2/3 the counter wraps; for modes 0/1/4/5 it saturates at zero
// (well, wraps mod 65536 per QEMU but that's effectively saturation
// after one cycle).
func (p *pit8254) currentCount(c *pitChannel) uint16 {
	if c == &p.channels[2] && !c.gateOn {
		return c.reload
	}
	if c.start.IsZero() {
		return c.reload
	}
	elapsed := time.Since(c.start)
	if elapsed <= 0 {
		return c.reload
	}
	ticks := uint64(elapsed.Nanoseconds()) * pitFrequencyHz / 1_000_000_000
	count := uint32(divisor(c))
	var counter uint32
	if c.mode == 2 || c.mode == 3 {
		counter = count - uint32(ticks%uint64(count))
	} else {
		// Mode 0/1/4/5: saturating decrement, wrap mod 65536.
		if ticks >= uint64(count) {
			counter = 0
		} else {
			counter = count - uint32(ticks)
		}
		counter &= 0xFFFF
	}
	return uint16(counter & 0xFFFF)
}

// handles reports whether this PIT emulator handles the given I/O port.
func (p *pit8254) handles(port uint16) bool {
	return port == 0x61 || (port >= 0x40 && port <= 0x43)
}

// readPort returns the byte the guest reads from port `port`.
func (p *pit8254) readPort(port uint16) byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch port {
	case 0x40, 0x41, 0x42:
		c := &p.channels[port-0x40]
		return p.readChannel(c)
	case 0x61:
		// Bits 0/1/4 follow what the kernel writes. Bit 5 reflects
		// channel-2 OUT. We approximate OUT2 by toggling at the channel-2
		// counter rate so TSC calibration sees motion regardless of the
		// loaded value. (Linux only checks that the bit changes; the
		// exact phase is not load-bearing.)
		ret := p.port61 & 0x17 // keep bits 0,1,2,4 of last write
		if p.channel2OutHigh() {
			ret |= 1 << 5
		}
		// Refresh toggle (bit 4): wired to system refresh, but our
		// implementation just XORs against a sub-15µs tick so the
		// kernel's BIOS-era refresh test sees a moving target. Linux
		// doesn't depend on it post-boot, but cheap to provide.
		if (time.Now().UnixNano()/15_000)&1 == 1 {
			ret ^= 1 << 4
		}
		return ret
	}
	return 0
}

// channel2OutHigh returns true when channel-2 OUT is asserted (port 0x61
// bit 5 readback). Used by TSC calibration.
func (p *pit8254) channel2OutHigh() bool {
	c := &p.channels[2]
	if !c.gateOn {
		return false
	}
	if c.start.IsZero() {
		return false
	}
	elapsed := time.Since(c.start)
	if elapsed <= 0 {
		return false
	}
	ticks := uint64(elapsed.Nanoseconds()) * pitFrequencyHz / 1_000_000_000
	return ticks >= uint64(divisor(c))
}

// writePort handles a guest write to port `port`.
func (p *pit8254) writePort(port uint16, value byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch port {
	case 0x43:
		p.writeCommand(value)
	case 0x40, 0x41, 0x42:
		idx := int(port - 0x40)
		p.writeChannel(idx, value)
	case 0x61:
		// Bit 0 = channel-2 gate. Toggling on reloads the counter.
		newGate := value&0x1 != 0
		if newGate && !p.channels[2].gateOn {
			p.channels[2].start = time.Now()
		}
		p.channels[2].gateOn = newGate
		// Preserve bits 0,1,2,4 of last write for readback.
		p.port61 = value & 0x17
	}
}

// writeCommand processes a write to port 0x43.
// Format: bits 7-6 channel, 5-4 access, 3-1 mode, 0 BCD.
// Access 0 = latch counter (no further fields meaningful).
func (p *pit8254) writeCommand(value byte) {
	if (value & 0xC0) == 0xC0 {
		// Read-back command — not used by Linux boot path.
		return
	}
	idx := int((value >> 6) & 0x03)
	if idx >= len(p.channels) {
		return
	}
	c := &p.channels[idx]
	access := (value >> 4) & 0x03
	if access == 0 {
		// Latch counter snapshot.
		c.latch = p.currentCount(c)
		c.latchValid = true
		c.readPhase = 0
		return
	}
	c.access = access
	c.mode = (value >> 1) & 0x07
	if c.mode > 5 {
		c.mode -= 4
	}
	c.writePhase = 0
	c.readPhase = 0
	c.latchValid = false
}

// writeChannel processes a data write to ports 0x40-0x42.
func (p *pit8254) writeChannel(idx int, value byte) {
	c := &p.channels[idx]
	switch c.access {
	case 1: // lobyte only
		c.reload = (c.reload & 0xFF00) | uint16(value)
		p.resetChannelTimer(idx)
	case 2: // hibyte only
		c.reload = (c.reload & 0x00FF) | (uint16(value) << 8)
		p.resetChannelTimer(idx)
	default: // 3: lo then hi
		if c.writePhase == 0 {
			c.reload = (c.reload & 0xFF00) | uint16(value)
			c.writePhase = 1
			return
		}
		c.reload = (c.reload & 0x00FF) | (uint16(value) << 8)
		c.writePhase = 0
		p.resetChannelTimer(idx)
	}
}

// resetChannelTimer is invoked when a channel finishes loading its
// reload value. For channel 0, it re-arms the IRQ schedule.
func (p *pit8254) resetChannelTimer(idx int) {
	now := time.Now()
	c := &p.channels[idx]
	c.start = now
	if idx == 0 {
		c.irqEnabled = true
		c.nextIRQ = now.Add(p.irqInterval(c))
	}
}

// readChannel returns the next byte of the channel's counter, respecting
// latch state and access mode. Caller holds p.mu.
func (p *pit8254) readChannel(c *pitChannel) byte {
	var value uint16
	if c.latchValid {
		value = c.latch
	} else {
		value = p.currentCount(c)
	}
	switch c.access {
	case 1:
		c.latchValid = false
		return byte(value & 0xFF)
	case 2:
		c.latchValid = false
		return byte((value >> 8) & 0xFF)
	default: // 3: lo then hi
		if c.readPhase == 0 {
			c.readPhase = 1
			return byte(value & 0xFF)
		}
		c.readPhase = 0
		c.latchValid = false
		return byte((value >> 8) & 0xFF)
	}
}
