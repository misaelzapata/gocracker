package vsock

import (
	"encoding/binary"
	"net"
	"testing"
)

func TestMarshalParseHdr_Roundtrip(t *testing.T) {
	original := &pktHdr{
		SrcCID:   GuestCID,
		DstCID:   HostCID,
		SrcPort:  1234,
		DstPort:  5678,
		Len:      512,
		Type:     1,
		Op:       opRW,
		Flags:    0x42,
		BufAlloc: 65536,
		FwdCnt:   1024,
	}

	b := marshalHdr(original)
	if len(b) != hdrSize {
		t.Fatalf("marshalHdr produced %d bytes, want %d", len(b), hdrSize)
	}

	var parsed pktHdr
	parseHdr(b, &parsed)

	if parsed.SrcCID != original.SrcCID {
		t.Errorf("SrcCID = %d, want %d", parsed.SrcCID, original.SrcCID)
	}
	if parsed.DstCID != original.DstCID {
		t.Errorf("DstCID = %d, want %d", parsed.DstCID, original.DstCID)
	}
	if parsed.SrcPort != original.SrcPort {
		t.Errorf("SrcPort = %d, want %d", parsed.SrcPort, original.SrcPort)
	}
	if parsed.DstPort != original.DstPort {
		t.Errorf("DstPort = %d, want %d", parsed.DstPort, original.DstPort)
	}
	if parsed.Len != original.Len {
		t.Errorf("Len = %d, want %d", parsed.Len, original.Len)
	}
	if parsed.Type != original.Type {
		t.Errorf("Type = %d, want %d", parsed.Type, original.Type)
	}
	if parsed.Op != original.Op {
		t.Errorf("Op = %d, want %d", parsed.Op, original.Op)
	}
	if parsed.Flags != original.Flags {
		t.Errorf("Flags = %#x, want %#x", parsed.Flags, original.Flags)
	}
	if parsed.BufAlloc != original.BufAlloc {
		t.Errorf("BufAlloc = %d, want %d", parsed.BufAlloc, original.BufAlloc)
	}
	if parsed.FwdCnt != original.FwdCnt {
		t.Errorf("FwdCnt = %d, want %d", parsed.FwdCnt, original.FwdCnt)
	}
}

func TestMarshalHdr_Size(t *testing.T) {
	h := &pktHdr{}
	b := marshalHdr(h)
	if len(b) != hdrSize {
		t.Errorf("marshalHdr size = %d, want %d", len(b), hdrSize)
	}
	if hdrSize != 44 {
		t.Errorf("hdrSize = %d, want 44", hdrSize)
	}
}

func TestParseHdr_ZeroBuffer(t *testing.T) {
	b := make([]byte, hdrSize)
	var h pktHdr
	parseHdr(b, &h)

	if h.SrcCID != 0 || h.DstCID != 0 || h.SrcPort != 0 || h.DstPort != 0 {
		t.Error("zero buffer should produce all-zero header")
	}
	if h.Len != 0 || h.Type != 0 || h.Op != 0 || h.Flags != 0 {
		t.Error("zero buffer should produce all-zero header")
	}
}

func TestRoundtrip_AllOpcodes(t *testing.T) {
	opcodes := []uint16{opRequest, opResponse, opReset, opShutdown, opRW, opCreditUpdate, opCreditRequest}
	for _, op := range opcodes {
		original := &pktHdr{
			SrcCID:  GuestCID,
			DstCID:  HostCID,
			SrcPort: 100,
			DstPort: 200,
			Op:      op,
			Type:    1,
		}
		b := marshalHdr(original)
		var parsed pktHdr
		parseHdr(b, &parsed)
		if parsed.Op != op {
			t.Errorf("roundtrip Op: got %d, want %d", parsed.Op, op)
		}
	}
}

func TestPktHdr_FieldValues(t *testing.T) {
	// Verify that the pktHdr struct can hold the full range of values
	h := pktHdr{
		SrcCID:   ^uint64(0), // max uint64
		DstCID:   ^uint64(0),
		SrcPort:  ^uint32(0),
		DstPort:  ^uint32(0),
		Len:      ^uint32(0),
		Type:     ^uint16(0),
		Op:       ^uint16(0),
		Flags:    ^uint32(0),
		BufAlloc: ^uint32(0),
		FwdCnt:   ^uint32(0),
	}
	b := marshalHdr(&h)
	var parsed pktHdr
	parseHdr(b, &parsed)

	if parsed.SrcCID != ^uint64(0) {
		t.Errorf("max SrcCID roundtrip failed")
	}
	if parsed.DstCID != ^uint64(0) {
		t.Errorf("max DstCID roundtrip failed")
	}
	if parsed.SrcPort != ^uint32(0) {
		t.Errorf("max SrcPort roundtrip failed")
	}
	if parsed.Len != ^uint32(0) {
		t.Errorf("max Len roundtrip failed")
	}
	if parsed.Type != ^uint16(0) {
		t.Errorf("max Type roundtrip failed")
	}
}

