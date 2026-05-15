//go:build !linux && !windows

package main

import "runtime"

func runtimeOSName() string { return runtime.GOOS }
