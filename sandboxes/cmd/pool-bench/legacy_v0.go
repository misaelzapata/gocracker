//go:build ignore

// pool-bench: pre-warms N VMs from a warm-cache snapshot and benchmarks
// exec latency against the live pool — no CLI overhead, no restore cost per
// request.
//
// Usage:
//
//	sudo go run ./tools/pool-bench \
//	  -image oven/bun:alpine \
//	  -kernel artifacts/kernels/gocracker-guest-minimal-vmlinux \
//	  -cmd 'bun --version' \
//	  -pool 3 -iter 20
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/gocracker/gocracker/internal/guestexec"
	"github.com/gocracker/gocracker/pkg/container"
	"github.com/gocracker/gocracker/pkg/vmm"
	"github.com/gocracker/gocracker/pkg/warmcache"
)

func main() {
	image := flag.String("image", "oven/bun:alpine", "OCI image")
	kernel := flag.String("kernel", "artifacts/kernels/gocracker-guest-minimal-vmlinux", "kernel path")
	cmdStr := flag.String("cmd", "bun --version", "command to exec in each VM")
	poolSize := flag.Int("pool", 3, "number of VMs to keep pre-restored")
	iter := flag.Int("iter", 20, "benchmark iterations")
	memMB := flag.Int("mem", 256, "guest memory MiB")
	flag.Parse()

	cmd := splitCmd(*cmdStr)

	fmt.Printf("pool-bench: image=%s pool=%d iter=%d cmd=%v\n\n",
		*image, *poolSize, *iter, cmd)

	// Ensure warm snapshot exists.
	fmt.Print("checking warm snapshot... ")
	key, ok := ensureSnapshot(*image, *kernel, *memMB)
	if !ok {
		fmt.Fprintln(os.Stderr, "\nfailed to build warm snapshot")
		os.Exit(1)
	}
	snapshotDir, hit := warmcache.Lookup(warmcache.DefaultRoot(), key)
	if !hit {
		fmt.Fprintln(os.Stderr, "snapshot not in cache after build")
		os.Exit(1)
	}
	fmt.Printf("ok (%s)\n\n", key[:12])

	// Pre-fill pool.
	fmt.Printf("pre-restoring %d VMs... ", *poolSize)
	pool := make(chan *container.RunResult, *poolSize)
	for i := 0; i < *poolSize; i++ {
		r, err := restoreVM(snapshotDir, *kernel, *memMB)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nfailed to restore VM %d: %v\n", i, err)
			os.Exit(1)
		}
		pool <- r
	}
	fmt.Printf("done\n\n")

	// Benchmark: grab VM from pool, exec cmd, measure, refill async.
	samples := make([]int64, 0, *iter)
	fails := 0
	var refillWg sync.WaitGroup

	fmt.Printf("%-6s  %8s  %s\n", "iter", "TTI", "output")
	fmt.Println("------  --------  ------")

	for i := 0; i < *iter; i++ {
		vm := <-pool

		t0 := time.Now()
		out, err := execOnVM(vm, cmd)
		tti := time.Since(t0)

		if err != nil {
			fmt.Printf("%-6d  %8s  FAIL: %v\n", i+1, tti.Round(time.Millisecond), err)
			fails++
		} else {
			samples = append(samples, tti.Milliseconds())
			fmt.Printf("%-6d  %8s  %s\n", i+1, tti.Round(time.Millisecond), out)
		}

		// Stop used VM and restore a fresh one in background.
		used := vm
		refillWg.Add(1)
		go func() {
			defer refillWg.Done()
			used.VM.Stop()
			r, err := restoreVM(snapshotDir, *kernel, *memMB)
			if err == nil {
				pool <- r
			}
		}()
	}

	refillWg.Wait()
	// drain pool
	close(pool)
	for r := range pool {
		r.VM.Stop()
	}

	// Stats
	if len(samples) == 0 {
		fmt.Println("no successful samples")
		os.Exit(1)
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	median := samples[len(samples)/2]
	var sum int64
	for _, s := range samples {
		sum += s
	}
	mean := sum / int64(len(samples))
	p95 := samples[int(float64(len(samples))*0.95)]

	fmt.Printf("\n=== results (%d ok / %d fail) ===\n", len(samples), fails)
	fmt.Printf("median  %dms\n", median)
	fmt.Printf("mean    %dms\n", mean)
	fmt.Printf("min     %dms\n", samples[0])
	fmt.Printf("max     %dms\n", samples[len(samples)-1])
	fmt.Printf("p95     %dms\n", p95)
}

func ensureSnapshot(image, kernel string, memMB int) (string, bool) {
	opts := container.RunOptions{
		Image:       image,
		KernelPath:  kernel,
		MemMB:       uint64(memMB),
		CPUs:        1,
		ExecEnabled: true,
		WarmCapture: true,
		JailerMode:  container.JailerModeOff,
	}
	key, ok := container.ComputeWarmCacheKey(opts)
	if !ok {
		return "", false
	}
	if _, hit := warmcache.Lookup(warmcache.DefaultRoot(), key); hit {
		return key, true
	}
	// Cold boot to capture snapshot.
	result, err := container.Run(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cold boot failed: %v\n", err)
		return "", false
	}
	if result.WarmDone != nil {
		<-result.WarmDone
	}
	result.VM.Stop()
	result.Close()
	return key, true
}

func restoreVM(snapshotDir, kernel string, memMB int) (*container.RunResult, error) {
	return container.Run(container.RunOptions{
		KernelPath:  kernel,
		MemMB:       uint64(memMB),
		CPUs:        1,
		SnapshotDir: snapshotDir,
		ExecEnabled: true,
		JailerMode:  container.JailerModeOff,
	})
}

func execOnVM(r *container.RunResult, cmd []string) (string, error) {
	dialer, ok := r.VM.(vmm.VsockDialer)
	if !ok {
		return "", fmt.Errorf("no vsock dialer")
	}
	cfg := r.VM.VMConfig()
	port := uint32(guestexec.DefaultVsockPort)
	if cfg.Exec != nil && cfg.Exec.VsockPort != 0 {
		port = cfg.Exec.VsockPort
	}
	conn, err := dialer.DialVsock(port)
	if err != nil {
		return "", fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	if err := guestexec.Encode(conn, guestexec.Request{
		Mode:    guestexec.ModeExec,
		Command: cmd,
	}); err != nil {
		return "", fmt.Errorf("encode: %w", err)
	}
	var resp guestexec.Response
	if err := guestexec.Decode(conn, &resp); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	if resp.Error != "" {
		return "", fmt.Errorf("%s", resp.Error)
	}
	out := resp.Stdout
	if len(out) > 40 {
		out = out[:40]
	}
	return out, nil
}

func splitCmd(s string) []string {
	// Simple split — for shell features use []string{"/bin/sh","-c",s}
	var parts []string
	cur := ""
	for _, c := range s {
		if c == ' ' && cur != "" {
			parts = append(parts, cur)
			cur = ""
		} else if c != ' ' {
			cur += string(c)
		}
	}
	if cur != "" {
		parts = append(parts, cur)
	}
	return parts
}
