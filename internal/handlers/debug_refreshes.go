package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// debugRefreshesBody is the JSON body returned by /debug/refreshes.
//
// AGGREGATE-ONLY (#61, C-3): process-wide totals + the live subscriber count.
// It DELIBERATELY exposes NO per-subscription-key or per-identity breakdown —
// a per-key dump would be a cross-user signal (which keys are armed reveals
// which widgets/resources a connected user is watching). The four counters +
// the connection count are identity-free and safe to expose at operator level.
type debugRefreshesBody struct {
	// SSEEnabled mirrors the live-refresh SSE layer toggle. When false the
	// broadcaster hub is nil and the counters never move; the endpoint still
	// returns 200 so it is usable as a diagnostic regardless of the flag.
	SSEEnabled bool `json:"sseEnabled"`
	// Subscribers is the current number of live /refreshes connections.
	Subscribers int `json:"subscribers"`
	// Published counts PublishRefresh calls that fanned out (post-coalesce).
	Published uint64 `json:"published"`
	// Delivered counts individual (key -> subscriber) sends that succeeded.
	// This is THE signal for verifying live-refresh delivery: >0 for an armed
	// key under churn means the #61 zero-delivery defect is cured.
	Delivered uint64 `json:"delivered"`
	// Dropped counts (key -> subscriber) sends dropped because the consumer's
	// buffer was full (degrades to the frontend 5s throttle, never stale).
	Dropped uint64 `json:"dropped"`
	// Coalesced counts emits suppressed by the per-key coalesce window.
	Coalesced uint64 `json:"coalesced"`
}

// DebugRefreshes is the read-only aggregate refresh-broadcaster diagnostic
// (docs/rca-refreshes-zero-delivery-2026-06-26.md §5). It is the on-cluster
// instrument the RCA flagged as the #1 missing diagnostic: previously there
// was no way to observe whether PublishRefresh was firing / delivering without
// a kubectl exec + log grep.
//
// READ-ONLY: it calls cache.RefreshBroadcasterCounters() (atomic loads) +
// cache.RefreshSubscriberCount() (RLock, no mutation) + cache.RefreshSSEEnabled()
// — zero effect on serving or delivery behaviour.
//
// NOT gated behind the cache flag: when the SSE layer is disabled the counters
// are all zero and subscribers=0, but the endpoint still returns 200 with
// sseEnabled=false, so it is available for debugging in both modes.
//
// @Summary Refresh-broadcaster diagnostic
// @Description Read-only AGGREGATE refresh-broadcaster counters (published/delivered/dropped/coalesced + subscriber count). No per-key/identity enumeration.
// @ID debug-refreshes
// @Produce  json
// @Success 200 {object} debugRefreshesBody
// @Router /debug/refreshes [get]
func DebugRefreshes() http.HandlerFunc {
	return func(wri http.ResponseWriter, _ *http.Request) {
		published, delivered, dropped, coalesced := cache.RefreshBroadcasterCounters()
		body := debugRefreshesBody{
			SSEEnabled:  cache.RefreshSSEEnabled(),
			Subscribers: cache.RefreshSubscriberCount(),
			Published:   published,
			Delivered:   delivered,
			Dropped:     dropped,
			Coalesced:   coalesced,
		}
		wri.Header().Set("Content-Type", "application/json")
		wri.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(wri).Encode(body)
	}
}
