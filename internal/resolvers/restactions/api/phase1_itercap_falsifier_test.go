// phase1_itercap_falsifier_test.go — Ship 0.30.127 falsifier for the
// phase1IteratorCap traversal bug.
//
// THE BUG (root cause of F2's warmed=2): phase1IteratorCap (=3, added
// 0.30.111) capped a `dependsOn.iterator` api stage to its first 3
// elements under a Phase-1 (startup-warmup) context. The sidebar-nav-menu's
// apiRef RESTAction iterates a per-namespace navmenuitems LIST; at bench
// scale the iterator's first 3 namespaces are bench-ns-01/02/03, which
// hold ZERO navmenuitems — the real nav-menu-item-* CRs live in
// krateo-system, past the cap. The navmenu's resourcesRefsTemplate then
// expanded to zero children → the Phase-1 walk descended nothing past
// the roots → the F2 apiRef harvester collected only the 2 root apiRefs
// → content.prewarm warmed=2; compositions-list never harvested.
//
// THE FIX (0.30.127): phase1IteratorCap and its IsPhase1Resolution gate
// are DELETED. createRequestOptions now expands a `dependsOn.iterator`
// stage FULLY regardless of context — so the navmenuitems iterator
// covers every namespace, krateo-system is included, and the navmenu's
// resourcesRefsTemplate yields the real nav-menu-item-* children.
//
// THE FALSIFIER drives the REAL createRequestOptions over a >3-element
// iterator under a Phase-1 context. PRE-FIX the cap truncated to 3
// (captured: /tmp/snowplow-runs/0.30.127/preflight/FAL127-prefix-itercap.txt
// — `phase1 iterator capped expanded=3 skipped=4`). POST-FIX (this file,
// the cap deleted) the iterator expands to ALL elements and the
// krateo-system element — the one carrying the navmenuitems — is
// present. The pre→post inversion is the falsifier proof.

//go:build unit
// +build unit

package api

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
)

// benchScaleNamespaces models the bench cluster's namespace set in the
// ORDER the iterator drains them: the bench-ns-* namespaces FIRST (they
// hold no navmenuitems), with krateo-system — where the real
// nav-menu-item-* CRs live — buried at index 6, past the old cap of 3.
func benchScaleNamespaces() []any {
	out := []any{}
	for _, n := range []string{"bench-ns-01", "bench-ns-02", "bench-ns-03",
		"bench-ns-04", "bench-ns-05", "kube-system", "krateo-system"} {
		out = append(out, n)
	}
	return out
}

// TestFAL127_PostFix_IteratorExpandsFullyReachingNavmenuitemNamespace is
// the post-fix proof. With phase1IteratorCap deleted, createRequestOptions
// expands the per-namespace iterator FULLY: all 7 namespaces, including
// krateo-system at index 6. The navmenu's apiRef RESTAction therefore
// LISTs navmenuitems in krateo-system, its resourcesRefsTemplate yields
// the real nav-menu-item-dashboard/compositions/blueprints children, and
// the Phase-1 walk descends them — so F2 harvests compositions-list.
//
// Ship 0.30.127 swept the cache.WithPhase1Resolution marker (its only
// consumer, the cap, is gone). This test uses a plain context.Background()
// — the assertion (the iterator expands fully) holds regardless of
// context, which is precisely the point: there is no longer any context
// flavour under which the iterator is capped.
func TestFAL127_PostFix_IteratorExpandsFullyReachingNavmenuitemNamespace(t *testing.T) {
	dict := map[string]any{"namespaces": benchScaleNamespaces()}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	all := createRequestOptions(context.Background(), logger, &templates.API{
		Name: "navmenuitems-per-ns",
		Path: `${ "/apis/widgets.templates.krateo.io/v1beta1/namespaces/" + (.) + "/navmenuitems" }`,
		DependsOn: &templates.Dependency{
			Name:     "namespaces",
			Iterator: ptr.To(".[]"),
		},
	}, dict)

	// THE FIX: the iterator expands to ALL 7 namespaces — no truncation.
	if len(all) != len(benchScaleNamespaces()) {
		t.Fatalf("FAL-127 FIX FAILED: expected the iterator to expand FULLY to %d "+
			"request options (the cap is deleted), got %d — the cap still truncates",
			len(benchScaleNamespaces()), len(all))
	}
	// And krateo-system — the namespace holding the real navmenuitems —
	// IS in the expansion (pre-fix it was skipped at index 6 > cap 3).
	foundKrateoSystem := false
	for _, el := range all {
		if contains(el.Path, "/namespaces/krateo-system/navmenuitems") {
			foundKrateoSystem = true
		}
	}
	if !foundKrateoSystem {
		t.Fatalf("FAL-127 FIX FAILED: krateo-system (iterator element 6) must be in the "+
			"FULL expansion — it is the namespace that holds the nav-menu-item-* CRs "+
			"the Phase-1 walk must descend to reach compositions-list")
	}
}

// contains() is shared with jsoncopy_test.go in this package (same substring helper);
// the duplicate definition here was removed to fix a redeclaration compile error.
