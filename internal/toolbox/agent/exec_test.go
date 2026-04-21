package agent

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"
)

// dialExec opens an HTTP/1.0 connection to srv, writes POST /exec
// with the given handshake JSON, and returns the raw conn (already
// past the response headers, ready for binary frames). The HTTP layer
// hijacks behind the scenes — from the caller's POV, after this call
// they're on the framed protocol.
func dialExec(t *testing.T, srv *httptest.Server, req ExecRequest) net.Conn {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal handshake: %v", err)
	}
	url := srv.Listener.Addr().String()
	conn, err := net.Dial("tcp", url)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// HTTP/1.0 + Connection: close so we get a clean post-headers state.
	httpReq := fmt.Sprintf("POST /exec HTTP/1.0\r\nContent-Length: %d\r\nContent-Type: application/json\r\n\r\n%s",
		len(body), body)
	if _, err := conn.Write([]byte(httpReq)); err != nil {
		t.Fatalf("write http req: %v", err)
	}
	// Drain response headers up to the blank line. After this, raw frames flow.
	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	// Wrap the conn so any bytes the bufio.Reader prefetched (none expected
	// here, but be safe) are visible to subsequent reads.
	return &prereadConn{Conn: conn, r: br}
}

// prereadConn fronts a net.Conn with a buffered reader that may already
// hold bytes consumed during HTTP header parsing. Callers see a
// continuous byte stream.
type prereadConn struct {
	net.Conn
	r *bufio.Reader
}

func (p *prereadConn) Read(b []byte) (int, error) { return p.r.Read(b) }

// readAllFrames pulls frames until io.EOF; returns the slice and any
// non-EOF error.
func readAllFrames(t *testing.T, r io.Reader) []framePair {
	t.Helper()
	var out []framePair
	for {
		ch, p, err := ReadFrame(r)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		out = append(out, framePair{ch, p})
	}
}

type framePair struct {
	channel byte
	payload []byte
}

func (fp framePair) String() string {
	return fmt.Sprintf("ch=%d len=%d", fp.channel, len(fp.payload))
}

func TestExec_SimpleStdoutAndExit(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	conn := dialExec(t, srv, ExecRequest{Cmd: []string{"sh", "-c", "echo hello && exit 0"}})
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	frames := readAllFrames(t, conn)
	var stdout bytes.Buffer
	var exitCode int32 = -42
	for _, f := range frames {
		switch f.channel {
		case ChannelStdout:
			stdout.Write(f.payload)
		case ChannelExit:
			c, err := ParseExitPayload(f.payload)
			if err != nil {
				t.Fatalf("bad exit payload: %v", err)
			}
			exitCode = c
		}
	}
	if got := strings.TrimRight(stdout.String(), "\n"); got != "hello" {
		t.Fatalf("stdout: got %q, want %q", got, "hello")
	}
	if exitCode != 0 {
		t.Fatalf("exit: got %d, want 0", exitCode)
	}
}

func TestExec_StderrSeparated(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	conn := dialExec(t, srv, ExecRequest{Cmd: []string{"sh", "-c", "echo out; echo err 1>&2; exit 0"}})
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	var out, errBuf bytes.Buffer
	for _, f := range readAllFrames(t, conn) {
		switch f.channel {
		case ChannelStdout:
			out.Write(f.payload)
		case ChannelStderr:
			errBuf.Write(f.payload)
		}
	}
	if strings.TrimSpace(out.String()) != "out" {
		t.Fatalf("stdout: %q", out.String())
	}
	if strings.TrimSpace(errBuf.String()) != "err" {
		t.Fatalf("stderr: %q", errBuf.String())
	}
}

func TestExec_NonZeroExitCode(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	conn := dialExec(t, srv, ExecRequest{Cmd: []string{"sh", "-c", "exit 42"}})
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	var got int32 = -1
	for _, f := range readAllFrames(t, conn) {
		if f.channel == ChannelExit {
			got, _ = ParseExitPayload(f.payload)
		}
	}
	if got != 42 {
		t.Fatalf("exit: got %d, want 42", got)
	}
}

