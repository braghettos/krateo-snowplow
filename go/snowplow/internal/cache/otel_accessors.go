// otel_accessors.go — exported, read-only accessors over cache-package
// counters that already back expvar but whose snapshot helpers are
// package-private (refresherStatsSnapshot, controllerHealthSnap). The
// internal/metrics OTLP mirror (a separate package) reads these at OTLP
// collection time to observe the SAME live snapshots the expvar `.Func`
// closures read.
//
// ADDITIVE: pure read accessors. They change no incrementer, no populate
// path, and the expvar surface is untouched.

package cache

// RefresherSnapshot returns the background re-resolve worker-pool counters,
// mirroring the snowplow_refresher_* expvar family. queueDepth is the live
// workqueue Len(). Safe before StartRefresher: refresherStatsSnapshot reads
// the singleton lazily and returns zeros when the pool is not yet built.
func RefresherSnapshot() (enqueued, completed, failed, retried, dropped,
	skippedNoEntry, skippedNoHandler, skippedStageError,
	yielded, capped, floored uint64, queueDepth int64) {

	s := refresherStatsSnapshot()
	r := refresherInstance
	if r != nil && r.queue != nil {
		queueDepth = int64(r.queue.Len())
	}
	return s.enqueued, s.completed, s.failed, s.retried, s.dropped,
		s.skippedNoEntry, s.skippedNoHandler, s.skippedStageError,
		s.yielded, s.capped, s.floored, queueDepth
}

// UpstreamHealthSnapshot collapses the per-controller controller-health
// snapshot into bounded aggregate gauges suitable for OTLP, mirroring the
// operationally-significant signal of snowplow_upstream_controller_health
// (every entry Healthy=1) and snowplow_upstream_webhook_failurepolicy (how
// many discovered webhooks carry a Fail policy). The per-name maps stay the
// expvar drill-down surface; OTLP gets the alarm-worthy counts so a
// dashboard can alert on "controllersUnhealthy > 0" or
// "webhooksFailPolicy > 0 on a degraded controller" without per-name
// cardinality.
//
// Returns all zeros when cache is off or no snapshot has been published yet.
func UpstreamHealthSnapshot() (controllersHealthy, controllersUnhealthy,
	webhooksTotal, webhooksFailPolicy int64) {

	if Disabled() {
		return 0, 0, 0, 0
	}
	s := controllerHealthSnap.Load()
	if s == nil {
		return 0, 0, 0, 0
	}
	for _, c := range s.Controllers {
		if c.Healthy == 1 {
			controllersHealthy++
		} else {
			controllersUnhealthy++
		}
	}
	for _, w := range s.Webhooks {
		webhooksTotal++
		if w.Policy == "Fail" {
			webhooksFailPolicy++
		}
	}
	return controllersHealthy, controllersUnhealthy, webhooksTotal, webhooksFailPolicy
}