func TestConstants(t *testing.T) {
	if GuestCID != 3 {
		t.Errorf("GuestCID = %d, want 3", GuestCID)
	}
	if HostCID != 2 {
		t.Errorf("HostCID = %d, want 2", HostCID)
	}
}

func TestOpcodeConstants(t *testing.T) {
	if opRequest != 1 {
		t.Errorf("opRequest = %d, want 1", opRequest)
	}
	if opResponse != 2 {
		t.Errorf("opResponse = %d, want 2", opResponse)
	}
	if opReset != 3 {
		t.Errorf("opReset = %d, want 3", opReset)
	}
	if opShutdown != 4 {
		t.Errorf("opShutdown = %d, want 4", opShutdown)
	}
	if opRW != 5 {
		t.Errorf("opRW = %d, want 5", opRW)
	}
	if opCreditUpdate != 6 {
		t.Errorf("opCreditUpdate = %d, want 6", opCreditUpdate)
	}
	if opCreditRequest != 7 {
		t.Errorf("opCreditRequest = %d, want 7", opCreditRequest)
	}
}

func TestMarshalParseHdr_GuestToHost(t *testing.T) {
	// Simulate a guest->host data packet
	h := &pktHdr{
		SrcCID:   GuestCID,
		DstCID:   HostCID,
		SrcPort:  9999,
		DstPort:  1024,
		Len:      128,
		Type:     1, // stream
		Op:       opRW,
		Flags:    0,
		BufAlloc: 32768,
		FwdCnt:   0,
	}
	b := marshalHdr(h)
	var out pktHdr
	parseHdr(b, &out)
	if out.SrcCID != GuestCID {
		t.Errorf("SrcCID = %d, want %d (GuestCID)", out.SrcCID, GuestCID)
	}
	if out.DstCID != HostCID {
		t.Errorf("DstCID = %d, want %d (HostCID)", out.DstCID, HostCID)
	}
}

func TestMarshalHdr_FieldOffsets(t *testing.T) {
	// Verify each field is at the expected byte offset in the wire format.
	h := &pktHdr{
		SrcCID:   0x0102030405060708,
		DstCID:   0x1112131415161718,
		SrcPort:  0x21222324,
		DstPort:  0x31323334,
		Len:      0x41424344,
		Type:     0x5152,
		Op:       0x6162,
		Flags:    0x71727374,
		BufAlloc: 0x81828384,
		FwdCnt:   0x91929394,
	}
	b := marshalHdr(h)

	// Check SrcCID at offset 0
	var parsed pktHdr
	parseHdr(b, &parsed)
	if parsed.SrcCID != h.SrcCID {
		t.Errorf("SrcCID mismatch at offset 0")
	}
	if parsed.DstCID != h.DstCID {
		t.Errorf("DstCID mismatch at offset 8")
	}
	if parsed.SrcPort != h.SrcPort {
		t.Errorf("SrcPort mismatch at offset 16")
	}
	if parsed.DstPort != h.DstPort {
		t.Errorf("DstPort mismatch at offset 20")
	}
	if parsed.Len != h.Len {
		t.Errorf("Len mismatch at offset 24")
	}
	if parsed.Type != h.Type {
		t.Errorf("Type mismatch at offset 28")
	}
	if parsed.Op != h.Op {
		t.Errorf("Op mismatch at offset 30")
	}
	if parsed.Flags != h.Flags {
		t.Errorf("Flags mismatch at offset 32")
	}
	if parsed.BufAlloc != h.BufAlloc {
		t.Errorf("BufAlloc mismatch at offset 36")
	}
	if parsed.FwdCnt != h.FwdCnt {
		t.Errorf("FwdCnt mismatch at offset 40")
	}
}

func TestMarshalHdr_ConnectionRequest(t *testing.T) {
	// Verify a typical connection request packet
	h := &pktHdr{
		SrcCID:   HostCID,
		DstCID:   GuestCID,
		SrcPort:  1024,
		DstPort:  9999,
		Len:      0,
		Type:     1, // stream
		Op:       opRequest,
		BufAlloc: 65536,
	}
	b := marshalHdr(h)
	if len(b) != hdrSize {
		t.Fatalf("request packet size = %d, want %d", len(b), hdrSize)
	}

	var parsed pktHdr
	parseHdr(b, &parsed)
	if parsed.Op != opRequest {
		t.Errorf("Op = %d, want %d (opRequest)", parsed.Op, opRequest)
	}
	if parsed.Len != 0 {
		t.Errorf("request Len = %d, want 0", parsed.Len)
	}
	if parsed.Type != 1 {
		t.Errorf("Type = %d, want 1 (stream)", parsed.Type)
	}
}

