package console

import (
	"io"
	"os"
	"testing"
	"time"
)

func TestNewSessionAutoNotTTY(t *testing.T) {
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	s, err := NewSession(ModeAuto, true, r, w)
	if err != nil {
		t.Fatalf("NewSession(auto, pipe): %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatal("NewSession(auto, pipe) should return nil for non-TTY")
	}
}

func TestNewSessionForceNotTTY(t *testing.T) {
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	_, err := NewSession(ModeForce, true, r, w)
	if err == nil {
		t.Fatal("NewSession(force, pipe) should fail for non-TTY")
	}
}

func TestNewSessionNilStdinStdout(t *testing.T) {
	s, err := NewSession(ModeAuto, true, nil, nil)
	if err != nil {
		t.Fatalf("NewSession(auto, nil): %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatal("NewSession(auto, nil) should return nil")
	}
}

func TestSessionConsoleInOut(t *testing.T) {
	s := &Session{
		slave:    os.Stdin,
		master:   os.Stdout,
		detachCh: make(chan struct{}),
		stopCh:   make(chan struct{}),
	}
	if s.ConsoleIn() == nil {
		t.Fatal("ConsoleIn() should not be nil")
	}
	if s.ConsoleOut() == nil {
		t.Fatal("ConsoleOut() should not be nil")
	}
	if s.Detached() == nil {
		t.Fatal("Detached() should not be nil")
	}
	if s.StopRequested() == nil {
		t.Fatal("StopRequested() should not be nil")
	}
}

func TestSessionCloseIdempotent(t *testing.T) {
	s := &Session{
		detachCh: make(chan struct{}),
		stopCh:   make(chan struct{}),
	}
	s.Close()
	s.Close()
}

func TestDrainInputNilFile(t *testing.T) {
	drainInput(nil)
}

func TestInputReplyFilterSplitCSI(t *testing.T) {
	f := newInputReplyFilter()
	out := f.process([]byte("hello\x1b["))
	if string(out) != "hello" {
		t.Fatalf("first = %q, want hello", out)
	}
	out = f.process([]byte("1m more"))
	if string(out) != "\x1b[1m more" {
		t.Fatalf("second = %q, want \\x1b[1m more", out)
	}
}

func TestInputReplyFilterEscNonCSI(t *testing.T) {
	f := newInputReplyFilter()
	out := f.process([]byte("a\x1bO more"))
	if string(out) != "a\x1bO more" {
		t.Fatalf("output = %q", out)
	}
}

func TestTerminalInputFilterEOFWithCarryData(t *testing.T) {
	filter := newTerminalInputFilter(&chunkReader{
		chunks: [][]byte{
			[]byte("hello\x1b"),
		},
	})
	data, err := io.ReadAll(filter)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(data) != "hello\x1b" {
		t.Fatalf("data = %q, want hello\\x1b", data)
	}
}

func TestTerminalOutputFilterFlushEmpty(t *testing.T) {
	f := terminalOutputFilter{}
	if tail := f.Flush(); tail != nil {
		t.Fatalf("flush empty = %q, want nil", tail)
	}
}

func TestFilterTerminalQueriesNonCSI(t *testing.T) {
	payload, reply, carry := filterTerminalQueries([]byte("\x1bO more"))
	if string(payload) != "\x1bO more" {
		t.Fatalf("payload = %q", payload)
	}
	if len(reply) != 0 {
		t.Fatalf("reply = %q, want empty", reply)
	}
	if len(carry) != 0 {
		t.Fatalf("carry = %q, want empty", carry)
	}
}

func TestFilterTerminalQueriesPartialCSI(t *testing.T) {
	payload, reply, carry := filterTerminalQueries([]byte("hello\x1b[6"))
	if string(payload) != "hello" {
		t.Fatalf("payload = %q, want hello", payload)
	}
	if len(reply) != 0 {
		t.Fatalf("reply = %q", reply)
	}
	if string(carry) != "\x1b[6" {
		t.Fatalf("carry = %q, want \\x1b[6", carry)
	}
}

func TestFilterTerminalRepliesNonCSI(t *testing.T) {
	out, carry := filterTerminalReplies([]byte("\x1bO text"))
	if string(out) != "\x1bO text" {
		t.Fatalf("out = %q", out)
	}
	if carry != nil {
		t.Fatalf("carry = %q", carry)
	}
}

func TestFilterTerminalRepliesPartialCSI(t *testing.T) {
	out, carry := filterTerminalReplies([]byte("data\x1b[99"))
	if string(out) != "data" {
		t.Fatalf("out = %q, want data", out)
	}
	if string(carry) != "\x1b[99" {
		t.Fatalf("carry = %q", carry)
	}
}

func TestPumpInputWriteErrorReturns(t *testing.T) {
	stdinR, stdinW, _ := os.Pipe()
	defer stdinR.Close()

	masterR, masterW, _ := os.Pipe()
	masterR.Close()
	masterW.Close()

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

	stdinW.Write([]byte("data"))
	stdinW.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pumpInput did not exit on write error")
	}
}

func TestShouldDropTerminalReplyShortSeq(t *testing.T) {
	if shouldDropTerminalReply([]byte("\x1b")) {
		t.Fatal("should not drop 1-byte sequence")
	}
	if shouldDropTerminalReply([]byte("ab")) {
		t.Fatal("should not drop non-CSI sequence")
	}
}

func TestShouldDropTerminalReplyNonNumericBody(t *testing.T) {
	if shouldDropTerminalReply([]byte("\x1b[abR")) {
		t.Fatal("should not drop CSI with non-numeric body ending in R")
	}
}

func TestWrapPTYOpenErrorNil(t *testing.T) {
	if err := wrapPTYOpenError(nil); err != nil {
		t.Fatalf("wrapPTYOpenError(nil) = %v", err)
	}
}

func TestPumpOutputForwardsData(t *testing.T) {
	masterR, masterW, _ := os.Pipe()
	defer masterR.Close()

	stdoutR, stdoutW, _ := os.Pipe()
	defer stdoutR.Close()

	s := &Session{
		master:   masterR,
		stdout:   stdoutW,
		detachCh: make(chan struct{}),
		stopCh:   make(chan struct{}),
	}

	done := make(chan struct{})
	go func() {
		s.pumpOutput()
		close(done)
	}()

	masterW.Write([]byte("hello from guest"))
	masterW.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pumpOutput did not exit")
	}

	stdoutW.Close()
	data, _ := io.ReadAll(stdoutR)
	if string(data) != "hello from guest" {
		t.Fatalf("output = %q, want %q", data, "hello from guest")
	}
}

