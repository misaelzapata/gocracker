//go:build linux

package main

import (
	"fmt"
	"os"

	"github.com/gocracker/gocracker/internal/jailer"
)

func main() {
	if err := jailer.RunCLI(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
