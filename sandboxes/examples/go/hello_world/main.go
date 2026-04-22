// Cookbook 1/N (Go): create + exec `echo hello`.
//
// Usage:
//
//	sudo go run ./sandboxes/examples/go/hello_world [KERNEL_PATH]
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	gocracker "github.com/gocracker/gocracker/sandboxes/sdk/go"
)

func main() {
	kernel := "/home/misael/Desktop/projects/gocracker/artifacts/kernels/gocracker-guest-standard-vmlinux"
	if len(os.Args) > 1 {
		kernel = os.Args[1]
	}
	ctx := context.Background()
	client := gocracker.NewClient("http://127.0.0.1:9091")

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
