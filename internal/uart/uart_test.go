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
