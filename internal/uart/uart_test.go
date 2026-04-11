package uart

import (
	"bytes"
	"testing"
)

func TestUARTWriteTHRTransmitsToOutput(t *testing.T) {
	var out bytes.Buffer
	u := New(&out, nil, nil)

	u.Write(RegTHR, 'A')

	if got := out.String(); got != "A" {
		t.Fatalf("output: got %q, want %q", got, "A")
	}
}

func TestUARTLoopbackKeepsOutputQuietAndFeedsRX(t *testing.T) {
	var out bytes.Buffer
	u := New(&out, nil, nil)

	u.Write(RegMCR, MCRLoopback|MCRDTR|MCRRTS|MCROut2)
	u.Write(RegTHR, 'Z')

	if got := out.String(); got != "" {
		t.Fatalf("output in loopback: got %q, want empty", got)
	}
	if got := u.Read(RegRBR); got != 'Z' {
		t.Fatalf("loopback RX: got %q, want %q", got, 'Z')
	}
	if u.Read(RegLSR)&LSRDataReady != 0 {
		t.Fatalf("expected RX buffer to be drained")
	}
}

func TestUARTLoopbackUpdatesMSRFromMCR(t *testing.T) {
	u := New(ioDiscard{}, nil, nil)

	u.Write(RegMCR, MCRLoopback|MCRDTR|MCRRTS|MCROut1)

	want := uint8(0x10 | 0x20 | 0x40)
	if got := u.Read(RegMSR); got != want {
		t.Fatalf("MSR in loopback: got %#x, want %#x", got, want)
	}

	u.Write(RegMCR, MCRDTR|MCRRTS)
	if got := u.Read(RegMSR); got != 0xB0 {
		t.Fatalf("MSR without loopback: got %#x, want %#x", got, uint8(0xB0))
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }

func TestReadAllRegisters(t *testing.T) {
	u := New(ioDiscard{}, nil, nil)

	tests := []struct {
		name   string
		offset uint8
	}{
		{"RBR", RegRBR},
		{"IER", RegIER},
		{"IIR", RegIIR},
		{"LCR", RegLCR},
		{"MCR", RegMCR},
		{"LSR", RegLSR},
		{"MSR", RegMSR},
		{"SCR", RegSCR},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Just verify no panic on read
			_ = u.Read(tc.offset)
		})
	}
}

func TestWriteAllRegisters(t *testing.T) {
	u := New(ioDiscard{}, nil, nil)

	tests := []struct {
		name   string
		offset uint8
	}{
		{"THR", RegTHR},
		{"IER", RegIER},
		{"FCR", RegFCR},
		{"LCR", RegLCR},
		{"MCR", RegMCR},
		{"LSR", RegLSR}, // read-only, write ignored
		{"MSR", RegMSR}, // read-only, write ignored
		{"SCR", RegSCR},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Just verify no panic on write
			u.Write(tc.offset, 0x55)
		})
	}
}

func TestScratchRegisterRoundtrip(t *testing.T) {
	u := New(ioDiscard{}, nil, nil)
	for _, val := range []uint8{0x00, 0x55, 0xAA, 0xFF} {
		u.Write(RegSCR, val)
		if got := u.Read(RegSCR); got != val {
			t.Errorf("SCR: wrote %#x, read %#x", val, got)
		}
	}
}

func TestLCRRoundtrip(t *testing.T) {
	u := New(ioDiscard{}, nil, nil)
	u.Write(RegLCR, 0x1B) // 8N1 + break
	if got := u.Read(RegLCR); got != 0x1B {
		t.Errorf("LCR: got %#x, want %#x", got, uint8(0x1B))
	}
}

