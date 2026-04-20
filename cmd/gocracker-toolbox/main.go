package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/gocracker/gocracker/internal/toolbox/agent"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: toolboxguest <serve|spawn> [flags]")
		os.Exit(2)
	}

	switch os.Args[1] {
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		port := fs.Uint("vsock-port", 10023, "vsock port to listen on")
		fs.Parse(os.Args[2:])

		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if err := agent.Serve(ctx, uint32(*port)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "spawn":
		fs := flag.NewFlagSet("spawn", flag.ExitOnError)
		port := fs.Uint("vsock-port", 10023, "vsock port to listen on")
		logFile := fs.String("log-file", "/tmp/gocracker-toolbox.log", "log file path")
		fs.Parse(os.Args[2:])
		if err := spawnDetached(uint32(*port), *logFile); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: toolboxguest <serve|spawn> [flags]")
		os.Exit(2)
	}
}

func spawnDetached(port uint32, logFile string) error {
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		return err
	}
	logHandle, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer logHandle.Close()

	cmd := exec.Command(os.Args[0], "serve", "--vsock-port", strconv.FormatUint(uint64(port), 10))
	cmd.Stdout = logHandle
	cmd.Stderr = logHandle
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
