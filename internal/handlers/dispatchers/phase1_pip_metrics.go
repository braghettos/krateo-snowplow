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
	// Ship A.3 / 0.30.179 — binding-set enumeration counters. The first
	// two are the seed-loop's grand totals (restactions + widgets across
	// every enumerated class); the third is the failure counter per
	// AC-178 §"Three expvar counters". The bindingset-class count itself
	// + the powerset-skipped counter are exposed via accessors on
	// internal/cache (single source of truth).
	pipBindingSetSeedResolvesTotal atomic.Uint64
	pipBindingSetSeedFailuresTotal atomic.Uint64

	// #158 (P9-B) — the SPLIT of pipBindingSetSeedFailuresTotal by error
	// class. pipBindingSetSeedFailuresTotal is KEPT as the back-compat
	// grand total (= rbac_deny + operational) so existing dashboards do
	// not break; these two carry the discriminated signal:
	//
	//   - pipSeedRBACDenyTotal (snowplow_phase1_seed_rbac_deny_total):
	//     EXPECTED narrow-RBAC denies (403/401). A non-zero value here is
	//     normal — these cohorts genuinely cannot read the seed target and
	//     need no L1 entry. NOT re-enqueued.
	//   - pipSeedOperationalFailTotal (snowplow_phase1_seed_operational_fail_total):
	//     UNEXPECTED operational failures (ctx timeout/cancel, 5xx,
	//     transport, panic, fail-loud default). A non-zero value is a real
	//     hole the operator can act on; these ARE re-enqueued (legacy:
	//     bounded retry; engine: coalesced boot re-walk).
	//
	// Both call sites (runPIPSeed + seedScopeYielding) feed these via
	// classifySeedErr — see phase1_seed_classify.go.
	pipSeedRBACDenyTotal        atomic.Uint64
	pipSeedOperationalFailTotal atomic.Uint64
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

// incFailureCounter bumps the per-(cohort, target) failure counter. Lazy
// allocation on first observation via the LoadOrStore + Add pattern (same
// shape as the fallthrough meter's recordFallthroughCounter prior art).
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
		// Task #101 (hygiene) — the four legacy seed totals
		// (snowplow_phase1_seed_{restactions,widgets}_total and their two
		// _by_cohort maps) DELETED. Their only incrementer was runPIPSeed
		// (the legacy seed loop, PrewarmEngineEnabled()==false). Production
		// runs the unified engine path (PrewarmEngineEnabled()==true), which
		// bypasses runPIPSeed entirely (phase1_pip_seed.go:494, phase1_walk.go:409)
		// — so these four counters were permanently zero in production
		// (confirmed live: 0/0/{}/{} while prewarm_engine_processed_total>0).
		// Always-zero expvars are misleading observability. The LIVE seed
		// signals — the failure-classification family (rbac_deny /
		// operational_fail / per-(cohort,target) maps / cohort_seed_status)
		// published below — are populated on BOTH paths and stay.

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

		// #158 (P9-B) — the discriminated split of the failures total. The
		// back-compat grand total above stays = rbac_deny + operational so
		// existing dashboards don't break; these two let an operator tell an
		// EXPECTED narrow-RBAC deny from a REAL operational hole.
		expvar.Publish("snowplow_phase1_seed_rbac_deny_total", expvar.Func(func() any {
			return pipSeedRBACDenyTotal.Load()
		}))
		expvar.Publish("snowplow_phase1_seed_operational_fail_total", expvar.Func(func() any {
			return pipSeedOperationalFailTotal.Load()
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
