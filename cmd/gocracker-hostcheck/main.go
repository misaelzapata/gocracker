package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/gocracker/gocracker/internal/hostguard"
)

func main() {
	needKVM := flag.Bool("kvm", true, "require /dev/kvm")
	needTun := flag.Bool("tun", true, "require /dev/net/tun")
	checkPTY := flag.Bool("pty", true, "require usable host PTY support")
	flag.Parse()

	req := hostguard.DeviceRequirements{
		NeedKVM: *needKVM,
		NeedTun: *needTun,
	}
	if err := hostguard.CheckHostDevices(req); err != nil {
		fmt.Fprintf(os.Stderr, "host devices: %v\n", err)
		os.Exit(1)
	}
	if *checkPTY {
		if err := hostguard.CheckPTYSupport(); err != nil {
			fmt.Fprintf(os.Stderr, "pty: %v\n", err)
			os.Exit(1)
		}
	}
	fmt.Println("hostcheck: ok")
}
