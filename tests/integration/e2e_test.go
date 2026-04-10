//go:build integration

package integration

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gocracker/gocracker/pkg/container"
	"github.com/gocracker/gocracker/pkg/vmm"
)

// TestBootAndHalt boots a VM with a real kernel and verifies it reaches
// the running state, then stops it. Requires:
//   - KVM access (/dev/kvm readable)
//   - Root or CAP_SYS_ADMIN
//   - GOCRACKER_KERNEL env var pointing to a bzImage or vmlinux
func TestBootAndHalt(t *testing.T) {
	kernel := requireIntegrationKernel(t)

	vm, err := vmm.New(vmm.Config{
		MemMB:      128,
		KernelPath: kernel,
		Cmdline:    "console=ttyS0 reboot=k panic=1 nomodule i8042.noaux i8042.nomux i8042.dumbkbd swiotlb=noforce",
	})
	if err != nil {
		t.Fatalf("vmm.New: %v", err)
	}

	if err := vm.Start(); err != nil {
		t.Fatalf("vm.Start: %v", err)
	}

	// Give the VM a moment to start running
	time.Sleep(100 * time.Millisecond)
	if s := vm.State(); s != vmm.StateRunning && s != vmm.StateStopped {
		t.Errorf("expected running or stopped, got %s", s)
	}

	vm.Stop()
	// Allow run loop to exit
	time.Sleep(100 * time.Millisecond)
	if s := vm.State(); s != vmm.StateStopped {
		t.Errorf("expected stopped after Stop(), got %s", s)
	}
}

func TestBootAndHaltACPI(t *testing.T) {
	kernel := requireACPIKernel(t)

	vm, err := vmm.New(vmm.Config{
		MemMB:      128,
		KernelPath: kernel,
		Cmdline:    "console=ttyS0 reboot=k panic=1 nomodule i8042.noaux i8042.nomux i8042.dumbkbd swiotlb=noforce",
		X86Boot:    vmm.X86BootACPI,
	})
	if err != nil {
		t.Fatalf("vmm.New: %v", err)
	}

	if err := vm.Start(); err != nil {
		t.Fatalf("vm.Start: %v", err)
	}

	time.Sleep(250 * time.Millisecond)
	if s := vm.State(); s != vmm.StateRunning && s != vmm.StateStopped {
		t.Errorf("expected running or stopped, got %s", s)
	}

	vm.Stop()
	time.Sleep(100 * time.Millisecond)
	if s := vm.State(); s != vmm.StateStopped {
		t.Errorf("expected stopped after Stop(), got %s", s)
	}
}

func TestBootAndHaltSMP(t *testing.T) {
	kernel := requireIntegrationKernel(t)

	for _, cpus := range []int{2, 4} {
		t.Run("cpus_"+strconv.Itoa(cpus), func(t *testing.T) {
			var serial lockedBuffer
			vm, err := vmm.New(vmm.Config{
				MemMB:      256,
				KernelPath: kernel,
				Cmdline:    "console=ttyS0 reboot=k panic=1 nomodule i8042.noaux i8042.nomux i8042.dumbkbd swiotlb=noforce",
				VCPUs:      cpus,
				ConsoleOut: &serial,
			})
			if err != nil {
				t.Fatalf("vmm.New: %v", err)
			}
			defer vm.Stop()

			if err := vm.Start(); err != nil {
				t.Fatalf("vm.Start: %v", err)
			}

			want := "Total of " + strconv.Itoa(cpus) + " processors activated"
			if !waitForSerial(&serial, 12*time.Second, want) {
				t.Fatalf("did not observe %q in guest serial log:\n%s", want, serial.String())
			}

			vm.Stop()
			time.Sleep(100 * time.Millisecond)
			if s := vm.State(); s != vmm.StateStopped {
				t.Errorf("expected stopped after Stop(), got %s", s)
			}
		})
	}
}

func TestContainerRunACPIDiscoversVirtioBlockWithoutLegacyCmdline(t *testing.T) {
	kernel := requireACPIKernel(t)

	contextDir := t.TempDir()
	binaryPath := buildGuestProgram(t, `
package main
import "fmt"
func main() { fmt.Println("acpi-disk-ok") }
`)
	copyFileIntoContext(t, binaryPath, filepath.Join(contextDir, "guest"))
	if err := os.WriteFile(filepath.Join(contextDir, "Dockerfile"), []byte("FROM scratch\nCOPY guest /guest\nCMD [\"/guest\"]\n"), 0644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}

	var serial lockedBuffer
	result, err := container.Run(container.RunOptions{
		Dockerfile: filepath.Join(contextDir, "Dockerfile"),
		Context:    contextDir,
		KernelPath: kernel,
		MemMB:      256,
		X86Boot:    vmm.X86BootACPI,
		ConsoleOut: &serial,
		JailerMode: container.JailerModeOff,
	})
	if err != nil {
		t.Fatalf("container.Run: %v", err)
	}
	defer result.Close()
	defer result.VM.Stop()

	if !waitForSerial(&serial, 12*time.Second, "acpi-disk-ok") {
		t.Fatalf("guest did not boot container workload through pure ACPI path:\n%s", serial.String())
	}
	if !waitForVMState(result.VM, vmm.StateStopped, 12*time.Second) {
		t.Fatalf("vm did not stop cleanly, state=%s\nserial:\n%s", result.VM.State(), serial.String())
	}
}

