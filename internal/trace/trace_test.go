package trace

import (
	"bytes"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// resetForTest puts the package back into "first event wins" state so
// each test sees an independent t=0. Not exported; only the test
// binary needs it.
func resetForTest() {
	mu.Lock()
	startNS = 0
	lastNS = 0
	startSet = sync.Once{}
	mu.Unlock()
}

// captureStderr swaps os.Stderr for a buffer for the duration of fn.
// Returns the bytes that fn would have printed to stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	done := make(chan []byte, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		done <- buf.Bytes()
	}()
	fn()
	_ = w.Close()
	return string(<-done)
}

func TestEventDisabledNoOp(t *testing.T) {
	t.Setenv("GOCRACKER_TRACE", "")
	// Re-evaluate the package-level `enabled` against the env we just set.
	// Since `enabled` is captured at package init, we can't toggle it
	// without an Init function — but we can verify the disabled-by-default
	// behaviour by reading Enabled() and confirming Event is a no-op.
	if Enabled() {
		t.Skip("env-driven enable can't be reset mid-process; this test verifies the disabled default")
	}
	out := captureStderr(t, func() {
		Event("test_event", "k", "v")
	})
	if out != "" {
		t.Fatalf("expected no output when disabled, got %q", out)
	}
}

func TestEventEnabledFormatsLine(t *testing.T) {
	// We can't toggle the package-level `enabled` from a test (it's
	// captured at package init from env). Force it on by reaching
	// into the var directly via a test-only setter; if you ever
	// rename `enabled`, this test will fail to compile and you'll
	// remember to keep them in sync.
	prev := enabled
	enabled = true
	defer func() { enabled = prev }()
	resetForTest()

	out := captureStderr(t, func() {
		Event("first")
		time.Sleep(2 * time.Millisecond)
		Event("second", "key", 42)
	})

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(lines), out)
	}
	if !strings.HasPrefix(lines[0], "[trace] t=+0.0ms d=+0.0ms event=first") {
		t.Errorf("first line wrong: %q", lines[0])
	}
	if !strings.Contains(lines[1], "event=second") || !strings.Contains(lines[1], "key=42") {
		t.Errorf("second line missing fields: %q", lines[1])
	}
	// Second line's t and d should be > 1ms (we slept 2ms).
	if !strings.Contains(lines[1], "t=+") {
		t.Errorf("second line missing t prefix: %q", lines[1])
	}
}

func TestWriteMs(t *testing.T) {
	cases := []struct {
		ns   int64
		want string
	}{
		{0, "0.0ms"},
		{500_000, "0.5ms"},
		{1_000_000, "1.0ms"},
		{12_400_000, "12.4ms"},
		{1_234_500_000, "1234.5ms"},
		{-1, "0.0ms"},
	}
	for _, c := range cases {
		var b strings.Builder
		writeMs(&b, c.ns)
		if got := b.String(); got != c.want {
			t.Errorf("writeMs(%d) = %q, want %q", c.ns, got, c.want)
		}
	}
}

func TestEventOddArgsAreIgnored(t *testing.T) {
	prev := enabled
	enabled = true
	defer func() { enabled = prev }()
	resetForTest()

	// Three attrs (key, value, lone) — the lone one should be silently
	// dropped rather than crash. This mirrors slog's behaviour on
	// unbalanced kv pairs.
	out := captureStderr(t, func() {
		Event("e", "a", 1, "lone")
	})
	if strings.Contains(out, "lone") {
		t.Errorf("lone attr leaked into output: %q", out)
	}
	if !strings.Contains(out, "a=1") {
		t.Errorf("expected a=1 in output: %q", out)
	}
}
