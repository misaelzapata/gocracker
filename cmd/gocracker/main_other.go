//go:build !linux

// gocracker on non-Linux is a stub binary until the WHP-backed run loop
// inside pkg/vmm.VM is wired up (Phase 1.2 step 7-8: machineArchBackend
// takes HVVCPU/HVVM, then we drop the legacy *kvm.System fields and
// un-gate cmd/gocracker on Windows). Today the main CLI imports
// pkg/vmm.VM, which still embeds kvm types — both Linux-only.
//
// On Windows the working alternative TODAY is gocracker-whp.exe, which
// bypasses the CLI-style API entirely and drives a single Linux kernel
// directly through the WHP backend:
//
//	gocracker-whp.exe -initrd initramfs.cpio.gz vmlinux
//	gocracker-whp.exe -rootfs rootfs.ext4 vmlinux
package main

import (
	"fmt"
	"os"
	"runtime"
)

func main() {
	fmt.Fprintln(os.Stderr, "gocracker: the CLI surface is still Linux-only on "+runtime.GOOS+"/"+runtime.GOARCH+".")
	fmt.Fprintln(os.Stderr, "")
	if runtime.GOOS == "windows" {
		fmt.Fprintln(os.Stderr, "On Windows you can already boot a Linux kernel via the WHP backend:")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "  gocracker-whp.exe -initrd initramfs.cpio.gz vmlinux")
		fmt.Fprintln(os.Stderr, "  gocracker-whp.exe -rootfs rootfs.ext4 vmlinux")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "(requires Windows Hypervisor Platform — enable with")
		fmt.Fprintln(os.Stderr, "Enable-WindowsOptionalFeature -Online -FeatureName HypervisorPlatform -All)")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "The full gocracker.exe is pending Phase 1.2 of the port — see the plan.")
	} else {
		fmt.Fprintln(os.Stderr, "Native "+runtime.GOOS+" support is not yet planned.")
		fmt.Fprintln(os.Stderr, "Run gocracker inside a Linux VM (KVM is the supported hypervisor).")
	}
	os.Exit(2)
}
