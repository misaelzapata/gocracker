// template-bench validates plan §6 §6 step 3: "Segundo create
// idéntico: cache hit, < 10 ms". Drives sandboxd.Manager directly
// (no HTTP overhead, same as pool-bench) and measures the second
// CreateTemplate call against the same Spec.
//
// First Build is a cold-boot + WarmCapture (~3 s). Second Build
// must hit the SpecHash cache and return in <10 ms; that's the gate.
//
// Usage:
//
//	sudo go run ./sandboxes/cmd/template-bench \
//	  -image alpine:3.20 \
//	  -kernel /abs/path/to/kernel \
//	  -p95-budget-ms 10
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gocracker/gocracker/sandboxes/internal/sandboxd"
)

func main() {
	image := flag.String("image", "alpine:3.20", "OCI image")
	kernel := flag.String("kernel", "artifacts/kernels/gocracker-guest-standard-vmlinux", "kernel path (absolute recommended)")
	memMB := flag.Uint64("mem", 256, "guest memory MiB")
	stateDir := flag.String("state-dir", "/tmp/template-bench-state", "sandboxd state directory")
	cacheBudgetMs := flag.Int("p95-budget-ms", 10, "fail if 2nd-create cache hit exceeds this (plan §6 target <10 ms)")
	iterations := flag.Int("iterations", 5, "number of cache-hit iterations to measure")
	flag.Parse()

	if os.Getuid() != 0 {
		fmt.Fprintln(os.Stderr, "template-bench: must run as root (KVM + jailer require it)")
		os.Exit(2)
	}

	ctx := context.Background()
	if err := os.MkdirAll(*stateDir, 0o755); err != nil {
		fatal("mkdir state-dir: %v", err)
	}
	store, _ := sandboxd.NewStore("")
	vmm := resolveVMMBinary()
	fmt.Fprintf(os.Stderr, "template-bench: resolved VMMBinary=%s\n", vmm)
	mgr := &sandboxd.Manager{
		Store:        store,
		StateDir:     *stateDir,
		VMMBinary:    vmm,
		JailerBinary: vmm,
	}

	req := sandboxd.CreateTemplateRequest{
		Image:      *image,
		KernelPath: *kernel,
		MemMB:      *memMB,
	}

	fmt.Printf("template-bench: image=%s kernel=%s\n", *image, *kernel)
	fmt.Printf("first build (cold-boot + WarmCapture)...\n")
	t0 := time.Now()
	first, err := mgr.CreateTemplate(ctx, req)
	if err != nil {
		fatal("first CreateTemplate: %v", err)
	}
	if first.CacheHit {
		fmt.Printf("  WARNING: first build was a cache hit — likely a stale snapshot\n")
		fmt.Printf("           from a prior run. Delete /tmp/template-bench-state and\n")
		fmt.Printf("           ~/.cache/gocracker/snapshots/* for a clean run.\n")
	}
	fmt.Printf("  ok id=%s spec_hash=%s elapsed=%s\n",
		first.Template.ID, first.Template.SpecHash[:12], time.Since(t0).Round(time.Millisecond))

	fmt.Printf("\ncache-hit iterations (%d):\n", *iterations)
	var samples []time.Duration
	for i := 0; i < *iterations; i++ {
		t := time.Now()
		res, err := mgr.CreateTemplate(ctx, req)
		elapsed := time.Since(t)
		if err != nil {
			fatal("iter %d CreateTemplate: %v", i, err)
		}
		if !res.CacheHit {
			fatal("iter %d unexpectedly cold-booted (cache-hit logic broken?)", i)
		}
		samples = append(samples, elapsed)
		fmt.Printf("  [%d] id=%s elapsed=%s\n", i+1, res.Template.ID, elapsed.Round(time.Microsecond))
	}

	// Stats.
	var maxD, sumD time.Duration
	for _, d := range samples {
		if d > maxD {
			maxD = d
		}
		sumD += d
	}
	mean := sumD / time.Duration(len(samples))

	fmt.Println("\n================================================================")
	fmt.Printf("template-bench: %d cache-hit iterations\n", *iterations)
	fmt.Println("----------------------------------------------------------------")
	fmt.Printf("max   %5d µs (budget %d ms = %d µs)\n", maxD.Microseconds(), *cacheBudgetMs, *cacheBudgetMs*1000)
	fmt.Printf("mean  %5d µs\n", mean.Microseconds())
	fmt.Println("================================================================")

	// Cleanup: leave the snapshot in the cache (next run is faster)
	// but delete the registry entries so we don't pile up.
	for _, t := range mgr.ListTemplates() {
		_ = mgr.DeleteTemplate(t.ID)
	}

	if maxD > time.Duration(*cacheBudgetMs)*time.Millisecond {
		fmt.Printf("\nFAIL: max=%d µs > budget=%d ms\n", maxD.Microseconds(), *cacheBudgetMs)
		os.Exit(1)
	}
	fmt.Printf("\nPASS\n")
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "template-bench: "+format+"\n", args...)
	os.Exit(1)
}

// resolveVMMBinary finds the `gocracker` binary to spawn VMM workers.
// Same logic as sandboxd's main: sibling of this binary, else PATH.
// We never fall back to os.Executable() itself — this binary is
// template-bench, not gocracker.
func resolveVMMBinary() string {
	if self, err := os.Executable(); err == nil {
		// Walk up the tree looking for `gocracker` — handy in dev
		// where /tmp/template-bench sits next to nothing. Check
		// $PWD/gocracker first since that's where make build puts it.
		for _, candidate := range []string{
			filepath.Join(filepath.Dir(self), "gocracker"),
			filepath.Join(mustCwd(), "gocracker"),
			filepath.Join(mustCwd(), "bin", "gocracker"),
		} {
			if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
				return candidate
			}
		}
	}
	return "gocracker"
}

func mustCwd() string {
	d, _ := os.Getwd()
	return d
}
