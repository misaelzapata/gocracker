//go:build windows

// gocracker-whp boots a Linux kernel on the Windows Hypervisor Platform
// (WHP) and prints the kernel's serial-console output to stdout. This is
// the user-facing entry point for the Phase 2e+ WHP backend before the
// full gocracker.exe is wired up.
//
// Usage:
//
//	gocracker-whp [flags] <kernel-path>
//
// The kernel binary may be a bzImage (gzipped or uncompressed) or an
// ELF vmlinux. Boot output streams to stdout as the kernel produces it.
//
// First-boot recipes:
//
//	# Boot a kernel with an initramfs straight to a busybox shell:
//	gocracker-whp -initrd initramfs.cpio.gz vmlinux
//
//	# Boot a kernel with a virtio-blk rootfs (ext4) — /dev/vda is the
//	# block device, kernel mounts root=/dev/vda automatically:
//	gocracker-whp -rootfs rootfs.ext4 vmlinux
//
// The full subsystem-init log up to userspace handover is the proof
// that WHP, long-mode setup, page tables, GDT/IDT, port I/O dispatch,
// PIT, PIC, UART, the MMIO emulator, and virtio-blk all work end-to-end.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gocracker/gocracker/internal/whp"
	"github.com/gocracker/gocracker/pkg/vmm"
)

func main() {
	memMB := flag.Int("mem", 128, "guest RAM in MiB")
	cmdline := flag.String("cmdline", "console=ttyS0 earlyprintk=ttyS0 reboot=k panic=1 nomodule tsc_early_khz=2400000 tsc=reliable lpj=10000000 no_timer_check",
		"kernel command line. tsc_early_khz=N tells the kernel the TSC frequency in kHz up front — skips the PIT-based calibration loop that fails against our software PIT. Adjust to your host CPU's actual TSC rate if precise time matters in the guest.")
	initrdPath := flag.String("initrd", "", "optional initramfs / initrd path (CPIO archive)")
	rootfsPath := flag.String("rootfs", "", "optional ext4 rootfs to attach as /dev/vda via virtio-blk-mmio")
	rootfsReadOnly := flag.Bool("rootfs-ro", false, "open the rootfs read-only (sets VIRTIO_BLK_F_RO)")
	timeout := flag.Duration("timeout", 30*time.Second, "max wall time before killing the guest")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags] <kernel-path>\n\nFlags:\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	kernelPath := flag.Arg(0)

	// Fail fast with a clear message if WHP isn't available, instead of
	// the partition lifecycle failing later with a confusing HRESULT.
	if !whp.Available() {
		fmt.Fprintln(os.Stderr, "ERROR: WinHvPlatform.dll not loadable. This host does not expose the Windows Hypervisor Platform.")
		os.Exit(3)
	}
	present, err := whp.HypervisorPresent()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: WHvGetCapability(HypervisorPresent) failed: %v\n", err)
		os.Exit(3)
	}
	if !present {
		fmt.Fprintln(os.Stderr, "ERROR: Hypervisor Platform feature is not enabled on this host.")
		fmt.Fprintln(os.Stderr, "Enable it with (admin PowerShell, then reboot):")
		fmt.Fprintln(os.Stderr, "  Enable-WindowsOptionalFeature -Online -FeatureName HypervisorPlatform -All")
		os.Exit(3)
	}

	cfg := vmm.WHPBootConfig{
		KernelPath:     kernelPath,
		Cmdline:        *cmdline,
		MemoryBytes:    uint64(*memMB) * 1024 * 1024,
		VCPUs:          1,
		InitrdPath:     *initrdPath,
		RootfsPath:     *rootfsPath,
		RootfsReadOnly: *rootfsReadOnly,
		OnUARTOutput:   func(b byte) { os.Stdout.Write([]byte{b}) },
	}

	session, err := vmm.BootLinuxOnWHP(context.Background(), cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "BootLinuxOnWHP: %v\n", err)
		os.Exit(1)
	}
	defer session.Close()

	// Ctrl-C cleanly cancels the vCPU instead of leaving WHP state
	// stranded.
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	// Bridge host stdin to the guest's COM1 RX. Runs until ctx is
	// cancelled or stdin closes; bytes the user types appear at the
	// guest's serial console. `defer session.Close()` may run while
	// the goroutine still has a read pending — PushUARTInput is
	// stop-channel guarded so it's safe to keep firing on a closed
	// session.
	stdinDone := make(chan struct{})
	go func() {
		defer close(stdinDone)
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

	fmt.Fprintf(os.Stderr, "gocracker-whp: booting %s (%d MiB RAM, %s)\n", kernelPath, *memMB, *timeout)
	if err := session.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "\ngocracker-whp: vCPU exit error: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "\ngocracker-whp: guest halted cleanly")
}
