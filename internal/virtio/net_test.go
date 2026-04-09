package virtio

import (
	"encoding/binary"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestNetRxPumpRetriesTransientReadErrors(t *testing.T) {
	prevRead := tapReadFrameFn
	prevDelay := tapTransientRetryDelay
	tapTransientRetryDelay = 0
	t.Cleanup(func() {
		tapReadFrameFn = prevRead
		tapTransientRetryDelay = prevDelay
	})

	mem := make([]byte, 64*1024)
	dev := &NetDevice{tapName: "tap-test"}
	dev.Transport = NewTransport(dev, mem, 0x1000, 5, nil, nil)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	dev.tapFd = r

	rxQ := dev.Queue(0)
	rxQ.Ready = true
	rxQ.Size = 256
	rxQ.DescAddr = 0x1000
	rxQ.DriverAddr = 0x2000
	rxQ.DeviceAddr = 0x3000

	bufAddr := uint64(0x6000)
	writeDesc(mem, rxQ.DescAddr, 0, bufAddr, 32, DescFlagWrite, 0)
	writeAvailEntry(mem, rxQ.DriverAddr, 0, 0)

	packet := make([]byte, netHeaderLen+4)
	copy(packet[netHeaderLen:], []byte{1, 2, 3, 4})

	var calls atomic.Int32
	tapReadFrameFn = func(fd int, buf []byte) (int, error) {
		switch calls.Add(1) {
		case 1:
			return 0, syscall.EAGAIN
		case 2:
			return 0, syscall.ENOBUFS
		case 3:
			copy(buf, packet)
			return len(packet), nil
		default:
			return 0, syscall.EBADF
		}
	}

	done := make(chan struct{})
	go func() {
		dev.rxPump()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("rxPump did not exit after terminal tap error")
	}

	if got := calls.Load(); got < 4 {
		t.Fatalf("tap read calls = %d, want at least 4", got)
	}
	if got := binary.LittleEndian.Uint16(mem[rxQ.DeviceAddr+2:]); got != 1 {
		t.Fatalf("used ring idx = %d, want 1", got)
	}
	if mem[bufAddr+netHeaderLen] != 1 || mem[bufAddr+netHeaderLen+3] != 4 {
		t.Fatalf("RX payload not copied after transient read errors")
	}
}

func TestTapTransientReadErrorClassification(t *testing.T) {
	for _, err := range []error{syscall.EINTR, syscall.EAGAIN, syscall.ENOBUFS} {
		if !isTapTransientReadError(err) {
			t.Fatalf("%v should be classified as transient", err)
		}
	}
	if isTapTransientReadError(syscall.EBADF) {
		t.Fatal("EBADF should not be classified as transient")
	}
}
