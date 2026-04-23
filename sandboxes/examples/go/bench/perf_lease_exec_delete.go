package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	gocracker "github.com/gocracker/gocracker/sandboxes/sdk/go"
)

func main() {
	ctx := context.Background()
	c := gocracker.NewClient("http://127.0.0.1:9091")
	_ = c.UnregisterPool(ctx, "perfbench-go")
	_, _ = c.RegisterPool(ctx, gocracker.CreatePoolRequest{TemplateID: "perfbench-go", Image: "alpine:3.20", KernelPath: "/home/misael/Desktop/projects/gocracker/artifacts/kernels/gocracker-guest-standard-vmlinux", MinPaused: 8, MaxPaused: 8})
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		pools, _ := c.ListPools(ctx)
		for _, p := range pools {
			if p.TemplateID == "perfbench-go" && p.Counts["paused"] >= 6 {
				goto ready
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
ready:
	var lease, exec, del []float64
	for i := 0; i < 8; i++ {
		t0 := time.Now()
		sb, err := c.LeaseSandbox(ctx, gocracker.LeaseSandboxRequest{TemplateID: "perfbench-go"})
		if err != nil {
			fmt.Println("lease err:", err); continue
		}
		lease = append(lease, float64(time.Since(t0).Microseconds())/1000)
		t0 = time.Now()
		_, err = sb.Process().Exec(ctx, []string{"echo", "hi"})
		exec = append(exec, float64(time.Since(t0).Microseconds())/1000)
		t0 = time.Now()
		_ = sb.Delete(ctx)
		del = append(del, float64(time.Since(t0).Microseconds())/1000)
	}
	pr := func(name string, xs []float64) {
		sort.Float64s(xs)
		if len(xs) == 0 { fmt.Printf("  %-10s no samples\n", name); return }
		fmt.Printf("  %-10s min=%5.2f  p50=%5.2f  p95=%5.2f  max=%5.2f\n",
			name, xs[0], xs[len(xs)/2], xs[len(xs)-1], xs[len(xs)-1])
	}
	fmt.Println("go-sdk:")
	pr("lease", lease); pr("exec_echo", exec); pr("delete", del)
	_ = c.UnregisterPool(ctx, "perfbench-go")
}
