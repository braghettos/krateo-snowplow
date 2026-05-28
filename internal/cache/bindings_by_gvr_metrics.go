// bindings_by_gvr_metrics.go — Ship 1 expvar surfaces.
//
// Two read-only expvar publishers, same idiom as refresher_metrics.go
// (expvar.Publish + expvar.Func + sync.Once, gated by Disabled() so the
// keys never appear under CACHE_ENABLED=false — transparent-fallback
// contract, project_cache_off_is_transparent_fallback):
//
//   - snowplow_ra_full_list_serve — the RAFullList serve-outcome counters
//     (Hit / Repopulate / VerifiedSlice / Fallback). CLAUSE-5 measurability:
//     the falsifier asserts admin's first compositions /call drives a Hit
//     (+1) over a warm prewarm-pinned cell. Pre-Ship-1 these counters
//     existed (ra_full_list_store.go) but were NOT in expvar, so the
//     tester could not assert the cheap-serve path over the LB.
//
//   - snowplow_bindings_by_gvr_delta_skipped_non_typed — the S1 index
//     drift canary: a non-zero value means a delta event object was
//     neither typed nor convertible and was DROPPED (the index will drift
//     until the next boot rebuild). Surfacing it makes the drift
//     observable instead of silent.
//
// All values are expvar.Func — evaluated lazily at scrape time, zero
// per-/call cost.

package cache

import (
	"expvar"
	"sync"
)

var bindingsByGVRMetricsOnce sync.Once

func init() {
	// CFG-1 mirror: under CACHE_ENABLED=false the index + RAFullList store
	// do not run, so these keys MUST NOT be registered.
	if Disabled() {
		return
	}
	registerBindingsByGVRMetrics()
}

// registerBindingsByGVRMetrics publishes the Ship 1 expvar keys. Guarded
// by sync.Once (expvar.Publish panics on a duplicate key) so it is safe to
// call from both init() and the test helper.
func registerBindingsByGVRMetrics() {
	bindingsByGVRMetricsOnce.Do(func() {
		// CLAUSE-5 — the RAFullList serve-outcome map. The tester asserts a
		// Hit delta of 1 on admin's first compositions /call (served from
		// the warm prewarm-pinned full-list cell as a cheap Go-slice).
		expvar.Publish("snowplow_ra_full_list_serve", expvar.Func(func() any {
			s := RAFullListServeSnapshot()
			return map[string]uint64{
				"hit":            s.Hit,
				"repopulate":     s.Repopulate,
				"verified_slice": s.VerifiedSlice,
				"fallback":       s.Fallback,
			}
		}))
		// S1 — the index drift canary.
		expvar.Publish("snowplow_bindings_by_gvr_delta_skipped_non_typed", expvar.Func(func() any {
			return BindingsIndexDeltaSkippedNonTyped()
		}))
	})
}

// RegisterBindingsByGVRMetricsForTest forces registration under tests that
// flip CACHE_ENABLED=true after init() already ran. Idempotent. Production
// callers MUST NOT use this.
func RegisterBindingsByGVRMetricsForTest() {
	registerBindingsByGVRMetrics()
}
