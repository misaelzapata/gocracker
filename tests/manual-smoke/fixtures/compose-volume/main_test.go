package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteResultCreatesParentDirectories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "result.txt")
	if err := writeResult(path, "ok\n"); err != nil {
		t.Fatalf("writeResult() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(data) != "ok\n" {
		t.Fatalf("data = %q", data)
	}
}

func TestWriteResultRequiresPath(t *testing.T) {
	if err := writeResult("", "x"); err == nil {
		t.Fatal("writeResult() error = nil, want error")
	}
}
