package console

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"github.com/gocracker/gocracker/internal/hostguard"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

type Mode string

const (
	ModeAuto  Mode = "auto"
	ModeOff   Mode = "off"
	ModeForce Mode = "force"
)

func ParseMode(raw string) (Mode, error) {
	mode := Mode(raw)
	switch mode {
	case "", ModeAuto:
		return ModeAuto, nil
	case ModeOff, ModeForce:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid tty mode %q (want auto, off, or force)", raw)
	}
}

type Session struct {
	master *os.File
	slave  *os.File
	stdin  *os.File
	stdout *os.File

	rawState *term.State
	sigCh    chan os.Signal
	detachCh chan struct{}
	stopCh   chan struct{}

	closeOnce  sync.Once
	detachOnce sync.Once
	stopOnce   sync.Once
}

func NewSession(mode Mode, wait bool, stdin, stdout *os.File) (*Session, error) {
	if !wait {
		if mode == ModeForce {
			return nil, fmt.Errorf("--tty=force requires --wait")
		}
		return nil, nil
	}
	if mode == ModeOff {
		return nil, nil
	}

	isTTY := stdin != nil && stdout != nil &&
		term.IsTerminal(int(stdin.Fd())) &&
		term.IsTerminal(int(stdout.Fd()))
	if mode == ModeAuto && !isTTY {
		return nil, nil
	}
	if mode == ModeForce && !isTTY {
		return nil, fmt.Errorf("--tty=force requires a real terminal on stdin/stdout")
	}

	master, slave, err := pty.Open()
	if err != nil {
		return nil, wrapPTYOpenError(err)
	}
	// Put the host-side pty slave into raw mode. The slave is consumed by
	// the VMM as a byte pipe into the UART; if it stays in the default
	// cooked mode the slave's line discipline intercepts ISIG control
	// chars (VINTR=0x03, VQUIT=0x1c, VSUSP=0x1a, VEOF=0x04) and never
	// forwards them to the reader, so Ctrl-C/Ctrl-\ never reach the guest.
	if _, err := term.MakeRaw(int(slave.Fd())); err != nil {
		_ = slave.Close()
		_ = master.Close()
		return nil, fmt.Errorf("set pty slave raw: %w", err)
	}
	return &Session{
		master:   master,
		slave:    slave,
		stdin:    stdin,
		stdout:   stdout,
		detachCh: make(chan struct{}),
		stopCh:   make(chan struct{}),
	}, nil
}

func wrapPTYOpenError(err error) error {
	if err == nil {
		return nil
	}
	if diagnoseErr := hostguard.CheckPTYSupport(); diagnoseErr != nil {
		return fmt.Errorf("%w (%v)", err, diagnoseErr)
	}
	msg := err.Error()
	if msg != "out of pty devices" {
		return err
	}
	return fmt.Errorf("%w (host PTY support is unhealthy; check /dev/ptmx and /dev/pts mount options)", err)
}

func (s *Session) ConsoleIn() io.Reader {
	if s == nil {
		return nil
	}
	return s.slave
}

func (s *Session) ConsoleOut() io.Writer {
	if s == nil {
		return nil
	}
	return s.slave
}

func (s *Session) Detached() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.detachCh
}

func (s *Session) StopRequested() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.stopCh
}

func (s *Session) Start() error {
	if s == nil {
		return nil
	}
	rawState, err := term.MakeRaw(int(s.stdin.Fd()))
	if err != nil {
		return err
	}
	s.rawState = rawState
	_ = pty.InheritSize(s.stdin, s.slave)

	s.sigCh = make(chan os.Signal, 1)
	signal.Notify(s.sigCh, syscall.SIGWINCH)
	go func() {
		for range s.sigCh {
			_ = pty.InheritSize(s.stdin, s.slave)
		}
	}()

	go s.pumpOutput()
	go s.pumpInput()
	return nil
}

func (s *Session) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		if s.sigCh != nil {
			signal.Stop(s.sigCh)
			close(s.sigCh)
		}
		if s.slave != nil {
			_ = s.slave.Close()
		}
		if s.master != nil {
			_ = s.master.Close()
		}
		drainInput(s.stdin)
		if s.rawState != nil {
			_ = term.Restore(int(s.stdin.Fd()), s.rawState)
		}
	})
}

func (s *Session) detach() {
	s.detachOnce.Do(func() {
		if s.detachCh != nil {
			close(s.detachCh)
		}
	})
}

func (s *Session) requestStop() {
	s.stopOnce.Do(func() {
		if s.stopCh != nil {
			close(s.stopCh)
		}
	})
}

