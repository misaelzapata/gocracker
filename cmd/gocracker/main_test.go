package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	internalapi "github.com/gocracker/gocracker/internal/api"
	"github.com/gocracker/gocracker/internal/guestexec"
	"github.com/gocracker/gocracker/internal/oci"
	"github.com/gocracker/gocracker/internal/runtimecfg"
	"github.com/gocracker/gocracker/pkg/container"
	"github.com/gocracker/gocracker/pkg/vmm"
	mobyterm "github.com/moby/term"
)

type testHandle struct {
	cfg       vmm.Config
	state     vmm.State
	stopCalls int
	waitErr   error
	dial      func(uint32) (net.Conn, error)
}

func (h *testHandle) Start() error                               { return nil }
func (h *testHandle) Stop()                                      { h.stopCalls++; h.state = vmm.StateStopped }
func (h *testHandle) TakeSnapshot(string) (*vmm.Snapshot, error) { return nil, nil }
func (h *testHandle) State() vmm.State                           { return h.state }
func (h *testHandle) ID() string                                 { return h.cfg.ID }
func (h *testHandle) Uptime() time.Duration                      { return 0 }
func (h *testHandle) Events() vmm.EventSource                    { return vmm.NewEventLog() }
func (h *testHandle) VMConfig() vmm.Config                       { return h.cfg }
func (h *testHandle) DeviceList() []vmm.DeviceInfo               { return nil }
func (h *testHandle) ConsoleOutput() []byte                      { return nil }
func (h *testHandle) WaitStopped(ctx context.Context) error      { return h.waitErr }
func (h *testHandle) DialVsock(port uint32) (net.Conn, error) {
	if h.dial == nil {
		return nil, errors.New("no dialer")
	}
	return h.dial(port)
}

type closeWriterConn struct {
	net.Conn
	closed bool
}

func (c *closeWriterConn) CloseWrite() error {
	c.closed = true
	return nil
}

func TestResolveRequiredExistingPath_ReturnsAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "kernel.bin")
	writeTestFile(t, file, []byte("kernel"))

	got := resolveRequiredExistingPath("kernel", file)
	if !filepath.IsAbs(got) {
		t.Fatalf("resolveRequiredExistingPath returned %q, want absolute path", got)
	}
	if got != file {
		t.Fatalf("resolveRequiredExistingPath = %q, want %q", got, file)
	}
}

func TestPid1ModeForCLIWait(t *testing.T) {
	if got := pid1ModeForCLIWait(true); got != runtimecfg.PID1ModeSupervised {
		t.Fatalf("pid1ModeForCLIWait(true) = %q, want %q", got, runtimecfg.PID1ModeSupervised)
	}
	if got := pid1ModeForCLIWait(false); got != "" {
		t.Fatalf("pid1ModeForCLIWait(false) = %q, want empty", got)
	}
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestSplitHelpers(t *testing.T) {
	if got := splitComma(""); got != nil {
		t.Fatalf("splitComma(\"\") = %#v, want nil", got)
	}
	if got := splitComma("a,b"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("splitComma() = %#v", got)
	}
	if got := splitFields(""); got != nil {
		t.Fatalf("splitFields(\"\") = %#v, want nil", got)
	}
	if got := splitFields(`echo "hello world"`); !reflect.DeepEqual(got, []string{"echo", "hello world"}) {
		t.Fatalf("splitFields() = %#v", got)
	}
}

func TestNormalizeNetworkMode(t *testing.T) {
	if got := normalizeNetworkMode("none"); got != "" {
		t.Fatalf("normalizeNetworkMode(none) = %q", got)
	}
	if got := normalizeNetworkMode("AUTO"); got != container.NetworkModeAuto {
		t.Fatalf("normalizeNetworkMode(auto) = %q", got)
	}
}

func TestFlagsAndTrustedDirsHelpers(t *testing.T) {
	var dirs multiStringFlag
	if err := dirs.Set(" . "); err != nil {
		t.Fatalf("dirs.Set() error = %v", err)
	}
	if err := dirs.Set(" . "); err != nil {
		t.Fatalf("dirs.Set() error = %v", err)
	}
	values := dirs.Values()
	if len(values) != 1 || !filepath.IsAbs(values[0]) {
		t.Fatalf("dirs.Values() = %#v", values)
	}

	var kv multiKVFlag
	if err := kv.Set("A=B"); err != nil {
		t.Fatalf("kv.Set() error = %v", err)
	}
	if got := kv.Map()["A"]; got != "B" {
		t.Fatalf("kv.Map()[A] = %q", got)
	}

	workDirs := defaultTrustedWorkDirs()
	if len(workDirs) == 0 {
		t.Fatal("defaultTrustedWorkDirs() returned empty slice")
	}
	snapshotDirs := defaultTrustedSnapshotDirs("/tmp/state")
	if len(snapshotDirs) != 3 {
		t.Fatalf("defaultTrustedSnapshotDirs() = %#v", snapshotDirs)
	}

	root := t.TempDir()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(prev) }()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join("artifacts", "kernels"), 0o755); err != nil {
		t.Fatal(err)
	}
	kernelDirs := defaultTrustedKernelDirs()
	if len(kernelDirs) != 1 || !strings.HasSuffix(kernelDirs[0], filepath.Join("artifacts", "kernels")) {
		t.Fatalf("defaultTrustedKernelDirs() = %#v", kernelDirs)
	}
}

