// resolved_cache_metrics.go — Ship 0.30.236 (T8): expose the
// ResolvedCacheStore resident-region counters via expvar so the
// post-deploy Gate C (resident-demote silent-fallback canary) can
// observe whether a pin REQUEST was DEMOTED to transient by the
// resident-byte-cap guard (resolved.go:761-772).
//
// Pre-0.30.236 the counters were only emitted in the resolved-cache
// 5-min summary slog line; that is sufficient for diagnosis but invisible
// to /debug/vars scrape — which the bench harness + post-deploy gate
// read. Without this surface, a successful pin-graceful-demote (the
// kill-switch / overflow degrade path) is silently hidden behind a
// transient-LRU regression that LOOKS identical to the F3 defect this
// ship closes.
//
// Idiom mirrors refresher_metrics.go / bindings_by_gvr_metrics.go:
//
//   - sync.Once gate (expvar.Publish panics on duplicate key).
//   - Disabled() gate at init() — the keys MUST NOT appear under
//     CACHE_ENABLED=false (transparent-fallback contract,
//     project_cache_off_is_transparent_fallback).
//   - expvar.Func — lazy at scrape time; zero per-/call cost; safe
//     when ResolvedCache() returns nil (pre-init or RESOLVED_CACHE_ENABLED=false).
//
// COST PER SCRAPE: four atomic.Uint64.Load + one Stats() snapshot under
// the cache mu (read-only — Lock()/Unlock() of c.mu, ~microseconds).
// Scrape rate is ~every 10-60s; the 1.0× per-/call hot path is
// unchanged. Surfacing the counters does NOT touch the resolve / Put /
// Get path.

package cache

import (
	"expvar"
	"sync"
)

var resolvedCacheMetricsOnce sync.Once

func init() {
	if Disabled() {
		return
	}
	registerResolvedCacheMetrics()
}

// registerResolvedCacheMetrics publishes the resident-region counter
// expvar keys. Guarded by sync.Once so it is safe to call from both
// init() and a test helper.
func registerResolvedCacheMetrics() {
	resolvedCacheMetricsOnce.Do(func() {
		// resident_demote_total — Ship 0.30.236 T8. A Put that REQUESTED
		// a pin (entry.Pinned == true at the call site) but was stored
		// TRANSIENT instead, either because:
		//   - maxResidentBytes <= 0 (kill-switch: pinning disabled), or
		//   - the resident byte budget would overflow
		//     (curResidentBytes - priorResident + bytes > maxResidentBytes).
		// A non-zero delta during the post-deploy gate window means the
		// fix is firing but the resident budget is too small — operators
		// must raise RESOLVED_CACHE_MAX_RESIDENT_BYTES before declaring
		// PASS.
		expvar.Publish("snowplow_resident_demote_total", expvar.Func(func() any {
			c := ResolvedCache()
			if c == nil {
				return uint64(0)
			}
			return c.residentDemoteTotal.Load()
		}))
		// resident_pin_total — Puts that landed in the resident region
		// (entry.Pinned honoured). Pairs with demote_total for the
		// budget-fit ratio: pin/(pin+demote) is the resident-coverage
		// signal. Surfaced as a free side-along to demote so the gate
		// can compute the ratio in one scrape.
		expvar.Publish("snowplow_resident_pin_total", expvar.Func(func() any {
			c := ResolvedCache()
			if c == nil {
				return uint64(0)
			}
			return c.residentPinTotal.Load()
		}))
		// resident_bytes / max_resident_bytes — live live resident-region
		// occupancy + cap. Lets the gate compute headroom directly
		// instead of inferring from the demote counter alone.
		expvar.Publish("snowplow_resident_bytes", expvar.Func(func() any {
			c := ResolvedCache()
			if c == nil {
				return int64(0)
			}
			return c.Stats().ResidentBytes
		}))
		expvar.Publish("snowplow_resident_max_bytes", expvar.Func(func() any {
			c := ResolvedCache()
			if c == nil {
				return int64(0)
			}
			return c.Stats().MaxResidentBytes
		}))
	})
}

// RegisterResolvedCacheMetricsForTest forces registration under tests
// that flip CACHE_ENABLED=true via t.Setenv after init() already ran
// with CACHE_ENABLED unset. Idempotent (sync.Once-guarded). Production
// callers MUST NOT use this function.
func RegisterResolvedCacheMetricsForTest() {
	registerResolvedCacheMetrics()
}
