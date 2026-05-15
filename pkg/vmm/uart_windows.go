//go:build windows

package vmm

import "sync"

// UART16550 is a minimal 16550A-style UART covering the registers
// Linux's serial8250 driver reads and writes during boot and runtime:
// THR/RBR, IER, IIR/FCR, LCR, MCR, LSR, MSR, SCR, plus the DLAB-gated
// divisor latch (DLL/DLM). Ported from node-vmm's
// native/whp/devices/uart.cc — only the subset gocracker's WHP boot
// path needs to keep the guest kernel happy.
//
// What this DOES implement:
//   - All 8 register offsets at base 0x3F8 (0x3F8..0x3FF)
//   - DLAB switch (LCR bit 7) gating THR/RBR <-> DLL and IER <-> DLM
//   - LSR with bit 5 (THRE) and bit 6 (TEMT) permanently set so Linux
//     never blocks polling for TX-empty
//   - LSR bit 0 (DR) set whenever a byte is ready in RBR
//   - IER bit 0 (RDI) — raises IRQ4 when the guest pushes a byte in
//   - IER bit 1 (THRI) — raises IRQ4 once after each TX (THR drains
//     immediately since we hand the byte to the host synchronously)
//   - MCR bit 4 (loopback) — TX bytes feed straight into RBR. Needed
//     by the 8250 driver's auto-detect probe.
//   - SCR read/write (the kernel uses it as a presence probe)
//
// What it does NOT implement (lifted from node-vmm but not needed for
// the gocracker boot smoke):
//   - FCR FIFO mode + trigger levels (we behave like a 1-byte FIFO)
//   - IIR pending-interrupt source identification beyond the basic
//     "is something pending" signal
//   - MSR delta-status reporting / IER bit 3 (MSI)
//   - Break-condition / receive-line-status interrupts
//   - DLL/DLM are stored but never affect timing
//
// Concurrency: every public method takes mu. RaiseIRQ and OnTx are
// invoked with mu *released* so the callbacks may freely re-enter the
// UART (e.g. PushRX from a host RX-pump goroutine) without deadlock.
type UART16550 struct {
	mu sync.Mutex

	// 16550A register file. Names mirror the Linux 8250 driver.
	ier byte // 0x3F9 — interrupt enable
	iir byte // 0x3FA read — interrupt identification (bit 0 = no IRQ pending)
	fcr byte // 0x3FA write — FIFO control (stored; not honoured)
	lcr byte // 0x3FB — line control (bit 7 = DLAB)
	mcr byte // 0x3FC — modem control (bit 4 = loopback)
	lsr byte // 0x3FD — line status (bit 5 THRE, bit 6 TEMT always set)
	msr byte // 0x3FE — modem status
	scr byte // 0x3FF — scratch

	dll byte // divisor latch low  (visible at 0x3F8 when DLAB=1)
	dlm byte // divisor latch high (visible at 0x3F9 when DLAB=1)

	rbr byte // 0x3F8 read — receive buffer

	// rxReady mirrors LSR bit 0. Kept as a separate field so PushRX
	// can decide whether to raise IRQ4 without having to re-derive it.
	rxReady bool

	// IRQ pending bits, latched until cleared by the appropriate read.
	rdiPending  bool // RX data available
	thriPending bool // TX holding register empty

	// RaiseIRQ delivers IRQ4 to the host's interrupt controller. May
	// be nil during tests that don't care about IRQ wiring.
	RaiseIRQ func()
	// OnTx is invoked once per byte the guest writes to THR (and not in
	// loopback mode). May be nil if the caller doesn't capture output.
	OnTx func(byte)
}

// 16550A bit masks. Public-looking names kept lowercase since they're
// implementation details — the only thing leaving the package is the
// register-port surface.
const (
	uartIERRdi  = 0x01 // received data available IRQ enable
	uartIERThri = 0x02 // transmitter holding register empty IRQ enable

	uartLCRDLAB = 0x80 // divisor latch access

	uartMCRLoop = 0x10 // loopback

	uartLSRDR   = 0x01 // data ready (RBR has a byte)
	uartLSRTHRE = 0x20 // transmit holding register empty
	uartLSRTEMT = 0x40 // transmit empty (FIFO + shift register)

	uartIIRNone = 0x01 // bit 0 = "no interrupt pending"
	uartIIRRDI  = 0x04 // received data available
	uartIIRTHRI = 0x02 // transmitter holding register empty
)

