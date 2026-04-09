package i8042

import "testing"

func TestResetCommandTriggersCallback(t *testing.T) {
	called := false
	dev := New(func() { called = true })

	dev.Write(statusOffset, cmdResetCPU)

	if !called {
		t.Fatal("reset callback was not triggered")
	}
}

func TestControlRegisterRoundTrip(t *testing.T) {
	dev := New(nil)

	dev.Write(statusOffset, cmdWriteCTR)
	dev.Write(dataOffset, 0x52)
	dev.Write(statusOffset, cmdReadCTR)

	if got := dev.Read(dataOffset); got != 0x52 {
		t.Fatalf("Read(dataOffset) = %#x, want %#x", got, 0x52)
	}
}

func TestKeyboardCommandAck(t *testing.T) {
	dev := New(nil)

	dev.Write(dataOffset, 0xED)

	if got := dev.Read(dataOffset); got != 0xFA {
		t.Fatalf("Read(dataOffset) = %#x, want %#x", got, 0xFA)
	}
}