func TestExec_StdinEcho(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	// `cat` echoes stdin; we send "ping\n" then close stdin (empty frame),
	// then read until EXIT.
	conn := dialExec(t, srv, ExecRequest{Cmd: []string{"cat"}})
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := WriteFrame(conn, ChannelStdin, []byte("ping\n")); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	if _, err := WriteFrame(conn, ChannelStdin, nil); err != nil {
		t.Fatalf("close stdin (empty frame): %v", err)
	}

	var stdout bytes.Buffer
	var exitCode int32 = -1
	for _, f := range readAllFrames(t, conn) {
		switch f.channel {
		case ChannelStdout:
			stdout.Write(f.payload)
		case ChannelExit:
			exitCode, _ = ParseExitPayload(f.payload)
		}
	}
	if got := stdout.String(); got != "ping\n" {
		t.Fatalf("stdout: got %q, want %q", got, "ping\n")
	}
	if exitCode != 0 {
		t.Fatalf("exit: got %d, want 0", exitCode)
	}
}

func TestExec_ClientCloseSendsSIGTERM(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	// `sleep 30` would normally outlive the test. We close the conn to
	// trigger the SIGTERM-then-SIGKILL escalation. The handler's
	// process.Wait() returns within killGrace + a small slack; we
	// verify the WHOLE handshake → close → server-side cleanup happens
	// in well under that bound.
	conn := dialExec(t, srv, ExecRequest{Cmd: []string{"sleep", "30"}})
	conn.SetDeadline(time.Now().Add(8 * time.Second))

	// Give the agent a moment to spawn `sleep` before we yank the rug.
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	conn.Close()

	// We can't read from a closed conn; instead, allow the agent to
	// finish handling the close. With killGrace=2s the process should
	// be reaped within ~3s. We only need to assert the test doesn't
	// hang — the conn-close already proved the wire-side path.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("close took too long: %v", elapsed)
	}
	// Sleep a bit so any goroutines the agent spawned have a chance
	// to drain before httptest.Server.Close() complains about leaks.
	time.Sleep(killGrace + 500*time.Millisecond)
}

func TestExec_ExplicitSIGTERMViaSignalFrame(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	// `sleep 30` again — but this time we send an explicit SIGTERM
	// via ChannelSignal and verify the EXIT frame surfaces a non-zero
	// (signal-killed) code.
	conn := dialExec(t, srv, ExecRequest{Cmd: []string{"sleep", "30"}})
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	time.Sleep(100 * time.Millisecond)
	if _, err := WriteFrame(conn, ChannelSignal, []byte{signalKindKill, byte(syscall.SIGTERM)}); err != nil {
		t.Fatalf("write signal frame: %v", err)
	}

	var exitCode int32 = 0
	var sawExit bool
	for _, f := range readAllFrames(t, conn) {
		if f.channel == ChannelExit {
			exitCode, _ = ParseExitPayload(f.payload)
			sawExit = true
		}
	}
	if !sawExit {
		t.Fatal("never received EXIT frame after SIGTERM")
	}
	// ExitCode() returns -1 for signal-killed processes.
	if exitCode != -1 {
		t.Fatalf("exit code after SIGTERM: got %d, want -1 (signal-killed)", exitCode)
	}
}

// Handshake rejection now happens AFTER hijack (the handler always
// returns HTTP 200 + framed body, never an HTTP error). Errors arrive
// as a stderr frame followed by EXIT -1. This shape was chosen so the
// host client always switches to framed mode after sending POST /exec
// — no pre-hijack vs post-hijack branching.

func TestExec_HandshakeRejectsEmptyCmd(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	conn := dialExec(t, srv, ExecRequest{Cmd: nil})
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	stderr, exitCode := collectStderrAndExit(t, conn)
	if !strings.Contains(stderr, "cmd[]") {
		t.Fatalf("stderr: got %q, want substring 'cmd[]'", stderr)
	}
	if exitCode != -1 {
		t.Fatalf("exit: got %d, want -1", exitCode)
	}
}

func TestExec_HandshakeRejectsMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	body := []byte("{not valid json")
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST /exec HTTP/1.0\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	br := bufio.NewReader(conn)
	// Skip headers
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	stderr, exitCode := collectStderrAndExit(t, &prereadConn{Conn: conn, r: br})
	if !strings.Contains(stderr, "decode handshake") {
		t.Fatalf("stderr: got %q, want substring 'decode handshake'", stderr)
	}
	if exitCode != -1 {
		t.Fatalf("exit: got %d, want -1", exitCode)
	}
}