// NewUART16550 constructs a UART16550 wired to the given IRQ-raise and
// TX-byte callbacks. Either callback may be nil.
//
// Initial register state matches a freshly-powered 16550A: TX FIFO
// drained (LSR bits 5/6 set), no IRQs pending, MCR=0x08 (OUT2 set so
// the 8250 driver doesn't think the interrupt line is hard-disabled),
// MSR=0xB0 (DCD+DSR+CTS asserted — the Linux driver treats this as a
// normally-connected modem). Divisor latch defaults to 0x000C =
// 115200 baud / 9600 = a real DIV value Linux will accept.
func NewUART16550(raiseIRQ func(), onTx func(byte)) *UART16550 {
	return &UART16550{
		iir:      uartIIRNone,
		mcr:      0x08,
		msr:      0xB0,
		lsr:      uartLSRTHRE | uartLSRTEMT,
		dll:      0x0C,
		RaiseIRQ: raiseIRQ,
		OnTx:     onTx,
	}
}

// Handles reports whether the given I/O port belongs to COM1 (0x3F8 ..
// 0x3FF).
func (u *UART16550) Handles(port uint16) bool {
	return port >= 0x3F8 && port <= 0x3FF
}

// ReadPort returns the byte the guest sees at the given COM1 port.
func (u *UART16550) ReadPort(port uint16) byte {
	u.mu.Lock()
	defer u.mu.Unlock()

	dlab := u.lcr&uartLCRDLAB != 0

	switch port {
	case 0x3F8:
		if dlab {
			return u.dll
		}
		// RBR read: hand the byte back and clear DR. The 8250 driver
		// uses this read to acknowledge the RX data interrupt, so we
		// also clear the RDI pending latch and update IIR.
		v := u.rbr
		u.rbr = 0
		u.rxReady = false
		u.lsr &^= uartLSRDR
		u.rdiPending = false
		u.refreshIIRLocked()
		return v

	case 0x3F9:
		if dlab {
			return u.dlm
		}
		return u.ier

	case 0x3FA:
		// IIR read: the 8250 driver uses this to identify the IRQ
		// source. Returning IIR with the bottom 4 bits = THRI also
		// clears the THRI pending latch (per 16550A spec).
		v := u.iir
		if v&0x0F == uartIIRTHRI {
			u.thriPending = false
			u.refreshIIRLocked()
		}
		return v

	case 0x3FB:
		return u.lcr

	case 0x3FC:
		return u.mcr

	case 0x3FD:
		// LSR always reports TX-empty so the kernel never blocks
		// polling for transmit ready. RX-ready follows the rxReady
		// flag.
		v := u.lsr | uartLSRTHRE | uartLSRTEMT
		if u.rxReady {
			v |= uartLSRDR
		} else {
			v &^= uartLSRDR
		}
		return v

	case 0x3FE:
		return u.msr

	case 0x3FF:
		return u.scr
	}
	return 0xFF
}

// WritePort handles a guest write to one of the COM1 ports.
func (u *UART16550) WritePort(port uint16, value byte) {
	u.mu.Lock()

	dlab := u.lcr&uartLCRDLAB != 0

	switch port {
	case 0x3F8:
		if dlab {
			u.dll = value
			u.mu.Unlock()
			return
		}
		// THR write: hand off to TX path. In loopback mode the byte
		// feeds straight back into RBR. Otherwise we hand it to OnTx.
		u.transmitLocked(value)

	case 0x3F9:
		if dlab {
			u.dlm = value
			u.mu.Unlock()
			return
		}
		oldIER := u.ier
		u.ier = value & 0x0F
		// IER bit 1 (THRI) being newly set immediately latches a THRI
		// interrupt — the 8250 spec says THRE has been true since
		// reset, so the edge counts. Linux's serial8250_startup relies
		// on this to receive its very first IRQ.
		if u.ier&uartIERThri != 0 && oldIER&uartIERThri == 0 {
			u.thriPending = true
		}
		u.refreshIIRLocked()
		raise := u.maybeRaiseLocked()
		u.mu.Unlock()
		if raise && u.RaiseIRQ != nil {
			u.RaiseIRQ()
		}
		return

	case 0x3FA:
		// FCR write. We store the value for diagnostic visibility but
		// don't model FIFO trigger levels.
		u.fcr = value
		u.mu.Unlock()
		return

	case 0x3FB:
		u.lcr = value
		u.mu.Unlock()
		return

	case 0x3FC:
		u.mcr = value & 0x1F
		u.mu.Unlock()
		return

	case 0x3FF:
		u.scr = value
		u.mu.Unlock()
		return

	default:
		u.mu.Unlock()
		return
	}

	// transmitLocked may have raised RDI (in loopback) or THRI. Decide
	// once with the lock dropped, since RaiseIRQ may call back into
	// the IO controller.
	raise := u.maybeRaiseLocked()
	u.mu.Unlock()
	if raise && u.RaiseIRQ != nil {
		u.RaiseIRQ()
	}
}

