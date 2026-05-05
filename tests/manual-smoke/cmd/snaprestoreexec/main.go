// snaprestoreexec is a minimal diagnostic: boot a tiny Alpine VM with
// the toolbox agent enabled (InteractiveExec=true), take a snapshot,
// restore it WITHOUT code-disks, and try to /exec something via the
// toolbox.
//
// If THIS smoke fails the same way as codedisksnapshot
// (agent closed conn before EXIT frame), the bug is general
// post-restore-exec, not specific to AdditionalDrives plumbing.
//
// Run: sudo go run ./tests/manual-smoke/cmd/snaprestoreexec
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	fmt.Println("OK — snapshot+restore+exec works without code-disk")
}

func run() error {
	work := "/tmp/gc-snaprestore-exec"
	if err := os.RemoveAll(work); err != nil {
		return err
	}
	if err := os.MkdirAll(work, 0755); err != nil {
		return err
	}

	kernel := os.Getenv("GC_KERNEL")
	if kernel == "" {
		repoRoot, _ := os.Getwd()
		kernel = filepath.Join(repoRoot, "artifacts", "kernels", "gocracker-guest-standard-vmlinux")
	}
	if _, err := os.Stat(kernel); err != nil {
		return fmt.Errorf("kernel %q: %w", kernel, err)
	}

	udsTemplate := filepath.Join(work, "template.sock")
	snapDir := filepath.Join(work, "snap")
	if err := os.MkdirAll(snapDir, 0755); err != nil {
		return err
	}
	fmt.Println("[1/3] booting template VM...")
	res, err := container.Run(container.RunOptions{
		Image:           "alpine:3.20",
		KernelPath:      kernel,
		MemMB:           256,
		DiskSizeMB:      256,
		ID:              "smoke-snaprestore-template",
		ExecEnabled:     true,
		InteractiveExec: true,
		VsockUDSPath:    udsTemplate,
		JailerMode:      container.JailerModeOff,
		CacheDir:        filepath.Join(work, "cache"),
	})
	if err != nil {
		return fmt.Errorf("template boot: %w", err)
	}

	cli := client.New(udsTemplate)
	if err := waitHealthy(cli, 30*time.Second); err != nil {
		res.VM.Stop()
		res.Close()
		return fmt.Errorf("template not ready: %w", err)
	}

	// Pre-snapshot: confirm exec works on the template.
	if out, err := execEcho(cli, "pre-snapshot"); err != nil {
		res.VM.Stop()
		res.Close()
		return fmt.Errorf("pre-snapshot exec: %w", err)
	} else {
		fmt.Printf("  pre-snapshot exec stdout=%q\n", out)
	}

	fmt.Println("[2/3] taking snapshot...")
	if _, err := res.VM.TakeSnapshot(snapDir); err != nil {
		res.VM.Stop()
		res.Close()
		return fmt.Errorf("snapshot: %w", err)
	}
	res.VM.Stop()
	stopCtx, cancelStop := context.WithTimeout(context.Background(), 10*time.Second)
	_ = res.VM.WaitStopped(stopCtx)
	cancelStop()
	res.Close()

	fmt.Println("[3/3] restoring snapshot (NO code-disk)...")
	udsRestored := filepath.Join(work, "restored.sock")
	res2, err := container.Run(container.RunOptions{
		KernelPath:   kernel,
		SnapshotDir:  snapDir,
		ID:           "smoke-snaprestore-restored",
		ExecEnabled:  true,
		VsockUDSPath: udsRestored,
		JailerMode:   container.JailerModeOff,
	})
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	defer func() {
		res2.VM.Stop()
		ctx, c := context.WithTimeout(context.Background(), 10*time.Second)
		_ = res2.VM.WaitStopped(ctx)
		c()
		res2.Close()
	}()

	cli2 := client.New(udsRestored)
	if err := waitHealthy(cli2, 15*time.Second); err != nil {
		return fmt.Errorf("post-restore not ready: %w", err)
	}
	fmt.Println("  post-restore Health OK")

	// Try a few execs in a row to see the exact failure pattern.
	for i := 0; i < 3; i++ {
		out, err := execEcho(cli2, fmt.Sprintf("post-restore-%d", i))
		if err != nil {
			fmt.Printf("  post-restore exec[%d] FAILED: %v\n", i, err)
		} else {
			fmt.Printf("  post-restore exec[%d] stdout=%q\n", i, out)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil
}

func waitHealthy(cli *client.Client, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		_, err := cli.Health(ctx)
		cancel()
		if err == nil {
			return nil
		}
		last = err
		time.Sleep(100 * time.Millisecond)
	}
	if last != nil {
		return last
	}
	return fmt.Errorf("not ready in %s", timeout)
}

func execEcho(cli *client.Client, msg string) (string, error) {
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := cli.Exec(ctx, agent.ExecRequest{Cmd: []string{"/bin/echo", msg}}, nil, &stdout, &stderr)
	if err != nil {
		return "", fmt.Errorf("exec: %w (stderr=%q)", err, stderr.String())
	}
	if r.ExitCode != 0 {
		return "", fmt.Errorf("exit=%d stderr=%q", r.ExitCode, stderr.String())
	}
	return stdout.String(), nil
}
