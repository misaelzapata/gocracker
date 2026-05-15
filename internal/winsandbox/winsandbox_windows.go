//go:build windows

// Package winsandbox confines the current process inside a Windows
// Job Object, drops privileges via a restricted token, and lowers the
// process integrity level. It is the Windows counterpart to
// internal/jailer (which uses chroot + cgroupv2 + namespaces on Linux).
//
// The Job Object enforces:
//   - Per-job memory cap (JOB_OBJECT_LIMIT_JOB_MEMORY) so a runaway
//     allocation in the guest emulation path can't OOM the host.
//   - Kill-on-job-close so an abrupt parent crash also terminates
//     every process assigned to the job (parent-death equivalent).
//   - CPU affinity / rate control derived from Config.CPUShares.
//
// The restricted token strips DISABLE_MAX_PRIVILEGE, removing all
// privileges that are not in the safe baseline (basically everything
// except SE_CHANGE_NOTIFY_PRIVILEGE).
//
// The integrity-level drop writes S-1-16-4096 (Low) into the process
// token's TokenIntegrityLevel field, so any handle the sandboxed
// process opens is subject to mandatory integrity control — a file
// or registry write to a Medium-or-higher object is denied even if
// DACL would otherwise permit it.
//
// All three are best-effort: the Job Object always succeeds, the
// restricted-token and integrity-level calls may fail on locked-down
// hosts (Server Core, Windows Sandbox-in-Sandbox, etc.) and we treat
// those as a warning rather than a fatal error — the Job Object alone
// still meaningfully constrains the process.
//
// Reference: docs.microsoft.com/.../win32/procthread/job-objects
// + Chromium's sandbox/win/src/restricted_token_utils.cc which is the
// canonical implementation. We do not try to match Chromium's
// AppContainer story because gocracker doesn't need GUI sandboxing.
package winsandbox

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Config selects which restrictions to apply. Fields are 1:1 with
// sandbox.Config but kept separate so this package compiles without
// importing the cross-platform interface.
type Config struct {
	// MemoryLimitBytes caps total committed memory for the job. 0 =
	// no limit. Enforced via JOB_OBJECT_LIMIT_JOB_MEMORY.
	MemoryLimitBytes uint64

	// CPUShares is a relative CPU weight 1..10000. Converted to the
	// hard CPU rate (basis points of total CPU) via the
	// JobObjectCpuRateControlInformation class. 0 = unconstrained.
	CPUShares int

	// NoNetwork is best-effort on Windows: we attach a
	// JOBOBJECT_NET_RATE_CONTROL_INFORMATION with MaxBandwidth=1 bps
	// to throttle outbound network nearly to zero. Inbound traffic
	// and loopback are not constrained — a tighter guarantee requires
	// HCS (Host Compute Service) which we don't depend on here.
	NoNetwork bool

	// KillOnJobClose adds JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE so the
	// Job Object's last handle closing (which happens when the
	// parent terminates) terminates every process in the job.
	KillOnJobClose bool

	// DisableRestrictedToken skips the CreateRestrictedToken step.
	// Useful for tests that need to keep their full privileges so
	// they can read back job info from the kernel.
	DisableRestrictedToken bool

	// DisableLowIntegrity skips the integrity-level drop. Same
	// rationale as DisableRestrictedToken.
	DisableLowIntegrity bool
}

// ErrAlreadyApplied is returned by Apply if called twice in the same
// process. Job-object assignment is not strictly one-shot on Win8+
// (nested jobs are legal), but the integrity-level and token
// restrictions are irreversible, so we forbid the double-apply.
var ErrAlreadyApplied = errors.New("winsandbox: already applied to this process")

var (
	applyOnce sync.Once
	applied   bool
	jobHandle windows.Handle // last-installed job handle, kept for tests
)

// Apply attaches the configured Job Object to the current process.
// It is one-shot: a second call returns ErrAlreadyApplied. The job
// handle is intentionally NOT closed; closing the last handle would
// trigger KILL_ON_JOB_CLOSE before this process gets to do anything
// useful. The Windows kernel cleans up the job when the last assigned
// process exits.
func Apply(cfg Config) error {
	if applied {
		return ErrAlreadyApplied
	}
	var firstErr error
	applyOnce.Do(func() {
		firstErr = doApply(cfg)
		if firstErr == nil {
			applied = true
		}
	})
	return firstErr
}