func (s *Session) pumpInput() {
	// Pass user input straight to the master pty, with two narrow filters:
	//
	//   1. Detach trigger Ctrl-] (0x1d). Same convention as telnet/screen.
	//      Ctrl-C (0x03) is forwarded to the guest unchanged so the guest
	//      shell can deliver SIGINT to its foreground process group.
	//
	//   2. Drop two specific terminal-reply sequences that the host
	//      terminal emits in RESPONSE to queries the guest sent us
	//      (cursor-position report `\x1b[N;NR` and bracketed-paste toggle
	//      acks `\x1b[?2004{h,l}`). Those bytes come from the host TTY
	//      driver, not from the user, but they end up on stdin and would
	//      otherwise be typed at the guest prompt.
	//
	// Anything else — including arrow keys (`\x1b[A`/B/C/D), function
	// keys, alt-prefixed bytes, regular ASCII — is forwarded byte for
	// byte. The OLD reply filter ate those because it ran the input
	// stream through the same parser that strips reply sequences from
	// the OUTPUT path; that was the bug behind `top` arriving as `tp`,
	// `exit` arriving as `xt`, etc.
	const detachByte = 0x1d
	filter := newInputReplyFilter()
	buf := make([]byte, 1024)
	for {
		n, err := s.stdin.Read(buf)
		if n > 0 {
			data := filter.process(buf[:n])
			if idx := bytes.IndexByte(data, detachByte); idx >= 0 {
				if idx > 0 {
					if _, writeErr := s.master.Write(data[:idx]); writeErr != nil {
						return
					}
				}
				s.requestStop()
				s.detach()
				return
			}
			if len(data) > 0 {
				if _, writeErr := s.master.Write(data); writeErr != nil {
					return
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				_, _ = s.master.Write([]byte{0x04})
				s.detach()
			}
			return
		}
	}
}

// inputReplyFilter strips terminal-emulator replies that the HOST tty
// echoes back into our stdin in response to queries the GUEST emitted.
// Currently it removes:
//   - `\x1b[N;NR`           cursor position report
//   - `\x1b[?2004h` / `?2004l` bracketed-paste mode toggle ack
//
// All other bytes (arrows, function keys, regular text, control chars)
// pass through. A short carry buffer covers an escape sequence that
// arrives split across two stdin reads.
type inputReplyFilter struct {
	carry []byte
}

func newInputReplyFilter() *inputReplyFilter { return &inputReplyFilter{} }

func (f *inputReplyFilter) process(data []byte) []byte {
	if len(f.carry) > 0 {
		data = append(append([]byte(nil), f.carry...), data...)
		f.carry = nil
	}
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); {
		if data[i] != 0x1b {
			out = append(out, data[i])
			i++
			continue
		}
		// Need at least `\x1b[` to start a CSI sequence; otherwise pass
		// the lone ESC through (it could be Alt+key or a partial that
		// will resolve next read; either way the user typed it).
		if i+1 >= len(data) {
			f.carry = append(f.carry, data[i])
			return out
		}
		if data[i+1] != '[' {
			out = append(out, data[i])
			i++
			continue
		}
		// Walk to the final byte of the CSI sequence (in [0x40,0x7e]).
		end := i + 2
		for end < len(data) {
			b := data[end]
			if b >= 0x40 && b <= 0x7e {
				break
			}
			end++
		}
		if end >= len(data) {
			f.carry = append(f.carry, data[i:]...)
			return out
		}
		seq := data[i : end+1]
		if isHostTerminalReply(seq) {
			i = end + 1
			continue
		}
		out = append(out, seq...)
		i = end + 1
	}
	return out
}

// isHostTerminalReply returns true for CSI sequences that the host TTY
// emits as a *response* to a query, not as a user keystroke.
func isHostTerminalReply(seq []byte) bool {
	if len(seq) < 3 || seq[0] != 0x1b || seq[1] != '[' {
		return false
	}
	// Bracketed-paste mode toggle ack: `\x1b[?2004h` / `\x1b[?2004l`.
	if bytes.Equal(seq, []byte("\x1b[?2004h")) || bytes.Equal(seq, []byte("\x1b[?2004l")) {
		return true
	}
	final := seq[len(seq)-1]
	// Cursor-position report: `\x1b[<row>;<col>R` (or with leading `?`).
	if final != 'R' {
		return false
	}
	for _, b := range seq[2 : len(seq)-1] {
		if (b < '0' || b > '9') && b != ';' && b != '?' {
			return false
		}
	}
	return true
}

func (s *Session) pumpOutput() {
	filter := terminalOutputFilter{}
	buf := make([]byte, 1024)
	for {
		n, err := s.master.Read(buf)
		if n > 0 {
			payload, reply := filter.Filter(buf[:n])
			if len(payload) > 0 {
				if _, writeErr := s.stdout.Write(payload); writeErr != nil {
					return
				}
			}
			if len(reply) > 0 {
				if _, writeErr := s.master.Write(reply); writeErr != nil {
					return
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				if tail := filter.Flush(); len(tail) > 0 {
					_, _ = s.stdout.Write(tail)
				}
			}
			return
		}
	}
}

func drainInput(file *os.File) {
	if file == nil {
		return
	}
	fd := int(file.Fd())
	if err := unix.SetNonblock(fd, true); err != nil {
		return
	}
	defer unix.SetNonblock(fd, false) //nolint:errcheck

	buf := make([]byte, 256)
	for {
		_, err := unix.Read(fd, buf)
		if err == nil {
			continue
		}
		if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
			return
		}
		return
	}
}

type terminalInputFilter struct {
	r       io.Reader
	pending []byte
	carry   []byte
}

