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

		// Ship A.3 / 0.30.179 — binding-set enumeration counters.
		expvar.Publish("snowplow_phase1_bindingset_classes_total", expvar.Func(func() any {
			return cache.Phase1EnumBindingsetClassesTotal()
		}))
		expvar.Publish("snowplow_phase1_bindingset_seed_resolves_total", expvar.Func(func() any {
			return pipBindingSetSeedResolvesTotal.Load()
		}))
		expvar.Publish("snowplow_phase1_bindingset_seed_failures_total", expvar.Func(func() any {
			return pipBindingSetSeedFailuresTotal.Load()
		}))
		expvar.Publish("snowplow_phase1_enum_powerset_skipped", expvar.Func(func() any {
			return cache.Phase1EnumPowersetSkippedTotal()
		}))
	})
}