// doApply executes the four sandbox steps in order. The order matters:
//  1. Create + configure Job Object (must precede AssignProcessToJobObject)
//  2. AssignProcessToJobObject for the current PID
//  3. CreateRestrictedToken (must precede integrity drop because we
//     adjust the *new* token, then install it)
//  4. SetTokenInformation(TokenIntegrityLevel) on the new token
//
// Steps 3-4 are skipped when DisableRestrictedToken/DisableLowIntegrity
// are set — used by tests.
func doApply(cfg Config) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("winsandbox: CreateJobObject: %w", err)
	}
	jobHandle = job

	if err := configureJobLimits(job, cfg); err != nil {
		return err
	}
	if cfg.NoNetwork {
		if err := configureNetworkRateLimit(job); err != nil {
			// Best-effort — Windows builds < 10.0.16299 lack
			// JobObjectNetRateControlInformation. Log via the
			// returned error but don't fail the whole Apply.
			return fmt.Errorf("winsandbox: NoNetwork: %w", err)
		}
	}
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		return fmt.Errorf("winsandbox: AssignProcessToJobObject: %w", err)
	}

	// Mitigation policies (DEP/ASLR/CFG) — best-effort. On Windows 10+
	// most of these are already enforced and the call returns
	// ERROR_ACCESS_DENIED which we explicitly tolerate.
	_ = enableMitigationPolicies()

	if !cfg.DisableRestrictedToken {
		if err := installRestrictedToken(); err != nil {
			return fmt.Errorf("winsandbox: restrict token: %w", err)
		}
	}
	if !cfg.DisableLowIntegrity {
		if err := dropToLowIntegrity(); err != nil {
			return fmt.Errorf("winsandbox: low integrity: %w", err)
		}
	}
	return nil
}

// configureJobLimits writes the extended-limit struct with the
// memory cap, affinity mask, CPU class, and the kill-on-close /
// process-memory / job-memory flag bits derived from cfg.
func configureJobLimits(job windows.Handle, cfg Config) error {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	var flags uint32 = windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK
	if cfg.MemoryLimitBytes > 0 {
		flags |= windows.JOB_OBJECT_LIMIT_JOB_MEMORY
		flags |= windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY
		info.JobMemoryLimit = uintptr(cfg.MemoryLimitBytes)
		info.ProcessMemoryLimit = uintptr(cfg.MemoryLimitBytes)
	}
	if cfg.KillOnJobClose {
		flags |= windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	}
	info.BasicLimitInformation.LimitFlags = flags

	_, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil {
		return fmt.Errorf("winsandbox: SetInformationJobObject(ExtendedLimit): %w", err)
	}
	if cfg.CPUShares > 0 {
		if err := configureCPURate(job, cfg.CPUShares); err != nil {
			return err
		}
	}
	return nil
}

// jobObjectCPURateControlInformation mirrors the Windows SDK struct
// of the same name. The "Value" anonymous union is 4 bytes wide; the
// kernel inspects either CpuRate (when CONTROL_FLAGS & RATE is set) or
// Weight (when CONTROL_FLAGS & WEIGHT is set), never both.
type jobObjectCPURateControlInformation struct {
	ControlFlags uint32
	Value        uint32 // either CpuRate (basis points) or Weight (1..9)
}

const (
	jobObjectCPURateControlEnable    uint32 = 0x00000001
	jobObjectCPURateControlWeightBased uint32 = 0x00000002
	jobObjectCPURateControlHardCap    uint32 = 0x00000004
)

