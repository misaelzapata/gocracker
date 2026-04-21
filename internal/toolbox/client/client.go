// Package client is the host-side library for talking to the gocracker
// toolbox agent over a Firecracker-style UDS (Fase 1) bridged to vsock
// 10023 (Fase 2 agent). It hides the wire protocol — callers pass an
// ExecRequest + io.Readers/Writers and get back an exit code.
//
// Two surfaces:
//
//   Health(ctx) — one-shot GET /healthz, returns {ok, version}.
//
//   Exec(ctx, req, stdin, stdout, stderr) (exit, error) — runs one
//   command. stdin is read until EOF and forwarded as ChannelStdin
//   frames; stdout/stderr writers receive whatever frames the agent
//   emits. Blocks until the agent emits ChannelExit. Closing stdin
//   early (returning EOF before the process exits) sends an empty
//   frame to signal half-close, which the agent treats as POSIX EOF.
//
//   Stream(ctx, req) (*Session, error) — opens an exec session for
//   callers that need signal delivery, PTY resize, or async I/O.
//
// All three create a fresh net.Conn per call (CONNECT N\n to the UDS
// + read OK\n + send the HTTP request). There is no connection pool
// — UDS+CONNECT is sub-millisecond and pooling would just hide
// per-conn lifetime bugs.
package client

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gocracker/gocracker/internal/toolbox/agent"
	toolboxspec "github.com/gocracker/gocracker/internal/toolbox/spec"
)

// Client is the cheap-to-construct entrypoint. Call New once per VM
// (or per UDS path) and reuse for many Health/Exec/Stream calls.
type Client struct {
	// UDSPath is the absolute path to the Firecracker-style vsock
	// UDS the gocracker runtime exposes for this VM. Required.
	UDSPath string

	// Port is the guest vsock port the toolbox agent listens on.
	// Defaults to toolboxspec.VsockPort (10023) when zero.
	Port uint32

	// DialTimeout caps the UDS dial + CONNECT handshake. Defaults
	// to 5s, which is generous for a sub-millisecond local hop.
	DialTimeout time.Duration
}

// New builds a Client with the default port + dial timeout. Same as
// Client{UDSPath: udsPath} but explicit.
func New(udsPath string) *Client {
	return &Client{UDSPath: udsPath}
}

// Health performs a single GET /healthz round-trip and returns the
// agent's reported state. Useful as a readiness probe — a successful
// call proves the UDS bridge, vsock, AND agent are all live.
func (c *Client) Health(ctx context.Context) (agent.Health, error) {
	conn, err := c.dialAndConnect(ctx)
	if err != nil {
		return agent.Health{}, err
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "GET /healthz HTTP/1.0\r\nHost: x\r\nConnection: close\r\n\r\n"); err != nil {
		return agent.Health{}, fmt.Errorf("write /healthz: %w", err)
	}
	br := bufio.NewReader(conn)
	if err := skipHTTPHeaders(br); err != nil {
		return agent.Health{}, err
	}
	var h agent.Health
	if err := json.NewDecoder(br).Decode(&h); err != nil {
		return agent.Health{}, fmt.Errorf("decode /healthz body: %w", err)
	}
	return h, nil
}

// ExecResult is the synchronous Exec return: the exit code surfaced
// by the agent. Note that an exit of -1 means the process was
// killed by signal (POSIX convention is 128+sig but the agent
// preserves the raw -1 so the caller can distinguish).
type ExecResult struct {
	ExitCode int32
}

// Exec runs req synchronously. stdin is read until EOF (nil = no
// stdin). stdout/stderr receive bytes from the corresponding agent
// channels. Returns the exit code or an error if the wire breaks
// before the EXIT frame arrives.
func (c *Client) Exec(
	ctx context.Context,
	req agent.ExecRequest,
	stdin io.Reader,
	stdout, stderr io.Writer,
) (ExecResult, error) {
	sess, err := c.Stream(ctx, req)
	if err != nil {
		return ExecResult{}, err
	}
	// Pump stdin in a goroutine so we can also read frames inline.
	stdinDone := make(chan error, 1)
	go func() {
		if stdin == nil {
			_ = sess.CloseStdin()
			stdinDone <- nil
			return
		}
		if _, err := io.Copy(sess, stdin); err != nil {
			stdinDone <- err
			return
		}
		stdinDone <- sess.CloseStdin()
	}()

	exit, err := sess.copyOutputUntilExit(stdout, stderr)
	_ = sess.Close()
	<-stdinDone
	if err != nil {
		return ExecResult{}, err
	}
	return ExecResult{ExitCode: exit}, nil
}

// Stream opens an exec session for callers that need streaming I/O,
// signal delivery, or PTY resize between Wait calls.
func (c *Client) Stream(ctx context.Context, req agent.ExecRequest) (*Session, error) {
	conn, err := c.dialAndConnect(ctx)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("marshal exec request: %w", err)
	}
	httpReq := fmt.Sprintf(
		"POST /exec HTTP/1.0\r\nContent-Length: %d\r\nContent-Type: application/json\r\nConnection: close\r\n\r\n",
		len(body),
	)
	if _, err := conn.Write([]byte(httpReq)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write exec request line+headers: %w", err)
	}
	if _, err := conn.Write(body); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write exec request body: %w", err)
	}
	br := bufio.NewReader(conn)
	if err := skipHTTPHeaders(br); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &Session{conn: conn, br: br}, nil
}