// PushRX injects a byte from the host into the UART RX path. The byte
// lands in RBR, LSR.DR is set, and IRQ4 is raised if the guest has
// enabled the RDI interrupt source.
//
// If a byte is already pending in RBR it is overwritten (LSR.OE would
// normally be set on a real 16550A; we don't track overrun here since
// node-vmm's tracking isn't load-bearing for the Linux 8250 driver in
// the gocracker boot smoke).
func (u *UART16550) PushRX(b byte) {
	u.mu.Lock()

	// In loopback mode the host RX path is disconnected; mirror the
	// node-vmm behavior and drop the byte.
	if u.mcr&uartMCRLoop != 0 {
		u.mu.Unlock()
		return
	}

	u.rbr = b
	u.rxReady = true
	u.lsr |= uartLSRDR

	// RDI is level-triggered: every byte that arrives while IER.RDI is
	// set latches an IRQ4 raise. The earlier edge-triggered logic
	// (raise only on empty→non-empty) dropped IRQs whenever two host
	// bytes arrived before the guest read RBR, which is the steady
	// state when the host sends a multi-byte command faster than the
	// kernel's 8250 read path drains the FIFO.
	raise := false
	if u.ier&uartIERRdi != 0 {
		u.rdiPending = true
		raise = true
	}
	u.refreshIIRLocked()
	u.mu.Unlock()

	if raise && u.RaiseIRQ != nil {
		u.RaiseIRQ()
	}
}

// transmitLocked drains a single TX byte. Caller holds u.mu.
//
// In loopback mode the byte is funnelled into the RX path (LSR.DR set,
// RDI pending if enabled). Otherwise we invoke OnTx. Either way THRE
// stays asserted (we drain instantly) and a THRI interrupt is latched
// if the guest has enabled it.
func (u *UART16550) transmitLocked(value byte) {
	if u.mcr&uartMCRLoop != 0 {
		wasEmpty := !u.rxReady
		u.rbr = value
		u.rxReady = true
		u.lsr |= uartLSRDR
		if wasEmpty && u.ier&uartIERRdi != 0 {
			u.rdiPending = true
		}
	} else if u.OnTx != nil {
		// Drop the lock for the host callback so we don't deadlock if
		// OnTx feeds back through a different device.
		cb := u.OnTx
		u.mu.Unlock()
		cb(value)
		u.mu.Lock()
	}

	// THR drained instantly — re-assert THRE/TEMT and latch THRI if
	// the guest has enabled the THR-empty interrupt.
	u.lsr |= uartLSRTHRE | uartLSRTEMT
	if u.ier&uartIERThri != 0 {
		u.thriPending = true
	}
	u.refreshIIRLocked()
}

// refreshIIRLocked recomputes the IIR register based on the pending
// latches. Priority follows the 16550A spec — RDI outranks THRI.
// Caller holds u.mu.
func (u *UART16550) refreshIIRLocked() {
	switch {
	case u.rdiPending && u.ier&uartIERRdi != 0:
		u.iir = uartIIRRDI
	case u.thriPending && u.ier&uartIERThri != 0:
		u.iir = uartIIRTHRI
	default:
		u.iir = uartIIRNone
	}
}

// maybeRaiseLocked reports whether the caller should invoke RaiseIRQ.
// Returns true exactly when there is an unmasked interrupt source
// pending. Caller holds u.mu.
func (u *UART16550) maybeRaiseLocked() bool {
	if u.rdiPending && u.ier&uartIERRdi != 0 {
		return true
	}
	if u.thriPending && u.ier&uartIERThri != 0 {
		return true
	}
	return false
}
