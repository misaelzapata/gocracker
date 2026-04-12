//go:build linux && amd64

package seccomp

import (
	"testing"

	"golang.org/x/sys/unix"
)

// ---------- buildProgram ----------

func TestBuildProgramEmpty(t *testing.T) {
	_, err := buildProgram(nil)
	if err == nil {
		t.Fatal("expected error for empty syscall list")
	}
}

func TestBuildProgramStructure(t *testing.T) {
	prog, err := buildProgram([]uintptr{unix.SYS_READ, unix.SYS_WRITE})
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	// Structure: arch check (3) + load syscall nr (1) + 2 syscalls * 2 (check+allow) + final kill (1)
	expectedMin := 3 + 1 + 2*2 + 1
	if len(prog) < expectedMin {
		t.Fatalf("program too short: %d instructions, want >= %d", len(prog), expectedMin)
	}

	// First instruction: load arch
	if prog[0].Code != unix.BPF_LD|unix.BPF_W|unix.BPF_ABS || prog[0].K != seccompDataArch {
		t.Fatalf("first instruction should load arch, got code=%#x k=%d", prog[0].Code, prog[0].K)
	}

	// Second instruction: compare to x86_64 arch
	if prog[1].K != unix.AUDIT_ARCH_X86_64 {
		t.Fatalf("arch check should compare to x86_64, got %#x", prog[1].K)
	}

	// Third instruction: kill on wrong arch
	if prog[2].K != unix.SECCOMP_RET_KILL_PROCESS {
		t.Fatalf("wrong arch action should be kill, got %#x", prog[2].K)
	}

	// Fourth instruction: load syscall number
	if prog[3].Code != unix.BPF_LD|unix.BPF_W|unix.BPF_ABS || prog[3].K != seccompDataNR {
		t.Fatalf("4th instruction should load syscall nr, got code=%#x k=%d", prog[3].Code, prog[3].K)
	}

	// Last instruction: default kill
	last := prog[len(prog)-1]
	if last.K != unix.SECCOMP_RET_KILL_PROCESS {
		t.Fatalf("last instruction should be kill, got %#x", last.K)
	}
}

func TestBuildProgramDeduplicatesSyscalls(t *testing.T) {
	prog1, _ := buildProgram([]uintptr{unix.SYS_READ, unix.SYS_WRITE})
	prog2, _ := buildProgram([]uintptr{unix.SYS_READ, unix.SYS_READ, unix.SYS_WRITE, unix.SYS_WRITE})

	if len(prog1) != len(prog2) {
		t.Fatalf("duplicate syscalls should produce same program: %d != %d", len(prog1), len(prog2))
	}
}

// ---------- profileSyscalls ----------

func TestProfileSyscallsAPI(t *testing.T) {
	sc := profileSyscalls(ProfileAPI)
	if len(sc) == 0 {
		t.Fatal("API profile should have syscalls")
	}
	// Should contain basic runtime + file + net
	found := map[uintptr]bool{}
	for _, nr := range sc {
		found[nr] = true
	}
	if !found[unix.SYS_READ] {
		t.Fatal("API profile should include SYS_READ")
	}
	if !found[unix.SYS_SOCKET] {
		t.Fatal("API profile should include SYS_SOCKET")
	}
	if !found[unix.SYS_OPENAT] {
		t.Fatal("API profile should include SYS_OPENAT")
	}
}

func TestProfileSyscallsVMM(t *testing.T) {
	sc := profileSyscalls(ProfileVMM)
	if len(sc) == 0 {
		t.Fatal("VMM profile should have syscalls")
	}
	found := map[uintptr]bool{}
	for _, nr := range sc {
		found[nr] = true
	}
	// VMM should include vm-specific syscalls
	if !found[unix.SYS_MEMFD_CREATE] {
		t.Fatal("VMM profile should include SYS_MEMFD_CREATE")
	}
}

func TestProfileSyscallsVCPU(t *testing.T) {
	sc := profileSyscalls(ProfileVCPU)
	if len(sc) == 0 {
		t.Fatal("vCPU profile should have syscalls")
	}
	found := map[uintptr]bool{}
	for _, nr := range sc {
		found[nr] = true
	}
	if !found[unix.SYS_IOCTL] {
		t.Fatal("vCPU profile should include SYS_IOCTL")
	}
}

func TestProfileSyscallsUnknown(t *testing.T) {
	sc := profileSyscalls(Profile("nonexistent"))
	if sc != nil {
		t.Fatalf("unknown profile should return nil, got %d syscalls", len(sc))
	}
}

// ---------- vmSyscalls / vcpuSyscalls ----------

