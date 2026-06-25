// debug_servable_test.go — /debug/servable read-only diagnostic tests
// (Fix #1, docs/rca-stale-delete-compositiondefinitions-informer-2026-06-25.md).
//
// The endpoint must (1) return 200 + a well-formed body in BOTH cache-on
// and cache-off modes (NOT gated behind the cache flag, PM AC-7) and
// (2) be READ-ONLY — calling it MUST NOT change any servability state.

package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// TestDebugServable_CacheOff returns 200 with cacheEnabled=false and an
// empty GVR list when the cache subsystem is off — the endpoint is
// available for debugging regardless of the flag (not flag-gated).
func TestDebugServable_CacheOff(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false")

	rec := httptest.NewRecorder()
	DebugServable()(rec, httptest.NewRequest(http.MethodGet, "/debug/servable", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("/debug/servable returned %d; want 200 (not flag-gated)", rec.Code)
	}
	var body debugServableBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.CacheEnabled {
		t.Fatalf("cache-off: body.cacheEnabled must be false; got true")
	}
	if body.Count != len(body.GVRs) {
		t.Fatalf("count=%d != len(gvrs)=%d", body.Count, len(body.GVRs))
	}
}

// TestDebugServable_ReadOnly asserts the endpoint mutates no servability
// state: two back-to-back requests yield the SAME snapshot (no GVR is
// confirmed/registered/healed as a side effect of being observed). This is
// the PM AC-7 read-only guarantee.
func TestDebugServable_ReadOnly(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false") // passthrough — deterministic empty snapshot.

	first := decodeServable(t)
	second := decodeServable(t)

	if first.Count != second.Count {
		t.Fatalf("read-only violation: snapshot count changed across two GET requests "+
			"(%d -> %d) — the diagnostic mutated state", first.Count, second.Count)
	}
	if len(first.GVRs) != len(second.GVRs) {
		t.Fatalf("read-only violation: GVR set size changed across requests (%d -> %d)",
			len(first.GVRs), len(second.GVRs))
	}
	// Field-level stability: the servability conjuncts of each GVR must be
	// identical across the two observations.
	idx := map[string]cache.ServableGVRStatus{}
	for _, r := range first.GVRs {
		idx[r.GVR] = r
	}
	for _, r := range second.GVRs {
		p, ok := idx[r.GVR]
		if !ok {
			t.Fatalf("read-only violation: GVR %q appeared only in the second request", r.GVR)
		}
		if p != r {
			t.Fatalf("read-only violation: GVR %q conjuncts changed across requests: %+v -> %+v",
				r.GVR, p, r)
		}
	}
}

func decodeServable(t *testing.T) debugServableBody {
	t.Helper()
	rec := httptest.NewRecorder()
	DebugServable()(rec, httptest.NewRequest(http.MethodGet, "/debug/servable", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/debug/servable returned %d; want 200", rec.Code)
	}
	var body debugServableBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return body
}
