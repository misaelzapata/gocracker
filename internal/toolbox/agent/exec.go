package agent

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// ExecRequest is the handshake JSON the client writes immediately
// after the HTTP request line + headers. Must fit in the bounded
// handshake buffer (handshakeMaxBytes). Field naming is snake_case
// to match the rest of the agent's HTTP surface.
type ExecRequest struct {
	Cmd     []string `json:"cmd"`
	Env     []string `json:"env,omitempty"`     // KEY=VALUE; merged with the agent's env
	WorkDir string   `json:"workdir,omitempty"` // empty = inherit
	TTY     bool     `json:"tty,omitempty"`     // PTY-allocated execution
	Cols    int      `json:"cols,omitempty"`    // initial PTY width (TTY mode only)
	Rows    int      `json:"rows,omitempty"`    // initial PTY height
}

// Signal payload format (channel = ChannelSignal). First byte is the
// signal kind discriminant; the rest is kind-specific. Kept tiny so
// host clients can encode in a few lines.
const (
	signalKindKill    byte = 's' // 1 byte: signal number (e.g. 15 for SIGTERM)
	signalKindWinsize byte = 'w' // 4 bytes: cols(u16 BE) + rows(u16 BE)
)

const (
	// Bounded handshake to keep a slow/garbage client from pinning a
	// goroutine forever between hijack and process spawn. 64 KiB is
	// generous for cmd + env (PLAN §4); over → 400 + close.
	handshakeMaxBytes = 64 << 10
	handshakeTimeout  = 5 * time.Second

	// Grace period between SIGTERM and SIGKILL when the host closes
	// the connection mid-exec. Matches the docker-stop default.
	killGrace = 2 * time.Second

	// Output chunk size for process → host frames. 32 KiB is a
	// kernel pipe-buffer-friendly read; output larger than this
	// gets split into multiple frames automatically. With the host
	// vsock device now sending opCreditUpdate after every TX
	// payload, there is no longer a 64-KiB stall at the bridge.
	outputChunkSize = 32 << 10
)

func handleExec(w http.ResponseWriter, r *http.Request) {
	// Hijack first, then read+validate the handshake from the buffered
	// reader. We do NOT decode the JSON via r.Body because under some
	// transports (notably the UDS + virtio-vsock bridge we use in
	// production) reading r.Body before Hijack causes downstream writes
	// to wedge silently — see commit history for the long debugging
	// session that surfaced this. brw.Reader exposes the same bytes
	// (Go's http server stages the request body through the same
	// bufio.Reader that gets handed back from Hijack).
	hj, ok := w.(http.Hijacker)
	if !ok {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("connection does not support hijack"))
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("hijack: %w", err))
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Time{})

	// Acknowledge the hijack with a minimal HTTP response so the
	// client knows the protocol switch succeeded. After the blank
	// line we drop into framed binary on the same conn — the rest
	// of the response body is interpreted as a stream of frames.
	if _, err := conn.Write([]byte("HTTP/1.0 200 OK\r\nContent-Type: application/octet-stream\r\nConnection: close\r\n\r\n")); err != nil {
		return
	}

	// Read the handshake JSON from brw.Reader with a deadline +
	// bounded length. The client wrote it as the HTTP request body
	// per Content-Length; those bytes are sitting in brw.Reader.
	cl := r.ContentLength
	if cl <= 0 || cl > handshakeMaxBytes {
		_, _ = WriteFrame(conn, ChannelStderr, []byte(fmt.Sprintf("toolbox exec: invalid Content-Length=%d\n", cl)))
		_ = WriteExitFrame(conn, -1)
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	hsBuf := make([]byte, cl)
	if _, err := io.ReadFull(brw.Reader, hsBuf); err != nil {
		_, _ = WriteFrame(conn, ChannelStderr, []byte("toolbox exec: short handshake read: "+err.Error()+"\n"))
		_ = WriteExitFrame(conn, -1)
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	var req ExecRequest
	if err := json.Unmarshal(hsBuf, &req); err != nil {
		_, _ = WriteFrame(conn, ChannelStderr, []byte("toolbox exec: decode handshake: "+err.Error()+"\n"))
		_ = WriteExitFrame(conn, -1)
		return
	}
	if len(req.Cmd) == 0 {
		_, _ = WriteFrame(conn, ChannelStderr, []byte("toolbox exec: cmd[] required and non-empty\n"))
		_ = WriteExitFrame(conn, -1)
		return
	}

	if err := runExecSession(r.Context(), conn, brw, req); err != nil {
		_, _ = WriteFrame(conn, ChannelStderr, []byte("toolbox exec: "+err.Error()+"\n"))
		_ = WriteExitFrame(conn, -1)
	}
}

// runExecSession spawns the requested process, multiplexes I/O over
// the framed protocol on conn, and emits the EXIT frame when done.
// brw carries any bytes already read from conn during HTTP parsing —
// we hand it to the read side so the client can pipeline the first
// stdin frame after the handshake JSON without a round-trip.
func runExecSession(ctx context.Context, conn net.Conn, brw *bufio.ReadWriter, req ExecRequest) error {
	cmd := exec.CommandContext(ctx, req.Cmd[0], req.Cmd[1:]...)
	if req.WorkDir != "" {
		cmd.Dir = req.WorkDir
	}
	// Merge req.Env with the agent's environment (PATH, HOME, etc.
	// that init.go configured for the toolbox process). Replacing
	// cmd.Env wholesale with only req.Env was a footgun — callers
	// that set a single KEY=VALUE ended up losing PATH and then
	// exec.LookPath failed for trivial commands like "echo".
	if len(req.Env) > 0 {
		cmd.Env = append(os.Environ(), req.Env...)
	}

	// frameWriter serializes WriteFrame calls — multiple goroutines
	// (stdout, stderr, exit, signal echoes) write to the same conn.
	var writeMu sync.Mutex
	emit := func(channel byte, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_, err := WriteFrame(conn, channel, payload)
		return err
	}
	emitExit := func(code int32) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return WriteExitFrame(conn, code)
	}

	if req.TTY {
		return runWithPTY(ctx, conn, brw, cmd, req, emit, emitExit)
	}
	return runWithPipes(ctx, conn, brw, cmd, emit, emitExit)
}

