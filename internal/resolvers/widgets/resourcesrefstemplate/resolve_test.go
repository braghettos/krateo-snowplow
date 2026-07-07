// resolve_test.go — Task #322 (#318-R2) Commit 1 falsifier F-premise
// (from docs/task-318-step3-fan-prewindow-design-2026-06-11.md §7 + the
// PM gate C9). PINS the fan-already-windowed contract that killed
// Step 3(b)(2): for a compositions datagrid at page=3 / perPage=5,
// resourcesrefstemplate.Resolve's jqutil.ForEach over the iterator
// ${.compositionspanels} yields <= perPage (5) ResourceRefs, NOT
// ~4,423 — because ds.compositionspanels is ALREADY sliced to ~perPage
// at the apiRef chokepoint (apiref.raFullListServe -> cache.GoSliceFullList,
// ra_full_list_slice.go:122; widgets/resolve.go injectSlice).
//
// GREEN (<=5) = the fan is already windowed upstream, so Step 3(b)(2) is
// moot and this cost-trim ship correctly does NOT touch the fan. RED
// (~4,423) = the chokepoint slice is not reaching the fan and (b)(2)
// would be real — re-open. Expected GREEN.
//
// This is the permanent pin that the discovery-cache cost-trim does not
// silently re-introduce a full-set fan: Resolve faithfully iterates
// whatever ds it is GIVEN; the windowing lives upstream in ds. The
// contrasting full-set sub-test documents that the bound is ds's
// pre-windowed size (5), not anything internal to Resolve.
//
// The package had NO test file before this — exactly the gap the §7 pin
// closes.

package resourcesrefstemplate

import (
	"context"
	"fmt"
	"testing"

	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"k8s.io/utils/ptr"
)

// compositionsPageDatagridItems builds the iterator-template slice for the
// canonical compositions-page-datagrid widget: one ResourceRefTemplate
// whose Iterator is ${.compositionspanels} and whose Template fans a
// per-panel GET (the live shape per the §7 trace,
// e2e/bench/bench/cluster.py probe). Mirrors how the widget's
// resourcesRefs is declared.
func compositionsPageDatagridItems() []templatesv1.ResourceRefTemplate {
	return []templatesv1.ResourceRefTemplate{
		{
			Iterator: ptr.To("${.compositionspanels}"),
			Template: templatesv1.ResourceRef{
				ID:         "${ .name }",
				Verb:       "GET",
				APIVersion: "widgets.templates.krateo.io/v1beta1",
				Name:       "${ .name }",
				Namespace:  "${ .namespace }",
				Resource:   "panels",
			},
		},
	}
}

// dsWithPanels builds a ds whose .compositionspanels holds exactly n panel
// objects — modelling the post-chokepoint sliced set fed to the fan.
func dsWithPanels(n int) map[string]any {
	panels := make([]any, 0, n)
	for i := 0; i < n; i++ {
		panels = append(panels, map[string]any{
			"name":      fmt.Sprintf("panel-%d", i),
			"namespace": "comp-ns",
		})
	}
	return map[string]any{
		"compositionspanels": panels,
	}
}

// TestResolve_FanIsWindowedAtPerPage is the F-premise gate. With ds
// pre-windowed to perPage=5 (the post-chokepoint set at page 3), the fan
// yields exactly 5 ResourceRefs — NOT the full ~4,423.
func TestResolve_FanIsWindowedAtPerPage(t *testing.T) {
	const perPage = 5 // page=3, perPage=5 — the §7 / C9 scenario.

	// ds.compositionspanels already sliced to perPage at the apiRef
	// chokepoint (hop 4). This is the contract the cost-trim relies on.
	ds := dsWithPanels(perPage)

	refs, err := Resolve(context.Background(), compositionsPageDatagridItems(), ds)
	if err != nil {
		t.Fatalf("Resolve errored: %v", err)
	}

	// THE PIN: the fan is bounded by ds's pre-windowed size.
	if len(refs) > perPage {
		t.Fatalf("F-premise RED: resourcesrefstemplate.Resolve fanned %d ResourceRefs "+
			"at page=3/perPage=%d — the fan is NOT windowed at the apiRef chokepoint "+
			"(Step 3(b)(2) would be real; re-open). Expected <= %d.",
			len(refs), perPage, perPage)
	}
	if len(refs) != perPage {
		t.Fatalf("expected exactly %d ResourceRefs for a %d-panel pre-windowed ds "+
			"(one per panel), got %d", perPage, perPage, len(refs))
	}

	// Sanity: the fanned refs carry the per-element jq evaluation
	// (ID/Name resolved from each panel) — proving ForEach walked the
	// windowed set element-by-element, not a single collapsed entry.
	for i, r := range refs {
		want := fmt.Sprintf("panel-%d", i)
		if r.Name != want {
			t.Errorf("ref[%d].Name = %q, want %q (per-element jq over the windowed iterator)",
				i, r.Name, want)
		}
		if r.Verb != "GET" {
			t.Errorf("ref[%d].Verb = %q, want GET", i, r.Verb)
		}
	}
}

