//go:build linux

package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestRunJailerSuccess(t *testing.T) {
	orig := runJailerCLI
	runJailerCLI = func(args []string) error {
		if got := strings.Join(args, ","); got != "--help" {
			t.Fatalf("args = %q", got)
		}
		return nil
	}
	defer func() { runJailerCLI = orig }()

	var stderr bytes.Buffer
	if code := run([]string{"--help"}, &stderr); code != 0 {
		t.Fatalf("run() code = %d, want 0", code)
	}
}

func TestRunJailerError(t *testing.T) {
	orig := runJailerCLI
	runJailerCLI = func([]string) error { return errors.New("bad jail") }
	defer func() { runJailerCLI = orig }()

	var stderr bytes.Buffer
	if code := run(nil, &stderr); code != 1 {
		t.Fatalf("run() code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "error: bad jail") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
