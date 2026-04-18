package container

import (
	"encoding/json"
	"os"
	"path/filepath"
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
// ComputeWarmCacheKey is the exported form used by external tools (e.g. pool-bench).
func ComputeWarmCacheKey(opts RunOptions) (string, bool) { return computeWarmCacheKey(opts) }

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
	// Wait for the guest's exec agent to be ready. FirstOutputAt is set when
	// the guest first writes to the serial console (~50ms); the exec agent
	// starts listening on vsock ~100ms after that. Poll until first output
	// is seen, then add a 150ms grace period. Total typical wait: ~200ms.
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if !handle.FirstOutputAt().IsZero() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
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
	snap, err := handle.TakeSnapshot(tmp)
	if err != nil {
		gclog.Container.Warn("warm-cache capture: snapshot", "error", err)
		return
	}
	// The snapshot skipped disk bundling. Hardlink the runtime disk from the
	// VM's config into the snapshot dir and rewrite snapshot.json accordingly
	// so the restore can find it after the VM's runtime dir is cleaned up.
	// For worker-backed VMs, takeSnapshotViaExport already did this.
	if snap != nil && snap.Config.DiskImage != "" && !strings.HasPrefix(snap.Config.DiskImage, "artifacts/") {
		artifactsDir := filepath.Join(tmp, "artifacts")
		if err := os.MkdirAll(artifactsDir, 0755); err == nil {
			diskDst := filepath.Join(artifactsDir, "disk.ext4")
			if err := os.Link(snap.Config.DiskImage, diskDst); err != nil {
				// Same-FS hardlink failed; rewrite to absolute path as fallback.
				gclog.Container.Debug("warm-cache disk hardlink failed, using absolute path", "error", err)
			} else {
				snap.Config.DiskImage = "artifacts/disk.ext4"
				if data, jerr := json.MarshalIndent(snap, "", "  "); jerr == nil {
					_ = os.WriteFile(filepath.Join(tmp, "snapshot.json"), data, 0644)
				}
			}
		}
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