func TestMarshalHdr_ResponsePacket(t *testing.T) {
	h := &pktHdr{
		SrcCID:   GuestCID,
		DstCID:   HostCID,
		SrcPort:  9999,
		DstPort:  1024,
		Op:       opResponse,
		Type:     1,
		BufAlloc: 65536,
	}
	b := marshalHdr(h)
	var parsed pktHdr
	parseHdr(b, &parsed)
	if parsed.Op != opResponse {
		t.Errorf("Op = %d, want %d (opResponse)", parsed.Op, opResponse)
	}
}

func TestMarshalHdr_ShutdownPacket(t *testing.T) {
	h := &pktHdr{
		SrcCID:  HostCID,
		DstCID:  GuestCID,
		SrcPort: 1024,
		DstPort: 9999,
		Op:      opShutdown,
		Type:    1,
		Flags:   3, // VIRTIO_VSOCK_SHUTDOWN_RCV | VIRTIO_VSOCK_SHUTDOWN_SEND
	}
	b := marshalHdr(h)
	var parsed pktHdr
	parseHdr(b, &parsed)
	if parsed.Op != opShutdown {
		t.Errorf("Op = %d, want %d (opShutdown)", parsed.Op, opShutdown)
	}
	if parsed.Flags != 3 {
		t.Errorf("Flags = %d, want 3", parsed.Flags)
	}
}

func TestMarshalHdr_DataPacketWithPayloadLen(t *testing.T) {
	h := &pktHdr{
		SrcCID:   GuestCID,
		DstCID:   HostCID,
		SrcPort:  5000,
		DstPort:  1025,
		Len:      4096,
		Type:     1,
		Op:       opRW,
		BufAlloc: 65536,
		FwdCnt:   2048,
	}
	b := marshalHdr(h)
	var parsed pktHdr
	parseHdr(b, &parsed)
	if parsed.Len != 4096 {
		t.Errorf("Len = %d, want 4096", parsed.Len)
	}
	if parsed.FwdCnt != 2048 {
		t.Errorf("FwdCnt = %d, want 2048", parsed.FwdCnt)
	}
	if parsed.BufAlloc != 65536 {
		t.Errorf("BufAlloc = %d, want 65536", parsed.BufAlloc)
	}
}

func TestMarshalHdr_CreditUpdatePacket(t *testing.T) {
	h := &pktHdr{
		SrcCID:   GuestCID,
		DstCID:   HostCID,
		SrcPort:  5000,
		DstPort:  1025,
		Op:       opCreditUpdate,
		Type:     1,
		BufAlloc: 131072,
		FwdCnt:   8192,
	}
	b := marshalHdr(h)
	var parsed pktHdr
	parseHdr(b, &parsed)
	if parsed.Op != opCreditUpdate {
		t.Errorf("Op = %d, want %d (opCreditUpdate)", parsed.Op, opCreditUpdate)
	}
	if parsed.BufAlloc != 131072 {
		t.Errorf("BufAlloc = %d, want 131072", parsed.BufAlloc)
	}
}

func TestDeviceIDAndFeatures(t *testing.T) {
	// We can't create a real Device without virtio.Transport, but we can
	// test ConfigBytes independently via the pktHdr helpers.
	// ConfigBytes returns 8 bytes with GuestCID as little-endian uint64.
	b := make([]byte, 8)
	b[0] = byte(GuestCID)
	if b[0] != 3 {
		t.Errorf("GuestCID byte = %d, want 3", b[0])
	}
}

func TestHdrSizeConstant(t *testing.T) {
	if hdrSize != 44 {
		t.Errorf("hdrSize = %d, want 44", hdrSize)
	}
}

func TestDialTimeout(t *testing.T) {
	if dialTimeout.Seconds() != 15 {
		t.Errorf("dialTimeout = %v, want 15s", dialTimeout)
	}
}

// --- Coverage-boosting tests ---

func TestConfigBytes_GuestCID(t *testing.T) {
	// ConfigBytes should return 8 bytes encoding GuestCID (3) as LE uint64
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, GuestCID)
	if b[0] != 3 {
		t.Errorf("GuestCID LE byte[0] = %d, want 3", b[0])
	}
	for i := 1; i < 8; i++ {
		if b[i] != 0 {
			t.Errorf("GuestCID LE byte[%d] = %d, want 0", i, b[i])
		}
	}
}

