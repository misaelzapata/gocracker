package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/gocracker/gocracker/internal/hostguard"
)

var (
	checkHostDevices = hostguard.CheckHostDevices
	checkPTYSupport  = hostguard.CheckPTYSupport
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gocracker-hostcheck", flag.ContinueOnError)
	fs.SetOutput(stderr)

	needKVM := fs.Bool("kvm", true, "require /dev/kvm")
	needTun := fs.Bool("tun", true, "require /dev/net/tun")
	checkPTY := fs.Bool("pty", true, "require usable host PTY support")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	req := hostguard.DeviceRequirements{
		NeedKVM: *needKVM,
		NeedTun: *needTun,
	}
	if err := checkHostDevices(req); err != nil {
		fmt.Fprintf(stderr, "host devices: %v\n", err)
		return 1
	}
	if *checkPTY {
		if err := checkPTYSupport(); err != nil {
			fmt.Fprintf(stderr, "pty: %v\n", err)
			return 1
		}
	}
	fmt.Fprintln(stdout, "hostcheck: ok")
	return 0
}
