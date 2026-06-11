// phase1_walk_metrics.go — Gate 1 (0.30.201-diag) boot children-count
// instrumentation for the prewarm discovery walk.
//
// PURPOSE — DIAGNOSTIC. Surface, per walked navigation widget, the
// children-count the discovery walk observed at boot-walk time so we can
// confirm the TRACED hypothesis that the dynamic-children navmenu
// (sidebar-nav-menu, ns krateo-system) resolves with children=0 at
// boot-walk time (vs warm=3) — and discriminate WHICH sub-cause fired.
// This decides whether a bounded post-sync re-walk suffices vs whether
// we need an event-driven re-harvest trigger.
//
// The per-widget record is captured at the SINGLE site in walk()
// immediately after `children := extractResourcesRefsItems(res.Object)`
// (phase1_walk.go) — recording the three counts the design's three
// sub-causes turn on:
//
//   - len(children)            — raw status.resourcesRefs.items[] count.
//   - recurseCount             — count passing walkShouldRecurse (verb==GET
//                                && non-empty path).
//   - parseCount               — count whose Path util.ParseCallPathToObjectRef
//                                accepts as a /call?... widget endpoint.
//
// EXPVAR — deterministic read over the LB (so Gate-1 capture is not a
// flaky log-tail). Mirrors the existing PIP-metrics + fallthrough-meter
// expvar pattern (phase1_pip_metrics.go / fallthrough_meter_expvar.go).
// Two surfaces:
//
//   - snowplow_phase1_walk_children expvar.Func returning
//     map["gvr|ns|name"] -> {depth, children, recurse, parse, observations}
//     — the per-widget last-observed counts (+ observation count for the
//     dedupe-aware re-resolve case).
//   - snowplow_phase1_walk_zero_children_total — grand-total count of
//     walked widgets that observed children==0 (the headline Gate-1
//     signal: a non-zero value here means at least one navigation widget
//     descended nothing).
//
// REGISTERED VIA init() with the SAME CFG-1 cache-off gate as the PIP
// metrics: under CACHE_ENABLED=false the cache subsystem does not exist
// and the gauges MUST NOT be registered (cache-off transparent-fallback
// contract — project_cache_off_is_transparent_fallback.md).
//
// PERMANENT-QUALITY (audit-instrumentation policy): the recorder is
// telemetry-only — it never changes walk behaviour. It is bounded by the
// navigation-widget set (~tens of widgets), so the sync.Map cardinality
// is tiny. This may survive into main.

package dispatchers

