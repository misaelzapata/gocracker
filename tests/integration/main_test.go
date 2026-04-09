//go:build integration

package integration

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"testing"

	"github.com/gocracker/gocracker/internal/jailer"
	"github.com/gocracker/gocracker/internal/vmmserver"
	"github.com/gocracker/gocracker/pkg/vmm"
)

// TestMain dispatches the worker/jailer subcommands when this binary is
// re-exec'd by `worker.LaunchVMM`. Without this, the test binary has no
// dispatcher and re-exec recurses into the test runner.
//
// `worker.LaunchVMM` calls `os.Executable()` when no explicit binary path is
// provided and prepends "vmm" or "jailer" to the args. We mirror the same
// flag/option layout as cmd/gocracker/main.go's cmdVMM and cmdJailer.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "vmm":
			runVMMSubcommand(os.Args[2:])
			return
		case "jailer":
			runJailerSubcommand(os.Args[2:])
			return
		}
	}
	// Tests use container.JailerMode=off, which runs the VMM in-process.
	// The vCPU seccomp profile (pkg/vmm/vmm.go runLoop) is installed
	// per-thread without TSYNC and persists on the OS thread even after
	// runtime.UnlockOSThread, so when Go reuses that thread for an
	// unrelated goroutine the next syscall outside the vCPU allow-list
	// crashes the test process with SIGSYS. In production this is fine
	// because workers run as separate processes; in tests we just turn
	// it off.
	if os.Getenv("GOCRACKER_SECCOMP") == "" {
		_ = os.Setenv("GOCRACKER_SECCOMP", "off")
	}
	os.Exit(m.Run())
}

func runVMMSubcommand(args []string) {
	fs := flag.NewFlagSet("vmm", flag.ExitOnError)
	socketPath := fs.String("socket", "/tmp/gocracker-vmm.sock", "Unix socket path")
	defaultBoot := fs.String("default-x86-boot", string(vmm.X86BootAuto), "default x86 boot mode")
	vmID := fs.String("vm-id", "", "VM identifier")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	srv := vmmserver.NewWithOptions(vmmserver.Options{
		DefaultX86Boot: vmm.X86BootMode(*defaultBoot),
		VMID:           *vmID,
	})

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		srv.Close()
		os.Exit(0)
	}()

	if err := srv.ListenUnix(*socketPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runJailerSubcommand(args []string) {
	if err := jailer.RunCLI(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
