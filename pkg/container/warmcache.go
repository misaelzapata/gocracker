package container

import (
	"os"
	"strings"
	"time"

	"github.com/gocracker/gocracker/internal/guestexec"
	gclog "github.com/gocracker/gocracker/internal/log"
	"github.com/gocracker/gocracker/pkg/vmm"
	"github.com/gocracker/gocracker/pkg/warmcache"
)

// warmCacheEnabled reports whether the caller opted into warm-cache lookup
// via the GOCRACKER_WARM_CACHE=1 environment variable. Keeping the feature
// opt-in during the MVP avoids surprising existing workflows when the cache
// happens to hold a stale entry that still key-matches but whose underlying
// image was since rebuilt out-of-band.
func warmCacheEnabled() bool {
	v := strings.TrimSpace(os.Getenv("GOCRACKER_WARM_CACHE"))
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}

// warmCacheInputsReady returns true when opts carries enough deterministic
// identity to build a cache key. We require an image ref (Dockerfile/repo
// builds are dropped — their rootfs is build-time non-deterministic and
// caching them would be a foot-gun) and a kernel path (we hash the kernel
// bytes into the key so a rebuilt vmlinux invalidates old entries).
func warmCacheInputsReady(opts RunOptions) bool {
	if strings.TrimSpace(opts.Image) == "" {
		return false
	}
	if strings.TrimSpace(opts.KernelPath) == "" {
		return false
	}
	if len(opts.Drives) > 0 {
		// Extra block devices aren't captured in the cache key and would
		// drift silently. Safer to fall through to cold boot.
		return false
	}
	return true
}

// computeWarmCacheKey builds a warmcache.Key from the run options. Returns
// ok=false if any unavoidable I/O (currently: hashing the kernel binary)
// fails — callers treat that as a cache miss and fall through to cold boot.
func computeWarmCacheKey(opts RunOptions) (string, bool) {
	kHash, err := warmcache.HashFile(opts.KernelPath)
	if err != nil {
		gclog.Container.Debug("warm-cache key: kernel hash failed", "path", opts.KernelPath, "error", err)
		return "", false
	}
	// When WarmCapture is set the snapshot is taken in InteractiveExec mode
	// (idle exec agent, no CMD running), so it is CMD-agnostic. Exclude the
	// user CMD from the key so every CMD reuses the same cached snapshot.
	// Without WarmCapture (plain GOCRACKER_WARM_CACHE=1 lookup), keep the
	// CMD in the key for safety — the snapshot was captured with a specific
	// CMD frozen in the kernel cmdline.
	var cmdline string
	if !opts.WarmCapture {
		cmdline = strings.Join(append(append([]string{}, opts.Entrypoint...), opts.Cmd...), " ")
	}
	return warmcache.Key(warmcache.KeyInput{
		ImageDigest: opts.Image,
		KernelHash:  kHash,
		Cmdline:     cmdline,
		MemMB:       opts.MemMB,
		VCPUs:       opts.CPUs,
		Arch:        opts.Arch,
		NetworkMode: opts.NetworkMode,
	}), true
}

// captureWarmSnapshot takes a snapshot of a freshly cold-booted VM and stores
// it in the warmcache so subsequent runs with identical parameters skip the
// cold-boot path entirely. Always runs in a goroutine — errors are logged and
// silently discarded so they never affect the caller's RunResult.
//
// Only exec-enabled VMs are snapshotted: the exec agent provides a reliable
// "guest is ready" signal, and these VMs idle between commands so TakeSnapshot
// is safe. One-shot VMs (no exec) may have already stopped by the time this
// goroutine runs, and a snapshot of a stopping VM is not useful.
func captureWarmSnapshot(handle vmm.Handle, opts RunOptions, key string) {
	if !opts.ExecEnabled {
		return
	}
	root := warmcache.DefaultRoot()
	if _, hit := warmcache.Lookup(root, key); hit {
		return
	}
	waitExecReady(handle, 2*time.Second)
	if handle.State() != vmm.StateRunning {
		return
	}
	tmp, err := os.MkdirTemp("", "gocracker-warmsnap-*")
	if err != nil {
		gclog.Container.Warn("warm-cache capture: mktemp", "error", err)
		return
	}
	defer os.RemoveAll(tmp)
	// TakeSnapshot with Resume:true pauses briefly, snapshots, then resumes —
	// the VM stays running and the caller's session is unaffected.
	if _, err := handle.TakeSnapshot(tmp); err != nil {
		gclog.Container.Warn("warm-cache capture: snapshot", "error", err)
		return
	}
	if err := warmcache.Store(tmp, root, key); err != nil {
		gclog.Container.Warn("warm-cache capture: store", "error", err)
		return
	}
	gclog.Container.Info("warm-cache stored", "key", key[:12])
}

// waitExecReady polls the vsock exec port until the guest agent accepts a
// connection or the timeout expires. A successful dial (immediately closed)
// proves the in-guest exec agent is up and the snapshot will be taken at a
// stable point. Silently returns on timeout or when exec is not configured.
func waitExecReady(handle vmm.Handle, timeout time.Duration) {
	dialer, ok := handle.(vmm.VsockDialer)
	if !ok {
		return
	}
	cfg := handle.VMConfig()
	if cfg.Exec == nil || !cfg.Exec.Enabled {
		return
	}
	port := uint32(guestexec.DefaultVsockPort)
	if cfg.Exec.VsockPort != 0 {
		port = cfg.Exec.VsockPort
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := dialer.DialVsock(port)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}
