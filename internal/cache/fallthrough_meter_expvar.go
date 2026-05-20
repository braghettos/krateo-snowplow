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
package cache

import (
	"expvar"
	"sync/atomic"
)

func init() {
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
}