type terminalOutputFilter struct {
	carry []byte
}

func newTerminalInputFilter(r io.Reader) io.Reader {
	return &terminalInputFilter{r: r}
}

func (f *terminalInputFilter) Read(p []byte) (int, error) {
	for len(f.pending) == 0 {
		buf := make([]byte, 1024)
		n, err := f.r.Read(buf)
		if len(f.carry) > 0 {
			buf = append(append([]byte{}, f.carry...), buf[:n]...)
			f.carry = nil
			n = len(buf)
		} else {
			buf = buf[:n]
		}
		if n > 0 {
			filtered, carry := filterTerminalReplies(buf)
			f.pending = append(f.pending, filtered...)
			f.carry = carry
		}
		if len(f.pending) > 0 {
			break
		}
		if err != nil {
			if err == io.EOF && len(f.carry) > 0 {
				f.pending = append(f.pending, f.carry...)
				f.carry = nil
				break
			}
			return 0, err
		}
	}
	n := copy(p, f.pending)
	f.pending = f.pending[n:]
	return n, nil
}

func (f *terminalOutputFilter) Filter(data []byte) ([]byte, []byte) {
	if len(f.carry) > 0 {
		data = append(append([]byte{}, f.carry...), data...)
		f.carry = nil
	}
	payload, reply, carry := filterTerminalQueries(data)
	f.carry = carry
	return payload, reply
}

func (f *terminalOutputFilter) Flush() []byte {
	if len(f.carry) == 0 {
		return nil
	}
	tail := append([]byte{}, f.carry...)
	f.carry = nil
	return tail
}

func filterTerminalReplies(data []byte) ([]byte, []byte) {
	var out bytes.Buffer
	for i := 0; i < len(data); {
		if data[i] != 0x1b {
			out.WriteByte(data[i])
			i++
			continue
		}
		if i+1 >= len(data) {
			return out.Bytes(), append([]byte{}, data[i:]...)
		}
		if data[i+1] != '[' {
			out.WriteByte(data[i])
			i++
			continue
		}
		end := i + 2
		for end < len(data) {
			b := data[end]
			if b >= 0x40 && b <= 0x7e {
				break
			}
			end++
		}
		if end >= len(data) {
			return out.Bytes(), append([]byte{}, data[i:]...)
		}
		if shouldDropTerminalReply(data[i : end+1]) {
			i = end + 1
			continue
		}
		out.Write(data[i : end+1])
		i = end + 1
	}
	return out.Bytes(), nil
}

func filterTerminalQueries(data []byte) ([]byte, []byte, []byte) {
	var out bytes.Buffer
	var reply bytes.Buffer
	for i := 0; i < len(data); {
		if data[i] != 0x1b {
			out.WriteByte(data[i])
			i++
			continue
		}
		if i+1 >= len(data) {
			return out.Bytes(), reply.Bytes(), append([]byte{}, data[i:]...)
		}
		if data[i+1] != '[' {
			out.WriteByte(data[i])
			i++
			continue
		}
		end := i + 2
		for end < len(data) {
			b := data[end]
			if b >= 0x40 && b <= 0x7e {
				break
			}
			end++
		}
		if end >= len(data) {
			return out.Bytes(), reply.Bytes(), append([]byte{}, data[i:]...)
		}
		seq := data[i : end+1]
		if response, ok := terminalQueryResponse(seq); ok {
			reply.Write(response)
			i = end + 1
			continue
		}
		if shouldDropTerminalReply(seq) {
			i = end + 1
			continue
		}
		out.Write(seq)
		i = end + 1
	}
	return out.Bytes(), reply.Bytes(), nil
}

// filterConsoleInput is retained as a thin pass-through to keep the
// surrounding test surface stable. The old behaviour also routed user input
// through filterTerminalReplies and treated Ctrl-C (0x03) as a detach
// trigger, both of which were wrong: the reply filter is for the OUTPUT
// path and ate keystrokes whose byte stream contained a partial `\x1b[`,
// while the detach-on-Ctrl-C broke SIGINT delivery in the guest.
//
// pumpInput now writes stdin straight to the master pty and uses Ctrl-]
// (0x1d) as the explicit detach trigger.
func filterConsoleInput(data []byte) ([]byte, bool) {
	return data, false
}

func terminalQueryResponse(seq []byte) ([]byte, bool) {
	if bytes.Equal(seq, []byte("\x1b[6n")) || bytes.Equal(seq, []byte("\x1b[?6n")) {
		return []byte("\x1b[1;1R"), true
	}
	return nil, false
}

func shouldDropTerminalReply(seq []byte) bool {
	if bytes.Equal(seq, []byte("\x1b[?2004h")) || bytes.Equal(seq, []byte("\x1b[?2004l")) {
		return true
	}
	if len(seq) < 3 || seq[0] != 0x1b || seq[1] != '[' {
		return false
	}
	final := seq[len(seq)-1]
	if final != 'R' && final != 'n' {
		return false
	}
	for _, b := range seq[2 : len(seq)-1] {
		if (b < '0' || b > '9') && b != ';' && b != '?' {
			return false
		}
	}
	return true
}
