//go:build integration

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gocracker/gocracker/internal/guestexec"
	"github.com/gocracker/gocracker/pkg/container"
	"github.com/gocracker/gocracker/pkg/vmm"
)

const balloonWorkloadSource = `
package main

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "ping" {
		fmt.Println("balloon-worker-ok")
		return
	}
	buf := make([]byte, 384<<20)
	for i := 0; i < len(buf); i += 4096 {
		buf[i] = 0x7f
	}
	runtime.KeepAlive(buf)
	fmt.Println("balloon-allocated")
	time.Sleep(2 * time.Second)
	buf = nil
	runtime.GC()
	debug.FreeOSMemory()
	fmt.Println("balloon-freed")
	time.Sleep(40 * time.Second)
}
`

const hotplugWorkloadSource = `
package main

import (
	"fmt"
	"os"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "ping" {
		fmt.Println("hotplug-worker-ok")
		return
	}
	fmt.Println("hotplug-ready")
	time.Sleep(60 * time.Second)
}
`

func TestVirtioBalloonLocalStatsAndExec(t *testing.T) {
	if os.Getenv("GOCRACKER_SKIP_LARGE_MEM") == "1" {
		t.Skip("GOCRACKER_SKIP_LARGE_MEM=1: skipping 1024MB VM test")
	}
	requirePrivilegedExecIntegration(t)
	kernel := requireIntegrationKernel(t)

	contextDir := buildBalloonFixtureContext(t)

	var serial lockedBuffer
	result, err := container.Run(container.RunOptions{
		Dockerfile:  filepath.Join(contextDir, "Dockerfile"),
		Context:     contextDir,
		KernelPath:  kernel,
		MemMB:       1024,
		DiskSizeMB:  256,
		CacheDir:    filepath.Join(t.TempDir(), "cache"),
		ExecEnabled: true,
		Balloon: &vmm.BalloonConfig{
			AmountMiB:             0,
			DeflateOnOOM:          true,
			StatsPollingIntervalS: 1,
		},
		ConsoleOut: &serial,
		JailerMode: container.JailerModeOff,
	})
	if err != nil {
		t.Fatalf("container.Run: %v", err)
	}
	defer result.Close()
	defer result.VM.Stop()

	if !waitForSerial(&serial, 20*time.Second, "balloon-allocated") {
		t.Fatalf("guest did not allocate memory:\n%s", serial.String())
	}
	if !waitForSerial(&serial, 20*time.Second, "balloon-freed") {
		t.Fatalf("guest did not free memory:\n%s", serial.String())
	}

	controller, ok := result.VM.(vmm.BalloonController)
	if !ok {
		t.Fatal("VM does not implement BalloonController")
	}
	stats := waitForBalloonStats(t, controller, 15*time.Second)
	if stats.TotalMemory == 0 || stats.AvailableMemory == 0 {
		t.Fatalf("unexpected balloon stats: %#v", stats)
	}

	if err := controller.UpdateBalloon(vmm.BalloonUpdate{AmountMiB: 128}); err != nil {
		t.Fatalf("UpdateBalloon(): %v", err)
	}
	stats = waitForBalloonTarget(t, controller, 128, 15*time.Second)
	if stats.TargetMiB != 128 {
		t.Fatalf("target_mib = %d, want 128", stats.TargetMiB)
	}
	stats = waitForBalloonActualAtLeast(t, controller, 32, 20*time.Second)
	if stats.ActualMiB < 32 {
		t.Fatalf("actual_mib = %d, want at least 32", stats.ActualMiB)
	}

	resp := execGuestOneShot(t, result.VM, []string{"/balloon-worker", "ping"})
	if !strings.Contains(resp.Stdout, "balloon-worker-ok") {
		t.Fatalf("exec stdout = %q, want balloon-worker-ok", resp.Stdout)
	}
}

