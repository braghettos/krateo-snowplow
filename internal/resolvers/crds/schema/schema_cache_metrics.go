// schema_cache_metrics.go — Task #326. Expose the per-GVR compiled-CRD-schema
// memo counters (declared at schema_cache.go:137-142, snapshotted by
// crdSchemaMemoStats) via the snowplow_crd_schema_memo_* expvar family so
// /debug/vars reports them. Read-only — zero changes to the lookup / store /
// invalidate hot path; the counters are bumped in schema_cache.go and merely
// surfaced here.
//
// PRIOR ART — mirrors internal/cache/refresher_metrics.go exactly:
//   - expvar.Publish + expvar.Func + sync.Once register-once idiom.
//   - init() gated by cache.Disabled() so the keys do NOT appear under
//     CACHE_ENABLED=false (transparent-fallback contract —
//     project_cache_off_is_transparent_fallback). expvar.Func is lazy — it
//     reads crdSchemaMemoStats() at scrape time, so registration is safe even
//     before the memo is warmed (a pre-warm scrape returns zeros).
//
// PLACEMENT JUSTIFICATION (the layering note in the brief) — the atomics are
// package-private to schema (schema_cache.go), and the schema package ALREADY
// imports internal/cache (schema.go:9 uses cache.GVRFor / cache.IsResolverGVRHit
// and gates here on cache.Disabled()). cache does NOT import schema, so
// registering the expvars from WITHIN the schema package is cycle-free and is
// the cleanest existing precedent: cache/refresher_metrics.go registers cache's
// own atomics, dispatchers/phase1_pip_metrics.go registers dispatchers' own
// atomics — each package owns its counters and publishes them in-package. No
// reader needs to be exported across the package boundary (crdSchemaMemoStats()
// is read directly here, same package).

package schema

import (
	"expvar"
	"sync"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// crdSchemaMemoMetricsOnce guards expvar.Publish so registration runs at most
// once per process even if both init() and the test helper invoke it.
// expvar.Publish panics on a duplicate key; sync.Once prevents that.
var crdSchemaMemoMetricsOnce sync.Once

func init() {
	// CFG-1 mirror (same gate as the cache-side expvar publishers): under
	// CACHE_ENABLED=false the memo is never warmed on the hot path and these
	// counters MUST NOT be registered.
	if cache.Disabled() {
		return
	}
	registerCRDSchemaMemoMetrics()
}

// registerCRDSchemaMemoMetrics performs the expvar.Publish calls for the four
// CRD-schema-memo observability keys. Guarded by crdSchemaMemoMetricsOnce so
// it is safe to call from init() and from RegisterCRDSchemaMemoMetricsForTest.
//
// All values are expvar.Func — evaluated lazily at scrape time, so there is no
// per-/call cost; crdSchemaMemoStats() is a four-atomic Load read on demand.
func registerCRDSchemaMemoMetrics() {
	crdSchemaMemoMetricsOnce.Do(func() {
		expvar.Publish("snowplow_crd_schema_memo_hits_total", expvar.Func(func() any {
			return crdSchemaMemoStats().Hits
		}))
		expvar.Publish("snowplow_crd_schema_memo_misses_total", expvar.Func(func() any {
			return crdSchemaMemoStats().Misses
		}))
		// _stale_dropped_total — the generation-fence drop count (a fenced
		// store dropped because a CRD-lifecycle reset moved the generation
		// during the GET+compile window). A non-zero value is expected under a
		// concurrent CRD install; a steadily climbing value with no installs
		// would indicate generation churn.
		expvar.Publish("snowplow_crd_schema_memo_stale_dropped_total", expvar.Func(func() any {
			return crdSchemaMemoStats().StaleDropped
		}))
		// _invalidations_total — the full-reset count (InvalidateCRDSchemaMemo
		// fired by the CRD-lifecycle bridge). Named _invalidations_total per
		// the brief; backed by the crdSchemaMemoResets atomic (Resets in the
		// snapshot struct — "reset" and "invalidation" are the same event:
		// the bridge-keyed full clear).
		expvar.Publish("snowplow_crd_schema_memo_invalidations_total", expvar.Func(func() any {
			return crdSchemaMemoStats().Resets
		}))
	})
}

// RegisterCRDSchemaMemoMetricsForTest forces CRD-schema-memo expvar
// registration under tests that flip CACHE_ENABLED=true via t.Setenv after
// init() already ran with CACHE_ENABLED unset. Idempotent (sync.Once-guarded).
// Production callers MUST NOT use this function.
func RegisterCRDSchemaMemoMetricsForTest() {
	registerCRDSchemaMemoMetrics()
}
