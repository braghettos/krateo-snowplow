// cluster_list_uaf_derive_test.go — Ship S.2 / 0.30.213 unit tests for the
// NEW UAF-derivation second path that lets compositions-panels qualify
// for the cluster-list collapse (PM Condition 2, design risk R7).
//
// Four sub-tests per PM acceptance:
//
//   (a) Match — correct sibling match for compositions-panels shape
//       returns the expected GVR.
//   (b) Wrong-verb — sibling UAF has verb != list → no-derive.
//   (c) Wrong-GVR — iterator path-template GVR ≠ sibling UAF resource
//       → no-derive.
//   (d) Adversarial two-siblings — RA spec has TWO UAF siblings, only
//       one matches the iterator's GVR; helper picks the matching one,
//       NOT the wrong one. If two siblings match the SAME resource on
//       DIFFERENT groups (ambiguous data), helper fails-closed
//       (no-derive). Resolution choice documented inline.
//
// Per memory feedback_no_special_cases: helper reads from RA spec only,
// no hardcoded resource/path allowlist.

package api

import (
	"context"
	"testing"

	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/plumbing/ptr"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// compositionsPanelsStage builds the stage-2 (iterator-over-namespaces)
// shape from the live compositions-panels RA spec (design §2.1). The
// path template substitutes the iterator element into the namespace
// segment. The iterator-element shape is a BARE STRING (from stage 1's
// post-filter `[.namespaces.items[] | .metadata.name]`), so the
// original deriveTargetGVRForClusterList path FAILS — the S.2 fallback
// must succeed.
func compositionsPanelsStage() *templates.API {
	return &templates.API{
		Name:      "compositionspanels",
		Path:      `${ "/apis/widgets.templates.krateo.io/v1beta1/namespaces/" + (.) + "/panels" }`,
		DependsOn: &templates.Dependency{Iterator: ptr.To(".namespaces")},
	}
}

// compositionsPanelsUAFSibling builds the stage-1 (namespaces) sibling
// that carries the userAccessFilter the S.2 fallback keys off:
// verb=list, resource=panels, group=widgets.templates.krateo.io.
// Mirrors the live spec verbatim.
func compositionsPanelsUAFSibling() *templates.API {
	return &templates.API{
		Name: "namespaces",
		Path: "/api/v1/namespaces",
		UserAccessFilter: &templates.UserAccessFilterSpec{
			Verb:     "list",
			Group:    "widgets.templates.krateo.io",
			Resource: "panels",
		},
	}
}

// (a) Match — correct sibling match returns the expected GVR.
func TestDeriveTargetGVRFromUAFStage_Match(t *testing.T) {
	stage := compositionsPanelsStage()
	siblings := []*templates.API{compositionsPanelsUAFSibling(), stage}

	gvr, ok := deriveTargetGVRForClusterListFromUAFStage(
		context.Background(), clusterListLogger(t), stage, siblings)
	if !ok {
		t.Fatalf("expected ok=true on compositions-panels shape; got false")
	}
	want := schema.GroupVersionResource{
		Group:    "widgets.templates.krateo.io",
		Version:  "v1beta1",
		Resource: "panels",
	}
	if gvr != want {
		t.Fatalf("derived GVR mismatch:\n  got  %s\n  want %s", gvr, want)
	}
}

// (b) Wrong-verb — sibling UAF has verb != list → no-derive.
func TestDeriveTargetGVRFromUAFStage_WrongVerb(t *testing.T) {
	stage := compositionsPanelsStage()
	sibling := compositionsPanelsUAFSibling()
	sibling.UserAccessFilter.Verb = "get" // not list
	siblings := []*templates.API{sibling, stage}

	_, ok := deriveTargetGVRForClusterListFromUAFStage(
		context.Background(), clusterListLogger(t), stage, siblings)
	if ok {
		t.Fatalf("expected ok=false when sibling UAF verb != list")
	}
}

// (c) Wrong-GVR — iterator path-template GVR ≠ sibling UAF resource
// → no-derive. The iterator targets `panels` but the sibling UAF
// targets `widgets` (a different resource entirely).
func TestDeriveTargetGVRFromUAFStage_WrongGVR(t *testing.T) {
	stage := compositionsPanelsStage()
	sibling := compositionsPanelsUAFSibling()
	sibling.UserAccessFilter.Resource = "widgets" // template says "panels"
	siblings := []*templates.API{sibling, stage}

	_, ok := deriveTargetGVRForClusterListFromUAFStage(
		context.Background(), clusterListLogger(t), stage, siblings)
	if ok {
		t.Fatalf("expected ok=false when sibling UAF resource mismatches template")
	}
}

// (d) Adversarial two-siblings — TWO UAF siblings, only one matches
// the template's GVR; helper picks the matching one. Resolution
// choice for matching-resource-different-group: FAIL-CLOSED (no-derive)
// to avoid mis-targeting the cluster-LIST cell.
//
// Sub-test (d.1): Two siblings, ONE matches, ONE doesn't. Helper picks
// the matching one.
// Sub-test (d.2): Two siblings BOTH match resource=panels but on
// DIFFERENT groups (ambiguous). Helper fails-closed.
func TestDeriveTargetGVRFromUAFStage_AdversarialTwoSiblings(t *testing.T) {
	t.Run("OneMatchesOneDoesnt", func(t *testing.T) {
		stage := compositionsPanelsStage()
		correctSibling := compositionsPanelsUAFSibling()
		wrongSibling := &templates.API{
			Name: "other",
			Path: "/api/v1/configmaps",
			UserAccessFilter: &templates.UserAccessFilterSpec{
				Verb:     "list",
				Group:    "",
				Resource: "configmaps", // different resource
			},
		}
		// Order: wrong first, correct second — helper must pick the
		// correct one regardless of declaration order.
		siblings := []*templates.API{wrongSibling, correctSibling, stage}

		gvr, ok := deriveTargetGVRForClusterListFromUAFStage(
			context.Background(), clusterListLogger(t), stage, siblings)
		if !ok {
			t.Fatalf("expected ok=true when at least one sibling matches; got false")
		}
		want := schema.GroupVersionResource{
			Group:    "widgets.templates.krateo.io",
			Version:  "v1beta1",
			Resource: "panels",
		}
		if gvr != want {
			t.Fatalf("derived GVR mismatch:\n  got  %s\n  want %s", gvr, want)
		}
	})

	t.Run("TwoMatchingResourceDifferentGroup_FailsClosed", func(t *testing.T) {
		// IMPORTANT — this is the architect's flagged tension case (R7
		// adversarial). Two siblings both declare `verb=list,
		// resource=panels` but on DIFFERENT groups. The template's
		// extracted group is widgets.templates.krateo.io; sibling A
		// matches it, sibling B declares a different group also called
		// "panels".
		//
		// Resolution choice (documented per cache-developer brief
		// instruction): FAIL-CLOSED. Even though sibling A's group
		// matches the template-extracted group, sibling B's presence
		// on the same resource = AMBIGUOUS data — the RA author's
		// intent is unclear. Per memory feedback_no_special_cases we
		// don't pick a tie-break heuristic that could silently
		// mis-target; we deny the collapse and let the per-NS iterator
		// path serve.
		//
		// In practice this case never arises on the production cluster
		// (only 2 RAs qualify, both target the SAME (group,resource)
		// per the byte-budget probe), but the fail-closed branch
		// MUST exist to defend against future RA spec drift.
		stage := compositionsPanelsStage()
		correctSibling := compositionsPanelsUAFSibling()
		ambiguousSibling := &templates.API{
			Name: "another-uaf",
			Path: "/apis/other.group.io/v1/namespaces", // unrelated path
			UserAccessFilter: &templates.UserAccessFilterSpec{
				Verb:     "list",
				Group:    "other.group.io", // different from extracted
				Resource: "panels",         // SAME resource as template
			},
		}
		siblings := []*templates.API{correctSibling, ambiguousSibling, stage}

		_, ok := deriveTargetGVRForClusterListFromUAFStage(
			context.Background(), clusterListLogger(t), stage, siblings)
		if ok {
			t.Fatalf("expected ok=false on ambiguous matching-resource-different-group (R7 adversarial); got true")
		}
	})
}

// TestDeriveTargetGVRFromUAFStage_NoSiblings — empty siblings slice
// disables the fallback. Confirms back-compat with the pre-S.2 call
// shape (tests pass nil).
func TestDeriveTargetGVRFromUAFStage_NoSiblings(t *testing.T) {
	stage := compositionsPanelsStage()
	_, ok := deriveTargetGVRForClusterListFromUAFStage(
		context.Background(), clusterListLogger(t), stage, nil)
	if ok {
		t.Fatalf("expected ok=false with nil siblings")
	}
	_, ok = deriveTargetGVRForClusterListFromUAFStage(
		context.Background(), clusterListLogger(t), stage, []*templates.API{})
	if ok {
		t.Fatalf("expected ok=false with empty siblings")
	}
}

// TestDeriveTargetGVRFromUAFStage_NoUAFOnSiblings — siblings exist but
// none carry a userAccessFilter. Confirms the helper does not accept a
// stage's path template alone (the UAF cross-check is load-bearing).
func TestDeriveTargetGVRFromUAFStage_NoUAFOnSiblings(t *testing.T) {
	stage := compositionsPanelsStage()
	plainSibling := &templates.API{Name: "namespaces", Path: "/api/v1/namespaces"}
	siblings := []*templates.API{plainSibling, stage}
	_, ok := deriveTargetGVRForClusterListFromUAFStage(
		context.Background(), clusterListLogger(t), stage, siblings)
	if ok {
		t.Fatalf("expected ok=false when no sibling carries a UAF")
	}
}

// TestDeriveTargetGVRFromUAFStage_ClusterScopeTemplate — path template
// is cluster-scoped (no /namespaces/ segment). Helper rejects so the
// caller keeps the iterator verbatim.
func TestDeriveTargetGVRFromUAFStage_ClusterScopeTemplate(t *testing.T) {
	stage := &templates.API{
		Name: "cluster-stage",
		Path: `${ "/apis/widgets.templates.krateo.io/v1beta1/panels/" + (.) }`,
		DependsOn: &templates.Dependency{Iterator: ptr.To(".names")},
	}
	siblings := []*templates.API{compositionsPanelsUAFSibling(), stage}
	_, ok := deriveTargetGVRForClusterListFromUAFStage(
		context.Background(), clusterListLogger(t), stage, siblings)
	if ok {
		t.Fatalf("expected ok=false on cluster-scope template (no /namespaces/ segment)")
	}
}
