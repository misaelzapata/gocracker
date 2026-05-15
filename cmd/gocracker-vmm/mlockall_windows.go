//go:build windows

package main

// On Linux the worker calls unix.Mlockall(MCL_CURRENT) to pin its
// resident set into RAM so cold-boot p95/p99 isn't tail-loaded by
// page-faults on rarely-touched pages (IRQ tables, virtio descriptor
// rings, seccomp filter pages, etc.). Windows has no direct mlockall
// analogue; the closest is:
//
//   1. SetProcessWorkingSetSizeEx — promise the kernel a minimum
//      working-set so it won't trim our pages under memory pressure.
//   2. VirtualLock — pin a specific virtual range. Requires the
//      SE_LOCK_MEMORY_NAME privilege, which gocracker-vmm typically
//      does NOT hold (it's reserved for admin or SeLockMemoryPrivilege-
//      enabled service accounts).
//
// Because (2) almost always fails for non-elevated processes and (1)
// is purely advisory, this implementation is best-effort: it bumps
// the working-set minimum to ~64 MiB and logs a one-line warning if
// either call fails. The VM still boots fine without it — only the
// p95 cold-boot latency regresses, matching the Linux opt-out
// behaviour (GOCRACKER_NO_MLOCK).

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/windows"
)

// Windows kernel constants not exposed by x/sys/windows@v0.43 directly.
const (
	// QUOTA_LIMITS_HARDWS_MIN_ENABLE forces the working-set minimum to
	// stay pinned (rather than just being a hint). Without this flag,
	// the kernel's memory manager treats the minimum as soft and can
	// trim below it under heavy pressure.
	quotaLimitsHardWSMinEnable uint32 = 0x00000001

	// 64 MiB minimum working-set. This matches the empirical resident
	// set of a Go-managed gocracker-vmm process during boot (heap +
	// goroutine stacks + bss + read-only Go runtime tables). Larger
	// values waste host RAM; smaller values get trimmed under load.
	wsMinBytes uintptr = 64 * 1024 * 1024

	// 256 MiB maximum working-set ceiling — generous enough to cover
	// the guest RAM mapping that BootLinuxOnWHP backs with host pages,
	// without preventing the kernel from reclaiming pages we never
	// touch again after boot.
	wsMaxBytes uintptr = 256 * 1024 * 1024
)

// applyWorkingSetPolicy is the Windows equivalent of Mlockall(MCL_CURRENT).
// Best-effort: prints a warning on failure but never aborts startup,
// because tight working-set quotas inside Docker for Windows containers
// or sandboxed CI runners would otherwise make the binary unusable.
//
// Opt-outable via GOCRACKER_NO_MLOCK=1 for parity with the Linux build.
func applyWorkingSetPolicy(stderr io.Writer) {
	if os.Getenv("GOCRACKER_NO_MLOCK") == "1" {
		return
	}
	h := windows.CurrentProcess()
	if err := windows.SetProcessWorkingSetSizeEx(h, wsMinBytes, wsMaxBytes, quotaLimitsHardWSMinEnable); err != nil {
		// Common failures: ERROR_PRIVILEGE_NOT_HELD when the process
		// lacks SeIncreaseQuotaPrivilege, or ERROR_INVALID_PARAMETER
		// on hosts where the working-set ceiling collides with the job
		// object cap. Either way we log and continue.
		fmt.Fprintf(stderr, "gocracker-vmm: SetProcessWorkingSetSizeEx skipped (%v); p95 latency may regress\n", err)
	}
}
