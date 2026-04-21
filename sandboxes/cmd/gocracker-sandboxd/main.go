// gocracker-sandboxd is the sandbox control plane HTTP server.
//
//	gocracker-sandboxd serve --addr :9091 --state-dir /var/lib/gocracker-state
//
// Routes (Fase 4 slice 1):
//   GET    /healthz
//   POST   /sandboxes              — create cold-boot sandbox
//   GET    /sandboxes              — list
//   GET    /sandboxes/{id}         — fetch one
//   DELETE /sandboxes/{id}         — stop + remove
//
// Future slices add /sandboxes/{id}/process/execute, /files,
// /events SSE, warm pool primitives, templates, and preview.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gocracker/gocracker/sandboxes/internal/sandboxd"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: gocracker-sandboxd serve [flags]")
	os.Exit(2)
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":9091", "HTTP listen address")
	stateDir := fs.String("state-dir", "/var/lib/gocracker-sandboxd", "directory for sandbox runtime state (UDS sockets, store)")
	fs.Parse(args)

	if err := os.MkdirAll(*stateDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "mkdir state-dir:", err)
		os.Exit(1)
	}

	store, err := sandboxd.NewStore(filepath.Join(*stateDir, "store.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "open store:", err)
		os.Exit(1)
	}
	mgr := &sandboxd.Manager{Store: store, StateDir: *stateDir}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           sandboxd.NewServer(mgr).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer shutCancel()
		_ = srv.Shutdown(shutCtx)
	}()

	fmt.Printf("gocracker-sandboxd: listening on %s state=%s\n", *addr, *stateDir)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}
