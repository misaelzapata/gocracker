package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

var (
	diskLineRE = regexp.MustCompile(`(?m)^[[:space:]]*disk:[[:space:]]*(\S+)[[:space:]]*$`)
	promptRE   = regexp.MustCompile(`(?m)(?:^|[\r\n])(?:/ # |[^ \r\n]+:[^\r\n]*[#\$] |[^ \r\n]+@[^ \r\n]+:[^\r\n]*[#\$] )`)
)

type transcriptState struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	diskPath string
}

type terminalQueryFilter struct {
	carry []byte
}

func (s *transcriptState) feed(chunk []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, _ = s.buf.Write(chunk)
	data := s.buf.Bytes()
	if s.diskPath == "" {
		if matches := diskLineRE.FindSubmatch(data); len(matches) == 2 {
			s.diskPath = string(matches[1])
		}
	}
}

func (s *transcriptState) snapshot() (string, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.diskPath, len(promptRE.FindAllIndex(s.buf.Bytes(), -1)), s.buf.Len()
}

func main() {
	var (
		logPath      string
		inputPath    string
		diskPathFile string
		readyTimeout time.Duration
		exitTimeout  time.Duration
	)

	flag.StringVar(&logPath, "log", "", "Path to transcript log")
	flag.StringVar(&inputPath, "input", "", "Path to scripted input file")
	flag.StringVar(&diskPathFile, "disk-path-file", "", "Path to write the resolved runtime disk path")
	flag.DurationVar(&readyTimeout, "ready-timeout", 45*time.Second, "How long to wait for the guest shell to become ready")
	flag.DurationVar(&exitTimeout, "exit-timeout", 45*time.Second, "How long to wait for natural process exit after sending input")
	flag.Parse()

	cmdArgs := flag.Args()
	if logPath == "" || inputPath == "" {
		fatalf("--log and --input are required")
	}
	if len(cmdArgs) == 0 {
		fatalf("command is required after --")
	}

	inputBytes, err := os.ReadFile(inputPath)
	if err != nil {
		fatalf("read input file: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		fatalf("mkdir log dir: %v", err)
	}
	if diskPathFile != "" {
		if err := os.MkdirAll(filepath.Dir(diskPathFile), 0o755); err != nil {
			fatalf("mkdir disk path dir: %v", err)
		}
	}

	logFile, err := os.Create(logPath)
	if err != nil {
		fatalf("create log: %v", err)
	}
	defer logFile.Close()

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)

	ptmx, err := pty.Start(cmd)
	if err != nil {
		fatalf("start PTY command: %v", err)
	}
	defer ptmx.Close()

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	state := &transcriptState{}
	readerDone := make(chan error, 1)
	go func() {
		readerDone <- streamTranscript(ptmx, logFile, state)
	}()

	if diskPathFile != "" {
		diskPath, err := waitForDiskPath(state, readyTimeout)
		if err != nil {
			terminateCommand(cmd)
			<-waitDone
			<-readerDone
			fatalf("%v", err)
		}
		if err := os.WriteFile(diskPathFile, []byte(diskPath+"\n"), 0o644); err != nil {
			terminateCommand(cmd)
			<-waitDone
			<-readerDone
			fatalf("write disk path file: %v", err)
		}
	}

	promptCount, err := waitForReady(state, readyTimeout)
	if err != nil {
		terminateCommand(cmd)
		<-waitDone
		<-readerDone
		fatalf("%v", err)
	}

	promptCount, err = primePrompt(ptmx, state, promptCount, readyTimeout)
	if err != nil {
		terminateCommand(cmd)
		<-waitDone
		<-readerDone
		fatalf("prime interactive prompt: %v", err)
	}

	promptCount, err = runScript(ptmx, state, normalizeInput(inputBytes), promptCount, readyTimeout)
	if err != nil {
		terminateCommand(cmd)
		<-waitDone
		<-readerDone
		fatalf("run scripted input: %v", err)
	}

	select {
	case err := <-waitDone:
		if err != nil {
			<-readerDone
			fatalf("interactive command exited with error: %v", err)
		}
	case <-time.After(5 * time.Second):
		if _, err := ptmx.Write([]byte{0x04}); err != nil {
			terminateCommand(cmd)
			<-waitDone
			<-readerDone
			fatalf("write EOF to PTY: %v", err)
		}
		select {
		case err := <-waitDone:
			if err != nil {
				<-readerDone
				fatalf("interactive command exited with error: %v", err)
			}
		case <-time.After(exitTimeout):
			terminateCommand(cmd)
			<-waitDone
			<-readerDone
			fatalf("interactive session did not exit cleanly")
		}
	case <-time.After(exitTimeout):
		terminateCommand(cmd)
		<-waitDone
		<-readerDone
		fatalf("interactive session did not exit cleanly")
	}

	if err := <-readerDone; err != nil && err != io.EOF {
		fatalf("read transcript: %v", err)
	}
}

