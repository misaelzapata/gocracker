package vsock

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/gocracker/gocracker/internal/virtio"
)

// ---------- helpers for setting up in-memory virtqueues ----------

const (
	testMemSz = 256 * 1024 // 256 KiB
	descB     = uint64(0x0000)
	availB    = uint64(0x1000)
	usedB     = uint64(0x2000)
	dataB     = uint64(0x4000)
)

func writeDescE(mem []byte, base uint64, idx uint16, addr uint64, length uint32, flags uint16, next uint16) {
	off := base + uint64(idx)*16
	binary.LittleEndian.PutUint64(mem[off:], addr)
	binary.LittleEndian.PutUint32(mem[off+8:], length)
	binary.LittleEndian.PutUint16(mem[off+12:], flags)
	binary.LittleEndian.PutUint16(mem[off+14:], next)
}

func writeAvailE(mem []byte, base uint64, ringIdx uint16, descHead uint16) {
	off := base + 4 + uint64(ringIdx)*2
	binary.LittleEndian.PutUint16(mem[off:], descHead)
	binary.LittleEndian.PutUint16(mem[base+2:], ringIdx+1)
}

func newTestDev(mem []byte, listenFn func(uint32) (net.Conn, error)) *Device {
	d := &Device{
		mem:          mem,
		conns:        make(map[connKey]*Connection),
		pending:      make(map[uint32]*pendingConn),
		nextHostPort: 1024,
		listenFn:     listenFn,
	}
	d.Transport = virtio.NewTransport(d, mem, 0x10000, 5, nil, nil)
	return d
}

func setupRXQ(mem []byte, d *Device) {
	rxQ := d.Transport.Queue(0)
	rxQ.DescAddr = descB
	rxQ.DriverAddr = availB
	rxQ.DeviceAddr = usedB
	rxQ.Ready = true
	rxQ.Size = 256

	for i := uint16(0); i < 16; i++ {
		addr := dataB + uint64(i)*4096
		writeDescE(mem, descB, i, addr, 4096, virtio.DescFlagWrite, 0)
		writeAvailE(mem, availB, i, i)
	}
}

const (
	txDescB  = uint64(0x8000)
	txAvailB = uint64(0x9000)
	txUsedB  = uint64(0xA000)
	txDataB  = uint64(0xC000)
)

func setupTXQ(mem []byte, d *Device) *virtio.Queue {
	txQ := d.Transport.Queue(1)
	txQ.DescAddr = txDescB
	txQ.DriverAddr = txAvailB
	txQ.DeviceAddr = txUsedB
	txQ.Ready = true
	txQ.Size = 256
	return txQ
}

func injectTX(mem []byte, txQ *virtio.Queue, idx uint16, hdr *pktHdr, payload []byte) {
	hdrBytes := marshalHdr(hdr)
	if len(payload) == 0 {
		addr := txDataB + uint64(idx)*4096
		copy(mem[addr:], hdrBytes)
		writeDescE(mem, txDescB, idx, addr, uint32(len(hdrBytes)), 0, 0)
	} else {
		hdrAddr := txDataB + uint64(idx)*4096
		payAddr := hdrAddr + uint64(hdrSize)
		copy(mem[hdrAddr:], hdrBytes)
		copy(mem[payAddr:], payload)
		writeDescE(mem, txDescB, idx, hdrAddr, uint32(len(hdrBytes)), virtio.DescFlagNext, idx+1)
		writeDescE(mem, txDescB, idx+1, payAddr, uint32(len(payload)), 0, 0)
	}
	writeAvailE(mem, txAvailB, idx, idx)
}

// ---------- handleConnect full lifecycle ----------

