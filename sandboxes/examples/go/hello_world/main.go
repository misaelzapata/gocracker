// Cookbook 1/N (Go): create + exec `echo hello`.
//
// Kernel resolves from os.Args[1] -> $GOCRACKER_KERNEL -> repo default.
// Sandboxd URL from $GOCRACKER_SANDBOXD -> http://127.0.0.1:9091.
//
// Usage:
//
//	sudo go run ./sandboxes/examples/go/hello_world [KERNEL_PATH]
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	gocracker "github.com/gocracker/gocracker/sandboxes/sdk/go"
)

func resolveKernel() string {
	if len(os.Args) > 1 && os.Args[1] != "" {
		return os.Args[1]
	}
	if v := os.Getenv("GOCRACKER_KERNEL"); v != "" {
		return v
	}
	// Repo-relative default: this file is at
	// <repo>/sandboxes/examples/go/hello_world/main.go
	_, f, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(f), "..", "..", "..", "..")
	def := filepath.Join(repo, "artifacts", "kernels", "gocracker-guest-standard-vmlinux")
	if _, err := os.Stat(def); err == nil {
		return def
	}
	fmt.Fprintln(os.Stderr, "error: pass kernel path as arg 1 or set $GOCRACKER_KERNEL")
	os.Exit(2)
	return ""
}

func main() {
	kernel := resolveKernel()
	sandboxdURL := os.Getenv("GOCRACKER_SANDBOXD")
	if sandboxdURL == "" {
		sandboxdURL = "http://127.0.0.1:9091"
	}
	ctx := context.Background()
	client := gocracker.NewClient(sandboxdURL)

	ok, err := client.Healthz(ctx)
	if err != nil || !ok {
		fmt.Fprintln(os.Stderr, "sandboxd not reachable at 127.0.0.1:9091")
		os.Exit(1)
	}

	fmt.Printf("creating sandbox (alpine:3.20, kernel=%s)...\n", kernel)
	sb, err := client.CreateSandbox(ctx, gocracker.CreateSandboxRequest{
		Image:      "alpine:3.20",
		KernelPath: kernel,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "create:", err)
		os.Exit(1)
	}
	fmt.Printf("  id=%s guest_ip=%s uds=%s\n", sb.ID, sb.GuestIP, sb.UDSPath)

	defer func() {
		if err := sb.Delete(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "delete:", err)
		} else {
			fmt.Printf("deleted id=%s\n", sb.ID)
		}
	}()

	result, err := sb.Toolbox().Exec(ctx, []string{"echo", "hello from gocracker (Go)"})
	if err != nil {
		fmt.Fprintln(os.Stderr, "exec:", err)
		os.Exit(1)
	}
	fmt.Printf("exit=%d\n", result.ExitCode)
	fmt.Printf("stdout: %s\n", strings.TrimRight(string(result.Stdout), "\n"))
	if len(result.Stderr) > 0 {
		fmt.Printf("stderr: %s\n", strings.TrimRight(string(result.Stderr), "\n"))
	}
}
