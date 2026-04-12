package worker

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gocracker/gocracker/pkg/vmm"
)

func TestAppendForwardedWorkerEnvFlagsIncludesSeccompOverride(t *testing.T) {
	t.Setenv("GOCRACKER_SECCOMP", "off")

	args := appendForwardedWorkerEnvFlags([]string{"--id", "vm-123"})

	want := []string{"--id", "vm-123", "--env", "GOCRACKER_SECCOMP=off"}
	if !slices.Equal(args, want) {
		t.Fatalf("appendForwardedWorkerEnvFlags() = %#v, want %#v", args, want)
	}
}

func TestInsertForwardedWorkerEnvFlagsKeepsEnvBeforeWorkerCommand(t *testing.T) {
	t.Setenv("GOCRACKER_SECCOMP", "off")

	args := insertForwardedWorkerEnvFlags([]string{
		"--id", "snap-123",
		"--uid", "1000",
		"--gid", "1000",
		"--",
		"vmm", "--socket", "/worker/vmm.sock",
	}, 6)

	want := []string{
		"--id", "snap-123",
		"--uid", "1000",
		"--gid", "1000",
		"--env", "GOCRACKER_SECCOMP=off",
		"--",
		"vmm", "--socket", "/worker/vmm.sock",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("insertForwardedWorkerEnvFlags() = %#v, want %#v", args, want)
	}
}

// --- Additional tests ---

func TestFirstNonNegative(t *testing.T) {
	tests := []struct {
		values []int
		want   int
	}{
		{[]int{5, 10}, 5},
		{[]int{-1, 10}, 10},
		{[]int{-1, -2, 3}, 3},
		{[]int{0, 10}, 0},
		{[]int{-1, -2}, 0},
		{nil, 0},
	}
	for _, tt := range tests {
		got := firstNonNegative(tt.values...)
		if got != tt.want {
			t.Errorf("firstNonNegative(%v) = %d, want %d", tt.values, got, tt.want)
		}
	}
}

func TestDefaultChrootBaseDir(t *testing.T) {
	dir := DefaultChrootBaseDir()
	if dir == "" {
		t.Fatal("DefaultChrootBaseDir() returned empty string")
	}
	if !strings.Contains(dir, "gocracker-jailer") {
		t.Fatalf("DefaultChrootBaseDir() = %q, expected to contain gocracker-jailer", dir)
	}
	if !filepath.IsAbs(dir) {
		t.Fatalf("DefaultChrootBaseDir() = %q, expected absolute path", dir)
	}
}

func TestJailerInstanceIDUsesRunDirBasename(t *testing.T) {
	runDir := "/tmp/gocracker-vmm-worker-293311611"
	if got := jailerInstanceID(runDir); got != "gocracker-vmm-worker-293311611" {
		t.Fatalf("jailerInstanceID(%q) = %q", runDir, got)
	}
}

func TestJailerInstanceIDFallsBackForInvalidPath(t *testing.T) {
	if got := jailerInstanceID(""); got != "gocracker-vmm" {
		t.Fatalf("jailerInstanceID(\"\") = %q", got)
	}
}

