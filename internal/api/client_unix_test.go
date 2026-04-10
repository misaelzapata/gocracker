package api

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestClientListVMsOverUnixSocket(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "api.sock")
	srv := New()
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer func() {
		_ = ln.Close()
		_ = os.Remove(socketPath)
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = http.Serve(ln, srv)
	}()
	defer func() {
		_ = ln.Close()
		<-done
	}()

	client := NewClient("unix://" + socketPath)
	vms, err := client.ListVMs(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListVMs(): %v", err)
	}
	if len(vms) != 0 {
		t.Fatalf("ListVMs() = %#v, want empty list", vms)
	}
}
