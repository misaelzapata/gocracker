//go:build darwin

// darwin-smoke boots a Linux VM on macOS using Apple Virtualization.framework.
// It builds a disk image from an OCI image using the gocracker container pipeline,
// then boots it with the vz backend.
//
// Usage:
//   go build -o darwin-smoke ./cmd/darwin-smoke/
//   codesign --entitlements entitlements.local.plist -s - ./darwin-smoke
//   ./darwin-smoke                           # uses alpine:3.20
//   IMAGE=ubuntu:24.04 ./darwin-smoke        # custom image
//   KERNEL=/path/to/vmlinuz ./darwin-smoke   # custom kernel
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/gocracker/gocracker/pkg/container"
)

func main() {
	image := os.Getenv("IMAGE")
	if image == "" {
		image = "alpine:3.20"
	}
	kernel := os.Getenv("KERNEL")
	if kernel == "" {
		candidates := []string{
			"artifacts/kernels/gocracker-guest-standard-arm64-Image",
			"artifacts/kernels/gocracker-guest-standard-vmlinux",
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				kernel = c
				break
			}
		}
	}
	if kernel == "" {
		fmt.Fprintln(os.Stderr, "error: no kernel. Set KERNEL env or decompress artifacts/kernels/*.gz")
		os.Exit(1)
	}

	fmt.Printf("=== gocracker darwin smoke test ===\n")
	fmt.Printf("Host:   %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Printf("Image:  %s\n", image)
	fmt.Printf("Kernel: %s\n\n", kernel)

	result, err := container.Run(container.RunOptions{
		Image:       image,
		MemMB:       256,
		CPUs:        2,
		KernelPath:  kernel,
		NetworkMode: "auto",
		Cmd:         []string{"sh", "-c", "echo '=== Hello from gocracker on macOS! ===' && uname -a && cat /proc/cpuinfo | head -5 && free -m 2>/dev/null; echo '=== VM done ===' && poweroff -f 2>/dev/null; halt -f 2>/dev/null; reboot -f"},
		ConsoleOut:  os.Stdout,
		ConsoleIn:   os.Stdin,
		DiskSizeMB:  512,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "container.Run: %v\n", err)
		os.Exit(1)
	}
	defer result.Close()

	fmt.Printf("VM created: %s\n", result.ID)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\nReceived signal, stopping VM...")
		result.VM.Stop()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := result.VM.WaitStopped(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "WaitStopped: %v\n", err)
		result.VM.Stop()
	}

	fmt.Printf("\n=== VM stopped. Uptime: %s ===\n", result.VM.Uptime().Round(time.Millisecond))
}
