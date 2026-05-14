// deps_extract_test.go — Tag 0.30.8 tests for the widget dep-edge
// extractor. Both positive (render ref recorded) and negative (action
// ref filtered) cases per Revision 14 BINDING.

package dispatchers

import (
	"log/slog"
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func widgetForTest() *unstructured.Unstructured {
	w := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "widgets.templates.krateo.io/v1beta1",
		"kind":       "Panel",
		"metadata": map[string]any{
			"name":      "panel-1",
			"namespace": "demo-ns",
		},
		"spec": map[string]any{
			"apiRef": map[string]any{
				"name":      "compositions-list",
				"namespace": "krateo-system",
			},
			"resourcesRefs": map[string]any{
				"items": []any{
					// Render ref — should be recorded.
					map[string]any{
						"id":         "render-1",
						"apiVersion": "composition.krateo.io/v1",
						"resource":   "compositions",
						"namespace":  "bench-ns-01",
						"name":       "bench-app-01-01",
					},
					// Action-only ref — should be filtered out.
					map[string]any{
						"id":         "action-1",
						"apiVersion": "v1",
						"resource":   "pods",
						"namespace":  "bench-ns-01",
						"name":       "viewlogs-pod",
					},
				},
			},
		},
		"status": map[string]any{
			"widgetData": map[string]any{
				"actions": map[string]any{
					"navigate": []any{
						map[string]any{"resourceRefId": "action-1"},
					},
				},
			},
		},
	}}
	return w
}

func TestRecordWidgetDeps_RenderRefsRecorded_ActionRefsFiltered(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	cache.ResetDepsForTest()

	w := widgetForTest()
	l1Key := "L1_panel_demo"
	widgetGVR := schema.GroupVersionResource{
		Group: "widgets.templates.krateo.io", Version: "v1beta1", Resource: "panels",
	}

	recordWidgetDeps(slog.Default(), l1Key, widgetGVR, w)

	deps := cache.Deps()
	// Expected edges:
	//  1. self-dep:    (widgetGVR, "demo-ns", "panel-1") -> L1
	//  2. apiRef:      (restactions, "krateo-system", "compositions-list") -> L1
	//  3. render ref:  (compositions, "bench-ns-01", "bench-app-01-01") -> L1
	// Action ref ("v1/pods/bench-ns-01/viewlogs-pod") must NOT appear.
	wantPos := []struct {
		gvr  schema.GroupVersionResource
		ns   string
		name string
	}{
		{widgetGVR, "demo-ns", "panel-1"},
		{schema.GroupVersionResource{Group: "templates.krateo.io", Version: "v1", Resource: "restactions"}, "krateo-system", "compositions-list"},
		{schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1", Resource: "compositions"}, "bench-ns-01", "bench-app-01-01"},
	}
	for _, c := range wantPos {
		matched := deps.CollectMatchesForTest(c.gvr, c.ns, c.name)
		if _, ok := matched[l1Key]; !ok {
			t.Errorf("expected positive dep %v/%s/%s but L1 key not found; got matched=%v",
				c.gvr, c.ns, c.name, matched)
		}
	}

	// Action ref must NOT be recorded.
	actionGVR := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	negMatched := deps.CollectMatchesForTest(actionGVR, "bench-ns-01", "viewlogs-pod")
	if _, ok := negMatched[l1Key]; ok {
		t.Fatalf("action-only ref WAS recorded as a render dep — Revision 14 filter broken")
	}
}

func TestParseGVR(t *testing.T) {
	cases := []struct {
		apiVer, resource string
		wantGroup, wantVer string
		wantOK bool
	}{
		{"v1", "pods", "", "v1", true},
		{"apps/v1", "deployments", "apps", "v1", true},
		{"templates.krateo.io/v1", "restactions", "templates.krateo.io", "v1", true},
		{"", "pods", "", "", false},
		{"v1", "", "", "", false},
	}
	for _, c := range cases {
		gvr, ok := parseGVR(c.apiVer, c.resource)
		if ok != c.wantOK {
			t.Errorf("parseGVR(%q,%q) ok=%v want %v", c.apiVer, c.resource, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if gvr.Group != c.wantGroup || gvr.Version != c.wantVer {
			t.Errorf("parseGVR(%q,%q) = %v, want group=%q version=%q",
				c.apiVer, c.resource, gvr, c.wantGroup, c.wantVer)
		}
	}
}

func TestExtractActionRefIDs_BothShapes(t *testing.T) {
	// Slice shape.
	w1 := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"widgetData": map[string]any{"actions": map[string]any{
			"navigate": []any{
				map[string]any{"resourceRefId": "a1"},
				map[string]any{"resourceRefId": "a2"},
			},
		}}},
	}}
	got := extractActionRefIDs(w1)
	if !got["a1"] || !got["a2"] {
		t.Errorf("slice-shape action ids missing: %v", got)
	}

	// Map shape.
	w2 := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"widgetData": map[string]any{"actions": map[string]any{
			"openDrawer": map[string]any{"resourceRefId": "d1"},
		}}},
	}}
	got2 := extractActionRefIDs(w2)
	if !got2["d1"] {
		t.Errorf("map-shape action id missing: %v", got2)
	}

	// Empty status.
	w3 := &unstructured.Unstructured{Object: map[string]any{}}
	if got3 := extractActionRefIDs(w3); len(got3) != 0 {
		t.Errorf("empty status should yield empty set, got %v", got3)
	}
}