func TestHandleConnectNilListenFnSendsReset(t *testing.T) {
	mem := make([]byte, testMemSz)
	d := newTestDev(mem, nil)
	setupRXQ(mem, d)
	txQ := setupTXQ(mem, d)

	hdr := &pktHdr{
		SrcCID: GuestCID, DstCID: HostCID,
		SrcPort: 1000, DstPort: 52,
		Op: opRequest, Type: 1,
	}
	injectTX(mem, txQ, 0, hdr, nil)
	d.processTX(txQ)

	usedIdx := binary.LittleEndian.Uint16(mem[usedB+2:])
	if usedIdx == 0 {
		t.Fatal("expected RST packet in RX queue but used ring is empty")
	}
}

func TestHandleConnectListenFnErrorSendsReset(t *testing.T) {
	mem := make([]byte, testMemSz)
	d := newTestDev(mem, func(port uint32) (net.Conn, error) {
		return nil, fmt.Errorf("refused")
	})
	setupRXQ(mem, d)
	txQ := setupTXQ(mem, d)

	hdr := &pktHdr{
		SrcCID: GuestCID, DstCID: HostCID,
		SrcPort: 1000, DstPort: 52,
		Op: opRequest, Type: 1,
	}
	injectTX(mem, txQ, 0, hdr, nil)
	d.processTX(txQ)

	usedIdx := binary.LittleEndian.Uint16(mem[usedB+2:])
	if usedIdx == 0 {
		t.Fatal("expected RST in RX queue on listenFn error")
	}
}

func TestHandleConnectSuccessRegistersConn(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	mem := make([]byte, testMemSz)
	d := newTestDev(mem, func(port uint32) (net.Conn, error) {
		return clientConn, nil
	})
	setupRXQ(mem, d)
	txQ := setupTXQ(mem, d)

	hdr := &pktHdr{
		SrcCID: GuestCID, DstCID: HostCID,
		SrcPort: 1000, DstPort: 52,
		Op: opRequest, Type: 1,
	}
	injectTX(mem, txQ, 0, hdr, nil)
	d.processTX(txQ)

	d.mu.Lock()
	connCount := len(d.conns)
	d.mu.Unlock()
	if connCount != 1 {
		t.Fatalf("expected 1 connection, got %d", connCount)
	}

	usedIdx := binary.LittleEndian.Uint16(mem[usedB+2:])
	if usedIdx == 0 {
		t.Fatal("expected RESPONSE packet in RX queue")
	}
}

// ---------- handleData with real data flow ----------

func TestHandleDataWritesToHostConn(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	mem := make([]byte, testMemSz)
	d := newTestDev(mem, nil)
	setupRXQ(mem, d)
	txQ := setupTXQ(mem, d)

	d.mu.Lock()
	d.conns[connKey{guestPort: 1000, hostPort: 52}] = &Connection{
		guestPort: 1000, hostPort: 52, conn: clientConn, device: d,
	}
	d.mu.Unlock()

	payload := []byte("hello from guest")
	hdr := &pktHdr{
		SrcCID: GuestCID, DstCID: HostCID,
		SrcPort: 1000, DstPort: 52,
		Len: uint32(len(payload)), Op: opRW, Type: 1,
	}
	injectTX(mem, txQ, 0, hdr, payload)

	var received []byte
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 256)
		n, _ := serverConn.Read(buf)
		received = buf[:n]
		close(done)
	}()

	d.processTX(txQ)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for data")
	}

	if string(received) != "hello from guest" {
		t.Fatalf("received = %q, want %q", received, "hello from guest")
	}
}

// ---------- handleDisconnect with active connection ----------

