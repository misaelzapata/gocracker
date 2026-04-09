package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gocracker/gocracker/internal/guest"
	"github.com/gocracker/gocracker/internal/runtimecfg"
	"github.com/gocracker/gocracker/pkg/vmm"
)

func main() {
	spec := runtimecfg.GuestSpec{
		Process: runtimecfg.Process{Exec: "/bin/sh"},
		Env:     []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		WorkDir: "/",
	}
	parts := runtimecfg.DefaultKernelArgs(true)
	parts = append(parts, "rw", "root=/dev/vda", "rootfstype=ext4")
	parts = spec.AppendKernelArgs(parts)

	initrdPath := "/tmp/debugvm-initrd.img"
	if err := guest.BuildInitrd(initrdPath, nil); err != nil {
		fmt.Fprintf(os.Stderr, "BuildInitrd: %v\n", err)
		os.Exit(1)
	}

	vm, err := vmm.New(vmm.Config{
		ID:         "debugvm",
		MemMB:      128,
		KernelPath: debugKernelPath(),
		InitrdPath: initrdPath,
		DiskImage:  "/tmp/gocracker-smoke-alpine-routing/disk.ext4",
		Cmdline:    strings.Join(parts, " "),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "vmm.New: %v\n", err)
		os.Exit(1)
	}
	if err := vm.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "vm.Start: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("debugvm running; Ctrl-C to stop")
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sigCh:
	case <-time.After(40 * time.Second):
	}
	vm.Stop()
	time.Sleep(500 * time.Millisecond)
}

func debugKernelPath() string {
	if kernel := strings.TrimSpace(os.Getenv("GOCRACKER_DEBUG_KERNEL")); kernel != "" {
		return kernel
	}
	if _, err := os.Stat("artifacts/kernels/gocracker-guest-standard-vmlinux"); err == nil {
		return "artifacts/kernels/gocracker-guest-standard-vmlinux"
	}
	if _, err := os.Stat("artifacts/kernels/host-current-vmlinuz"); err == nil {
		return "artifacts/kernels/host-current-vmlinuz"
	}
	release, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err == nil {
		candidate := filepath.Join("/boot", "vmlinuz-"+strings.TrimSpace(string(release)))
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return "/boot/vmlinuz"
}
