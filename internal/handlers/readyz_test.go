// readyz_test.go — 0.30.102 Tag B premature-Ready falsifier for the
// /readyz probe handler. /readyz returning 200 before cache.Phase1Done
// is a FAIL — the readinessProbe would admit traffic to a pod whose
// navigated informers are still cold.

package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// TestReadyz_503BeforePhase1Done is the premature-Ready falsifier: while
// Phase1Done is false, /readyz MUST return 503. A 200 here is the exact
// regression the falsifier guards against.
func TestReadyz_503BeforePhase1Done(t *testing.T) {
	cache.ResetPhase1DoneForTest()
	t.Cleanup(cache.ResetPhase1DoneForTest)

	rec := httptest.NewRecorder()
	ReadyCheck()(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("premature-Ready FAIL: /readyz returned %d before Phase1Done; want 503", rec.Code)
	}
	var body readyzInfo
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Phase1Done {
		t.Fatalf("/readyz body must report phase1Done=false while warming")
	}
	if body.Status != "warming" {
		t.Fatalf("status = %q, want \"warming\"", body.Status)
	}
}

// TestReadyz_200AfterPhase1Done asserts /readyz flips to 200 once the
// Phase 1 warmup signals done. This is also the flag-OFF behavior: when
// PREWARM_ENABLED is OFF, main.go calls MarkPhase1Done immediately so
// /readyz is an immediate-200 no-op from the first probe.
func TestReadyz_200AfterPhase1Done(t *testing.T) {
	cache.ResetPhase1DoneForTest()
	t.Cleanup(cache.ResetPhase1DoneForTest)

	cache.MarkPhase1Done()

	rec := httptest.NewRecorder()
	ReadyCheck()(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("/readyz returned %d after Phase1Done; want 200", rec.Code)
	}
	var body readyzInfo
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !body.Phase1Done {
		t.Fatalf("/readyz body must report phase1Done=true once ready")
	}
	if body.Status != "ready" {
		t.Fatalf("status = %q, want \"ready\"", body.Status)
	}
}

// TestReadyz_TransitionStaysGated walks the full premature-Ready
// transition: 503 -> (MarkPhase1Done) -> 200. The handler must NEVER
// report 200 in the pre-done window.
func TestReadyz_TransitionStaysGated(t *testing.T) {
	cache.ResetPhase1DoneForTest()
	t.Cleanup(cache.ResetPhase1DoneForTest)

	// Probe repeatedly while still warming — every probe must be 503.
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		ReadyCheck()(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("probe %d returned %d while warming; premature-Ready FAIL", i, rec.Code)
		}
	}

	cache.MarkPhase1Done()

	rec := httptest.NewRecorder()
	ReadyCheck()(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/readyz returned %d after done; want 200", rec.Code)
	}
}