func TestNetworkingAndConsoleHelpers(t *testing.T) {
	if !isLoopbackTCPAddr("127.0.0.1:8080") {
		t.Fatal("expected loopback for 127.0.0.1")
	}
	if !isLoopbackTCPAddr("[::1]:8080") {
		t.Fatal("expected loopback for ::1")
	}
	if isLoopbackTCPAddr(":8080") {
		t.Fatal("wildcard bind should not count as loopback")
	}

	if got := formatConsoleTail([]byte("a\n\nb\r\nc\n"), 2); got != "b\nc" {
		t.Fatalf("formatConsoleTail() = %q", got)
	}
	if err := normalizeCopyError(io.EOF); err != nil {
		t.Fatalf("normalizeCopyError(io.EOF) = %v", err)
	}
	if err := normalizeCopyError(net.ErrClosed); err != nil {
		t.Fatalf("normalizeCopyError(net.ErrClosed) = %v", err)
	}
	if err := normalizeCopyError(errors.New("boom")); err == nil {
		t.Fatal("normalizeCopyError(boom) = nil")
	}
}

func TestPrepareInteractiveTerminal_PrintsNewlineAfterRestore(t *testing.T) {
	oldGetFdInfo := terminalGetFdInfo
	oldSetRaw := terminalSetRaw
	oldRestore := terminalRestore
	defer func() {
		terminalGetFdInfo = oldGetFdInfo
		terminalSetRaw = oldSetRaw
		terminalRestore = oldRestore
	}()

	var restored bool
	var state mobyterm.State
	terminalGetFdInfo = func(interface{}) (uintptr, bool) {
		return 42, true
	}
	terminalSetRaw = func(fd uintptr) (*mobyterm.State, error) {
		if fd != 42 {
			t.Fatalf("terminalSetRaw fd = %d, want 42", fd)
		}
		return &state, nil
	}
	terminalRestore = func(fd uintptr, restoredState *mobyterm.State) error {
		if fd != 42 {
			t.Fatalf("terminalRestore fd = %d, want 42", fd)
		}
		if restoredState != &state {
			t.Fatalf("terminalRestore state = %p, want %p", restoredState, &state)
		}
		restored = true
		return nil
	}

	var stdout bytes.Buffer
	restore := prepareInteractiveTerminal(os.Stdin, &stdout)
	restore()

	if !restored {
		t.Fatal("prepareInteractiveTerminal() did not restore terminal state")
	}
	if got := stdout.String(); got != "\x1b[?25h\r\n" {
		t.Fatalf("prepareInteractiveTerminal() wrote %q, want newline", got)
	}
}

func TestRunInteractiveConnWithIO_AppendsPromptNewline(t *testing.T) {
	oldGetFdInfo := terminalGetFdInfo
	oldSetRaw := terminalSetRaw
	oldRestore := terminalRestore
	defer func() {
		terminalGetFdInfo = oldGetFdInfo
		terminalSetRaw = oldSetRaw
		terminalRestore = oldRestore
	}()

	var restored bool
	var state mobyterm.State
	terminalGetFdInfo = func(interface{}) (uintptr, bool) {
		return 7, true
	}
	terminalSetRaw = func(fd uintptr) (*mobyterm.State, error) {
		if fd != 7 {
			t.Fatalf("terminalSetRaw fd = %d, want 7", fd)
		}
		return &state, nil
	}
	terminalRestore = func(fd uintptr, restoredState *mobyterm.State) error {
		if fd != 7 {
			t.Fatalf("terminalRestore fd = %d, want 7", fd)
		}
		if restoredState != &state {
			t.Fatalf("terminalRestore state = %p, want %p", restoredState, &state)
		}
		restored = true
		return nil
	}

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	defer stdinR.Close()
	_ = stdinW.Close()

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()

	var stdout bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.WriteString(serverConn, "session closed")
		_ = serverConn.Close()
	}()

	if err := runInteractiveConnWithIO(clientConn, stdinR, &stdout); err != nil {
		t.Fatalf("runInteractiveConnWithIO() error = %v", err)
	}
	<-done

	if !restored {
		t.Fatal("runInteractiveConnWithIO() did not restore terminal state")
	}
	if got := stdout.String(); got != "session closed\x1b[?25h\r\n" {
		t.Fatalf("stdout = %q, want %q", got, "session closed\x1b[?25h\r\n")
	}
}

