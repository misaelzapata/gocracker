//go:build linux && arm64

package seccomp

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	seccompDataNR   = 0
	seccompDataArch = 4
)

type Profile string

const (
	ProfileAPI  Profile = "api"
	ProfileVMM  Profile = "vmm"
	ProfileVCPU Profile = "vcpu"
)

func InstallWorkerProcessProfile() error {
	if disabled() {
		return nil
	}
	return installProfile(union(profileSyscalls(ProfileAPI), profileSyscalls(ProfileVMM)), uintptr(unix.SECCOMP_FILTER_FLAG_TSYNC))
}

func InstallThreadProfile(profile Profile) error {
	if disabled() {
		return nil
	}
	return installProfile(profileSyscalls(profile), 0)
}

func ProfileProgram(profile Profile) ([]unix.SockFilter, error) {
	return buildProgram(profileSyscalls(profile))
}

func disabled() bool {
	switch strings.TrimSpace(strings.ToLower(os.Getenv("GOCRACKER_SECCOMP"))) {
	case "0", "off", "false", "disabled":
		return true
	default:
		return false
	}
}

func installProfile(syscalls []uintptr, flags uintptr) error {
	filters, err := buildProgram(syscalls)
	if err != nil {
		return err
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}
	prog := unix.SockFprog{
		Len:    uint16(len(filters)),
		Filter: &filters[0],
	}
	if _, _, errno := unix.Syscall(unix.SYS_SECCOMP, uintptr(unix.SECCOMP_SET_MODE_FILTER), flags, uintptr(unsafe.Pointer(&prog))); errno != 0 {
		return fmt.Errorf("install seccomp filter: %w", errno)
	}
	return nil
}

func buildProgram(syscalls []uintptr) ([]unix.SockFilter, error) {
	allowed := uniqueSorted(syscalls)
	if len(allowed) == 0 {
		return nil, fmt.Errorf("seccomp profile is empty")
	}
	filters := []unix.SockFilter{
		stmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, seccompDataArch),
		jump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, unix.AUDIT_ARCH_AARCH64, 1, 0),
		stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_KILL_PROCESS),
		stmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, seccompDataNR),
	}
	for _, nr := range allowed {
		filters = append(filters,
			jump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, uint32(nr), 0, 1),
			stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ALLOW),
		)
	}
	filters = append(filters, stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_KILL_PROCESS))
	return filters, nil
}

func stmt(code uint16, k uint32) unix.SockFilter {
	return unix.SockFilter{Code: code, K: k}
}

func jump(code uint16, k uint32, jt, jf uint8) unix.SockFilter {
	return unix.SockFilter{Code: code, Jt: jt, Jf: jf, K: k}
}

