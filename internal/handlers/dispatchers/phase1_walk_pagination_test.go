// phase1_walk_pagination_test.go — Path 3.2.2 unit tests for the
// walker apiRef pagination predicates and bounds.
//
// SCOPE: This test file exercises the SHAPE PREDICATES and the
// SAFETY BOUND constant — the parts of phase1_walk_pagination.go that
// are testable without a live apiserver / mocked widget resolver.
// End-to-end pagination (resolver→populate→recurse) is exercised by
// the integration falsifier (Phase 3 bench probe on the live cluster).

package dispatchers

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestIsApiRefTemplateDriven_PositiveShape: widget with non-empty
// spec.apiRef.name AND non-empty spec.resourcesRefsTemplate triggers
// pagination eligibility. Mirrors the compositions-page-datagrid shape.
func TestIsApiRefTemplateDriven_PositiveShape(t *testing.T) {
	w := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "widgets.templates.krateo.io/v1beta1",
		"kind":       "Datagrid",
		"metadata":   map[string]any{"namespace": "krateo-system", "name": "compositions-page-datagrid"},
		"spec": map[string]any{
			"apiRef": map[string]any{
				"name":      "compositions-list",
				"namespace": "krateo-system",
			},
			"resourcesRefsTemplate": []any{
				map[string]any{
					"id":   "${.name}",
					"path": "/call?resource=panels&apiVersion=widgets.templates.krateo.io/v1beta1&namespace=${.namespace}&name=${.name}-composition-panel",
					"verb": "GET",
				},
			},
		},
	}}
	if !isApiRefTemplateDriven(w.Object) {
		t.Fatalf("isApiRefTemplateDriven should fire on apiRef+template widget; obj=%v", w.Object)
	}
}

// TestIsApiRefTemplateDriven_NegativeNoApiRef: widget WITHOUT apiRef
// MUST NOT trigger pagination (no external data source to page over).
func TestIsApiRefTemplateDriven_NegativeNoApiRef(t *testing.T) {
	w := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "widgets.templates.krateo.io/v1beta1",
		"kind":       "Markdown",
		"metadata":   map[string]any{"namespace": "ns", "name": "static-md"},
		"spec": map[string]any{
			// Static markdown — no apiRef
			"content": "hello",
		},
	}}
	if isApiRefTemplateDriven(w.Object) {
		t.Fatalf("isApiRefTemplateDriven must be false when apiRef absent; obj=%v", w.Object)
	}
}

// TestIsApiRefTemplateDriven_NegativeNoTemplate: widget with apiRef but
// NO resourcesRefsTemplate — the items list comes from a static
// spec.resourcesRefs (not the apiRef). Paginating apiRef pages would
// not produce new items; pagination is correctly disabled.
func TestIsApiRefTemplateDriven_NegativeNoTemplate(t *testing.T) {
	w := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "widgets.templates.krateo.io/v1beta1",
		"kind":       "Piechart",
		"metadata":   map[string]any{"namespace": "ns", "name": "agg-pie"},
		"spec": map[string]any{
			"apiRef": map[string]any{
				"name":      "compositions-list",
				"namespace": "krateo-system",
			},
			// No resourcesRefsTemplate — piechart renders entirely
			// from status.widgetData. Pagination would not help.
		},
	}}
	if isApiRefTemplateDriven(w.Object) {
		t.Fatalf("isApiRefTemplateDriven must be false when resourcesRefsTemplate absent; obj=%v", w.Object)
	}
}

// TestIsApiRefTemplateDriven_NegativeEmptyApiRefName: a spec.apiRef
// block whose `.name` is empty is treated as absent (same conservative
// direction widget_content.go uses).
func TestIsApiRefTemplateDriven_NegativeEmptyApiRefName(t *testing.T) {
	w := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"apiRef":                map[string]any{"name": "", "namespace": "ns"},
			"resourcesRefsTemplate": []any{map[string]any{"id": "x"}},
		},
	}}
	if isApiRefTemplateDriven(w.Object) {
		t.Fatalf("isApiRefTemplateDriven must be false for empty apiRef.name")
	}
}

