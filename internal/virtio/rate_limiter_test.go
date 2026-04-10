//go:build linux

package virtio

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"
	"time"
)

func TestRateLimiterDisabledIsNoop(t *testing.T) {
	var rl RateLimiter
	if rl.Enabled() {
		t.Fatal("zero-value limiter should be disabled")
	}
	if got := rl.reserve(64, 1); got != 0 {
		t.Fatalf("reserve() = %v, want 0", got)
	}
}

func TestRateLimiterReserveConsumesTokens(t *testing.T) {
	rl := NewRateLimiter(RateLimiterConfig{
		Bandwidth: TokenBucket{Size: 10, OneTimeBurst: 5, RefillTime: time.Second},
	})

	if got := rl.reserve(5, 0); got != 0 {
		t.Fatalf("reserve() = %v, want 0", got)
	}
	if got := rl.reserve(6, 0); got <= 0 {
		t.Fatalf("reserve() = %v, want positive wait", got)
	}
}

func TestRateLimiterReserveOps(t *testing.T) {
	rl := NewRateLimiter(RateLimiterConfig{
		Ops: TokenBucket{Size: 1, OneTimeBurst: 0, RefillTime: 20 * time.Millisecond},
	})

	if got := rl.reserve(0, 1); got <= 0 {
		t.Fatalf("reserve() = %v, want positive wait", got)
	}
}

func TestNetTransmitRateLimited(t *testing.T) {
	mem := make([]byte, 64*1024)
	nd := &NetDevice{tapName: "tap-test"}
	nd.Transport = NewTransport(nd, mem, 0x1000, 5, nil, nil)
	nd.tapFd = mustPipeWriter(t)
	nd.SetRateLimiter(NewRateLimiter(RateLimiterConfig{
		Bandwidth: TokenBucket{Size: 4, OneTimeBurst: 0, RefillTime: 15 * time.Millisecond},
	}))
	t.Cleanup(func() { _ = nd.Close() })

	q := nd.Queue(1)
	q.Ready = true
	q.Size = 256
	q.DescAddr = 0x1000
	q.DriverAddr = 0x2000
	q.DeviceAddr = 0x3000

	pktAddr := uint64(0x5000)
	copy(mem[pktAddr:], []byte("abcd"))
	writeDesc(mem, q.DescAddr, 0, pktAddr, 4, 0, 0)
	writeAvailEntry(mem, q.DriverAddr, 0, 0)

	start := time.Now()
	nd.transmit(q)
	if time.Since(start) < 10*time.Millisecond {
		t.Fatalf("transmit returned too quickly, limiter likely not applied")
	}
}

func TestNetReceiveRateLimited(t *testing.T) {
	mem := make([]byte, 64*1024)
	nd := &NetDevice{tapName: "tap-test"}
	nd.Transport = NewTransport(nd, mem, 0x1000, 5, nil, nil)
	nd.SetRateLimiter(NewRateLimiter(RateLimiterConfig{
		Bandwidth: TokenBucket{Size: 16, OneTimeBurst: 0, RefillTime: 15 * time.Millisecond},
	}))

	rxQ := nd.Queue(0)
	rxQ.Ready = true
	rxQ.Size = 256
	rxQ.DescAddr = 0x1000
	rxQ.DriverAddr = 0x2000
	rxQ.DeviceAddr = 0x3000

	bufAddr := uint64(0x6000)
	writeDesc(mem, rxQ.DescAddr, 0, bufAddr, 16, DescFlagWrite, 0)
	writeAvailEntry(mem, rxQ.DriverAddr, 0, 0)

	pkt := make([]byte, 16)
	copy(pkt, []byte{1, 2, 3, 4})
	start := time.Now()
	if written, ok := nd.deliverRXPacket(pkt); !ok || written == 0 {
		t.Fatalf("deliverRXPacket() = (%d, %v), want success", written, ok)
	}
	if time.Since(start) < 10*time.Millisecond {
		t.Fatalf("deliverRXPacket returned too quickly, limiter likely not applied")
	}
	if !bytes.Equal(mem[bufAddr:bufAddr+4], []byte{1, 2, 3, 4}) {
		t.Fatalf("RX payload not copied into guest memory")
	}
}

func TestBlockDeviceRateLimitedReadAndFlush(t *testing.T) {
	const diskSize = 4096
	blk, mem, q := newBlkTestEnv(t, diskSize, false)
	blk.SetRateLimiter(NewRateLimiter(RateLimiterConfig{
		Bandwidth: TokenBucket{Size: 512, OneTimeBurst: 0, RefillTime: 15 * time.Millisecond},
		Ops:       TokenBucket{Size: 1, OneTimeBurst: 0, RefillTime: 15 * time.Millisecond},
	}))

	pattern := bytes.Repeat([]byte("ABCD"), 128)
	statusAddr := processBlkRequest(t, mem, q, blk, BlkTOut, 0, pattern, 512, 0, 0)
	if mem[statusAddr] != BlkSOK {
		t.Fatalf("write status = %d, want %d", mem[statusAddr], BlkSOK)
	}

	start := time.Now()
	statusAddr = processBlkRequest(t, mem, q, blk, BlkTIn, 0, nil, 512, DescFlagWrite, 3)
	if mem[statusAddr] != BlkSOK {
		t.Fatalf("read status = %d, want %d", mem[statusAddr], BlkSOK)
	}
	if time.Since(start) < 10*time.Millisecond {
		t.Fatalf("blk read returned too quickly, limiter likely not applied")
	}

	start = time.Now()
	statusAddr = processBlkRequest(t, mem, q, blk, BlkTFlush, 0, nil, 0, DescFlagWrite, 0)
	if mem[statusAddr] != BlkSOK {
		t.Fatalf("flush status = %d, want %d", mem[statusAddr], BlkSOK)
	}
	if time.Since(start) < 10*time.Millisecond {
		t.Fatalf("blk flush returned too quickly, limiter likely not applied")
	}
}

func TestRNGDeviceRateLimited(t *testing.T) {
	mem := make([]byte, 64*1024)
	rng := NewRNGDevice(mem, 0x1000, 5, nil, nil)
	rng.SetRateLimiter(NewRateLimiter(RateLimiterConfig{
		Bandwidth: TokenBucket{Size: 16, OneTimeBurst: 0, RefillTime: 15 * time.Millisecond},
		Ops:       TokenBucket{Size: 1, OneTimeBurst: 0, RefillTime: 15 * time.Millisecond},
	}))

	q := rng.Queue(0)
	q.Ready = true
	q.Size = 256
	q.DescAddr = 0x1000
	q.DriverAddr = 0x2000
	q.DeviceAddr = 0x3000

	bufAddr := uint64(0x7000)
	writeDesc(mem, q.DescAddr, 0, bufAddr, 16, DescFlagWrite, 0)
	writeAvailEntry(mem, q.DriverAddr, 0, 0)

	start := time.Now()
	rng.HandleQueue(0, q)
	if time.Since(start) < 10*time.Millisecond {
		t.Fatalf("rng delivery returned too quickly, limiter likely not applied")
	}
	if binary.LittleEndian.Uint32(mem[bufAddr:bufAddr+4]) == 0 {
		t.Fatal("rng output appears empty")
	}
}

func mustPipeWriter(t *testing.T) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})
	return w
}
