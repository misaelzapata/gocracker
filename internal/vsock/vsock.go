// Package vsock implements a virtio-vsock device and a host-side
// connection multiplexer. It lets the host communicate with processes
// inside the guest over lightweight streams, similar to Firecracker's vsock.
//
// Guest CID is fixed at 3 (the conventional "guest" CID).
// Host CID is 2.
package vsock

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	gclog "github.com/gocracker/gocracker/internal/log"
	"github.com/gocracker/gocracker/internal/virtio"
	"golang.org/x/sys/unix"
)

const (
	GuestCID    = 3
	HostCID     = 2
	dialTimeout = 15 * time.Second

	// Firecracker's virtio-vsock advertises VIRTIO_F_IN_ORDER in addition to
	// VIRTIO_F_VERSION_1. Our device also returns used buffers in order, so
	// expose the same hint to keep the guest driver path aligned.
	vsockFeatureInOrder = 1 << 35

	// Packet opcodes
	opRequest       = 1
	opResponse      = 2
	opReset         = 3
	opShutdown      = 4
	opRW            = 5
	opCreditUpdate  = 6
	opCreditRequest = 7

	// vsock packet header size
	hdrSize = 44
)

// pktHdr is the virtio-vsock packet header (44 bytes).
type pktHdr struct {
	SrcCID   uint64
	DstCID   uint64
	SrcPort  uint32
	DstPort  uint32
	Len      uint32
	Type     uint16 // 1=stream
	Op       uint16
	Flags    uint32
	BufAlloc uint32
	FwdCnt   uint32
}

// Connection represents one guest↔host vsock stream.
type Connection struct {
	guestPort uint32
	hostPort  uint32
	conn      net.Conn
	device    *Device
}

type connKey struct {
	guestPort uint32
	hostPort  uint32
}

type pendingConn struct {
	conn  *Connection
	ready chan error
}

// Device is a virtio-vsock device.
type Device struct {
	*virtio.Transport
	mu           sync.Mutex
	conns        map[connKey]*Connection
	pending      map[uint32]*pendingConn
	nextHostPort uint32
	listenFn     func(port uint32) (net.Conn, error)
	mem          []byte
	closed       bool
	closeCh      chan struct{}
	closeOnce    sync.Once
	rxWG         sync.WaitGroup
	Label        string // optional VM identifier for debug logs
}

// NewDevice creates a vsock device. listenFn is called when the guest
// initiates a connection to the given host port.
func NewDevice(mem []byte, basePA uint64, irq uint8, listenFn func(uint32) (net.Conn, error), dirty *virtio.DirtyTracker, irqFn func(bool)) *Device {
	d := &Device{
		mem:          mem,
		conns:        make(map[connKey]*Connection),
		pending:      make(map[uint32]*pendingConn),
		nextHostPort: 1024,
		listenFn:     listenFn,
		closeCh:      make(chan struct{}),
	}
	d.Transport = virtio.NewTransport(d, mem, basePA, irq, dirty, irqFn)
	return d
}

func (d *Device) DeviceID() uint32       { return 19 } // VIRTIO_ID_VSOCK
func (d *Device) DeviceFeatures() uint64 { return vsockFeatureInOrder }
func (d *Device) ConfigBytes() []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, GuestCID)
	return b
}

// HandleQueue processes rx (0), tx (1), and event (2) queues.
func (d *Device) HandleQueue(idx uint32, q *virtio.Queue) {
	if idx == 1 { // TX queue: guest → host
		d.processTX(q)
	}
	// RX (0) and event (2) are filled by the host asynchronously
}

func (d *Device) HandleQueueNotify(idx uint32, q *virtio.Queue) bool {
	if idx != 1 {
		return false
	}
	gclog.VMM.Debug("vsock HandleQueueNotify TX", "vm", d.Label, "idx", idx)
	return d.processTX(q)
}

