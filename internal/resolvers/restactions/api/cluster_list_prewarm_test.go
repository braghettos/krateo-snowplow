// cluster_list_prewarm_test.go — Path 3.2.1 / 0.30.219 unit tests for
// EnumerateClusterListCells and extractClusterListGVRFromStage. The
// Path-3.2 algorithm produced cells=0 in production because the
// dominant RA-stage shape is a literal cluster-scope LIST (no
// DependsOn.Iterator). These tests pin the new static-extraction path
// against the empirical RA inventory captured at 2026-05-31 from
// kubectl get restactions.templates.krateo.io -n krateo-system.

package api

import (
	"context"
	"log/slog"
	"testing"

	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
)

// raWithStages is a test helper that builds a *templates.RESTAction from
// a list of stages. The Name field is unused by EnumerateClusterListCells
// (dedup is by GVR not RA name) but populated for log readability.
func raWithStages(name string, stages ...*templates.API) *templates.RESTAction {
	ra := &templates.RESTAction{}
	ra.Name = name
	ra.Spec.API = stages
	return ra
}

// TestExtractClusterListGVRFromStage_ClusterScopeLiteralPath covers the
// dominant production case: a literal cluster-scope path with no
// iterator. (Path-3.2 algorithm SKIPPED these — the production bug.)
// Mirrors blueprints-list/allCompositionDefinitions in the 0.30.218
// inventory.
func TestExtractClusterListGVRFromStage_ClusterScopeLiteralPath(t *testing.T) {
	stage := &templates.API{
		Name: "allCompositionDefinitions",
		Path: "/apis/core.krateo.io/v1alpha1/compositiondefinitions",
		Verb: ptr.To("GET"),
	}
	gvr, ok := extractClusterListGVRFromStage(stage)
	if !ok {
		t.Fatalf("extractClusterListGVRFromStage: expected ok=true for literal cluster-scope LIST path")
	}
	if gvr.Group != "core.krateo.io" || gvr.Version != "v1alpha1" || gvr.Resource != "compositiondefinitions" {
		t.Fatalf("extractClusterListGVRFromStage: got gvr=%q want core.krateo.io/v1alpha1/compositiondefinitions", gvr.String())
	}
}

// TestExtractClusterListGVRFromStage_WidgetClusterScope covers the
// widget GVRs the SPA actually fetches at /dashboard + /compositions.
// Mirrors all-routes / blueprints-panels / compositions-panels /
// sidebar-nav-menu-items in the 0.30.218 inventory — these were ALL
// skipped by the empty-dict jq probe.
func TestExtractClusterListGVRFromStage_WidgetClusterScope(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		group  string
		ver    string
		plural string
	}{
		{"routes", "/apis/widgets.templates.krateo.io/v1beta1/routes", "widgets.templates.krateo.io", "v1beta1", "routes"},
		{"panels", "/apis/widgets.templates.krateo.io/v1beta1/panels", "widgets.templates.krateo.io", "v1beta1", "panels"},
		{"navmenuitems", "/apis/widgets.templates.krateo.io/v1beta1/navmenuitems", "widgets.templates.krateo.io", "v1beta1", "navmenuitems"},
		{"crds", "/apis/apiextensions.k8s.io/v1/customresourcedefinitions", "apiextensions.k8s.io", "v1", "customresourcedefinitions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stage := &templates.API{Name: tc.name, Path: tc.path}
			gvr, ok := extractClusterListGVRFromStage(stage)
			if !ok {
				t.Fatalf("extractClusterListGVRFromStage: expected ok=true for %q", tc.path)
			}
			if gvr.Group != tc.group || gvr.Version != tc.ver || gvr.Resource != tc.plural {
				t.Fatalf("extractClusterListGVRFromStage: got %q want %s/%s/%s", gvr.String(), tc.group, tc.ver, tc.plural)
			}
		})
	}
}

// TestExtractClusterListGVRFromStage_NamespaceScopeLiteralPath covers
// the edge case where the path is namespace-scoped but the namespace
// is a literal (not a jq template). The cluster-list cell still
// registers the GVR (with the cluster-scope key) because
// populateClusterListCellSync issues a cluster-scope LIST.
func TestExtractClusterListGVRFromStage_NamespaceScopeLiteralPath(t *testing.T) {
	stage := &templates.API{
		Name: "krateo-system-panels",
		Path: "/apis/widgets.templates.krateo.io/v1beta1/namespaces/krateo-system/panels",
	}
	gvr, ok := extractClusterListGVRFromStage(stage)
	if !ok {
		t.Fatalf("extractClusterListGVRFromStage: expected ok=true for literal namespace-scope LIST path")
	}
	if gvr.Resource != "panels" {
		t.Fatalf("extractClusterListGVRFromStage: got resource=%q want panels", gvr.Resource)
	}
}