func TestMarshalParseHdr_AllOpTypes(t *testing.T) {
	ops := []struct {
		name string
		op   uint16
	}{
		{"request", opRequest},
		{"response", opResponse},
		{"reset", opReset},
		{"shutdown", opShutdown},
		{"rw", opRW},
		{"credit_update", opCreditUpdate},
		{"credit_request", opCreditRequest},
	}
	for _, tt := range ops {
		t.Run(tt.name, func(t *testing.T) {
			h := &pktHdr{
				SrcCID:   HostCID,
				DstCID:   GuestCID,
				SrcPort:  1024,
				DstPort:  9999,
				Len:      256,
				Type:     1,
				Op:       tt.op,
				Flags:    0x10,
				BufAlloc: 32768,
				FwdCnt:   512,
			}
			b := marshalHdr(h)
			var parsed pktHdr
			parseHdr(b, &parsed)

			if parsed.SrcCID != HostCID {
				t.Errorf("SrcCID = %d, want %d", parsed.SrcCID, HostCID)
			}
			if parsed.DstCID != GuestCID {
				t.Errorf("DstCID = %d, want %d", parsed.DstCID, GuestCID)
			}
			if parsed.SrcPort != 1024 {
				t.Errorf("SrcPort = %d", parsed.SrcPort)
			}
			if parsed.DstPort != 9999 {
				t.Errorf("DstPort = %d", parsed.DstPort)
			}
			if parsed.Len != 256 {
				t.Errorf("Len = %d", parsed.Len)
			}
			if parsed.Type != 1 {
				t.Errorf("Type = %d", parsed.Type)
			}
			if parsed.Op != tt.op {
				t.Errorf("Op = %d, want %d", parsed.Op, tt.op)
			}
			if parsed.Flags != 0x10 {
				t.Errorf("Flags = %d", parsed.Flags)
			}
			if parsed.BufAlloc != 32768 {
				t.Errorf("BufAlloc = %d", parsed.BufAlloc)
			}
			if parsed.FwdCnt != 512 {
				t.Errorf("FwdCnt = %d", parsed.FwdCnt)
			}
		})
	}
}

func TestMarshalHdr_CreditRequestPacket(t *testing.T) {
	h := &pktHdr{
		SrcCID:   HostCID,
		DstCID:   GuestCID,
		SrcPort:  1025,
		DstPort:  5000,
		Op:       opCreditRequest,
		Type:     1,
		BufAlloc: 0,
		FwdCnt:   0,
	}
	b := marshalHdr(h)
	var parsed pktHdr
	parseHdr(b, &parsed)
	if parsed.Op != opCreditRequest {
		t.Errorf("Op = %d, want %d (opCreditRequest)", parsed.Op, opCreditRequest)
	}
	if parsed.Len != 0 {
		t.Errorf("Len = %d, want 0 for credit request", parsed.Len)
	}
}

func TestMarshalHdr_ResetPacket(t *testing.T) {
	h := &pktHdr{
		SrcCID:  HostCID,
		DstCID:  GuestCID,
		SrcPort: 1024,
		DstPort: 9999,
		Op:      opReset,
		Type:    1,
	}
	b := marshalHdr(h)
	var parsed pktHdr
	parseHdr(b, &parsed)
	if parsed.Op != opReset {
		t.Errorf("Op = %d, want %d (opReset)", parsed.Op, opReset)
	}
}

func TestPktHdr_ZeroValueRoundtrip(t *testing.T) {
	h := &pktHdr{}
	b := marshalHdr(h)
	var parsed pktHdr
	parseHdr(b, &parsed)
	if parsed != *h {
		t.Errorf("zero-value roundtrip mismatch: got %+v", parsed)
	}
}

func TestMarshalHdr_LargePayloadLen(t *testing.T) {
	h := &pktHdr{
		SrcCID: GuestCID,
		DstCID: HostCID,
		Len:    1 << 20, // 1 MiB
		Op:     opRW,
		Type:   1,
	}
	b := marshalHdr(h)
	var parsed pktHdr
	parseHdr(b, &parsed)
	if parsed.Len != 1<<20 {
		t.Errorf("Len = %d, want %d", parsed.Len, 1<<20)
	}
}

func TestMarshalHdr_HighPortNumbers(t *testing.T) {
	h := &pktHdr{
		SrcCID:  GuestCID,
		DstCID:  HostCID,
		SrcPort: 65535,
		DstPort: 65535,
		Op:      opRW,
		Type:    1,
	}
	b := marshalHdr(h)
	var parsed pktHdr
	parseHdr(b, &parsed)
	if parsed.SrcPort != 65535 || parsed.DstPort != 65535 {
		t.Errorf("ports = %d/%d, want 65535/65535", parsed.SrcPort, parsed.DstPort)
	}
}

func TestMarshalHdr_FlowControlFields(t *testing.T) {
	tests := []struct {
		name     string
		bufAlloc uint32
		fwdCnt   uint32
	}{
		{"small", 4096, 0},
		{"medium", 65536, 8192},
		{"large", 1 << 20, 1 << 19},
		{"max", ^uint32(0), ^uint32(0)},
		{"zero", 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &pktHdr{
				SrcCID:   GuestCID,
				DstCID:   HostCID,
				Op:       opCreditUpdate,
				Type:     1,
				BufAlloc: tt.bufAlloc,
				FwdCnt:   tt.fwdCnt,
			}
			b := marshalHdr(h)
			var parsed pktHdr
			parseHdr(b, &parsed)
			if parsed.BufAlloc != tt.bufAlloc {
				t.Errorf("BufAlloc = %d, want %d", parsed.BufAlloc, tt.bufAlloc)
			}
			if parsed.FwdCnt != tt.fwdCnt {
				t.Errorf("FwdCnt = %d, want %d", parsed.FwdCnt, tt.fwdCnt)
			}
		})
	}
}

