package worker

import (
	"slices"
	"testing"
)

func TestAppendForwardedWorkerEnvFlagsIncludesSeccompOverride(t *testing.T) {
	t.Setenv("GOCRACKER_SECCOMP", "off")

	args := appendForwardedWorkerEnvFlags([]string{"--id", "vm-123"})

	want := []string{"--id", "vm-123", "--env", "GOCRACKER_SECCOMP=off"}
	if !slices.Equal(args, want) {
		t.Fatalf("appendForwardedWorkerEnvFlags() = %#v, want %#v", args, want)
	}
}

func TestInsertForwardedWorkerEnvFlagsKeepsEnvBeforeWorkerCommand(t *testing.T) {
	t.Setenv("GOCRACKER_SECCOMP", "off")

	args := insertForwardedWorkerEnvFlags([]string{
		"--id", "snap-123",
		"--uid", "1000",
		"--gid", "1000",
		"--",
		"vmm", "--socket", "/worker/vmm.sock",
	}, 6)

	want := []string{
		"--id", "snap-123",
		"--uid", "1000",
		"--gid", "1000",
		"--env", "GOCRACKER_SECCOMP=off",
		"--",
		"vmm", "--socket", "/worker/vmm.sock",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("insertForwardedWorkerEnvFlags() = %#v, want %#v", args, want)
	}
}
