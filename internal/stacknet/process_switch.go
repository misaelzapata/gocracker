// process_switch.go provides a pure-Go in-process Layer-2 ethernet switch.
//
// It is used on hosts that cannot rely on a kernel bridge / netns (Windows,
// darwin, or any future platform) to wire several VM virtio-net backends
// together inside a single Compose project.  Each VM gets a port; the switch
// learns MAC addresses from observed source addresses and forwards unicast
// frames to the matching port, flooding broadcast / multicast / unknown
// unicast.
//
// No build tag: the type is cross-platform and is safe to import on every
// supported OS.  Platform-specific glue (e.g. wiring this onto the Windows
// stacknet.Manager) lives elsewhere.
package stacknet

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
)

// EthAddr is a parsed 48-bit ethernet hardware address.
type EthAddr [6]byte

// broadcastAddr is the all-ones destination used for L2 broadcast.
var broadcastAddr = EthAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

// switchPort represents a single attached port on the switch.
type switchPort struct {
	id     uint32
	rw     io.ReadWriter
	inQ    chan []byte
	closed atomic.Bool
}

// ProcessSwitch is an in-process Layer-2 ethernet switch.
//
// Ports are io.ReadWriter pairs supplied by the caller.  The switch reads
// frames from each port's reader, learns source MAC -> port mappings, and
// dispatches frames based on the destination MAC.
//
// The zero value is not usable; construct with NewProcessSwitch.
type ProcessSwitch struct {
	mu     sync.RWMutex
	ports  map[uint32]*switchPort
	nextID uint32
	macTab map[EthAddr]uint32

	stop   chan struct{}
	closed atomic.Bool

	// queueSize is the per-port inbound queue depth; drops happen if a
	// port can't keep up.  Exposed for tests.
	queueSize int
}

// NewProcessSwitch constructs an empty in-process L2 switch.
func NewProcessSwitch() *ProcessSwitch {
	return &ProcessSwitch{
		ports:     map[uint32]*switchPort{},
		macTab:    map[EthAddr]uint32{},
		stop:      make(chan struct{}),
		queueSize: 256,
	}
}

// Attach registers a port backed by rw.
//
// The switch starts a goroutine that reads ethernet frames from rw.Read and
// forwards them.  Anything written to the port by other senders is delivered
// via rw.Write.  Each port has an internal queue; if the writer is slower
// than the queue depth, frames are dropped silently (the goroutine logs no
// state — drops are tolerable for an L2 switch).
//
// Frames must be read by the underlying transport in whole-frame chunks: one
// Read call returns one frame.  This matches the semantics of the virtio-net
// shim used by VMs in the Compose stack.
func (s *ProcessSwitch) Attach(rw io.ReadWriter) uint32 {
	if rw == nil {
		return 0
	}
	s.mu.Lock()
	if s.closed.Load() {
		s.mu.Unlock()
		return 0
	}
	s.nextID++
	id := s.nextID
	p := &switchPort{
		id:  id,
		rw:  rw,
		inQ: make(chan []byte, s.queueSize),
	}
	s.ports[id] = p
	s.mu.Unlock()

	go s.readLoop(p)
	go s.writeLoop(p)
	return id
}

// Detach removes a port and stops its goroutines.  Any frames queued for
// this port that have not yet been written are dropped.  Detach is idempotent
// and safe to call from multiple goroutines.
func (s *ProcessSwitch) Detach(portID uint32) {
	s.mu.Lock()
	p, ok := s.ports[portID]
	if !ok {
		s.mu.Unlock()
		return
	}
	delete(s.ports, portID)
	// Purge MAC table entries pointing at this port.
	for mac, id := range s.macTab {
		if id == portID {
			delete(s.macTab, mac)
		}
	}
	s.mu.Unlock()

	if p.closed.CompareAndSwap(false, true) {
		close(p.inQ)
	}
}

