//go:build linux && amd64

package seccomp

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestProfileProgramIncludesArchGuardAndAllowsSyscalls(t *testing.T) {
	prog, err := ProfileProgram(ProfileVCPU)
	if err != nil {
		t.Fatalf("profile program: %v", err)
	}
	if len(prog) < 6 {
		t.Fatalf("program too short: %d", len(prog))
	}
	if prog[0].K != seccompDataArch {
		t.Fatalf("first instruction offset = %d, want %d", prog[0].K, seccompDataArch)
	}
	if prog[len(prog)-1].K != unix.SECCOMP_RET_KILL_PROCESS {
		t.Fatalf("final action = %#x, want kill", prog[len(prog)-1].K)
	}
	allowFound := false
	for _, insn := range prog {
		if insn.K == uint32(unix.SYS_IOCTL) {
			allowFound = true
			break
		}
	}
	if !allowFound {
		t.Fatal("expected ioctl syscall in vcpu profile")
	}
	rseqFound := false
	for _, insn := range prog {
		if insn.K == uint32(unix.SYS_RSEQ) {
			rseqFound = true
			break
		}
	}
	if !rseqFound {
		t.Fatal("expected rseq syscall in vcpu profile")
	}
	clone3Found := false
	for _, insn := range prog {
		if insn.K == uint32(unix.SYS_CLONE3) {
			clone3Found = true
			break
		}
	}
	if !clone3Found {
		t.Fatal("expected clone3 syscall in vcpu profile")
	}
	fsyncFound := false
	for _, insn := range prog {
		if insn.K == uint32(unix.SYS_FSYNC) {
			fsyncFound = true
			break
		}
	}
	if !fsyncFound {
		t.Fatal("expected fsync syscall in vcpu profile")
	}
	fcntlFound := false
	for _, insn := range prog {
		if insn.K == uint32(unix.SYS_FCNTL) {
			fcntlFound = true
			break
		}
	}
	if !fcntlFound {
		t.Fatal("expected fcntl syscall in vcpu profile")
	}
}

func TestDisabledEnvSwitch(t *testing.T) {
	t.Setenv("GOCRACKER_SECCOMP", "off")
	if !disabled() {
		t.Fatal("expected seccomp env switch to disable filters")
	}
}
