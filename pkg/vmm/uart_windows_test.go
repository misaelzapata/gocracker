//go:build windows

package vmm

import "testing"

// uartFixture wires a UART16550 to in-memory IRQ + TX capture so tests
// can assert exactly what the device produced.
type uartFixture struct {
	u       *UART16550
	tx      []byte
	irqHits int
}

func newUARTFixture() *uartFixture {
	f := &uartFixture{}
	f.u = NewUART16550(
		func() { f.irqHits++ },
		func(b byte) { f.tx = append(f.tx, b) },
	)
	return f
}

// TestUARTTransmitPath drives the THR write path — guest writes a byte
// to 0x3F8 (DLAB=0) and the device hands it to OnTx.
func TestUARTTransmitPath(t *testing.T) {
	f := newUARTFixture()

	f.u.WritePort(0x3F8, 'H')
	f.u.WritePort(0x3F8, 'i')

	if got := string(f.tx); got != "Hi" {
		t.Fatalf("OnTx captured %q; want %q", got, "Hi")
	}

	// LSR.THRE + LSR.TEMT should be set so the kernel never blocks
	// polling for TX-empty.
	lsr := f.u.ReadPort(0x3FD)
	if lsr&uartLSRTHRE == 0 || lsr&uartLSRTEMT == 0 {
		t.Fatalf("LSR after TX = %#x; expected THRE+TEMT bits set", lsr)
	}

	// No IRQ should fire when IER is zero — THRI is gated by IER bit 1.
	if f.irqHits != 0 {
		t.Fatalf("RaiseIRQ called %d times with IER=0; expected 0", f.irqHits)
	}
}

// TestUARTLSRReady asserts the always-on TX-empty bits are present
// even before any write, so the kernel's first probe sees a writable
// UART.
func TestUARTLSRReady(t *testing.T) {
	f := newUARTFixture()
	lsr := f.u.ReadPort(0x3FD)
	if lsr&uartLSRTHRE == 0 || lsr&uartLSRTEMT == 0 {
		t.Fatalf("initial LSR = %#x; expected THRE|TEMT", lsr)
	}
	if lsr&uartLSRDR != 0 {
		t.Fatalf("initial LSR = %#x; expected DR clear", lsr)
	}
}

// TestUARTRxIRQ pushes a byte from the host side and confirms it
// arrives in RBR, LSR.DR goes high, IRQ4 fires exactly once, and the
// RBR read clears DR + acknowledges the IRQ.
func TestUARTRxIRQ(t *testing.T) {
	f := newUARTFixture()

	// Enable RDI (bit 0). DLAB must be 0 to hit the IER register.
	f.u.WritePort(0x3FB, 0x00)
	f.u.WritePort(0x3F9, uartIERRdi)
	preHits := f.irqHits

	f.u.PushRX('A')
	if got := f.irqHits - preHits; got != 1 {
		t.Fatalf("PushRX: IRQ raised %d times; want exactly 1", got)
	}

	lsr := f.u.ReadPort(0x3FD)
	if lsr&uartLSRDR == 0 {
		t.Fatalf("LSR after PushRX = %#x; expected DR bit set", lsr)
	}

	// A second PushRX with RBR still pending shouldn't raise again —
	// the IRQ is edge-triggered on empty→non-empty.
	f.u.PushRX('B')
	if got := f.irqHits - preHits; got != 1 {
		t.Fatalf("second PushRX with RBR pending raised IRQ %d times; want still 1", got)
	}

	if v := f.u.ReadPort(0x3F8); v != 'B' {
		// Second push overwrites; that's the documented overrun policy.
		t.Fatalf("RBR read = %#x; want 'B' (0x42) — second PushRX overwrites", v)
	}
	if lsr := f.u.ReadPort(0x3FD); lsr&uartLSRDR != 0 {
		t.Fatalf("LSR after RBR read = %#x; DR should be clear", lsr)
	}
}

