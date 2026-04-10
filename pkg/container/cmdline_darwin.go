//go:build darwin

package container

import "github.com/gocracker/gocracker/internal/runtimecfg"

func platformKernelArgs(withSerialConsole, allowKernelModules bool) []string {
	return runtimecfg.DarwinKernelArgsForRuntime(withSerialConsole, allowKernelModules)
}