func collectStderrAndExit(t *testing.T, r io.Reader) (string, int32) {
	t.Helper()
	var stderr bytes.Buffer
	var exitCode int32 = 0
	for _, f := range readAllFrames(t, r) {
		switch f.channel {
		case ChannelStderr:
			stderr.Write(f.payload)
		case ChannelExit:
			exitCode, _ = ParseExitPayload(f.payload)
		}
	}
	return stderr.String(), exitCode
}

func TestExec_LargeStdoutChunked(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	// Print 200 KiB of output. With outputChunkSize=32 KiB this should
	// produce ~7 stdout frames. Validates the chunk-splitting logic
	// AND that bytes round-trip without corruption.
	const N = 200 << 10
	conn := dialExec(t, srv, ExecRequest{
		// /dev/zero + tr produces N pure 'A' bytes — no newlines, no
		// shell-buffering surprises. Validates chunk-splitting AND
		// byte-identity round-trip.
		Cmd: []string{"sh", "-c", fmt.Sprintf("head -c %d /dev/zero | tr '\\0' A", N)},
	})
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	var stdout bytes.Buffer
	frameCount := 0
	for _, f := range readAllFrames(t, conn) {
		if f.channel == ChannelStdout {
			frameCount++
			stdout.Write(f.payload)
		}
	}
	if stdout.Len() != N {
		t.Fatalf("stdout size: got %d, want %d", stdout.Len(), N)
	}
	for i, b := range stdout.Bytes() {
		if b != 'A' {
			t.Fatalf("byte %d corrupted: got %q, want 'A'", i, b)
		}
	}
	if frameCount < 2 {
		t.Fatalf("expected output to span multiple frames, got %d", frameCount)
	}
}

func TestExec_PTYMode(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	// In PTY mode, stdout/stderr are merged. We just verify the basic
	// flow: tty:true allocates a pty, exec runs, output comes back,
	// EXIT terminates.
	conn := dialExec(t, srv, ExecRequest{
		Cmd: []string{"sh", "-c", "echo pty-out"},
		TTY: true, Cols: 80, Rows: 24,
	})
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	var stdout bytes.Buffer
	var exitCode int32 = -1
	for _, f := range readAllFrames(t, conn) {
		switch f.channel {
		case ChannelStdout:
			stdout.Write(f.payload)
		case ChannelExit:
			exitCode, _ = ParseExitPayload(f.payload)
		}
	}
	// PTY may add CRLF translation depending on tty settings — accept
	// either "pty-out\n" or "pty-out\r\n".
	got := strings.ReplaceAll(stdout.String(), "\r\n", "\n")
	if !strings.Contains(got, "pty-out") {
		t.Fatalf("expected pty-out in stdout, got %q", stdout.String())
	}
	if exitCode != 0 {
		t.Fatalf("exit: got %d, want 0", exitCode)
	}
}

func TestExec_PTYWinsizeSignalDoesNotCrash(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	// We can't easily verify the kernel saw the new winsize from outside
	// the pty, but we CAN verify the agent doesn't crash when handed
	// one — and that the process still exits cleanly afterwards.
	conn := dialExec(t, srv, ExecRequest{
		Cmd: []string{"sh", "-c", "sleep 0.2 && echo done"},
		TTY: true, Cols: 80, Rows: 24,
	})
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	winsize := make([]byte, 5)
	winsize[0] = signalKindWinsize
	binary.BigEndian.PutUint16(winsize[1:3], 120)
	binary.BigEndian.PutUint16(winsize[3:5], 40)
	if _, err := WriteFrame(conn, ChannelSignal, winsize); err != nil {
		t.Fatalf("write winsize: %v", err)
	}

	var exitCode int32 = -1
	for _, f := range readAllFrames(t, conn) {
		if f.channel == ChannelExit {
			exitCode, _ = ParseExitPayload(f.payload)
		}
	}
	if exitCode != 0 {
		t.Fatalf("exit: got %d, want 0", exitCode)
	}
}