func TestDLABDivisorLatch(t *testing.T) {
	u := New(ioDiscard{}, nil, nil)

	// Enable DLAB
	u.Write(RegLCR, 0x80)

	// Write divisor latch low and high
	u.Write(RegDLL, 0x0C) // offset 0, DLAB=1
	u.Write(RegDLH, 0x00) // offset 1, DLAB=1

	// Read them back
	if got := u.Read(RegDLL); got != 0x0C {
		t.Errorf("DLL: got %#x, want %#x", got, uint8(0x0C))
	}
	if got := u.Read(RegDLH); got != 0x00 {
		t.Errorf("DLH: got %#x, want %#x", got, uint8(0x00))
	}

	// Disable DLAB, verify IER/RBR work again
	u.Write(RegLCR, 0x00)
}

func TestInitialLSR(t *testing.T) {
	u := New(ioDiscard{}, nil, nil)
	lsr := u.Read(RegLSR)
	if lsr&LSRTHREmpty == 0 {
		t.Error("initial LSR should have THR empty bit set")
	}
	if lsr&LSRTransmitEmpty == 0 {
		t.Error("initial LSR should have transmit empty bit set")
	}
	if lsr&LSRDataReady != 0 {
		t.Error("initial LSR should not have data ready bit set")
	}
}

func TestInitialMSR(t *testing.T) {
	u := New(ioDiscard{}, nil, nil)
	msr := u.Read(RegMSR)
	if msr != 0xB0 {
		t.Errorf("initial MSR = %#x, want %#x", msr, uint8(0xB0))
	}
}

func TestIERMasking(t *testing.T) {
	u := New(ioDiscard{}, nil, nil)
	// Write full byte, but only lower 4 bits should stick
	u.Write(RegIER, 0xFF)
	if got := u.Read(RegIER); got != 0x0F {
		t.Errorf("IER: got %#x, want %#x (masked to lower 4 bits)", got, uint8(0x0F))
	}
}

func TestMCRMasking(t *testing.T) {
	u := New(ioDiscard{}, nil, nil)
	u.Write(RegMCR, 0xFF)
	if got := u.Read(RegMCR); got != 0x1F {
		t.Errorf("MCR: got %#x, want %#x (masked to lower 5 bits)", got, uint8(0x1F))
	}
}

func TestIIRFIFOBits(t *testing.T) {
	u := New(ioDiscard{}, nil, nil)
	iir := u.Read(RegIIR)
	// FIFO enabled bits (0xC0) should always be set
	if iir&IIRFIFOEnabled != IIRFIFOEnabled {
		t.Errorf("IIR FIFO bits not set: got %#x", iir)
	}
	// No pending interrupt initially
	if iir&0x3F != IIRNoPending {
		t.Errorf("IIR should show no pending interrupt: got %#x", iir&0x3F)
	}
}

func TestIIRReadClearsTHREmptyInterrupt(t *testing.T) {
	var irqs int
	u := New(ioDiscard{}, nil, func(asserted bool) { irqs++ })

	// Enable THR empty interrupt
	u.Write(RegIER, IERTHREmpty)
	// Write a char to trigger THR-empty interrupt
	u.Write(RegTHR, 'X')

	// Read IIR should show THR empty
	iir := u.Read(RegIIR)
	if iir&0x3F != IIRTHREmpty {
		t.Errorf("IIR = %#x, want THR empty (%#x)", iir&0x3F, IIRTHREmpty)
	}

	// Second read should show no pending (THR empty cleared on read)
	iir2 := u.Read(RegIIR)
	if iir2&0x3F != IIRNoPending {
		t.Errorf("IIR after second read = %#x, want no pending (%#x)", iir2&0x3F, IIRNoPending)
	}
}

func TestLSRWriteIgnored(t *testing.T) {
	u := New(ioDiscard{}, nil, nil)
	before := u.Read(RegLSR)
	u.Write(RegLSR, 0x00) // should be ignored
	after := u.Read(RegLSR)
	if after != before {
		t.Errorf("LSR changed after write: before=%#x, after=%#x", before, after)
	}
}

func TestMSRWriteIgnored(t *testing.T) {
	u := New(ioDiscard{}, nil, nil)
	before := u.Read(RegMSR)
	u.Write(RegMSR, 0x00) // should be ignored
	after := u.Read(RegMSR)
	if after != before {
		t.Errorf("MSR changed after write: before=%#x, after=%#x", before, after)
	}
}

