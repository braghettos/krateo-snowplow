package handlers

import (
	"net/http"
	"net/http/httptest"
	"net/http/pprof"
	"testing"
)

// TestPprofHeap_Smoke confirms /debug/pprof/heap returns 200 + non-empty
// body — the smoke described in the architect's Q-DIAG-PPROF (0.25.321)
// spec. main.go wraps the same pprof.Handler("heap") with the
// X-Snowplow-Build header; this test pins the underlying handler
// behavior so a stdlib swap (e.g. net/http/pprof rename) is caught
// before deploy.
func TestPprofHeap_Smoke(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap?debug=1", nil)
	rec := httptest.NewRecorder()
	pprof.Handler("heap").ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("expected non-empty body, got 0 bytes")
	}
}
