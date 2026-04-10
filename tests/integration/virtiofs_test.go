//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gocracker/gocracker/internal/compose"
	"github.com/gocracker/gocracker/pkg/container"
	"github.com/gocracker/gocracker/pkg/vmm"
)

func TestVirtioFSMountReflectsHostAndGuestWrites(t *testing.T) {
	kernel := requireVirtioFSKernel(t)

	sharedDir := filepath.Join(t.TempDir(), "shared")
	if err := os.MkdirAll(sharedDir, 0755); err != nil {
		t.Fatalf("mkdir shared: %v", err)
	}

	binaryPath := buildGuestProgram(t, `
package main
import (
	"os"
	"path/filepath"
	"strings"
	"time"
)
func main() {
	if err := os.WriteFile("/data/from-guest.txt", []byte("guest-ok\n"), 0644); err != nil {
		os.Exit(1)
	}
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile("/data/from-host.txt")
		if err == nil && strings.TrimSpace(string(data)) == "host-ok" {
			_ = os.WriteFile(filepath.Join("/data", "done.txt"), []byte("done\n"), 0644)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	os.Exit(2)
}
`)
	contextDir := t.TempDir()
	copyFileIntoContext(t, binaryPath, filepath.Join(contextDir, "guest"))
	if err := os.WriteFile(filepath.Join(contextDir, "Dockerfile"), []byte("FROM scratch\nCOPY guest /guest\nCMD [\"/guest\"]\n"), 0644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}

	result, err := container.Run(container.RunOptions{
		Dockerfile: filepath.Join(contextDir, "Dockerfile"),
		Context:    contextDir,
		KernelPath: kernel,
		MemMB:      256,
		X86Boot:    vmm.X86BootACPI,
		ConsoleOut: testConsoleWriter(t),
		// Use in-process VMM so stdio inherited by jailer subprocesses
		// doesn't confuse go test's post-exit I/O tracker. The worker path
		// is still exercised by TestComposeSharedRWVolumeViaVirtioFS.
		JailerMode: container.JailerModeOff,
		Mounts: []container.Mount{{
			Source:  sharedDir,
			Target:  "/data",
			Backend: container.MountBackendVirtioFS,
		}},
	})
	if err != nil {
		t.Fatalf("container.Run: %v", err)
	}
	defer result.VM.Stop()
	defer result.Close()

	hostFile := filepath.Join(sharedDir, "from-host.txt")
	if !waitForFile(filepath.Join(sharedDir, "from-guest.txt"), 12*time.Second) {
		t.Logf("guest console:\n%s", string(result.VM.ConsoleOutput()))
		t.Fatalf("guest did not write shared file")
	}
	if err := os.WriteFile(hostFile, []byte("host-ok\n"), 0644); err != nil {
		t.Fatalf("write host file: %v", err)
	}
	if !waitForFile(filepath.Join(sharedDir, "done.txt"), 12*time.Second) {
		t.Fatalf("guest did not observe host-side write")
	}
	if !waitForVMState(result.VM, vmm.StateStopped, 12*time.Second) {
		t.Fatalf("vm did not stop cleanly, state=%s", result.VM.State())
	}
}

