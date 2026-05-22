// fallthrough_meter_expvar.go — Ship D (0.30.141). expvar exposure
// for `snowplow_apiserver_fallthrough_total` and the per-cell
// (path, gvr, reason) breakdown. Mirrors the existing snowplow
// metric-exposure pattern (informer_dispatch_metrics.go +
// resolved.go `startResolvedCacheSummary`).
//
// REGISTRATION TIME. The expvar handles are registered in `init()` so
// any process that imports this package picks them up. The registry
// keys are stable for log-aggregation and grep tooling:
//
//   - snowplow_apiserver_fallthrough_total — grand-total uint64.
//   - snowplow_apiserver_fallthrough_cells  — per-cell breakdown
//     as a map[string]uint64, key `"path|gvr|reason"`.
//
// expvar is the existing pattern. No new dependency.
//
// CFG-1 (Ship 0.30.163) — cache-off compliance per project memory
// `project_cache_off_is_transparent_fallback`. Diego's 2026-05-22
// contract: "there is no cache with cache_enabled=false". Under
// CACHE_ENABLED=false the cache subsystem does not exist and these
// gauges MUST NOT be registered (so they don't appear at /debug/vars
// even with empty values). The gate is at init() time: Go runtime
// populates env vars BEFORE package init() runs, so Disabled() is
// authoritative here.
package cache

import (
	"expvar"
	"sync"
	"sync/atomic"
)

// fallthroughExpvarOnce guards registerFallthroughExpvar so the
// registration body runs at most once per process even if invoked from
// both init() (production) and RegisterExpvarForTest (in-process tests
// that boot with CACHE_ENABLED unset and later flip it via t.Setenv).
// expvar.Publish panics on duplicate key; sync.Once prevents that.
var fallthroughExpvarOnce sync.Once

func init() {
	// CFG-1: under CACHE_ENABLED=false, no cache subsystem exists →
	// gauges must not be registered. init() runs once per process so
	// this branch cannot be unit-tested in-process; falsifier is
	// HG-321 (4-env-value matrix process spawn, see
	// e2e/bench/cfg1_falsifier.sh).
	if Disabled() {
		return
	}
	registerFallthroughExpvar()
}

// registerFallthroughExpvar performs the three expvar.Publish calls
// for the fallthrough meter. Guarded by fallthroughExpvarOnce so it
// is safe to call from both init() and the test helper.
func registerFallthroughExpvar() {
	fallthroughExpvarOnce.Do(func() {
		expvar.Publish("snowplow_apiserver_fallthrough_total", expvar.Func(func() any {
			return fallthroughTotal.Load()
		}))
		expvar.Publish("snowplow_assertion_violations_total", expvar.Func(func() any {
			// Ship D — only one check is wired today (read_paths_scoped),
			// so the expvar value is the per-check map shape ready for
			// future expansion.
			return map[string]uint64{
				"read_paths_scoped": assertionViolationsTotal.Load(),
			}
		}))
		expvar.Publish("snowplow_apiserver_fallthrough_cells", expvar.Func(func() any {
			out := map[string]uint64{}
			fallthroughCounters.Range(func(k, v any) bool {
				key := k.(fallthroughKey)
				// Pipe-separated label tuple — none of the three label
				// values contains a pipe (path is a closed enum; gvr
				// uses `/`; reason is a closed enum).
				out[key.path+"|"+key.gvr+"|"+string(key.reason)] = v.(*atomic.Uint64).Load()
				return true
			})
			return out
		}))
	})
}