func TestInjectBytesMakesDataReadable(t *testing.T) {
	u := New(ioDiscard{}, nil, nil)

	u.InjectBytes([]byte("Hi"))

	// LSR should have DataReady set
	if u.Read(RegLSR)&LSRDataReady == 0 {
		t.Error("LSR DataReady not set after InjectBytes")
	}

	// Read first byte
	if got := u.Read(RegRBR); got != 'H' {
		t.Errorf("first byte: got %q, want 'H'", got)
	}
	// Read second byte
	if got := u.Read(RegRBR); got != 'i' {
		t.Errorf("second byte: got %q, want 'i'", got)
	}
	// Buffer empty now
	if u.Read(RegLSR)&LSRDataReady != 0 {
		t.Error("LSR DataReady should be clear after draining")
	}
	// Reading from empty buffer returns 0
	if got := u.Read(RegRBR); got != 0 {
		t.Errorf("empty RBR: got %#x, want 0", got)
	}
}

func TestInjectBytesEmpty(t *testing.T) {
	u := New(ioDiscard{}, nil, nil)
	// Should not panic or change state
	u.InjectBytes(nil)
	u.InjectBytes([]byte{})
	if u.Read(RegLSR)&LSRDataReady != 0 {
		t.Error("LSR DataReady should not be set after empty inject")
	}
}

func TestInjectBytesIRQ(t *testing.T) {
	var irqCount int
	u := New(ioDiscard{}, nil, func(asserted bool) {
		if asserted {
			irqCount++
		}
	})
	// Enable RX data available interrupt
	u.Write(RegIER, IERRxDataAvail)

	u.InjectBytes([]byte("A"))
	if irqCount != 1 {
		t.Errorf("IRQ count after first inject = %d, want 1", irqCount)
	}
}

func TestOutputBytesCapture(t *testing.T) {
	u := New(ioDiscard{}, nil, nil)

	u.Write(RegTHR, 'H')
	u.Write(RegTHR, 'e')
	u.Write(RegTHR, 'l')
	u.Write(RegTHR, 'l')
	u.Write(RegTHR, 'o')

	got := u.OutputBytes()
	if string(got) != "Hello" {
		t.Errorf("OutputBytes = %q, want %q", got, "Hello")
	}

	// Calling again returns the same data (it's buffered, not consumed)
	got2 := u.OutputBytes()
	if string(got2) != "Hello" {
		t.Errorf("second OutputBytes = %q, want %q", got2, "Hello")
	}
}

func TestOutputBytesRingBuffer(t *testing.T) {
	u := New(ioDiscard{}, nil, nil)
	// outBufMax is 64 KiB. Write more than that.
	for i := 0; i < 70000; i++ {
		u.Write(RegTHR, 'X')
	}
	out := u.OutputBytes()
	if len(out) > defaultOutputBufSize {
		t.Errorf("output buffer len = %d, should be <= %d", len(out), defaultOutputBufSize)
	}
}

func TestStateRestoreRoundtrip(t *testing.T) {
	u := New(ioDiscard{}, nil, nil)

	// Set various registers
	u.Write(RegSCR, 0x42)
	u.Write(RegLCR, 0x80) // DLAB on
	u.Write(RegDLL, 0x0C)
	u.Write(RegDLH, 0x01)
	u.Write(RegLCR, 0x03) // DLAB off, 8N1
	u.Write(RegIER, IERRxDataAvail|IERTHREmpty)
	u.Write(RegTHR, 'A')  // produce output

	// Inject some RX data
	u.InjectBytes([]byte("test"))

	state := u.State()

	// Create fresh UART and restore
	u2 := New(ioDiscard{}, nil, nil)
	u2.RestoreState(state)

	state2 := u2.State()

	if state2.SCR != 0x42 {
		t.Errorf("restored SCR = %#x, want %#x", state2.SCR, uint8(0x42))
	}
	if state2.DLL != 0x0C {
		t.Errorf("restored DLL = %#x, want %#x", state2.DLL, uint8(0x0C))
	}
	if state2.DLH != 0x01 {
		t.Errorf("restored DLH = %#x, want %#x", state2.DLH, uint8(0x01))
	}
	if state2.LCR != 0x03 {
		t.Errorf("restored LCR = %#x, want %#x", state2.LCR, uint8(0x03))
	}
	if string(state2.RxBuf) != "test" {
		t.Errorf("restored RxBuf = %q, want %q", state2.RxBuf, "test")
	}
	if len(state2.OutBuf) == 0 || state2.OutBuf[0] != 'A' {
		t.Errorf("restored OutBuf unexpected: %q", state2.OutBuf)
	}
}

