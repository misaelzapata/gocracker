package vsock

import (
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
