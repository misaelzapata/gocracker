//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
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
		DiskSizeMB: 256,
		CacheDir:   filepath.Join(t.TempDir(), "cache"),
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
		DiskSizeMB: 256,
		CacheDir:   filepath.Join(t.TempDir(), "cache"),
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

func TestPauseResumeRoundTrip(t *testing.T) {
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

	// Wait for the VM to be fully running before pausing.
	if !waitForVMState(vm, vmm.StateRunning, 5*time.Second) {
		t.Fatalf("VM never reached running state, got %s", vm.State())
	}

	// Give vCPUs time to enter the KVM run loop so pause can interrupt them.
	time.Sleep(200 * time.Millisecond)

	if err := vm.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if s := vm.State(); s != vmm.StatePaused {
		t.Fatalf("expected paused after Pause(), got %s", s)
	}

	if err := vm.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if s := vm.State(); s != vmm.StateRunning {
		t.Fatalf("expected running after Resume(), got %s", s)
	}

	vm.Stop()
	time.Sleep(100 * time.Millisecond)
	if s := vm.State(); s != vmm.StateStopped {
		t.Fatalf("expected stopped after Stop(), got %s", s)
	}
}

func TestConsoleOutputCapture(t *testing.T) {
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

	// The kernel should print "Linux version" early in boot on the serial console.
	if !waitForSerial(&serial, 12*time.Second, "Linux version") {
		t.Fatalf("kernel boot message not observed in serial output:\n%s", serial.String())
	}

	// Also verify ConsoleOutput() returns the same data.
	consoleBytes := vm.ConsoleOutput()
	if len(consoleBytes) == 0 {
		t.Fatal("ConsoleOutput() returned empty buffer")
	}
	if !strings.Contains(string(consoleBytes), "Linux version") {
		t.Fatalf("ConsoleOutput() does not contain kernel boot messages:\n%s", string(consoleBytes))
	}
}

