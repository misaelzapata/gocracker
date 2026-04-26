package buildinfo

import (
	"runtime"
	"strings"
	"testing"
)

func TestStringIncludesGoVersionAndArch(t *testing.T) {
	got := String()
	if !strings.Contains(got, runtime.Version()) {
		t.Errorf("String() = %q, expected to contain %q", got, runtime.Version())
	}
	if !strings.Contains(got, runtime.GOOS) {
		t.Errorf("String() = %q, expected to contain %q", got, runtime.GOOS)
	}
	if !strings.Contains(got, runtime.GOARCH) {
		t.Errorf("String() = %q, expected to contain %q", got, runtime.GOARCH)
	}
}

func TestStringStartsWithGocracker(t *testing.T) {
	got := String()
	if !strings.HasPrefix(got, "gocracker ") {
		t.Errorf("String() = %q, expected to start with 'gocracker '", got)
	}
}

func TestVersionDefaultsSafe(t *testing.T) {
	// All three default values must be non-empty strings so dev-builds
	// never produce a version line like "gocracker  (, )".
	for _, v := range []struct {
		name string
		val  string
	}{{"Version", Version}, {"Commit", Commit}, {"Date", Date}} {
		if v.val == "" {
			t.Errorf("%s default is empty — would produce a malformed version string", v.name)
		}
	}
}
