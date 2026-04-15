package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/gocracker/gocracker/internal/vmmserver"
	"github.com/gocracker/gocracker/pkg/vmm"
)

func init() {
	// The gocracker-vmm process is short-lived (one VM per lifetime) and
	// holds less than ~10 MiB of Go-managed heap even during boot. Disabling
	// GC trades that heap growth for a few hundred microseconds of scheduler
	// time on the critical boot path — worth it here because the process
	// exits on VM teardown. Users who want normal GC behaviour can override
	// with GOGC=100 in the env.
	debug.SetGCPercent(-1)

	// Pin the VMM's current working set into RAM. Without this, Linux can
	// swap out pages the VMM accesses rarely between boot and the next VM
	// event (seccomp filter, IRQ table, virtio queue descriptors); the
	// page-fault to bring them back shows up as a p95/p99 tail on cold boot.
	//
	// ONLY MCL_CURRENT — not MCL_FUTURE — because MCL_FUTURE would force
	// the kernel to eagerly fault in every subsequent mmap, including the
	// MAP_PRIVATE snapshot memory region. That turns the ~3 ms lazy-COW
	// restore into a ~30 ms eager-page-in (measured), defeating the point
	// of the COW fast path. Pages allocated after init stay lazy.
	//
	// Opt-outable via GOCRACKER_NO_MLOCK=1 for environments where
	// RLIMIT_MEMLOCK is tight and we prefer a warning to an outright
	// failure on startup.
	if os.Getenv("GOCRACKER_NO_MLOCK") != "1" {
		if err := unix.Mlockall(unix.MCL_CURRENT); err != nil {
			// Don't abort — small hosts with tight RLIMIT_MEMLOCK (common
			// in CI containers) would otherwise become unusable. The env
			// opt-out above exists for users who want to silence this too.
			fmt.Fprintf(os.Stderr, "gocracker-vmm: mlockall skipped (%v); p95 latency may regress\n", err)
		}
	}
}

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