// Session is the streaming exec handle. Methods are safe for
// concurrent use. Close() is idempotent.
type Session struct {
	conn net.Conn
	br   *bufio.Reader

	writeMu sync.Mutex
}

// Write implements io.Writer for stdin. Each Write becomes one
// ChannelStdin frame (capped at agent.MaxFrameLen — callers writing
// larger blobs should use io.Copy with a smaller buffer or chunk
// themselves).
func (s *Session) Write(p []byte) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := agent.WriteFrame(s.conn, agent.ChannelStdin, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// CloseStdin sends an empty ChannelStdin frame, the canonical EOF
// signal in non-PTY mode. The process sees its stdin close and may
// exit. In PTY mode this is a no-op for the kernel pty (Ctrl-D as
// in-band 0x04 is the equivalent).
func (s *Session) CloseStdin() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := agent.WriteFrame(s.conn, agent.ChannelStdin, nil)
	return err
}

// Signal sends a kill-style signal (SIGTERM/SIGKILL/SIGINT/etc) to
// the running process. Returns an error if the wire is dead.
func (s *Session) Signal(sig syscall.Signal) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	payload := []byte{'s', byte(sig)}
	_, err := agent.WriteFrame(s.conn, agent.ChannelSignal, payload)
	return err
}

// Resize sends a TIOCSWINSZ-equivalent to the running process's
// controlling pty. Has no effect when the session was started with
// TTY:false.
func (s *Session) Resize(cols, rows uint16) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	payload := make([]byte, 5)
	payload[0] = 'w'
	binary.BigEndian.PutUint16(payload[1:3], cols)
	binary.BigEndian.PutUint16(payload[3:5], rows)
	_, err := agent.WriteFrame(s.conn, agent.ChannelSignal, payload)
	return err
}

// NextFrame reads the next frame from the agent. Returns io.EOF
// after the agent emits ChannelExit and closes the conn. Callers
// using io.Reader-style stdout/stderr handling should prefer
// copyOutputUntilExit (used by Client.Exec) instead.
func (s *Session) NextFrame() (channel byte, payload []byte, err error) {
	return agent.ReadFrame(s.br)
}

// Close terminates the session — closes the conn, which signals the
// agent to SIGTERM the running process if it hasn't exited yet.
func (s *Session) Close() error { return s.conn.Close() }

// copyOutputUntilExit pumps frames until the EXIT frame arrives,
// fanning stdout/stderr to the supplied writers. Used by the
// synchronous Exec helper.
func (s *Session) copyOutputUntilExit(stdout, stderr io.Writer) (int32, error) {
	for {
		ch, payload, err := s.NextFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0, fmt.Errorf("agent closed conn before EXIT frame")
			}
			return 0, err
		}
		switch ch {
		case agent.ChannelStdout:
			if stdout != nil {
				if _, werr := stdout.Write(payload); werr != nil {
					return 0, fmt.Errorf("stdout writer: %w", werr)
				}
			}
		case agent.ChannelStderr:
			if stderr != nil {
				if _, werr := stderr.Write(payload); werr != nil {
					return 0, fmt.Errorf("stderr writer: %w", werr)
				}
			}
		case agent.ChannelExit:
			code, err := agent.ParseExitPayload(payload)
			if err != nil {
				return 0, err
			}
			return code, nil
		default:
			// Ignore unknown channels — forward-compatible.
		}
	}
}

// dialAndConnect opens the UDS, sends "CONNECT <port>\n", validates
// the OK response, and returns the raw conn ready for HTTP.
func (c *Client) dialAndConnect(ctx context.Context) (net.Conn, error) {
	if c.UDSPath == "" {
		return nil, fmt.Errorf("toolbox client: UDSPath is required")
	}
	port := c.Port
	if port == 0 {
		port = toolboxspec.VsockPort
	}
	timeout := c.DialTimeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "unix", c.UDSPath)
	if err != nil {
		return nil, fmt.Errorf("dial UDS %s: %w", c.UDSPath, err)
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write CONNECT: %w", err)
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read CONNECT response: %w", err)
	}
	got := strings.TrimRight(line, "\r\n")
	if got != "OK" {
		_ = conn.Close()
		return nil, fmt.Errorf("CONNECT rejected: %q", got)
	}
	// Clear the deadline now that the bridge is up; subsequent reads
	// have their own timeouts (or none, for long-running execs).
	_ = conn.SetDeadline(time.Time{})
	// We can't return the bufio.Reader wrapper to the caller because
	// the HTTP request goes out on conn directly. The "OK\n" is the
	// last line read from the bridge layer; nothing else was buffered.
	return conn, nil
}

// skipHTTPHeaders reads from br until it consumes the blank line
// terminating the HTTP response headers. Anything after this is the
// raw response body — for /exec, that's the framed binary stream.
func skipHTTPHeaders(br *bufio.Reader) error {
	statusLine, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read status line: %w", err)
	}
	if !strings.HasPrefix(statusLine, "HTTP/1.0 200") && !strings.HasPrefix(statusLine, "HTTP/1.1 200") {
		return fmt.Errorf("agent did not return 200: %q", strings.TrimRight(statusLine, "\r\n"))
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read response headers: %w", err)
		}
		if line == "\r\n" || line == "\n" {
			return nil
		}
	}
}