import (
	"expvar"
	"sync"
	"sync/atomic"

	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// phase1WalkChildrenEntry is the per-widget last-observed children-count
// record. All fields are plain ints/uint64 so the expvar JSON marshal is
// stable. A fresh copy is published each scrape (the reader builds a new
// map), so the stored record is never exposed to the marshaller directly.
type phase1WalkChildrenEntry struct {
	Depth        int    `json:"depth"`
	Children     int    `json:"children"`
	Recurse      int    `json:"recurse"`
	Parse        int    `json:"parse"`
	Observations uint64 `json:"observations"`
}

// phase1WalkChildren is keyed by "gvr|ns|name" (the walked widget
// identity), value is *atomic.Pointer[phase1WalkChildrenEntry]. The
// pointer-swap keeps load/store atomic without a per-entry mutex; the
// walk records each widget at most a handful of times (visited-set
// dedupe + cross-root re-resolve), so last-write-wins on the counts plus
// a monotonic Observations counter is the right shape.
var phase1WalkChildren sync.Map

// phase1WalkZeroChildrenTotal is the grand-total count of walk
// observations that saw children==0 — the headline Gate-1 signal.
var phase1WalkZeroChildrenTotal atomic.Uint64

// phase1WalkObservationsTotal is the grand-total count of walk
// observations (every recordWalkChildren call). Lets the reader compute
// the zero-children RATIO without enumerating the per-widget map.
var phase1WalkObservationsTotal atomic.Uint64

// recordWalkChildren is the Gate-1 recorder. Called from walk() once per
// widget resolution, immediately after the children slice is extracted.
// It computes the recurse-pass and parse-pass counts over the same
// children slice the walk descends, then publishes the per-widget record
// + bumps the grand totals. Telemetry-only: never changes walk behaviour.
//
// recurseCount mirrors the walk's own gate (walkShouldRecurse). parseCount
// mirrors the walk's own util.ParseCallPathToObjectRef accept. Computing
// them HERE (over the same slice, before the descent loop) keeps the
// counts byte-faithful to what the walk actually descends.
func recordWalkChildren(gvr schema.GroupVersionResource, ns, name string, depth int, children []navChildRef) {
	recurse := 0
	parse := 0
	for _, child := range children {
		if walkShouldRecurse(child) {
			recurse++
		}
		if _, ok := objects.ParseCallPathToObjectRef(child.Path); ok {
			parse++
		}
	}

	key := gvr.String() + "|" + ns + "|" + name

	var observations uint64 = 1
	if v, ok := phase1WalkChildren.Load(key); ok {
		prev := v.(*atomic.Pointer[phase1WalkChildrenEntry]).Load()
		if prev != nil {
			observations = prev.Observations + 1
		}
		v.(*atomic.Pointer[phase1WalkChildrenEntry]).Store(&phase1WalkChildrenEntry{
			Depth:        depth,
			Children:     len(children),
			Recurse:      recurse,
			Parse:        parse,
			Observations: observations,
		})
	} else {
		p := &atomic.Pointer[phase1WalkChildrenEntry]{}
		p.Store(&phase1WalkChildrenEntry{
			Depth:        depth,
			Children:     len(children),
			Recurse:      recurse,
			Parse:        parse,
			Observations: observations,
		})
		// LoadOrStore handles the rare concurrent-first-observe race (the
		// walk is single-goroutine per root but two roots can share a
		// subtree across goroutines in a future parallel walk; defensive).
		actual, loaded := phase1WalkChildren.LoadOrStore(key, p)
		if loaded {
			actual.(*atomic.Pointer[phase1WalkChildrenEntry]).Store(&phase1WalkChildrenEntry{
				Depth:        depth,
				Children:     len(children),
				Recurse:      recurse,
				Parse:        parse,
				Observations: observations,
			})
		}
	}

	phase1WalkObservationsTotal.Add(1)
	if len(children) == 0 {
		phase1WalkZeroChildrenTotal.Add(1)
	}
}

// phase1WalkMetricsOnce guards the expvar.Publish calls — same pattern as
// pipMetricsOnce / fallthroughExpvarOnce (sync.Once prevents the
// double-publish panic if init() runs twice in a test harness).
var phase1WalkMetricsOnce sync.Once

func init() {
	// CFG-1 mirror: under CACHE_ENABLED=false the cache subsystem does
	// not exist and these diagnostic gauges MUST NOT be registered.
	// cache.Disabled() is authoritative at init() time.
	if cache.Disabled() {
		return
	}
	registerPhase1WalkMetrics()
}

// registerPhase1WalkMetrics performs the expvar.Publish calls for the
// Gate-1 boot children-count gauges. Guarded by phase1WalkMetricsOnce so
// it is safe to call from both init() and a test helper.
func registerPhase1WalkMetrics() {
	phase1WalkMetricsOnce.Do(func() {
		expvar.Publish("snowplow_phase1_walk_children", expvar.Func(func() any {
			out := map[string]phase1WalkChildrenEntry{}
			phase1WalkChildren.Range(func(k, v any) bool {
				e := v.(*atomic.Pointer[phase1WalkChildrenEntry]).Load()
				if e != nil {
					out[k.(string)] = *e
				}
				return true
			})
			return out
		}))
		expvar.Publish("snowplow_phase1_walk_zero_children_total", expvar.Func(func() any {
			return phase1WalkZeroChildrenTotal.Load()
		}))
		expvar.Publish("snowplow_phase1_walk_observations_total", expvar.Func(func() any {
			return phase1WalkObservationsTotal.Load()
		}))
	})
}