func TestVirtioBalloonWorkerReclaimsRSS(t *testing.T) {
	if os.Getenv("GOCRACKER_SKIP_LARGE_MEM") == "1" {
		t.Skip("GOCRACKER_SKIP_LARGE_MEM=1: skipping 1024MB VM test")
	}
	requirePrivilegedExecIntegration(t)
	kernel := requireIntegrationKernel(t)
	bins := repoWorkerBinaries(t)

	contextDir := buildBalloonFixtureContext(t)

	cacheDir := filepath.Join(t.TempDir(), "cache")
	var serial lockedBuffer
	result, err := container.Run(container.RunOptions{
		Dockerfile:   filepath.Join(contextDir, "Dockerfile"),
		Context:      contextDir,
		KernelPath:   kernel,
		MemMB:        1024,
		DiskSizeMB:   256,
		ExecEnabled:  true,
		CacheDir:     cacheDir,
		JailerMode:   container.JailerModeOn,
		JailerBinary: bins.jailer,
		VMMBinary:    bins.vmm,
		ConsoleOut:   &serial,
		Balloon: &vmm.BalloonConfig{
			AmountMiB:             0,
			DeflateOnOOM:          true,
			StatsPollingIntervalS: 1,
		},
	})
	if err != nil {
		t.Fatalf("container.Run: %v", err)
	}
	defer result.Close()
	defer result.VM.Stop()

	if _, ok := result.VM.(vmm.WorkerBacked); !ok {
		t.Fatal("VM is not worker-backed")
	}
	controller, ok := result.VM.(vmm.BalloonController)
	if !ok {
		t.Fatal("VM does not implement BalloonController")
	}
	stats := waitForBalloonStats(t, controller, 15*time.Second)
	if stats.AvailableMemory == 0 {
		t.Fatalf("balloon stats never populated: %#v", stats)
	}
	time.Sleep(4 * time.Second)

	pid := waitForProcessByVMID(t, result.VM.ID(), 10*time.Second)
	rssBefore := readRSSBytes(t, pid)
	if err := controller.UpdateBalloon(vmm.BalloonUpdate{AmountMiB: 256}); err != nil {
		t.Fatalf("UpdateBalloon(): %v", err)
	}
	stats = waitForBalloonActualAtLeast(t, controller, 64, 20*time.Second)
	rssAfter := waitForRSSBelow(t, pid, rssBefore-(64<<20), 20*time.Second)
	if rssAfter >= rssBefore {
		t.Fatalf("worker RSS did not drop: before=%d after=%d stats=%#v", rssBefore, rssAfter, stats)
	}

	resp := execGuestOneShot(t, result.VM, []string{"/balloon-worker", "ping"})
	if !strings.Contains(resp.Stdout, "balloon-worker-ok") {
		t.Fatalf("exec stdout = %q, want balloon-worker-ok", resp.Stdout)
	}
}

func TestMemoryHotplugLocalGrowShrink(t *testing.T) {
	if os.Getenv("GOCRACKER_SKIP_LARGE_MEM") == "1" {
		t.Skip("GOCRACKER_SKIP_LARGE_MEM=1: skipping 512MB+ VM test on this host")
	}
	requirePrivilegedExecIntegration(t)
	kernel := requireIntegrationKernel(t)

	contextDir := buildHotplugFixtureContext(t)

	var serial lockedBuffer
	result, err := container.Run(container.RunOptions{
		Dockerfile:  filepath.Join(contextDir, "Dockerfile"),
		Context:     contextDir,
		KernelPath:  kernel,
		MemMB:       512,
		DiskSizeMB:  256,
		CacheDir:    filepath.Join(t.TempDir(), "cache"),
		ExecEnabled: true,
		MemoryHotplug: &vmm.MemoryHotplugConfig{
			TotalSizeMiB: 256,
			SlotSizeMiB:  256,
			BlockSizeMiB: 128,
		},
		ConsoleOut: &serial,
		JailerMode: container.JailerModeOff,
	})
	if err != nil {
		t.Fatalf("container.Run: %v", err)
	}
	defer result.Close()
	defer result.VM.Stop()

	if !waitForSerial(&serial, 20*time.Second, "hotplug-ready") {
		t.Fatalf("guest did not reach hotplug workload:\n%s", serial.String())
	}

	controller, ok := result.VM.(vmm.MemoryHotplugController)
	if !ok {
		t.Fatal("VM does not implement MemoryHotplugController")
	}
	status, err := controller.GetMemoryHotplug()
	if err != nil {
		t.Fatalf("GetMemoryHotplug(): %v", err)
	}
	if status.PluggedSizeMiB != 0 {
		t.Fatalf("initial plugged_size_mib = %d, want 0", status.PluggedSizeMiB)
	}

	statsBefore := guestMemoryStats(t, result.VM)
	if err := controller.UpdateMemoryHotplug(vmm.MemoryHotplugSizeUpdate{RequestedSizeMiB: 128}); err != nil {
		t.Fatalf("UpdateMemoryHotplug(grow): %v", err)
	}
	status = waitForMemoryHotplugPlugged(t, controller, 128, 30*time.Second)
	statsAfterGrow := waitForGuestMemTotalAtLeast(t, result.VM, statsBefore.TotalMemory+(96<<20), 30*time.Second)
	if status.PluggedSizeMiB != 128 {
		t.Fatalf("grown plugged_size_mib = %d, want 128", status.PluggedSizeMiB)
	}
	if statsAfterGrow.TotalMemory <= statsBefore.TotalMemory {
		t.Fatalf("guest MemTotal did not grow: before=%d after=%d", statsBefore.TotalMemory, statsAfterGrow.TotalMemory)
	}

	if err := controller.UpdateMemoryHotplug(vmm.MemoryHotplugSizeUpdate{RequestedSizeMiB: 0}); err != nil {
		t.Fatalf("UpdateMemoryHotplug(shrink): %v", err)
	}
	status = waitForMemoryHotplugPlugged(t, controller, 0, 45*time.Second)
	statsAfterShrink := waitForGuestMemTotalAtMost(t, result.VM, statsBefore.TotalMemory+(32<<20), 45*time.Second)
	if status.PluggedSizeMiB != 0 {
		t.Fatalf("shrunk plugged_size_mib = %d, want 0", status.PluggedSizeMiB)
	}
	if statsAfterShrink.TotalMemory > statsBefore.TotalMemory+(32<<20) {
		t.Fatalf("guest MemTotal did not shrink close to baseline: before=%d after=%d", statsBefore.TotalMemory, statsAfterShrink.TotalMemory)
	}

	resp := execGuestOneShot(t, result.VM, []string{"/hotplug-worker", "ping"})
	if !strings.Contains(resp.Stdout, "hotplug-worker-ok") {
		t.Fatalf("exec stdout = %q, want hotplug-worker-ok", resp.Stdout)
	}
}

