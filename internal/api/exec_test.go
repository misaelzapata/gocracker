package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gocracker/gocracker/internal/guestexec"
	"github.com/gocracker/gocracker/pkg/vmm"
)

type execDialHandle struct {
	*fakeHandle
	dial func(uint32) (net.Conn, error)
}

func (h *execDialHandle) DialVsock(port uint32) (net.Conn, error) {
	return h.dial(port)
}

type hijackResponseWriter struct {
	header http.Header
	code   int
	body   bytes.Buffer
	conn   net.Conn
	rw     *bufio.ReadWriter
}

func (w *hijackResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *hijackResponseWriter) Write(data []byte) (int, error) {
	return w.body.Write(data)
}

func (w *hijackResponseWriter) WriteHeader(code int) {
	w.code = code
}

func (w *hijackResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, w.rw, nil
}

func newExecTestHandle(t *testing.T, fn func(net.Conn)) *execDialHandle {
	t.Helper()
	handle := &execDialHandle{fakeHandle: newFakeHandle("vm-1")}
	handle.cfg.Exec = &vmm.ExecConfig{Enabled: true}
	handle.dial = func(port uint32) (net.Conn, error) {
		serverConn, clientConn := net.Pipe()
		go func() {
			defer serverConn.Close()
			fn(serverConn)
		}()
		return clientConn, nil
	}
	return handle
}

func TestRunExecCommand(t *testing.T) {
	entry := &vmEntry{
		handle: newExecTestHandle(t, func(conn net.Conn) {
			var req guestexec.Request
			if err := guestexec.Decode(conn, &req); err != nil {
				t.Errorf("Decode() error = %v", err)
				return
			}
			if req.Mode != guestexec.ModeExec || strings.Join(req.Command, " ") != "echo ok" {
				t.Errorf("request = %+v", req)
			}
			_ = guestexec.Encode(conn, guestexec.Response{Stdout: "ok\n", ExitCode: 0})
		}),
	}

	resp, err := runExecCommand(context.Background(), entry, ExecRequest{Command: []string{"echo", "ok"}, Env: []string{"A=B"}, WorkDir: "/tmp"})
	if err != nil {
		t.Fatalf("runExecCommand() error = %v", err)
	}
	if resp.Stdout != "ok\n" || resp.ExitCode != 0 {
		t.Fatalf("response = %+v", resp)
	}
}

func TestRunExecCommandGuestError(t *testing.T) {
	entry := &vmEntry{
		handle: newExecTestHandle(t, func(conn net.Conn) {
			var req guestexec.Request
			_ = guestexec.Decode(conn, &req)
			_ = guestexec.Encode(conn, guestexec.Response{Error: "guest failed"})
		}),
	}
	if _, err := runExecCommand(context.Background(), entry, ExecRequest{Command: []string{"false"}}); err == nil || err.Error() != "guest failed" {
		t.Fatalf("runExecCommand() err = %v", err)
	}
}

func TestHandleVMExec(t *testing.T) {
	srv := New()
	srv.vms["vm-1"] = &vmEntry{
		handle: newExecTestHandle(t, func(conn net.Conn) {
			var req guestexec.Request
			_ = guestexec.Decode(conn, &req)
			_ = guestexec.Encode(conn, guestexec.Response{Stdout: "done", ExitCode: 0})
		}),
	}

	req := httptest.NewRequest(http.MethodPost, "/vms/vm-1/exec", strings.NewReader(`{"command":["echo","done"]}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp ExecResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if resp.Stdout != "done" {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestHandleVMExecStream(t *testing.T) {
	srv := New()
	srv.vms["vm-1"] = &vmEntry{
		handle: newExecTestHandle(t, func(conn net.Conn) {
			var req guestexec.Request
			if err := guestexec.Decode(conn, &req); err != nil {
				t.Errorf("Decode() error = %v", err)
				return
			}
			if req.Mode != guestexec.ModeStream {
				t.Errorf("Mode = %q", req.Mode)
			}
			if err := guestexec.Encode(conn, guestexec.Response{OK: true}); err != nil {
				t.Errorf("Encode() error = %v", err)
				return
			}
			buf := make([]byte, 4)
			if _, err := io.ReadFull(conn, buf); err != nil {
				t.Errorf("ReadFull() error = %v", err)
				return
			}
			_, _ = conn.Write(bytes.ToUpper(buf))
		}),
	}

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	rw := bufio.NewReadWriter(bufio.NewReader(serverConn), bufio.NewWriter(serverConn))
	w := &hijackResponseWriter{conn: serverConn, rw: rw}
	req := httptest.NewRequest(http.MethodPost, "/vms/vm-1/exec/stream", strings.NewReader(`{"columns":80,"rows":24}`))

	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(w, req)
		close(done)
	}()

	reader := bufio.NewReader(clientConn)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("ReadString(status) error = %v", err)
	}
	if !strings.Contains(line, "101 Switching Protocols") {
		t.Fatalf("status line = %q", line)
	}
	for {
		headerLine, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("ReadString(header) error = %v", err)
		}
		if headerLine == "\r\n" {
			break
		}
	}
	if _, err := clientConn.Write([]byte("ping")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	reply := make([]byte, 4)
	if _, err := io.ReadFull(reader, reply); err != nil {
		t.Fatalf("ReadFull(reply) error = %v", err)
	}
	if string(reply) != "PING" {
		t.Fatalf("reply = %q", reply)
	}
	_ = clientConn.Close()
	<-done
}
