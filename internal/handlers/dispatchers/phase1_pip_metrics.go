// phase1_pip_metrics.go — Ship PIP (0.30.173) observability counters.
//
// Two grand-total counters + two per-cohort maps, exposed via expvar so
// the existing snowplow expvar pipeline (the tester scrapes /debug/vars)
// picks them up automatically. Mirrors the existing pattern in
// internal/cache/fallthrough_meter_expvar.go (the prior art for this
// shape).
//
// REGISTERED VIA init(). Same gate as the fallthrough meter: under
// CACHE_ENABLED=false the counters MUST NOT be registered so they don't
// appear at /debug/vars even with zero values (cache-off transparent-
// fallback contract — project_cache_off_is_transparent_fallback.md).
//
// PER-COHORT MAPS are exposed as a single expvar.Func returning a
// map[string]uint64 — same shape as snowplow_apiserver_fallthrough_cells.
// The cohort label is the canonical cohortLogLabel (Username for
// user-kind cohorts, "group:<name>" for group-kind cohorts). PIP itself
// is gated such that under PREWARM_PIP_ENABLED=false the counters
// register at zero and never increment — operator sees fresh zeros
// rather than a missing key.
//
// Ship 0.30.187 D1: two additional observability surfaces address the
// 0.30.186 silent widget-seed-failure mode where a single cohort's
// widget seed errors (e.g. RBAC denial on a particular widget for the
// group:devs cohort) were logged but produced no operator-visible
// signal of WHICH widget broke WHICH cohort.
//
//   - snowplow_phase1_widget_seed_failure_total
//       expvar.Func returning map["cohort|widget_name|gvr"] -> uint64
//       Bumped at every seedOneWidget error.
//   - snowplow_phase1_restaction_seed_failure_total
//       Symmetric counter for restaction failures (same defect class).
//   - snowplow_phase1_cohort_seed_status
//       expvar.Func returning map["cohort"] -> "success"|"partial"|"failed"
//       Set per cohort goroutine: "success" iff every restaction+widget
//       Put completed; "partial" iff any per-(restaction/widget) error
//       was recorded; "failed" iff the cohort timed out or errored
//       fatally before reaching any target.

package dispatchers

