package main

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/gocracker/gocracker/internal/vmmserver"
	"github.com/gocracker/gocracker/pkg/vmm"
)

type fakeUnixListener struct {
	listenPath string
	listenErr  error
	closed     bool
}

func (f *fakeUnixListener) Close() { f.closed = true }
func (f *fakeUnixListener) ListenUnix(path string) error {
	f.listenPath = path
	return f.listenErr
}

func TestRunVMMPassesOptions(t *testing.T) {
	origServer := newVMMServer
	origNotify := notifySignals
	server := &fakeUnixListener{listenErr: errors.New("listen failed")}
	var gotOpts vmmserver.Options
	newVMMServer = func(opts vmmserver.Options) unixListener {
		gotOpts = opts
		return server
	}
	notifySignals = func(c chan<- os.Signal, sig ...os.Signal) {}
	defer func() {
		newVMMServer = origServer
		notifySignals = origNotify
	}()

	var stderr bytes.Buffer
	code := run([]string{"-socket", "/tmp/test.sock", "-default-x86-boot", string(vmm.X86BootLegacy), "-vm-id", "vm-1"}, &stderr)
	if code != 1 {
		t.Fatalf("run() code = %d, want 1", code)
	}
	if server.listenPath != "/tmp/test.sock" {
		t.Fatalf("listen path = %q", server.listenPath)
	}
	if gotOpts.DefaultX86Boot != vmm.X86BootLegacy || gotOpts.VMID != "vm-1" {
		t.Fatalf("opts = %+v", gotOpts)
	}
}

func TestRunVMMFlagError(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"-bad-flag"}, &stderr); code != 2 {
		t.Fatalf("run() code = %d, want 2", code)
	}
}
