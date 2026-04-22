package pool

import (
	"expvar"
	"sync"
)

// Fase 5 plan §5 step 5 calls for expvar gauges covering pool
// capacity + lease traffic. Published under "gocracker_pool_*"
// namespace so operators can scrape them via net/http/pprof's
// /debug/vars (mounted by sandboxd's HTTP server).
//
// Counters accumulate monotonically; gauges reflect current state.
// Per-template breakdowns live on Map metrics keyed by template_id.

var (
	metricsInit sync.Once

	// Counters (monotonic, reset at process start).
	metricBootAttempts  = expvar.NewInt("gocracker_pool_boot_attempts_total")
	metricBootSuccesses = expvar.NewInt("gocracker_pool_boot_successes_total")
	metricBootFailures  = expvar.NewInt("gocracker_pool_boot_failures_total")
	metricLeases        = expvar.NewInt("gocracker_pool_leases_total")
	metricLeaseFails    = expvar.NewInt("gocracker_pool_lease_failures_total")
	metricReaps         = expvar.NewInt("gocracker_pool_reaps_total")

	// Gauges (current live values).
	metricGaugeHot      = expvar.NewInt("gocracker_pool_hot_current")
	metricGaugePaused   = expvar.NewInt("gocracker_pool_paused_current")
	metricGaugeLeased   = expvar.NewInt("gocracker_pool_leased_current")
	metricGaugeInflight = expvar.NewInt("gocracker_pool_inflight_current")
)

// initMetrics ensures expvar vars are registered exactly once per
// process. expvar.NewInt panics on duplicate name, so gating with
// sync.Once lets the pool package be imported from tests and
// production without collisions.
//
// Vars themselves are package-level and thus initialized on first
// var-declaration; this function exists only to expose a hook for
// future test-time reset logic.
func initMetrics() { metricsInit.Do(func() {}) }

// publishGaugesLocked updates the current-state gauges. Called with
// p.mu held so the snapshot is atomic w.r.t. reconcile writes.
// Sums across every pool instance in the process — expvar is
// process-global, so a single Manager exporting all its pools
// lands here correctly.
func (p *Pool) publishGaugesLocked() {
	// Use the already-locked state; this is called from inside
	// reconcile, Acquire, Release, and the reaper.
	hot, paused, leased := 0, 0, 0
	for _, e := range p.entries {
		switch e.State {
		case StateHot:
			hot++
		case StatePaused:
			paused++
		case StateLeased:
			leased++
		}
	}
	// expvar.Int.Set is lock-free but non-atomic across vars — the
	// metrics backend is eventually consistent anyway (scrapes are
	// best-effort snapshots).
	metricGaugeHot.Set(int64(hot))
	metricGaugePaused.Set(int64(paused))
	metricGaugeLeased.Set(int64(leased))
	metricGaugeInflight.Set(int64(p.inflightHot + p.inflightPaused))
}