func TestPumpOutputFiltersCursorQuery(t *testing.T) {
	masterR, masterW, _ := os.Pipe()

	stdoutR, stdoutW, _ := os.Pipe()
	defer stdoutR.Close()

	s := &Session{
		master:   masterR,
		stdout:   stdoutW,
		detachCh: make(chan struct{}),
		stopCh:   make(chan struct{}),
	}

	done := make(chan struct{})
	go func() {
		s.pumpOutput()
		close(done)
	}()

	masterW.Write([]byte("/ # \x1b[6n"))
	masterW.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pumpOutput did not exit")
	}

	stdoutW.Close()
	data, _ := io.ReadAll(stdoutR)
	if string(data) != "/ # " {
		t.Fatalf("output = %q, want %q", data, "/ # ")
	}
}

func TestPumpOutputFlushesOnEOF(t *testing.T) {
	masterR, masterW, _ := os.Pipe()

	stdoutR, stdoutW, _ := os.Pipe()
	defer stdoutR.Close()

	s := &Session{
		master:   masterR,
		stdout:   stdoutW,
		detachCh: make(chan struct{}),
		stopCh:   make(chan struct{}),
	}

	done := make(chan struct{})
	go func() {
		s.pumpOutput()
		close(done)
	}()

	masterW.Write([]byte("text\x1b"))
	masterW.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pumpOutput did not exit")
	}

	stdoutW.Close()
	data, _ := io.ReadAll(stdoutR)
	if string(data) != "text\x1b" {
		t.Fatalf("output = %q, want text\\x1b", data)
	}
}


func TestNewSessionForceNilStdinStdout(t *testing.T) {
	_, err := NewSession(ModeForce, true, nil, nil)
	if err == nil {
		t.Fatal("expected error for force with nil stdin/stdout")
	}
}

func TestPumpOutputWriteError(t *testing.T) {
	masterR, masterW, _ := os.Pipe()
	defer masterR.Close()

	// Create a stdout that will fail on write
	stdoutR, stdoutW, _ := os.Pipe()
	stdoutR.Close() // close read end so writes fail

	s := &Session{
		master:   masterR,
		stdout:   stdoutW,
		detachCh: make(chan struct{}),
		stopCh:   make(chan struct{}),
	}

	done := make(chan struct{})
	go func() {
		s.pumpOutput()
		close(done)
	}()

	masterW.Write([]byte("data that causes write error"))
	masterW.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pumpOutput did not exit on write error")
	}
	stdoutW.Close()
}

func TestFilterTerminalQueriesDropsReply(t *testing.T) {
	// filterTerminalQueries should drop shouldDropTerminalReply sequences
	payload, reply, carry := filterTerminalQueries([]byte("text\x1b[?2004hmore"))
	if string(payload) != "textmore" {
		t.Fatalf("payload = %q, want textmore", payload)
	}
	if len(reply) != 0 {
		t.Fatalf("reply = %q", reply)
	}
	if len(carry) != 0 {
		t.Fatalf("carry = %q", carry)
	}
}

func TestFilterTerminalQueriesLoneEsc(t *testing.T) {
	payload, reply, carry := filterTerminalQueries([]byte("data\x1b"))
	if string(payload) != "data" {
		t.Fatalf("payload = %q", payload)
	}
	if len(reply) != 0 {
		t.Fatalf("reply = %q", reply)
	}
	if string(carry) != "\x1b" {
		t.Fatalf("carry = %q", carry)
	}
}

func TestWrapPTYOpenErrorWithMessage(t *testing.T) {
	err := wrapPTYOpenError(os.ErrNotExist)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestWrapPTYOpenErrorOutOfPty(t *testing.T) {
	err := wrapPTYOpenError(os.ErrPermission)
	if err == nil {
		t.Fatal("expected error")
	}
}


func TestFilterTerminalRepliesNonDropCSI(t *testing.T) {
	// CSI that is not a reply should pass through
	out, carry := filterTerminalReplies([]byte("\x1b[1mhello\x1b[0m"))
	if string(out) != "\x1b[1mhello\x1b[0m" {
		t.Fatalf("out = %q", out)
	}
	if carry != nil {
		t.Fatalf("carry = %q", carry)
	}
}

func TestFilterTerminalQueriesPrivateCursorQuery(t *testing.T) {
	// \x1b[?6n is a private cursor query
	payload, reply, carry := filterTerminalQueries([]byte("prompt\x1b[?6n"))
	if string(payload) != "prompt" {
		t.Fatalf("payload = %q", payload)
	}
	if string(reply) != "\x1b[1;1R" {
		t.Fatalf("reply = %q, want cursor reply", reply)
	}
	if len(carry) != 0 {
		t.Fatalf("carry = %q", carry)
	}
}
