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
	// Leave an actual stale socket file (as happens on crash) and
	// verify newUDSListener removes it cleanly.
	staleLn, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("stage stale socket: %v", err)
	}
	staleLn.Close()
	l, err := newUDSListener(path, &mockDialer{})
	if err != nil {
		t.Fatalf("newUDSListener over stale socket: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial new socket: %v", err)
	}
	c.Close()
}

// TestUDSListener_RefusesNonSocketPath guards the safety check Copilot
// flagged: a misconfigured UDSPath pointing at a regular file or
// directory must NOT be silently clobbered.
func TestUDSListener_RefusesNonSocketPath(t *testing.T) {
	t.Run("regular file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "important.data")
		if err := os.WriteFile(path, []byte("user data"), 0o600); err != nil {
			t.Fatalf("stage file: %v", err)
		}
		if _, err := newUDSListener(path, &mockDialer{}); err == nil {
			t.Fatal("expected refuse, got nil")
		}
		// Original file still there, unmodified.
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("regular file was clobbered: %v", err)
		}
		if string(data) != "user data" {
			t.Fatalf("regular file contents changed: %q", data)
		}
	})
	t.Run("directory", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "a-dir")
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if _, err := newUDSListener(path, &mockDialer{}); err == nil {
			t.Fatal("expected refuse, got nil")
		}
	})
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

// TestVM_Cleanup_ClosesUDSListener verifies that cleanup() closes the UDS
// listener and removes the socket file, without needing a full VM with
// KVM. All other device fields stay nil; cleanup's nil-guards handle that.
func TestVM_Cleanup_ClosesUDSListener(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vm.sock")
	l, err := newUDSListener(path, &mockDialer{})
	if err != nil {
		t.Fatalf("newUDSListener: %v", err)
	}
	go l.run()

	vm := &VM{udsListener: l}
	vm.cleanup()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("socket file should be removed after cleanup, stat err=%v", err)
	}
	if vm.udsListener != nil {
		t.Fatal("udsListener should be cleared after cleanup")
	}
	// Second cleanup must be a no-op (cleanupOnce + nil listener).
	vm.cleanup()
}

// TestVM_Cleanup_NilUDSListener ensures cleanup does not panic when no
// UDS listener was ever attached.
func TestVM_Cleanup_NilUDSListener(t *testing.T) {
	vm := &VM{}
	vm.cleanup()
}

// TestVM_Cleanup_WhileHandlersInDial verifies that cleanup() does not
// deadlock when UDS handlers are executing DialVsock at the moment of
// cleanup. The invariant: udsListener.Close() runs before vsockDialMu is
// acquired, so handlers with in-flight DialVsock calls finish (the real
// Device.Dial has dialTimeout=15s, so they always return eventually),
// their RLocks release, and cleanup proceeds to WLock. This models the
// realistic shutdown race — not the pathological "dialer blocks forever"
// scenario, which cannot happen in production.
func TestVM_Cleanup_WhileHandlersInDial(t *testing.T) {
	// Dialer takes a realistic amount of time, then returns an error —
	// mirroring a dev.Dial that times out because the guest didn't
	// respond.
	dialer := &mockDialer{
		respond: func(port uint32) (net.Conn, error) {
			time.Sleep(80 * time.Millisecond)
			return nil, errors.New("simulated vsock dial timeout")
		},
	}
	path := filepath.Join(t.TempDir(), "vm.sock")
	l, err := newUDSListener(path, dialer)
	if err != nil {
		t.Fatalf("newUDSListener: %v", err)
	}
	go l.run()

	// Fan out 10 concurrent clients; each drives a handler into DialVsock.
	for i := 0; i < 10; i++ {
		go func() {
			c, err := net.Dial("unix", path)
			if err != nil {
				return
			}
			defer c.Close()
			_, _ = c.Write([]byte("CONNECT 10023\n"))
			_, _ = io.ReadAll(c)
		}()
	}
	time.Sleep(30 * time.Millisecond) // let handlers enter the dialer

	vm := &VM{udsListener: l}
	done := make(chan struct{})
	go func() {
		vm.cleanup()
		close(done)
	}()

	select {
	case <-done:
		// cleanup returned; handlers finished within the dialer's own
		// time budget; no deadlock between udsListener.Close() and
		// vsockDialMu.Lock().
	case <-time.After(3 * time.Second):
		t.Fatal("cleanup deadlocked with handlers mid-DialVsock")
	}
}