func (d *Device) processTX(q *virtio.Queue) bool {
	processed := false
	if err := q.IterAvail(func(head uint16) {
		processed = true
		chain, err := q.WalkChain(head)
		if err != nil {
			gclog.VMM.Warn("virtio-vsock invalid TX descriptor chain", "head", head, "error", err)
			_ = q.PushUsed(uint32(head), 0)
			return
		}
		if len(chain) == 0 {
			gclog.VMM.Debug("[DBG-TX] empty chain", "vm", d.Label, "head", head)
			return
		}
		// Read header
		if chain[0].Len < hdrSize {
			gclog.VMM.Debug("[DBG-TX] short descriptor", "vm", d.Label, "head", head, "len", chain[0].Len)
			_ = q.PushUsed(uint32(head), 0)
			return
		}
		hdrBuf := make([]byte, hdrSize)
		if err := q.GuestRead(chain[0].Addr, hdrBuf); err != nil {
			gclog.VMM.Warn("virtio-vsock TX header read failed", "head", head, "error", err)
			_ = q.PushUsed(uint32(head), 0)
			return
		}
		var hdr pktHdr
		parseHdr(hdrBuf, &hdr)
		gclog.VMM.Debug("[DBG-TX] parsed", "vm", d.Label, "op", hdr.Op, "srcPort", hdr.SrcPort, "dstPort", hdr.DstPort)

		switch hdr.Op {
		case opRequest:
			d.handleConnect(&hdr, q)
		case opResponse:
			d.handleResponse(&hdr, q)
		case opRW:
			d.handleData(&hdr, chain, q)
		case opShutdown, opReset:
			d.handleDisconnect(&hdr)
		case opCreditUpdate, opCreditRequest:
			// flow control — acknowledge
		}
		_ = q.PushUsed(uint32(head), 0)
	}); err != nil {
		gclog.VMM.Warn("virtio-vsock TX queue iteration failed", "error", err)
	}
	// Log TX queue state after processing
	txQ := d.Transport.Queue(1)
	if txQ != nil {
		avIdx, _ := txQ.AvailIdx()
		gclog.VMM.Debug("[DBG-TX] queue state after processTX", "vm", d.Label, "processed", processed, "lastAvail", txQ.LastAvail, "availIdx", avIdx, "delta", uint16(avIdx-txQ.LastAvail))
	}
	return processed
}

func (d *Device) handleConnect(hdr *pktHdr, q *virtio.Queue) {
	gclog.VMM.Debug("vsock handleConnect", "vm", d.Label, "guestPort", hdr.SrcPort, "hostPort", hdr.DstPort, "listenFn_nil", d.listenFn == nil)
	if d.listenFn == nil {
		gclog.VMM.Debug("vsock handleConnect: no listenFn, sending reset", "vm", d.Label)
		d.sendReset(hdr)
		return
	}
	conn, err := d.listenFn(hdr.DstPort)
	if err != nil {
		gclog.VMM.Debug("vsock handleConnect: listenFn error, sending reset", "vm", d.Label, "err", err)
		d.sendReset(hdr)
		return
	}
	gclog.VMM.Debug("vsock handleConnect: accepted", "vm", d.Label, "guestPort", hdr.SrcPort, "hostPort", hdr.DstPort)
	c := &Connection{
		guestPort: hdr.SrcPort,
		hostPort:  hdr.DstPort,
		conn:      conn,
		device:    d,
	}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		conn.Close()
		return
	}
	d.conns[connKey{guestPort: hdr.SrcPort, hostPort: hdr.DstPort}] = c
	d.rxWG.Add(1)
	d.mu.Unlock()

	// Send RESPONSE
	d.sendPkt(hdr.DstCID, hdr.SrcCID, hdr.DstPort, hdr.SrcPort, opResponse, nil)

	// Pump data from host connection → guest
	go func() {
		defer d.rxWG.Done()
		c.rxPump(q)
	}()
}