// TestUARTRxMaskedByIER confirms that an RDI-disabled IER (bit 0 = 0)
// leaves IRQ4 dormant when the host pushes RX bytes.
func TestUARTRxMaskedByIER(t *testing.T) {
	f := newUARTFixture()
	// IER stays at zero (default). Push a byte.
	f.u.PushRX('Z')
	if f.irqHits != 0 {
		t.Fatalf("RaiseIRQ called %d times with IER=0; expected 0", f.irqHits)
	}
	// LSR.DR should still show data ready — masking only affects the
	// IRQ line, not the polling path.
	lsr := f.u.ReadPort(0x3FD)
	if lsr&uartLSRDR == 0 {
		t.Fatalf("LSR = %#x; expected DR bit even when IRQ masked", lsr)
	}
	if v := f.u.ReadPort(0x3F8); v != 'Z' {
		t.Fatalf("RBR = %#x; want 'Z' (0x5A)", v)
	}
}

// TestUARTDLABDivisorRoundtrip flips DLAB on, writes the divisor low /
// high pair via 0x3F8/0x3F9, flips DLAB off, and confirms the data
// ports go back to THR/RBR semantics. Mirrors the Linux 8250 driver's
// baud-rate setup.
func TestUARTDLABDivisorRoundtrip(t *testing.T) {
	f := newUARTFixture()

	// Set DLAB so 0x3F8/0x3F9 expose DLL/DLM.
	f.u.WritePort(0x3FB, uartLCRDLAB)
	f.u.WritePort(0x3F8, 0x01) // DLL = 0x01
	f.u.WritePort(0x3F9, 0x00) // DLM = 0x00

	if v := f.u.ReadPort(0x3F8); v != 0x01 {
		t.Errorf("DLL readback = %#x; want 0x01", v)
	}
	if v := f.u.ReadPort(0x3F9); v != 0x00 {
		t.Errorf("DLM readback = %#x; want 0x00", v)
	}

	// Writing to 0x3F8 while DLAB is set should NOT have called OnTx.
	if len(f.tx) != 0 {
		t.Errorf("OnTx invoked %d times during DLAB write; expected 0", len(f.tx))
	}

	// Clear DLAB; now 0x3F8 should hit THR / OnTx again.
	f.u.WritePort(0x3FB, 0x00)
	f.u.WritePort(0x3F8, 'x')
	if string(f.tx) != "x" {
		t.Errorf("post-DLAB TX = %q; want %q", string(f.tx), "x")
	}

	// And IER should be available at 0x3F9 (DLAB cleared).
	f.u.WritePort(0x3F9, uartIERRdi)
	// The IER read goes through 0x3F9 with DLAB=0.
	if v := f.u.ReadPort(0x3F9); v != uartIERRdi {
		t.Errorf("IER readback after DLAB clear = %#x; want %#x", v, uartIERRdi)
	}
}

// TestUARTMCRLoopback exercises the auto-detect loopback path: the
// 8250 driver flips MCR bit 4 then writes to THR expecting the byte to
// show up in RBR without escaping to the host.
func TestUARTMCRLoopback(t *testing.T) {
	f := newUARTFixture()

	f.u.WritePort(0x3FC, uartMCRLoop) // MCR = 0x10
	f.u.WritePort(0x3F8, 0x42)        // TX 'B'

	// OnTx must NOT have been called — loopback keeps the byte
	// internal.
	if len(f.tx) != 0 {
		t.Fatalf("OnTx called in loopback: %q", string(f.tx))
	}

	lsr := f.u.ReadPort(0x3FD)
	if lsr&uartLSRDR == 0 {
		t.Fatalf("LSR after loopback TX = %#x; expected DR set", lsr)
	}
	if v := f.u.ReadPort(0x3F8); v != 0x42 {
		t.Fatalf("RBR after loopback TX = %#x; want 0x42", v)
	}
}

// TestUARTSCR exercises the scratch register — the 8250 driver writes
// a known pattern and reads it back to detect a real UART.
func TestUARTSCR(t *testing.T) {
	f := newUARTFixture()
	f.u.WritePort(0x3FF, 0xA5)
	if v := f.u.ReadPort(0x3FF); v != 0xA5 {
		t.Fatalf("SCR readback = %#x; want 0xA5", v)
	}
}

// TestUARTHandles asserts the port window matches the 16550A surface.
func TestUARTHandles(t *testing.T) {
	f := newUARTFixture()
	for p := uint16(0x3F8); p <= 0x3FF; p++ {
		if !f.u.Handles(p) {
			t.Errorf("Handles(%#x) = false; want true", p)
		}
	}
	if f.u.Handles(0x3F7) || f.u.Handles(0x400) {
		t.Errorf("Handles wrongly claimed an off-range port")
	}
}
