// l1_lookup_metrics.go — Ship OBS-1 (0.30.186): per-(handlerKind, gvr)
// L1 hit/miss counters for the resolved-output cache lookup sites in
// restactions.go + widgets.go. Exposed via expvar so /debug/vars
// reports them.
//
// PRIOR ART
//
//   - internal/handlers/dispatchers/phase1_pip_metrics.go:94-127 —
//     established sync.Map-of-*atomic.Uint64 + expvar.Func returning
//     map[string]uint64 idiom. We follow the same shape but key
//     entries by `handlerKind|gvrString` and split each entry's value
//     into hit_total/miss_total via a small inner struct.
//   - internal/cache/cohort_gate_memo_metrics.go:99 — established
//     pattern for an expvar.Func that walks live runtime state at
//     scrape time.
//
// SHAPE
//
// One expvar.Func published at key "snowplow_dispatch_l1_lookups".
// The Func returns a map[string]map[string]uint64 keyed first by
// "<handlerKind>|<gvrString>" (e.g. "widgets|widgets.ui.krateo.io/v1beta1,
// widgets") and inside by "hit_total" + "miss_total". This is
// idiomatic JSON-serialisable shape and matches the cells layout
// used by snowplow_apiserver_fallthrough_cells.
//
// CARDINALITY
//
// (handlerKind ∈ {restactions, widgets, widgetContent}) × (GVR set,
// ≤ 30 in cluster — empirically verified in
// /tmp/snowplow-runs expvar_post_burst.json apiserver_fallthrough_cells
// keyset). Worst-case ≈ 90 entries; safe at scrape cost.
//
// COST PER /call
//
// One sync.Map.Load (or LoadOrStore on first observation of a
// (handlerKind, gvrString) pair) + one atomic.Uint64.Add. ~1.0×
// amplification (a single bump per dispatch L1 lookup); already on
// the request-path serial budget.

package dispatchers

import (
	"expvar"
	"sync"
	"sync/atomic"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// l1LookupCell holds the hit + miss counters for one
// (handlerKind, gvrString) pair.
type l1LookupCell struct {
	hit  atomic.Uint64
	miss atomic.Uint64
}

// l1LookupCells is the sync.Map keyed by "<handlerKind>|<gvrString>"
// → *l1LookupCell. Lazily populated on first observation; never
// pruned (cardinality is bounded by the GVR set × handler kinds).
var l1LookupCells sync.Map

// recordL1Lookup bumps the hit or miss counter for the given
// (handlerKind, gvrString) pair. Called from emitResolvedCacheLookup
// after the falsifier log line is emitted, so it is invariant to the
// log path (an operator filtering on the log still sees the same data
// they see at /debug/vars).
//
// LoadOrStore pattern matches phase1_pip_metrics.go's
// incCohortCounter so a concurrent first-observation from N workers
// converges on one cell.
func recordL1Lookup(handlerKind, gvrString string, hit bool) {
	if handlerKind == "" {
		return
	}
	key := handlerKind + "|" + gvrString
	cell, ok := l1LookupCells.Load(key)
	if !ok {
		fresh := &l1LookupCell{}
		actual, loaded := l1LookupCells.LoadOrStore(key, fresh)
		if loaded {
			cell = actual
		} else {
			cell = fresh
		}
	}
	c := cell.(*l1LookupCell)
	if hit {
		c.hit.Add(1)
	} else {
		c.miss.Add(1)
	}
}

// l1LookupMetricsOnce guards the expvar.Publish call — same pattern
// as pipMetricsOnce / cohortMemoMetricsOnce. expvar.Publish panics on
// duplicate key; sync.Once prevents that.
var l1LookupMetricsOnce sync.Once

func init() {
	// CFG-1 mirror: under CACHE_ENABLED=false the dispatchers do not
	// look up L1 (the cacheHandle is nil at the call sites in
	// restactions.go + widgets.go), so the counters would forever read
	// zero. Keep them un-published to preserve the cache-off
	// transparent-fallback contract.
	if cache.Disabled() {
		return
	}
	registerL1LookupMetrics()
}

// registerL1LookupMetrics publishes the single expvar key. Guarded by
// l1LookupMetricsOnce so it is safe to call from both init() and any
// future test helper.
func registerL1LookupMetrics() {
	l1LookupMetricsOnce.Do(func() {
		expvar.Publish("snowplow_dispatch_l1_lookups", expvar.Func(func() any {
			out := map[string]map[string]uint64{}
			l1LookupCells.Range(func(k, v any) bool {
				ks, _ := k.(string)
				cell, _ := v.(*l1LookupCell)
				if cell == nil {
					return true
				}
				out[ks] = map[string]uint64{
					"hit_total":  cell.hit.Load(),
					"miss_total": cell.miss.Load(),
				}
				return true
			})
			return out
		}))
	})
}

// RegisterL1LookupMetricsForTest forces registration under tests that
// flip CACHE_ENABLED=true via t.Setenv after init() already ran with
// CACHE_ENABLED unset. Idempotent (sync.Once-guarded). Production
// callers MUST NOT use this function.
func RegisterL1LookupMetricsForTest() {
	registerL1LookupMetrics()
}
