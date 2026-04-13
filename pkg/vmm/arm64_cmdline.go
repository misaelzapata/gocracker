package vmm

import (
	"strings"

	"github.com/gocracker/gocracker/internal/runtimecfg"
)

const arm64EarlyconArg = "earlycon=uart,mmio,0x40002000"

func normalizeARM64KernelCmdline(cmdline string) string {
	cmdline = strings.TrimSpace(cmdline)
	if cmdline == "" {
		cmdline = strings.Join(runtimecfg.DefaultKernelArgsForRuntime(true, false), " ")
	}

	fields := strings.Fields(stripARM64IrrelevantX86Args(cmdline))
	if !containsKernelArg(fields, "keep_bootcon") {
		fields = append(fields, "keep_bootcon")
	}
	if !containsKernelArgPrefix(fields, "earlycon=") {
		fields = append(fields, arm64EarlyconArg)
	}
	return strings.Join(fields, " ")
}

func stripARM64IrrelevantX86Args(cmdline string) string {
	parts := strings.Fields(cmdline)
	filtered := parts[:0]
	for _, p := range parts {
		if strings.HasPrefix(p, "i8042.") ||
			strings.HasPrefix(p, "8250.") ||
			p == "pci=off" {
			continue
		}
		filtered = append(filtered, p)
	}
	return strings.Join(filtered, " ")
}

func containsKernelArg(fields []string, want string) bool {
	for _, field := range fields {
		if field == want {
			return true
		}
	}
	return false
}

func containsKernelArgPrefix(fields []string, prefix string) bool {
	for _, field := range fields {
		if strings.HasPrefix(field, prefix) {
			return true
		}
	}
	return false
}
