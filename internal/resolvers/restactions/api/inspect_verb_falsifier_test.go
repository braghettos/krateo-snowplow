// inspect_verb_falsifier_test.go — /rbac in-cluster-stage verb fix
// (docs/restaction-rbac-endpoint-design.md §4).
//
// THE BUG (#44 shipped it unfixed): inspectInClusterStage emitted a CONSTANT
// Verb:"get" for every non-UAF in-cluster stage, on the (wrong) rationale
// that "core.go:30 bounds the HTTP-stage method to GET/HEAD, so get is exact".
// But the RBAC verb for a COLLECTION GET (`GET /apis/g/v/resource`, no object
// name) is `list`, NOT `get` — the apiserver authorizes a collection read as
// the `list` verb. So a RESTAction whose in-cluster stage is a collection LIST
// (the dominant catalog shape, e.g. `/apis/.../compositiondefinitions`) was
// granted `get` on that resource, which 403s a real LIST at /call → the
// core-provider under-grants and the catalog read fails.
//
// THE FIX: capture the object name from ParseAPIServerPathToDep and emit
// `verb := "list"; if name != "" { verb = "get" }`. A by-name GET
// (`.../pods/my-pod`) stays `get`; a collection GET becomes `list`. UAF stages
// (which emit uaf.Verb verbatim) are untouched.
//
// Falsifier-FIRST (feedback_falsifier_first_before_ship): this test is written
// BEFORE the inspect.go change. The collection-LIST assertion FAILS against the
// unfixed tree (it emits `get`); the by-name + UAF assertions pass either way
// (negative controls that pin the fix does NOT over-reach). Kind/in-process
// only — NEVER remote go test (feedback_no_go_test_against_remote_kubeconfig).

package api

import (
	"context"
	"testing"

	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
)

