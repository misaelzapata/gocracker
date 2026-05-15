package stacknet

import (
	"bytes"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakePort implements io.ReadWriter for tests.  Frames written by the switch
// land in `rxFrames` (channel) and frames the test wants to inject are pushed
// via the `tx` channel which the switch's readLoop pulls.
type fakePort struct {
	mu      sync.Mutex
	tx      chan []byte
	rx      chan []byte
	closed  atomic.Bool
	readErr error
}

func newFakePort() *fakePort {
	return &fakePort{
		tx: make(chan []byte, 32),
		rx: make(chan []byte, 32),
	}
}

func (p *fakePort) Read(b []byte) (int, error) {
	if p.closed.Load() {
		return 0, io.EOF
	}
	frame, ok := <-p.tx
	if !ok {
		return 0, io.EOF
	}
	n := copy(b, frame)
	return n, nil
}

func (p *fakePort) Write(b []byte) (int, error) {
	if p.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	buf := make([]byte, len(b))
	copy(buf, b)
	select {
	case p.rx <- buf:
		return len(b), nil
	case <-time.After(2 * time.Second):
		return 0, io.ErrShortWrite
	}
}

func (p *fakePort) close() {
	if p.closed.CompareAndSwap(false, true) {
		close(p.tx)
	}
}

// frame builds an Ethernet II header (dst+src+ethertype) plus payload.
func frame(dst, src EthAddr, ethertype uint16, payload []byte) []byte {
	out := make([]byte, 14+len(payload))
	copy(out[0:6], dst[:])
	copy(out[6:12], src[:])
	out[12] = byte(ethertype >> 8)
	out[13] = byte(ethertype)
	copy(out[14:], payload)
	return out
}

func waitFrame(t *testing.T, ch <-chan []byte, want []byte) {
	t.Helper()
	select {
	case got := <-ch:
		if !bytes.Equal(got, want) {
			t.Fatalf("frame mismatch:\n got: %x\nwant: %x", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for frame")
	}
}

func expectNoFrame(t *testing.T, ch <-chan []byte, dur time.Duration) {
	t.Helper()
	select {
	case got := <-ch:
		t.Fatalf("unexpected frame: %x", got)
	case <-time.After(dur):
	}
}

// TestBroadcastFloods: a broadcast frame from port1 must be seen on port2+3
// but not on port1 itself.
func TestProcessSwitch_BroadcastFloods(t *testing.T) {
	sw := NewProcessSwitch()
	defer sw.Close()

	p1, p2, p3 := newFakePort(), newFakePort(), newFakePort()
	id1 := sw.Attach(p1)
	_ = sw.Attach(p2)
	_ = sw.Attach(p3)

	srcMAC := EthAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x01}
	f := frame(broadcastAddr, srcMAC, 0x0806, []byte("arp-payload"))
	sw.Send(id1, f)

	waitFrame(t, p2.rx, f)
	waitFrame(t, p3.rx, f)
	expectNoFrame(t, p1.rx, 50*time.Millisecond)
}

// TestUnicastAfterLearning: after port2 has sent a frame (so its MAC is
// learned), a unicast from port1 to that MAC must reach only port2.
func TestProcessSwitch_UnicastAfterLearning(t *testing.T) {
	sw := NewProcessSwitch()
	defer sw.Close()

	p1, p2, p3 := newFakePort(), newFakePort(), newFakePort()
	id1 := sw.Attach(p1)
	id2 := sw.Attach(p2)
	_ = sw.Attach(p3)

	macA := EthAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0xaa}
	macB := EthAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0xbb}

	// Port 2 sends a broadcast so its MAC is learned.
	announce := frame(broadcastAddr, macB, 0x0800, []byte("hello"))
	sw.Send(id2, announce)
	// Drain delivered copies on the other two ports so we don't pick them
	// up later.
	waitFrame(t, p1.rx, announce)
	waitFrame(t, p3.rx, announce)

	// Verify MAC table.
	if got := sw.LookupMAC(macB); got != id2 {
		t.Fatalf("MAC table: got port %d for macB, want %d", got, id2)
	}

	// Now port 1 sends a unicast to macB.  Must go to p2 only.
	pkt := frame(macB, macA, 0x0800, []byte("ping"))
	sw.Send(id1, pkt)
	waitFrame(t, p2.rx, pkt)
	expectNoFrame(t, p3.rx, 50*time.Millisecond)
	expectNoFrame(t, p1.rx, 0)
}

