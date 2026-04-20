package vmm

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockDialer implements VsockDialer for tests. Each DialVsock call invokes
// respond to produce the guest-side net.Conn. Tests typically wire respond
// to net.Pipe() and keep the counterpart for assertions.
type mockDialer struct {
	respond func(port uint32) (net.Conn, error)
	mu      sync.Mutex
	calls   []uint32
}

func (m *mockDialer) DialVsock(port uint32) (net.Conn, error) {
	m.mu.Lock()
	m.calls = append(m.calls, port)
	m.mu.Unlock()
	if m.respond == nil {
		return nil, errors.New("no_respond")
	}
	return m.respond(port)
}

func mkUDSPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "sandboxes", "vm-test.sock")
}

func startListener(t *testing.T, dialer VsockDialer) *udsListener {
	t.Helper()
	l, err := newUDSListener(mkUDSPath(t), dialer)
	if err != nil {
		t.Fatalf("newUDSListener: %v", err)
	}
	go l.run()
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func dialUDS(t *testing.T, path string) net.Conn {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial unix %s: %v", path, err)
	}
	return c
}

func readLine(t *testing.T, r io.Reader) string {
	t.Helper()
	br := bufio.NewReader(r)
	line, err := br.ReadString('\n')
	if err != nil && err != io.EOF {
		t.Fatalf("read line: %v", err)
	}
	return strings.TrimRight(line, "\n")
}

func TestUDSListener_ConnectOK(t *testing.T) {
	guestSrv, guestClient := net.Pipe()
	t.Cleanup(func() { guestSrv.Close(); guestClient.Close() })
	dialer := &mockDialer{
		respond: func(port uint32) (net.Conn, error) {
			if port != 10023 {
				return nil, fmt.Errorf("unexpected port %d", port)
			}
			return guestSrv, nil
		},
	}
	l := startListener(t, dialer)

	client := dialUDS(t, l.Path())
	defer client.Close()
	if _, err := client.Write([]byte("CONNECT 10023\n")); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	if got := readLine(t, client); got != "OK" {
		t.Fatalf("handshake response = %q, want OK", got)
	}

	// host→guest: client writes, guestClient reads.
	if _, err := client.Write([]byte("hello guest\n")); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	buf := make([]byte, 64)
	guestClient.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := guestClient.Read(buf)
	if err != nil {
		t.Fatalf("guest read: %v", err)
	}
	if got := string(buf[:n]); got != "hello guest\n" {
		t.Fatalf("guest got %q, want %q", got, "hello guest\n")
	}

	// guest→host: guestClient writes, client reads.
	if _, err := guestClient.Write([]byte("hi host\n")); err != nil {
		t.Fatalf("guest write: %v", err)
	}
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err = client.Read(buf)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	if got := string(buf[:n]); got != "hi host\n" {
		t.Fatalf("client got %q, want %q", got, "hi host\n")
	}
}

func TestUDSListener_ConnectFailure(t *testing.T) {
	dialer := &mockDialer{
		respond: func(port uint32) (net.Conn, error) {
			return nil, errors.New("vsock device not configured")
		},
	}
	l := startListener(t, dialer)
	client := dialUDS(t, l.Path())
	defer client.Close()
	if _, err := client.Write([]byte("CONNECT 10023\n")); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	got := readLine(t, client)
	if !strings.HasPrefix(got, "FAILURE ") {
		t.Fatalf("response = %q, want FAILURE prefix", got)
	}
	if !strings.Contains(got, "vsock device not configured") {
		t.Fatalf("response = %q, want to contain dialer error", got)
	}
}

func TestUDSListener_MalformedConnect(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{"not connect", "HELLO\n", "FAILURE malformed_request"},
		{"no space", "CONNECT\n", "FAILURE malformed_request"},
		{"missing port", "CONNECT \n", "FAILURE missing_port"},
		{"non numeric port", "CONNECT abc\n", "FAILURE invalid_port"},
		{"port zero", "CONNECT 0\n", "FAILURE invalid_port"},
		{"port over uint32", "CONNECT 99999999999\n", "FAILURE invalid_port"},
		{"negative port", "CONNECT -5\n", "FAILURE invalid_port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var dialerCalled atomic.Bool
			dialer := &mockDialer{
				respond: func(port uint32) (net.Conn, error) {
					dialerCalled.Store(true)
					return nil, errors.New("should_not_be_called")
				},
			}
			l := startListener(t, dialer)
			client := dialUDS(t, l.Path())
			defer client.Close()
			if _, err := client.Write([]byte(tc.line)); err != nil {
				t.Fatalf("write: %v", err)
			}
			got := readLine(t, client)
			if got != tc.want {
				t.Fatalf("response = %q, want %q", got, tc.want)
			}
			if dialerCalled.Load() {
				t.Fatalf("dialer invoked for %q; should be short-circuited", tc.line)
			}
		})
	}
}

func TestUDSListener_CleanClose(t *testing.T) {
	l := startListener(t, &mockDialer{})
	path := l.Path()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("socket file should exist before Close: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("socket file should be removed after Close, got err=%v", err)
	}
	// Accepting should now fail with ErrClosed via Dial (socket gone → ENOENT).
	if _, err := net.Dial("unix", path); err == nil {
		t.Fatalf("Dial after Close should fail")
	}
}

func TestUDSListener_StaleSocketRemovedOnStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sandboxes", "vm.sock")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatalf("write stale: %v", err)
	}
	l, err := newUDSListener(path, &mockDialer{})
	if err != nil {
		t.Fatalf("newUDSListener over stale file: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	// New socket should be listenable.
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial new socket: %v", err)
	}
	c.Close()
}

