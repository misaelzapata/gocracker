package container

import (
	"os"
	"strings"

	gclog "github.com/gocracker/gocracker/internal/log"
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
	cmdline := strings.Join(append(append([]string{}, opts.Entrypoint...), opts.Cmd...), " ")
	return warmcache.Key(warmcache.KeyInput{
		ImageDigest: opts.Image,
		KernelHash:  kHash,
		Cmdline:     cmdline,
		MemMB:       opts.MemMB,
		VCPUs:       opts.CPUs,
		Arch:        opts.Arch,
	}), true
}
