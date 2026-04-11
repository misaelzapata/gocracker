//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gocracker/gocracker/pkg/container"
	"github.com/gocracker/gocracker/pkg/vmm"
)

func TestContainerRunFromOCIImage(t *testing.T) {
	kernel := requireIntegrationKernel(t)

	// Build a guest binary that just prints a marker and exits.
	// We package it in a Dockerfile built from scratch because
	// pulling alpine:3.20 requires network access and a registry.
	// This test verifies the container.Run Dockerfile path end-to-end.
	contextDir := t.TempDir()
	binaryPath := buildGuestProgram(t, `
package main
import "fmt"
func main() { fmt.Println("oci-container-ok") }
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

	if !waitForSerial(&serial, 12*time.Second, "oci-container-ok") {
		t.Fatalf("guest workload did not produce expected output:\n%s", serial.String())
	}
	if !waitForVMState(result.VM, vmm.StateStopped, 12*time.Second) {
		t.Fatalf("vm did not stop after workload, state=%s\nserial:\n%s", result.VM.State(), serial.String())
	}
}

func TestContainerRunFromDockerfile(t *testing.T) {
	kernel := requireIntegrationKernel(t)

	contextDir := t.TempDir()
	binaryPath := buildGuestProgram(t, `
package main
import "fmt"
func main() { fmt.Println("dockerfile-build-ok") }
`)
	copyFileIntoContext(t, binaryPath, filepath.Join(contextDir, "app"))

	// Multi-step Dockerfile: copies binary and sets entrypoint.
	dockerfile := `FROM scratch
COPY app /app
ENTRYPOINT ["/app"]
`
	if err := os.WriteFile(filepath.Join(contextDir, "Dockerfile"), []byte(dockerfile), 0644); err != nil {
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

	if !waitForSerial(&serial, 12*time.Second, "dockerfile-build-ok") {
		t.Fatalf("guest workload did not run from Dockerfile build:\n%s", serial.String())
	}
	if !waitForVMState(result.VM, vmm.StateStopped, 12*time.Second) {
		t.Fatalf("vm did not stop cleanly, state=%s\nserial:\n%s", result.VM.State(), serial.String())
	}
}

func TestContainerRunWithCustomCmdline(t *testing.T) {
	kernel := requireIntegrationKernel(t)

	contextDir := t.TempDir()
	// Guest reads /proc/cmdline and verifies it contains the standard
	// console= parameter that container.Run always includes.
	binaryPath := buildGuestProgram(t, `
package main
import (
	"fmt"
	"os"
	"strings"
)
func main() {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		fmt.Printf("cmdline-error=%v\n", err)
		return
	}
	cmdline := strings.TrimSpace(string(data))
	if strings.Contains(cmdline, "console=ttyS0") {
		fmt.Println("cmdline-console-ok")
	} else {
		fmt.Printf("cmdline-unexpected: %s\n", cmdline)
	}
	// Also check that init= is set (container.Run sets the workload as init).
	if strings.Contains(cmdline, "init=") {
		fmt.Println("cmdline-init-ok")
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
		ConsoleOut: &serial,
		JailerMode: container.JailerModeOff,
	})
	if err != nil {
		t.Fatalf("container.Run: %v", err)
	}
	defer result.Close()
	defer result.VM.Stop()

	if !waitForSerial(&serial, 12*time.Second, "cmdline-console-ok") {
		t.Fatalf("guest /proc/cmdline did not contain console=ttyS0:\n%s", serial.String())
	}
	if !waitForSerial(&serial, 2*time.Second, "cmdline-init-ok") {
		t.Fatalf("guest /proc/cmdline did not contain init=:\n%s", serial.String())
	}
}