func TestParseState(t *testing.T) {
	tests := []struct {
		input string
		want  vmm.State
	}{
		{"running", vmm.StateRunning},
		{"Running", vmm.StateRunning},
		{"RUNNING", vmm.StateRunning},
		{" running ", vmm.StateRunning},
		{"paused", vmm.StatePaused},
		{"stopped", vmm.StateStopped},
		{"created", vmm.StateCreated},
		{"", vmm.StateCreated},
		{"unknown", vmm.StateCreated},
	}
	for _, tt := range tests {
		got := parseState(tt.input)
		if got != tt.want {
			t.Errorf("parseState(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestVsockGuestCID(t *testing.T) {
	if got := vsockGuestCID(nil); got != 0 {
		t.Fatalf("vsockGuestCID(nil) = %d, want 0", got)
	}
	cfg := &vmm.VsockConfig{GuestCID: 3}
	if got := vsockGuestCID(cfg); got != 3 {
		t.Fatalf("vsockGuestCID() = %d, want 3", got)
	}
}

func TestExecVsockPort(t *testing.T) {
	if got := execVsockPort(nil); got != 0 {
		t.Fatalf("execVsockPort(nil) = %d, want 0", got)
	}
	cfg := &vmm.ExecConfig{VsockPort: 52}
	if got := execVsockPort(cfg); got != 52 {
		t.Fatalf("execVsockPort() = %d, want 52", got)
	}
}

func TestCloneRateLimiter_Nil(t *testing.T) {
	if got := cloneRateLimiter(nil); got != nil {
		t.Fatalf("cloneRateLimiter(nil) = %v, want nil", got)
	}
}

func TestCloneRateLimiter_DeepCopy(t *testing.T) {
	orig := &vmm.RateLimiterConfig{
		Bandwidth: vmm.TokenBucketConfig{Size: 100},
	}
	clone := cloneRateLimiter(orig)
	if clone == orig {
		t.Fatal("clone should be a different pointer")
	}
	if clone.Bandwidth.Size != 100 {
		t.Fatalf("clone Bandwidth.Size = %d, want 100", clone.Bandwidth.Size)
	}
	clone.Bandwidth.Size = 999
	if orig.Bandwidth.Size != 100 {
		t.Fatal("modifying clone affected original")
	}
}

func TestResolveLauncher_Explicit(t *testing.T) {
	exe, prefix, err := resolveLauncher("/usr/bin/mybin", "vmm")
	if err != nil {
		t.Fatalf("resolveLauncher: %v", err)
	}
	if exe != "/usr/bin/mybin" {
		t.Fatalf("exe = %q, want /usr/bin/mybin", exe)
	}
	if len(prefix) != 0 {
		t.Fatalf("prefix = %v, want empty", prefix)
	}
}

func TestResolveLauncher_SelfDiscover(t *testing.T) {
	exe, prefix, err := resolveLauncher("", "vmm")
	if err != nil {
		t.Fatalf("resolveLauncher: %v", err)
	}
	if exe == "" {
		t.Fatal("expected non-empty exe from self-discovery")
	}
	if len(prefix) != 1 || prefix[0] != "vmm" {
		t.Fatalf("prefix = %v, want [vmm]", prefix)
	}
}

func TestSubprocessLogBuffer_Tail(t *testing.T) {
	buf := &subprocessLogBuffer{}

	// Empty buffer
	if got := buf.Tail(10); got != "" {
		t.Fatalf("Tail on empty buffer = %q, want empty", got)
	}

	buf.buf.WriteString("line1\nline2\nline3\n")
	got := buf.Tail(2)
	if !strings.Contains(got, "line2") || !strings.Contains(got, "line3") {
		t.Fatalf("Tail(2) = %q, expected last 2 lines", got)
	}

	got = buf.Tail(0)
	lines := strings.Split(got, "\n")
	if len(lines) != 3 {
		t.Fatalf("Tail(0) returned %d lines, want 3", len(lines))
	}
}

func TestSubprocessLogBuffer_TailSkipsBlankLines(t *testing.T) {
	buf := &subprocessLogBuffer{}
	buf.buf.WriteString("line1\n\n\nline2\n\n")
	got := buf.Tail(10)
	lines := strings.Split(got, "\n")
	if len(lines) != 2 {
		t.Fatalf("Tail should skip blank lines, got %d lines: %q", len(lines), got)
	}
}

func TestWrapSubprocessError_NilError(t *testing.T) {
	if got := wrapSubprocessError(nil, nil); got != nil {
		t.Fatalf("wrapSubprocessError(nil, nil) = %v, want nil", got)
	}
}

func TestWrapSubprocessError_WithLogs(t *testing.T) {
	buf := &subprocessLogBuffer{}
	buf.buf.WriteString("some error output\n")
	err := wrapSubprocessError(os.ErrNotExist, buf)
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	if !strings.Contains(err.Error(), "some error output") {
		t.Fatalf("error = %q, expected to contain log tail", err.Error())
	}
}

func TestAppendForwardedWorkerEnvFlags_NoEnvSet(t *testing.T) {
	t.Setenv("GOCRACKER_SECCOMP", "")
	os.Unsetenv("GOCRACKER_SECCOMP")

	args := appendForwardedWorkerEnvFlags([]string{"--id", "vm-1"})
	want := []string{"--id", "vm-1"}
	if !slices.Equal(args, want) {
		t.Fatalf("appendForwardedWorkerEnvFlags() = %#v, want %#v (no env forwarded)", args, want)
	}
}

// --- Coverage-boosting tests ---

func TestVMMOptionsFields(t *testing.T) {
	opts := VMMOptions{
		JailerBinary: "/usr/bin/jailer",
		VMMBinary:    "/usr/bin/vmm",
		UID:          1000,
		GID:          1000,
		ChrootBase:   "/srv/jailer",
		NetNS:        "/var/run/netns/vm1",
	}
	if opts.JailerBinary != "/usr/bin/jailer" {
		t.Fatalf("JailerBinary = %q", opts.JailerBinary)
	}
	if opts.VMMBinary != "/usr/bin/vmm" {
		t.Fatalf("VMMBinary = %q", opts.VMMBinary)
	}
	if opts.UID != 1000 || opts.GID != 1000 {
		t.Fatalf("UID/GID = %d/%d", opts.UID, opts.GID)
	}
	if opts.ChrootBase != "/srv/jailer" {
		t.Fatalf("ChrootBase = %q", opts.ChrootBase)
	}
	if opts.NetNS != "/var/run/netns/vm1" {
		t.Fatalf("NetNS = %q", opts.NetNS)
	}
}

func TestParseState_AllCases(t *testing.T) {
	tests := []struct {
		input string
		want  vmm.State
	}{
		{"running", vmm.StateRunning},
		{"Running", vmm.StateRunning},
		{"RUNNING", vmm.StateRunning},
		{" running ", vmm.StateRunning},
		{"paused", vmm.StatePaused},
		{"Paused", vmm.StatePaused},
		{"PAUSED", vmm.StatePaused},
		{" paused ", vmm.StatePaused},
		{"stopped", vmm.StateStopped},
		{"Stopped", vmm.StateStopped},
		{"STOPPED", vmm.StateStopped},
		{" stopped ", vmm.StateStopped},
		{"created", vmm.StateCreated},
		{"Created", vmm.StateCreated},
		{"", vmm.StateCreated},
		{"unknown", vmm.StateCreated},
		{"  ", vmm.StateCreated},
		{"invalid-state", vmm.StateCreated},
	}
	for _, tt := range tests {
		got := parseState(tt.input)
		if got != tt.want {
			t.Errorf("parseState(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestVsockGuestCID_AllCases(t *testing.T) {
	tests := []struct {
		name string
		cfg  *vmm.VsockConfig
		want uint32
	}{
		{"nil config", nil, 0},
		{"zero cid", &vmm.VsockConfig{GuestCID: 0}, 0},
		{"cid 3", &vmm.VsockConfig{GuestCID: 3}, 3},
		{"cid 100", &vmm.VsockConfig{GuestCID: 100}, 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := vsockGuestCID(tt.cfg); got != tt.want {
				t.Fatalf("vsockGuestCID() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestExecVsockPort_AllCases(t *testing.T) {
	tests := []struct {
		name string
		cfg  *vmm.ExecConfig
		want uint32
	}{
		{"nil config", nil, 0},
		{"zero port", &vmm.ExecConfig{VsockPort: 0}, 0},
		{"port 52", &vmm.ExecConfig{VsockPort: 52}, 52},
		{"port 1000", &vmm.ExecConfig{VsockPort: 1000}, 1000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := execVsockPort(tt.cfg); got != tt.want {
				t.Fatalf("execVsockPort() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCloneRateLimiter_AllBranches(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		if got := cloneRateLimiter(nil); got != nil {
			t.Fatalf("cloneRateLimiter(nil) = %v, want nil", got)
		}
	})
	t.Run("deep copy", func(t *testing.T) {
		orig := &vmm.RateLimiterConfig{
			Bandwidth: vmm.TokenBucketConfig{Size: 100, OneTimeBurst: 50, RefillTimeMs: 1000},
			Ops:       vmm.TokenBucketConfig{Size: 200},
		}
		clone := cloneRateLimiter(orig)
		if clone == orig {
			t.Fatal("clone should be a different pointer")
		}
		if clone.Bandwidth.Size != 100 || clone.Bandwidth.OneTimeBurst != 50 {
			t.Fatalf("clone values wrong: %+v", clone)
		}
		if clone.Ops.Size != 200 {
			t.Fatalf("clone Ops.Size = %d", clone.Ops.Size)
		}
		clone.Bandwidth.Size = 999
		if orig.Bandwidth.Size != 100 {
			t.Fatal("modifying clone affected original")
		}
	})
	t.Run("zero values", func(t *testing.T) {
		orig := &vmm.RateLimiterConfig{}
		clone := cloneRateLimiter(orig)
		if clone == orig {
			t.Fatal("clone should be a different pointer")
		}
		if clone.Bandwidth.Size != 0 || clone.Ops.Size != 0 {
			t.Fatalf("zero values not preserved: %+v", clone)
		}
	})
}

func TestResolveLauncher_AllBranches(t *testing.T) {
	t.Run("explicit path", func(t *testing.T) {
		exe, prefix, err := resolveLauncher("/usr/bin/mybin", "vmm")
		if err != nil {
			t.Fatalf("error: %v", err)
		}
		if exe != "/usr/bin/mybin" {
			t.Fatalf("exe = %q", exe)
		}
		if len(prefix) != 0 {
			t.Fatalf("prefix = %v, want empty", prefix)
		}
	})
	t.Run("explicit jailer path", func(t *testing.T) {
		exe, prefix, err := resolveLauncher("/usr/bin/jailer", "jailer")
		if err != nil {
			t.Fatalf("error: %v", err)
		}
		if exe != "/usr/bin/jailer" {
			t.Fatalf("exe = %q", exe)
		}
		if len(prefix) != 0 {
			t.Fatalf("prefix = %v", prefix)
		}
	})
	t.Run("self-discover vmm", func(t *testing.T) {
		exe, prefix, err := resolveLauncher("", "vmm")
		if err != nil {
			t.Fatalf("error: %v", err)
		}
		if exe == "" {
			t.Fatal("expected non-empty exe")
		}
		if !filepath.IsAbs(exe) {
			t.Fatalf("expected absolute path, got %q", exe)
		}
		if len(prefix) != 1 || prefix[0] != "vmm" {
			t.Fatalf("prefix = %v, want [vmm]", prefix)
		}
	})
	t.Run("self-discover jailer", func(t *testing.T) {
		exe, prefix, err := resolveLauncher("", "jailer")
		if err != nil {
			t.Fatalf("error: %v", err)
		}
		if exe == "" {
			t.Fatal("expected non-empty exe")
		}
		if len(prefix) != 1 || prefix[0] != "jailer" {
			t.Fatalf("prefix = %v, want [jailer]", prefix)
		}
	})
}

func TestSubprocessLogBuffer_Write(t *testing.T) {
	buf := &subprocessLogBuffer{}
	n, err := buf.Write([]byte("hello\n"))
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if n != 6 {
		t.Fatalf("Write returned %d, want 6", n)
	}
	if got := buf.Tail(10); got != "hello" {
		t.Fatalf("Tail after Write = %q, want hello", got)
	}
}

func TestSubprocessLogBuffer_MultipleWrites(t *testing.T) {
	buf := &subprocessLogBuffer{}
	buf.Write([]byte("line1\n"))
	buf.Write([]byte("line2\n"))
	buf.Write([]byte("line3\n"))
	got := buf.Tail(2)
	if !strings.Contains(got, "line2") || !strings.Contains(got, "line3") {
		t.Fatalf("Tail(2) = %q, expected line2 and line3", got)
	}
	if strings.Contains(got, "line1") {
		t.Fatalf("Tail(2) = %q, should not contain line1", got)
	}
}

func TestSubprocessLogBuffer_TailAllLines(t *testing.T) {
	buf := &subprocessLogBuffer{}
	buf.buf.WriteString("a\nb\nc\n")
	got := buf.Tail(0)
	lines := strings.Split(got, "\n")
	if len(lines) != 3 {
		t.Fatalf("Tail(0) returned %d lines, want 3: %q", len(lines), got)
	}
}

func TestSubprocessLogBuffer_OnlyBlankLines(t *testing.T) {
	buf := &subprocessLogBuffer{}
	buf.buf.WriteString("\n\n\n  \n")
	if got := buf.Tail(10); got != "" {
		t.Fatalf("Tail on only-blank lines = %q, want empty", got)
	}
}

func TestSubprocessLogBuffer_CRLFHandling(t *testing.T) {
	buf := &subprocessLogBuffer{}
	buf.buf.WriteString("line1\r\nline2\r\n")
	got := buf.Tail(10)
	if !strings.Contains(got, "line1") || !strings.Contains(got, "line2") {
		t.Fatalf("CRLF handling: got %q", got)
	}
}

func TestWrapSubprocessError_AllBranches(t *testing.T) {
	t.Run("nil error nil logs", func(t *testing.T) {
		if got := wrapSubprocessError(nil, nil); got != nil {
			t.Fatalf("got %v, want nil", got)
		}
	})
	t.Run("nil error with logs", func(t *testing.T) {
		buf := &subprocessLogBuffer{}
		buf.buf.WriteString("some output\n")
		if got := wrapSubprocessError(nil, buf); got != nil {
			t.Fatalf("got %v, want nil", got)
		}
	})
	t.Run("error with nil logs", func(t *testing.T) {
		err := wrapSubprocessError(os.ErrNotExist, nil)
		if err != os.ErrNotExist {
			t.Fatalf("got %v, want %v", err, os.ErrNotExist)
		}
	})
	t.Run("error with logs", func(t *testing.T) {
		buf := &subprocessLogBuffer{}
		buf.buf.WriteString("error: something failed\n")
		err := wrapSubprocessError(os.ErrNotExist, buf)
		if err == nil {
			t.Fatal("expected non-nil error")
		}
		if !strings.Contains(err.Error(), "something failed") {
			t.Fatalf("error = %q, expected log tail", err.Error())
		}
		if !strings.Contains(err.Error(), "worker log tail") {
			t.Fatalf("error = %q, expected 'worker log tail' prefix", err.Error())
		}
	})
	t.Run("error with empty logs", func(t *testing.T) {
		buf := &subprocessLogBuffer{}
		err := wrapSubprocessError(os.ErrNotExist, buf)
		if err != os.ErrNotExist {
			t.Fatalf("got %v, want %v (empty logs should return original)", err, os.ErrNotExist)
		}
	})
}

func TestFirstNonNegative_ExtendedCases(t *testing.T) {
	tests := []struct {
		values []int
		want   int
	}{
		{[]int{5}, 5},
		{[]int{0}, 0},
		{[]int{-1}, 0},
		{[]int{-1, -2, -3}, 0},
		{[]int{-1, 0, 1}, 0},
		{[]int{100, 200, 300}, 100},
		{[]int{-100, -200, 50}, 50},
	}
	for _, tt := range tests {
		got := firstNonNegative(tt.values...)
		if got != tt.want {
			t.Errorf("firstNonNegative(%v) = %d, want %d", tt.values, got, tt.want)
		}
	}
}

func TestDefaultChrootBaseDir_Properties(t *testing.T) {
	dir := DefaultChrootBaseDir()
	if dir == "" {
		t.Fatal("returned empty string")
	}
	if !filepath.IsAbs(dir) {
		t.Fatalf("not absolute: %q", dir)
	}
	if !strings.Contains(dir, "gocracker-jailer") {
		t.Fatalf("missing gocracker-jailer: %q", dir)
	}
	// Should be deterministic
	dir2 := DefaultChrootBaseDir()
	if dir != dir2 {
		t.Fatalf("not deterministic: %q != %q", dir, dir2)
	}
}

func TestAppendForwardedWorkerEnvFlags_WithMultipleEnvVars(t *testing.T) {
	t.Setenv("GOCRACKER_SECCOMP", "trace")
	args := appendForwardedWorkerEnvFlags([]string{"--id", "test"})
	found := false
	for i, arg := range args {
		if arg == "--env" && i+1 < len(args) && args[i+1] == "GOCRACKER_SECCOMP=trace" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected GOCRACKER_SECCOMP=trace in args: %v", args)
	}
}

func TestInsertForwardedWorkerEnvFlags_NoEnv(t *testing.T) {
	// Unset env so nothing gets forwarded
	origVal, wasSet := os.LookupEnv("GOCRACKER_SECCOMP")
	os.Unsetenv("GOCRACKER_SECCOMP")
	defer func() {
		if wasSet {
			os.Setenv("GOCRACKER_SECCOMP", origVal)
		}
	}()

	args := []string{"--id", "snap-1", "--", "vmm", "--socket", "/worker/vmm.sock"}
	result := insertForwardedWorkerEnvFlags(args, 2)
	if !slices.Equal(result, args) {
		t.Fatalf("expected no change when no env set: %v", result)
	}
}

func TestReattachOptionsFields(t *testing.T) {
	opts := ReattachOptions{
		Config: vmm.Config{ID: "test-vm", MemMB: 256},
		Metadata: vmm.WorkerMetadata{
			Kind:       "worker",
			SocketPath: "/tmp/vmm.sock",
			WorkerPID:  1234,
		},
	}
	if opts.Config.ID != "test-vm" {
		t.Fatalf("Config.ID = %q", opts.Config.ID)
	}
	if opts.Metadata.SocketPath != "/tmp/vmm.sock" {
		t.Fatalf("Metadata.SocketPath = %q", opts.Metadata.SocketPath)
	}
}

// --- New coverage-boosting tests ---

func TestWorkerProcessAlive_InvalidPID(t *testing.T) {
	// pid <= 0 should return true (assume alive)
	if !workerProcessAlive(0) {
		t.Error("workerProcessAlive(0) = false, want true")
	}
	if !workerProcessAlive(-1) {
		t.Error("workerProcessAlive(-1) = false, want true")
	}
}

func TestWorkerProcessAlive_CurrentProcess(t *testing.T) {
	// Our own process should be alive
	if !workerProcessAlive(os.Getpid()) {
		t.Error("workerProcessAlive(self) = false, want true")
	}
}

func TestWorkerProcessAlive_NonexistentPID(t *testing.T) {
	// A very high PID is unlikely to exist
	alive := workerProcessAlive(1<<22 - 1)
	// We can't assert the exact result since it depends on system state,
	// but verify it doesn't panic.
	_ = alive
}

func TestSocketReachable_EmptyPath(t *testing.T) {
	if socketReachable("") {
		t.Error("socketReachable('') = true, want false")
	}
}

func TestSocketReachable_NonexistentPath(t *testing.T) {
	if socketReachable("/nonexistent/path/to/socket.sock") {
		t.Error("socketReachable(nonexistent) = true, want false")
	}
}

func TestSocketReachable_RealSocket(t *testing.T) {
	// Create a real Unix socket to test positive case
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	if !socketReachable(sockPath) {
		t.Error("socketReachable(real socket) = false, want true")
	}
}

func TestCloneVMLimiter_Nil(t *testing.T) {
	if got := cloneVMLimiter(nil); got != nil {
		t.Fatalf("cloneVMLimiter(nil) = %v, want nil", got)
	}
}

func TestCloneVMLimiter_DeepCopy(t *testing.T) {
	orig := &vmm.RateLimiterConfig{
		Bandwidth: vmm.TokenBucketConfig{Size: 500, OneTimeBurst: 100, RefillTimeMs: 2000},
		Ops:       vmm.TokenBucketConfig{Size: 300},
	}
	clone := cloneVMLimiter(orig)
	if clone == orig {
		t.Fatal("clone should be a different pointer")
	}
	if clone.Bandwidth.Size != 500 || clone.Ops.Size != 300 {
		t.Fatalf("clone values wrong: %+v", clone)
	}
	clone.Bandwidth.Size = 999
	if orig.Bandwidth.Size != 500 {
		t.Fatal("modifying clone affected original")
	}
}

func TestForwardedWorkerEnv_NoEnvSet(t *testing.T) {
	origVal, wasSet := os.LookupEnv("GOCRACKER_SECCOMP")
	os.Unsetenv("GOCRACKER_SECCOMP")
	defer func() {
		if wasSet {
			os.Setenv("GOCRACKER_SECCOMP", origVal)
		}
	}()

	env := forwardedWorkerEnv()
	if len(env) != 0 {
		t.Fatalf("forwardedWorkerEnv() = %v, want empty", env)
	}
}

func TestForwardedWorkerEnv_WithEnvSet(t *testing.T) {
	t.Setenv("GOCRACKER_SECCOMP", "log")
	env := forwardedWorkerEnv()
	if len(env) != 1 || env[0] != "GOCRACKER_SECCOMP=log" {
		t.Fatalf("forwardedWorkerEnv() = %v, want [GOCRACKER_SECCOMP=log]", env)
	}
}

func TestWaitForSocket_NonexistentPath(t *testing.T) {
	err := waitForSocket("/nonexistent/socket.sock", 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %q, want 'timed out'", err.Error())
	}
}

func TestWaitForSocket_RealSocket(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	if err := waitForSocket(sockPath, 2*time.Second); err != nil {
		t.Fatalf("waitForSocket(real) = %v", err)
	}
}

func TestWaitForSocketOrExit_Timeout(t *testing.T) {
	waitErrCh := make(chan error, 1)
	exited, err := waitForSocketOrExit("/nonexistent/socket.sock", 100*time.Millisecond, waitErrCh)
	if err == nil {
		t.Fatal("expected error")
	}
	if exited {
		t.Error("exited should be false on timeout")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %q, want 'timed out'", err.Error())
	}
}

func TestWaitForSocketOrExit_ProcessExitsCleanly(t *testing.T) {
	waitErrCh := make(chan error, 1)
	waitErrCh <- nil // process exited cleanly before socket
	exited, err := waitForSocketOrExit("/nonexistent/socket.sock", 2*time.Second, waitErrCh)
	if err == nil {
		t.Fatal("expected error")
	}
	if !exited {
		t.Error("exited should be true")
	}
	if !strings.Contains(err.Error(), "worker exited before opening socket") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestWaitForSocketOrExit_ProcessExitsWithError(t *testing.T) {
	waitErrCh := make(chan error, 1)
	waitErrCh <- fmt.Errorf("exit status 1")
	exited, err := waitForSocketOrExit("/nonexistent/socket.sock", 2*time.Second, waitErrCh)
	if err == nil {
		t.Fatal("expected error")
	}
	if !exited {
		t.Error("exited should be true")
	}
	if !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("error = %q, want to contain exit status", err.Error())
	}
}

func TestWaitForSocketOrExit_SocketReady(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	waitErrCh := make(chan error, 1)
	exited, err := waitForSocketOrExit(sockPath, 2*time.Second, waitErrCh)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if exited {
		t.Error("exited should be false when socket ready")
	}
}

func TestCopyTree_SimpleDirectory(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "dest")

	// Create files in src
	os.MkdirAll(filepath.Join(src, "subdir"), 0755)
	os.WriteFile(filepath.Join(src, "file.txt"), []byte("hello"), 0644)
	os.WriteFile(filepath.Join(src, "subdir", "nested.txt"), []byte("world"), 0644)

	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copyTree: %v", err)
	}

	// Verify files exist in dst
	data, err := os.ReadFile(filepath.Join(dst, "file.txt"))
	if err != nil {
		t.Fatalf("read file.txt: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("file.txt = %q, want hello", string(data))
	}
	data, err = os.ReadFile(filepath.Join(dst, "subdir", "nested.txt"))
	if err != nil {
		t.Fatalf("read nested.txt: %v", err)
	}
	if string(data) != "world" {
		t.Fatalf("nested.txt = %q, want world", string(data))
	}
}

func TestCopyTree_EmptyDirectory(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "dest")
	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copyTree empty: %v", err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("dst should be a directory")
	}
}

func TestSubprocessLogBuffer_ConcurrentWrites(t *testing.T) {
	buf := &subprocessLogBuffer{}
	done := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 100; j++ {
				buf.Write([]byte(fmt.Sprintf("goroutine %d line %d\n", n, j)))
			}
		}(i)
	}
	for i := 0; i < 10; i++ {
		<-done
	}
	tail := buf.Tail(5)
	lines := strings.Split(tail, "\n")
	if len(lines) != 5 {
		t.Fatalf("Tail(5) after concurrent writes = %d lines, want 5", len(lines))
	}
}

func TestSubprocessLogBuffer_LargeWrite(t *testing.T) {
	buf := &subprocessLogBuffer{}
	// Write many lines
	for i := 0; i < 1000; i++ {
		buf.Write([]byte(fmt.Sprintf("line %d\n", i)))
	}
	tail := buf.Tail(3)
	lines := strings.Split(tail, "\n")
	if len(lines) != 3 {
		t.Fatalf("Tail(3) after 1000 writes = %d lines", len(lines))
	}
	// Should contain lines 997, 998, 999
	if !strings.Contains(lines[2], "999") {
		t.Fatalf("last line = %q, want to contain 999", lines[2])
	}
}

func TestInsertForwardedWorkerEnvFlags_AtEnd(t *testing.T) {
	t.Setenv("GOCRACKER_SECCOMP", "enforce")
	args := []string{"--id", "vm-1", "--uid", "1000"}
	result := insertForwardedWorkerEnvFlags(args, 4)
	// env flags should be at index 4
	want := []string{"--id", "vm-1", "--uid", "1000", "--env", "GOCRACKER_SECCOMP=enforce"}
	if !slices.Equal(result, want) {
		t.Fatalf("got %v, want %v", result, want)
	}
}

func TestInsertForwardedWorkerEnvFlags_AtStart(t *testing.T) {
	t.Setenv("GOCRACKER_SECCOMP", "off")
	args := []string{"--", "vmm", "--socket", "/worker/vmm.sock"}
	result := insertForwardedWorkerEnvFlags(args, 0)
	if result[0] != "--env" || result[1] != "GOCRACKER_SECCOMP=off" {
		t.Fatalf("expected env flags at start, got %v", result)
	}
}

// --- remoteVM unit tests (no real subprocess/client) ---

func TestRemoteVM_State(t *testing.T) {
	rvm := &remoteVM{state: vmm.StateRunning}
	if rvm.State() != vmm.StateRunning {
		t.Fatalf("State() = %v, want Running", rvm.State())
	}
}

func TestRemoteVM_ID(t *testing.T) {
	rvm := &remoteVM{cfg: vmm.Config{ID: "test-vm"}}
	if rvm.ID() != "test-vm" {
		t.Fatalf("ID() = %q", rvm.ID())
	}
}

func TestRemoteVM_Uptime_NotStarted(t *testing.T) {
	rvm := &remoteVM{state: vmm.StateCreated}
	if rvm.Uptime() != 0 {
		t.Fatalf("Uptime before start = %v", rvm.Uptime())
	}
}

func TestRemoteVM_Uptime_Running(t *testing.T) {
	rvm := &remoteVM{
		state:   vmm.StateRunning,
		started: time.Now().Add(-5 * time.Second),
	}
	up := rvm.Uptime()
	if up < 4*time.Second || up > 10*time.Second {
		t.Fatalf("Uptime() = %v, expected ~5s", up)
	}
}

func TestRemoteVM_Uptime_Paused(t *testing.T) {
	rvm := &remoteVM{
		state:  vmm.StatePaused,
		uptime: 10 * time.Second,
	}
	if rvm.Uptime() != 10*time.Second {
		t.Fatalf("Uptime() = %v, want 10s", rvm.Uptime())
	}
}

func TestRemoteVM_Events(t *testing.T) {
	el := vmm.NewEventLog()
	rvm := &remoteVM{events: el}
	if rvm.Events() != el {
		t.Fatal("Events() should return event log")
	}
}

func TestRemoteVM_VMConfig(t *testing.T) {
	rvm := &remoteVM{cfg: vmm.Config{MemMB: 512}}
	if rvm.VMConfig().MemMB != 512 {
		t.Fatalf("VMConfig().MemMB = %d", rvm.VMConfig().MemMB)
	}
}

func TestRemoteVM_WorkerMetadata(t *testing.T) {
	created := time.Now()
	rvm := &remoteVM{
		socket:   "/tmp/vmm.sock",
		pid:      1234,
		jailRoot: "/srv/jailer/root",
		runDir:   "/tmp/run",
		created:  created,
	}
	meta := rvm.WorkerMetadata()
	if meta.Kind != "worker" {
		t.Fatalf("Kind = %q", meta.Kind)
	}
	if meta.SocketPath != "/tmp/vmm.sock" {
		t.Fatalf("SocketPath = %q", meta.SocketPath)
	}
	if meta.WorkerPID != 1234 {
		t.Fatalf("WorkerPID = %d", meta.WorkerPID)
	}
	if meta.JailRoot != "/srv/jailer/root" {
		t.Fatalf("JailRoot = %q", meta.JailRoot)
	}
	if meta.RunDir != "/tmp/run" {
		t.Fatalf("RunDir = %q", meta.RunDir)
	}
	if meta.CreatedAt != created {
		t.Fatalf("CreatedAt mismatch")
	}
}

func TestRemoteVM_DeviceList_Empty(t *testing.T) {
	rvm := &remoteVM{}
	devices := rvm.DeviceList()
	if len(devices) != 0 {
		t.Fatalf("DeviceList() = %v, want empty", devices)
	}
}

func TestRemoteVM_DeviceList_ReturnsCopy(t *testing.T) {
	rvm := &remoteVM{
		devices: []vmm.DeviceInfo{{Type: "virtio-net", IRQ: 5}},
	}
	devices := rvm.DeviceList()
	if len(devices) != 1 || devices[0].Type != "virtio-net" {
		t.Fatalf("DeviceList() = %v", devices)
	}
	// Ensure it's a copy
	devices[0].Type = "modified"
	if rvm.devices[0].Type != "virtio-net" {
		t.Fatal("DeviceList() returned reference instead of copy")
	}
}

func TestRemoteVM_Start(t *testing.T) {
	rvm := &remoteVM{}
	if err := rvm.Start(); err != nil {
		t.Fatalf("Start() = %v", err)
	}
}

func TestRemoteVM_WaitStopped_AlreadyDone(t *testing.T) {
	rvm := &remoteVM{doneCh: make(chan struct{})}
	close(rvm.doneCh)
	if err := rvm.WaitStopped(context.Background()); err != nil {
		t.Fatalf("WaitStopped = %v", err)
	}
}

func TestRemoteVM_WaitStopped_ContextCanceled(t *testing.T) {
	rvm := &remoteVM{doneCh: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rvm.WaitStopped(ctx); err == nil {
		t.Fatal("expected context error")
	}
}

func TestRemoteVM_Finish(t *testing.T) {
	called := false
	rvm := &remoteVM{
		doneCh:  make(chan struct{}),
		events:  vmm.NewEventLog(),
		cleanup: func() { called = true },
	}
	rvm.finish()

	if rvm.State() != vmm.StateStopped {
		t.Fatalf("state after finish = %v", rvm.State())
	}
	select {
	case <-rvm.doneCh:
	default:
		t.Fatal("doneCh not closed")
	}
	if !called {
		t.Fatal("cleanup not called")
	}

	// Idempotent
	rvm.finish()
}

func TestRemoteVM_Finish_NoCleanup(t *testing.T) {
	rvm := &remoteVM{
		doneCh: make(chan struct{}),
		events: vmm.NewEventLog(),
	}
	// Should not panic
	rvm.finish()
}

func TestCopyTree_WithSymlink(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "dest")

	// Create a regular file
	os.WriteFile(filepath.Join(src, "file.txt"), []byte("content"), 0644)

	// Create a symlink
	os.Symlink("file.txt", filepath.Join(src, "link.txt"))

	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copyTree: %v", err)
	}

	// Verify symlink is preserved
	target, err := os.Readlink(filepath.Join(dst, "link.txt"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != "file.txt" {
		t.Fatalf("symlink target = %q, want file.txt", target)
	}
}

func TestCopyTree_NonexistentSource(t *testing.T) {
	err := copyTree("/nonexistent/path", t.TempDir())
	if err == nil {
		t.Fatal("expected error for nonexistent source")
	}
}

func TestJailerInstanceID_EdgeCases(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", "gocracker-vmm"},
		{".", "gocracker-vmm"},
		{"/", "gocracker-vmm"},
		{"  ", "gocracker-vmm"},
		{"/tmp/gocracker-vmm-worker-123", "gocracker-vmm-worker-123"},
		{"/a/b/c", "c"},
	}
	for _, tt := range tests {
		got := jailerInstanceID(tt.input)
		if got != tt.want {
			t.Errorf("jailerInstanceID(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseState_Coverage(t *testing.T) {
	// Ensure "created" maps to StateCreated
	if got := parseState("created"); got != vmm.StateCreated {
		t.Fatalf("parseState(created) = %d", got)
	}
	if got := parseState("Created"); got != vmm.StateCreated {
		t.Fatalf("parseState(Created) = %d", got)
	}
}
