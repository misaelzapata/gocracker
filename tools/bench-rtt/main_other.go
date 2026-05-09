//go:build !linux

// bench-rtt benchmarks gocracker microVM round-trip primitives. The
// underlying paths are KVM-coupled, so the benchmark is Linux-only;
// on other platforms this stub exists for cross-compile completeness.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "bench-rtt: Linux-only (KVM-coupled benchmark).")
	os.Exit(2)
}