func uniqueSorted(syscalls []uintptr) []uintptr {
	seen := make(map[uintptr]struct{}, len(syscalls))
	out := make([]uintptr, 0, len(syscalls))
	for _, nr := range syscalls {
		if _, ok := seen[nr]; ok {
			continue
		}
		seen[nr] = struct{}{}
		out = append(out, nr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func union(groups ...[]uintptr) []uintptr {
	total := 0
	for _, group := range groups {
		total += len(group)
	}
	out := make([]uintptr, 0, total)
	for _, group := range groups {
		out = append(out, group...)
	}
	return out
}

func profileSyscalls(profile Profile) []uintptr {
	switch profile {
	case ProfileAPI:
		return union(runtimeSyscalls(), fileSyscalls(), netSyscalls())
	case ProfileVMM:
		return union(runtimeSyscalls(), fileSyscalls(), netSyscalls(), vmSyscalls())
	case ProfileVCPU:
		return union(runtimeSyscalls(), fileSyscalls(), vcpuSyscalls())
	default:
		return nil
	}
}

func runtimeSyscalls() []uintptr {
	return []uintptr{
		unix.SYS_READ,
		unix.SYS_WRITE,
		unix.SYS_READV,
		unix.SYS_WRITEV,
		unix.SYS_CLOSE,
		unix.SYS_FSTAT,
		unix.SYS_NEWFSTATAT,
		unix.SYS_STATX,
		unix.SYS_LSEEK,
		unix.SYS_IOCTL,
		unix.SYS_MMAP,
		unix.SYS_MPROTECT,
		unix.SYS_MUNMAP,
		unix.SYS_BRK,
		unix.SYS_RT_SIGACTION,
		unix.SYS_RT_SIGPROCMASK,
		unix.SYS_RT_SIGRETURN,
		unix.SYS_SIGALTSTACK,
		unix.SYS_PREAD64,
		unix.SYS_PWRITE64,
		unix.SYS_FACCESSAT,
		unix.SYS_PIPE2,
		unix.SYS_DUP,
		unix.SYS_DUP3,
		unix.SYS_NANOSLEEP,
		unix.SYS_CLOCK_GETTIME,
		unix.SYS_CLOCK_NANOSLEEP,
		unix.SYS_SCHED_YIELD,
		unix.SYS_SCHED_GETAFFINITY,
		unix.SYS_MREMAP,
		unix.SYS_MSYNC,
		unix.SYS_MINCORE,
		unix.SYS_MADVISE,
		unix.SYS_FUTEX,
		unix.SYS_SET_TID_ADDRESS,
		unix.SYS_SET_ROBUST_LIST,
		unix.SYS_GETPID,
		unix.SYS_GETTID,
		unix.SYS_TGKILL,
		unix.SYS_PRCTL,
		unix.SYS_SECCOMP,
		unix.SYS_GETRLIMIT,
		unix.SYS_PRLIMIT64,
		unix.SYS_GETRANDOM,
		unix.SYS_GETCPU,
		unix.SYS_UNAME,
		unix.SYS_SYSINFO,
		unix.SYS_EPOLL_CREATE1,
		unix.SYS_EPOLL_CTL,
		unix.SYS_EPOLL_PWAIT,
		unix.SYS_EVENTFD2,
		unix.SYS_CLONE,
		unix.SYS_CLONE3,
		unix.SYS_EXIT,
		unix.SYS_EXIT_GROUP,
		unix.SYS_GETUID,
		unix.SYS_GETGID,
		unix.SYS_GETEUID,
		unix.SYS_GETEGID,
		unix.SYS_RSEQ,
		unix.SYS_RESTART_SYSCALL,
	}
}

func fileSyscalls() []uintptr {
	return []uintptr{
		unix.SYS_OPENAT,
		unix.SYS_GETCWD,
		unix.SYS_READLINKAT,
		unix.SYS_FCNTL,
		unix.SYS_FSYNC,
		unix.SYS_FDATASYNC,
		unix.SYS_FTRUNCATE,
		unix.SYS_FALLOCATE,
		unix.SYS_MKDIRAT,
		unix.SYS_UNLINKAT,
		unix.SYS_RENAMEAT,
		unix.SYS_RENAMEAT2,
		unix.SYS_SYNCFS,
	}
}

func netSyscalls() []uintptr {
	return []uintptr{
		unix.SYS_SOCKET,
		unix.SYS_CONNECT,
		unix.SYS_ACCEPT,
		unix.SYS_ACCEPT4,
		unix.SYS_SENDTO,
		unix.SYS_RECVFROM,
		unix.SYS_SENDMSG,
		unix.SYS_RECVMSG,
		unix.SYS_SHUTDOWN,
		unix.SYS_BIND,
		unix.SYS_LISTEN,
		unix.SYS_GETSOCKNAME,
		unix.SYS_GETPEERNAME,
		unix.SYS_SETSOCKOPT,
		unix.SYS_GETSOCKOPT,
		unix.SYS_SOCKETPAIR,
	}
}

func vmSyscalls() []uintptr {
	return []uintptr{
		unix.SYS_MEMFD_CREATE,
	}
}

func vcpuSyscalls() []uintptr {
	return []uintptr{
		unix.SYS_IOCTL,
		unix.SYS_PPOLL,
	}
}
