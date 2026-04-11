package worker

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