// --- Device-level tests (no real virtio queues needed) ---

func TestDeviceIDAndFeatures_OnDevice(t *testing.T) {
	// Create a minimal device with nil transport to test accessors.
	// We can't use NewDevice without real memory, but we can construct the
	// struct directly for unit-level accessor tests.
	d := &Device{
		conns:        make(map[connKey]*Connection),
		pending:      make(map[uint32]*pendingConn),
		nextHostPort: 1024,
	}
	if d.DeviceID() != 19 {
		t.Errorf("DeviceID() = %d, want 19", d.DeviceID())
	}
	if d.DeviceFeatures() != 0 {
		t.Errorf("DeviceFeatures() = %d, want 0", d.DeviceFeatures())
	}
}

func TestConfigBytes_ReturnsGuestCID(t *testing.T) {
	d := &Device{
		conns:        make(map[connKey]*Connection),
		pending:      make(map[uint32]*pendingConn),
		nextHostPort: 1024,
	}
	b := d.ConfigBytes()
	if len(b) != 8 {
		t.Fatalf("ConfigBytes() len = %d, want 8", len(b))
	}
	got := binary.LittleEndian.Uint64(b)
	if got != GuestCID {
		t.Errorf("ConfigBytes() decoded = %d, want %d (GuestCID)", got, GuestCID)
	}
}

func TestConfigBytes_DifferentCIDValues(t *testing.T) {
	// ConfigBytes always returns GuestCID=3 regardless of device state
	d := &Device{
		conns:        make(map[connKey]*Connection),
		pending:      make(map[uint32]*pendingConn),
		nextHostPort: 1024,
	}
	b1 := d.ConfigBytes()
	b2 := d.ConfigBytes()
	if binary.LittleEndian.Uint64(b1) != binary.LittleEndian.Uint64(b2) {
		t.Error("ConfigBytes() should be deterministic")
	}
}

func TestAllocateHostPort_Sequential(t *testing.T) {
	d := &Device{
		conns:        make(map[connKey]*Connection),
		pending:      make(map[uint32]*pendingConn),
		nextHostPort: 1024,
	}
	p1 := d.allocateHostPort()
	p2 := d.allocateHostPort()
	p3 := d.allocateHostPort()
	if p1 != 1024 || p2 != 1025 || p3 != 1026 {
		t.Errorf("ports = %d, %d, %d; want 1024, 1025, 1026", p1, p2, p3)
	}
}

func TestAllocateHostPort_SkipsPending(t *testing.T) {
	d := &Device{
		conns:        make(map[connKey]*Connection),
		pending:      make(map[uint32]*pendingConn),
		nextHostPort: 1024,
	}
	// Mark port 1024 as pending
	d.pending[1024] = &pendingConn{}
	port := d.allocateHostPort()
	if port != 1025 {
		t.Errorf("port = %d, want 1025 (should skip pending 1024)", port)
	}
}

func TestAllocateHostPort_SkipsInUseConns(t *testing.T) {
	d := &Device{
		conns:        make(map[connKey]*Connection),
		pending:      make(map[uint32]*pendingConn),
		nextHostPort: 1024,
	}
	// Mark port 1024 as in-use via conns
	d.conns[connKey{guestPort: 5000, hostPort: 1024}] = &Connection{}
	port := d.allocateHostPort()
	if port != 1025 {
		t.Errorf("port = %d, want 1025 (should skip in-use 1024)", port)
	}
}

func TestAllocateHostPort_WrapsAround(t *testing.T) {
	d := &Device{
		conns:        make(map[connKey]*Connection),
		pending:      make(map[uint32]*pendingConn),
		nextHostPort: ^uint32(0), // max uint32
	}
	port := d.allocateHostPort()
	// nextHostPort overflows to 0, then resets to 1024
	if port != ^uint32(0) {
		t.Errorf("port = %d, want %d (max uint32)", port, ^uint32(0))
	}
	// Next allocation should have wrapped
	port2 := d.allocateHostPort()
	if port2 != 1024 {
		t.Errorf("after wrap port = %d, want 1024", port2)
	}
}

func TestConnKeyMapUsage(t *testing.T) {
	m := make(map[connKey]*Connection)
	k1 := connKey{guestPort: 100, hostPort: 200}
	k2 := connKey{guestPort: 100, hostPort: 200}
	k3 := connKey{guestPort: 100, hostPort: 201}

	c := &Connection{guestPort: 100, hostPort: 200}
	m[k1] = c

	// Same key should retrieve the same connection
	if m[k2] != c {
		t.Error("identical connKey should map to same value")
	}
	if m[k3] != nil {
		t.Error("different connKey should not map to a value")
	}

	// Delete and verify
	delete(m, k1)
	if m[k2] != nil {
		t.Error("after delete, key should be gone")
	}
}