func TestContainerRunWorkloadBecomesPID1(t *testing.T) {
	kernel := requireIntegrationKernel(t)

	contextDir := t.TempDir()
	binaryPath := buildGuestProgram(t, `
package main
import (
	"fmt"
	"os"
)
func main() {
	fmt.Printf("guest-pid=%d guest-ppid=%d\n", os.Getpid(), os.Getppid())
}
`)
	copyFileIntoContext(t, binaryPath, filepath.Join(contextDir, "guest"))
	if err := os.WriteFile(filepath.Join(contextDir, "Dockerfile"), []byte("FROM scratch\nCOPY guest /guest\nCMD [\"/guest\"]\n"), 0644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}

	var serial lockedBuffer
	result, err := container.Run(container.RunOptions{
		Dockerfile: filepath.Join(contextDir, "Dockerfile"),
		Context:    contextDir,
		KernelPath: kernel,
		MemMB:      256,
		ConsoleOut: &serial,
		JailerMode: container.JailerModeOff,
	})
	if err != nil {
		t.Fatalf("container.Run: %v", err)
	}
	defer result.Close()
	defer result.VM.Stop()

	if !waitForSerial(&serial, 12*time.Second, "guest-pid=1") {
		t.Fatalf("guest workload did not become PID 1:\n%s", serial.String())
	}
	if !waitForVMState(result.VM, vmm.StateStopped, 12*time.Second) {
		t.Fatalf("vm did not stop after PID 1 workload exit, state=%s\nserial:\n%s", result.VM.State(), serial.String())
	}
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	kernel := requireIntegrationKernel(t)

	var serial lockedBuffer
	vm, err := vmm.New(vmm.Config{
		MemMB:      128,
		KernelPath: kernel,
		Cmdline:    "console=ttyS0 reboot=k panic=1 nomodule i8042.noaux i8042.nomux i8042.dumbkbd swiotlb=noforce",
		ConsoleOut: &serial,
	})
	if err != nil {
		t.Fatalf("vmm.New: %v", err)
	}
	defer vm.Stop()

	if err := vm.Start(); err != nil {
		t.Fatalf("vm.Start: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	snapDir := filepath.Join(t.TempDir(), "snapshot")
	if _, err := vm.TakeSnapshotWithOptions(snapDir, vmm.SnapshotOptions{Resume: false}); err != nil {
		t.Fatalf("TakeSnapshotWithOptions: %v", err)
	}
	if got := vm.State(); got != vmm.StatePaused {
		t.Fatalf("source VM state after snapshot = %s, want paused", got)
	}

	restored, err := vmm.RestoreFromSnapshotWithOptions(snapDir, vmm.RestoreOptions{
		ConsoleOut: &serial,
	})
	if err != nil {
		t.Fatalf("RestoreFromSnapshotWithOptions: %v", err)
	}
	defer restored.Stop()

	if err := restored.Start(); err != nil {
		t.Fatalf("restored.Start: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := restored.State(); got != vmm.StateRunning && got != vmm.StateStopped {
		t.Fatalf("restored state = %s, want running or stopped", got)
	}
}

func requireIntegrationKernel(t *testing.T) string {
	t.Helper()
	kernel := os.Getenv("GOCRACKER_KERNEL")
	if kernel == "" {
		t.Skip("GOCRACKER_KERNEL not set; skipping integration test")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("KVM not available; skipping integration test")
	}
	resolved, err := resolveIntegrationPath(repoRoot(t), kernel)
	if err != nil {
		t.Fatalf("resolve GOCRACKER_KERNEL: %v", err)
	}
	return resolved
}

func requireACPIKernel(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("KVM not available; skipping ACPI integration test")
	}
	for _, envName := range []string{"GOCRACKER_ACPI_KERNEL", "GOCRACKER_VIRTIOFS_KERNEL"} {
		if kernel := os.Getenv(envName); kernel != "" {
			resolved, err := resolveIntegrationPath(repoRoot(t), kernel)
			if err != nil {
				t.Fatalf("resolve %s: %v", envName, err)
			}
			return resolved
		}
	}
	t.Skip("GOCRACKER_ACPI_KERNEL or GOCRACKER_VIRTIOFS_KERNEL not set; skipping ACPI integration test")
	return ""
}

func resolveIntegrationPath(baseDir, value string) (string, error) {
	if filepath.IsAbs(value) {
		return value, nil
	}
	return filepath.Abs(filepath.Join(baseDir, value))
}

func waitForSerial(buf *lockedBuffer, timeout time.Duration, want string) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), want) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return strings.Contains(buf.String(), want)
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
