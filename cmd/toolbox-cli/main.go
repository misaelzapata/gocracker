// toolbox-cli is a tiny demo / debugging tool that uses the host
// internal/toolbox/client to talk to a running gocracker VM's
// toolbox agent over its UDS. Useful for smoke-testing the
// CONNECT 10023 + framed exec path without booting socat by hand.
//
// Usage:
//
//	toolbox-cli health -uds /path/to/vm.sock
//	toolbox-cli exec   -uds /path/to/vm.sock [-tty] -- cmd arg arg ...
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gocracker/gocracker/internal/toolbox/agent"
	"github.com/gocracker/gocracker/internal/toolbox/client"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "health":
		cmdHealth(os.Args[2:])
	case "exec":
		cmdExec(os.Args[2:])
	case "setnetwork":
		cmdSetNetwork(os.Args[2:])
	default:
		usage()
	}
}

func cmdSetNetwork(args []string) {
	fs := flag.NewFlagSet("setnetwork", flag.ExitOnError)
	uds := fs.String("uds", "", "absolute path to the VM's UDS")
	iface := fs.String("iface", "eth0", "guest interface name")
	ip := fs.String("ip", "", "CIDR (e.g. 10.100.7.2/30)")
	gw := fs.String("gw", "", "gateway IP (e.g. 10.100.7.1)")
	mac := fs.String("mac", "", "MAC address (optional)")
	timeout := fs.Duration("timeout", 5*time.Second, "request timeout")
	fs.Parse(args)
	if *uds == "" || *ip == "" {
		fmt.Fprintln(os.Stderr, "usage: toolbox-cli setnetwork -uds <path> -ip <CIDR> [-gw <ip>] [-iface eth0] [-mac <mac>]")
		os.Exit(2)
	}
	c := &client.Client{UDSPath: *uds}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	resp, err := c.SetNetwork(ctx, agent.SetNetworkRequest{
		Interface: *iface, IP: *ip, Gateway: *gw, MAC: *mac,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "setnetwork:", err)
		os.Exit(1)
	}
	fmt.Printf("ok=%v iface=%s ip=%s gw=%s mac=%s\n", resp.OK, resp.Interface, resp.IP, resp.Gateway, resp.MAC)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: toolbox-cli <health|exec> -uds <path> [flags]")
	os.Exit(2)
}

func cmdHealth(args []string) {
	fs := flag.NewFlagSet("health", flag.ExitOnError)
	uds := fs.String("uds", "", "absolute path to the VM's UDS")
	timeout := fs.Duration("timeout", 5*time.Second, "dial timeout")
	fs.Parse(args)
	if *uds == "" {
		fmt.Fprintln(os.Stderr, "-uds is required")
		os.Exit(2)
	}
	c := &client.Client{UDSPath: *uds, DialTimeout: *timeout}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	h, err := c.Health(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "health:", err)
		os.Exit(1)
	}
	fmt.Printf("ok=%v version=%s\n", h.OK, h.Version)
}

func cmdExec(args []string) {
	fs := flag.NewFlagSet("exec", flag.ExitOnError)
	uds := fs.String("uds", "", "absolute path to the VM's UDS")
	tty := fs.Bool("tty", false, "allocate a PTY")
	workdir := fs.String("workdir", "", "guest working directory")
	timeout := fs.Duration("timeout", 0, "overall exec timeout (0 = none)")
	fs.Parse(args)
	if *uds == "" || fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: toolbox-cli exec -uds <path> [-tty] -- cmd arg arg ...")
		os.Exit(2)
	}
	c := &client.Client{UDSPath: *uds}
	ctx := context.Background()
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}

	req := agent.ExecRequest{
		Cmd:     fs.Args(),
		WorkDir: *workdir,
		TTY:     *tty,
	}

	// Forward Ctrl-C / SIGTERM to the guest process via the agent's
	// signal channel. The Stream API exposes this directly.
	sess, err := c.Stream(ctx, req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "stream:", err)
		os.Exit(1)
	}
	defer sess.Close()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for s := range sigCh {
			_ = sess.Signal(s.(syscall.Signal))
		}
	}()

	// Forward our stdin to the agent. Best-effort — tiny CLI, no
	// raw-mode TTY handling.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				_, _ = sess.Write(buf[:n])
			}
			if err != nil {
				_ = sess.CloseStdin()
				return
			}
		}
	}()

	for {
		ch, payload, err := sess.NextFrame()
		if err != nil {
			fmt.Fprintln(os.Stderr, "next frame:", err)
			os.Exit(1)
		}
		switch ch {
		case agent.ChannelStdout:
			os.Stdout.Write(payload)
		case agent.ChannelStderr:
			os.Stderr.Write(payload)
		case agent.ChannelExit:
			code, err := agent.ParseExitPayload(payload)
			if err != nil {
				fmt.Fprintln(os.Stderr, "parse exit:", err)
				os.Exit(1)
			}
			if code < 0 {
				// Killed by signal — surface as 128+|sig| if we can,
				// otherwise just the raw negative.
				os.Exit(int(128 - code))
			}
			os.Exit(int(code))
		}
	}
}