// TestUnknownUnicastFloods: a unicast to an unknown MAC should flood.
func TestProcessSwitch_UnknownUnicastFloods(t *testing.T) {
	sw := NewProcessSwitch()
	defer sw.Close()

	p1, p2, p3 := newFakePort(), newFakePort(), newFakePort()
	id1 := sw.Attach(p1)
	_ = sw.Attach(p2)
	_ = sw.Attach(p3)

	srcMAC := EthAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x01}
	unknownDst := EthAddr{0x02, 0xff, 0xff, 0xff, 0xff, 0xff}
	f := frame(unknownDst, srcMAC, 0x0800, []byte("hello"))
	sw.Send(id1, f)

	waitFrame(t, p2.rx, f)
	waitFrame(t, p3.rx, f)
}

// TestDetachClearsMACEntries: after a port detaches, its MAC entries vanish.
func TestProcessSwitch_DetachClearsMAC(t *testing.T) {
	sw := NewProcessSwitch()
	defer sw.Close()

	p1 := newFakePort()
	id1 := sw.Attach(p1)
	mac := EthAddr{0x02, 0xaa, 0xbb, 0xcc, 0xdd, 0xee}
	sw.Send(id1, frame(broadcastAddr, mac, 0x0800, nil))

	if sw.LookupMAC(mac) != id1 {
		t.Fatal("MAC not learned")
	}
	sw.Detach(id1)
	if sw.LookupMAC(mac) != 0 {
		t.Fatal("MAC table not purged after Detach")
	}
}

// TestShortFrameDropped: frames < 14 bytes must be dropped.
func TestProcessSwitch_ShortFrameDropped(t *testing.T) {
	sw := NewProcessSwitch()
	defer sw.Close()
	p1, p2 := newFakePort(), newFakePort()
	_ = sw.Attach(p1)
	_ = sw.Attach(p2)
	sw.Send(0, []byte{1, 2, 3})
	expectNoFrame(t, p2.rx, 50*time.Millisecond)
}

// TestConcurrentSendDetach exercises the switch under -race: many goroutines
// send / detach / re-attach concurrently.  No deadlocks, no panics.
func TestProcessSwitch_ConcurrentSendDetach(t *testing.T) {
	sw := NewProcessSwitch()
	defer sw.Close()

	const (
		nPorts   = 8
		nSenders = 4
		duration = 250 * time.Millisecond
	)
	ports := make([]*fakePort, nPorts)
	ids := make([]uint32, nPorts)
	for i := range ports {
		ports[i] = newFakePort()
		ids[i] = sw.Attach(ports[i])
		// Drain rx in the background — fake writer blocks otherwise.
		go func(p *fakePort) {
			for range p.rx {
			}
		}(ports[i])
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for s := 0; s < nSenders; s++ {
		wg.Add(1)
		go func(seed byte) {
			defer wg.Done()
			mac := EthAddr{0x02, seed, 0, 0, 0, 0}
			f := frame(broadcastAddr, mac, 0x0800, []byte("hi"))
			for {
				select {
				case <-stop:
					return
				default:
				}
				sw.Send(ids[int(seed)%nPorts], f)
			}
		}(byte(s))
	}

	// Churn: detach and re-attach a port repeatedly.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			sw.Detach(ids[0])
			ports[0] = newFakePort()
			ids[0] = sw.Attach(ports[0])
			go func(p *fakePort) {
				for range p.rx {
				}
			}(ports[0])
		}
	}()

	time.Sleep(duration)
	close(stop)
	wg.Wait()
}

// TestCloseIdempotent: Close called twice must not panic.
func TestProcessSwitch_CloseIdempotent(t *testing.T) {
	sw := NewProcessSwitch()
	if err := sw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sw.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestAttachAfterClose returns 0.
func TestProcessSwitch_AttachAfterClose(t *testing.T) {
	sw := NewProcessSwitch()
	_ = sw.Close()
	if id := sw.Attach(newFakePort()); id != 0 {
		t.Fatalf("Attach after Close = %d, want 0", id)
	}
}
