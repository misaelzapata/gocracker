//go:build windows

package vmm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gocracker/gocracker/internal/whp"
)

// TestBootLinuxOnWHP_RealKernel is the Phase 2e milestone: actually boot
// a real Linux kernel on Windows via WHP and capture early-printk output.
// The kernel will likely panic later (no virtio root disk, no /init), but
// reaching kernel printk over the serial port proves:
//
//   - bzImage / vmlinux loader works
//   - Long-mode page tables + GDT + IDT setup is correct
//   - boot_params + cmdline are at the right addresses
//   - vCPU register configuration (CR0/CR3/CR4/EFER, segments, RIP) is right
//   - WHvRunVirtualProcessor returns IOPort exits with the right context
//   - UART (0x3F8) dispatch + RIP advance works
//   - Linux kernel's earliest printk reaches the host
//
// On a Win11 host with the Hypervisor Platform feature enabled, this
// test should produce output like "Linux version ..." within ~1 second.
// If it doesn't print anything, the bug is in one of the pieces above.
func TestBootLinuxOnWHP_RealKernel(t *testing.T) {
	if !whp.Available() {
		t.Skip("WinHvPlatform.dll not loadable")
	}
	present, err := whp.HypervisorPresent()
	if err != nil || !present {
		t.Skip("Hypervisor Platform feature not enabled")
	}

	// Locate the shipped x86_64 vmlinux. The repo ships
	// artifacts/kernels/gocracker-guest-standard-vmlinux* — the
	// uncompressed one is what loader.LoadKernel wants.
	kernel := findShippedKernel(t)
	if kernel == "" {
		t.Skip("no x86_64 kernel artifact available")
	}

	// Collect UART output with a mutex so the test goroutine and the
	// vCPU goroutine don't race on the slice header.
	var (
		mu     sync.Mutex
		output []byte
	)
	onByte := func(b byte) {
		mu.Lock()
		output = append(output, b)
		mu.Unlock()
	}

	cfg := WHPBootConfig{
		KernelPath:   kernel,
		Cmdline:      "console=ttyS0 earlyprintk=ttyS0 reboot=k panic=1 nomodule",
		MemoryBytes:  128 * 1024 * 1024,
		VCPUs:        1,
		OnUARTOutput: onByte,
	}

	session, err := BootLinuxOnWHP(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BootLinuxOnWHP: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	// Run the vCPU for up to 3 seconds. The kernel will either print
	// early output and panic on missing /init, or hang at some
	// intermediate setup. Either way, 3 seconds is plenty for any
	// observable signal at the printk level.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- session.Run(ctx) }()

	select {
	case err := <-runDone:
		mu.Lock()
		captured := string(output)
		mu.Unlock()
		t.Logf("Run returned: %v", err)
		t.Logf("captured %d bytes of UART output:\n%s", len(captured), captured)
		// We don't require successful boot — just SOME kernel printk
		// activity to prove the pipeline works.
		if len(captured) == 0 {
			t.Fatal("vCPU exited but produced ZERO UART bytes — boot pipeline is silent")
		}
		if !looksLikeKernelOutput(captured) {
			t.Errorf("UART output doesn't contain expected kernel boot strings; got: %q", captured)
		}

	case <-ctx.Done():
		_ = session.Close()
		<-runDone // wait for goroutine to drain
		mu.Lock()
		captured := string(output)
		mu.Unlock()
		t.Logf("timeout reached; captured %d UART bytes:\n%s", len(captured), captured)
		if len(captured) == 0 {
			t.Fatal("3s timeout and ZERO UART bytes — boot pipeline is silent")
		}
		if !looksLikeKernelOutput(captured) {
			t.Errorf("UART output doesn't contain expected kernel strings; got: %q", captured)
		}
	}
}

// findShippedKernel returns the path to an x86_64 vmlinux that ships
// with the repo, or "" if none is available. The artifacts/ directory
// is a sibling of pkg/vmm; we walk up two directories to find the repo
// root and look for the standard x86 kernel.
func findShippedKernel(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	repoRoot := wd
	for i := 0; i < 5; i++ {
		repoRoot = filepath.Dir(repoRoot)
		if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err == nil {
			break
		}
	}
	candidates := []string{
		"artifacts/kernels/gocracker-guest-standard-vmlinux",
		"artifacts/kernels/gocracker-guest-virtiofs-vmlinux",
	}
	for _, c := range candidates {
		path := filepath.Join(repoRoot, c)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

// looksLikeKernelOutput reports whether s contains any of the strings
// the Linux kernel prints very early in boot. Match a generous set so
// different kernel versions and configurations all qualify.
func looksLikeKernelOutput(s string) bool {
	markers := []string{
		"Linux version",
		"Command line:",
		"BIOS-provided",
		"Linux",
		"kernel:",
		"e820",
	}
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}
