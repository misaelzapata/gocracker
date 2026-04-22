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
	vmmBinary := fs.String("vmm-binary", "", "Path to gocracker binary used to spawn VMM workers (default: sibling of this binary, else PATH lookup)")
	jailerBinary := fs.String("jailer-binary", "", "Path to gocracker binary used as jailer launcher (default: same as -vmm-binary)")
	fs.Parse(args)

	if err := os.MkdirAll(*stateDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "mkdir state-dir:", err)
		os.Exit(1)
	}

	resolvedVMM := resolveWorkerBinary(*vmmBinary)
	resolvedJailer := *jailerBinary
	if resolvedJailer == "" {
		resolvedJailer = resolvedVMM
	}

	store, err := sandboxd.NewStore(filepath.Join(*stateDir, "store.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "open store:", err)
		os.Exit(1)
	}
	mgr := &sandboxd.Manager{
		Store:        store,
		StateDir:     *stateDir,
		VMMBinary:    resolvedVMM,
		JailerBinary: resolvedJailer,
	}
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
		// Drain registered pools BEFORE process exit so their
		// refiller goroutines terminate cleanly and their paused
		// VMs (KVM children of this process) don't orphan. Without
		// this, Ctrl-C leaves N qemu-like worker processes running
		// per pool slot until the OS reaper picks them up.
		mgr.Shutdown(shutCtx)
	}()

	fmt.Printf("gocracker-sandboxd: listening on %s state=%s vmm=%s jailer=%s\n",
		*addr, *stateDir, resolvedVMM, resolvedJailer)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}

// resolveWorkerBinary picks the gocracker binary used to spawn VMM
// workers. Precedence: explicit flag > sibling "gocracker" next to
// this executable > "gocracker" resolved via $PATH. We never fall
// back to os.Executable() itself (this binary is sandboxd — it has
// no "worker" / "jailer" / "build-worker" subcommands, so letting
// internal/worker.resolveLauncher default to it produces confusing
// failures like "usage: gocracker-sandboxd serve [flags]" in the
// worker stderr on every spawn).
func resolveWorkerBinary(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if self, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(self), "gocracker")
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate
		}
	}
	return "gocracker"
}