// TestIsApiRefTemplateDriven_NilObject: defensive — nil map returns
// false (the walker may hand a nil-Object widget when ParseCallPath fails).
func TestIsApiRefTemplateDriven_NilObject(t *testing.T) {
	if isApiRefTemplateDriven(nil) {
		t.Fatalf("isApiRefTemplateDriven must be false for nil object")
	}
}

// TestResolverWantsContinue_True: resolved envelope with
// status.resourcesRefs.slice.continue == true signals more pages
// available — the resolver's promise that another page would yield
// new items.
func TestResolverWantsContinue_True(t *testing.T) {
	res := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{
			"resourcesRefs": map[string]any{
				"items": []any{map[string]any{"id": "a"}},
				"slice": map[string]any{
					"perPage":  5,
					"page":     2,
					"continue": true,
				},
			},
		},
	}}
	if !resolverWantsContinue(res) {
		t.Fatalf("resolverWantsContinue should return true on .slice.continue==true")
	}
}

// TestResolverWantsContinue_False: slice.continue==false signals end
// of pagination.
func TestResolverWantsContinue_False(t *testing.T) {
	res := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{
			"resourcesRefs": map[string]any{
				"items": []any{},
				"slice": map[string]any{
					"perPage":  5,
					"page":     3,
					"continue": false,
				},
			},
		},
	}}
	if resolverWantsContinue(res) {
		t.Fatalf("resolverWantsContinue should return false on .slice.continue==false")
	}
}

// TestResolverWantsContinue_NoSlice: resolved envelope without a
// status.resourcesRefs.slice block (the resolver did NOT mark
// continuation — e.g. perPage was unbounded). Conservative direction:
// no continuation.
func TestResolverWantsContinue_NoSlice(t *testing.T) {
	res := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{
			"resourcesRefs": map[string]any{
				"items": []any{map[string]any{"id": "a"}},
			},
		},
	}}
	if resolverWantsContinue(res) {
		t.Fatalf("resolverWantsContinue must be false when .slice absent")
	}
}

// TestResolverWantsContinue_NilRes: defensive nil-input case.
func TestResolverWantsContinue_NilRes(t *testing.T) {
	if resolverWantsContinue(nil) {
		t.Fatalf("resolverWantsContinue must be false for nil envelope")
	}
}

// TestMaxApiRefPages_DefaultUsedAbsentOverride: the production default
// constant must be returned when no test override is set. Pins the
// safety cap so a code edit to lower it surfaces immediately.
func TestMaxApiRefPages_DefaultUsedAbsentOverride(t *testing.T) {
	old := phase1MaxApiRefPagesForTest
	phase1MaxApiRefPagesForTest = 0
	t.Cleanup(func() { phase1MaxApiRefPagesForTest = old })

	if got := maxApiRefPages(); got != phase1MaxApiRefPages {
		t.Fatalf("maxApiRefPages() = %d, want default %d", got, phase1MaxApiRefPages)
	}
}

// TestMaxApiRefPages_TestOverrideHonoured: a non-zero test override
// supersedes the default constant. Used by future end-to-end tests to
// bound the apiRef iteration to a small number.
func TestMaxApiRefPages_TestOverrideHonoured(t *testing.T) {
	old := phase1MaxApiRefPagesForTest
	phase1MaxApiRefPagesForTest = 3
	t.Cleanup(func() { phase1MaxApiRefPagesForTest = old })

	if got := maxApiRefPages(); got != 3 {
		t.Fatalf("maxApiRefPages() with override=3 = %d, want 3", got)
	}
}

// TestPhase1MaxApiRefPages_BoundedSane: the production cap must be a
// reasonable bound — not 0 (would disable pagination), not absurdly
// large (would never cap a runaway apiRef). Documents the cap as a
// load-bearing safety constant.
func TestPhase1MaxApiRefPages_BoundedSane(t *testing.T) {
	if phase1MaxApiRefPages <= 0 {
		t.Fatalf("phase1MaxApiRefPages must be > 0; got %d", phase1MaxApiRefPages)
	}
	if phase1MaxApiRefPages > 100_000 {
		t.Fatalf("phase1MaxApiRefPages absurdly large (%d) — unbounded apiRef would loop forever",
			phase1MaxApiRefPages)
	}
}