// TestResolve_FanTracksDsSize documents that the fan size is governed by
// ds's pre-windowed item count — the bound lives UPSTREAM in ds, not
// inside Resolve. The full-set ds (4,423) yields 4,423 refs; this is the
// RED state Step 3(b)(2) feared and the reason the windowing MUST stay at
// the chokepoint (it does — TestResolve_FanIsWindowedAtPerPage). Run as a
// contrast so a future change that accidentally pre-windows inside Resolve
// (or fails to window in ds) is visible.
func TestResolve_FanTracksDsSize(t *testing.T) {
	const fullSet = 4423 // the §7 live shape: 4,423 label-filtered panels.

	ds := dsWithPanels(fullSet)
	refs, err := Resolve(context.Background(), compositionsPageDatagridItems(), ds)
	if err != nil {
		t.Fatalf("Resolve errored: %v", err)
	}
	if len(refs) != fullSet {
		t.Fatalf("Resolve fanned %d refs for a %d-item ds; want %d — the fan must "+
			"track ds's size (windowing is the chokepoint's job, not Resolve's)",
			len(refs), fullSet, fullSet)
	}
}

// TestResolve_EmptyWindowYieldsNoRefs pins the boundary: a page past the
// end (empty post-chokepoint ds) fans zero refs, no error.
func TestResolve_EmptyWindowYieldsNoRefs(t *testing.T) {
	ds := dsWithPanels(0)
	refs, err := Resolve(context.Background(), compositionsPageDatagridItems(), ds)
	if err != nil {
		t.Fatalf("Resolve errored on empty window: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("empty post-chokepoint ds fanned %d refs; want 0", len(refs))
	}
}

// TestFARCH_InlineNeverPropagatesFromTemplate — A2 F-ARCH-5 guard-completeness
// invariant (definitive-cache-identity-architecture §4.4.6 + §8.1). The
// inline-parent per-USER key marker (dispatchers inlineParentIdentityForKey →
// hasInlineGETRef) statically scans a widget's DECLARED spec.resourcesRefs for
// inline:true. That static scan is SOUND only because createResourceRef here
// NEVER copies Template.Inline into the expanded ref (it copies only
// ID/Verb/APIVersion/Name/Namespace/Resource — resolve.go:75-81): a
// template-expanded ref cannot silently become inline post-scan.
//
// This arm PINS that non-propagation directly: a ResourceRefTemplate whose
// Template.Inline is TRUE must expand to refs with Inline UNSET (false). If a
// later change adds `out.Inline = in.Template.Inline` to createResourceRef,
// this arm goes RED — catching the leak BEFORE the F-ARCH-5 static scan silently
// under-approximates (a template-expanded inline child would then bypass the
// per-user marker, embed identity-varying content under a per-binding-shared
// key, and reopen the cross-user leak §4.4.6 closes). Cheap, hermetic, no cluster.
func TestFARCH_InlineNeverPropagatesFromTemplate(t *testing.T) {
	items := []templatesv1.ResourceRefTemplate{{
		Iterator: ptr.To("${.items}"),
		Template: templatesv1.ResourceRef{
			Inline:     true, // author sets Inline on the TEMPLATE
			Verb:       "get",
			Resource:   "configmaps",
			APIVersion: "v1",
			Name:       "${ .name }",
			Namespace:  "demo-system",
		},
	}}
	ds := map[string]any{"items": []any{
		map[string]any{"name": "cm-a"},
		map[string]any{"name": "cm-b"},
	}}

	refs, err := Resolve(context.Background(), items, ds)
	if err != nil {
		t.Fatalf("Resolve errored: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 expanded refs (one per ds item); got %d", len(refs))
	}
	for i, r := range refs {
		if r.Inline {
			t.Fatalf("F-ARCH inline-completeness: expanded ref[%d] carries Inline=true — createResourceRef must NOT propagate Template.Inline (resolve.go:75-81 copies only ID/Verb/APIVersion/Name/Namespace/Resource). A template-expanded inline child would bypass the F-ARCH-5 static hasInlineGETRef scan and reopen the §4.4.6 cross-user leak. RED = someone added out.Inline = in.Template.Inline.", i)
		}
	}
}
