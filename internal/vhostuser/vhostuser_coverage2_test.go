package vhostuser

import (
	"encoding/binary"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCloseFDsEmpty(t *testing.T) {
	if err := CloseFDs(nil); err != nil {
		t.Fatalf("CloseFDs(nil) = %v", err)
	}
	if err := CloseFDs([]int{}); err != nil {
		t.Fatalf("CloseFDs([]) = %v", err)
	}
}

func TestCloseFDsAllNegative(t *testing.T) {
	if err := CloseFDs([]int{-1, -2, -3}); err != nil {
		t.Fatalf("CloseFDs(negatives) = %v", err)
	}
}

func TestCloseFDsInvalidFD(t *testing.T) {
	// Closing an already-closed FD should not error (EBADF is ignored)
	err := CloseFDs([]int{99999})
	if err != nil {
		t.Fatalf("CloseFDs(invalid) = %v, want nil (EBADF ignored)", err)
	}
}

func TestRetryableDialErrNonNetError(t *testing.T) {
	if retryableDialErr(errors.New("something else")) {
		t.Fatal("non-net error should not be retryable")
	}
}

func TestRetryableDialErrECONNREFUSED(t *testing.T) {
	inner := &os.SyscallError{Syscall: "connect", Err: unix.ECONNREFUSED}
	err := &net.OpError{Op: "dial", Net: "unix", Err: inner}
	if !retryableDialErr(err) {
		t.Fatal("ECONNREFUSED should be retryable")
	}
}

func TestRetryableDialErrENOENT(t *testing.T) {
	inner := &os.SyscallError{Syscall: "connect", Err: unix.ENOENT}
	err := &net.OpError{Op: "dial", Net: "unix", Err: inner}
	if !retryableDialErr(err) {
		t.Fatal("ENOENT should be retryable")
	}
}

func TestRetryableDialErrNilInner(t *testing.T) {
	err := &net.OpError{Op: "dial", Net: "unix", Err: nil}
	if retryableDialErr(err) {
		t.Fatal("nil inner error should not be retryable")
	}
}

func TestRetryableDialErrOtherSyscallErr(t *testing.T) {
	inner := &os.SyscallError{Syscall: "connect", Err: unix.EPERM}
	err := &net.OpError{Op: "dial", Net: "unix", Err: inner}
	if retryableDialErr(err) {
		t.Fatal("EPERM should not be retryable")
	}
}

func TestClientCloseNil(t *testing.T) {
	var c *Client
	if err := c.Close(); err != nil {
		t.Fatalf("nil Close = %v", err)
	}
}

func TestClientCloseNilConn(t *testing.T) {
	c := &Client{conn: nil}
	if err := c.Close(); err != nil {
		t.Fatalf("nil conn Close = %v", err)
	}
}

func TestDialNonExistentSocket(t *testing.T) {
	_, err := Dial("/nonexistent/path/sock")
	if err == nil {
		t.Fatal("expected error dialing nonexistent socket")
	}
}

func TestGetProtocolFeatures(t *testing.T) {
	path := startVhostServer(t, func(conn *net.UnixConn) {
		req := readRequest(t, conn)
		if req.Request != FrontendReqGetProtocolFeatures {
			t.Fatalf("request = %d, want %d", req.Request, FrontendReqGetProtocolFeatures)
		}
		var body [8]byte
		binary.LittleEndian.PutUint64(body[:], ProtocolFeatureMQ|ProtocolFeatureReplyAck)
		writeReply(t, conn, FrontendReqGetProtocolFeatures, 0, body[:])
	})

	client, err := Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	features, err := client.GetProtocolFeatures()
	if err != nil {
		t.Fatalf("GetProtocolFeatures: %v", err)
	}
	if features != ProtocolFeatureMQ|ProtocolFeatureReplyAck {
		t.Fatalf("features = %#x", features)
	}
}

func TestGetQueueNum(t *testing.T) {
	path := startVhostServer(t, func(conn *net.UnixConn) {
		req := readRequest(t, conn)
		if req.Request != FrontendReqGetQueueNum {
			t.Fatalf("request = %d", req.Request)
		}
		var body [8]byte
		binary.LittleEndian.PutUint64(body[:], 4)
		writeReply(t, conn, FrontendReqGetQueueNum, 0, body[:])
	})

	client, err := Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	num, err := client.GetQueueNum()
	if err != nil {
		t.Fatalf("GetQueueNum: %v", err)
	}
	if num != 4 {
		t.Fatalf("queue_num = %d, want 4", num)
	}
}

func TestSetFeatures(t *testing.T) {
	path := startVhostServer(t, func(conn *net.UnixConn) {
		req := readRequest(t, conn)
		if req.Request != FrontendReqSetFeatures {
			t.Fatalf("request = %d", req.Request)
		}
		// SetFeatures with needReply=false uses sendRequest with allowAck=true
		// but since needReply is false (no protocol features set), no ack is read
	})

	client, err := Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if err := client.SetFeatures(0x12345678); err != nil {
		t.Fatalf("SetFeatures: %v", err)
	}
}

func TestSetOwner(t *testing.T) {
	path := startVhostServer(t, func(conn *net.UnixConn) {
		req := readRequest(t, conn)
		if req.Request != FrontendReqSetOwner {
			t.Fatalf("request = %d, want %d", req.Request, FrontendReqSetOwner)
		}
	})

	client, err := Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if err := client.SetOwner(); err != nil {
		t.Fatalf("SetOwner: %v", err)
	}
}

func TestWriteMessageOversized(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, _ := ln.AcceptUnix()
		if conn != nil {
			defer conn.Close()
		}
	}()

	client, err := Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	bigBody := make([]byte, maxMsgSize+1)
	err = client.writeMessage(1, bigBody, nil, false)
	if err == nil {
		t.Fatal("expected error for oversized message")
	}
}

func TestSetProtocolFeaturesWithoutReplyAck(t *testing.T) {
	path := startVhostServer(t, func(conn *net.UnixConn) {
		readRequest(t, conn)
	})

	client, err := Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// Set features WITHOUT ProtocolFeatureReplyAck
	if err := client.SetProtocolFeatures(ProtocolFeatureMQ); err != nil {
		t.Fatalf("SetProtocolFeatures: %v", err)
	}
	if client.needReply {
		t.Fatal("needReply should be false without ReplyAck flag")
	}
}
