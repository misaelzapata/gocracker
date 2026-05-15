//go:build windows

// gocracker on Windows is a slim wrapper around the Windows Hypervisor
// Platform (WHP) backend. The `run` subcommand boots a Linux kernel +
// optional initrd/rootfs directly through pkg/vmm.BootLinuxOnWHP and
// streams the guest's serial console to host stdout, with host stdin
// piped into the guest's COM1 RX.
//
// The Linux build (cmd/gocracker/main.go) carries the full
// jailer/seccomp/hostnet/OCI/REST surface; that stack is Linux-only and
// has no Windows analogue today, so the Windows build only mirrors the
// subset that maps cleanly onto WHP. Subcommands that don't fit yet
// (serve, build, compose, snapshot, restore, migrate) print a clear
// "not yet supported on Windows" message and exit 2 — the port roadmap
// in CHANGELOG.md tracks when they land.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gocracker/gocracker/internal/buildinfo"
	"github.com/gocracker/gocracker/internal/whp"
	"github.com/gocracker/gocracker/pkg/vmm"
)

const usage = `gocracker — lightweight microVM runtime (Windows / WHP build)

Usage:
  gocracker <command> [flags]

Commands:
  run        Boot a Linux kernel on the Windows Hypervisor Platform (WHP)
  version    Print build version, commit, date, and go runtime
  help       Print this usage

Examples:
  # Boot a kernel + initramfs straight to a busybox shell:
  gocracker.exe run --kernel vmlinux --initrd initramfs.cpio.gz --mem 256

  # Boot a kernel with an ext4 rootfs (mounted on /dev/vda):
  gocracker.exe run --kernel vmlinux --rootfs rootfs.ext4 --mem 512

Notes on the Windows build:
  * The Linux jailer/seccomp/hostnet/OCI/build/compose/snapshot/migrate
    surface is Linux-only and has no WHP analogue today.
  * On Windows, ` + "`gocracker run`" + ` currently requires an explicit
    --kernel plus --initrd and/or --rootfs. OCI image auto-fetch
    (--image, --dockerfile) lands in a follow-up Sprint.
  * WHP must be enabled on the host: in an admin PowerShell, run
    Enable-WindowsOptionalFeature -Online -FeatureName HypervisorPlatform -All
    and reboot.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(1)
	}
	switch os.Args[1] {
	case "run":
		cmdRun(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println(buildinfo.String())
	case "help", "-h", "--help":
		fmt.Print(usage)
	// Deferred subcommands — Linux-only today, will be ported in later
	// sprints. We surface them explicitly so the user sees a clear
	// message rather than "unknown command".
	case "repo", "compose", "up", "build", "snapshot", "restore",
		"migrate", "serve", "server", "vmm", "build-worker", "jailer":
		notSupportedOnWindows(os.Args[1])
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		fmt.Print(usage)
		os.Exit(1)
	}
}

// notSupportedOnWindows prints a friendly "deferred" message and exits 2.
// Used for every Linux-only subcommand the user may try on Windows today
// — keeps the error surface predictable while the port catches up.
func notSupportedOnWindows(sub string) {
	fmt.Fprintf(os.Stderr,
		"gocracker: subcommand %q is not yet supported on Windows; see CHANGELOG.md for the port roadmap.\n",
		sub)
	os.Exit(2)
}

// fatal prints msg to stderr and exits with status 1. Mirrors the helper
// of the same name in main.go (Linux build) so future ports of run-flag
// validation can drop in cleanly.
func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "gocracker: "+msg)
	os.Exit(1)
}

// cmdRun parses `gocracker run` flags and boots a single Linux kernel
// on WHP. The flag set deliberately mirrors a subset of the Linux build's
// flags — only the fields that map cleanly onto vmm.WHPBootConfig.
//
// Required: --kernel, plus at least one of --initrd / --rootfs (without
// either, a real Linux kernel panics within milliseconds for lack of
// root). The optional positional IMAGE argument is reserved for the OCI
// auto-fetch path; on Windows that path doesn't exist yet, so passing it
// without --kernel/--initrd/--rootfs is a hard error today.
func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	kernel := fs.String("kernel", "", "Kernel image path (bzImage or vmlinux)")
	initrd := fs.String("initrd", "", "Optional initramfs / initrd path (CPIO archive)")
	rootfs := fs.String("rootfs", "", "Optional ext4 rootfs path, mounted as /dev/vda via virtio-blk-mmio")
	rootfsReadOnly := fs.Bool("rootfs-ro", false, "Open the rootfs read-only (sets VIRTIO_BLK_F_RO)")
	cmdline := fs.String("cmdline", "console=ttyS0 earlyprintk=ttyS0 reboot=k panic=1",
		"Kernel command line. virtio-mmio device parameters are appended automatically.")
	memMB := fs.Uint64("mem", 256, "Guest RAM in MiB (minimum 64)")
	vcpus := fs.Int("vcpus", 1, "vCPU count (Phase 2e supports 1)")
	cpusAlias := fs.Int("cpus", 0, "Alias for --vcpus (Linux-build compat)")
	timeout := fs.Duration("timeout", 0,
		"Max wall time before killing the guest; 0 disables (Ctrl-C still cancels)")
	// Linux-only flags we accept-and-ignore on Windows so a shared
	// invocation script can run on both platforms without flag-parse
	// failures. Each one prints a short warning if used.
	image := fs.String("image", "", "(Linux only on this build) OCI image ref")
	dockerfile := fs.String("dockerfile", "", "(Linux only on this build) Dockerfile path")
	netMode := fs.String("net", "", "(Linux only on this build) network mode: none/auto/slirp")
	tap := fs.String("tap", "", "(Linux only on this build) TAP interface name")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: gocracker run [flags] [IMAGE]\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		// flag.ExitOnError already exited; this is defensive.
		os.Exit(2)
	}

	// Accept-and-warn for Linux-only flags. We don't exit because some
	// scripts pass both --image and --kernel; in that case --kernel wins
	// on Windows and --image is informational.
	for name, val := range map[string]string{
		"--image":      *image,
		"--dockerfile": *dockerfile,
		"--net":        *netMode,
		"--tap":        *tap,
	} {
		if val != "" {
			fmt.Fprintf(os.Stderr,
				"gocracker: warning: %s is Linux-only on this build and will be ignored.\n", name)
		}
	}

	// --cpus / --vcpus parity: prefer --vcpus, fall back to --cpus.
	effVCPUs := *vcpus
	if *cpusAlias > 0 && *vcpus == 1 {
		effVCPUs = *cpusAlias
	}

	// Validate inputs. The Windows path has no OCI fetcher today, so we
	// can't synthesise a kernel + rootfs from `gocracker run alpine`
	// alone. The user must hand us at least a kernel + (initrd or
	// rootfs).
	positional := fs.Args()
	if *kernel == "" {
		if len(positional) > 0 {
			fatal("Windows currently requires explicit --kernel + --rootfs (or --initrd) — " +
				"OCI auto-fetch lands in a follow-up Sprint. See CHANGELOG.md for the roadmap.")
		}
		fatal("--kernel is required")
	}
	if *initrd == "" && *rootfs == "" {
		fatal("--initrd or --rootfs is required (a kernel with no root panics within ms)")
	}
	if *memMB < 64 {
		fatal(fmt.Sprintf("--mem must be at least 64 MiB; got %d", *memMB))
	}
	if effVCPUs != 1 {
		fatal(fmt.Sprintf("--vcpus must be 1 on Windows today (Phase 2e); got %d", effVCPUs))
	}
	if *kernel != "" {
		if _, err := os.Stat(*kernel); err != nil {
			fatal(fmt.Sprintf("--kernel %q: %v", *kernel, err))
		}
	}
	if *initrd != "" {
		if _, err := os.Stat(*initrd); err != nil {
			fatal(fmt.Sprintf("--initrd %q: %v", *initrd, err))
		}
	}
	if *rootfs != "" {
		if _, err := os.Stat(*rootfs); err != nil {
			fatal(fmt.Sprintf("--rootfs %q: %v", *rootfs, err))
		}
	}

	// Fail fast on hosts without WHP, with the same actionable message
	// gocracker-whp prints. The boot path would otherwise fail later
	// with a confusing HRESULT deep inside the partition setup.
	if !whp.Available() {
		fmt.Fprintln(os.Stderr,
			"gocracker: ERROR: WinHvPlatform.dll not loadable. This host does not expose the Windows Hypervisor Platform.")
		os.Exit(3)
	}
	present, err := whp.HypervisorPresent()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gocracker: ERROR: WHvGetCapability(HypervisorPresent) failed: %v\n", err)
		os.Exit(3)
	}
	if !present {
		fmt.Fprintln(os.Stderr, "gocracker: ERROR: Hypervisor Platform feature is not enabled on this host.")
		fmt.Fprintln(os.Stderr, "Enable it with (admin PowerShell, then reboot):")
		fmt.Fprintln(os.Stderr, "  Enable-WindowsOptionalFeature -Online -FeatureName HypervisorPlatform -All")
		os.Exit(3)
	}

	cfg := vmm.WHPBootConfig{
		KernelPath:     *kernel,
		Cmdline:        *cmdline,
		MemoryBytes:    *memMB * 1024 * 1024,
		VCPUs:          effVCPUs,
		InitrdPath:     *initrd,
		RootfsPath:     *rootfs,
		RootfsReadOnly: *rootfsReadOnly,
		OnUARTOutput:   func(b byte) { os.Stdout.Write([]byte{b}) },
	}

	session, err := vmm.BootLinuxOnWHP(context.Background(), cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gocracker: BootLinuxOnWHP: %v\n", err)
		os.Exit(1)
	}
	defer session.Close()

	// Build the cancellation context. Timeout is optional — 0 means
	// "run until the guest halts or Ctrl-C", which matches the Linux
	// build's behaviour when --wait is implicit.
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)
	if *timeout > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), *timeout)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}
	defer cancel()

	// Ctrl-C cleanly cancels the vCPU loop. Without this, on Windows
	// SIGINT would kill the process while the WHP partition is still
	// mapped — leaving guest RAM stranded until the kernel reaps the
	// process. signal.Notify on Interrupt+SIGTERM matches gocracker-whp.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sigCh:
			fmt.Fprintln(os.Stderr, "\ngocracker: interrupt received; halting guest")
			cancel()
		case <-ctx.Done():
		}
	}()

	// Bridge host stdin to the guest's COM1 RX. Same pattern as
	// gocracker-whp: one byte at a time, guarded by the session's stop
	// channel so PushUARTInput is safe to call even if Close races us.
	go func() {
		buf := make([]byte, 1)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				return
			}
			if n > 0 {
				session.PushUARTInput(buf[0])
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	}()

	tmoStr := "no timeout"
	if *timeout > 0 {
		tmoStr = timeout.String()
	}
	fmt.Fprintf(os.Stderr, "gocracker: booting %s (%d MiB, %s)\n", *kernel, *memMB, tmoStr)
	if err := session.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "\ngocracker: vCPU exit error: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "\ngocracker: guest halted cleanly")
}