// TestExtractClusterListGVRFromStage_ParentDerivedIterator covers the
// compositions-list/allCompositions case — path template has unresolved
// ${...} fragments referencing parent-stage output. The Path-3.2 jq
// probe FAILED here (correctly); the new algorithm also returns false
// (correctly). The cell stays cold-fallback-served at customer touch.
func TestExtractClusterListGVRFromStage_ParentDerivedIterator(t *testing.T) {
	stage := &templates.API{
		Name: "allCompositions",
		Path: `${ "/apis/composition.krateo.io/" + (.version) + "/" + (.plural) }`,
	}
	if _, ok := extractClusterListGVRFromStage(stage); ok {
		t.Fatalf("extractClusterListGVRFromStage: expected ok=false for parent-derived ${...} template — cannot derive GVR at boot")
	}
}

// TestExtractClusterListGVRFromStage_InterRACall covers the
// compositions-list/allNamespacesAndCrds case — path is a snowplow
// /call?... inter-RA route, NOT an apiserver path. Must be skipped.
func TestExtractClusterListGVRFromStage_InterRACall(t *testing.T) {
	stage := &templates.API{
		Name: "allNamespacesAndCrds",
		Path: "/call?apiVersion=templates.krateo.io%2Fv1&resource=restactions&name=compositions-get-ns-and-crd&namespace=krateo-system",
	}
	if _, ok := extractClusterListGVRFromStage(stage); ok {
		t.Fatalf("extractClusterListGVRFromStage: expected ok=false for inter-RA /call?... path")
	}
}

// TestExtractClusterListGVRFromStage_GETByName rejects per-object GET
// paths — cluster-list cell models a LIST envelope, not a single-object
// GET. A name suffix must disqualify the stage.
func TestExtractClusterListGVRFromStage_GETByName(t *testing.T) {
	stage := &templates.API{
		Name: "namedPanel",
		Path: "/apis/widgets.templates.krateo.io/v1beta1/namespaces/krateo-system/panels/blueprints-page-datagrid",
	}
	if _, ok := extractClusterListGVRFromStage(stage); ok {
		t.Fatalf("extractClusterListGVRFromStage: expected ok=false for GET-by-name path")
	}
}

// TestExtractClusterListGVRFromStage_NonGETVerb rejects write verbs.
// The cluster-list cell models a read envelope; a PUT/POST/DELETE
// stage must never seed it.
func TestExtractClusterListGVRFromStage_NonGETVerb(t *testing.T) {
	cases := []string{"PUT", "POST", "DELETE", "PATCH"}
	for _, v := range cases {
		t.Run(v, func(t *testing.T) {
			stage := &templates.API{
				Name: "writer",
				Path: "/apis/widgets.templates.krateo.io/v1beta1/panels",
				Verb: ptr.To(v),
			}
			if _, ok := extractClusterListGVRFromStage(stage); ok {
				t.Fatalf("extractClusterListGVRFromStage: expected ok=false for verb=%s", v)
			}
		})
	}
}

// TestExtractClusterListGVRFromStage_EmptyOrNil covers degenerate
// inputs: nil stage, empty path, whitespace path, all return false
// without panic.
func TestExtractClusterListGVRFromStage_EmptyOrNil(t *testing.T) {
	if _, ok := extractClusterListGVRFromStage(nil); ok {
		t.Fatalf("extractClusterListGVRFromStage: expected ok=false for nil stage")
	}
	if _, ok := extractClusterListGVRFromStage(&templates.API{Path: ""}); ok {
		t.Fatalf("extractClusterListGVRFromStage: expected ok=false for empty path")
	}
	if _, ok := extractClusterListGVRFromStage(&templates.API{Path: "   "}); ok {
		t.Fatalf("extractClusterListGVRFromStage: expected ok=false for whitespace path")
	}
}

