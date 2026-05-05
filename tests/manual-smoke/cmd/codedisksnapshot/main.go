// codedisksnapshot is the manual smoke for Phase 2 of code-disk-attach:
// snapshot a template VM, then restore the SAME snapshot twice with
// two different code disks attached at restore time, and assert each
// run produces its own version's output via toolbox.Exec.
//
// Requires: root (for /dev/kvm + TUN), mkfs.ext4 on PATH, a guest
// kernel image. Run with:
//
//	sudo go run ./tests/manual-smoke/cmd/codedisksnapshot
//
// Override with GC_KERNEL=/path/to/vmlinux when the default isn't
// where the binary expects it.
//
// KNOWN LIMITATION (as of M1): the toolbox agent's /exec endpoint
// closes the connection before the EXIT frame on the FIRST request
// after a snapshot restore. The Health probe works, so the vsock
// channel is up, but exec is failing — this is a pre-existing
// toolbox/restore interaction (not introduced by Phase 2's
// AdditionalDrives plumbing). This smoke surfaces the issue and is
// expected to fail until that bug is fixed in a separate change.
// The plumbing itself is covered by the unit tests in
// pkg/vmm/restore_drives_test.go and pkg/container/codedisk_mount_test.go.
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gocracker/gocracker/internal/toolbox/agent"
	"github.com/gocracker/gocracker/internal/toolbox/client"
	"github.com/gocracker/gocracker/pkg/container"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("Phase 2 smoke OK — same snapshot, two distinct code disks attached at restore.")
}

func run() error {
	work := os.Getenv("WORK")
	if work == "" {
		work = "/tmp/gc-codedisk-snapshot-go"
	}
	kernel := os.Getenv("GC_KERNEL")
	if kernel == "" {
		repoRoot, err := repoRoot()
		if err != nil {
			return err
		}
		kernel = filepath.Join(repoRoot, "artifacts", "kernels", "gocracker-guest-standard-vmlinux")
	}
	if _, err := os.Stat(kernel); err != nil {
		return fmt.Errorf("kernel %q: %w", kernel, err)
	}

	if err := os.RemoveAll(work); err != nil {
		return err
	}
	if err := os.MkdirAll(work, 0755); err != nil {
		return err
	}

	// Build two code disks (same shape, different payload) so we can
	// prove they're swappable over the same snapshot.
	v1Disk, err := buildCodeDisk(work, "v1", "alpha")
	if err != nil {
		return fmt.Errorf("build v1: %w", err)
	}
	v2Disk, err := buildCodeDisk(work, "v2", "bravo")
	if err != nil {
		return fmt.Errorf("build v2: %w", err)
	}

	// Step 1: cold-boot a tiny Alpine + take a snapshot. We boot in
	// InteractiveExec mode so the toolbox agent stays idle waiting for
	// commands — that's exactly the point we want to capture.
	udsTemplate := filepath.Join(work, "template.sock")
	snapDir := filepath.Join(work, "snap")
	if err := os.MkdirAll(snapDir, 0755); err != nil {
		return err
	}
	fmt.Println("[1/3] booting template VM (Alpine, idle, exec-enabled)...")
	res, err := container.Run(container.RunOptions{
		Image:           "alpine:3.20",
		KernelPath:      kernel,
		MemMB:           256,
		DiskSizeMB:      256,
		ID:              "smoke-codedisk-template",
		ExecEnabled:     true,
		InteractiveExec: true,
		VsockUDSPath:    udsTemplate,
		JailerMode:      container.JailerModeOff,
		CacheDir:        filepath.Join(work, "cache"),
	})
	if err != nil {
		return fmt.Errorf("template boot: %w", err)
	}
	// Wait for the toolbox agent to bind on the UDS so the snapshot
	// captures a *responsive* agent. Without this the post-restore
	// dial races against init's supervisor spawn and we see "read
	// status line: EOF" on the first exec.
	if err := waitForToolboxReady(udsTemplate, 30*time.Second); err != nil {
		res.VM.Stop()
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = res.VM.WaitStopped(stopCtx)
		cancel()
		res.Close()
		return fmt.Errorf("template toolbox not ready: %w", err)
	}

	fmt.Println("[1/3] taking snapshot...")
	if _, err := res.VM.TakeSnapshot(snapDir); err != nil {
		res.VM.Stop()
		return fmt.Errorf("snapshot: %w", err)
	}
	res.VM.Stop()
	stopCtx, cancelStop := context.WithTimeout(context.Background(), 10*time.Second)
	_ = res.VM.WaitStopped(stopCtx)
	cancelStop()
	res.Close()

	// Step 2: restore + attach v1, exec /app/main.sh via toolbox.
	fmt.Println("[2/3] restoring snapshot with v1.ext4...")
	if got, err := restoreAndExec(kernel, snapDir, v1Disk, work, "v1"); err != nil {
		return err
	} else if !strings.Contains(got, "alpha") {
		return fmt.Errorf("v1 output = %q, want substring %q", got, "alpha")
	} else {
		fmt.Printf("  v1 stdout = %q\n", strings.TrimSpace(got))
	}

	// Step 3: restore SAME snapshot + attach v2.
	fmt.Println("[3/3] restoring SAME snapshot with v2.ext4...")
	if got, err := restoreAndExec(kernel, snapDir, v2Disk, work, "v2"); err != nil {
		return err
	} else if !strings.Contains(got, "bravo") {
		return fmt.Errorf("v2 output = %q, want substring %q", got, "bravo")
	} else {
		fmt.Printf("  v2 stdout = %q\n", strings.TrimSpace(got))
	}

	return nil
}