// TestUDSListener_CloseAllBridges_ClientsSeeEOF verifies the Pause
// contract: closing all bridges makes active clients observe EOF on
// their socket, while the listener itself stays up.
func TestUDSListener_CloseAllBridges_ClientsSeeEOF(t *testing.T) {
	guestSrv, guestClient := net.Pipe()
	t.Cleanup(func() { guestSrv.Close(); guestClient.Close() })
	dialer := &mockDialer{
		respond: func(port uint32) (net.Conn, error) { return guestSrv, nil },
	}
	l := startListener(t, dialer)

	client := dialUDS(t, l.Path())
	defer client.Close()
	if _, err := client.Write([]byte("CONNECT 10023\n")); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	if got := readLine(t, client); got != "OK" {
		t.Fatalf("handshake = %q", got)
	}

	// Drive the bridge so it is registered (handleConn registers *after*
	// writing OK, before the io.Copy loops start).
	time.Sleep(30 * time.Millisecond)

	l.closeAllBridges()

	// Client should now observe EOF on read (bridge forcibly closed).
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16)
	n, err := client.Read(buf)
	if err != io.EOF && !errors.Is(err, net.ErrClosed) && n != 0 {
		t.Fatalf("expected EOF/closed after closeAllBridges, got n=%d err=%v", n, err)
	}
}

// TestUDSListener_CloseAllBridges_ListenerSurvives verifies the listener
// remains usable after bridges are closed — new clients can connect and
// CONNECT successfully.
func TestUDSListener_CloseAllBridges_ListenerSurvives(t *testing.T) {
	var pipeCount int
	var mu sync.Mutex
	dialer := &mockDialer{
		respond: func(port uint32) (net.Conn, error) {
			mu.Lock()
			pipeCount++
			mu.Unlock()
			srv, client := net.Pipe()
			go func() { io.Copy(io.Discard, client); client.Close() }()
			return srv, nil
		},
	}
	l := startListener(t, dialer)

	// First client establishes a bridge.
	c1 := dialUDS(t, l.Path())
	if _, err := c1.Write([]byte("CONNECT 10023\n")); err != nil {
		t.Fatalf("c1 write: %v", err)
	}
	if got := readLine(t, c1); got != "OK" {
		t.Fatalf("c1 handshake = %q", got)
	}
	time.Sleep(30 * time.Millisecond)

	l.closeAllBridges()
	c1.Close()

	// Second client after bridges closed — listener still accepts.
	c2 := dialUDS(t, l.Path())
	defer c2.Close()
	if _, err := c2.Write([]byte("CONNECT 10023\n")); err != nil {
		t.Fatalf("c2 write: %v", err)
	}
	if got := readLine(t, c2); got != "OK" {
		t.Fatalf("c2 handshake = %q (listener should still be up)", got)
	}
}