func TestUDSListener_Permissions(t *testing.T) {
	l := startListener(t, &mockDialer{})
	info, err := os.Stat(l.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	mode := info.Mode() & os.ModePerm
	if mode != 0o660 {
		t.Fatalf("socket mode = %o, want 0660", mode)
	}
	dirInfo, err := os.Stat(filepath.Dir(l.Path()))
	if err != nil {
		t.Fatalf("stat parent: %v", err)
	}
	if dmode := dirInfo.Mode() & os.ModePerm; dmode != 0o750 {
		t.Fatalf("parent dir mode = %o, want 0750", dmode)
	}
}

func TestUDSListener_Concurrent(t *testing.T) {
	// Each DialVsock call returns a fresh pipe pair. The guest-side pipe
	// end simply echoes whatever it reads. Concurrent clients must receive
	// their own bytes back without cross-talk.
	dialer := &mockDialer{
		respond: func(port uint32) (net.Conn, error) {
			srv, client := net.Pipe()
			go func() {
				buf := make([]byte, 1024)
				for {
					n, err := client.Read(buf)
					if err != nil {
						client.Close()
						return
					}
					if _, err := client.Write(buf[:n]); err != nil {
						client.Close()
						return
					}
				}
			}()
			return srv, nil
		},
	}
	l := startListener(t, dialer)

	const n = 100
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := net.Dial("unix", l.Path())
			if err != nil {
				errs <- fmt.Errorf("client %d dial: %w", i, err)
				return
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := fmt.Fprintf(c, "CONNECT %d\n", 10023+i); err != nil {
				errs <- fmt.Errorf("client %d write connect: %w", i, err)
				return
			}
			br := bufio.NewReader(c)
			line, err := br.ReadString('\n')
			if err != nil {
				errs <- fmt.Errorf("client %d read OK: %w", i, err)
				return
			}
			if strings.TrimRight(line, "\n") != "OK" {
				errs <- fmt.Errorf("client %d handshake = %q", i, line)
				return
			}
			payload := fmt.Sprintf("hello-%d\n", i)
			if _, err := c.Write([]byte(payload)); err != nil {
				errs <- fmt.Errorf("client %d write payload: %w", i, err)
				return
			}
			got, err := br.ReadString('\n')
			if err != nil {
				errs <- fmt.Errorf("client %d read echo: %w", i, err)
				return
			}
			if got != payload {
				errs <- fmt.Errorf("client %d echo = %q, want %q", i, got, payload)
				return
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestUDSListener_NoGoroutineLeak(t *testing.T) {
	// Establish baseline (let runtime settle).
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	for i := 0; i < 5; i++ {
		dialer := &mockDialer{
			respond: func(port uint32) (net.Conn, error) {
				srv, client := net.Pipe()
				go func() { io.Copy(io.Discard, client); client.Close() }()
				return srv, nil
			},
		}
		l, err := newUDSListener(mkUDSPath(t), dialer)
		if err != nil {
			t.Fatalf("newUDSListener: %v", err)
		}
		go l.run()

		// Open and complete a connection, then close it from the client
		// side (guest-side pipe copier will exit on Read EOF).
		c, err := net.Dial("unix", l.Path())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		if _, err := c.Write([]byte("CONNECT 10023\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
		readLine(t, c)
		c.Close()

		if err := l.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}

	// Give goroutines a moment to settle; they should all return.
	deadline := time.Now().Add(2 * time.Second)
	var current int
	for time.Now().Before(deadline) {
		runtime.GC()
		current = runtime.NumGoroutine()
		if current <= baseline+2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("goroutine leak: baseline=%d current=%d", baseline, current)
}

func TestUDSListener_EmptyPathRejected(t *testing.T) {
	if _, err := newUDSListener("", &mockDialer{}); err == nil {
		t.Fatal("expected error for empty path")
	}
}

func TestUDSListener_RelativePathRejected(t *testing.T) {
	if _, err := newUDSListener("rel/path.sock", &mockDialer{}); err == nil {
		t.Fatal("expected error for relative path")
	}
}

func TestParseConnect(t *testing.T) {
	cases := []struct {
		line     string
		wantPort uint32
		wantErr  string
	}{
		{"CONNECT 10023\n", 10023, ""},
		{"CONNECT 1\n", 1, ""},
		{"CONNECT 4294967295\n", 4294967295, ""},
		{"CONNECT 10023\r\n", 10023, ""},
		{"HELLO\n", 0, "malformed_request"},
		{"CONNECT\n", 0, "malformed_request"},
		{"CONNECT \n", 0, "missing_port"},
		{"CONNECT abc\n", 0, "invalid_port"},
		{"CONNECT 0\n", 0, "invalid_port"},
		{"CONNECT 4294967296\n", 0, "invalid_port"},
		{"CONNECT -1\n", 0, "invalid_port"},
	}
	for _, tc := range cases {
		t.Run(tc.line, func(t *testing.T) {
			port, err := parseConnect(tc.line)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected err: %v", err)
				}
				if port != tc.wantPort {
					t.Fatalf("port = %d, want %d", port, tc.wantPort)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error %q, got nil (port=%d)", tc.wantErr, port)
			}
			if err.Error() != tc.wantErr {
				t.Fatalf("err = %q, want %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestSanitizeReason(t *testing.T) {
	in := "an\nerror\rwith control chars and " + strings.Repeat("x", 200)
	got := sanitizeReason(in)
	if strings.ContainsAny(got, "\n\r") {
		t.Fatalf("sanitizeReason left control chars: %q", got)
	}
	if len(got) > 120 {
		t.Fatalf("sanitizeReason did not cap length: len=%d", len(got))
	}
}
