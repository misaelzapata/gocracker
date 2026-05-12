//go:build windows

package vmm

import (
	"sync"
	"time"
)

// pit8254 is a minimal emulation of the 8254 Programmable Interval
// Timer. Linux uses it for TSC calibration on bare-hardware paths that
// don't expose Hyper-V paravirt clocks. We don't deliver IRQ 0 (Phase
// 2g+ wires that into the 8259 PIC); we ONLY satisfy reads/writes so
// the kernel's calibration loop completes.
//
// PIT operates at a fixed 1.193182 MHz. Each counter decrements from
// a reload value to zero; the kernel observes that descent and uses
// it as a time reference.
//
// What we implement:
//   - port 0x43: control word write — selects channel, access mode
//   - port 0x40-0x42: counter data ports — reload write, current read
//   - port 0x61: NMI/speaker control — bit 5 = channel 2 OUT readback
//                 (1 when counter has expired)
//
// What we do NOT implement (Phase 2g+):
//   - Real IRQ 0 generation on counter 0 expiry (needs 8259 PIC wiring)
//   - BCD counter mode (we treat all writes as binary)
//   - Special modes 3/4/5 (we treat everything as one-shot, mode 0)
const pitFrequencyHz = 1193182

type pit8254 struct {
	mu       sync.Mutex
	counters [3]pitCounter
	port61   byte // NMI/speaker control register
}

type pitCounter struct {
	reload     uint16
	loadedAt   time.Time // when reload was written; counter starts ticking down from here
	accessMode uint8     // 1=lobyte only, 2=hibyte only, 3=lobyte then hibyte
	writeState uint8     // 0 = expecting lobyte (or low half), 1 = expecting hibyte
	readState  uint8     // 0 = next read returns lobyte, 1 = next read returns hibyte
	loByte     uint8     // buffered low byte of pending write
	gateOn     bool      // channel 2 gate (controlled via port 0x61 bit 0)
}

func newPIT8254() *pit8254 { return &pit8254{} }

// currentCount returns the counter's value right now, accounting for
// elapsed real time since reload. Counter 2 only ticks while gate is on.
func (p *pit8254) currentCount(ch int) uint16 {
	c := &p.counters[ch]
	if ch == 2 && !c.gateOn {
		return c.reload // gate closed: counter frozen at reload
	}
	if c.loadedAt.IsZero() {
		return c.reload
	}
	elapsed := time.Since(c.loadedAt)
	ticks := uint64(elapsed.Nanoseconds()) * pitFrequencyHz / 1_000_000_000
	if ticks >= uint64(c.reload) {
		return 0
	}
	return c.reload - uint16(ticks)
}

// readPort returns the byte the guest reads from port `port` (0x40-0x43
// or 0x61). Returns 0 for unrecognised ports.
func (p *pit8254) readPort(port uint16) byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch port {
	case 0x40, 0x41, 0x42:
		ch := int(port - 0x40)
		c := &p.counters[ch]
		cur := p.currentCount(ch)
		switch c.accessMode {
		case 1: // lobyte only
			return byte(cur & 0xFF)
		case 2: // hibyte only
			return byte((cur >> 8) & 0xFF)
		case 3: // lobyte then hibyte
			if c.readState == 0 {
				c.readState = 1
				return byte(cur & 0xFF)
			}
			c.readState = 0
			return byte((cur >> 8) & 0xFF)
		default:
			return byte(cur & 0xFF)
		}
	case 0x61:
		// NMI/speaker control. Bit 5 = channel 2 OUT readback: high
		// when the counter has reached zero. The kernel polls this
		// during calibration.
		ret := p.port61 & 0x0F
		if p.currentCount(2) == 0 {
			ret |= 1 << 5
		}
		return ret
	}
	return 0
}

// writePort handles a guest write to port `port` (0x40-0x43 or 0x61).
func (p *pit8254) writePort(port uint16, value byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch port {
	case 0x43:
		// Control word format:
		//   bits 6-7: counter select (0=ch0, 1=ch1, 2=ch2, 3=readback)
		//   bits 4-5: access mode (1=lo, 2=hi, 3=lo then hi)
		//   bits 1-3: operating mode (ignored)
		//   bit 0:    BCD (ignored — we always do binary)
		ch := int(value >> 6)
		if ch >= 3 {
			return // readback or invalid — ignore for v1
		}
		c := &p.counters[ch]
		c.accessMode = (value >> 4) & 0x3
		c.writeState = 0
		c.readState = 0
	case 0x40, 0x41, 0x42:
		ch := int(port - 0x40)
		c := &p.counters[ch]
		switch c.accessMode {
		case 1: // lobyte only — reload = value (high byte 0)
			c.reload = uint16(value)
			c.loadedAt = time.Now()
		case 2: // hibyte only — reload = value << 8
			c.reload = uint16(value) << 8
			c.loadedAt = time.Now()
		case 3: // lobyte first, then hibyte
			if c.writeState == 0 {
				c.loByte = value
				c.writeState = 1
			} else {
				c.reload = uint16(c.loByte) | (uint16(value) << 8)
				c.writeState = 0
				c.loadedAt = time.Now()
			}
		}
	case 0x61:
		// Bit 0 = channel 2 gate. Toggling it reloads the counter
		// (matches real-hardware behaviour).
		newGate := value&0x1 != 0
		if newGate && !p.counters[2].gateOn {
			p.counters[2].loadedAt = time.Now()
		}
		p.counters[2].gateOn = newGate
		// Keep low 4 bits, ignore others (NMI mask etc).
		p.port61 = value & 0x0F
	}
}

// handles reports whether this PIT emulator handles the given I/O port.
func (p *pit8254) handles(port uint16) bool {
	return port == 0x61 || (port >= 0x40 && port <= 0x43)
}