// TestUDSListener_CloseAllBridges_NoGoroutineLeak ensures bridge
// goroutines exit when closeAllBridges is called and do not accumulate
// across repeated Pause-equivalent cycles.
func TestUDSListener_CloseAllBridges_NoGoroutineLeak(t *testing.T) {
	dialer := &mockDialer{
		respond: func(port uint32) (net.Conn, error) {
			srv, client := net.Pipe()
			go func() { io.Copy(io.Discard, client); client.Close() }()
			return srv, nil
		},
	}
	l := startListener(t, dialer)

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	for i := 0; i < 5; i++ {
		c := dialUDS(t, l.Path())
		if _, err := c.Write([]byte("CONNECT 10023\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
		readLine(t, c)
		time.Sleep(20 * time.Millisecond)
		l.closeAllBridges()
		c.Close()
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if runtime.NumGoroutine() <= baseline+2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("goroutine leak after closeAllBridges cycles: baseline=%d current=%d",
		baseline, runtime.NumGoroutine())
}

// TestAttachVsockUDSListener guards the shared helper both arch backends
// must call right after creating the vsock device. Prior regression:
// arm64MachineBackend.setupDevices did NOT call this (it had its own
// inline copy), so --vsock-uds-path silently did nothing on ARM64 even
// though amd64 worked. Any new arch backend that forgets this is caught
// by TestArchBackends_WireUDSListener below.
func TestAttachVsockUDSListener(t *testing.T) {
	t.Run("nil vm", func(t *testing.T) {
		if err := attachVsockUDSListener(nil); err != nil {
			t.Fatalf("nil vm should be no-op, got %v", err)
		}
	})
	t.Run("vsock nil", func(t *testing.T) {
		vm := &VM{cfg: Config{}}
		if err := attachVsockUDSListener(vm); err != nil {
			t.Fatal(err)
		}
		if vm.udsListener != nil {
			t.Fatal("listener created despite Vsock==nil")
		}
	})
	t.Run("udsPath empty", func(t *testing.T) {
		vm := &VM{cfg: Config{Vsock: &VsockConfig{Enabled: true}}}
		if err := attachVsockUDSListener(vm); err != nil {
			t.Fatal(err)
		}
		if vm.udsListener != nil {
			t.Fatal("listener created despite UDSPath==''")
		}
	})
	t.Run("creates listener and accepts", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "vm.sock")
		vm := &VM{cfg: Config{Vsock: &VsockConfig{Enabled: true, UDSPath: path}}}
		if err := attachVsockUDSListener(vm); err != nil {
			t.Fatal(err)
		}
		if vm.udsListener == nil {
			t.Fatal("attachVsockUDSListener returned nil err but no listener")
		}
		t.Cleanup(func() { _ = vm.udsListener.Close() })
		if vm.udsListener.Path() != path {
			t.Fatalf("listener path = %q, want %q", vm.udsListener.Path(), path)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("stat socket: %v", err)
		}
		c, err := net.Dial("unix", path)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		c.Close()
	})
	t.Run("relative path rejected", func(t *testing.T) {
		vm := &VM{cfg: Config{Vsock: &VsockConfig{Enabled: true, UDSPath: "relative/x.sock"}}}
		if err := attachVsockUDSListener(vm); err == nil {
			t.Fatal("expected error for relative path")
		}
	})
}

// TestArchBackends_WireUDSListener is the regression gate for the arm64
// bug that slipped slice 4: every arch backend's setupDevices must either
// delegate to vm.setupDevices() (which calls the helper) or invoke
// attachVsockUDSListener directly. Without this guardrail, a new arch
// silently breaks --vsock-uds-path.
func TestArchBackends_WireUDSListener(t *testing.T) {
	backends := []string{"arch_x86.go", "arch_arm64.go"}
	for _, name := range backends {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(name)
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			src := string(data)
			hasHelper := strings.Contains(src, "attachVsockUDSListener")
			hasDelegate := strings.Contains(src, "vm.setupDevices()")
			if !hasHelper && !hasDelegate {
				t.Fatalf("%s: neither attachVsockUDSListener nor vm.setupDevices() referenced — UDS wiring would silently not run on this arch", name)
			}
		})
	}
}

// TestVM_Cleanup_Idempotent ensures cleanup runs exactly once regardless
// of how many times it is called.
func TestVM_Cleanup_Idempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vm.sock")
	l, err := newUDSListener(path, &mockDialer{})
	if err != nil {
		t.Fatalf("newUDSListener: %v", err)
	}
	go l.run()

	vm := &VM{udsListener: l}
	for i := 0; i < 5; i++ {
		vm.cleanup()
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("socket not removed: %v", err)
	}
}
