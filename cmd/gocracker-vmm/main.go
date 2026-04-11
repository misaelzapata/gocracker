package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/gocracker/gocracker/internal/vmmserver"
	"github.com/gocracker/gocracker/pkg/vmm"
)

func main() {
	socketPath := flag.String("socket", "/tmp/gocracker-vmm.sock", "Unix socket path to listen on")
	defaultBoot := flag.String("default-x86-boot", string(vmm.X86BootAuto), "default x86 boot mode: auto, acpi, legacy")
	vmID := flag.String("vm-id", "", "VM identifier used for worker-backed launches")
	flag.Parse()

	srv := vmmserver.NewWithOptions(vmmserver.Options{
		DefaultX86Boot: vmm.X86BootMode(*defaultBoot),
		VMID:           *vmID,
	})

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		srv.Close()
		os.Exit(0)
	}()

	if err := srv.ListenUnix(*socketPath); err != nil {
		log.Fatal(fmt.Errorf("listen unix socket %s: %w", *socketPath, err))
	}
}