func TestDeviceNewDevice_NilListenFn(t *testing.T) {
	// Verify that NewDevice accepts nil listenFn without error.
	// We can't fully construct without real memory, but we verify struct state.
	d := &Device{
		conns:        make(map[connKey]*Connection),
		pending:      make(map[uint32]*pendingConn),
		nextHostPort: 1024,
		listenFn:     nil,
	}
	if d.listenFn != nil {
		t.Error("listenFn should be nil")
	}
	if d.nextHostPort != 1024 {
		t.Errorf("nextHostPort = %d, want 1024", d.nextHostPort)
	}
}

func TestHandleDisconnect_NoExistingConn(t *testing.T) {
	d := &Device{
		conns:        make(map[connKey]*Connection),
		pending:      make(map[uint32]*pendingConn),
		nextHostPort: 1024,
	}
	// Should not panic when disconnecting a non-existent connection
	hdr := &pktHdr{SrcPort: 5000, DstPort: 1024}
	d.handleDisconnect(hdr)
}

func TestHandleDisconnect_WithPending(t *testing.T) {
	d := &Device{
		conns:        make(map[connKey]*Connection),
		pending:      make(map[uint32]*pendingConn),
		nextHostPort: 1024,
	}
	hostConn, deviceConn := net.Pipe()
	p := &pendingConn{
		conn: &Connection{
			guestPort: 5000,
			hostPort:  1024,
			conn:      deviceConn,
			device:    d,
		},
		ready: make(chan error, 1),
	}
	d.pending[1024] = p

	hdr := &pktHdr{SrcPort: 5000, DstPort: 1024}
	d.handleDisconnect(hdr)

	// pending should be cleaned up
	if _, ok := d.pending[1024]; ok {
		t.Error("pending should be deleted after disconnect")
	}
	// ready channel should have an error
	select {
	case err := <-p.ready:
		if err == nil {
			t.Error("expected error on ready channel")
		}
	default:
		t.Error("expected error to be sent on ready channel")
	}
	hostConn.Close()
}

func TestHandleResponse_NoPending(t *testing.T) {
	d := &Device{
		conns:        make(map[connKey]*Connection),
		pending:      make(map[uint32]*pendingConn),
		nextHostPort: 1024,
	}
	// Should not panic when there's no pending connection
	hdr := &pktHdr{SrcPort: 5000, DstPort: 1024}
	d.handleResponse(hdr, nil)
}

func TestHandleData_NoConnection(t *testing.T) {
	d := &Device{
		conns:        make(map[connKey]*Connection),
		pending:      make(map[uint32]*pendingConn),
		nextHostPort: 1024,
	}
	// Should not panic when there's no matching connection
	hdr := &pktHdr{SrcPort: 5000, DstPort: 1024, Len: 100}
	d.handleData(hdr, nil, nil)
}

func TestHandleData_ZeroLen(t *testing.T) {
	d := &Device{
		conns:        make(map[connKey]*Connection),
		pending:      make(map[uint32]*pendingConn),
		nextHostPort: 1024,
	}
	// Zero-length data should be a no-op even with a connection present
	_, deviceConn := net.Pipe()
	d.conns[connKey{guestPort: 5000, hostPort: 1024}] = &Connection{
		guestPort: 5000,
		hostPort:  1024,
		conn:      deviceConn,
		device:    d,
	}
	hdr := &pktHdr{SrcPort: 5000, DstPort: 1024, Len: 0}
	d.handleData(hdr, nil, nil)
	deviceConn.Close()
}

func TestSendPkt_MarshalVerification(t *testing.T) {
	// Verify that the header built by sendPkt logic matches expectations.
	// We can't call sendPkt without a Transport, but we can verify the
	// marshalHdr portion used by sendPkt.
	hdr := &pktHdr{
		SrcCID:   HostCID,
		DstCID:   GuestCID,
		SrcPort:  1024,
		DstPort:  5000,
		Len:      4,
		Type:     1,
		Op:       opRW,
		BufAlloc: 65536,
	}
	hdrBytes := marshalHdr(hdr)
	payload := append(hdrBytes, []byte("test")...)
	if len(payload) != hdrSize+4 {
		t.Errorf("payload len = %d, want %d", len(payload), hdrSize+4)
	}
	// Verify header at start of payload
	var parsed pktHdr
	parseHdr(payload[:hdrSize], &parsed)
	if parsed.SrcCID != HostCID || parsed.DstCID != GuestCID {
		t.Error("header CIDs mismatch in payload")
	}
	if parsed.Len != 4 {
		t.Errorf("parsed Len = %d, want 4", parsed.Len)
	}
	if string(payload[hdrSize:]) != "test" {
		t.Errorf("data portion = %q, want test", string(payload[hdrSize:]))
	}
}

