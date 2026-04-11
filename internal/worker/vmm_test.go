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
