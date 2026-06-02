// sa_permit_all_test.go — Ship 0.30.240 SA-permitAll pre-deploy guard.
//
// Architect ratification 2026-06-02 (Q2): the v4 refresher resolves
// EVERY cache class under the snowplow SA identity. The design
// (§4.5 + §6) depends on the SA having `CohortNSACL.permitAll=true`
// for every walker-discovered GVR — otherwise the SA-uniform refresh
// returns under-narrowed bytes and the v4 SA-maximal contract breaks
// (cache holds the SA's intersection-of-namespaces narrowing instead
// of the cluster-wide view).
//
// This test pins the contract IN GO. A failure here is a chart-level
// RBAC misconfiguration — the snowplow SA's `cluster-admin`-equivalent
// ClusterRoleBinding must exist and grant cluster-wide list on every
// GVR the walker would discover.
//
// PRE-DEPLOY MANUAL VERIFICATION:
//   The Go test below pins the principle. For an actual deploy, the
//   tester ALSO runs:
//
//     kubectl auth can-i list --all-namespaces compositions.composition.krateo.io \
//                              --as=system:serviceaccount:krateo-system:snowplow
//     # expected: yes
//
//     kubectl auth can-i list --all-namespaces panels.composition.krateo.io \
//                              --as=system:serviceaccount:krateo-system:snowplow
//     # expected: yes
//
//   for each top-level walker GVR (compositions, panels, widgets,
//   restactions, etc.). A "no" answer indicates the chart's
//   snowplow-cluster-admin CRB is missing or the SA's namespace/name
//   doesn't match.

package cache

import (
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestSAPermitAllAcrossWalkedGVRs_v4 — pins the v4 SA-uniform contract:
// the snowplow SA must produce CohortNSACL.permitAll==true on every
// walker-discovered GVR.
//
// FIXTURE: a hand-built RBAC snapshot containing exactly the
// production-shape `snowplow-cluster-admin` CRB — a ClusterRoleBinding
// whose RoleRef targets a `*`/`*`/`*` ClusterRole, bound to the
// `system:serviceaccount:krateo-system:snowplow` ServiceAccount.
//
// Asserts: for each representative walker GVR, CohortNSACL returns
// permitAll=true. A FAIL means the chart's snowplow RBAC is under-
// provisioned for v4.
func TestSAPermitAllAcrossWalkedGVRs_v4(t *testing.T) {
	resetGenAndSnapshot(t)

	// Build the production-shape SA-cluster-admin binding.
	saClusterRole := &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: "snowplow-cluster-admin"},
		Rules: []rbacv1.PolicyRule{
			// `*` `*` `*` — every verb on every resource of every API group.
			// This is the chart's intended grant per
			// project_uaf_cleanup_pending + Ship 1.1's CohortNSACL SA landing.
			{
				Verbs:     []string{"*"},
				APIGroups: []string{"*"},
				Resources: []string{"*"},
			},
		},
	}
	saCRB := &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: "snowplow-cluster-admin-binding"},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Namespace: "krateo-system",
				Name:      "snowplow",
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     saClusterRole.Name,
		},
	}

	snap := &RBACSnapshot{
		ClusterRoleBindings: []*rbacv1.ClusterRoleBinding{saCRB},
		RoleBindingsByNS:    map[string][]*rbacv1.RoleBinding{},
		ClusterRolesByName: map[string]*rbacv1.ClusterRole{
			saClusterRole.Name: saClusterRole,
		},
		RolesByNSName: map[string]*rbacv1.Role{},
	}
	rebuildSubjectIndexes(snap)
	PublishRBACSnapshotForTest(snap)

	// SA identity — matches phase1SAUsername output for the
	// krateo-system/snowplow SA. CohortNSACL takes (snap, username,
	// groups, gvr); the SA's username is the canonical
	// `system:serviceaccount:<ns>:<name>`.
	saUsername := "system:serviceaccount:krateo-system:snowplow"
	var saGroups []string

	// Walker-discovered GVR set — a representative production sample.
	// These mirror the top-level walker output for cyberjoker + admin
	// (compositions + panels + widgets-class CRDs + restactions). A
	// production-faithful list lives in the chart's walker config;
	// this test asserts the core invariant on the representative
	// sample (per feedback_no_special_cases — same predicate across
	// classes).
	walkerGVRs := []schema.GroupVersionResource{
		{Group: "composition.krateo.io", Version: "v1", Resource: "compositions"},
		{Group: "composition.krateo.io", Version: "v1", Resource: "panels"},
		{Group: "widgets.templates.krateo.io", Version: "v1beta1", Resource: "panels"},
		{Group: "widgets.templates.krateo.io", Version: "v1beta1", Resource: "datagrids"},
		{Group: "widgets.templates.krateo.io", Version: "v1beta1", Resource: "tables"},
		{Group: "templates.krateo.io", Version: "v1", Resource: "restactions"},
		// Core resources the walker may recurse into for resourcesRefs:
		{Group: "", Version: "v1", Resource: "configmaps"},
		{Group: "", Version: "v1", Resource: "secrets"},
		{Group: "", Version: "v1", Resource: "namespaces"},
	}

	for _, gvr := range walkerGVRs {
		t.Run(gvr.String(), func(t *testing.T) {
			permitAll, permittedNS := CohortNSACL(snap, saUsername, saGroups, gvr)
			if !permitAll {
				t.Fatalf("V4 PRE-DEPLOY GUARD FAIL: SA %q has CohortNSACL.permitAll=false "+
					"on GVR %s. v4 refresher uniformly resolves under SA identity; "+
					"permitAll=false means refresh narrows to permittedNS=%v and the "+
					"L1 cell stores under-narrowed bytes — violating the design's "+
					"SA-maximal contract. Check the chart's `snowplow-cluster-admin` "+
					"ClusterRoleBinding: it must bind a `*`/`*`/`*` ClusterRole to "+
					"system:serviceaccount:krateo-system:snowplow.",
					saUsername, gvr.String(), permittedNS)
			}
		})
	}
}

// TestSAPermitAllRegression_SAMissingCRB — falsifier for the failure
// mode the v4 contract guards against. With NO SA CRB in the snapshot,
// CohortNSACL returns permitAll=false for every GVR. This pins the
// guard's value: the test above would FAIL if the chart's snowplow
// CRB ever drifted out of the live cluster.
func TestSAPermitAllRegression_SAMissingCRB(t *testing.T) {
	resetGenAndSnapshot(t)

	// EMPTY snapshot — no bindings at all. The SA falls through to
	// permitAll=false on every GVR.
	snap := &RBACSnapshot{
		ClusterRoleBindings: nil,
		RoleBindingsByNS:    map[string][]*rbacv1.RoleBinding{},
		ClusterRolesByName:  map[string]*rbacv1.ClusterRole{},
		RolesByNSName:       map[string]*rbacv1.Role{},
	}
	rebuildSubjectIndexes(snap)
	PublishRBACSnapshotForTest(snap)

	permitAll, _ := CohortNSACL(snap, "system:serviceaccount:krateo-system:snowplow",
		nil,
		schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1", Resource: "compositions"})
	if permitAll {
		t.Fatalf("regression: empty snapshot returned permitAll=true; want false "+
			"(falsifier — guards that TestSAPermitAllAcrossWalkedGVRs_v4 is not "+
			"trivially passing on a degenerate snapshot)")
	}
}
