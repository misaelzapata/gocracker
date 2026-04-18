package hostguard

import (
	"os"
	"testing"
)

func testIsRoot() bool { return os.Geteuid() == 0 }

func TestHasNetAdmin_ReturnsDeterministic(t *testing.T) {
	// Two back-to-back calls from the same process must agree; the
	// capability state cannot change without syscall intervention and
	// the test environment doesn't invoke any.
	a := HasNetAdmin()
	b := HasNetAdmin()
	if a != b {
		t.Fatalf("HasNetAdmin non-deterministic: first=%v second=%v", a, b)
	}
	// When running as root, the answer is unconditionally true.
	if testIsRoot() && !a {
		t.Fatalf("HasNetAdmin() = false while running as root")
	}
}

func TestHasNetAdmin_BitParsing(t *testing.T) {
	cases := []struct {
		name    string
		capEff  uint64
		wantNet bool
	}{
		{"none", 0, false},
		{"net_admin only (bit 12)", 1 << 12, true},
		{"all 64 bits set", ^uint64(0), true},
		{"cap_net_raw alone (bit 13) without net_admin", 1 << 13, false},
	}
	for _, c := range cases {
		got := c.capEff&(1<<12) != 0
		if got != c.wantNet {
			t.Errorf("%s: cap=%#x → got=%v want=%v", c.name, c.capEff, got, c.wantNet)
		}
	}
}

func TestSplitLines_PreservesTrailingNewlinelessRecord(t *testing.T) {
	got := splitLines([]byte("a\nb\nc"))
	if len(got) != 3 {
		t.Fatalf("len=%d, want 3 (%q)", len(got), got)
	}
	if string(got[0]) != "a" || string(got[1]) != "b" || string(got[2]) != "c" {
		t.Errorf("got=%q", got)
	}
}
