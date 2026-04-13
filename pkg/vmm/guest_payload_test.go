package vmm

import (
	"bytes"
	"testing"
)

func TestCopyGuestPayloadCopiesBytesVerbatim(t *testing.T) {
	mem := make([]byte, 64)
	payload := []byte("guest-payload")

	if err := copyGuestPayload(mem, 0x40000000, 0x40000010, payload); err != nil {
		t.Fatalf("copyGuestPayload() error = %v", err)
	}
	if !bytes.Equal(mem[0x10:0x10+len(payload)], payload) {
		t.Fatalf("guest payload mismatch: got %q want %q", mem[0x10:0x10+len(payload)], payload)
	}
}

func TestCopyGuestPayloadRejectsOutOfBounds(t *testing.T) {
	mem := make([]byte, 8)
	payload := []byte("too-large")

	if err := copyGuestPayload(mem, 0x40000000, 0x40000004, payload); err == nil {
		t.Fatal("copyGuestPayload() error = nil, want bounds rejection")
	}
}