func TestComposeSharedRWVolumeViaVirtioFS(t *testing.T) {
	requireRoot(t)
	kernel := requireVirtioFSKernel(t)
	baseDir := t.TempDir()
	sharedDir := filepath.Join(baseDir, "shared")
	if err := os.MkdirAll(sharedDir, 0755); err != nil {
		t.Fatalf("mkdir shared: %v", err)
	}

	writerA := buildGuestProgram(t, `
package main
import (
	"os"
	"strings"
	"time"
)
func main() {
	if err := os.WriteFile("/data/a.txt", []byte("service-a\n"), 0644); err != nil {
		os.Exit(1)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile("/data/b.txt")
		if err == nil && strings.TrimSpace(string(data)) == "service-b" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	os.Exit(2)
}
`)
	writerB := buildGuestProgram(t, `
package main
import (
	"os"
	"strings"
	"time"
)
func main() {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile("/data/a.txt")
		if err == nil && strings.TrimSpace(string(data)) == "service-a" {
			if err := os.WriteFile("/data/b.txt", []byte("service-b\n"), 0644); err != nil {
				os.Exit(1)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	os.Exit(2)
}
`)

	buildService := func(name string, binaryPath string) string {
		dir := filepath.Join(baseDir, name)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
		copyFileIntoContext(t, binaryPath, filepath.Join(dir, "guest"))
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY guest /guest\nCMD [\"/guest\"]\n"), 0644); err != nil {
			t.Fatalf("write Dockerfile %s: %v", name, err)
		}
		return dir
	}

	buildService("service-a", writerA)
	buildService("service-b", writerB)
	composeFile := filepath.Join(baseDir, "docker-compose.yml")
	composeData := strings.Join([]string{
		"services:",
		"  service-a:",
		"    build:",
		"      context: ./service-a",
		"    volumes:",
		"      - ./shared:/data",
		"  service-b:",
		"    build:",
		"      context: ./service-b",
		"    volumes:",
		"      - ./shared:/data",
		"",
	}, "\n")
	if err := os.WriteFile(composeFile, []byte(composeData), 0644); err != nil {
		t.Fatalf("write compose file: %v", err)
	}

	stack, err := compose.Up(compose.RunOptions{
		ComposePath: composeFile,
		ContextDir:  baseDir,
		KernelPath:  kernel,
		DefaultMem:  256,
		X86Boot:     vmm.X86BootACPI,
		JailerMode:  container.JailerModeOff,
	})
	if err != nil {
		t.Fatalf("compose.Up: %v", err)
	}
	defer stack.Down()

	if !waitForFile(filepath.Join(sharedDir, "a.txt"), 15*time.Second) {
		t.Fatalf("service-a did not write shared file")
	}
	if !waitForFile(filepath.Join(sharedDir, "b.txt"), 15*time.Second) {
		t.Fatalf("service-b did not observe and respond through shared volume")
	}
}

func requireVirtioFSKernel(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("KVM not available; skipping virtio-fs integration test")
	}
	if _, err := os.Stat("/usr/libexec/virtiofsd"); err != nil {
		if _, err := os.Stat("/usr/lib/qemu/virtiofsd"); err != nil {
			t.Skip("virtiofsd not installed; skipping virtio-fs integration test")
		}
	}
	for _, envName := range []string{"GOCRACKER_VIRTIOFS_KERNEL", "GOCRACKER_KERNEL"} {
		if kernel := strings.TrimSpace(os.Getenv(envName)); kernel != "" {
			f, err := os.Open(kernel)
			if err != nil {
				t.Skipf("%s=%s is not readable in this environment: %v", envName, kernel, err)
			}
			_ = f.Close()
			return kernel
		}
	}
	moduleMatches, _ := filepath.Glob(filepath.Join("/lib/modules", strings.TrimSpace(readFile(t, "/proc/sys/kernel/osrelease")), "kernel", "fs", "fuse", "virtiofs.ko*"))
	if len(moduleMatches) == 0 {
		t.Skip("virtiofs kernel module not available on host; skipping virtio-fs integration test")
	}
	kernel := "/boot/vmlinuz-" + strings.TrimSpace(readFile(t, "/proc/sys/kernel/osrelease"))
	if _, err := os.Stat(kernel); err != nil {
		t.Skipf("host kernel image %s not available; skipping virtio-fs integration test", kernel)
	}
	f, err := os.Open(kernel)
	if err != nil {
		t.Skipf("host kernel image %s is not readable in this environment: %v", kernel, err)
	}
	_ = f.Close()
	return kernel
}

func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("root is required for compose virtio-fs integration tests")
	}
}

func buildGuestProgram(t *testing.T, source string) string {
	t.Helper()
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "main.go")
	outPath := filepath.Join(dir, "guest")
	if err := os.WriteFile(srcPath, []byte(strings.TrimSpace(source)+"\n"), 0644); err != nil {
		t.Fatalf("write guest source: %v", err)
	}
	cmd := exec.Command("go", "build", "-o", outPath, srcPath)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build guest program: %v\n%s", err, output)
	}
	return outPath
}

func testConsoleWriter(t *testing.T) *os.File {
	t.Helper()
	if testing.Verbose() {
		return os.Stdout
	}
	return nil
}

func copyFileIntoContext(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0755); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

func waitForFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	_, err := os.Stat(path)
	return err == nil
}

func waitForVMState(vm vmm.Handle, state vmm.State, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if vm.State() == state {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return vm.State() == state
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
