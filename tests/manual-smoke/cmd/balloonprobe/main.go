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
	"github.com/gocracker/gocracker/pkg/vmm"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "err:", err)
		os.Exit(1)
	}
}

func run() error {
	work := "/tmp/balloon-probe"
	os.RemoveAll(work)
	os.MkdirAll(work, 0755)
	uds := filepath.Join(work, "vm.sock")
	res, err := container.Run(container.RunOptions{
		Image:        "alpine:3.20",
		KernelPath:   "/home/misael/Desktop/projects/gocracker/artifacts/kernels/gocracker-guest-standard-vmlinux",
		MemMB:        256,
		DiskSizeMB:   256,
		ID:           "balloon-probe",
		ExecEnabled:  true,
		Cmd:          []string{"/bin/sh", "-lc", "sleep infinity"},
		VsockUDSPath: uds,
		JailerMode:   container.JailerModeOff,
		CacheDir:     filepath.Join(work, "cache"),
		Balloon: &vmm.BalloonConfig{
			AmountMiB:             0, // baseline: no inflation
			DeflateOnOOM:          true,
			StatsPollingIntervalS: 1,
		},
	})
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}
	defer func() {
		res.VM.Stop()
		ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		_ = res.VM.WaitStopped(ctx)
		c()
		res.Close()
	}()

	type stater interface {
		GetBalloonConfig() (vmm.BalloonConfig, error)
		GetBalloonStats() (vmm.BalloonStats, error)
	}
	st, _ := res.VM.(stater)
	for _, sl := range []time.Duration{500 * time.Millisecond, 2 * time.Second, 5 * time.Second, 10 * time.Second} {
		time.Sleep(sl)
		cli := client.New(uds)
		var stdout, stderr bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := cli.Exec(ctx, agent.ExecRequest{Cmd: []string{"sh", "-c", "grep -E '^(MemTotal|MemFree|MemAvailable):' /proc/meminfo"}}, nil, &stdout, &stderr)
		cancel()
		if err != nil {
			fmt.Printf("[t=%s] exec err: %v\n", sl, err)
			continue
		}
		var hostInfo string
		if st != nil {
			cfg, _ := st.GetBalloonConfig()
			stats, sErr := st.GetBalloonStats()
			hostInfo = fmt.Sprintf("[host] target=%d MiB; stats: target=%d actual=%d (err=%v)\n",
				cfg.AmountMiB, stats.TargetMiB, stats.ActualMiB, sErr)
		}
		fmt.Printf("[t=%s] %s%s---\n", sl, hostInfo, stdout.String())
	}
	return nil
}