func (d *Device) handleResponse(hdr *pktHdr, q *virtio.Queue) {
	d.mu.Lock()
	pending := d.pending[hdr.DstPort]
	if pending != nil {
		delete(d.pending, hdr.DstPort)
		d.conns[connKey{guestPort: hdr.SrcPort, hostPort: hdr.DstPort}] = pending.conn
	}
	d.mu.Unlock()
	if pending == nil {
		return
	}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		pending.conn.conn.Close()
		pending.ready <- fmt.Errorf("vsock device closed")
		return
	}
	d.rxWG.Add(1)
	d.mu.Unlock()
	pending.ready <- nil
	go func() {
		defer d.rxWG.Done()
		pending.conn.rxPump(q)
	}()
}

func (d *Device) handleData(hdr *pktHdr, chain []virtio.Desc, q *virtio.Queue) {
	d.mu.Lock()
	c := d.conns[connKey{guestPort: hdr.SrcPort, hostPort: hdr.DstPort}]
	d.mu.Unlock()
	if c == nil {
		// Unknown connection — mirror Firecracker and reply with RST so the
		// guest drops the stale socket instead of silently blocking. This
		// matters after snapshot/restore: the restored host device has a
		// fresh (empty) conns map, so any packet arriving for a pre-snapshot
		// conn is "unknown" and the guest needs an explicit kill signal to
		// reach `for { dial; handleExecAgentConn }` and redial the new
		// broker. Without this RST the guest sits in Read forever.
		gclog.VMM.Debug("[DBG-VSOCK] handleData: conn NOT found, sending RST", "guestPort", hdr.SrcPort, "hostPort", hdr.DstPort)
		d.sendReset(hdr)
		return
	}
	if hdr.Len == 0 {
		return
	}
	// Collect data from subsequent descriptors
	var data []byte
	for _, desc := range chain[1:] {
		buf := make([]byte, desc.Len)
		if err := q.GuestRead(desc.Addr, buf); err != nil {
			gclog.VMM.Warn("virtio-vsock TX payload read failed", "error", err)
			return
		}
		data = append(data, buf...)
	}
	if len(data) > int(hdr.Len) {
		data = data[:hdr.Len]
	}
	gclog.VMM.Debug("[DBG-VSOCK] handleData: guest→host", "guestPort", hdr.SrcPort, "hostPort", hdr.DstPort, "len", len(data))
	n, err := c.conn.Write(data)
	if err != nil {
		gclog.VMM.Warn("[DBG-VSOCK] handleData: conn.Write FAILED", "guestPort", hdr.SrcPort, "hostPort", hdr.DstPort, "n", n, "err", err)
	}
}

func (d *Device) handleDisconnect(hdr *pktHdr) {
	d.mu.Lock()
	key := connKey{guestPort: hdr.SrcPort, hostPort: hdr.DstPort}
	c := d.conns[key]
	delete(d.conns, key)
	pending := d.pending[hdr.DstPort]
	if pending != nil {
		delete(d.pending, hdr.DstPort)
	}
	connsLen := len(d.conns)
	d.mu.Unlock()
	gclog.VMM.Debug("[DBG-DISC] handleDisconnect", "vm", d.Label, "op", hdr.Op, "guestPort", hdr.SrcPort, "hostPort", hdr.DstPort, "connFound", c != nil, "pendingFound", pending != nil, "remainingConns", connsLen)
	if c != nil {
		c.conn.Close()
		// Send RST back so the guest's close() completes promptly
		// instead of timing out waiting for the peer to acknowledge.
		d.sendPkt(hdr.DstCID, hdr.SrcCID, hdr.DstPort, hdr.SrcPort, opReset, nil)
	}
	if pending != nil {
		pending.conn.conn.Close()
		pending.ready <- fmt.Errorf("vsock connection rejected by guest")
	}
}

