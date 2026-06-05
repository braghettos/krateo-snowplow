// prewarm_engine_metrics.go — Ship 2 Stage 2.5 / 0.30.248 (Fix v2)
// PM Change #1. Publishes the prewarm engine's existing atomic counters
// (already maintained at enqueue/process/yield time on prewarmEngine
// struct) via expvar so the existing snowplow expvar pipeline (the
// tester scrapes /debug/vars) picks them up automatically.
//
// PRIOR ART: phase1_pip_metrics.go (the SOT for this expvar shape in the
// dispatchers package). Same shape: an expvar.Publish per counter,
// guarded by sync.Once, gated on cache.Disabled() so under
// CACHE_ENABLED=false the counters MUST NOT be registered (CFG-1 mirror,
// project_cache_off_is_transparent_fallback).
//
// SURFACES PUBLISHED (PM-required + observable):
//   - snowplow_prewarm_engine_enqueued_total  — cumulative enqueueScope
//     calls (every enqueue counted, even dedup-coalesced ones).
//   - snowplow_prewarm_engine_processed_total — scopes fully processed
//     by the worker. processed=enqueued-dedups means the queue is
//     drained.
//   - snowplow_prewarm_engine_yield_total     — ticks the worker parked
//     while a customer /call was in flight (customer-priority yield).
//   - snowplow_prewarm_engine_pending_depth   — live len(e.pending)
//     under the engine mutex. Steady-state expectation: 0 once the
//     worker drains. A non-zero value sustained over many scrapes
//     signals the worker is dead (Fix-v2 falsifier).
//
// REGISTRATION: registerPrewarmEngineMetrics(e) is called from inside
// StartPrewarmEngine's startedOnce.Do(...) block. Single registration
// per process by construction.

package dispatchers

import (
	"expvar"
	"sync"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

var prewarmEngineMetricsOnce sync.Once

// registerPrewarmEngineMetrics publishes the engine's counters via
// expvar.Publish. Idempotent across multiple calls (sync.Once); the
// engine singleton is the canonical source for the counter values.
//
// Cache-off gate: under CACHE_ENABLED=false the function returns
// without publishing so the expvar surface stays clean under the
// transparent-fallback contract.
func registerPrewarmEngineMetrics(e *prewarmEngine) {
	if cache.Disabled() {
		return
	}
	if e == nil {
		return
	}
	prewarmEngineMetricsOnce.Do(func() {
		expvar.Publish("snowplow_prewarm_engine_enqueued_total", expvar.Func(func() any {
			return e.enqueuedTotal.Load()
		}))
		expvar.Publish("snowplow_prewarm_engine_processed_total", expvar.Func(func() any {
			return e.processedTotal.Load()
		}))
		expvar.Publish("snowplow_prewarm_engine_yield_total", expvar.Func(func() any {
			return e.yieldTotal.Load()
		}))
		expvar.Publish("snowplow_prewarm_engine_pending_depth", expvar.Func(func() any {
			// Read pending depth under e.mu so the observation is
			// race-free against enqueue/dequeue (which mutate the map).
			e.mu.Lock()
			defer e.mu.Unlock()
			return len(e.pending)
		}))
	})
}
