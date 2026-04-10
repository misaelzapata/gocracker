package console

import (
	"bytes"
	"io"
	"os"
	"testing"
	"time"
)

type chunkReader struct {
	chunks [][]byte
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	chunk := r.chunks[0]
	r.chunks = r.chunks[1:]
	n := copy(p, chunk)
	return n, nil
}

func TestFilterConsoleInputIsPassThrough(t *testing.T) {
	// filterConsoleInput is now a pass-through. The reply filter belongs
	// on the OUTPUT path (terminalOutputFilter); applying it to user input
	// silently swallowed keystrokes whenever the byte stream looked like a
	// partial CSI sequence.
	out, detach := filterConsoleInput([]byte("abc\x1b[13;5Rdef"))
	if detach {
		t.Fatalf("detach = true, want false")
	}
	if got, want := string(out), "abc\x1b[13;5Rdef"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestFilterConsoleInputPassesThroughCtrlC(t *testing.T) {
	// Ctrl-C is no longer the detach trigger; it must be forwarded to the
	// guest unchanged so the guest shell can deliver SIGINT to its
	// foreground process group. Detach now uses Ctrl-] (0x1d).
	out, detach := filterConsoleInput([]byte("whoami\x03ignored"))
	if detach {
		t.Fatalf("detach = true, want false (Ctrl-C must pass through)")
	}
	if !bytes.Equal(out, []byte("whoami\x03ignored")) {
		t.Fatalf("output = %q, want %q", out, []byte("whoami\x03ignored"))
	}
}

func TestTerminalInputFilterDropsSplitTerminalReply(t *testing.T) {
	filter := newTerminalInputFilter(&chunkReader{
		chunks: [][]byte{
			[]byte("abc\x1b[6"),
			[]byte("ndef"),
		},
	})
	data, err := io.ReadAll(filter)
	if err != nil {
		t.Fatalf("ReadAll(): %v", err)
	}
	if got, want := string(data), "abcdef"; got != want {
		t.Fatalf("filtered = %q, want %q", got, want)
	}
}

func TestFilterTerminalQueriesRespondsToCursorPositionRequest(t *testing.T) {
	payload, reply, carry := filterTerminalQueries([]byte("abc\x1b[6ndef"))
	if got, want := string(payload), "abcdef"; got != want {
		t.Fatalf("payload = %q, want %q", got, want)
	}
	if got, want := string(reply), "\x1b[1;1R"; got != want {
		t.Fatalf("reply = %q, want %q", got, want)
	}
	if len(carry) != 0 {
		t.Fatalf("carry = %q, want empty", carry)
	}
}

func TestTerminalOutputFilterCarriesSplitCursorPositionRequest(t *testing.T) {
	filter := terminalOutputFilter{}

	payload, reply := filter.Filter([]byte("abc\x1b[6"))
	if got := string(payload); got != "abc" {
		t.Fatalf("first payload = %q, want %q", got, "abc")
	}
	if len(reply) != 0 {
		t.Fatalf("first reply = %q, want empty", reply)
	}

	payload, reply = filter.Filter([]byte("ndef"))
	if got, want := string(payload), "def"; got != want {
		t.Fatalf("second payload = %q, want %q", got, want)
	}
	if got, want := string(reply), "\x1b[1;1R"; got != want {
		t.Fatalf("second reply = %q, want %q", got, want)
	}
}

func TestTerminalOutputFilterCarriesEscBoundary(t *testing.T) {
	filter := terminalOutputFilter{}

	payload, reply := filter.Filter([]byte("/ # \x1b"))
	if got, want := string(payload), "/ # "; got != want {
		t.Fatalf("first payload = %q, want %q", got, want)
	}
	if len(reply) != 0 {
		t.Fatalf("first reply = %q, want empty", reply)
	}

	payload, reply = filter.Filter([]byte("[6n"))
	if got := string(payload); got != "" {
		t.Fatalf("second payload = %q, want empty", got)
	}
	if got, want := string(reply), "\x1b[1;1R"; got != want {
		t.Fatalf("second reply = %q, want %q", got, want)
	}
}

func TestTerminalOutputFilterDropsEchoedCursorPositionReply(t *testing.T) {
	filter := terminalOutputFilter{}

	payload, reply := filter.Filter([]byte("/ # \x1b[6n"))
	if got, want := string(payload), "/ # "; got != want {
		t.Fatalf("first payload = %q, want %q", got, want)
	}
	if got, want := string(reply), "\x1b[1;1R"; got != want {
		t.Fatalf("first reply = %q, want %q", got, want)
	}

	payload, reply = filter.Filter([]byte("\x1b[1;1Rtty-ok\r\n"))
	if got, want := string(payload), "tty-ok\r\n"; got != want {
		t.Fatalf("second payload = %q, want %q", got, want)
	}
	if len(reply) != 0 {
		t.Fatalf("second reply = %q, want empty", reply)
	}
}

func TestTerminalOutputFilterDropsBracketedPasteToggle(t *testing.T) {
	filter := terminalOutputFilter{}

	payload, reply := filter.Filter([]byte("/ # \x1b[?2004hecho ok\r\n\x1b[?2004l"))
	if got, want := string(payload), "/ # echo ok\r\n"; got != want {
		t.Fatalf("payload = %q, want %q", got, want)
	}
	if len(reply) != 0 {
		t.Fatalf("reply = %q, want empty", reply)
	}
}

func TestTerminalInputFilterCarriesEscBoundary(t *testing.T) {
	filter := newTerminalInputFilter(&chunkReader{
		chunks: [][]byte{
			[]byte("abc\x1b"),
			[]byte("[13;5Rdef"),
		},
	})
	data, err := io.ReadAll(filter)
	if err != nil {
		t.Fatalf("ReadAll(): %v", err)
	}
	if got, want := string(data), "abcdef"; got != want {
		t.Fatalf("filtered = %q, want %q", got, want)
	}
}

func TestSessionPumpInputDetachesOnEOF(t *testing.T) {
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe(stdin): %v", err)
	}
	defer stdinR.Close()

	masterR, masterW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe(master): %v", err)
	}
	defer masterR.Close()
	defer masterW.Close()

	s := &Session{
		master:   masterW,
		stdin:    stdinR,
		detachCh: make(chan struct{}),
		stopCh:   make(chan struct{}),
	}

	done := make(chan struct{})
	go func() {
		s.pumpInput()
		close(done)
	}()

	if _, err := io.WriteString(stdinW, "exit\n"); err != nil {
		t.Fatalf("WriteString(stdin): %v", err)
	}
	_ = stdinW.Close()

	select {
	case <-s.Detached():
	case <-time.After(2 * time.Second):
		t.Fatal("detach channel was not closed on EOF")
	}

	select {
	case <-s.StopRequested():
		t.Fatal("stop channel closed on EOF, want open")
	default:
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pumpInput did not exit")
	}

	_ = masterW.Close()
	data, err := io.ReadAll(masterR)
	if err != nil {
		t.Fatalf("io.ReadAll(masterR): %v", err)
	}
	if got, want := string(data), "exit\n\x04"; got != want {
		t.Fatalf("forwarded payload = %q, want %q", got, want)
	}
}