func TestDeviceList(t *testing.T) {
	kernel := requireIntegrationKernel(t)

	vm, err := vmm.New(vmm.Config{
		MemMB:      128,
		KernelPath: kernel,
		Cmdline:    "console=ttyS0 reboot=k panic=1 nomodule i8042.noaux i8042.nomux i8042.dumbkbd swiotlb=noforce",
	})
	if err != nil {
		t.Fatalf("vmm.New: %v", err)
	}
	defer vm.Stop()

	if err := vm.Start(); err != nil {
		t.Fatalf("vm.Start: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	devices := vm.DeviceList()
	if len(devices) == 0 {
		t.Fatal("DeviceList() returned no devices")
	}

	// At minimum there should be a virtio-rng device (always attached).
	foundRNG := false
	for _, d := range devices {
		if d.Type == "rng" || d.Type == "virtio-rng" {
			foundRNG = true
			break
		}
	}
	if !foundRNG {
		t.Logf("devices: %+v", devices)
		t.Fatal("DeviceList() did not include an RNG device")
	}
}

func TestVMEvents(t *testing.T) {
	kernel := requireIntegrationKernel(t)

	vm, err := vmm.New(vmm.Config{
		MemMB:      128,
		KernelPath: kernel,
		Cmdline:    "console=ttyS0 reboot=k panic=1 nomodule i8042.noaux i8042.nomux i8042.dumbkbd swiotlb=noforce",
	})
	if err != nil {
		t.Fatalf("vmm.New: %v", err)
	}
	defer vm.Stop()

	if err := vm.Start(); err != nil {
		t.Fatalf("vm.Start: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	events := vm.Events().Events(time.Time{})
	if len(events) == 0 {
		t.Fatal("EventLog has no events after boot")
	}

	hasType := func(typ vmm.EventType) bool {
		for _, e := range events {
			if e.Type == typ {
				return true
			}
		}
		return false
	}
	if !hasType(vmm.EventCreated) {
		t.Error("missing 'created' event")
	}
	if !hasType(vmm.EventRunning) {
		t.Error("missing 'running' event")
	}

	vm.Stop()
	// Wait for stopped events to be emitted.
	time.Sleep(200 * time.Millisecond)

	events = vm.Events().Events(time.Time{})
	if !hasType(vmm.EventStopped) {
		types := make([]string, len(events))
		for i, e := range events {
			types[i] = string(e.Type)
		}
		t.Fatalf("missing 'stopped' event; got types: %v", types)
	}
}

func TestMultipleVMsConcurrent(t *testing.T) {
	kernel := requireIntegrationKernel(t)

	const count = 3
	type result struct {
		idx int
		vm  *vmm.VM
		err error
	}

	results := make(chan result, count)
	for i := 0; i < count; i++ {
		go func(idx int) {
			vm, err := vmm.New(vmm.Config{
				MemMB:      128,
				KernelPath: kernel,
				Cmdline:    "console=ttyS0 reboot=k panic=1 nomodule i8042.noaux i8042.nomux i8042.dumbkbd swiotlb=noforce",
			})
			if err != nil {
				results <- result{idx: idx, err: err}
				return
			}
			if err := vm.Start(); err != nil {
				results <- result{idx: idx, err: err}
				return
			}
			results <- result{idx: idx, vm: vm}
		}(i)
	}

	var vms []*vmm.VM
	for i := 0; i < count; i++ {
		r := <-results
		if r.err != nil {
			t.Fatalf("VM[%d] failed: %v", r.idx, r.err)
		}
		vms = append(vms, r.vm)
	}
	defer func() {
		for _, vm := range vms {
			vm.Stop()
		}
	}()

	time.Sleep(200 * time.Millisecond)
	for i, vm := range vms {
		s := vm.State()
		if s != vmm.StateRunning && s != vmm.StateStopped {
			t.Errorf("VM[%d] state = %s, want running or stopped", i, s)
		}
	}
}

func TestStopIdempotent(t *testing.T) {
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
	time.Sleep(100 * time.Millisecond)

	// Call Stop() twice — should not panic.
	vm.Stop()
	time.Sleep(100 * time.Millisecond)
	vm.Stop() // second call must be safe
	time.Sleep(100 * time.Millisecond)

	if s := vm.State(); s != vmm.StateStopped {
		t.Errorf("expected stopped, got %s", s)
	}
}

func TestBootWithDisk(t *testing.T) {
	kernel := requireIntegrationKernel(t)

	contextDir := t.TempDir()
	binaryPath := buildGuestProgram(t, `
package main
import (
	"fmt"
	"os"
)
func main() {
	// Check if /dev/vda exists (virtio block device).
	if _, err := os.Stat("/dev/vda"); err == nil {
		fmt.Println("disk-vda-present")
	} else {
		fmt.Println("disk-vda-missing")
	}
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
		DiskSizeMB: 256,
		CacheDir:   filepath.Join(t.TempDir(), "cache"),
		ConsoleOut: &serial,
		JailerMode: container.JailerModeOff,
	})
	if err != nil {
		t.Fatalf("container.Run: %v", err)
	}
	defer result.Close()
	defer result.VM.Stop()

	// container.Run always creates a disk image for the rootfs.
	if !waitForSerial(&serial, 12*time.Second, "disk-vda-present") {
		t.Fatalf("guest did not find /dev/vda:\n%s", serial.String())
	}
}

func TestContainerRunWithEnv(t *testing.T) {
	kernel := requireIntegrationKernel(t)

	contextDir := t.TempDir()
	binaryPath := buildGuestProgram(t, `
package main
import (
	"fmt"
	"os"
)
func main() {
	fmt.Printf("MY_VAR=%s\n", os.Getenv("MY_VAR"))
	fmt.Printf("ANOTHER=%s\n", os.Getenv("ANOTHER"))
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
		DiskSizeMB: 256,
		CacheDir:   filepath.Join(t.TempDir(), "cache"),
		Env:        []string{"MY_VAR=hello", "ANOTHER=world"},
		ConsoleOut: &serial,
		JailerMode: container.JailerModeOff,
	})
	if err != nil {
		t.Fatalf("container.Run: %v", err)
	}
	defer result.Close()
	defer result.VM.Stop()

	if !waitForSerial(&serial, 12*time.Second, "MY_VAR=hello") {
		t.Fatalf("env var MY_VAR not found in guest output:\n%s", serial.String())
	}
	if !waitForSerial(&serial, 2*time.Second, "ANOTHER=world") {
		t.Fatalf("env var ANOTHER not found in guest output:\n%s", serial.String())
	}
}

func TestContainerRunWithWorkdir(t *testing.T) {
	kernel := requireIntegrationKernel(t)

	contextDir := t.TempDir()
	binaryPath := buildGuestProgram(t, `
package main
import (
	"fmt"
	"os"
)
func main() {
	dir, err := os.Getwd()
	if err != nil {
		fmt.Printf("cwd-error=%v\n", err)
		return
	}
	fmt.Printf("cwd=%s\n", dir)
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
		DiskSizeMB: 256,
		CacheDir:   filepath.Join(t.TempDir(), "cache"),
		WorkDir:    "/tmp",
		ConsoleOut: &serial,
		JailerMode: container.JailerModeOff,
	})
	if err != nil {
		t.Fatalf("container.Run: %v", err)
	}
	defer result.Close()
	defer result.VM.Stop()

	if !waitForSerial(&serial, 12*time.Second, "cwd=/tmp") {
		t.Fatalf("guest did not start in /tmp:\n%s", serial.String())
	}
}

func TestSnapshotContainsExpectedFiles(t *testing.T) {
	kernel := requireIntegrationKernel(t)

	vm, err := vmm.New(vmm.Config{
		MemMB:      128,
		KernelPath: kernel,
		Cmdline:    "console=ttyS0 reboot=k panic=1 nomodule i8042.noaux i8042.nomux i8042.dumbkbd swiotlb=noforce",
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

	// Verify snapshot.json exists.
	snapJSON := filepath.Join(snapDir, "snapshot.json")
	if _, err := os.Stat(snapJSON); err != nil {
		t.Fatalf("snapshot.json not found: %v", err)
	}

	// Verify mem.bin exists.
	memBin := filepath.Join(snapDir, "mem.bin")
	if _, err := os.Stat(memBin); err != nil {
		t.Fatalf("mem.bin not found: %v", err)
	}

	// mem.bin should have non-zero size (128MB of guest memory).
	info, err := os.Stat(memBin)
	if err != nil {
		t.Fatalf("stat mem.bin: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("mem.bin is empty")
	}
}

func TestSnapshotJSONRoundTrip(t *testing.T) {
	kernel := requireIntegrationKernel(t)

	vm, err := vmm.New(vmm.Config{
		MemMB:      128,
		KernelPath: kernel,
		Cmdline:    "console=ttyS0 reboot=k panic=1 nomodule i8042.noaux i8042.nomux i8042.dumbkbd swiotlb=noforce",
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
	snap, err := vm.TakeSnapshotWithOptions(snapDir, vmm.SnapshotOptions{Resume: false})
	if err != nil {
		t.Fatalf("TakeSnapshotWithOptions: %v", err)
	}

	// Read and decode snapshot.json
	data, err := os.ReadFile(filepath.Join(snapDir, "snapshot.json"))
	if err != nil {
		t.Fatalf("read snapshot.json: %v", err)
	}

	var decoded vmm.Snapshot
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal snapshot.json: %v", err)
	}

	// Verify key fields match the original snapshot.
	if decoded.Config.MemMB != 128 {
		t.Errorf("decoded MemMB = %d, want 128", decoded.Config.MemMB)
	}
	if decoded.Config.KernelPath != kernel {
		t.Errorf("decoded KernelPath = %q, want %q", decoded.Config.KernelPath, kernel)
	}
	if decoded.Version != snap.Version {
		t.Errorf("decoded Version = %d, want %d", decoded.Version, snap.Version)
	}
	if decoded.ID == "" {
		t.Error("decoded snapshot ID is empty")
	}
}

func TestBootWithVirtioRNG(t *testing.T) {
	kernel := requireIntegrationKernel(t)

	contextDir := t.TempDir()
	binaryPath := buildGuestProgram(t, `
package main
import (
	"fmt"
	"os"
)
func main() {
	if _, err := os.Stat("/dev/hwrng"); err == nil {
		fmt.Println("hwrng-present")
	} else {
		fmt.Println("hwrng-missing")
	}
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
		DiskSizeMB: 256,
		CacheDir:   filepath.Join(t.TempDir(), "cache"),
		ConsoleOut: &serial,
		JailerMode: container.JailerModeOff,
	})
	if err != nil {
		t.Fatalf("container.Run: %v", err)
	}
	defer result.Close()
	defer result.VM.Stop()

	if !waitForSerial(&serial, 12*time.Second, "hwrng-present") {
		t.Fatalf("guest did not find /dev/hwrng:\n%s", serial.String())
	}
}

func TestUARTState(t *testing.T) {
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

	// Wait for some output to ensure UART is in use.
	if !waitForSerial(&serial, 12*time.Second, "Linux version") {
		t.Fatalf("no serial output:\n%s", serial.String())
	}

	// ConsoleOutput returns buffered UART bytes; verify it is non-empty.
	buf := vm.ConsoleOutput()
	if len(buf) == 0 {
		t.Fatal("ConsoleOutput() returned empty after kernel boot")
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

// requireKVMClockCtrl skips the test if the host KVM does not support
// kvmclock control MSRs. Without kvmclock, snapshot/restore can corrupt
// guest memory mappings causing SIGSEGV in the Go runtime (the vCPU
// accesses memory that the host unmapped during restore).
func requireKVMClockCtrl(t *testing.T) {
	t.Helper()
	kernel := requireIntegrationKernel(t)

	// Quick probe: create a tiny VM, snapshot it, restore it. If the
	// restore logs a kvmclock warning, this host can't safely run
	// migration tests.
	vm, err := vmm.New(vmm.Config{
		MemMB:      64,
		VCPUs:      1,
		KernelPath: kernel,
	})
	if err != nil {
		t.Skipf("cannot probe kvmclock: vmm.New: %v", err)
	}
	if err := vm.Start(); err != nil {
		vm.Stop()
		t.Skipf("cannot probe kvmclock: vm.Start: %v", err)
	}
	snapDir := t.TempDir()
	_, err = vm.TakeSnapshot(snapDir)
	vm.Stop()
	if err != nil {
		t.Skipf("cannot probe kvmclock: snapshot: %v", err)
	}
	restored, err := vmm.RestoreFromSnapshotWithOptions(snapDir, vmm.RestoreOptions{})
	if err != nil {
		t.Skipf("kvmclock restore unsupported on this host: %v", err)
	}
	restored.Stop()
}
