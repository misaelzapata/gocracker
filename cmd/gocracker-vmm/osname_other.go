//go:build !linux

package main

import "runtime"

func runtimeOSName() string { return runtime.GOOS }