func TestVmSyscalls(t *testing.T) {
	sc := vmSyscalls()
	if len(sc) == 0 {
		t.Fatal("vmSyscalls should not be empty")
	}
	found := false
	for _, nr := range sc {
		if nr == unix.SYS_MEMFD_CREATE {
			found = true
		}
	}
	if !found {
		t.Fatal("vmSyscalls should include MEMFD_CREATE")
	}
}

func TestVcpuSyscalls(t *testing.T) {
	sc := vcpuSyscalls()
	if len(sc) == 0 {
		t.Fatal("vcpuSyscalls should not be empty")
	}
}

// ---------- uniqueSorted ----------

func TestUniqueSorted(t *testing.T) {
	input := []uintptr{5, 3, 1, 3, 5, 2, 1}
	got := uniqueSorted(input)
	want := []uintptr{1, 2, 3, 5}
	if len(got) != len(want) {
		t.Fatalf("uniqueSorted len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("uniqueSorted[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}

func TestUniqueSortedEmpty(t *testing.T) {
	got := uniqueSorted(nil)
	if len(got) != 0 {
		t.Fatalf("uniqueSorted(nil) len = %d, want 0", len(got))
	}
}

// ---------- union ----------

func TestUnion(t *testing.T) {
	a := []uintptr{1, 2, 3}
	b := []uintptr{4, 5}
	c := []uintptr{6}
	got := union(a, b, c)
	if len(got) != 6 {
		t.Fatalf("union len = %d, want 6", len(got))
	}
}

func TestUnionEmpty(t *testing.T) {
	got := union()
	if len(got) != 0 {
		t.Fatalf("union() len = %d, want 0", len(got))
	}
}

// ---------- disabled ----------

func TestDisabledVariousValues(t *testing.T) {
	tests := []struct {
		envVal string
		want   bool
	}{
		{"0", true},
		{"off", true},
		{"false", true},
		{"disabled", true},
		{"OFF", true},
		{"FALSE", true},
		{" off ", true},
		{"1", false},
		{"on", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Setenv("GOCRACKER_SECCOMP", tt.envVal)
		got := disabled()
		if got != tt.want {
			t.Errorf("disabled() with GOCRACKER_SECCOMP=%q = %v, want %v", tt.envVal, got, tt.want)
		}
	}
}

// ---------- ProfileProgram for all profiles ----------

func TestProfileProgramAPI(t *testing.T) {
	prog, err := ProfileProgram(ProfileAPI)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(prog) < 6 {
		t.Fatalf("program too short: %d", len(prog))
	}
}

func TestProfileProgramVMM(t *testing.T) {
	prog, err := ProfileProgram(ProfileVMM)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(prog) < 6 {
		t.Fatalf("program too short: %d", len(prog))
	}
	// VMM profile should include MEMFD_CREATE
	if !profileProgramIncludesSyscall(prog, uint32(unix.SYS_MEMFD_CREATE)) {
		t.Fatal("VMM profile should include MEMFD_CREATE")
	}
}

func TestProfileProgramVMMIncludesAllFromAPI(t *testing.T) {
	apiProg, _ := ProfileProgram(ProfileAPI)
	vmmProg, _ := ProfileProgram(ProfileVMM)

	// VMM should be a superset of API
	if len(vmmProg) < len(apiProg) {
		t.Fatalf("VMM program (%d instrs) should be >= API program (%d instrs)", len(vmmProg), len(apiProg))
	}
}

// ---------- InstallWorkerProcessProfile / InstallThreadProfile with disabled ----------

func TestInstallWorkerProcessProfileDisabled(t *testing.T) {
	t.Setenv("GOCRACKER_SECCOMP", "off")
	err := InstallWorkerProcessProfile()
	if err != nil {
		t.Fatalf("InstallWorkerProcessProfile with disabled should return nil, got %v", err)
	}
}

func TestInstallThreadProfileDisabled(t *testing.T) {
	t.Setenv("GOCRACKER_SECCOMP", "off")
	err := InstallThreadProfile(ProfileVCPU)
	if err != nil {
		t.Fatalf("InstallThreadProfile with disabled should return nil, got %v", err)
	}
}

// ---------- stmt / jump helpers ----------

func TestStmtAndJumpConstruction(t *testing.T) {
	s := stmt(0x20, 42)
	if s.Code != 0x20 || s.K != 42 {
		t.Fatalf("stmt: code=%#x k=%d", s.Code, s.K)
	}

	j := jump(0x15, 100, 1, 0)
	if j.Code != 0x15 || j.K != 100 || j.Jt != 1 || j.Jf != 0 {
		t.Fatalf("jump: code=%#x k=%d jt=%d jf=%d", j.Code, j.K, j.Jt, j.Jf)
	}
}