// StartTXAvailPoller starts a background goroutine that polls the TX
// queue's avail idx every 500ms for duration, logging when the guest
// adds entries that weren't picked up by HandleQueueNotify. This detects
// if the guest is writing to the TX vring without kicking the host.
func (d *Device) StartTXAvailPoller(duration time.Duration) {
	txQ := d.Transport.Queue(1)
	rxQ := d.Transport.Queue(0)
	if txQ == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		deadline := time.After(duration)
		for {
			select {
			case <-ticker.C:
				// Device.Close() races with guest RAM unmap in the VMM
				// cleanup path; re-check closed under d.mu before reading
				// from mmap'd memory.
				d.mu.Lock()
				if d.closed {
					d.mu.Unlock()
					return
				}
				d.mu.Unlock()
				avIdx, err := txQ.AvailIdx()
				if err != nil {
					continue
				}
				delta := uint16(avIdx - txQ.LastAvail)
				rxAvail := "n/a"
				if rxQ != nil {
					rxAvIdx, rxErr := rxQ.AvailIdx()
					if rxErr == nil {
						rxDelta := uint16(rxAvIdx - rxQ.LastAvail)
						rxAvail = fmt.Sprintf("avail=%d,last=%d,free=%d", rxAvIdx, rxQ.LastAvail, rxDelta)
					}
				}
				if delta > 0 {
					gclog.VMM.Warn("[DBG-POLL] TX avail AHEAD of lastAvail!", "vm", d.Label, "availIdx", avIdx, "lastAvail", txQ.LastAvail, "delta", delta, "rx", rxAvail)
				} else {
					gclog.VMM.Debug("[DBG-POLL] TX avail check", "vm", d.Label, "availIdx", avIdx, "lastAvail", txQ.LastAvail, "rx", rxAvail)
				}
			case <-d.closeCh:
				gclog.VMM.Debug("[DBG-POLL] TX poller stopped (device closed)", "vm", d.Label)
				return
			case <-deadline:
				gclog.VMM.Debug("[DBG-POLL] TX poller stopped", "vm", d.Label)
				return
			}
		}
	}()
}

func (d *Device) sendReset(hdr *pktHdr) {
	d.sendPkt(hdr.DstCID, hdr.SrcCID, hdr.DstPort, hdr.SrcPort, opReset, nil)
}

// sendPkt returns true if the packet was successfully queued into the guest RX
// queue, false if the queue was not ready (guest virtio driver not yet init'd).
func (d *Device) sendPkt(srcCID, dstCID uint64, srcPort, dstPort uint32, op uint16, data []byte) bool {
	hdr := &pktHdr{
		SrcCID:   srcCID,
		DstCID:   dstCID,
		SrcPort:  srcPort,
		DstPort:  dstPort,
		Len:      uint32(len(data)),
		Type:     1, // stream
		Op:       op,
		BufAlloc: 65536,
	}
	hdrBytes := marshalHdr(hdr)
	payload := append(hdrBytes, data...)

	if d.Transport == nil {
		gclog.VMM.Debug("[DBG-VSOCK] sendPkt: Transport is nil!", "op", op, "srcPort", srcPort, "dstPort", dstPort)
		return false
	}
	rxQ := d.Transport.Queue(0)
	if rxQ == nil || !rxQ.Ready {
		gclog.VMM.Debug("[DBG-VSOCK] sendPkt: RX queue not ready!", "op", op, "srcPort", srcPort, "dstPort", dstPort, "rxQ_nil", rxQ == nil)
		return false
	}
	ok, err := rxQ.ConsumeAvail(func(head uint16) {
		chain, walkErr := rxQ.WalkChain(head)
		if walkErr != nil {
			gclog.VMM.Warn("virtio-vsock invalid RX descriptor chain", "head", head, "error", walkErr)
			_ = rxQ.PushUsed(uint32(head), 0)
			return
		}
		written := uint32(0)
		remaining := payload
		for _, desc := range chain {
			if desc.Flags&virtio.DescFlagWrite == 0 {
				continue
			}
			sz := uint32(len(remaining))
			if sz > desc.Len {
				sz = desc.Len
			}
			if err := rxQ.GuestWrite(desc.Addr, remaining[:sz]); err != nil {
				gclog.VMM.Warn("virtio-vsock RX guest write failed", "head", head, "error", err)
				break
			}
			remaining = remaining[sz:]
			written += sz
			if len(remaining) == 0 {
				break
			}
		}
		_ = rxQ.PushUsed(uint32(head), written)
	})
	if err != nil {
		gclog.VMM.Warn("virtio-vsock RX queue consume failed", "error", err)
		return false
	}
	if !ok {
		gclog.VMM.Debug("[DBG-VSOCK] sendPkt: no avail descriptors in RX queue!", "op", op, "srcPort", srcPort, "dstPort", dstPort, "dataLen", len(data))
		return false
	}
	d.Transport.SetInterruptStat(1)
	d.Transport.SignalIRQ(true)
	return true
}