func TestHandleDisconnectRemovesConnAndSendsReset(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()

	mem := make([]byte, testMemSz)
	d := newTestDev(mem, nil)
	setupRXQ(mem, d)
	txQ := setupTXQ(mem, d)

	key := connKey{guestPort: 1000, hostPort: 52}
	d.mu.Lock()
	d.conns[key] = &Connection{
		guestPort: 1000, hostPort: 52, conn: clientConn, device: d,
	}
	d.mu.Unlock()

	hdr := &pktHdr{
		SrcCID: GuestCID, DstCID: HostCID,
		SrcPort: 1000, DstPort: 52,
		Op: opShutdown, Type: 1,
	}
	injectTX(mem, txQ, 0, hdr, nil)
	d.processTX(txQ)

	d.mu.Lock()
	_, exists := d.conns[key]
	d.mu.Unlock()
	if exists {
		t.Fatal("connection should be removed after disconnect")
	}

	// RST should be sent back
	usedIdx := binary.LittleEndian.Uint16(mem[usedB+2:])
	if usedIdx == 0 {
		t.Fatal("expected RST in RX queue after disconnect")
	}
}

func TestHandleDisconnectWithPendingReportsError(t *testing.T) {
	mem := make([]byte, testMemSz)
	d := newTestDev(mem, nil)
	setupRXQ(mem, d)
	txQ := setupTXQ(mem, d)

	_, pipeConn := net.Pipe()
	defer pipeConn.Close()

	pendingReady := make(chan error, 1)
	d.mu.Lock()
	d.pending[52] = &pendingConn{
		conn:  &Connection{guestPort: 1000, hostPort: 52, conn: pipeConn, device: d},
		ready: pendingReady,
	}
	d.mu.Unlock()

	hdr := &pktHdr{
		SrcCID: GuestCID, DstCID: HostCID,
		SrcPort: 1000, DstPort: 52,
		Op: opReset, Type: 1,
	}
	injectTX(mem, txQ, 0, hdr, nil)
	d.processTX(txQ)

	select {
	case err := <-pendingReady:
		if err == nil {
			t.Fatal("expected error for rejected pending connection")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for pending conn error")
	}
}

// ---------- handleResponse completing dial ----------

func TestHandleResponseCompletesDialHandshake(t *testing.T) {
	mem := make([]byte, testMemSz)
	d := newTestDev(mem, nil)
	setupRXQ(mem, d)
	txQ := setupTXQ(mem, d)

	_, pipeConn := net.Pipe()
	defer pipeConn.Close()

	pendingReady := make(chan error, 1)
	d.mu.Lock()
	d.pending[1024] = &pendingConn{
		conn:  &Connection{guestPort: 1234, hostPort: 1024, conn: pipeConn, device: d},
		ready: pendingReady,
	}
	d.mu.Unlock()

	hdr := &pktHdr{
		SrcCID: GuestCID, DstCID: HostCID,
		SrcPort: 1234, DstPort: 1024,
		Op: opResponse, Type: 1,
	}
	injectTX(mem, txQ, 0, hdr, nil)
	d.processTX(txQ)

	select {
	case err := <-pendingReady:
		if err != nil {
			t.Fatalf("pending ready error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}

	d.mu.Lock()
	_, exists := d.conns[connKey{guestPort: 1234, hostPort: 1024}]
	d.mu.Unlock()
	if !exists {
		t.Fatal("connection not registered after response")
	}
}

// ---------- sendPkt ----------

func TestSendPktNoRXQueueNoPanic(t *testing.T) {
	mem := make([]byte, testMemSz)
	d := newTestDev(mem, nil)
	// Queue 0 not ready
	d.sendPkt(HostCID, GuestCID, 1024, 1000, opRW, []byte("data"), 0)
}

func TestSendPktWritesToRXQueueWithPayload(t *testing.T) {
	mem := make([]byte, testMemSz)
	d := newTestDev(mem, nil)
	setupRXQ(mem, d)

	d.sendPkt(HostCID, GuestCID, 1024, 1000, opRW, []byte("test data"), 0)

	usedIdx := binary.LittleEndian.Uint16(mem[usedB+2:])
	if usedIdx != 1 {
		t.Fatalf("used ring idx = %d, want 1", usedIdx)
	}

	var rxHdr pktHdr
	hdrBuf := make([]byte, hdrSize)
	copy(hdrBuf, mem[dataB:dataB+hdrSize])
	parseHdr(hdrBuf, &rxHdr)
	if rxHdr.SrcCID != HostCID || rxHdr.DstCID != GuestCID {
		t.Fatalf("CIDs wrong: src=%d dst=%d", rxHdr.SrcCID, rxHdr.DstCID)
	}
	if rxHdr.Op != opRW {
		t.Fatalf("Op = %d, want %d", rxHdr.Op, opRW)
	}
	if rxHdr.Len != 9 {
		t.Fatalf("Len = %d, want 9", rxHdr.Len)
	}
	if rxHdr.BufAlloc != 65536 {
		t.Fatalf("BufAlloc = %d, want 65536", rxHdr.BufAlloc)
	}

	payload := make([]byte, rxHdr.Len)
	copy(payload, mem[dataB+hdrSize:])
	if string(payload) != "test data" {
		t.Fatalf("payload = %q, want %q", payload, "test data")
	}
}

func TestSendPktEmptyPayload(t *testing.T) {
	mem := make([]byte, testMemSz)
	d := newTestDev(mem, nil)
	setupRXQ(mem, d)

	d.sendPkt(HostCID, GuestCID, 1024, 1000, opShutdown, nil, 0)

	usedIdx := binary.LittleEndian.Uint16(mem[usedB+2:])
	if usedIdx != 1 {
		t.Fatalf("used ring idx = %d, want 1", usedIdx)
	}
}

// ---------- sendReset ----------

func TestSendResetSwapsAddresses(t *testing.T) {
	mem := make([]byte, testMemSz)
	d := newTestDev(mem, nil)
	setupRXQ(mem, d)

	hdr := &pktHdr{
		SrcCID: GuestCID, DstCID: HostCID,
		SrcPort: 1000, DstPort: 52,
	}
	d.sendReset(hdr)

	var rxHdr pktHdr
	hdrBuf := make([]byte, hdrSize)
	copy(hdrBuf, mem[dataB:dataB+hdrSize])
	parseHdr(hdrBuf, &rxHdr)
	if rxHdr.Op != opReset {
		t.Fatalf("Op = %d, want %d", rxHdr.Op, opReset)
	}
	if rxHdr.SrcCID != HostCID || rxHdr.DstCID != GuestCID {
		t.Fatalf("CIDs not swapped: src=%d dst=%d", rxHdr.SrcCID, rxHdr.DstCID)
	}
	if rxHdr.SrcPort != 52 || rxHdr.DstPort != 1000 {
		t.Fatalf("ports not swapped: src=%d dst=%d", rxHdr.SrcPort, rxHdr.DstPort)
	}
}

// ---------- rxPump ----------

func TestRxPumpForwardsDataAndCleansUp(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()

	mem := make([]byte, testMemSz)
	d := newTestDev(mem, nil)
	setupRXQ(mem, d)

	c := &Connection{
		guestPort: 1000, hostPort: 52,
		conn: clientConn, device: d,
	}
	d.mu.Lock()
	d.conns[connKey{guestPort: 1000, hostPort: 52}] = c
	d.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.rxPump(d.Transport.Queue(0))
	}()

	serverConn.Write([]byte("pump test"))
	time.Sleep(50 * time.Millisecond)
	serverConn.Close()
	wg.Wait()

	d.mu.Lock()
	_, exists := d.conns[connKey{guestPort: 1000, hostPort: 52}]
	d.mu.Unlock()
	if exists {
		t.Fatal("rxPump should have cleaned up the connection")
	}
}

// ---------- processTX credit opcodes ----------

func TestProcessTXCreditOpcodes(t *testing.T) {
	mem := make([]byte, testMemSz)
	d := newTestDev(mem, nil)
	txQ := setupTXQ(mem, d)

	hdr := &pktHdr{
		SrcCID: GuestCID, DstCID: HostCID,
		SrcPort: 1000, DstPort: 52,
		Op: opCreditUpdate, Type: 1,
	}
	injectTX(mem, txQ, 0, hdr, nil)
	processed := d.processTX(txQ)
	if !processed {
		t.Fatal("processTX should report processed=true")
	}
}

// ---------- processTX short header ----------

func TestProcessTXShortHeaderSkips(t *testing.T) {
	mem := make([]byte, testMemSz)
	d := newTestDev(mem, nil)
	txQ := setupTXQ(mem, d)

	addr := txDataB
	writeDescE(mem, txDescB, 0, addr, hdrSize-1, 0, 0)
	writeAvailE(mem, txAvailB, 0, 0)

	d.processTX(txQ) // Should not panic
}

// ---------- NewDevice with real constructor ----------

func TestNewDeviceRealConstructor(t *testing.T) {
	mem := make([]byte, testMemSz)
	d := NewDevice(mem, 0x10000, 5, func(port uint32) (net.Conn, error) {
		return nil, fmt.Errorf("not implemented")
	}, nil, nil)
	if d == nil {
		t.Fatal("NewDevice returned nil")
	}
	if d.DeviceID() != 19 {
		t.Fatalf("DeviceID = %d, want 19", d.DeviceID())
	}
	if d.nextHostPort != 1024 {
		t.Fatalf("nextHostPort = %d, want 1024", d.nextHostPort)
	}
}

// ---------- processTX empty chain ----------

func TestProcessTXEmptyQueue(t *testing.T) {
	mem := make([]byte, testMemSz)
	d := newTestDev(mem, nil)
	txQ := setupTXQ(mem, d)
	// Don't inject anything
	txQ.LastAvail = 0
	binary.LittleEndian.PutUint16(mem[txAvailB+2:], 0) // avail.idx = 0
	processed := d.processTX(txQ)
	if processed {
		t.Fatal("processTX on empty queue should return false")
	}
}

// ---------- HandleQueue for TX queue ----------

func TestHandleQueueTXProcessesPacket(t *testing.T) {
	mem := make([]byte, testMemSz)
	d := newTestDev(mem, nil)
	setupRXQ(mem, d)
	txQ := setupTXQ(mem, d)

	hdr := &pktHdr{
		SrcCID: GuestCID, DstCID: HostCID,
		SrcPort: 1000, DstPort: 52,
		Op: opCreditUpdate, Type: 1,
	}
	injectTX(mem, txQ, 0, hdr, nil)

	// HandleQueue with idx=1 should process TX
	d.HandleQueue(1, txQ)
}

func TestHandleQueueNotifyTXReturnsTrue(t *testing.T) {
	mem := make([]byte, testMemSz)
	d := newTestDev(mem, nil)
	txQ := setupTXQ(mem, d)

	hdr := &pktHdr{
		SrcCID: GuestCID, DstCID: HostCID,
		SrcPort: 1000, DstPort: 52,
		Op: opCreditRequest, Type: 1,
	}
	injectTX(mem, txQ, 0, hdr, nil)

	if !d.HandleQueueNotify(1, txQ) {
		t.Fatal("HandleQueueNotify(1) = false, want true when packets processed")
	}
}

// ---------- Multiple sendPkt in sequence ----------

func TestSendPktMultiplePackets(t *testing.T) {
	mem := make([]byte, testMemSz)
	d := newTestDev(mem, nil)
	setupRXQ(mem, d)

	for i := 0; i < 5; i++ {
		d.sendPkt(HostCID, GuestCID, uint32(1024+i), 1000, opRW, []byte(fmt.Sprintf("pkt%d", i)), 0)
	}

	usedIdx := binary.LittleEndian.Uint16(mem[usedB+2:])
	if usedIdx != 5 {
		t.Fatalf("used ring idx = %d, want 5 after 5 packets", usedIdx)
	}
}
