// pool-bench measures end-to-end lease latency against the Fase 5
// sandboxes/internal/pool warm pool. The plan §5 success criterion
// is p95 lease < 20 ms on a burst of 50 concurrent CreateSandbox
// requests, with 0 errors and 0 orphan VMs after 5 min.
//
// This bench drives sandboxd's Manager directly (no HTTP overhead)
// so the measurement is pure pool latency: pool.AcquireWait + IP
// allocator + Resume + SetNetwork. The legacy v0 bench in
// legacy_v0.go (build-tag-gated) exercises the warmcache restore
// path without going through the pool surface — keep both for now,
// the legacy one stays useful for regression checks on the warm-cache
// internals.
//
// Usage:
//
//	sudo go run ./tools/pool-bench \
//	  -image alpine:3.20 \
//	  -kernel artifacts/kernels/gocracker-guest-standard-vmlinux \
//	  -burst 50 -warm 8
//
// Reports p50 / p95 / p99 lease ms, plus a budget check against the
// plan target (--p95-budget-ms, default 20).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gocracker/gocracker/pkg/container"
	"github.com/gocracker/gocracker/sandboxes/internal/sandboxd"
)

func main() {
	image := flag.String("image", "alpine:3.20", "OCI image")
	kernel := flag.String("kernel", "artifacts/kernels/gocracker-guest-standard-vmlinux", "kernel path")
	burst := flag.Int("burst", 50, "concurrent lease requests")
	warm := flag.Int("warm", 8, "MinPaused / MaxPaused (pool size)")
	memMB := flag.Uint64("mem", 256, "guest memory MiB")
	jailerMode := flag.String("jailer", container.JailerModeOff, "jailer mode: on or off")
	p95BudgetMs := flag.Int("p95-budget-ms", 20, "fail if p95 exceeds this (plan §5: <20 ms)")
	stateDir := flag.String("state-dir", "/tmp/pool-bench-state", "sandboxd state directory")
	timeoutS := flag.Int("timeout-s", 30, "per-lease wait timeout")
	flag.Parse()

	if os.Getuid() != 0 {
		fmt.Fprintln(os.Stderr, "pool-bench: must run as root (KVM + jailer require it)")
		os.Exit(2)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := os.MkdirAll(*stateDir, 0o755); err != nil {
		fatal("mkdir state-dir: %v", err)
	}
	store, err := sandboxd.NewStore("")
	if err != nil {
		fatal("new store: %v", err)
	}
	mgr := &sandboxd.Manager{Store: store, StateDir: *stateDir}

	tmpl := "bench-pool"
	fmt.Printf("pool-bench: image=%s burst=%d warm=%d jailer=%s\n", *image, *burst, *warm, *jailerMode)
	fmt.Printf("registering pool '%s' (MinPaused=%d MaxPaused=%d)... ", tmpl, *warm, *warm)
	tStart := time.Now()
	reg, err := mgr.RegisterPool(ctx, sandboxd.CreatePoolRequest{
		TemplateID: tmpl,
		Image:      *image,
		KernelPath: *kernel,
		MemMB:      *memMB,
		JailerMode: *jailerMode,
		MinPaused:  *warm,
		MaxPaused:  *warm,
	})
	if err != nil {
		fatal("register pool: %v", err)
	}
	defer func() {
		_ = mgr.UnregisterPool(tmpl)
	}()
	fmt.Printf("ok (%dms)\n", time.Since(tStart).Milliseconds())

	// Wait until the pool is fully warm. Bench timing depends on hot
	// hits — measuring while half the requests cold-boot inflates p95
	// to seconds and obscures the steady-state target.
	fmt.Printf("warming up to %d paused VMs", *warm)
	warmStart := time.Now()
	for {
		counts := getPausedCount(mgr, tmpl)
		fmt.Printf(".")
		if counts >= *warm {
			break
		}
		if time.Since(warmStart) > 5*time.Minute {
			fmt.Println()
			fatal("pool didn't reach %d paused in 5 min (current: %d)", *warm, counts)
		}
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Printf(" ok (%ds)\n\n", int(time.Since(warmStart).Seconds()))
	_ = reg

	// Burst N concurrent leases.
	fmt.Printf("burst-%d leases ...\n", *burst)
	type sample struct {
		idx     int
		latency time.Duration
		err     error
		id      string
	}
	results := make(chan sample, *burst)
	var wg sync.WaitGroup
	var inFlight atomic.Int32

	burstStart := time.Now()
	for i := 0; i < *burst; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			inFlight.Add(1)
			defer inFlight.Add(-1)

			t0 := time.Now()
			sb, err := mgr.LeaseSandbox(ctx, sandboxd.LeaseSandboxRequest{
				TemplateID: tmpl,
				Timeout:    time.Duration(*timeoutS) * time.Second,
			})
			elapsed := time.Since(t0)
			results <- sample{idx: i, latency: elapsed, err: err, id: sb.ID}
		}(i)
	}
	wg.Wait()
	close(results)
	burstElapsed := time.Since(burstStart)

	// Collect.
	var oks []time.Duration
	var leasedIDs []string
	fails := 0
	var firstErr error
	for s := range results {
		if s.err != nil {
			fails++
			if firstErr == nil {
				firstErr = s.err
			}
			continue
		}
		oks = append(oks, s.latency)
		leasedIDs = append(leasedIDs, s.id)
	}

	// Tear leased VMs down so the next bench / shutdown is clean.
	fmt.Printf("\nreleasing %d leases...\n", len(leasedIDs))
	for _, id := range leasedIDs {
		_ = mgr.ReleaseLeased(id)
	}

	if len(oks) == 0 {
		fatal("no successful leases (fails=%d, first err: %v)", fails, firstErr)
	}

	// Stats.
	sort.Slice(oks, func(i, j int) bool { return oks[i] < oks[j] })
	p50 := oks[len(oks)/2].Milliseconds()
	p95 := oks[int(float64(len(oks))*0.95)].Milliseconds()
	p99 := oks[int(float64(len(oks))*0.99)].Milliseconds()
	min := oks[0].Milliseconds()
	max := oks[len(oks)-1].Milliseconds()
	var sum time.Duration
	for _, d := range oks {
		sum += d
	}
	mean := (sum / time.Duration(len(oks))).Milliseconds()

	fmt.Println("\n================================================================")
	fmt.Printf("burst-%d (%d ok / %d fail, total %s)\n", *burst, len(oks), fails, burstElapsed.Round(time.Millisecond))
	fmt.Println("----------------------------------------------------------------")
	fmt.Printf("min   %3d ms\n", min)
	fmt.Printf("p50   %3d ms\n", p50)
	fmt.Printf("mean  %3d ms\n", mean)
	fmt.Printf("p95   %3d ms  (budget %d ms)\n", p95, *p95BudgetMs)
	fmt.Printf("p99   %3d ms\n", p99)
	fmt.Printf("max   %3d ms\n", max)
	fmt.Println("================================================================")

	if firstErr != nil {
		fmt.Printf("\nfirst error: %v\n", firstErr)
	}

	// Gate.
	if fails > 0 {
		fmt.Printf("\nFAIL: %d failures (plan: 0)\n", fails)
		os.Exit(1)
	}
	if int(p95) > *p95BudgetMs {
		fmt.Printf("\nFAIL: p95=%dms > budget=%dms\n", p95, *p95BudgetMs)
		os.Exit(1)
	}
	fmt.Printf("\nPASS\n")
}

func getPausedCount(mgr *sandboxd.Manager, templateID string) int {
	for _, r := range mgr.ListPools() {
		if r.TemplateID == templateID {
			return r.Counts["paused"]
		}
	}
	return 0
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "pool-bench: "+format+"\n", args...)
	os.Exit(1)
}