func TestMemoryHotplugWorkerGrowShrink(t *testing.T) {
	if os.Getenv("GOCRACKER_SKIP_LARGE_MEM") == "1" {
		t.Skip("GOCRACKER_SKIP_LARGE_MEM=1: skipping 512MB VM test")
	}
	requirePrivilegedExecIntegration(t)
	kernel := requireIntegrationKernel(t)
	bins := repoWorkerBinaries(t)

	contextDir := buildHotplugFixtureContext(t)

	cacheDir := filepath.Join(t.TempDir(), "cache")
	var serial lockedBuffer
	result, err := container.Run(container.RunOptions{
		Dockerfile:   filepath.Join(contextDir, "Dockerfile"),
		Context:      contextDir,
		KernelPath:   kernel,
		MemMB:        512,
		DiskSizeMB:   256,
		ExecEnabled:  true,
		CacheDir:     cacheDir,
		JailerMode:   container.JailerModeOn,
		JailerBinary: bins.jailer,
		VMMBinary:    bins.vmm,
		ConsoleOut:   &serial,
		MemoryHotplug: &vmm.MemoryHotplugConfig{
			TotalSizeMiB: 256,
			SlotSizeMiB:  256,
			BlockSizeMiB: 128,
		},
	})
	if err != nil {
		t.Fatalf("container.Run: %v", err)
	}
	defer result.Close()
	defer result.VM.Stop()

	if _, ok := result.VM.(vmm.WorkerBacked); !ok {
		t.Fatal("VM is not worker-backed")
	}
	if !waitForGuestExecContains(result.VM, []string{"/hotplug-worker", "ping"}, "hotplug-worker-ok", 20*time.Second) {
		t.Fatalf("guest did not become exec-ready:\n%s", serial.String())
	}

	controller, ok := result.VM.(vmm.MemoryHotplugController)
	if !ok {
		t.Fatal("VM does not implement MemoryHotplugController")
	}
	status, err := controller.GetMemoryHotplug()
	if err != nil {
		t.Fatalf("GetMemoryHotplug(): %v", err)
	}
	if status.PluggedSizeMiB != 0 {
		t.Fatalf("initial plugged_size_mib = %d, want 0", status.PluggedSizeMiB)
	}

	statsBefore := guestMemoryStats(t, result.VM)
	if err := controller.UpdateMemoryHotplug(vmm.MemoryHotplugSizeUpdate{RequestedSizeMiB: 128}); err != nil {
		t.Fatalf("UpdateMemoryHotplug(grow): %v", err)
	}
	status = waitForMemoryHotplugPlugged(t, controller, 128, 30*time.Second)
	statsAfterGrow := waitForGuestMemTotalAtLeast(t, result.VM, statsBefore.TotalMemory+(96<<20), 30*time.Second)
	if status.PluggedSizeMiB != 128 {
		t.Fatalf("grown plugged_size_mib = %d, want 128", status.PluggedSizeMiB)
	}
	if statsAfterGrow.TotalMemory <= statsBefore.TotalMemory {
		t.Fatalf("guest MemTotal did not grow: before=%d after=%d", statsBefore.TotalMemory, statsAfterGrow.TotalMemory)
	}

	if err := controller.UpdateMemoryHotplug(vmm.MemoryHotplugSizeUpdate{RequestedSizeMiB: 0}); err != nil {
		t.Fatalf("UpdateMemoryHotplug(shrink): %v", err)
	}
	status = waitForMemoryHotplugPlugged(t, controller, 0, 45*time.Second)
	statsAfterShrink := waitForGuestMemTotalAtMost(t, result.VM, statsBefore.TotalMemory+(32<<20), 45*time.Second)
	if status.PluggedSizeMiB != 0 {
		t.Fatalf("shrunk plugged_size_mib = %d, want 0", status.PluggedSizeMiB)
	}
	if statsAfterShrink.TotalMemory > statsBefore.TotalMemory+(32<<20) {
		t.Fatalf("guest MemTotal did not shrink close to baseline: before=%d after=%d", statsBefore.TotalMemory, statsAfterShrink.TotalMemory)
	}

	resp := execGuestOneShot(t, result.VM, []string{"/hotplug-worker", "ping"})
	if !strings.Contains(resp.Stdout, "hotplug-worker-ok") {
		t.Fatalf("exec stdout = %q, want hotplug-worker-ok", resp.Stdout)
	}
}