// Send transmits a frame originating from the port identified by `from`.
// The frame must be a valid Ethernet II frame (≥14 bytes).  Frames smaller
// than that are dropped silently.
//
// Send is non-blocking: if the destination port's queue is full the frame is
// dropped.
func (s *ProcessSwitch) Send(from uint32, frame []byte) {
	if len(frame) < 14 {
		return
	}
	if s.closed.Load() {
		return
	}

	var dst, src EthAddr
	copy(dst[:], frame[0:6])
	copy(src[:], frame[6:12])

	// Learn from the source MAC unless src is zero/multicast.  Multicast
	// bit on src is invalid per 802.3 — ignore.
	s.mu.Lock()
	if !isMulticast(src) && !isZero(src) {
		if existing, ok := s.macTab[src]; !ok || existing != from {
			s.macTab[src] = from
		}
	}

	// Make a copy so caller can reuse `frame` immediately.
	buf := make([]byte, len(frame))
	copy(buf, frame)

	if isBroadcast(dst) || isMulticast(dst) {
		s.floodLocked(from, buf)
		s.mu.Unlock()
		return
	}
	if targetID, ok := s.macTab[dst]; ok && targetID != from {
		target := s.ports[targetID]
		s.mu.Unlock()
		if target != nil {
			s.deliver(target, buf)
		}
		return
	}
	// Unknown unicast: flood.
	s.floodLocked(from, buf)
	s.mu.Unlock()
}

// floodLocked delivers `buf` to every port except `exclude`.  s.mu must be
// held (read or write) by the caller.
func (s *ProcessSwitch) floodLocked(exclude uint32, buf []byte) {
	for id, p := range s.ports {
		if id == exclude {
			continue
		}
		s.deliver(p, buf)
	}
}

// deliver enqueues `buf` for `p`; drops on full queue.
func (s *ProcessSwitch) deliver(p *switchPort, buf []byte) {
	if p.closed.Load() {
		return
	}
	select {
	case p.inQ <- buf:
	default:
		// queue full — drop.
	}
}

// readLoop pulls frames off the port's underlying reader and calls Send.
func (s *ProcessSwitch) readLoop(p *switchPort) {
	// MTU + ethernet header + room for jumbo/tags.  Compose VMs use the
	// default 1500 MTU.
	const maxFrame = 65536
	buf := make([]byte, maxFrame)
	for {
		if p.closed.Load() || s.closed.Load() {
			return
		}
		n, err := p.rw.Read(buf)
		if err != nil {
			return
		}
		if n <= 0 {
			continue
		}
		s.Send(p.id, buf[:n])
	}
}

// writeLoop drains the port's inbound queue into the underlying writer.
func (s *ProcessSwitch) writeLoop(p *switchPort) {
	for frame := range p.inQ {
		if p.closed.Load() {
			return
		}
		_, err := p.rw.Write(frame)
		if err != nil {
			return
		}
	}
}

// Close shuts the switch down: detach every port and refuse new attaches.
// Close is idempotent.
func (s *ProcessSwitch) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	s.mu.Lock()
	ports := s.ports
	s.ports = map[uint32]*switchPort{}
	s.macTab = map[EthAddr]uint32{}
	s.mu.Unlock()

	for _, p := range ports {
		if p.closed.CompareAndSwap(false, true) {
			close(p.inQ)
		}
	}
	close(s.stop)
	return nil
}

// PortCount returns the number of currently-attached ports.  Mainly for
// tests and metrics.
func (s *ProcessSwitch) PortCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.ports)
}

// LookupMAC returns the port ID associated with `mac`, or 0 if unknown.
// Mainly for tests.
func (s *ProcessSwitch) LookupMAC(mac EthAddr) uint32 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.macTab[mac]
}

// --- helpers --------------------------------------------------------------

// isBroadcast reports whether mac is the all-ones broadcast address.
func isBroadcast(mac EthAddr) bool {
	return mac == broadcastAddr
}

// isMulticast reports whether mac is a multicast address (LSB of first byte
// set).  Broadcast is a special-case multicast and also matches.
func isMulticast(mac EthAddr) bool {
	return mac[0]&0x01 != 0
}

// isZero reports whether mac is the all-zeros invalid address.
func isZero(mac EthAddr) bool {
	for _, b := range mac {
		if b != 0 {
			return false
		}
	}
	return true
}

// ErrSwitchClosed is returned if an operation is invoked on a closed switch.
// Currently informational only — Send/Attach silently no-op after Close.
var ErrSwitchClosed = errors.New("stacknet: process switch is closed")