import (
	"expvar"
	"sync"
	"sync/atomic"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// Grand totals across all cohorts.
var (
	pipSeedRestactionsTotal atomic.Uint64
	pipSeedWidgetsTotal     atomic.Uint64

	// Ship A.3 / 0.30.179 — binding-set enumeration counters. The first
	// two are the seed-loop's grand totals (restactions + widgets across
	// every enumerated class); the third is the failure counter per
	// AC-178 §"Three expvar counters". The bindingset-class count itself
	// + the powerset-skipped counter are exposed via accessors on
	// internal/cache (single source of truth).
	pipBindingSetSeedResolvesTotal atomic.Uint64
	pipBindingSetSeedFailuresTotal atomic.Uint64
)

// Per-cohort counters. sync.Map keyed by cohort label, value is
// *atomic.Uint64. Same shape as fallthroughCounters in the prior-art
// meter.
var (
	pipSeedRestactionsByCohort sync.Map
	pipSeedWidgetsByCohort     sync.Map
)

// Ship 0.30.187 D1 — per-(cohort, target) failure maps. Keyed by
// "cohort|name|gvr" (widgets) or "cohort|namespace/name" (restactions);
// value is *atomic.Uint64. The composite key keeps the per-cohort and
// per-target dimensions independent: an operator inspecting
// /debug/vars sees the exact widget that broke the exact cohort.
//
// Per-cohort seed status. Keyed by cohort label; value is a *atomic.Pointer[string]
// holding "success", "partial", or "failed". A pointer to an interned
// string keeps the load/store atomic.
var (
	pipWidgetSeedFailureByKey     sync.Map
	pipRestactionSeedFailureByKey sync.Map
	pipCohortSeedStatus           sync.Map
)

// cohort seed status constants. Kept as package-level strings so the
// expvar Func returns stable values (no per-call allocation).
const (
	cohortStatusSuccess = "success"
	cohortStatusPartial = "partial"
	cohortStatusFailed  = "failed"
)

// incFailureCounter bumps the per-(cohort, target) failure counter.
// Same lazy-allocation shape as incCohortCounter.
func incFailureCounter(m *sync.Map, compositeKey string) {
	if v, ok := m.Load(compositeKey); ok {
		v.(*atomic.Uint64).Add(1)
		return
	}
	fresh := new(atomic.Uint64)
	actual, loaded := m.LoadOrStore(compositeKey, fresh)
	if loaded {
		actual.(*atomic.Uint64).Add(1)
		return
	}
	fresh.Add(1)
}

// recordCohortSeedStatus writes the per-cohort seed status. Idempotent:
// "partial" overrides "success" only; "failed" overrides any prior
// status. The recorder is called from a single goroutine per cohort
// (the runPIPSeed errgroup task) so no cross-goroutine race exists, but
// the sync.Map keeps the published view consistent for the expvar
// reader.
func recordCohortSeedStatus(cohortLabel, status string) {
	// Last-write-wins is acceptable here: per cohort there's exactly one
	// writer (the errgroup goroutine). The architect's intent is that
	// the FINAL status set by that goroutine is the published status.
	pipCohortSeedStatus.Store(cohortLabel, status)
}

// incCohortCounter increments the per-cohort counter for the given
// label. The counter is lazily allocated on first observation; the
// Loaded/Store pattern matches the fallthrough meter's
// recordFallthroughCounter (LoadOrStore + Add).
func incCohortCounter(m *sync.Map, label string) {
	if v, ok := m.Load(label); ok {
		v.(*atomic.Uint64).Add(1)
		return
	}
	fresh := new(atomic.Uint64)
	actual, loaded := m.LoadOrStore(label, fresh)
	if loaded {
		actual.(*atomic.Uint64).Add(1)
		return
	}
	fresh.Add(1)
}

// pipMetricsOnce guards the expvar.Publish calls — same pattern as
// fallthroughExpvarOnce (sync.Once prevents double-publish panic if
// init() runs twice, e.g. in some test harnesses).
var pipMetricsOnce sync.Once

func init() {
	// CFG-1 mirror: under CACHE_ENABLED=false the cache subsystem does
	// not exist and PIP counters MUST NOT be registered. cache.Disabled()
	// is authoritative at init() time (Go runtime populates env vars
	// before package init() runs).
	if cache.Disabled() {
		return
	}
	registerPIPMetrics()
}

// registerPIPMetrics performs the expvar.Publish calls for the PIP
// counters. Guarded by pipMetricsOnce so it is safe to call from both
// init() and a test helper.
func registerPIPMetrics() {
	pipMetricsOnce.Do(func() {
		expvar.Publish("snowplow_phase1_seed_restactions_total", expvar.Func(func() any {
			return pipSeedRestactionsTotal.Load()
		}))
		expvar.Publish("snowplow_phase1_seed_widgets_total", expvar.Func(func() any {
			return pipSeedWidgetsTotal.Load()
		}))
		expvar.Publish("snowplow_phase1_seed_restactions_by_cohort", expvar.Func(func() any {
			out := map[string]uint64{}
			pipSeedRestactionsByCohort.Range(func(k, v any) bool {
				out[k.(string)] = v.(*atomic.Uint64).Load()
				return true
			})
			return out
		}))
		expvar.Publish("snowplow_phase1_seed_widgets_by_cohort", expvar.Func(func() any {
			out := map[string]uint64{}
			pipSeedWidgetsByCohort.Range(func(k, v any) bool {
				out[k.(string)] = v.(*atomic.Uint64).Load()
				return true
			})
			return out
		}))

		// Ship 0.30.242 H.c-layered Phase 2b — binding-set classes /
		// powerset-skipped counters DELETED. The underlying functions
		// (cache.Phase1EnumBindingsetClassesTotal,
		// cache.Phase1EnumPowersetSkippedTotal) lived in the deleted
		// binding_set_enumeration.go; their counters incremented from
		// code paths that no longer exist. Keeping the expvar
		// registrations would publish always-zero values (misleading
		// observability — design-gap fix Gap 4).
		//
		// The per-target seed resolves / failures counters KEPT (they
		// now count per-binding-target instead of per-cohort, same shape
		// — see runPIPSeed for the seedTarget loop).
		expvar.Publish("snowplow_phase1_bindingset_seed_resolves_total", expvar.Func(func() any {
			return pipBindingSetSeedResolvesTotal.Load()
		}))
		expvar.Publish("snowplow_phase1_bindingset_seed_failures_total", expvar.Func(func() any {
			return pipBindingSetSeedFailuresTotal.Load()
		}))

		// Ship 0.30.187 D1 — per-(cohort, target) seed-failure maps so
		// operators see WHICH widget/restaction broke WHICH cohort.
		// Composite key shape: "cohort|name|gvr" (widgets) /
		// "cohort|namespace/name" (restactions).
		expvar.Publish("snowplow_phase1_widget_seed_failure_total", expvar.Func(func() any {
			out := map[string]uint64{}
			pipWidgetSeedFailureByKey.Range(func(k, v any) bool {
				out[k.(string)] = v.(*atomic.Uint64).Load()
				return true
			})
			return out
		}))
		expvar.Publish("snowplow_phase1_restaction_seed_failure_total", expvar.Func(func() any {
			out := map[string]uint64{}
			pipRestactionSeedFailureByKey.Range(func(k, v any) bool {
				out[k.(string)] = v.(*atomic.Uint64).Load()
				return true
			})
			return out
		}))
		// Per-cohort seed status: "success" | "partial" | "failed".
		expvar.Publish("snowplow_phase1_cohort_seed_status", expvar.Func(func() any {
			out := map[string]string{}
			pipCohortSeedStatus.Range(func(k, v any) bool {
				out[k.(string)] = v.(string)
				return true
			})
			return out
		}))
	})
}
