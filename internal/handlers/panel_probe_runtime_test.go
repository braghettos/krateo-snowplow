package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// Q-PANEL-PROBE (0.25.326). /metrics/runtime exposes the panel-probe
// counters under widgets.panel_probe.{managed_empty,managed_populated,
// path_malformed}. The canary scraper greps these JSON keys; renaming
// MUST fail this test, not silently break the dashboard.

func clearPanelProbeMetrics() {
	cache.GlobalMetrics.PanelProbeManagedEmpty.Store(0)
	cache.GlobalMetrics.PanelProbeManagedPopulated.Store(0)
	cache.GlobalMetrics.PanelProbePathMalformed.Store(0)
}

func TestRuntimeMetrics_PanelProbeShape(t *testing.T) {
	clearPanelProbeMetrics()
	t.Cleanup(clearPanelProbeMetrics)

	cache.GlobalMetrics.PanelProbeManagedEmpty.Store(7)
	cache.GlobalMetrics.PanelProbeManagedPopulated.Store(3)
	cache.GlobalMetrics.PanelProbePathMalformed.Store(1)

	req := httptest.NewRequest(http.MethodGet, "/metrics/runtime", nil)
	rec := httptest.NewRecorder()
	RuntimeMetricsHandler(nil, nil, nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}

	var typed RuntimeMetrics
	if err := json.Unmarshal(rec.Body.Bytes(), &typed); err != nil {
		t.Fatalf("decode typed: %v", err)
	}
	if got := typed.Widgets.PanelProbe.ManagedEmpty; got != 7 {
		t.Errorf("PanelProbe.ManagedEmpty: got %d, want 7", got)
	}
	if got := typed.Widgets.PanelProbe.ManagedPopulated; got != 3 {
		t.Errorf("PanelProbe.ManagedPopulated: got %d, want 3", got)
	}
	if got := typed.Widgets.PanelProbe.PathMalformed; got != 1 {
		t.Errorf("PanelProbe.PathMalformed: got %d, want 1", got)
	}

	// Pin the JSON key contract at widgets.panel_probe.{...}. Canary
	// observers grep these literal keys; a rename here must fail loud.
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	w, _ := raw["widgets"].(map[string]any)
	pp, ok := w["panel_probe"].(map[string]any)
	if !ok {
		t.Fatalf("widgets.panel_probe missing or wrong shape: %T", w["panel_probe"])
	}
	for _, k := range []string{"managed_empty", "managed_populated", "path_malformed"} {
		if _, present := pp[k]; !present {
			t.Errorf("widgets.panel_probe.%s key missing (raw=%v)", k, pp)
		}
	}
}

func TestRuntimeMetrics_PanelProbeZeroBaseline(t *testing.T) {
	clearPanelProbeMetrics()
	t.Cleanup(clearPanelProbeMetrics)

	req := httptest.NewRequest(http.MethodGet, "/metrics/runtime", nil)
	rec := httptest.NewRecorder()
	RuntimeMetricsHandler(nil, nil, nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	var raw map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	w, _ := raw["widgets"].(map[string]any)
	pp, ok := w["panel_probe"].(map[string]any)
	if !ok {
		t.Fatalf("widgets.panel_probe always-render contract: missing block (got %v)", w)
	}
	for _, k := range []string{"managed_empty", "managed_populated", "path_malformed"} {
		got, present := pp[k]
		if !present {
			t.Errorf("widgets.panel_probe.%s missing at zero baseline", k)
			continue
		}
		if got.(float64) != 0 {
			t.Errorf("widgets.panel_probe.%s at zero baseline: got %v, want 0", k, got)
		}
	}
}
