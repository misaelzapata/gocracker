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
	dev.tapRawFd = int(r.Fd())

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

func TestNetDeviceConfigBytes(t *testing.T) {
	mem := make([]byte, 64*1024)
	dev := &NetDevice{tapName: "tap-test"}
	dev.cfg.MAC = [6]byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}
	dev.cfg.Status = 0
	dev.Transport = NewTransport(dev, mem, 0x1000, 5, nil, nil)

	cfg := dev.ConfigBytes()
	if len(cfg) != 8 {
		t.Fatalf("ConfigBytes len = %d, want 8", len(cfg))
	}
	// Check MAC bytes
	if cfg[0] != 0xAA || cfg[1] != 0xBB || cfg[5] != 0xFF {
		t.Fatalf("MAC bytes mismatch: %x", cfg[:6])
	}
	// Status should be 0 (link down)
	status := binary.LittleEndian.Uint16(cfg[6:])
	if status != 0 {
		t.Fatalf("status = %d, want 0", status)
	}
}

func TestNetDeviceActivateLink(t *testing.T) {
	mem := make([]byte, 64*1024)
	dev := &NetDevice{tapName: "tap-test"}
	dev.Transport = NewTransport(dev, mem, 0x1000, 5, nil, nil)

	if dev.cfg.Status != 0 {
		t.Fatalf("initial status = %d, want 0", dev.cfg.Status)
	}
	dev.ActivateLink()
	if dev.cfg.Status != 1 {
		t.Fatalf("status after ActivateLink = %d, want 1", dev.cfg.Status)
	}
}

func TestNetDeviceID(t *testing.T) {
	mem := make([]byte, 64*1024)
	dev := &NetDevice{tapName: "tap-test"}
	dev.Transport = NewTransport(dev, mem, 0x1000, 5, nil, nil)

	if dev.DeviceID() != DeviceIDNet {
		t.Fatalf("DeviceID = %d, want %d", dev.DeviceID(), DeviceIDNet)
	}
}

func TestNetDeviceFeatures(t *testing.T) {
	mem := make([]byte, 64*1024)
	dev := &NetDevice{tapName: "tap-test"}
	dev.Transport = NewTransport(dev, mem, 0x1000, 5, nil, nil)

	features := dev.DeviceFeatures()
	if features&NetFeatureMac == 0 {
		t.Fatal("missing NetFeatureMac")
	}
	if features&NetFeatureMrgRxBuf == 0 {
		t.Fatal("missing NetFeatureMrgRxBuf")
	}
	if features&NetFeatureStatus == 0 {
		t.Fatal("missing NetFeatureStatus")
	}
}

func TestNetDeviceHandleQueue_TXQueue(t *testing.T) {
	prevRead := tapReadFrameFn
	tapReadFrameFn = func(fd int, buf []byte) (int, error) {
		return 0, syscall.EBADF
	}
	t.Cleanup(func() { tapReadFrameFn = prevRead })

	mem := make([]byte, 64*1024)
	dev := &NetDevice{tapName: "tap-test"}
	dev.Transport = NewTransport(dev, mem, 0x1000, 5, nil, nil)
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	dev.tapFd = r
	dev.tapRawFd = int(r.Fd())

	// HandleQueue with idx=0 (RX) should be a no-op
	dev.HandleQueue(0, dev.Queue(0))

	// HandleQueue with idx=1 (TX) should try to transmit
	txQ := dev.Queue(1)
	txQ.Ready = true
	txQ.Size = 256
	txQ.DescAddr = 0x1000
	txQ.DriverAddr = 0x2000
	txQ.DeviceAddr = 0x3000
	dev.HandleQueue(1, txQ)
}

func TestNetDeviceSetRateLimiter(t *testing.T) {
	mem := make([]byte, 64*1024)
	dev := &NetDevice{tapName: "tap-test"}
	dev.Transport = NewTransport(dev, mem, 0x1000, 5, nil, nil)

	rl := NewRateLimiter(RateLimiterConfig{Bandwidth: TokenBucket{Size: 1000, RefillTime: 1000}})
	dev.SetRateLimiter(rl)
	if dev.rl != rl {
		t.Fatal("rate limiter not set")
	}
}

func TestNetDeviceClose_NilFd(t *testing.T) {
	mem := make([]byte, 64*1024)
	dev := &NetDevice{tapName: ""}
	dev.Transport = NewTransport(dev, mem, 0x1000, 5, nil, nil)

	// Close with nil tapFd and empty tapName should succeed
	if err := dev.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
}