// waitForToolboxReady polls until the UDS path exists AND the toolbox
// agent answers a Health probe successfully, or until the deadline
// elapses. Captures the cold-boot init race: the listener appears at
// the path before the in-guest supervisor has spawned the agent, so a
// stat-only check is insufficient.
func waitForToolboxReady(udsPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	cli := client.New(udsPath)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(udsPath); err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		_, err := cli.Health(ctx)
		cancel()
		if err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("toolbox at %s not ready within %s", udsPath, timeout)
}

func restoreAndExec(kernel, snapDir, diskPath, work, label string) (string, error) {
	uds := filepath.Join(work, label+".sock")
	res, err := container.Run(container.RunOptions{
		KernelPath:   kernel,
		SnapshotDir:  snapDir,
		ID:           "smoke-codedisk-" + label,
		ExecEnabled:  true,
		VsockUDSPath: uds,
		JailerMode:   container.JailerModeOff,
		CodeDisks: []container.CodeDisk{
			{HostPath: diskPath, Mount: "/app", FSType: "ext4", ReadOnly: true},
		},
	})
	if err != nil {
		return "", fmt.Errorf("restore (%s): %w", label, err)
	}
	defer func() {
		res.VM.Stop()
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = res.VM.WaitStopped(stopCtx)
		cancel()
		res.Close()
	}()

	// Wait for the new UDS listener (post-restore) to bind and the
	// toolbox to answer Health. Without this, our exec races the
	// vsock device's bring-up after restore.
	if err := waitForToolboxReady(uds, 15*time.Second); err != nil {
		return "", fmt.Errorf("post-restore toolbox not ready (%s): %w", label, err)
	}

	cli := client.New(uds)
	var stdout, stderr bytes.Buffer
	execCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := cli.Exec(execCtx, agent.ExecRequest{Cmd: []string{"sh", "-c", "cat /app/main.sh"}}, nil, &stdout, &stderr)
	if err != nil {
		return "", fmt.Errorf("exec (%s): %w (stderr=%q)", label, err, stderr.String())
	}
	if r.ExitCode != 0 {
		return "", fmt.Errorf("exec (%s) exit=%d stderr=%q", label, r.ExitCode, stderr.String())
	}
	return stdout.String(), nil
}

func buildCodeDisk(work, label, marker string) (string, error) {
	payloadDir := filepath.Join(work, label, "payload")
	imgPath := filepath.Join(work, label+".ext4")
	if err := os.MkdirAll(payloadDir, 0755); err != nil {
		return "", err
	}
	script := fmt.Sprintf("#!/bin/sh\necho code-disk-snapshot %s: %s\n", label, marker)
	if err := os.WriteFile(filepath.Join(payloadDir, "main.sh"), []byte(script), 0755); err != nil {
		return "", err
	}
	if err := exec.Command("truncate", "-s", "32M", imgPath).Run(); err != nil {
		return "", fmt.Errorf("truncate %s: %w", imgPath, err)
	}
	cmd := exec.Command("mkfs.ext4", "-F", "-L", "code-"+label, "-d", payloadDir, imgPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("mkfs.ext4 %s: %w\n%s", imgPath, err, string(out))
	}
	return imgPath, nil
}

func repoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for d := wd; d != "/"; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d, nil
		}
	}
	return "", fmt.Errorf("could not locate repo root from %s", wd)
}