func execGuestOneShot(t *testing.T, vm vmm.Handle, command []string) guestexec.Response {
	t.Helper()
	dialer, ok := vm.(vmm.VsockDialer)
	if !ok {
		t.Fatal("VM does not implement VsockDialer")
	}
	conn, err := dialer.DialVsock(guestexec.DefaultVsockPort)
	if err != nil {
		t.Fatalf("DialVsock(): %v", err)
	}
	defer conn.Close()
	if err := guestexec.Encode(conn, guestexec.Request{Mode: guestexec.ModeExec, Command: command}); err != nil {
		t.Fatalf("encode guestexec request: %v", err)
	}
	var resp guestexec.Response
	if err := guestexec.Decode(conn, &resp); err != nil {
		t.Fatalf("decode guestexec response: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exec exit_code = %d stderr=%q stdout=%q", resp.ExitCode, resp.Stderr, resp.Stdout)
	}
	return resp
}

func waitForGuestExecContains(vm vmm.Handle, command []string, want string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		dialer, ok := vm.(vmm.VsockDialer)
		if !ok {
			return false
		}
		conn, err := dialer.DialVsock(guestexec.DefaultVsockPort)
		if err != nil {
			time.Sleep(250 * time.Millisecond)
			continue
		}
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		if err := guestexec.Encode(conn, guestexec.Request{Mode: guestexec.ModeExec, Command: command}); err != nil {
			_ = conn.Close()
			time.Sleep(250 * time.Millisecond)
			continue
		}
		var resp guestexec.Response
		err = guestexec.Decode(conn, &resp)
		_ = conn.Close()
		if err == nil && resp.ExitCode == 0 && strings.Contains(resp.Stdout, want) {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

func guestMemoryStats(t *testing.T, vm vmm.Handle) guestexec.MemoryStats {
	t.Helper()
	dialer, ok := vm.(vmm.VsockDialer)
	if !ok {
		t.Fatal("VM does not implement VsockDialer")
	}
	conn, err := dialer.DialVsock(guestexec.DefaultVsockPort)
	if err != nil {
		t.Fatalf("DialVsock(): %v", err)
	}
	defer conn.Close()
	if err := guestexec.Encode(conn, guestexec.Request{Mode: guestexec.ModeMemoryStats}); err != nil {
		t.Fatalf("encode memory stats request: %v", err)
	}
	var resp guestexec.Response
	if err := guestexec.Decode(conn, &resp); err != nil {
		t.Fatalf("decode memory stats response: %v", err)
	}
	if resp.MemoryStats == nil {
		t.Fatalf("memory stats missing in response: %#v", resp)
	}
	return *resp.MemoryStats
}

func waitForBalloonStats(t *testing.T, controller vmm.BalloonController, timeout time.Duration) vmm.BalloonStats {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		stats, err := controller.GetBalloonStats()
		if err == nil && stats.TotalMemory > 0 {
			return stats
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("balloon stats not ready within %s: %v", timeout, lastErr)
	return vmm.BalloonStats{}
}

func waitForBalloonTarget(t *testing.T, controller vmm.BalloonController, targetMiB uint64, timeout time.Duration) vmm.BalloonStats {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last vmm.BalloonStats
	for time.Now().Before(deadline) {
		stats, err := controller.GetBalloonStats()
		if err == nil {
			last = stats
			if stats.TargetMiB == targetMiB {
				return stats
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("balloon target did not reach %d MiB, last=%#v", targetMiB, last)
	return vmm.BalloonStats{}
}

func waitForBalloonActualAtLeast(t *testing.T, controller vmm.BalloonController, amountMiB uint64, timeout time.Duration) vmm.BalloonStats {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last vmm.BalloonStats
	for time.Now().Before(deadline) {
		stats, err := controller.GetBalloonStats()
		if err == nil {
			last = stats
			if stats.ActualMiB >= amountMiB {
				return stats
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("balloon actual did not reach %d MiB, last=%#v", amountMiB, last)
	return vmm.BalloonStats{}
}

func waitForMemoryHotplugPlugged(t *testing.T, controller vmm.MemoryHotplugController, amountMiB uint64, timeout time.Duration) vmm.MemoryHotplugStatus {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last vmm.MemoryHotplugStatus
	for time.Now().Before(deadline) {
		status, err := controller.GetMemoryHotplug()
		if err == nil {
			last = status
			if status.PluggedSizeMiB == amountMiB {
				return status
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("memory hotplug plugged size did not reach %d MiB, last=%#v", amountMiB, last)
	return vmm.MemoryHotplugStatus{}
}

func waitForGuestMemTotalAtLeast(t *testing.T, vm vmm.Handle, limit uint64, timeout time.Duration) guestexec.MemoryStats {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last guestexec.MemoryStats
	for time.Now().Before(deadline) {
		last = guestMemoryStats(t, vm)
		if last.TotalMemory >= limit {
			return last
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("guest MemTotal did not reach at least %d bytes, last=%d", limit, last.TotalMemory)
	return guestexec.MemoryStats{}
}

func waitForGuestMemTotalAtMost(t *testing.T, vm vmm.Handle, limit uint64, timeout time.Duration) guestexec.MemoryStats {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last guestexec.MemoryStats
	for time.Now().Before(deadline) {
		last = guestMemoryStats(t, vm)
		if last.TotalMemory <= limit {
			return last
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("guest MemTotal did not fall to at most %d bytes, last=%d", limit, last.TotalMemory)
	return guestexec.MemoryStats{}
}

func readRSSBytes(t *testing.T, pid int) uint64 {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatalf("read /proc/%d/status: %v", pid, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			t.Fatalf("parse VmRSS %q: %v", line, err)
		}
		return kb << 10
	}
	t.Fatalf("VmRSS not found for pid %d", pid)
	return 0
}

func waitForRSSBelow(t *testing.T, pid int, limit uint64, timeout time.Duration) uint64 {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last uint64
	for time.Now().Before(deadline) {
		last = readRSSBytes(t, pid)
		if last <= limit {
			return last
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("RSS for pid %d did not fall below %d bytes, last=%d", pid, limit, last)
	return last
}

func waitForProcessByVMID(t *testing.T, vmID string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		pid, err := findProcessByVMID(vmID)
		if err == nil {
			return pid
		}
		lastErr = err
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("process pid for vm %s not found within %s: %v", vmID, timeout, lastErr)
	return 0
}

func findProcessByVMID(vmID string) (int, error) {
	want := "--vm-id\x00" + vmID + "\x00"
	procEntries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	for _, entry := range procEntries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		if strings.Contains(string(cmdline), want) {
			return pid, nil
		}
	}
	return 0, fmt.Errorf("no process found for vm id %s", vmID)
}

func buildBalloonFixtureContext(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	binaryPath := buildGuestProgram(t, balloonWorkloadSource)
	copyFileIntoContext(t, binaryPath, filepath.Join(dir, "balloon-worker"))
	dockerfile := "FROM scratch\nCOPY balloon-worker /balloon-worker\nCMD [\"/balloon-worker\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	return dir
}

func buildHotplugFixtureContext(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	binaryPath := buildGuestProgram(t, hotplugWorkloadSource)
	copyFileIntoContext(t, binaryPath, filepath.Join(dir, "hotplug-worker"))
	dockerfile := "FROM scratch\nCOPY hotplug-worker /hotplug-worker\nCMD [\"/hotplug-worker\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	return dir
}

type workerBinaryPaths struct {
	jailer string
	vmm    string
}

func repoWorkerBinaries(t *testing.T) workerBinaryPaths {
	t.Helper()
	bins := buildProjectBinaries(t)
	return workerBinaryPaths{
		jailer: bins.jailer,
		vmm:    bins.vmm,
	}
}

func requireExecutable(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("%s is not executable", path)
	}
	return path
}