func TestIsTapShutdownError(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{syscall.EBADF, true},
		{syscall.EIO, true},
		{syscall.EINVAL, true},
		{syscall.ENODEV, true},
		{syscall.EFAULT, true},
		{syscall.EAGAIN, false},
		{syscall.EINTR, false},
		{nil, false},
	}
	for _, tt := range tests {
		got := isTapShutdownError(tt.err)
		if got != tt.want {
			t.Errorf("isTapShutdownError(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

func TestWriteTapFrame_Success(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	pkt := []byte{0x01, 0x02, 0x03, 0x04}
	if err := writeTapFrame(int(w.Fd()), pkt); err != nil {
		t.Fatalf("writeTapFrame: %v", err)
	}

	buf := make([]byte, 16)
	n, _ := r.Read(buf)
	if n != 4 || buf[0] != 1 || buf[3] != 4 {
		t.Fatalf("read back %d bytes: %x", n, buf[:n])
	}
}

func TestWriteTapFrame_Empty(t *testing.T) {
	if err := writeTapFrame(0, nil); err != nil {
		t.Fatalf("writeTapFrame(nil): %v", err)
	}
}

func TestNetDeviceTransmit_EmptyQueue(t *testing.T) {
	prevRead := tapReadFrameFn
	tapReadFrameFn = func(fd int, buf []byte) (int, error) {
		return 0, syscall.EBADF
	}
	t.Cleanup(func() { tapReadFrameFn = prevRead })

	mem := make([]byte, 64*1024)
	dev := &NetDevice{tapName: "tap-test"}
	dev.Transport = NewTransport(dev, mem, 0x1000, 5, nil, nil)
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	dev.tapFd = r
	dev.tapRawFd = int(r.Fd())

	txQ := dev.Queue(1)
	txQ.Ready = true
	txQ.Size = 256
	txQ.DescAddr = 0x1000
	txQ.DriverAddr = 0x2000
	txQ.DeviceAddr = 0x3000

	// Transmit on empty queue should be fine
	dev.transmit(txQ)
}

func TestNetDeviceTransmit_WithPacket(t *testing.T) {
	prevRead := tapReadFrameFn
	tapReadFrameFn = func(fd int, buf []byte) (int, error) {
		return 0, syscall.EBADF
	}
	t.Cleanup(func() { tapReadFrameFn = prevRead })

	mem := make([]byte, 64*1024)
	dev := &NetDevice{tapName: "tap-test"}
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	dev.tapFd = w
	dev.tapRawFd = int(w.Fd())
	dev.Transport = NewTransport(dev, mem, 0x1000, 5, nil, nil)

	txQ := dev.Queue(1)
	txQ.Ready = true
	txQ.Size = 256
	txQ.DescAddr = 0x1000
	txQ.DriverAddr = 0x2000
	txQ.DeviceAddr = 0x3000

	// Write a packet into guest memory and set up descriptor
	pktAddr := uint64(0x5000)
	pkt := make([]byte, 20)
	copy(pkt, []byte{0xDE, 0xAD, 0xBE, 0xEF})
	copy(mem[pktAddr:], pkt)

	writeDesc(mem, txQ.DescAddr, 0, pktAddr, uint32(len(pkt)), 0, 0)
	writeAvailEntry(mem, txQ.DriverAddr, 0, 0)

	dev.transmit(txQ)

	// Verify used ring was updated
	usedIdx := binary.LittleEndian.Uint16(mem[txQ.DeviceAddr+2:])
	if usedIdx != 1 {
		t.Fatalf("used idx = %d, want 1", usedIdx)
	}
}

func TestDeliverRXPacket_ShortPacket(t *testing.T) {
	mem := make([]byte, 64*1024)
	dev := &NetDevice{tapName: "tap-test"}
	dev.Transport = NewTransport(dev, mem, 0x1000, 5, nil, nil)

	rxQ := dev.Queue(0)
	rxQ.Ready = true

	// Packet shorter than header should be rejected
	_, ok := dev.deliverRXPacket(make([]byte, netHeaderLen-1))
	if ok {
		t.Fatal("expected false for short packet")
	}
}

func TestDeliverRXPacket_QueueNotReady(t *testing.T) {
	mem := make([]byte, 64*1024)
	dev := &NetDevice{tapName: "tap-test"}
	dev.Transport = NewTransport(dev, mem, 0x1000, 5, nil, nil)

	rxQ := dev.Queue(0)
	rxQ.Ready = false

	_, ok := dev.deliverRXPacket(make([]byte, netHeaderLen+10))
	if ok {
		t.Fatal("expected false for non-ready queue")
	}
}