// TestEnumerateClusterListCells_FullProductionRoster pins the 0.30.218
// production RA inventory verbatim (captured 2026-05-31 from
// `kubectl get restactions.templates.krateo.io -n krateo-system`) and
// asserts the new algorithm produces the expected cells. This is the
// north-star regression guard: if a future change to the algorithm
// silently shrinks the roster, this test fails.
func TestEnumerateClusterListCells_FullProductionRoster(t *testing.T) {
	ras := []*templates.RESTAction{
		raWithStages("all-routes", &templates.API{
			Name: "routes",
			Path: "/apis/widgets.templates.krateo.io/v1beta1/routes",
		}),
		raWithStages("blueprints-list", &templates.API{
			Name: "allCompositionDefinitions",
			Path: "/apis/core.krateo.io/v1alpha1/compositiondefinitions",
		}),
		raWithStages("blueprints-panels", &templates.API{
			Name: "blueprintspanels",
			Path: "/apis/widgets.templates.krateo.io/v1beta1/panels",
		}),
		raWithStages("compositions-get-ns-and-crd", &templates.API{
			Name: "crds",
			Path: "/apis/apiextensions.k8s.io/v1/customresourcedefinitions",
		}),
		raWithStages("compositions-list",
			&templates.API{
				Name: "allNamespacesAndCrds",
				Path: "/call?apiVersion=templates.krateo.io%2Fv1&resource=restactions&name=compositions-get-ns-and-crd&namespace=krateo-system",
			},
			&templates.API{
				Name: "allCompositions",
				Path: `${ "/apis/composition.krateo.io/" + (.version) + "/" + (.plural) }`,
				DependsOn: &templates.Dependency{
					Iterator: ptr.To(".allNamespacesAndCrds.status"),
				},
			},
		),
		raWithStages("compositions-panels", &templates.API{
			Name: "compositionspanels",
			Path: "/apis/widgets.templates.krateo.io/v1beta1/panels",
		}),
		raWithStages("sidebar-nav-menu-items", &templates.API{
			Name: "navmenuitems",
			Path: "/apis/widgets.templates.krateo.io/v1beta1/navmenuitems",
		}),
	}

	roster := EnumerateClusterListCells(context.Background(), slog.Default(), ras)

	// Expected GVRs (after dedup — panels appears in two RAs):
	//   1. widgets.templates.krateo.io/v1beta1/routes
	//   2. core.krateo.io/v1alpha1/compositiondefinitions
	//   3. widgets.templates.krateo.io/v1beta1/panels         (deduped)
	//   4. apiextensions.k8s.io/v1/customresourcedefinitions
	//   5. widgets.templates.krateo.io/v1beta1/navmenuitems
	//
	// allCompositions stays cold-fallback (parent-derived ${...}).
	// allNamespacesAndCrds is an inter-RA /call — not apiserver.
	expectedGVRs := map[string]bool{
		"widgets.templates.krateo.io/v1beta1, Resource=routes":           false,
		"core.krateo.io/v1alpha1, Resource=compositiondefinitions":       false,
		"widgets.templates.krateo.io/v1beta1, Resource=panels":           false,
		"apiextensions.k8s.io/v1, Resource=customresourcedefinitions":    false,
		"widgets.templates.krateo.io/v1beta1, Resource=navmenuitems":     false,
	}
	for _, cell := range roster.Cells {
		key := cell.GVR.String()
		if _, ok := expectedGVRs[key]; !ok {
			t.Errorf("EnumerateClusterListCells: unexpected GVR in roster: %q", key)
		}
		expectedGVRs[key] = true
	}
	for k, seen := range expectedGVRs {
		if !seen {
			t.Errorf("EnumerateClusterListCells: missing expected GVR: %q", k)
		}
	}
	// Hard cardinality assertion — guards against silent roster shrink.
	if len(roster.Cells) != len(expectedGVRs) {
		t.Fatalf("EnumerateClusterListCells: got %d cells, want %d (expected GVRs: %v)",
			len(roster.Cells), len(expectedGVRs), expectedGVRs)
	}
}

// TestEnumerateClusterListCells_NilAndEmpty covers degenerate inputs —
// nil slice, empty slice, nil RA entries, RAs with nil API stages.
func TestEnumerateClusterListCells_NilAndEmpty(t *testing.T) {
	ctx := context.Background()
	log := slog.Default()
	if r := EnumerateClusterListCells(ctx, log, nil); len(r.Cells) != 0 {
		t.Fatalf("expected empty roster from nil RA slice, got %d", len(r.Cells))
	}
	if r := EnumerateClusterListCells(ctx, log, []*templates.RESTAction{}); len(r.Cells) != 0 {
		t.Fatalf("expected empty roster from empty RA slice, got %d", len(r.Cells))
	}
	if r := EnumerateClusterListCells(ctx, log, []*templates.RESTAction{nil}); len(r.Cells) != 0 {
		t.Fatalf("expected empty roster from nil RA entry, got %d", len(r.Cells))
	}
	ra := &templates.RESTAction{}
	ra.Spec.API = []*templates.API{nil}
	if r := EnumerateClusterListCells(ctx, log, []*templates.RESTAction{ra}); len(r.Cells) != 0 {
		t.Fatalf("expected empty roster from nil API stage, got %d", len(r.Cells))
	}
}

// TestEnumerateClusterListCells_DedupAcrossRAs covers the canonical
// dedup case — the same GVR appears in multiple RAs (blueprints-panels
// and compositions-panels both target panels). The roster MUST contain
// exactly one cell for that GVR.
func TestEnumerateClusterListCells_DedupAcrossRAs(t *testing.T) {
	ras := []*templates.RESTAction{
		raWithStages("blueprints-panels", &templates.API{
			Name: "blueprintspanels",
			Path: "/apis/widgets.templates.krateo.io/v1beta1/panels",
		}),
		raWithStages("compositions-panels", &templates.API{
			Name: "compositionspanels",
			Path: "/apis/widgets.templates.krateo.io/v1beta1/panels",
		}),
	}
	r := EnumerateClusterListCells(context.Background(), slog.Default(), ras)
	if len(r.Cells) != 1 {
		t.Fatalf("expected 1 deduped cell, got %d", len(r.Cells))
	}
	if r.Cells[0].GVR.Resource != "panels" {
		t.Fatalf("expected dedupe winner GVR.Resource=panels, got %q", r.Cells[0].GVR.Resource)
	}
}