func TestResolveInteractiveRunCommandAndEffectiveSlice(t *testing.T) {
	base := []string{"sh", "-c"}
	override := []string{"bash"}
	if got := effectiveCommandSlice(override, base); !reflect.DeepEqual(got, override) {
		t.Fatalf("effectiveCommandSlice(override) = %#v", got)
	}
	if got := effectiveCommandSlice(nil, base); !reflect.DeepEqual(got, base) {
		t.Fatalf("effectiveCommandSlice(base) = %#v", got)
	}

	command := resolveInteractiveRunCommand(
		oci.ImageConfig{Entrypoint: []string{"python"}, Cmd: []string{"app.py"}},
		container.RunOptions{Entrypoint: []string{"python"}, Cmd: []string{"app.py"}},
	)
	if !reflect.DeepEqual(command, []string{"python", "app.py"}) {
		t.Fatalf("resolveInteractiveRunCommand() = %#v", command)
	}
}

func TestAPIPostAndStandaloneHelpers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			_, _ = w.Write([]byte("done"))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad"))
	}))
	defer server.Close()

	resp, err := apiPOST(server.URL+"/ok", `{"ok":true}`)
	if err != nil || resp != "done" {
		t.Fatalf("apiPOST(ok) = (%q, %v)", resp, err)
	}
	if _, err := apiPOST(server.URL+"/bad", `{"ok":false}`); err == nil {
		t.Fatal("apiPOST(bad) error = nil")
	}

	exe := filepath.Join(t.TempDir(), "helper")
	writeTestFile(t, exe, []byte("#!/bin/sh\nexit 0\n"))
	if err := os.Chmod(exe, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := resolveStandaloneBinary(exe)
	if err != nil || got != exe {
		t.Fatalf("resolveStandaloneBinary() = (%q, %v)", got, err)
	}
	if _, err := resolveStandaloneBinary(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("resolveStandaloneBinary(missing) error = nil")
	}
}

func TestExecStreamHelpers(t *testing.T) {
	handle := &testHandle{
		cfg: vmm.Config{
			ID:   "vm-1",
			Exec: &vmm.ExecConfig{Enabled: true, VsockPort: 1234},
		},
		state: vmm.StateRunning,
	}
	handle.dial = func(port uint32) (net.Conn, error) {
		if port != 1234 {
			t.Fatalf("DialVsock port = %d", port)
		}
		serverConn, clientConn := net.Pipe()
		go func() {
			defer serverConn.Close()
			var req guestexec.Request
			if err := guestexec.Decode(serverConn, &req); err != nil {
				t.Errorf("Decode() error = %v", err)
				return
			}
			_ = guestexec.Encode(serverConn, guestexec.Response{OK: true})
		}()
		return clientConn, nil
	}

	conn, err := openLocalExecStream(handle, internalapi.ExecRequest{Command: []string{"sh"}, Columns: 80, Rows: 24})
	if err != nil {
		t.Fatalf("openLocalExecStream() error = %v", err)
	}
	_ = conn.Close()

	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	cw := &closeWriterConn{Conn: c1}
	closeNetWriter(cw)
	if !cw.closed {
		t.Fatal("closeNetWriter() did not call CloseWrite")
	}
}

func TestStopVMAndWait(t *testing.T) {
	handle := &testHandle{
		cfg:   vmm.Config{ID: "vm-1"},
		state: vmm.StateRunning,
	}
	stopVMAndWait(handle, 10*time.Millisecond)
	if handle.stopCalls != 1 {
		t.Fatalf("stopCalls = %d", handle.stopCalls)
	}
}
