package guestexec

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveExecutable_UsesProvidedPATH(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hello")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	got, err := ResolveExecutable("hello", []string{"PATH=" + dir})
	if err != nil {
		t.Fatalf("ResolveExecutable() error = %v", err)
	}
	if got != path {
		t.Fatalf("ResolveExecutable() = %q, want %q", got, path)
	}
}

func TestResolveExecutable_LastPATHWins(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	first := filepath.Join(dir1, "tool")
	if err := os.WriteFile(first, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(first) error = %v", err)
	}
	second := filepath.Join(dir2, "tool")
	if err := os.WriteFile(second, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(second) error = %v", err)
	}

	got, err := ResolveExecutable("tool", []string{
		"PATH=" + dir1,
		"PATH=" + dir2,
	})
	if err != nil {
		t.Fatalf("ResolveExecutable() error = %v", err)
	}
	if got != second {
		t.Fatalf("ResolveExecutable() = %q, want %q", got, second)
	}
}

func TestResolveExecutable_PathWithSlashPassthrough(t *testing.T) {
	got, err := ResolveExecutable("./tool", nil)
	if err != nil {
		t.Fatalf("ResolveExecutable() error = %v", err)
	}
	if got != "./tool" {
		t.Fatalf("ResolveExecutable() = %q, want ./tool", got)
	}
}

func TestResolveExecutable_NotFound(t *testing.T) {
	_, err := ResolveExecutable("missing", []string{"PATH=" + t.TempDir()})
	if err == nil {
		t.Fatal("ResolveExecutable() error = nil, want not found")
	}
}
