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

func TestParseMode(t *testing.T) {
	tests := []struct {
		input   string
		want    Mode
		wantErr bool
	}{
		{"", ModeAuto, false},
		{"auto", ModeAuto, false},
		{"off", ModeOff, false},
		{"force", ModeForce, false},
		{"invalid", "", true},
		{"AUTO", "", true},
		{"ON", "", true},
		{"true", "", true},
	}
	for _, tt := range tests {
		t.Run("input="+tt.input, func(t *testing.T) {
			got, err := ParseMode(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseMode(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("ParseMode(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSessionNilMethodsSafe(t *testing.T) {
	// All Session methods should be safe to call on a nil receiver.
	var s *Session

	if r := s.ConsoleIn(); r != nil {
		t.Fatalf("nil Session.ConsoleIn() = %v, want nil", r)
	}
	if w := s.ConsoleOut(); w != nil {
		t.Fatalf("nil Session.ConsoleOut() = %v, want nil", w)
	}
	if ch := s.Detached(); ch != nil {
		t.Fatalf("nil Session.Detached() = %v, want nil", ch)
	}
	if ch := s.StopRequested(); ch != nil {
		t.Fatalf("nil Session.StopRequested() = %v, want nil", ch)
	}
	// Start and Close should not panic on nil receiver
	if err := s.Start(); err != nil {
		t.Fatalf("nil Session.Start() = %v, want nil", err)
	}
	s.Close() // should not panic
}

func TestNewSessionModeOff(t *testing.T) {
	s, err := NewSession(ModeOff, true, os.Stdin, os.Stdout)
	if err != nil {
		t.Fatalf("NewSession(off) = %v", err)
	}
	if s != nil {
		t.Fatalf("NewSession(off) = %v, want nil", s)
	}
}

func TestNewSessionWaitFalse(t *testing.T) {
	s, err := NewSession(ModeAuto, false, os.Stdin, os.Stdout)
	if err != nil {
		t.Fatalf("NewSession(auto, wait=false) error = %v", err)
	}
	if s != nil {
		t.Fatalf("NewSession(auto, wait=false) = %v, want nil", s)
	}
}

func TestNewSessionForceWithoutWait(t *testing.T) {
	_, err := NewSession(ModeForce, false, os.Stdin, os.Stdout)
	if err == nil {
		t.Fatal("NewSession(force, wait=false) succeeded, want error")
	}
}

func TestInputReplyFilterPassesThroughRegularText(t *testing.T) {
	f := newInputReplyFilter()
	data := f.process([]byte("hello world"))
	if got, want := string(data), "hello world"; got != want {
		t.Fatalf("process() = %q, want %q", got, want)
	}
}

func TestInputReplyFilterStripsCursorPositionReport(t *testing.T) {
	f := newInputReplyFilter()
	// \x1b[24;80R is a cursor position report
	data := f.process([]byte("abc\x1b[24;80Rdef"))
	if got, want := string(data), "abcdef"; got != want {
		t.Fatalf("process() = %q, want %q", got, want)
	}
}

func TestInputReplyFilterStripsBracketedPaste(t *testing.T) {
	f := newInputReplyFilter()
	data := f.process([]byte("abc\x1b[?2004hdef\x1b[?2004l"))
	if got, want := string(data), "abcdef"; got != want {
		t.Fatalf("process() = %q, want %q", got, want)
	}
}

func TestInputReplyFilterPassesArrowKeys(t *testing.T) {
	f := newInputReplyFilter()
	// Arrow up = \x1b[A, arrow down = \x1b[B
	data := f.process([]byte("\x1b[A\x1b[B"))
	if got, want := string(data), "\x1b[A\x1b[B"; got != want {
		t.Fatalf("process() = %q, want %q", got, want)
	}
}

func TestInputReplyFilterHandlesSplitEscape(t *testing.T) {
	f := newInputReplyFilter()
	// ESC at end of first chunk
	out1 := f.process([]byte("text\x1b"))
	// Should carry the ESC
	if got := string(out1); got != "text" {
		t.Fatalf("first chunk = %q, want %q", got, "text")
	}
	// Complete with [A (arrow up)
	out2 := f.process([]byte("[A more"))
	if got, want := string(out2), "\x1b[A more"; got != want {
		t.Fatalf("second chunk = %q, want %q", got, want)
	}
}

func TestIsHostTerminalReply(t *testing.T) {
	tests := []struct {
		name string
		seq  []byte
		want bool
	}{
		{"cursor position report", []byte("\x1b[24;80R"), true},
		{"bracketed paste enable", []byte("\x1b[?2004h"), true},
		{"bracketed paste disable", []byte("\x1b[?2004l"), true},
		{"arrow up", []byte("\x1b[A"), false},
		{"arrow down", []byte("\x1b[B"), false},
		{"too short", []byte("\x1b["), false},
		{"non-CSI", []byte("\x1bO"), false},
		{"clear screen", []byte("\x1b[2J"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isHostTerminalReply(tt.seq)
			if got != tt.want {
				t.Fatalf("isHostTerminalReply(%q) = %v, want %v", tt.seq, got, tt.want)
			}
		})
	}
}

func TestTerminalOutputFilterFlush(t *testing.T) {
	f := terminalOutputFilter{}
	// Send data ending with a partial escape
	payload, _ := f.Filter([]byte("hello\x1b"))
	if got := string(payload); got != "hello" {
		t.Fatalf("payload = %q, want %q", got, "hello")
	}
	tail := f.Flush()
	if got := string(tail); got != "\x1b" {
		t.Fatalf("flush = %q, want %q", got, "\x1b")
	}
	// Flush again should return nil
	if tail := f.Flush(); tail != nil {
		t.Fatalf("second flush = %q, want nil", tail)
	}
}

func TestFilterTerminalReplies(t *testing.T) {
	tests := []struct {
		name      string
		input     []byte
		wantOut   string
		wantCarry bool
	}{
		{
			name:    "plain text",
			input:   []byte("hello world"),
			wantOut: "hello world",
		},
		{
			name:    "strips cursor position report",
			input:   []byte("abc\x1b[6ndef"),
			wantOut: "abcdef",
		},
		{
			name:    "strips bracketed paste toggle",
			input:   []byte("ok\x1b[?2004h"),
			wantOut: "ok",
		},
		{
			name:      "partial escape carries over",
			input:     []byte("data\x1b"),
			wantOut:   "data",
			wantCarry: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, carry := filterTerminalReplies(tt.input)
			if got := string(out); got != tt.wantOut {
				t.Fatalf("output = %q, want %q", got, tt.wantOut)
			}
			if tt.wantCarry && len(carry) == 0 {
				t.Fatal("expected carry, got empty")
			}
			if !tt.wantCarry && len(carry) != 0 {
				t.Fatalf("unexpected carry = %q", carry)
			}
		})
	}
}

func TestShouldDropTerminalReply(t *testing.T) {
	tests := []struct {
		name string
		seq  []byte
		want bool
	}{
		{"bracketed paste h", []byte("\x1b[?2004h"), true},
		{"bracketed paste l", []byte("\x1b[?2004l"), true},
		{"cursor report", []byte("\x1b[1;1R"), true},
		{"status report", []byte("\x1b[0n"), true},
		{"regular CSI", []byte("\x1b[1m"), false},
		{"arrow key", []byte("\x1b[A"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldDropTerminalReply(tt.seq)
			if got != tt.want {
				t.Fatalf("shouldDropTerminalReply(%q) = %v, want %v", tt.seq, got, tt.want)
			}
		})
	}
}

func TestTerminalQueryResponse(t *testing.T) {
	tests := []struct {
		name     string
		seq      []byte
		wantResp bool
	}{
		{"cursor position query", []byte("\x1b[6n"), true},
		{"private cursor query", []byte("\x1b[?6n"), true},
		{"not a query", []byte("\x1b[A"), false},
		{"clear screen", []byte("\x1b[2J"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, ok := terminalQueryResponse(tt.seq)
			if ok != tt.wantResp {
				t.Fatalf("terminalQueryResponse(%q) ok = %v, want %v", tt.seq, ok, tt.wantResp)
			}
			if ok && string(resp) != "\x1b[1;1R" {
				t.Fatalf("response = %q, want cursor position reply", resp)
			}
		})
	}
}

// ---- NEW TESTS: ParseMode additional edge cases ----

func TestParseMode_AllCases(t *testing.T) {
	// Valid cases already tested above, add comprehensive invalid cases
	invalidInputs := []string{"yes", "no", "on", "enabled", "disabled", "1", "0", "tty", "pty"}
	for _, input := range invalidInputs {
		_, err := ParseMode(input)
		if err == nil {
			t.Errorf("ParseMode(%q) should return error", input)
		}
	}
}

// ---- NEW TESTS: Session nil safety for detach and requestStop ----

func TestSessionDetachIdempotent(t *testing.T) {
	s := &Session{
		detachCh: make(chan struct{}),
		stopCh:   make(chan struct{}),
	}
	// Call detach multiple times; should not panic
	s.detach()
	s.detach()
	select {
	case <-s.Detached():
		// good
	default:
		t.Fatal("detachCh should be closed")
	}
}

func TestSessionRequestStopIdempotent(t *testing.T) {
	s := &Session{
		detachCh: make(chan struct{}),
		stopCh:   make(chan struct{}),
	}
	s.requestStop()
	s.requestStop()
	select {
	case <-s.StopRequested():
		// good
	default:
		t.Fatal("stopCh should be closed")
	}
}

// ---- NEW TESTS: NewSession mode=force without wait ----

func TestNewSessionForceWaitFalse(t *testing.T) {
	_, err := NewSession(ModeForce, false, os.Stdin, os.Stdout)
	if err == nil {
		t.Fatal("expected error for force mode without wait")
	}
}

// ---- NEW TESTS: filterConsoleInput pass-through ----

func TestFilterConsoleInput_Regular(t *testing.T) {
	data, detach := filterConsoleInput([]byte("hello"))
	if detach {
		t.Fatal("detach should be false")
	}
	if string(data) != "hello" {
		t.Fatalf("output = %q, want hello", data)
	}
}

func TestFilterConsoleInput_Empty(t *testing.T) {
	data, detach := filterConsoleInput([]byte{})
	if detach {
		t.Fatal("detach should be false")
	}
	if len(data) != 0 {
		t.Fatalf("output = %q, want empty", data)
	}
}

// ---- NEW TESTS: isHostTerminalReply comprehensive ----

func TestIsHostTerminalReply_Comprehensive(t *testing.T) {
	tests := []struct {
		name string
		seq  []byte
		want bool
	}{
		{"cursor position report 1;1", []byte("\x1b[1;1R"), true},
		{"cursor position with ?", []byte("\x1b[?24;80R"), true},
		{"bracketed paste h", []byte("\x1b[?2004h"), true},
		{"bracketed paste l", []byte("\x1b[?2004l"), true},
		{"arrow up", []byte("\x1b[A"), false},
		{"arrow down", []byte("\x1b[B"), false},
		{"arrow right", []byte("\x1b[C"), false},
		{"arrow left", []byte("\x1b[D"), false},
		{"home key", []byte("\x1b[H"), false},
		{"clear screen", []byte("\x1b[2J"), false},
		{"bold on", []byte("\x1b[1m"), false},
		{"too short 2 bytes", []byte("\x1b["), false},
		{"not CSI", []byte("\x1bO"), false},
		{"single byte", []byte{0x1b}, false},
		{"non-numeric before R", []byte("\x1b[abR"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isHostTerminalReply(tt.seq)
			if got != tt.want {
				t.Fatalf("isHostTerminalReply(%q) = %v, want %v", tt.seq, got, tt.want)
			}
		})
	}
}

// ---- NEW TESTS: shouldDropTerminalReply comprehensive ----

func TestShouldDropTerminalReply_Comprehensive(t *testing.T) {
	tests := []struct {
		name string
		seq  []byte
		want bool
	}{
		{"bracketed paste h", []byte("\x1b[?2004h"), true},
		{"bracketed paste l", []byte("\x1b[?2004l"), true},
		{"cursor report", []byte("\x1b[24;80R"), true},
		{"status report", []byte("\x1b[?0n"), true},
		{"color SGR", []byte("\x1b[31m"), false},
		{"cursor up", []byte("\x1b[A"), false},
		{"clear line", []byte("\x1b[K"), false},
		{"too short", []byte{0x1b}, false},
		{"empty", []byte{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldDropTerminalReply(tt.seq)
			if got != tt.want {
				t.Fatalf("shouldDropTerminalReply(%q) = %v, want %v", tt.seq, got, tt.want)
			}
		})
	}
}

// ---- NEW TESTS: inputReplyFilter edge cases ----

func TestInputReplyFilterMultipleRepliesInOneChunk(t *testing.T) {
	f := newInputReplyFilter()
	data := f.process([]byte("abc\x1b[24;80R\x1b[?2004hdef"))
	if got, want := string(data), "abcdef"; got != want {
		t.Fatalf("process() = %q, want %q", got, want)
	}
}

func TestInputReplyFilterSplitCursorReport(t *testing.T) {
	f := newInputReplyFilter()
	out1 := f.process([]byte("text\x1b[24"))
	// carry should have the partial CSI
	if got := string(out1); got != "text" {
		t.Fatalf("first chunk = %q, want text", got)
	}
	out2 := f.process([]byte(";80Rmore"))
	if got, want := string(out2), "more"; got != want {
		t.Fatalf("second chunk = %q, want %q", got, want)
	}
}

func TestInputReplyFilterPassesNonCSIEscape(t *testing.T) {
	f := newInputReplyFilter()
	// \x1bO is SS3, not CSI - should pass through
	data := f.process([]byte("\x1bOP"))
	if got := string(data); got != "\x1bOP" {
		t.Fatalf("process() = %q, want \\x1bOP", got)
	}
}

func TestInputReplyFilterPassesFunctionKeys(t *testing.T) {
	f := newInputReplyFilter()
	// F1-F4 use SS3 (passed), F5+ use CSI
	data := f.process([]byte("\x1b[15~")) // F5
	if got, want := string(data), "\x1b[15~"; got != want {
		t.Fatalf("process() = %q, want %q", got, want)
	}
}

// ---- NEW TESTS: filterTerminalQueries ----

func TestFilterTerminalQueries_NoQueries(t *testing.T) {
	payload, reply, carry := filterTerminalQueries([]byte("plain text"))
	if string(payload) != "plain text" {
		t.Fatalf("payload = %q", payload)
	}
	if len(reply) != 0 {
		t.Fatalf("reply = %q", reply)
	}
	if len(carry) != 0 {
		t.Fatalf("carry = %q", carry)
	}
}

func TestFilterTerminalQueries_PrivateCursorQuery(t *testing.T) {
	payload, reply, carry := filterTerminalQueries([]byte("abc\x1b[?6ndef"))
	if string(payload) != "abcdef" {
		t.Fatalf("payload = %q", payload)
	}
	if string(reply) != "\x1b[1;1R" {
		t.Fatalf("reply = %q", reply)
	}
	if len(carry) != 0 {
		t.Fatalf("carry = %q", carry)
	}
}

func TestFilterTerminalQueries_DropsReplyAndQuery(t *testing.T) {
	// Output from guest containing a cursor position query AND a reply echo
	payload, reply, carry := filterTerminalQueries([]byte("prompt\x1b[6n\x1b[1;1R"))
	if string(payload) != "prompt" {
		t.Fatalf("payload = %q", payload)
	}
	if string(reply) != "\x1b[1;1R" {
		t.Fatalf("reply = %q, want cursor report answer", reply)
	}
	if len(carry) != 0 {
		t.Fatalf("carry = %q", carry)
	}
}

func TestFilterTerminalQueries_CarriesPartialEscape(t *testing.T) {
	payload, reply, carry := filterTerminalQueries([]byte("text\x1b"))
	if string(payload) != "text" {
		t.Fatalf("payload = %q", payload)
	}
	if len(reply) != 0 {
		t.Fatalf("reply = %q", reply)
	}
	if string(carry) != "\x1b" {
		t.Fatalf("carry = %q, want \\x1b", carry)
	}
}

func TestFilterTerminalQueries_PassesThroughNonCSI(t *testing.T) {
	payload, reply, carry := filterTerminalQueries([]byte("\x1bOP")) // F1 key via SS3
	if string(payload) != "\x1bOP" {
		t.Fatalf("payload = %q, want \\x1bOP", payload)
	}
	if len(reply) != 0 {
		t.Fatalf("reply = %q", reply)
	}
	if len(carry) != 0 {
		t.Fatalf("carry = %q", carry)
	}
}

// ---- NEW TESTS: filterTerminalReplies additional cases ----

func TestFilterTerminalReplies_NonCSIEscape(t *testing.T) {
	out, carry := filterTerminalReplies([]byte("\x1bOQ"))
	if string(out) != "\x1bOQ" {
		t.Fatalf("output = %q, want \\x1bOQ", out)
	}
	if len(carry) != 0 {
		t.Fatalf("carry = %q", carry)
	}
}

func TestFilterTerminalReplies_PartialCSI(t *testing.T) {
	out, carry := filterTerminalReplies([]byte("abc\x1b[24"))
	if string(out) != "abc" {
		t.Fatalf("output = %q", out)
	}
	if string(carry) != "\x1b[24" {
		t.Fatalf("carry = %q", carry)
	}
}

// ---- NEW TESTS: terminalOutputFilter double flush ----

func TestTerminalOutputFilterFlush_Empty(t *testing.T) {
	f := terminalOutputFilter{}
	tail := f.Flush()
	if tail != nil {
		t.Fatalf("flush of empty filter = %q, want nil", tail)
	}
}

// ---- NEW TESTS: terminalInputFilter EOF with carry ----

func TestTerminalInputFilterEOFWithCarry(t *testing.T) {
	// Reader that produces an ESC byte then EOF
	filter := newTerminalInputFilter(&chunkReader{
		chunks: [][]byte{
			{0x1b},
		},
	})
	data, err := io.ReadAll(filter)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	// The carry ESC byte should be flushed on EOF
	if string(data) != "\x1b" {
		t.Fatalf("data = %q, want \\x1b", data)
	}
}

// ---- NEW TESTS: pumpInput with reply filtering ----

func TestSessionPumpInputFiltersReplies(t *testing.T) {
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer stdinR.Close()

	masterR, masterW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
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

	// Write text with an embedded cursor position report that should be filtered
	if _, err := stdinW.Write([]byte("abc\x1b[24;80Rdef")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_ = stdinW.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pumpInput did not exit")
	}

	_ = masterW.Close()
	data, err := io.ReadAll(masterR)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	// The cursor position report should be stripped
	if got := string(data); !bytes.Contains(data, []byte("abcdef")) {
		t.Fatalf("forwarded = %q, want abcdef (with reply stripped)", got)
	}
}
