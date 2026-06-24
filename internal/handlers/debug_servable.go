package handlers

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// debugServableBody is the JSON body returned by /debug/servable.
type debugServableBody struct {
	// CacheEnabled mirrors the cache subsystem state. When false there is
	// no informer registry to report (GVRs is empty) — the endpoint still
	// returns 200 so it is usable as a diagnostic regardless of the flag.
	CacheEnabled bool `json:"cacheEnabled"`
	// Count is len(GVRs) — convenience for a quick "how many informers".
	Count int `json:"count"`
	// GVRs is the per-GVR servability snapshot (sorted by GVR string for
	// stable output across requests).
	GVRs []cache.ServableGVRStatus `json:"gvrs"`
}

// DebugServable is the read-only per-GVR servability diagnostic
// (docs/rca-stale-delete-compositiondefinitions-informer-2026-06-25.md).
//
// It dumps, per registered informer, the four servability conjuncts
// {HasSynced, watchBroken, confirmed, servable} so an operator can see WHY
// a GVR is (not) servable — the exact signal needed to diagnose the
// stale-delete latch (a registered-but-unconfirmed / watch-broken GVR whose
// data informer is not delivering DELETEs) without a kubectl exec into the
// pod.
//
// READ-ONLY (PM AC-7): it calls cache.Global().ServableSnapshot(), which
// takes rw.mu in READ mode and mutates NO state — no confirm, no register,
// no watch-flag change. Calling it has zero effect on serving behaviour.
//
// NOT gated behind the cache flag: when CACHE_ENABLED=false the snapshot is
// empty (passthrough has no informers) but the endpoint still returns 200
// with cacheEnabled=false, so it is available for debugging in both modes.
//
// @Summary Servability diagnostic
// @Description Read-only per-GVR servability snapshot (HasSynced/watchBroken/confirmed/servable).
// @ID debug-servable
// @Produce  json
// @Success 200 {object} debugServableBody
// @Router /debug/servable [get]
func DebugServable() http.HandlerFunc {
	return func(wri http.ResponseWriter, _ *http.Request) {
		rw := cache.Global()
		rows := rw.ServableSnapshot() // nil-receiver safe
		sort.Slice(rows, func(i, j int) bool { return rows[i].GVR < rows[j].GVR })

		body := debugServableBody{
			CacheEnabled: !cache.Disabled(),
			Count:        len(rows),
			GVRs:         rows,
		}
		wri.Header().Set("Content-Type", "application/json")
		wri.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(wri).Encode(body)
	}
}
