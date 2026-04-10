//go:build linux

package virtio

import (
	"sync"
	"time"
)

// TokenBucket describes a Firecracker-style token bucket.
// Size is the refill amount and upper steady-state bound.
// OneTimeBurst is the initial token count at first use.
// RefillTime controls how long it takes to refill Size tokens.
type TokenBucket struct {
	Size         uint64
	OneTimeBurst uint64
	RefillTime   time.Duration
}

// RateLimiterConfig groups the bandwidth and ops buckets.
type RateLimiterConfig struct {
	Bandwidth TokenBucket
	Ops       TokenBucket
}

// RateLimiter applies token buckets to device requests.
// A zero-value limiter is a no-op.
type RateLimiter struct {
	mu        sync.Mutex
	bandwidth tokenBucketState
	ops       tokenBucketState
}

type tokenBucketState struct {
	cfg    TokenBucket
	tokens float64
	last   time.Time
}

// NewRateLimiter builds a limiter from config.
func NewRateLimiter(cfg RateLimiterConfig) *RateLimiter {
	return &RateLimiter{
		bandwidth: tokenBucketState{cfg: cfg.Bandwidth},
		ops:       tokenBucketState{cfg: cfg.Ops},
	}
}

// Enabled reports whether either bucket is active.
func (r *RateLimiter) Enabled() bool {
	return r != nil && (r.bandwidth.enabled() || r.ops.enabled())
}

// Wait blocks until both buckets can pay for the request.
// bytes are charged against the bandwidth bucket and ops against the ops bucket.
func (r *RateLimiter) Wait(bytes, ops uint64) {
	if r == nil || !r.Enabled() {
		return
	}

	remainingBytes := bytes
	remainingOps := ops
	firstChunk := true

	for remainingBytes > 0 || (firstChunk && remainingOps > 0) {
		chunkBytes := remainingBytes
		if size := r.bandwidth.cfg.Size; size > 0 && chunkBytes > size {
			chunkBytes = size
		}
		chunkOps := uint64(0)
		if firstChunk {
			chunkOps = remainingOps
		}

		if wait := r.reserve(chunkBytes, chunkOps); wait > 0 {
			time.Sleep(wait)
			continue
		}

		if chunkBytes > 0 {
			remainingBytes -= chunkBytes
		}
		if firstChunk {
			remainingOps = 0
			firstChunk = false
		}
	}
}

func (r *RateLimiter) reserve(bytes, ops uint64) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	r.bandwidth.refill(now)
	r.ops.refill(now)

	bwWait := r.bandwidth.reserve(bytes)
	opsWait := r.ops.reserve(ops)
	if bwWait == 0 && opsWait == 0 {
		r.bandwidth.consume(bytes)
		r.ops.consume(ops)
		return 0
	}
	if bwWait > opsWait {
		return bwWait
	}
	return opsWait
}

func (b *tokenBucketState) enabled() bool {
	return b.cfg.Size > 0 && b.cfg.RefillTime > 0
}

func (b *tokenBucketState) refill(now time.Time) {
	if !b.enabled() {
		return
	}
	if b.last.IsZero() {
		b.last = now
		if b.tokens == 0 {
			b.tokens = float64(b.cfg.OneTimeBurst)
		}
		return
	}
	elapsed := now.Sub(b.last)
	if elapsed <= 0 {
		return
	}
	b.tokens += float64(b.cfg.Size) * float64(elapsed) / float64(b.cfg.RefillTime)
	capacity := float64(b.cfg.Size)
	if b.tokens > capacity {
		b.tokens = capacity
	}
	b.last = now
}

func (b *tokenBucketState) reserve(amount uint64) time.Duration {
	if amount == 0 || !b.enabled() {
		return 0
	}
	if b.tokens >= float64(amount) {
		return 0
	}
	missing := float64(amount) - b.tokens
	return time.Duration(missing * float64(b.cfg.RefillTime) / float64(b.cfg.Size))
}

func (b *tokenBucketState) consume(amount uint64) {
	if amount == 0 || !b.enabled() {
		return
	}
	if b.tokens >= float64(amount) {
		b.tokens -= float64(amount)
		return
	}
	b.tokens = 0
}