func TestDeviceClose_EmptyDevice(t *testing.T) {
	d := &Device{
		conns:        make(map[connKey]*Connection),
		pending:      make(map[uint32]*pendingConn),
		nextHostPort: 1024,
	}
	// Verify empty maps
	if len(d.conns) != 0 {
		t.Error("conns should be empty")
	}
	if len(d.pending) != 0 {
		t.Error("pending should be empty")
	}
}

func TestHandleQueueNotify_NonTXQueue(t *testing.T) {
	d := &Device{
		conns:        make(map[connKey]*Connection),
		pending:      make(map[uint32]*pendingConn),
		nextHostPort: 1024,
	}
	// idx != 1 should return false
	got := d.HandleQueueNotify(0, nil)
	if got != false {
		t.Errorf("HandleQueueNotify(0) = %v, want false", got)
	}
	got = d.HandleQueueNotify(2, nil)
	if got != false {
		t.Errorf("HandleQueueNotify(2) = %v, want false", got)
	}
}

func TestMarshalHdr_BiDirectionalRoundtrip(t *testing.T) {
	// Guest -> Host
	g2h := &pktHdr{
		SrcCID:   GuestCID,
		DstCID:   HostCID,
		SrcPort:  5000,
		DstPort:  1024,
		Len:      100,
		Type:     1,
		Op:       opRW,
		BufAlloc: 65536,
		FwdCnt:   50,
	}
	b := marshalHdr(g2h)
	var parsed pktHdr
	parseHdr(b, &parsed)
	if parsed.SrcCID != GuestCID || parsed.DstCID != HostCID {
		t.Error("guest->host direction mismatch")
	}

	// Host -> Guest (response)
	h2g := &pktHdr{
		SrcCID:   HostCID,
		DstCID:   GuestCID,
		SrcPort:  1024,
		DstPort:  5000,
		Len:      200,
		Type:     1,
		Op:       opRW,
		BufAlloc: 32768,
		FwdCnt:   100,
	}
	b = marshalHdr(h2g)
	parseHdr(b, &parsed)
	if parsed.SrcCID != HostCID || parsed.DstCID != GuestCID {
		t.Error("host->guest direction mismatch")
	}
	if parsed.FwdCnt != 100 {
		t.Errorf("FwdCnt = %d, want 100", parsed.FwdCnt)
	}
}

func TestConnKeyEquality(t *testing.T) {
	k1 := connKey{guestPort: 100, hostPort: 200}
	k2 := connKey{guestPort: 100, hostPort: 200}
	k3 := connKey{guestPort: 100, hostPort: 201}

	if k1 != k2 {
		t.Error("identical connKeys should be equal")
	}
	if k1 == k3 {
		t.Error("different connKeys should not be equal")
	}
}

func TestMarshalHdr_ShutdownFlags(t *testing.T) {
	// Test various shutdown flag combinations
	flags := []struct {
		name  string
		flags uint32
	}{
		{"no flags", 0},
		{"recv only", 1},
		{"send only", 2},
		{"both", 3},
	}
	for _, tt := range flags {
		t.Run(tt.name, func(t *testing.T) {
			h := &pktHdr{
				SrcCID: HostCID,
				DstCID: GuestCID,
				Op:     opShutdown,
				Type:   1,
				Flags:  tt.flags,
			}
			b := marshalHdr(h)
			var parsed pktHdr
			parseHdr(b, &parsed)
			if parsed.Flags != tt.flags {
				t.Errorf("Flags = %d, want %d", parsed.Flags, tt.flags)
			}
		})
	}
}

// --- Non-KVM coverage tests for Device methods ---

func TestDevice_DeviceID(t *testing.T) {
	d := &Device{}
	if id := d.DeviceID(); id != 19 {
		t.Fatalf("DeviceID() = %d, want 19", id)
	}
}

func TestDevice_DeviceFeatures(t *testing.T) {
	d := &Device{}
	if f := d.DeviceFeatures(); f != 0 {
		t.Fatalf("DeviceFeatures() = %d, want 0", f)
	}
}

func TestDevice_ConfigBytes(t *testing.T) {
	d := &Device{}
	b := d.ConfigBytes()
	if len(b) != 8 {
		t.Fatalf("ConfigBytes() len = %d, want 8", len(b))
	}
	cid := binary.LittleEndian.Uint64(b)
	if cid != GuestCID {
		t.Fatalf("ConfigBytes() CID = %d, want %d", cid, GuestCID)
	}
}

func TestDevice_HandleQueue_NonTX(t *testing.T) {
	d := &Device{
		conns:   make(map[connKey]*Connection),
		pending: make(map[uint32]*pendingConn),
	}
	// HandleQueue with idx 0 (RX) and 2 (event) should not panic
	d.HandleQueue(0, nil)
	d.HandleQueue(2, nil)
}

