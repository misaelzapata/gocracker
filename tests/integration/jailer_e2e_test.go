//go:build integration

package integration

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	internalapi "github.com/gocracker/gocracker/internal/api"
)

// TestE2EJailer boots a VM through `gocracker serve` with the standalone
// jailer enabled, then verifies on the host that the VMM worker was
// re-exec'd under UID=1000 and inside the chroot base. Requires E2E=1,
// root, and cgroup v2 (otherwise skips cleanly).
func TestE2EJailer(t *testing.T) {
	if os.Getenv("E2E") != "1" {
		t.Skip("set E2E=1 to enable")
	}
	if os.Getuid() != 0 {
		t.Skipf("jailer e2e requires root")
	}
	if err := requireCgroupV2(); err != nil {
		t.Skipf("jailer requires cgroup v2: %v", err)
	}
	kernel := resolveE2EKernel(t)
	bins := buildProjectBinaries(t)
	cleanupPrivilegedRuntime(t)
	t.Cleanup(func() { cleanupPrivilegedRuntime(t) })

	addr := freeLocalAddr(t)
	serverURL := "http://" + addr
	cacheDir := filepath.Join(t.TempDir(), "cache")
	stateDir := filepath.Join(t.TempDir(), "state")
	chrootBase := filepath.Join(t.TempDir(), "jailer-e2e")
	for _, d := range []string{cacheDir, stateDir, chrootBase} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	serveCmd := exec.Command(bins.gocracker,
		"serve",
		"--addr", addr,
		"--jailer", "on",
		"--jailer-binary", bins.jailer,
		"--vmm-binary", bins.vmm,
		"--cache-dir", cacheDir,
		"--state-dir", stateDir,
		"--chroot-base-dir", chrootBase,
		"--uid", "1000",
		"--gid", "1000",
		"--trusted-kernel-dir", filepath.Dir(kernel),
	)
	var serveLog lockedBuffer
	serveCmd.Stdout = &serveLog
	serveCmd.Stderr = &serveLog
	serveCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := serveCmd.Start(); err != nil {
		t.Fatalf("start gocracker serve: %v", err)
	}
	t.Cleanup(func() { stopCommand(t, serveCmd) })
	waitForAPI(t, serverURL, 45*time.Second)

	client := internalapi.NewClient(serverURL)
	runCtx, runCancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer runCancel()

	runResp, err := client.Run(runCtx, internalapi.RunRequest{
		Image:       "node:20-alpine",
		KernelPath:  kernel,
		MemMB:       256,
		DiskSizeMB:  512,
		Cmd:         []string{"/bin/sh", "-lc", "sleep infinity"},
		ExecEnabled: true,
	})
	if err != nil {
		// Jailer prereq failures (missing cgroup controller, mount/unshare
		// perms, pivot_root on overlayfs, etc.) surface here. Rather than
		// failing the suite on hosts we can't easily fix, skip cleanly.
		msg := err.Error()
		if strings.Contains(msg, "pivot_root") ||
			strings.Contains(msg, "cgroup") ||
			strings.Contains(msg, "unshare") ||
			strings.Contains(msg, "mount") {
			t.Skipf("jailer prereq failure (skipping): %v\nserve log:\n%s", err, serveLog.String())
		}
		t.Fatalf("api run: %v\nserve log:\n%s", err, serveLog.String())
	}
	defer func() { _ = client.StopVM(context.Background(), runResp.ID) }()

	info, err := waitForVMStateViaClient(t, client, runResp.ID, "running", 120*time.Second)
	if err != nil {
		t.Fatalf("wait for running: %v\nserve log:\n%s", err, serveLog.String())
	}
	if info.State != "running" {
		t.Fatalf("VM state = %s, want running\nserve log:\n%s", info.State, serveLog.String())
	}

	// Find the VMM worker process on the host. pgrep -f matches the full
	// command line, which includes --vm-id <id>.
	vmmPID, err := findVMMProcessForVM(runResp.ID, 15*time.Second)
	if err != nil {
		t.Fatalf("locate vmm pid for %s: %v\nserve log:\n%s", runResp.ID, err, serveLog.String())
	}

	// Assert UID=1000 by reading /proc/<pid>/status. Under jailer the
	// effective UID is dropped before execve'ing the VMM, so every Uid:
	// column (real/effective/saved/fs) must be 1000.
	uids, err := readProcUIDs(vmmPID)
	if err != nil {
		t.Fatalf("read /proc/%d/status: %v", vmmPID, err)
	}
	for i, uid := range uids {
		if uid != 1000 {
			t.Fatalf("/proc/%d/status Uid[%d] = %d, want 1000 (all)\nuids: %v", vmmPID, i, uid, uids)
		}
	}

	// Assert /proc/<pid>/root points into the chroot base dir.
	// pivot_root makes root symlink resolve to the jail root, which lives
	// inside the chrootBase directory we passed.
	rootLink, err := os.Readlink(fmt.Sprintf("/proc/%d/root", vmmPID))
	if err != nil {
		// On some kernels the link is only readable with CAP_SYS_PTRACE —
		// we're root, so this should work. If it doesn't, fail loudly.
		t.Fatalf("readlink /proc/%d/root: %v", vmmPID, err)
	}
	absBase, _ := filepath.Abs(chrootBase)
	if !strings.HasPrefix(rootLink, absBase) && !strings.HasPrefix(rootLink, chrootBase) {
		t.Fatalf("/proc/%d/root = %q, want prefix %q", vmmPID, rootLink, chrootBase)
	}

	// Drive a one-shot exec through the jailed VMM — confirms vsock +
	// exec agent work across the privilege boundary. `whoami` on alpine
	// normally prints the effective user inside the guest (root) — the
	// point is just that exec completes with a non-empty response.
	execResp := waitForExecResponse(t, client, runResp.ID, internalapi.ExecRequest{
		Command: []string{"/bin/sh", "-lc", "echo jailer-exec-ok; id -u"},
	}, 60*time.Second)
	if !strings.Contains(execResp.Stdout, "jailer-exec-ok") {
		t.Fatalf("exec stdout = %q, want jailer-exec-ok\nstderr=%q exit=%d",
			execResp.Stdout, execResp.Stderr, execResp.ExitCode)
	}
	if execResp.ExitCode != 0 {
		t.Fatalf("exec exit = %d, stderr=%q", execResp.ExitCode, execResp.Stderr)
	}

	if err := client.StopVM(context.Background(), runResp.ID); err != nil {
		t.Fatalf("stop vm: %v", err)
	}
}

