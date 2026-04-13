package vhostuser

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func startVhostServer(t *testing.T, handler func(*net.UnixConn)) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.AcceptUnix()
		if err != nil {
			return
		}
		defer conn.Close()
		handler(conn)
	}()
	return path
}

func readRequest(t *testing.T, conn *net.UnixConn) Header {
	t.Helper()
	buf := make([]byte, headerSize)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	header := Header{
		Request: binary.LittleEndian.Uint32(buf[0:4]),
		Flags:   binary.LittleEndian.Uint32(buf[4:8]),
		Size:    binary.LittleEndian.Uint32(buf[8:12]),
	}
	if header.Size > 0 {
		body := make([]byte, header.Size)
		if _, err := io.ReadFull(conn, body); err != nil {
			t.Fatal(err)
		}
	}
	return header
}

func writeReply(t *testing.T, conn *net.UnixConn, req uint32, flags uint32, body []byte) {
	t.Helper()
	packet := make([]byte, headerSize+len(body))
	binary.LittleEndian.PutUint32(packet[0:], req)
	binary.LittleEndian.PutUint32(packet[4:], protoV1Flag|headerFlagReply|flags)
	binary.LittleEndian.PutUint32(packet[8:], uint32(len(body)))
	copy(packet[headerSize:], body)
	if _, _, err := conn.WriteMsgUnix(packet, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestDialGetFeaturesAndAckedRequests(t *testing.T) {
	path := startVhostServer(t, func(conn *net.UnixConn) {
		req := readRequest(t, conn)
		if req.Request != FrontendReqGetFeatures {
			t.Fatalf("request = %d", req.Request)
		}
		var body [8]byte
		binary.LittleEndian.PutUint64(body[:], VhostUserVirtioFeatureProtocolFeatures)
		writeReply(t, conn, FrontendReqGetFeatures, 0, body[:])

		req = readRequest(t, conn)
		if req.Request != FrontendReqSetProtocolFeatures {
			t.Fatalf("request = %d", req.Request)
		}

		req = readRequest(t, conn)
		if req.Request != FrontendReqSetVringEnable {
			t.Fatalf("request = %d", req.Request)
		}
		var ack [8]byte
		writeReply(t, conn, FrontendReqSetVringEnable, 0, ack[:])
	})

	client, err := Dial(path)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer client.Close()

	features, err := client.GetFeatures()
	if err != nil {
		t.Fatalf("GetFeatures() error = %v", err)
	}
	if features != VhostUserVirtioFeatureProtocolFeatures {
		t.Fatalf("features = %#x", features)
	}
	if err := client.SetProtocolFeatures(ProtocolFeatureReplyAck); err != nil {
		t.Fatalf("SetProtocolFeatures() error = %v", err)
	}
	if !client.needReply {
		t.Fatal("needReply = false, want true")
	}
	if err := client.SetVringEnable(1, true); err != nil {
		t.Fatalf("SetVringEnable() error = %v", err)
	}
}

func TestCloseFDsAndRetryableDialErr(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := CloseFDs([]int{fds[0], fds[1], -1}); err != nil {
		t.Fatalf("CloseFDs() error = %v", err)
	}
	if !retryableDialErr(&net.OpError{Err: os.ErrNotExist}) {
		t.Fatal("retryableDialErr(os.ErrNotExist) = false")
	}
	if retryableDialErr(errors.New("boom")) {
		t.Fatal("retryableDialErr(unrelated) = true")
	}
}

func TestDialRetriesUntilSocketExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sock")
	go func() {
		time.Sleep(150 * time.Millisecond)
		ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			return
		}
		defer ln.Close()
		conn, err := ln.AcceptUnix()
		if err != nil {
			return
		}
		defer conn.Close()
	}()
	client, err := Dial(path)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	_ = client.Close()
}
