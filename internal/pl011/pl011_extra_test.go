package pl011

import (
	"bytes"
	"io"
	"testing"
	"time"
)

func TestPL011ReadEmptyDRReturnsZero(t *testing.T) {
	dev := New(nil, nil, nil)
	if got := dev.Read32(RegDR); got != 0 {
		t.Fatalf("DR empty = %d, want 0", got)
	}
}

func TestPL011ReadUnknownRegister(t *testing.T) {
	dev := New(nil, nil, nil)
	if got := dev.Read32(0xFFF); got != 0 {
		t.Fatalf("unknown reg = %d, want 0", got)
	}
}

func TestPL011FlagRegBusyWhenRxData(t *testing.T) {
	dev := New(nil, nil, nil)
	dev.InjectBytes([]byte("x"))
	fr := dev.Read32(RegFR)
	if fr&FRBusy == 0 {
		t.Fatalf("FR = %#x, expected Busy when rxBuf non-empty", fr)
	}
	if fr&FRRxFE != 0 {
		t.Fatalf("FR = %#x, expected RxFE cleared when rxBuf has data", fr)
	}
}

func TestPL011InjectEmptyBytes(t *testing.T) {
	dev := New(nil, nil, nil)
	dev.InjectBytes(nil)
	if got := dev.Read32(RegFR); got&FRRxFE == 0 {
		t.Fatalf("FR = %#x, expected RxFE set after empty inject", got)
	}
}

func TestPL011WriteNilOut(t *testing.T) {
	dev := New(nil, nil, nil)
	dev.Write32(RegDR, 'X')
	out := dev.OutputBytes()
	if len(out) != 1 || out[0] != 'X' {
		t.Fatalf("output = %q, want X", out)
	}
}

func TestPL011OutputBufferTruncation(t *testing.T) {
	dev := New(nil, nil, nil)
	dev.outBufMax = 4
	for i := 0; i < 10; i++ {
		dev.Write32(RegDR, uint32('A'+i))
	}
	out := dev.OutputBytes()
	if len(out) != 4 {
		t.Fatalf("output buf len = %d, want 4", len(out))
	}
	if string(out) != "GHIJ" {
		t.Fatalf("output = %q, want GHIJ", out)
	}
}

func TestPL011ReadAllRegisters(t *testing.T) {
	dev := New(nil, nil, nil)
	dev.Write32(RegIBRD, 42)
	dev.Write32(RegFBRD, 7)
	dev.Write32(RegLCRH, 0x70)
	dev.Write32(RegCR, 0x301)
	dev.Write32(RegIFLS, 3)
	dev.Write32(RegIMSC, 0x30)

	if got := dev.Read32(RegIBRD); got != 42 {
		t.Fatalf("IBRD = %d", got)
	}
	if got := dev.Read32(RegFBRD); got != 7 {
		t.Fatalf("FBRD = %d", got)
	}
	if got := dev.Read32(RegLCRH); got != 0x70 {
		t.Fatalf("LCRH = %#x", got)
	}
	if got := dev.Read32(RegCR); got != 0x301 {
		t.Fatalf("CR = %#x", got)
	}
	if got := dev.Read32(RegIFLS); got != 3 {
		t.Fatalf("IFLS = %d", got)
	}
	if got := dev.Read32(RegIMSC); got != 0x30 {
		t.Fatalf("IMSC = %#x", got)
	}
}

func TestPL011MIS(t *testing.T) {
	dev := New(nil, nil, nil)
	dev.Write32(RegIMSC, intRx|intTx)
	dev.InjectBytes([]byte("A"))
	mis := dev.Read32(RegMIS)
	if mis&intRx == 0 {
		t.Fatalf("MIS = %#x, expected RX interrupt masked in", mis)
	}
}

func TestPL011ICR(t *testing.T) {
	var irqCalls []bool
	dev := New(nil, nil, func(level bool) { irqCalls = append(irqCalls, level) })
	dev.Write32(RegIMSC, intTx)
	dev.Write32(RegDR, 'A')
	dev.Write32(RegICR, intTx)
	ris := dev.Read32(RegRIS)
	if ris&intTx != 0 {
		t.Fatalf("RIS after ICR = %#x, expected TX cleared", ris)
	}
}

func TestPL011RxPumpReadsFromInput(t *testing.T) {
	input := bytes.NewReader([]byte("hello"))
	dev := New(nil, input, nil)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		dev.mu.Lock()
		n := len(dev.rxBuf)
		dev.mu.Unlock()
		if n >= 5 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	for i, want := range []byte("hello") {
		got := dev.Read32(RegDR)
		if byte(got) != want {
			t.Fatalf("DR[%d] = %c, want %c", i, got, want)
		}
	}
}

func TestPL011RxDrainClearsInterrupt(t *testing.T) {
	var irqCalls []bool
	dev := New(nil, nil, func(level bool) { irqCalls = append(irqCalls, level) })
	dev.Write32(RegIMSC, intRx)
	dev.InjectBytes([]byte("X"))
	dev.Read32(RegDR)
	ris := dev.Read32(RegRIS)
	if ris&intRx != 0 {
		t.Fatalf("RIS after drain = %#x, expected RX cleared", ris)
	}
}

func TestPL011UpdateIRQWithNilFn(t *testing.T) {
	dev := New(nil, nil, nil)
	dev.Write32(RegIMSC, intTx)
	dev.Write32(RegDR, 'A')
}

func TestPL011RestoreStateUpdatesIRQ(t *testing.T) {
	var irqCalls []bool
	dev := New(nil, nil, func(level bool) { irqCalls = append(irqCalls, level) })
	state := State{IMSC: intTx, RIS: intTx}
	dev.RestoreState(state)
	if len(irqCalls) == 0 {
		t.Fatal("expected IRQ callback on RestoreState")
	}
	if !irqCalls[len(irqCalls)-1] {
		t.Fatal("expected IRQ asserted after RestoreState with pending interrupt")
	}
}

func TestPL011RxPumpWithPipe(t *testing.T) {
	pr, pw := io.Pipe()
	dev := New(nil, pr, nil)
	go func() {
		pw.Write([]byte("ab"))
		pw.Close()
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		dev.mu.Lock()
		n := len(dev.rxBuf)
		dev.mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := dev.Read32(RegDR); byte(got) != 'a' {
		t.Fatalf("first = %c, want a", got)
	}
	if got := dev.Read32(RegDR); byte(got) != 'b' {
		t.Fatalf("second = %c, want b", got)
	}
}
