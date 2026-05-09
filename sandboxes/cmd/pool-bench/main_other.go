//go:build !linux

// pool-bench is a KVM-coupled benchmark. Cross-compile stub.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "pool-bench: Linux-only.")
	os.Exit(2)
}
