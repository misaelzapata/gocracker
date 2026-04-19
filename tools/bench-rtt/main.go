// bench-rtt: measures round-trip times for individual primitives that make up
// the gocracker warm-cache hot path. Unlike pool-bench (end-to-end exec
// latency) or bench-node-tti.sh (full cold-boot), this tool isolates each
// primitive so regressions can be attributed to a specific code path.
//
// Usage:
//
//	sudo go run ./tools/bench-rtt \
//	  -image alpine:3.20 \
//	  -kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux \
//	  -iter 50 -warmups 3 -output /tmp/bench-rtt.json
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/gocracker/gocracker/pkg/container"
	"github.com/gocracker/gocracker/pkg/vmm"
	"github.com/gocracker/gocracker/pkg/warmcache"
)

type metric struct {
	Name    string    `json:"name"`
	N       int       `json:"n"`
	MinMS   float64   `json:"min_ms"`
	P50MS   float64   `json:"p50_ms"`
	P90MS   float64   `json:"p90_ms"`
	MeanMS  float64   `json:"mean_ms"`
	MaxMS   float64   `json:"max_ms"`
	Samples []float64 `json:"samples_ms,omitempty"`
}

func main() {
	image := flag.String("image", "alpine:3.20", "OCI image")
	kernel := flag.String("kernel", "./artifacts/kernels/gocracker-guest-standard-vmlinux", "kernel path")
	iter := flag.Int("iter", 50, "samples per metric")
	warmups := flag.Int("warmups", 3, "warmup iterations (not recorded)")
	memMB := flag.Int("mem", 256, "guest memory MiB")
	output := flag.String("output", "", "optional JSON output path")
	flag.Parse()

	fmt.Printf("bench-rtt: image=%s iter=%d warmups=%d\n\n", *image, *iter, *warmups)

	// Boot one VM that will be reused for Pause/Resume/Snapshot metrics.
	fmt.Print("cold-booting template VM... ")
	t0 := time.Now()
	opts := container.RunOptions{
		Image:       *image,
		KernelPath:  *kernel,
		MemMB:       uint64(*memMB),
		CPUs:        1,
		ExecEnabled: true,
		JailerMode:  container.JailerModeOff,
	}
	result, err := container.Run(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: cold boot: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("ok (%s)\n\n", time.Since(t0).Round(time.Millisecond))
	defer func() { result.VM.Stop() }()

	// Capture a canonical snapshot once for the restore metric.
	canonSnap, err := os.MkdirTemp("", "bench-rtt-canon-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mktemp: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(canonSnap)
	fmt.Print("taking canonical snapshot... ")
	if _, err := result.VM.TakeSnapshot(canonSnap); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: snapshot: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("ok")
	fmt.Println()

	results := make([]metric, 0, 6)

	// 1. Pause RTT.
	results = append(results, measure("pause_rtt", *iter, *warmups, func() (time.Duration, error) {
		t := time.Now()
		if err := result.VM.Pause(); err != nil {
			return 0, err
		}
		d := time.Since(t)
		if err := result.VM.Resume(); err != nil {
			return 0, err
		}
		return d, nil
	}))

	// 2. Resume RTT.
	results = append(results, measure("resume_rtt", *iter, *warmups, func() (time.Duration, error) {
		if err := result.VM.Pause(); err != nil {
			return 0, err
		}
		t := time.Now()
		if err := result.VM.Resume(); err != nil {
			return 0, err
		}
		return time.Since(t), nil
	}))

	// 3. Snapshot capture RTT — fresh tmpdir per iter.
	var snapDirs []string
	defer func() {
		for _, d := range snapDirs {
			os.RemoveAll(d)
		}
	}()
	results = append(results, measure("snapshot_capture_rtt", *iter, *warmups, func() (time.Duration, error) {
		d, err := os.MkdirTemp("", "bench-snap-*")
		if err != nil {
			return 0, err
		}
		snapDirs = append(snapDirs, d)
		t := time.Now()
		if _, err := result.VM.TakeSnapshot(d); err != nil {
			return 0, err
		}
		return time.Since(t), nil
	}))

	// 4. Warm-cache lookup miss RTT.
	root := warmcache.DefaultRoot()
	results = append(results, measure("warmcache_miss_rtt", *iter, *warmups, func() (time.Duration, error) {
		var b [16]byte
		_, _ = rand.Read(b[:])
		k := "miss-" + hex.EncodeToString(b[:])
		t := time.Now()
		_, hit := warmcache.Lookup(root, k)
		d := time.Since(t)
		if hit {
			return 0, fmt.Errorf("unexpected hit on random key")
		}
		return d, nil
	}))

	// 5. Warm-cache lookup hit RTT — stage a fake complete entry.
	hitKey := "bench-rtt-hit-key-0000000000000000000000000000000000000000000000000000000000000001"
	hitSrc, err := os.MkdirTemp("", "bench-hit-src-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mktemp: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(filepath.Join(hitSrc, "snapshot.json"), []byte(`{"stub":true}`), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write stub snapshot.json: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(filepath.Join(hitSrc, "mem.bin"), []byte("stub"), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write stub mem.bin: %v\n", err)
		os.Exit(1)
	}
	if err := warmcache.Store(hitSrc, root, hitKey); err != nil {
		fmt.Fprintf(os.Stderr, "store stub entry: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(warmcache.Dir(root, hitKey))

	results = append(results, measure("warmcache_hit_rtt", *iter, *warmups, func() (time.Duration, error) {
		t := time.Now()
		_, hit := warmcache.Lookup(root, hitKey)
		d := time.Since(t)
		if !hit {
			return 0, fmt.Errorf("expected hit on staged key")
		}
		return d, nil
	}))

	// 6. Snapshot restore RTT.
	results = append(results, measure("snapshot_restore_rtt", *iter, *warmups, func() (time.Duration, error) {
		t := time.Now()
		vm, err := vmm.RestoreFromSnapshotWithOptions(canonSnap, vmm.RestoreOptions{})
		if err != nil {
			return 0, err
		}
		if err := vm.Start(); err != nil {
			vm.Stop()
			return 0, err
		}
		d := time.Since(t)
		vm.Stop()
		return d, nil
	}))

	// Print.
	fmt.Println()
	for _, m := range results {
		fmt.Printf("%-22s min/p50/p90/mean/max = %6.2fms / %6.2fms / %6.2fms / %6.2fms / %6.2fms  (N=%d)\n",
			m.Name, m.MinMS, m.P50MS, m.P90MS, m.MeanMS, m.MaxMS, m.N)
	}

	// JSON summary line.
	compact := make([]metric, len(results))
	for i, m := range results {
		mc := m
		mc.Samples = nil
		compact[i] = mc
	}
	j, _ := json.Marshal(compact)
	fmt.Printf("\nJSON:%s\n", string(j))

	if *output != "" {
		full, _ := json.MarshalIndent(results, "", "  ")
		if err := os.WriteFile(*output, append(full, '\n'), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", *output, err)
		} else {
			fmt.Printf("wrote %s\n", *output)
		}
	}
}

func measure(name string, iter, warmups int, fn func() (time.Duration, error)) metric {
	samples := make([]float64, 0, iter)
	for i := 0; i < warmups+iter; i++ {
		d, err := fn()
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s iter %d FAIL: %v\n", name, i, err)
			continue
		}
		if i < warmups {
			continue
		}
		samples = append(samples, float64(d)/float64(time.Millisecond))
	}
	if len(samples) == 0 {
		return metric{Name: name}
	}
	sorted := make([]float64, len(samples))
	copy(sorted, samples)
	sort.Float64s(sorted)

	var sum float64
	for _, v := range sorted {
		sum += v
	}
	p := func(q float64) float64 {
		idx := int(q * float64(len(sorted)-1))
		return sorted[idx]
	}
	return metric{
		Name:    name,
		N:       len(sorted),
		MinMS:   sorted[0],
		P50MS:   p(0.50),
		P90MS:   p(0.90),
		MeanMS:  sum / float64(len(sorted)),
		MaxMS:   sorted[len(sorted)-1],
		Samples: sorted,
	}
}
