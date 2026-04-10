//go:build linux

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
	}
	d.Transport = virtio.NewTransport(d, mem, basePA, irq, dirty, irqFn)
	return d
}

func (d *Device) DeviceID() uint32       { return 19 } // VIRTIO_ID_VSOCK
func (d *Device) DeviceFeatures() uint64 { return 0 }
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
	return d.processTX(q)
}

func (d *Device) processTX(q *virtio.Queue) bool {
	processed := false
	if err := q.IterAvail(func(head uint16) {
		processed = true
		chain, err := q.WalkChain(head)
		if err != nil {
			gclog.VMM.Warn("virtio-vsock invalid TX descriptor chain", "head", head, "error", err)
			_ = q.PushUsedLocked(uint32(head), 0)
			return
		}
		if len(chain) == 0 {
			return
		}
		// Read header
		if chain[0].Len < hdrSize {
			_ = q.PushUsedLocked(uint32(head), 0)
			return
		}
		hdrBuf := make([]byte, hdrSize)
		if err := q.GuestRead(chain[0].Addr, hdrBuf); err != nil {
			gclog.VMM.Warn("virtio-vsock TX header read failed", "head", head, "error", err)
			_ = q.PushUsedLocked(uint32(head), 0)
			return
		}
		var hdr pktHdr
		parseHdr(hdrBuf, &hdr)

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
		_ = q.PushUsedLocked(uint32(head), 0)
	}); err != nil {
		gclog.VMM.Warn("virtio-vsock TX queue iteration failed", "error", err)
	}
	return processed
}

func (d *Device) handleConnect(hdr *pktHdr, q *virtio.Queue) {
	if d.listenFn == nil {
		d.sendReset(hdr)
		return
	}
	conn, err := d.listenFn(hdr.DstPort)
	if err != nil {
		d.sendReset(hdr)
		return
	}
	c := &Connection{
		guestPort: hdr.SrcPort,
		hostPort:  hdr.DstPort,
		conn:      conn,
		device:    d,
	}
	d.mu.Lock()
	d.conns[connKey{guestPort: hdr.SrcPort, hostPort: hdr.DstPort}] = c
	d.mu.Unlock()

	// Send RESPONSE
	d.sendPkt(hdr.DstCID, hdr.SrcCID, hdr.DstPort, hdr.SrcPort, opResponse, nil)

	// Pump data from host connection → guest
	go c.rxPump(q)
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
	pending.ready <- nil
	go pending.conn.rxPump(q)
}

func (d *Device) handleData(hdr *pktHdr, chain []virtio.Desc, q *virtio.Queue) {
	d.mu.Lock()
	c := d.conns[connKey{guestPort: hdr.SrcPort, hostPort: hdr.DstPort}]
	d.mu.Unlock()
	if c == nil || hdr.Len == 0 {
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
	c.conn.Write(data) //nolint:errcheck
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
	d.mu.Unlock()
	if c != nil {
		c.conn.Close()
	}
	if pending != nil {
		pending.conn.conn.Close()
		pending.ready <- fmt.Errorf("vsock connection rejected by guest")
	}
}

func (d *Device) sendReset(hdr *pktHdr) {
	d.sendPkt(hdr.DstCID, hdr.SrcCID, hdr.DstPort, hdr.SrcPort, opReset, nil)
}

func (d *Device) sendPkt(srcCID, dstCID uint64, srcPort, dstPort uint32, op uint16, data []byte) {
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

	rxQ := d.Transport.Queue(0)
	if rxQ == nil || !rxQ.Ready {
		return
	}
	ok, err := rxQ.ConsumeAvail(func(head uint16) {
		chain, walkErr := rxQ.WalkChain(head)
		if walkErr != nil {
			gclog.VMM.Warn("virtio-vsock invalid RX descriptor chain", "head", head, "error", walkErr)
			_ = rxQ.PushUsedLocked(uint32(head), 0)
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
		_ = rxQ.PushUsedLocked(uint32(head), written)
	})
	if err != nil {
		gclog.VMM.Warn("virtio-vsock RX queue consume failed", "error", err)
		return
	}
	if !ok {
		return
	}
	d.Transport.SetInterruptStat(1)
	d.Transport.SignalIRQ(true)
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
		if n > 0 {
			c.device.sendPkt(HostCID, GuestCID, c.hostPort, c.guestPort, opRW, buf[:n])
		}
		if err == io.EOF || err != nil {
			c.device.sendPkt(HostCID, GuestCID, c.hostPort, c.guestPort, opShutdown, nil)
			return
		}
	}
}

func (d *Device) Dial(port uint32) (net.Conn, error) {
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

	d.sendPkt(HostCID, GuestCID, pending.conn.hostPort, port, opRequest, nil)

	select {
	case err := <-pending.ready:
		if err != nil {
			hostConn.Close()
			return nil, err
		}
		return hostConn, nil
	case <-time.After(dialTimeout):
		d.mu.Lock()
		delete(d.pending, pending.conn.hostPort)
		d.mu.Unlock()
		hostConn.Close()
		deviceConn.Close()
		return nil, fmt.Errorf("vsock dial timeout for guest port %d", port)
	}
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