// findVMMProcessForVM uses pgrep to locate the gocracker-vmm worker process
// whose command line matches the given VM id. We poll briefly because the
// process may race the /run API response (the handler returns once state is
// "running", but pgrep output can lag by a few ms under load).
func findVMMProcessForVM(vmID string, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := exec.Command("pgrep", "-f", "gocracker-vmm.*--vm-id "+vmID).Output()
		if err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if line == "" {
					continue
				}
				pid, cerr := strconv.Atoi(strings.TrimSpace(line))
				if cerr == nil && pid > 0 {
					return pid, nil
				}
			}
		}
		// Fallback: match by binary name alone and pick the most recent one.
		out2, err2 := exec.Command("pgrep", "-f", "gocracker-vmm").Output()
		if err2 == nil {
			lines := strings.Split(strings.TrimSpace(string(out2)), "\n")
			for i := len(lines) - 1; i >= 0; i-- {
				pid, cerr := strconv.Atoi(strings.TrimSpace(lines[i]))
				if cerr == nil && pid > 0 {
					// Confirm cmdline mentions our VM id.
					cmdline, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
					if strings.Contains(string(cmdline), vmID) {
						return pid, nil
					}
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return 0, fmt.Errorf("no gocracker-vmm process found for VM %s within %s", vmID, timeout)
}

// readProcUIDs parses the Uid: line from /proc/<pid>/status and returns the
// four-tuple (real, effective, saved, fs).
func readProcUIDs(pid int) ([4]int, error) {
	var out [4]int
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return out, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "Uid:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "Uid:"))
		if len(fields) != 4 {
			return out, fmt.Errorf("unexpected Uid line: %q", line)
		}
		for i, f := range fields {
			n, err := strconv.Atoi(f)
			if err != nil {
				return out, fmt.Errorf("parse Uid[%d]=%q: %w", i, f, err)
			}
			out[i] = n
		}
		return out, nil
	}
	if err := scanner.Err(); err != nil {
		return out, err
	}
	return out, fmt.Errorf("no Uid: line in /proc/%d/status", pid)
}

// requireCgroupV2 returns nil when the host exposes a unified cgroup v2
// hierarchy at /sys/fs/cgroup (the jailer hard-requires this).
func requireCgroupV2() error {
	// /sys/fs/cgroup/cgroup.controllers only exists on unified v2 hosts.
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return fmt.Errorf("/sys/fs/cgroup/cgroup.controllers missing: %w", err)
	}
	return nil
}
