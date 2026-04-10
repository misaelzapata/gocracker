package pl011

import (
	"bytes"
	"testing"
)

func TestPL011TransmitAndOutputBuffer(t *testing.T) {
	var out bytes.Buffer
	var irqLevels []bool
	dev := New(&out, nil, func(level bool) { irqLevels = append(irqLevels, level) })

	dev.Write32(RegIMSC, intTx)
	dev.Write32(RegDR, 'A')

	if got := out.String(); got != "A" {
		t.Fatalf("output = %q, want %q", got, "A")
	}
	if got := string(dev.OutputBytes()); got != "A" {
		t.Fatalf("buffered output = %q, want %q", got, "A")
	}
	if len(irqLevels) == 0 || !irqLevels[len(irqLevels)-1] {
		t.Fatal("expected TX interrupt assertion")
	}
}

func TestPL011ReceivePath(t *testing.T) {
	dev := New(nil, nil, nil)

	if got := dev.Read32(RegFR); got&FRRxFE == 0 {
		t.Fatalf("FR before RX = %#x, want RXFE set", got)
	}

	dev.InjectBytes([]byte("BC"))
	if got := dev.Read32(RegFR); got&FRRxFE != 0 {
		t.Fatalf("FR after RX = %#x, want RXFE cleared", got)
	}
	if got := dev.Read32(RegDR); got != 'B' {
		t.Fatalf("first DR read = %#x, want %#x", got, uint32('B'))
	}
	if got := dev.Read32(RegDR); got != 'C' {
		t.Fatalf("second DR read = %#x, want %#x", got, uint32('C'))
	}
	if got := dev.Read32(RegFR); got&FRRxFE == 0 {
		t.Fatalf("FR after draining RX = %#x, want RXFE set", got)
	}
}

func TestPL011StateRoundTrip(t *testing.T) {
	dev := New(nil, nil, nil)
	dev.Write32(RegIBRD, 13)
	dev.Write32(RegFBRD, 7)
	dev.Write32(RegLCRH, 0x70)
	dev.Write32(RegCR, CRUARTEN|CRTXE)
	dev.Write32(RegIFLS, 2)
	dev.Write32(RegIMSC, intRx|intTx)

	state := dev.State()

	clone := New(nil, nil, nil)
	clone.RestoreState(state)

	if got := clone.Read32(RegIBRD); got != 13 {
		t.Fatalf("restored IBRD = %d, want 13", got)
	}
	if got := clone.Read32(RegFBRD); got != 7 {
		t.Fatalf("restored FBRD = %d, want 7", got)
	}
	if got := clone.Read32(RegLCRH); got != 0x70 {
		t.Fatalf("restored LCRH = %#x, want %#x", got, uint32(0x70))
	}
	if got := clone.Read32(RegCR); got != CRUARTEN|CRTXE {
		t.Fatalf("restored CR = %#x", got)
	}
	if got := clone.Read32(RegIMSC); got != intRx|intTx {
		t.Fatalf("restored IMSC = %#x", got)
	}
}