// runWithPipes spawns cmd with explicit stdin/stdout/stderr pipes and
// fans bytes from each onto the framed protocol.
func runWithPipes(
	ctx context.Context,
	conn net.Conn,
	brw *bufio.ReadWriter,
	cmd *exec.Cmd,
	emit func(byte, []byte) error,
	emitExit func(int32) error,
) error {
	stdinW, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutR, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderrR, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	// Pump output → frames. CRITICAL: drain BEFORE cmd.Wait. Per
	// os/exec docs, "it is incorrect to call Wait before all reads
	// from the pipe have completed" — Wait's cleanup() closes the
	// parent end of the pipes, dropping any kernel-buffered bytes
	// the pump hasn't yet read. Concurrent sessions hit this race
	// hardest (small stdout, fast exit). Draining first is safe
	// because the pumps see EOF naturally when the child closes its
	// write end on exit.
	var outWG sync.WaitGroup
	outWG.Add(2)
	go pumpReaderToFrames(stdoutR, ChannelStdout, emit, &outWG)
	go pumpReaderToFrames(stderrR, ChannelStderr, emit, &outWG)

	// Pump input frames → stdin / signals. Returns when the client
	// closes the conn or sends EOF on stdin. clientGone triggers
	// the SIGTERM-then-SIGKILL escalation if the process is still
	// running.
	clientGone := make(chan struct{})
	go func() {
		pumpFramesToProcess(brw.Reader, stdinW, nil, cmd)
		close(clientGone)
	}()

	processDone := make(chan struct{})
	go escalateOnClientGone(cmd, clientGone, processDone)

	outWG.Wait() // pumps drained — child wrote everything and EOF'd
	waitErr := cmd.Wait()
	close(processDone)

	code := exitCodeFromWait(waitErr)
	return emitExit(int32(code))
}

// runWithPTY spawns cmd attached to a freshly allocated pseudo-TTY.
// stdout and stderr are merged on the master fd; resize is handled via
// ChannelSignal frames.
func runWithPTY(
	ctx context.Context,
	conn net.Conn,
	brw *bufio.ReadWriter,
	cmd *exec.Cmd,
	req ExecRequest,
	emit func(byte, []byte) error,
	emitExit func(int32) error,
) error {
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("pty start: %w", err)
	}
	defer ptmx.Close()
	if req.Cols > 0 || req.Rows > 0 {
		_ = pty.Setsize(ptmx, &pty.Winsize{
			Cols: clampUint16(req.Cols),
			Rows: clampUint16(req.Rows),
		})
	}

	// Single output pump — TTY merges stdout/stderr.
	var outWG sync.WaitGroup
	outWG.Add(1)
	go pumpReaderToFrames(ptmx, ChannelStdout, emit, &outWG)

	clientGone := make(chan struct{})
	go func() {
		pumpFramesToProcess(brw.Reader, ptmx, ptmx, cmd)
		close(clientGone)
	}()

	processDone := make(chan struct{})
	go escalateOnClientGone(cmd, clientGone, processDone)

	// Same drain-first ordering as runWithPipes: the output pump on
	// ptmx exits when the child closes the slave (on exit). After
	// that we cmd.Wait to reap, then force the input pump to exit.
	outWG.Wait()
	waitErr := cmd.Wait()
	close(processDone)
	// Cleanup ordering: the input pump goroutine may still be holding
	// ptmx and calling pty.Setsize on it from a queued winsize frame.
	// Closing ptmx while Setsize is in flight is a data race on the
	// underlying fd. Force the input pump to exit by setting a
	// past-deadline on the conn (its ReadFrame errors out and the
	// pump's defer closes ptmx for us). We avoid closing the conn
	// here because we still need to emit EXIT on it below.
	_ = conn.SetReadDeadline(time.Now())
	<-clientGone
	// Restore the conn for write — the read deadline is irrelevant
	// once the input pump is gone.
	_ = conn.SetDeadline(time.Time{})
	_ = ptmx // keep reference live until here; input pump closes via stdinW alias

	code := exitCodeFromWait(waitErr)
	return emitExit(int32(code))
}