func TestStateSnapshotIsolation(t *testing.T) {
	u := New(ioDiscard{}, nil, nil)
	u.InjectBytes([]byte("abc"))
	state := u.State()

	// Mutate original UART
	u.Read(RegRBR) // drain a byte

	// State should still have original data
	if string(state.RxBuf) != "abc" {
		t.Errorf("snapshot RxBuf mutated: got %q", state.RxBuf)
	}
}

func TestPorts(t *testing.T) {
	lo, hi := Ports(0x3F8)
	if lo != 0x3F8 {
		t.Errorf("lo = %#x, want %#x", lo, uint16(0x3F8))
	}
	if hi != 0x400 {
		t.Errorf("hi = %#x, want %#x", hi, uint16(0x400))
	}
}

func TestLoopbackMSRReflection(t *testing.T) {
	u := New(ioDiscard{}, nil, nil)

	tests := []struct {
		mcr     uint8
		wantMSR uint8
	}{
		{MCRLoopback, 0x00},
		{MCRLoopback | MCRDTR, 0x20},                       // DSR
		{MCRLoopback | MCRRTS, 0x10},                        // CTS
		{MCRLoopback | MCROut1, 0x40},                       // RI
		{MCRLoopback | MCROut2, 0x80},                       // DCD
		{MCRLoopback | MCRDTR | MCRRTS | MCROut1 | MCROut2, 0xF0},
	}
	for _, tc := range tests {
		u.Write(RegMCR, tc.mcr)
		got := u.Read(RegMSR)
		if got != tc.wantMSR {
			t.Errorf("MCR=%#x: MSR = %#x, want %#x", tc.mcr, got, tc.wantMSR)
		}
	}
}

func TestFCRWriteAccepted(t *testing.T) {
	u := New(ioDiscard{}, nil, nil)
	// FCR write should not panic (it's accepted but mostly ignored)
	u.Write(RegFCR, 0x07) // enable FIFO, clear both FIFOs
	u.Write(RegFCR, 0x00) // disable FIFO
}

func TestReadUnknownOffset(t *testing.T) {
	u := New(ioDiscard{}, nil, nil)
	// Offset >= 8 is out of range, should return 0
	if got := u.Read(0x08); got != 0 {
		t.Errorf("Read(0x08) = %#x, want 0", got)
	}
}

func TestTHRIRQWhenEnabled(t *testing.T) {
	var irqs []bool
	u := New(ioDiscard{}, nil, func(asserted bool) {
		irqs = append(irqs, asserted)
	})

	// Enable THR empty interrupt
	u.Write(RegIER, IERTHREmpty)

	// Write a byte: should trigger THR empty IRQ
	u.Write(RegTHR, 'X')
	if len(irqs) == 0 {
		t.Fatal("expected at least one IRQ after THR write with IER_THR enabled")
	}
}

func TestNoTHRIRQWhenDisabled(t *testing.T) {
	irqCount := 0
	u := New(ioDiscard{}, nil, func(asserted bool) {
		irqCount++
	})

	// IER cleared (default) - no THR interrupt
	u.Write(RegTHR, 'X')
	if irqCount != 0 {
		t.Errorf("got %d IRQs with IER=0, want 0", irqCount)
	}
}
