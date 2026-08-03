// refresh_broadcaster_expvar.go — Ship 1 (live-refresh-coherence, option A).
//
// expvar exposure for the live-refresh broadcaster counters at
// /debug/vars/snowplow_refresh_broadcaster. Mirrors the established cache
// expvar pattern (crd_discovery_expvar.go, fallthrough_meter_expvar.go):
// sync.Once-guarded expvar.Publish from init(), skipped under
// CACHE_ENABLED=false (the cache subsystem does not exist there, so these
// gauges MUST NOT register — project_cache_off_is_transparent_fallback).
//
// These counters back the falsifier artifacts: refreshDeliveredTotal proves
// signals reach subscribers (9.2), refreshDroppedTotal proves the slow-
// consumer drop arm fired without stalling the refresher (9.6), and
// subscribers is the connection-scale gauge (design §10).

package cache

import (
	"expvar"
	"sync"
)

var refreshBroadcasterExpvarOnce sync.Once

func init() {
	if Disabled() {
		return
	}
	registerRefreshBroadcasterExpvar()
}

// registerRefreshBroadcasterExpvar publishes the broadcaster counters. The
// handler reads the atomics via RefreshBroadcasterCounters so every scrape
// observes a coherent point-in-time snapshot.
func registerRefreshBroadcasterExpvar() {
	refreshBroadcasterExpvarOnce.Do(func() {
		expvar.Publish("snowplow_refresh_broadcaster", expvar.Func(func() any {
			published, delivered, dropped, coalesced := RefreshBroadcasterCounters()
			return map[string]any{
				"published":   published,
				"delivered":   delivered,
				"dropped":     dropped,
				"coalesced":   coalesced,
				"subscribers": RefreshSubscriberCount(),
			}
		}))
	})
}