// rxPump reads from the host connection and injects data into the guest RX queue.
func (c *Connection) rxPump(q *virtio.Queue) {
	defer func() {
		c.device.mu.Lock()
		delete(c.device.conns, connKey{guestPort: c.guestPort, hostPort: c.hostPort})
		c.device.mu.Unlock()
	}()

	buf := make([]byte, 4096)
	for {
		n, err := c.conn.Read(buf)
		if c.device.isClosed() {
			return
		}
		if n > 0 {
			gclog.VMM.Debug("[DBG-VSOCK] rxPump: host→guest", "hostPort", c.hostPort, "guestPort", c.guestPort, "len", n)
			c.device.sendPkt(HostCID, GuestCID, c.hostPort, c.guestPort, opRW, buf[:n])
		}
		if err == io.EOF || err != nil {
			gclog.VMM.Debug("[DBG-VSOCK] rxPump: exit", "hostPort", c.hostPort, "guestPort", c.guestPort, "err", err)
			if !c.device.isClosed() {
				c.device.sendPkt(HostCID, GuestCID, c.hostPort, c.guestPort, opShutdown, nil)
			}
			return
		}
	}
}

func (d *Device) Dial(port uint32) (net.Conn, error) {
	deadline := time.Now().Add(dialTimeout)

	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("vsock dial timeout for guest port %d", port)
		}

		hostConn, deviceConn := net.Pipe()
		pending := &pendingConn{
			conn: &Connection{
				guestPort: port,
				hostPort:  d.allocateHostPort(),
				conn:      deviceConn,
				device:    d,
			},
			ready: make(chan error, 1),
		}

		d.mu.Lock()
		d.pending[pending.conn.hostPort] = pending
		d.mu.Unlock()

		// Retry sending opRequest until the guest's RX queue is ready.
		// On fresh boot the virtio-vsock driver initializes its queues ~50–200 ms
		// after the vCPU starts; before that sendPkt returns false (queue not ready).
		sent := false
		for !sent {
			if time.Now().After(deadline) {
				d.mu.Lock()
				delete(d.pending, pending.conn.hostPort)
				d.mu.Unlock()
				hostConn.Close()
				deviceConn.Close()
				return nil, fmt.Errorf("vsock dial timeout: RX queue never ready for port %d", port)
			}
			if d.sendPkt(HostCID, GuestCID, pending.conn.hostPort, port, opRequest, nil) {
				sent = true
			} else {
				time.Sleep(10 * time.Millisecond)
			}
		}

		select {
		case err := <-pending.ready:
			if err != nil {
				// opReset received — guest had no listener yet (or rejected).
				// Clean up this attempt and retry after a short pause.
				hostConn.Close()
				gclog.VMM.Debug("vsock Dial: opReset, retrying", "vm", d.Label, "port", port, "err", err)
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return hostConn, nil
		case <-time.After(time.Until(deadline)):
			d.mu.Lock()
			delete(d.pending, pending.conn.hostPort)
			d.mu.Unlock()
			hostConn.Close()
			deviceConn.Close()
			return nil, fmt.Errorf("vsock dial timeout for guest port %d", port)
		}
	}
}