// TestInspect_CollectionStage_VerbList is the DISCRIMINATING falsifier: a
// non-UAF in-cluster stage whose path is a COLLECTION (no trailing object
// name) must emit verb=list. Pre-fix this FAILS (constant get). The
// group/version/resource/namespace of the row are asserted unchanged — only
// the verb flips.
func TestInspect_CollectionStage_VerbList(t *testing.T) {
	withInspectSARESTConfig(t, fakeDiscoveryServer(t))

	ra := &templates.RESTAction{
		Spec: templates.RESTActionSpec{
			API: []*templates.API{
				// Cluster-scoped collection LIST — no object name. namespaces is
				// cluster-scoped (namespaced:false) and served by the fake
				// discovery, so validateGVR passes. This is the dominant catalog
				// shape (/blueprints-class).
				{Name: "ns-list", Path: "/api/v1/namespaces"},
			},
		},
	}
	rows, unresolved, err := InspectReadSet(context.Background(), ra, nil)
	if err != nil {
		t.Fatalf("InspectReadSet errored: %v", err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("expected zero unresolved, got %+v", unresolved)
	}

	row, ok := findRow(rows, "", "namespaces", "list")
	if !ok {
		t.Fatalf("VERB FALSIFIER FAIL: a COLLECTION in-cluster stage "+
			"(/api/v1/namespaces, no object name) must emit verb=list — the apiserver "+
			"authorizes a collection read as `list`, so the prior constant `get` "+
			"UNDER-GRANTS and 403s the real LIST at /call. got rows=%+v", rows)
	}
	// The rest of the row is unchanged (group/version/resource present; the
	// cluster-scoped collection has empty namespace). Only the verb flipped.
	if row.Group != "" || row.Resource != "namespaces" {
		t.Fatalf("verb fix must not alter group/resource; got %+v", row)
	}
	if row.Namespace != "" {
		t.Fatalf("cluster-scoped collection must have empty namespace; got %q", row.Namespace)
	}
	// And there must be NO stray `get` row for the same resource (the fix
	// REPLACES the verb, it does not add a second row).
	if _, dupGet := findRow(rows, "", "namespaces", "get"); dupGet {
		t.Fatalf("verb fix must REPLACE get with list, not emit both; got %+v", rows)
	}
}

// TestInspect_NamespacedCollectionStage_VerbList — the namespaced collection
// form (`/api/v1/namespaces/<ns>/pods`, no object name) must also be `list`,
// with the namespace preserved on the row.
func TestInspect_NamespacedCollectionStage_VerbList(t *testing.T) {
	withInspectSARESTConfig(t, fakeDiscoveryServer(t))

	ra := &templates.RESTAction{
		Spec: templates.RESTActionSpec{
			API: []*templates.API{
				{Name: "pods", Path: "/api/v1/namespaces/krateo-system/pods"},
			},
		},
	}
	rows, _, err := InspectReadSet(context.Background(), ra, nil)
	if err != nil {
		t.Fatalf("InspectReadSet errored: %v", err)
	}
	row, ok := findRow(rows, "", "pods", "list")
	if !ok {
		t.Fatalf("VERB FALSIFIER FAIL: namespaced COLLECTION stage "+
			"(/api/v1/namespaces/krateo-system/pods, no object name) must emit verb=list; got %+v", rows)
	}
	if row.Namespace != "krateo-system" {
		t.Fatalf("namespace must be preserved on the list row; got %q", row.Namespace)
	}
}

// TestInspect_GetByNameStage_VerbGet is the NEGATIVE CONTROL: a by-name GET
// (path ending in an object name) must STILL emit verb=get — the fix must not
// over-reach and turn legitimate by-name reads into list. Passes both pre- and
// post-fix; pins the discriminant.
func TestInspect_GetByNameStage_VerbGet(t *testing.T) {
	withInspectSARESTConfig(t, fakeDiscoveryServer(t))

	ra := &templates.RESTAction{
		Spec: templates.RESTActionSpec{
			API: []*templates.API{
				// Object name present → GET-by-name → verb=get.
				{Name: "one-pod", Path: "/api/v1/namespaces/krateo-system/pods/my-pod"},
			},
		},
	}
	rows, _, err := InspectReadSet(context.Background(), ra, nil)
	if err != nil {
		t.Fatalf("InspectReadSet errored: %v", err)
	}
	row, ok := findRow(rows, "", "pods", "get")
	if !ok {
		t.Fatalf("VERB FALSIFIER FAIL: a GET-by-name stage "+
			"(/api/v1/namespaces/krateo-system/pods/my-pod) must STILL emit verb=get; got %+v", rows)
	}
	if row.Namespace != "krateo-system" {
		t.Fatalf("namespace must be preserved on the get row; got %q", row.Namespace)
	}
	// No stray list row for the by-name read.
	if _, dupList := findRow(rows, "", "pods", "list"); dupList {
		t.Fatalf("a by-name GET must NOT emit a list row; got %+v", rows)
	}
}

// TestInspect_UAFVerb_UntouchedByVerbFix is the negative control for the UAF
// path: the fix touches ONLY inspectInClusterStage; a UAF stage still emits
// uaf.Verb verbatim (here a free-form verb on a collection path that, were it a
// plain in-cluster stage, would now be `list`). Proves the verb fix did not
// leak into inspectUAFStage.
func TestInspect_UAFVerb_UntouchedByVerbFix(t *testing.T) {
	withInspectSARESTConfig(t, fakeDiscoveryServer(t))

	ra := &templates.RESTAction{
		Spec: templates.RESTActionSpec{
			API: []*templates.API{
				{
					Name: "ns",
					Path: "/api/v1/namespaces", // collection path, but UAF-classified
					UserAccessFilter: &templates.UserAccessFilterSpec{
						Verb:     "deletecollection",
						Group:    "",
						Resource: "namespaces",
					},
				},
			},
		},
	}
	rows, _, err := InspectReadSet(context.Background(), ra, nil)
	if err != nil {
		t.Fatalf("InspectReadSet errored: %v", err)
	}
	if _, ok := findRow(rows, "", "namespaces", "deletecollection"); !ok {
		t.Fatalf("UAF row must emit uaf.Verb verbatim (deletecollection), untouched by the "+
			"in-cluster verb fix; got %+v", rows)
	}
	// The UAF path must NOT have been rewritten to list/get by the fix.
	if _, leakedList := findRow(rows, "", "namespaces", "list"); leakedList {
		t.Fatalf("verb fix leaked into the UAF path (emitted list); got %+v", rows)
	}
}