func streamTranscript(ptmx *os.File, out io.Writer, state *transcriptState) error {
	filter := terminalQueryFilter{}
	buf := make([]byte, 4096)
	for {
		n, err := ptmx.Read(buf)
		if n > 0 {
			payload, reply := filter.Filter(buf[:n])
			if len(reply) > 0 {
				if _, writeErr := ptmx.Write(reply); writeErr != nil {
					return writeErr
				}
			}
			if len(payload) > 0 {
				state.feed(payload)
			}
			if _, writeErr := out.Write(payload); writeErr != nil {
				return writeErr
			}
		}
		if err != nil {
			if err == io.EOF || errors.Is(err, syscall.EIO) {
				if tail := filter.Flush(); len(tail) > 0 {
					state.feed(tail)
					if _, writeErr := out.Write(tail); writeErr != nil {
						return writeErr
					}
				}
				return nil
			}
			return err
		}
	}
}

func waitForDiskPath(state *transcriptState, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if diskPath, _, _ := state.snapshot(); diskPath != "" {
			return diskPath, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", fmt.Errorf("run did not report disk path")
}

func waitForReady(state *transcriptState, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	var (
		lastSize    int
		stableSince time.Time
	)
	for time.Now().Before(deadline) {
		_, prompts, size := state.snapshot()
		if prompts > 0 {
			if size != lastSize {
				lastSize = size
				stableSince = time.Now()
			}
			if !stableSince.IsZero() && time.Since(stableSince) >= 200*time.Millisecond {
				return prompts, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return 0, fmt.Errorf("interactive shell did not become ready")
}

func normalizeInput(input []byte) []byte {
	normalized := bytes.ReplaceAll(input, []byte("\r\n"), []byte("\n"))
	normalized = bytes.ReplaceAll(normalized, []byte("\r"), []byte("\n"))
	return normalized
}

func runScript(ptmx *os.File, state *transcriptState, input []byte, promptCount int, timeout time.Duration) (int, error) {
	rawLines := bytes.Split(input, []byte("\n"))
	lines := make([][]byte, 0, len(rawLines))
	for _, raw := range rawLines {
		line := bytes.TrimRight(raw, "\r")
		if len(line) == 0 {
			continue
		}
		lines = append(lines, append([]byte{}, line...))
	}
	for _, line := range lines {
		_, _, baselineSize := state.snapshot()
		payload := append(append([]byte{}, line...), '\r')
		if _, err := ptmx.Write(payload); err != nil {
			return promptCount, err
		}
		if err := waitForTranscriptGrowth(state, baselineSize, len(line), timeout); err != nil {
			return promptCount, err
		}
	}
	return promptCount, nil
}

func primePrompt(ptmx *os.File, state *transcriptState, promptCount int, timeout time.Duration) (int, error) {
	if _, err := ptmx.Write([]byte{'\r'}); err != nil {
		return promptCount, err
	}
	primeTimeout := timeout
	if primeTimeout > time.Second {
		primeTimeout = time.Second
	}
	nextPrompt, err := waitForPromptAdvance(state, promptCount, primeTimeout)
	if err != nil {
		return promptCount, nil
	}
	return nextPrompt, nil
}

func waitForPromptAdvance(state *transcriptState, current int, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	var (
		lastSize    int
		stableSince time.Time
	)
	for time.Now().Before(deadline) {
		_, prompts, size := state.snapshot()
		if prompts > current {
			if size != lastSize {
				lastSize = size
				stableSince = time.Now()
			}
			if !stableSince.IsZero() && time.Since(stableSince) >= 200*time.Millisecond {
				return prompts, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return current, fmt.Errorf("interactive shell did not return to prompt")
}

func waitForTranscriptGrowth(state *transcriptState, baselineSize, minDelta int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, _, size := state.snapshot()
		if size >= baselineSize+minDelta {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("interactive shell did not acknowledge scripted input")
}

func (f *terminalQueryFilter) Filter(data []byte) ([]byte, []byte) {
	if len(f.carry) > 0 {
		data = append(append([]byte{}, f.carry...), data...)
		f.carry = nil
	}
	var out bytes.Buffer
	var reply bytes.Buffer
	for i := 0; i < len(data); {
		if data[i] != 0x1b {
			out.WriteByte(data[i])
			i++
			continue
		}
		if i+1 >= len(data) {
			f.carry = append([]byte{}, data[i:]...)
			return out.Bytes(), reply.Bytes()
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
			f.carry = append([]byte{}, data[i:]...)
			return out.Bytes(), reply.Bytes()
		}
		seq := data[i : end+1]
		if bytes.Equal(seq, []byte("\x1b[6n")) || bytes.Equal(seq, []byte("\x1b[?6n")) {
			reply.WriteString("\x1b[1;1R")
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
	return out.Bytes(), reply.Bytes()
}

func (f *terminalQueryFilter) Flush() []byte {
	if len(f.carry) == 0 {
		return nil
	}
	tail := append([]byte{}, f.carry...)
	f.carry = nil
	return tail
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

func terminateCommand(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	time.Sleep(2 * time.Second)
	_ = cmd.Process.Signal(syscall.SIGKILL)
}

func fatalf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	_, _ = os.Stderr.WriteString("error: " + msg)
	os.Exit(1)
}
