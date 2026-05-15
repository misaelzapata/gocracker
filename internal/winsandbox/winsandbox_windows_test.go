//go:build windows

package winsandbox

import (
	"errors"
	"sync"
	"testing"

	"golang.org/x/sys/windows"
)

// TestJobLimits_KillOnClose builds a job with kill-on-job-close set,
// queries it back, and asserts the bit survived. The job is created
// via createJobForTest which deliberately SKIPS AssignProcessToJobObject
// so the test process itself isn't subject to the cap — otherwise a
// 64 MiB memory test below would kill the Go test runner mid-suite.
//
// This proves the layout of JOBOBJECT_EXTENDED_LIMIT_INFORMATION and
// the SetInformationJobObject argument shape: if either were wrong
// the kernel would reject the set call with ERROR_INVALID_PARAMETER
// or read back zeroed flags.
func TestJobLimits_KillOnClose(t *testing.T) {
	job, err := createJobForTest(Config{KillOnJobClose: true})
	if err != nil {
		t.Fatalf("createJobForTest: %v", err)
	}
	defer windows.CloseHandle(job)

	info, err := queryExtendedLimitsOf(job)
	if err != nil {
		t.Fatalf("queryExtendedLimitsOf: %v", err)
	}
	if info.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE == 0 {
		t.Fatalf("KILL_ON_JOB_CLOSE not set; got flags = 0x%08x",
			info.BasicLimitInformation.LimitFlags)
	}
}

// TestJobLimits_Memory installs a 64 MiB cap on a NEW job that is
// never assigned to the test process, then reads back the limit. The
// cheaper alternative would be to attempt a 256 MiB VirtualAlloc and
// observe the failure — but doing that on the test process itself
// crashes Go's runtime (the test scheduler grows stacks via mmap and
// panics on ENOMEM). The flag-bit + value round-trip is enough to
// prove our struct encoding matches the kernel's expectation.
func TestJobLimits_Memory(t *testing.T) {
	const limit = 64 << 20
	job, err := createJobForTest(Config{MemoryLimitBytes: limit})
	if err != nil {
		t.Fatalf("createJobForTest: %v", err)
	}
	defer windows.CloseHandle(job)

	info, err := queryExtendedLimitsOf(job)
	if err != nil {
		t.Fatalf("queryExtendedLimitsOf: %v", err)
	}
	if info.JobMemoryLimit != uintptr(limit) {
		t.Errorf("JobMemoryLimit = %d, want %d", info.JobMemoryLimit, limit)
	}
	wantFlags := uint32(windows.JOB_OBJECT_LIMIT_JOB_MEMORY |
		windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY)
	if info.BasicLimitInformation.LimitFlags&wantFlags != wantFlags {
		t.Errorf("limit flags = 0x%08x, want subset 0x%08x",
			info.BasicLimitInformation.LimitFlags, wantFlags)
	}
}

// TestApply_OneShot verifies the package-level guard. Apply is one
// shot: a second call must return ErrAlreadyApplied even with a
// different config. We disable token + integrity drops so the test
// runner stays functional after the first Apply.
//
// We deliberately set NO resource caps in this test — the goal is
// strictly to exercise the gate, not to constrain the test process.
func TestApply_OneShot(t *testing.T) {
	resetForTest(t)

	first := Config{
		DisableRestrictedToken: true,
		DisableLowIntegrity:    true,
	}
	if err := Apply(first); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	second := Config{
		DisableRestrictedToken: true,
		DisableLowIntegrity:    true,
	}
	if err := Apply(second); !errors.Is(err, ErrAlreadyApplied) {
		t.Fatalf("second Apply: got %v, want ErrAlreadyApplied", err)
	}
}

// resetForTest forcibly resets the package-level Apply guards. The
// real package is one-shot per process, but `go test` runs every
// TestXxx in the same process, so without a reset only one Apply
// test can ever pass per `go test` invocation. We reset the sync.Once
// + applied flag and close any prior job handle.
func resetForTest(t *testing.T) {
	t.Helper()
	if jobHandle != 0 {
		_ = windows.CloseHandle(jobHandle)
		jobHandle = 0
	}
	applied = false
	applyOnce = sync.Once{}
}
