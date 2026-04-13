package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/gocracker/gocracker/internal/vmmserver"
	"github.com/gocracker/gocracker/pkg/vmm"
)

type unixListener interface {
	Close()
	ListenUnix(string) error
}

var (
	newVMMServer = func(opts vmmserver.Options) unixListener {
		return vmmserver.NewWithOptions(opts)
	}
	notifySignals = signal.Notify
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("gocracker-vmm", flag.ContinueOnError)
	fs.SetOutput(stderr)

	socketPath := fs.String("socket", "/tmp/gocracker-vmm.sock", "Unix socket path to listen on")
	defaultBoot := fs.String("default-x86-boot", string(vmm.X86BootAuto), "default x86 boot mode: auto, acpi, legacy")
	vmID := fs.String("vm-id", "", "VM identifier used for worker-backed launches")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	srv := newVMMServer(vmmserver.Options{
		DefaultX86Boot: vmm.X86BootMode(*defaultBoot),
		VMID:           *vmID,
	})

	sigCh := make(chan os.Signal, 1)
	notifySignals(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		srv.Close()
	}()

	if err := srv.ListenUnix(*socketPath); err != nil {
		// Stop the signal goroutine so it doesn't leak when ListenUnix
		// fails immediately (e.g., in unit tests).
		signal.Stop(sigCh)
		close(sigCh)
		fmt.Fprintln(stderr, fmt.Errorf("listen unix socket %s: %w", *socketPath, err))
		return 1
	}
	return 0
}
