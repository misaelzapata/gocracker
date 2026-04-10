//go:build darwin

// darwin-compose-smoke tests Docker Compose on macOS.
package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/gocracker/gocracker/internal/compose"
)

func main() {
	composePath := os.Getenv("COMPOSE_FILE")
	if composePath == "" {
		composePath = "/tmp/gocracker-compose-test/docker-compose.yml"
	}
	kernel := os.Getenv("KERNEL")
	if kernel == "" {
		candidates := []string{
			"artifacts/kernels/gocracker-guest-standard-arm64-Image",
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				kernel = c
				break
			}
		}
	}
	if kernel == "" {
		fmt.Fprintln(os.Stderr, "error: no kernel found")
		os.Exit(1)
	}

	fmt.Printf("=== gocracker compose smoke test ===\n")
	fmt.Printf("Host:    %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Printf("Compose: %s\n", composePath)
	fmt.Printf("Kernel:  %s\n\n", kernel)

	stack, err := compose.Up(compose.RunOptions{
		ComposePath: composePath,
		KernelPath:  kernel,
		DefaultMem:  256,
		DefaultDisk: 512,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "compose.Up: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("\nStack started. Services:")
	for name, state := range stack.Status() {
		fmt.Printf("  %s: %s\n", name, state)
	}

	fmt.Println("\nWaiting 15s for services...")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	<-ctx.Done()

	fmt.Println("\nStopping stack...")
	stack.Down()
	fmt.Println("Done.")
}
