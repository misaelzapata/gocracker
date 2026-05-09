//go:build !linux

// template-bench is a KVM-coupled benchmark. Cross-compile stub.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "template-bench: Linux-only.")
	os.Exit(2)
}