// escalateOnClientGone watches both client-gone and process-done. If
// the client disappears first, escalates SIGTERM → SIGKILL with the
// killGrace deadline. If the process exits first, returns silently.
func escalateOnClientGone(cmd *exec.Cmd, clientGone, processDone <-chan struct{}) {
	select {
	case <-clientGone:
	case <-processDone:
		return
	}
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-time.After(killGrace):
		_ = cmd.Process.Kill()
	case <-processDone:
	}
}

// pumpReaderToFrames reads from r in MaxFrameLen-sized chunks (or
// smaller as bytes arrive) and emits each chunk as a frame on
// `channel`. Done() is signaled on EOF or error so the caller can
// wait for the final flush before emitting EXIT.
func pumpReaderToFrames(r io.Reader, channel byte, emit func(byte, []byte) error, wg *sync.WaitGroup) {
	defer wg.Done()
	buf := make([]byte, outputChunkSize)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if werr := emit(channel, buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// pumpFramesToProcess reads frames from the client and dispatches them.
// stdin frames go to stdinW; signal frames are translated into kill /
// resize syscalls. ptyMaster is non-nil only in PTY mode and is used
// for TIOCSWINSZ-style winsize updates. Returns when the client
// closes the conn or sends a zero-byte stdin frame in non-PTY mode
// (= canonical EOF — Ctrl-D in PTY mode is in-band byte 0x04 and not
// signalled separately).
func pumpFramesToProcess(r io.Reader, stdinW io.WriteCloser, ptyMaster *os.File, cmd *exec.Cmd) {
	ptyMode := ptyMaster != nil
	for {
		channel, payload, err := ReadFrame(r)
		if err != nil {
			// EOF or anything else: close stdin so the process sees
			// EOF and can exit gracefully. Escalation to SIGTERM is
			// handled by escalateOnClientGone.
			_ = stdinW.Close()
			return
		}
		switch channel {
		case ChannelStdin:
			if len(payload) == 0 && !ptyMode {
				_ = stdinW.Close()
				continue
			}
			if _, werr := stdinW.Write(payload); werr != nil {
				return
			}
		case ChannelSignal:
			dispatchSignal(payload, ptyMaster, cmd)
		default:
			// Unknown channel — ignore. We don't want to die because a
			// future client speaks a newer dialect.
		}
	}
}

func dispatchSignal(payload []byte, ptyMaster *os.File, cmd *exec.Cmd) {
	if len(payload) < 1 || cmd.Process == nil {
		return
	}
	switch payload[0] {
	case signalKindKill:
		if len(payload) < 2 {
			return
		}
		_ = cmd.Process.Signal(syscall.Signal(payload[1]))
	case signalKindWinsize:
		if ptyMaster == nil || len(payload) < 5 {
			return
		}
		cols := binary.BigEndian.Uint16(payload[1:3])
		rows := binary.BigEndian.Uint16(payload[3:5])
		_ = pty.Setsize(ptyMaster, &pty.Winsize{Cols: cols, Rows: rows})
	}
}

func exitCodeFromWait(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		// ExitCode() returns -1 if the process was killed by signal.
		// POSIX convention is 128+sig; we keep the raw -1 here so the
		// host can distinguish "killed by signal" from a real -1 exit.
		return ee.ExitCode()
	}
	// Process couldn't be started, or context cancelled, etc. Use
	// 127 (the standard "command not found / not executable") so the
	// host has a non-zero exit to surface.
	return 127
}

func clampUint16(v int) uint16 {
	if v < 0 {
		return 0
	}
	if v > 0xFFFF {
		return 0xFFFF
	}
	return uint16(v)
}