// QuiesceForSnapshot makes the guest-side vsock driver drop every open
// socket, before the vCPUs are paused and a memory snapshot is taken.
//
// The Firecracker contract is "vsock connections do not survive
// snapshot/restore" (docs/snapshotting/snapshot-support.md). If the guest
// kernel's AF_VSOCK sockets are still in state ESTABLISHED when the memory
// image is captured, the restored guest will be blocked in
// serveExecAgent's Decode forever — the host process that owned the peer
// half of those sockets no longer exists, but the guest has no way to find
// that out by itself.
//
// Two independent wake-up signals are delivered here, in this order,
// because every individual signal has a failure mode:
//
//  1. Per-conn synchronous opRst on the RX queue (index 0). For every
//     connection we currently track, we push a single "reset" packet to
//     the RX ring. The Linux virtio-vsock driver processes this inline
//     from its RX work function, walks the socket by (dst_cid, dst_port,
//     src_cid, src_port), and calls virtio_transport_reset_no_sock /
//     virtio_transport_do_close — which wakes any reader blocked in
//     recvmsg. This is the same path virtio uses for a natural peer RST,
//     so it is already well-exercised in the guest kernel and does not
//     depend on the event queue.
//
//  2. VIRTIO_VSOCK_EVENT_TRANSPORT_RESET on the event queue (index 2).
//     This is what Firecracker emits; it walks virtio_vsock_conns and
//     resets any socket the driver knows about but our host-side conns
//     map has already forgotten (stale from a previous restore, or
//     connections we never tracked). Belt-and-suspenders.
//
// After signaling, we poll the RX queue's avail ring for evidence that
// the guest driver drained our packets (it refills descriptors after
// consuming them). This replaces the previous fixed 100 ms sleep — in
// practice the guest drains in <5 ms but the old sleep occasionally
// wasn't enough under scheduler contention, and 100 ms was a floor under
// every snapshot.
func (d *Device) QuiesceForSnapshot() {
	if d.Transport == nil {
		return
	}

	// Snapshot the conn set; we will issue per-conn opRst for each.
	d.mu.Lock()
	drained := make([]*Connection, 0, len(d.conns))
	for _, c := range d.conns {
		drained = append(drained, c)
	}
	// Clear d.conns first so any racing rxPump that wakes from the
	// subsequent c.conn.Close() cannot re-send a duplicate opShutdown
	// (it would hit a now-unknown connKey in handleData anyway, but
	// clearing up front makes the sequence observable).
	d.conns = map[connKey]*Connection{}
	d.mu.Unlock()

	rxQ := d.Transport.Queue(0)
	eventQ := d.Transport.Queue(2)

	// Track the guest-visible descriptor position on RX *before* we
	// inject anything, so we can verify drain afterwards.
	var rxBaselineAvail uint16
	rxPresent := rxQ != nil && rxQ.Ready
	if rxPresent {
		if av, err := rxQ.AvailIdx(); err == nil {
			rxBaselineAvail = av
		} else {
			rxPresent = false
		}
	}

	signalNeeded := false

	// (1) Per-conn opRst on RX queue.
	rstInjected := 0
	rstFailed := 0
	for _, c := range drained {
		if !rxPresent {
			break
		}
		// Direction: host -> guest, so src=Host/hostPort, dst=Guest/guestPort.
		if d.injectPkt(rxQ, HostCID, GuestCID, c.hostPort, c.guestPort, opReset) {
			signalNeeded = true
			rstInjected++
		} else {
			rstFailed++
		}
	}
	gclog.VMM.Debug("vsock quiesce rst injected",
		"injected", rstInjected,
		"failed", rstFailed,
		"total_conns", len(drained),
	)

	// (2) TRANSPORT_RESET on event queue.
	if eventQ != nil && eventQ.Ready {
		event := make([]byte, 4)
		binary.LittleEndian.PutUint32(event, 0) // VIRTIO_VSOCK_EVENT_TRANSPORT_RESET = 0
		wrote := false
		_ = eventQ.IterAvail(func(head uint16) {
			chain, err := eventQ.WalkChain(head)
			if err != nil || len(chain) == 0 {
				_ = eventQ.PushUsed(uint32(head), 0)
				return
			}
			if err := eventQ.GuestWrite(chain[0].Addr, event); err != nil {
				_ = eventQ.PushUsed(uint32(head), 0)
				return
			}
			_ = eventQ.PushUsed(uint32(head), 4)
			wrote = true
		})
		if wrote {
			signalNeeded = true
		}
	}

	if signalNeeded {
		// Virtio spec §4.2.2.5: ISR bit 0 = used buffer notification.
		// Without this the guest's virtio-mmio ISR treats the IRQ as
		// spurious and never drains the queues.
		d.Transport.SetInterruptStat(1)
		d.Transport.SignalIRQ(true)
	}

	// Release the host-side pipes. Close each guestConn so the rxPump
	// goroutine sees EOF and sends opShutdown to the guest's RX queue.
	// We then wait (bounded) for the rxPump goroutines to exit, which
	// guarantees the opShutdown landed before we proceed to Pause.
	var rxWG sync.WaitGroup
	gclog.VMM.Debug("vsock quiesce closing conns", "count", len(drained))
	for _, c := range drained {
		rxWG.Add(1)
		go func(conn net.Conn) {
			defer rxWG.Done()
			conn.Close()
		}(c.conn)
	}
	// Wait up to 200ms for rxPumps to close and send opShutdown.
	waitDone := make(chan struct{})
	go func() { rxWG.Wait(); close(waitDone) }()
	select {
	case <-waitDone:
		gclog.VMM.Debug("vsock quiesce rxPumps all closed")
	case <-time.After(200 * time.Millisecond):
		gclog.VMM.Warn("vsock quiesce rxPump close timeout")
	}

	gclog.VMM.Debug("vsock quiesce signaled",
		"conns", len(drained),
		"rx_present", rxPresent,
		"event_present", eventQ != nil && eventQ.Ready,
		"rx_baseline_avail", rxBaselineAvail,
	)

	// The RX drain is only meaningful when we actually injected RST packets
	// onto the RX queue (i.e. there were active connections). TRANSPORT_RESET
	// goes to the event queue (index 2) which is independent — the RX avail
	// index won't advance for an event-only signal, so we skip the drain when
	// drained is empty to avoid an unconditional 250ms timeout on every
	// snapshot of an idle VM.
	if !rxPresent || len(drained) == 0 {
		return
	}

	// Wait for the guest to process what we just enqueued. Evidence:
	// the guest replenishes RX descriptors after consuming them, so the
	// avail ring's idx advances. Bounded at 250 ms.
	drainDeadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(drainDeadline) {
		av, err := rxQ.AvailIdx()
		if err != nil {
			break
		}
		if uint16(av-rxBaselineAvail) > 0 {
			gclog.VMM.Debug("vsock quiesce drained",
				"delta", uint16(av-rxBaselineAvail),
				"elapsed_ms", time.Since(drainDeadline.Add(-250*time.Millisecond)).Milliseconds(),
			)
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	gclog.VMM.Warn("vsock quiesce drain timeout",
		"timeout_ms", 250,
		"rx_baseline_avail", rxBaselineAvail,
	)
}

// injectPkt synthesises a vsock control packet (header only, no payload)
// and pushes it onto the given RX queue, returning whether a descriptor
// was actually consumed. Reused from QuiesceForSnapshot so we do not
// interleave sendPkt's own IRQ signaling with the batched one above.
func (d *Device) injectPkt(rxQ *virtio.Queue, srcCID, dstCID uint64, srcPort, dstPort uint32, op uint16) bool {
	hdr := &pktHdr{
		SrcCID:   srcCID,
		DstCID:   dstCID,
		SrcPort:  srcPort,
		DstPort:  dstPort,
		Type:     1,
		Op:       op,
		BufAlloc: 65536,
	}
	payload := marshalHdr(hdr)
	ok, err := rxQ.ConsumeAvail(func(head uint16) {
		chain, walkErr := rxQ.WalkChain(head)
		if walkErr != nil {
			_ = rxQ.PushUsed(uint32(head), 0)
			return
		}
		written := uint32(0)
		remaining := payload
		for _, desc := range chain {
			if desc.Flags&virtio.DescFlagWrite == 0 {
				continue
			}
			sz := uint32(len(remaining))
			if sz > desc.Len {
				sz = desc.Len
			}
			if err := rxQ.GuestWrite(desc.Addr, remaining[:sz]); err != nil {
				break
			}
			remaining = remaining[sz:]
			written += sz
			if len(remaining) == 0 {
				break
			}
		}
		_ = rxQ.PushUsed(uint32(head), written)
	})
	if err != nil || !ok {
		return false
	}
	return true
}

// Close shuts down the vsock device: it marks the device closed, terminates
// all active guest↔host connections, and waits for in-flight rxPump goroutines
// to exit. Must be called before guest memory is unmapped, otherwise rxPump
// goroutines may sigpanic writing into freed guest RAM.
func (d *Device) Close() {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.closed = true
	d.closeOnce.Do(func() {
		if d.closeCh != nil {
			close(d.closeCh)
		}
	})
	conns := make([]*Connection, 0, len(d.conns))
	for _, c := range d.conns {
		conns = append(conns, c)
	}
	pending := make([]*pendingConn, 0, len(d.pending))
	for _, p := range d.pending {
		pending = append(pending, p)
	}
	d.conns = map[connKey]*Connection{}
	d.pending = map[uint32]*pendingConn{}
	d.mu.Unlock()

	for _, c := range conns {
		c.conn.Close()
	}
	for _, p := range pending {
		p.conn.conn.Close()
		select {
		case p.ready <- fmt.Errorf("vsock device closed"):
		default:
		}
	}
	d.rxWG.Wait()
}

func (d *Device) isClosed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closed
}

func (d *Device) allocateHostPort() uint32 {
	d.mu.Lock()
	defer d.mu.Unlock()
	for {
		port := d.nextHostPort
		d.nextHostPort++
		if d.nextHostPort == 0 {
			d.nextHostPort = 1024
		}
		if _, ok := d.pending[port]; ok {
			continue
		}
		inUse := false
		for key := range d.conns {
			if key.hostPort == port {
				inUse = true
				break
			}
		}
		if !inUse {
			return port
		}
	}
}

// DialGuest dials a vsock port inside the guest using AF_VSOCK.
func DialGuest(cid, port uint32) (net.Conn, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	sa := &unix.SockaddrVM{
		CID:  cid,
		Port: port,
	}
	if err := unix.Connect(fd, sa); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock connect cid=%d port=%d: %w", cid, port, err)
	}
	f := os.NewFile(uintptr(fd), fmt.Sprintf("vsock:%d:%d", cid, port))
	conn, err := net.FileConn(f)
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("vsock fileconn: %w", err)
	}
	return conn, nil
}

