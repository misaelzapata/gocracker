package i8042

import "testing"

func TestReadStatusReturnsCurrentStatus(t *testing.T) {
	dev := New(nil)
	got := dev.Read(statusOffset)
	if got&statusKbdEnabled == 0 {
		t.Fatalf("Read(statusOffset) = %#x, expected statusKbdEnabled bit set", got)
	}
}

func TestReadDataEmptyBufferReturnsZero(t *testing.T) {
	dev := New(nil)
	if got := dev.Read(dataOffset); got != 0 {
		t.Fatalf("Read(dataOffset) on empty buffer = %#x, want 0", got)
	}
}

func TestReadUnknownOffsetReturnsZero(t *testing.T) {
	dev := New(nil)
	if got := dev.Read(2); got != 0 {
		t.Fatalf("Read(2) = %#x, want 0", got)
	}
}

func TestReadDataDrainsBuffer(t *testing.T) {
	dev := New(nil)
	dev.Write(dataOffset, 0xED)
	first := dev.Read(dataOffset)
	if first != 0xFA {
		t.Fatalf("first read = %#x, want 0xFA", first)
	}
	if dev.status&statusOutDataAvail != 0 {
		t.Fatal("statusOutDataAvail should be cleared after draining buffer")
	}
	second := dev.Read(dataOffset)
	if second != 0 {
		t.Fatalf("second read = %#x, want 0 (empty buffer)", second)
	}
}

func TestOutputPortRegisterRoundTrip(t *testing.T) {
	dev := New(nil)
	dev.Write(statusOffset, cmdWriteOutp)
	dev.Write(dataOffset, 0xAB)
	dev.Write(statusOffset, cmdReadOutp)
	if got := dev.Read(dataOffset); got != 0xAB {
		t.Fatalf("output port read = %#x, want 0xAB", got)
	}
}

func TestResetWithNilCallback(t *testing.T) {
	dev := New(nil)
	dev.Write(statusOffset, cmdResetCPU)
}

func TestMultipleDataBufferEntries(t *testing.T) {
	dev := New(nil)
	dev.Write(statusOffset, cmdReadCTR)
	if dev.status&statusOutDataAvail == 0 {
		t.Fatal("statusOutDataAvail should be set after cmdReadCTR")
	}
	got := dev.Read(dataOffset)
	expected := uint8(controlPostOK | controlKbdInt)
	if got != expected {
		t.Fatalf("CTR read = %#x, want %#x", got, expected)
	}
}

func TestFlushClearsBufferOnNewCommand(t *testing.T) {
	dev := New(nil)
	dev.Write(dataOffset, 0xED)
	if dev.status&statusOutDataAvail == 0 {
		t.Fatal("expected OutDataAvail after keyboard cmd")
	}
	dev.Write(statusOffset, cmdReadCTR)
	got := dev.Read(dataOffset)
	expected := uint8(controlPostOK | controlKbdInt)
	if got != expected {
		t.Fatalf("after flush got = %#x, want %#x (CTR value)", got, expected)
	}
}

func TestWriteCTRClearsStatusCmdDataAfterDataWrite(t *testing.T) {
	dev := New(nil)
	dev.Write(statusOffset, cmdWriteCTR)
	if dev.status&statusCmdData == 0 {
		t.Fatal("statusCmdData should be set after cmdWriteCTR")
	}
	dev.Write(dataOffset, 0x42)
	if dev.status&statusCmdData != 0 {
		t.Fatal("statusCmdData should be cleared after data write")
	}
}

func TestWriteOutpClearsStatusCmdDataAfterDataWrite(t *testing.T) {
	dev := New(nil)
	dev.Write(statusOffset, cmdWriteOutp)
	if dev.status&statusCmdData == 0 {
		t.Fatal("statusCmdData should be set after cmdWriteOutp")
	}
	dev.Write(dataOffset, 0x99)
	if dev.status&statusCmdData != 0 {
		t.Fatal("statusCmdData should be cleared after data write")
	}
	dev.Write(statusOffset, cmdReadOutp)
	if got := dev.Read(dataOffset); got != 0x99 {
		t.Fatalf("output port = %#x, want 0x99", got)
	}
}

func TestReadCTRAfterWriteCTR(t *testing.T) {
	dev := New(nil)
	dev.Write(statusOffset, cmdWriteCTR)
	dev.Write(dataOffset, 0xFF)
	dev.Write(statusOffset, cmdReadCTR)
	if got := dev.Read(dataOffset); got != 0xFF {
		t.Fatalf("CTR after write = %#x, want 0xFF", got)
	}
}

func TestNewSetsInitialState(t *testing.T) {
	dev := New(nil)
	if dev.status != statusKbdEnabled {
		t.Fatalf("initial status = %#x, want %#x", dev.status, statusKbdEnabled)
	}
	if dev.control != controlPostOK|controlKbdInt {
		t.Fatalf("initial control = %#x, want %#x", dev.control, controlPostOK|controlKbdInt)
	}
	if len(dev.buf) != 0 || cap(dev.buf) != 16 {
		t.Fatalf("initial buf len=%d cap=%d, want len=0 cap=16", len(dev.buf), cap(dev.buf))
	}
}

func TestReadOutpDefaultsToZero(t *testing.T) {
	dev := New(nil)
	dev.Write(statusOffset, cmdReadOutp)
	if got := dev.Read(dataOffset); got != 0 {
		t.Fatalf("initial outp = %#x, want 0", got)
	}
}

func TestBufferGrowsBeyondInitialCap(t *testing.T) {
	dev := New(nil)
	for i := 0; i < 20; i++ {
		dev.push(uint8(i))
	}
	if len(dev.buf) != 20 {
		t.Fatalf("buf len = %d, want 20", len(dev.buf))
	}
	for i := 0; i < 20; i++ {
		got := dev.Read(dataOffset)
		if got != uint8(i) {
			t.Fatalf("buf[%d] = %#x, want %#x", i, got, uint8(i))
		}
	}
}

func TestFlushOnReadOutp(t *testing.T) {
	dev := New(nil)
	dev.Write(dataOffset, 0xED)
	dev.Write(statusOffset, cmdReadOutp)
	got := dev.Read(dataOffset)
	if got != 0 {
		t.Fatalf("outp after flush = %#x, want 0", got)
	}
}

func TestWriteDataWithoutCmdStatus(t *testing.T) {
	dev := New(nil)
	// Writing to data port without prior command should produce ACK
	dev.Write(dataOffset, 0xF3)
	got := dev.Read(dataOffset)
	if got != 0xFA {
		t.Fatalf("ACK = %#x, want 0xFA", got)
	}
}