func TestSessionPumpInputRequestsStopOnCtrlBracket(t *testing.T) {
	// Detach trigger is now Ctrl-] (0x1d). Ctrl-C must pass through to the
	// guest unchanged so the guest shell can deliver SIGINT.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe(stdin): %v", err)
	}
	defer stdinR.Close()
	defer stdinW.Close()

	masterR, masterW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe(master): %v", err)
	}
	defer masterR.Close()

	s := &Session{
		master:   masterW,
		stdin:    stdinR,
		detachCh: make(chan struct{}),
		stopCh:   make(chan struct{}),
	}

	done := make(chan struct{})
	go func() {
		s.pumpInput()
		close(done)
	}()

	if _, err := stdinW.Write([]byte("whoami\x1dignored")); err != nil {
		t.Fatalf("stdinW.Write(): %v", err)
	}

	select {
	case <-s.StopRequested():
	case <-time.After(2 * time.Second):
		t.Fatal("stop channel was not closed on Ctrl-]")
	}

	select {
	case <-s.Detached():
	case <-time.After(2 * time.Second):
		t.Fatal("detach channel was not closed on Ctrl-]")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pumpInput did not exit")
	}

	_ = masterW.Close()
	data, err := io.ReadAll(masterR)
	if err != nil {
		t.Fatalf("io.ReadAll(masterR): %v", err)
	}
	if got, want := string(data), "whoami"; got != want {
		t.Fatalf("forwarded payload = %q, want %q", got, want)
	}
}