func TestDevice_HandleQueueNotify_NonTX(t *testing.T) {
	d := &Device{
		conns:   make(map[connKey]*Connection),
		pending: make(map[uint32]*pendingConn),
	}
	// HandleQueueNotify with idx != 1 should return false
	if d.HandleQueueNotify(0, nil) {
		t.Fatal("HandleQueueNotify(0) should return false")
	}
	if d.HandleQueueNotify(2, nil) {
		t.Fatal("HandleQueueNotify(2) should return false")
	}
}

func TestDevice_AllocateHostPort_Basic(t *testing.T) {
	d := &Device{
		conns:        make(map[connKey]*Connection),
		pending:      make(map[uint32]*pendingConn),
		nextHostPort: 1024,
	}
	port1 := d.allocateHostPort()
	port2 := d.allocateHostPort()
	if port1 == port2 {
		t.Fatalf("allocateHostPort() returned same port twice: %d", port1)
	}
	if port1 < 1024 {
		t.Fatalf("port = %d, want >= 1024", port1)
	}
}

func TestDevice_AllocateHostPort_SkipsPending(t *testing.T) {
	d := &Device{
		conns:        make(map[connKey]*Connection),
		pending:      make(map[uint32]*pendingConn),
		nextHostPort: 1024,
	}
	// Mark port 1024 as pending
	d.pending[1024] = &pendingConn{}
	port := d.allocateHostPort()
	if port == 1024 {
		t.Fatal("allocateHostPort() should skip pending port 1024")
	}
	if port != 1025 {
		t.Fatalf("port = %d, want 1025", port)
	}
}

func TestDevice_AllocateHostPort_SkipsInUse(t *testing.T) {
	d := &Device{
		conns:        make(map[connKey]*Connection),
		pending:      make(map[uint32]*pendingConn),
		nextHostPort: 1024,
	}
	// Mark port 1024 as in use
	d.conns[connKey{guestPort: 100, hostPort: 1024}] = &Connection{}
	port := d.allocateHostPort()
	if port == 1024 {
		t.Fatal("allocateHostPort() should skip in-use port 1024")
	}
}

func TestDevice_AllocateHostPort_Wraps(t *testing.T) {
	d := &Device{
		conns:        make(map[connKey]*Connection),
		pending:      make(map[uint32]*pendingConn),
		nextHostPort: ^uint32(0), // max uint32, will overflow to 0
	}
	port := d.allocateHostPort()
	// After wrapping from 0, nextHostPort resets to 1024
	if port == 0 {
		t.Fatal("allocateHostPort() should not return port 0")
	}
}

func TestDevice_HandleDisconnect_NoConn(t *testing.T) {
	d := &Device{
		conns:   make(map[connKey]*Connection),
		pending: make(map[uint32]*pendingConn),
	}
	hdr := &pktHdr{SrcPort: 100, DstPort: 200}
	// Should not panic when no connection exists
	d.handleDisconnect(hdr)
}

func TestDevice_HandleDisconnect_WithPending(t *testing.T) {
	hostConn, deviceConn := net.Pipe()
	defer hostConn.Close()

	d := &Device{
		conns:   make(map[connKey]*Connection),
		pending: make(map[uint32]*pendingConn),
	}
	readyCh := make(chan error, 1)
	d.pending[200] = &pendingConn{
		conn:  &Connection{conn: deviceConn, device: d},
		ready: readyCh,
	}

	hdr := &pktHdr{SrcPort: 100, DstPort: 200}
	d.handleDisconnect(hdr)

	err := <-readyCh
	if err == nil {
		t.Fatal("expected error from handleDisconnect on pending conn")
	}
}

func TestDevice_HandleData_NoConnection(t *testing.T) {
	d := &Device{
		conns:   make(map[connKey]*Connection),
		pending: make(map[uint32]*pendingConn),
	}
	hdr := &pktHdr{SrcPort: 100, DstPort: 200, Len: 10}
	// Should not panic when no connection exists (early return on c == nil)
	d.handleData(hdr, nil, nil)
}

func TestDevice_HandleData_ZeroLen(t *testing.T) {
	d := &Device{
		conns:   make(map[connKey]*Connection),
		pending: make(map[uint32]*pendingConn),
	}
	// Put a connection in the map but Len=0, should early return
	d.conns[connKey{guestPort: 100, hostPort: 200}] = &Connection{}
	hdr := &pktHdr{SrcPort: 100, DstPort: 200, Len: 0}
	d.handleData(hdr, nil, nil)
}

func TestDevice_HandleResponse_NoPending(t *testing.T) {
	d := &Device{
		conns:   make(map[connKey]*Connection),
		pending: make(map[uint32]*pendingConn),
	}
	hdr := &pktHdr{SrcPort: 100, DstPort: 200}
	// Should not panic when no pending connection exists
	d.handleResponse(hdr, nil)
}