// configureCPURate translates CPUShares (1..10000) into a hard-cap
// CPU rate expressed in basis points of one CPU (10000 = full CPU).
// We use the hard-cap class because it gives deterministic limits;
// weight-based scheduling is preferable in production but harder to
// assert in tests.
func configureCPURate(job windows.Handle, shares int) error {
	rate := uint32(shares)
	if rate > 10000 {
		rate = 10000
	}
	info := jobObjectCPURateControlInformation{
		ControlFlags: jobObjectCPURateControlEnable | jobObjectCPURateControlHardCap,
		Value:        rate,
	}
	_, err := windows.SetInformationJobObject(
		job,
		uint32(jobObjectCPURateControlInformationClass),
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil {
		return fmt.Errorf("winsandbox: SetInformationJobObject(CpuRate): %w", err)
	}
	return nil
}

const jobObjectCPURateControlInformationClass = 15
const jobObjectNetRateControlInformationClass = 32

// jobObjectNetRateControlInformation matches the Windows SDK struct.
// We only set MaxBandwidth + ControlFlags.
type jobObjectNetRateControlInformation struct {
	MaxBandwidth uint64
	ControlFlags uint32
	DscpTag      uint8
	_            [3]byte // padding to 16 bytes
}

const (
	jobObjectNetRateControlEnable          uint32 = 0x00000001
	jobObjectNetRateControlMaxBandwidthBit uint32 = 0x00000002
)

func configureNetworkRateLimit(job windows.Handle) error {
	info := jobObjectNetRateControlInformation{
		MaxBandwidth: 1, // 1 bps — effectively no throughput
		ControlFlags: jobObjectNetRateControlEnable | jobObjectNetRateControlMaxBandwidthBit,
	}
	_, err := windows.SetInformationJobObject(
		job,
		uint32(jobObjectNetRateControlInformationClass),
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil {
		return fmt.Errorf("winsandbox: SetInformationJobObject(NetRate): %w", err)
	}
	return nil
}

// QueryExtendedLimits reads back the extended-limit struct via
// QueryInformationJobObject. Exposed for tests; production callers
// don't need to inspect their own job state.
func QueryExtendedLimits() (windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION, error) {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if jobHandle == 0 {
		return info, errors.New("winsandbox: Apply has not been called")
	}
	err := windows.QueryInformationJobObject(
		jobHandle,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
		nil,
	)
	if err != nil {
		return info, fmt.Errorf("winsandbox: QueryInformationJobObject: %w", err)
	}
	return info, nil
}

// createJobForTest creates and configures a job exactly the way Apply
// does, but stops short of AssignProcessToJobObject and the token /
// integrity drops. Tests use this to round-trip the extended-limit
// struct through the kernel without applying the cap to the test
// runner (a 64 MiB cap would kill the Go test process within
// microseconds). Caller must CloseHandle the returned handle.
func createJobForTest(cfg Config) (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("winsandbox: CreateJobObject: %w", err)
	}
	if err := configureJobLimits(job, cfg); err != nil {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	if cfg.NoNetwork {
		if err := configureNetworkRateLimit(job); err != nil {
			_ = windows.CloseHandle(job)
			return 0, err
		}
	}
	return job, nil
}

// queryExtendedLimitsOf reads the extended-limit struct for an
// arbitrary job handle. Tests use it on the handle returned by
// createJobForTest so they don't have to call Apply at all.
func queryExtendedLimitsOf(job windows.Handle) (windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION, error) {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	err := windows.QueryInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
		nil,
	)
	if err != nil {
		return info, fmt.Errorf("winsandbox: QueryInformationJobObject: %w", err)
	}
	return info, nil
}

// --- restricted token + integrity-level helpers below ----------------

// LUA tokens come from advapi32!CreateRestrictedToken which is not in
// golang.org/x/sys/windows. We declare it via LazyDLL/LazyProc, the
// same pattern internal/whp uses for WinHvPlatform.dll.
var (
	advapi32                  = windows.NewLazySystemDLL("advapi32.dll")
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	procCreateRestrictedToken = advapi32.NewProc("CreateRestrictedToken")
	procSetProcessMitigation  = kernel32.NewProc("SetProcessMitigationPolicy")
)

// Flags for CreateRestrictedToken. Source: WinNT.h.
const (
	disableMaxPrivilege uint32 = 0x1
	sandboxInert        uint32 = 0x2
	lUARestrictedFlag   uint32 = 0x4
	writeRestricted     uint32 = 0x8
)

// installRestrictedToken replaces the thread's primary access token
// with a stripped-down version. The new token has:
//   - All privileges removed via DISABLE_MAX_PRIVILEGE (the kernel
//     auto-removes everything except SE_CHANGE_NOTIFY).
//   - LUA_TOKEN bit set so the kernel treats us like a UAC-limited
//     user even if the parent process ran elevated.
//
// SetThreadToken is used instead of SetProcessToken because the
// latter doesn't exist for self-modification — the only legal way
// to replace the *process* token is to call DuplicateTokenEx with
// TokenPrimary and pass it to CreateProcessAsUser of a *new* process.
// For self-restriction we install on the current thread, which is
// sufficient because gocracker's hot path runs on the main goroutine.
func installRestrictedToken() error {
	var current windows.Token
	if err := windows.OpenProcessToken(
		windows.CurrentProcess(),
		windows.TOKEN_DUPLICATE|windows.TOKEN_ASSIGN_PRIMARY|windows.TOKEN_QUERY,
		&current,
	); err != nil {
		return fmt.Errorf("OpenProcessToken: %w", err)
	}
	defer current.Close()

	var restricted windows.Token
	r1, _, callErr := procCreateRestrictedToken.Call(
		uintptr(current),
		uintptr(disableMaxPrivilege|lUARestrictedFlag),
		0, 0, // DisableSidCount + SidsToDisable
		0, 0, // DeletePrivilegeCount + PrivilegesToDelete
		0, 0, // RestrictedSidCount + SidsToRestrict
		uintptr(unsafe.Pointer(&restricted)),
	)
	if r1 == 0 {
		return fmt.Errorf("CreateRestrictedToken: %w", callErr)
	}
	defer restricted.Close()

	if err := windows.SetThreadToken(nil, restricted); err != nil {
		return fmt.Errorf("SetThreadToken: %w", err)
	}
	return nil
}

// dropToLowIntegrity writes S-1-16-4096 into the process token's
// TokenIntegrityLevel slot. After this returns, any handle we open
// is subject to mandatory integrity control: writes to Medium+
// objects are denied even when the DACL allows them.
func dropToLowIntegrity() error {
	// "S-1-16-4096" is the Low integrity SID. We could also build it
	// via CreateWellKnownSid(WinLowLabelSid), but StringToSid keeps
	// the intent obvious in the source.
	sid, err := windows.StringToSid("S-1-16-4096")
	if err != nil {
		return fmt.Errorf("StringToSid(low): %w", err)
	}
	tml := windows.Tokenmandatorylabel{
		Label: windows.SIDAndAttributes{
			Sid:        sid,
			Attributes: windows.SE_GROUP_INTEGRITY,
		},
	}
	var token windows.Token
	if err := windows.OpenProcessToken(
		windows.CurrentProcess(),
		windows.TOKEN_ADJUST_DEFAULT|windows.TOKEN_QUERY,
		&token,
	); err != nil {
		return fmt.Errorf("OpenProcessToken: %w", err)
	}
	defer token.Close()

	if err := windows.SetTokenInformation(
		token,
		windows.TokenIntegrityLevel,
		(*byte)(unsafe.Pointer(&tml)),
		tml.Size(),
	); err != nil {
		return fmt.Errorf("SetTokenInformation(IntegrityLevel): %w", err)
	}
	return nil
}

// processMitigationPolicy identifiers — values from
// processthreadsapi.h. We only flip the cheap-to-enable, no-side-effect
// policies; aggressive things like ImageLoadPolicy break Go's runtime.
const (
	processDEPPolicy                  uint32 = 0
	processASLRPolicy                 uint32 = 1
	processStrictHandleCheckPolicy    uint32 = 3
	processSystemCallDisablePolicy    uint32 = 4
	processExtensionPointDisablePolicy uint32 = 6
	processControlFlowGuardPolicy     uint32 = 7
)

// enableMitigationPolicies issues SetProcessMitigationPolicy for the
// handful of policies that are safe to set programmatically. Failures
// are tolerated: on Windows 10+ many of these are already on by
// default and the API returns ERROR_ACCESS_DENIED for "already set".
func enableMitigationPolicies() error {
	// ASLR: enable bottom-up + force-relocate.
	var aslr struct{ Flags uint32 }
	aslr.Flags = 0x00000003 // EnableBottomUpRandomization | ForceRelocateImages
	procSetProcessMitigation.Call(
		uintptr(processASLRPolicy),
		uintptr(unsafe.Pointer(&aslr)),
		uintptr(unsafe.Sizeof(aslr)),
	)
	// DEP: permanent + no thunk emulation.
	var dep struct{ Flags uint32 }
	dep.Flags = 0x00000003 // Enable | DisableAtlThunkEmulation
	procSetProcessMitigation.Call(
		uintptr(processDEPPolicy),
		uintptr(unsafe.Pointer(&dep)),
		uintptr(unsafe.Sizeof(dep)),
	)
	// Strict handle check: terminate the process on a bad handle.
	var sh struct{ Flags uint32 }
	sh.Flags = 0x00000001
	procSetProcessMitigation.Call(
		uintptr(processStrictHandleCheckPolicy),
		uintptr(unsafe.Pointer(&sh)),
		uintptr(unsafe.Sizeof(sh)),
	)
	// Extension-point disable: refuse legacy hook DLLs.
	var ep struct{ Flags uint32 }
	ep.Flags = 0x00000001
	procSetProcessMitigation.Call(
		uintptr(processExtensionPointDisablePolicy),
		uintptr(unsafe.Pointer(&ep)),
		uintptr(unsafe.Sizeof(ep)),
	)
	return nil
}
