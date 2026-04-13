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
	for _, nr := range []uint32{
		uint32(unix.SYS_IOCTL),
		uint32(unix.SYS_RSEQ),
		uint32(unix.SYS_CLONE3),
		uint32(unix.SYS_FSYNC),
		uint32(unix.SYS_FCNTL),
		uint32(unix.SYS_KILL),
		uint32(unix.SYS_SETITIMER),
		uint32(unix.SYS_TIMER_CREATE),
		uint32(unix.SYS_TIMER_SETTIME),
		uint32(unix.SYS_TIMER_DELETE),
		uint32(unix.SYS_FACCESSAT),
	} {
		if !profileProgramIncludesSyscall(prog, nr) {
			t.Fatalf("expected syscall %d in vcpu profile", nr)
		}
	}
}

func profileProgramIncludesSyscall(prog []unix.SockFilter, nr uint32) bool {
	for _, insn := range prog {
		if insn.K == nr {
			return true
		}
	}
	return false
}

func TestDisabledEnvSwitch(t *testing.T) {
	t.Setenv("GOCRACKER_SECCOMP", "off")
	if !disabled() {
		t.Fatal("expected seccomp env switch to disable filters")
	}
}