// --- helpers ---

func parseHdr(b []byte, h *pktHdr) {
	h.SrcCID = binary.LittleEndian.Uint64(b[0:])
	h.DstCID = binary.LittleEndian.Uint64(b[8:])
	h.SrcPort = binary.LittleEndian.Uint32(b[16:])
	h.DstPort = binary.LittleEndian.Uint32(b[20:])
	h.Len = binary.LittleEndian.Uint32(b[24:])
	h.Type = binary.LittleEndian.Uint16(b[28:])
	h.Op = binary.LittleEndian.Uint16(b[30:])
	h.Flags = binary.LittleEndian.Uint32(b[32:])
	h.BufAlloc = binary.LittleEndian.Uint32(b[36:])
	h.FwdCnt = binary.LittleEndian.Uint32(b[40:])
}

func marshalHdr(h *pktHdr) []byte {
	b := make([]byte, hdrSize)
	binary.LittleEndian.PutUint64(b[0:], h.SrcCID)
	binary.LittleEndian.PutUint64(b[8:], h.DstCID)
	binary.LittleEndian.PutUint32(b[16:], h.SrcPort)
	binary.LittleEndian.PutUint32(b[20:], h.DstPort)
	binary.LittleEndian.PutUint32(b[24:], h.Len)
	binary.LittleEndian.PutUint16(b[28:], h.Type)
	binary.LittleEndian.PutUint16(b[30:], h.Op)
	binary.LittleEndian.PutUint32(b[32:], h.Flags)
	binary.LittleEndian.PutUint32(b[36:], h.BufAlloc)
	binary.LittleEndian.PutUint32(b[40:], h.FwdCnt)
	return b
}
