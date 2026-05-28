package cache

// Ship-1.1 regression guards — CohortNSACL under the SA identity vs the
// Group identity, against the live SA */* CRB wire shape.
//
// NON-DESTRUCTIVE: pure in-package unit probe over a hand-built
// *RBACSnapshot (PublishRBACSnapshotForTest equivalent via direct field
// set). Does NOT touch the remote kubeconfig.
//
// ROOT CAUSE (PINNED, now FIXED): pre-Ship-1.1 CohortNSACL computed
// permitAll=false for the snowplow SA even though the SA holds a */*
// get/list/watch CRB, because collectCohortClusterBindings /
// collectCohortNamespaceBindings consulted ONLY CRBsByUser + CRBsByGroup —
// NEVER CRBsByServiceAccount. The SA's grant is a ServiceAccount-kind
// subject, so it lands in CRBsByServiceAccount and was invisible to
// CohortNSACL → permitAll=false → the SA discovery re-walk's `namespaces`
// stage kept 0/62 → 0 children. Ship 1.1 adds the ServiceAccount-kind
// landing (+ synthetic-SA-group expansion) to both collect functions in
// symmetry with the User/Group landings, mirroring EvaluateRBAC. These
// probes now guard against a regression that re-introduces the blindness.

import (
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func ship11StarRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "snowplow-krateo-system", UID: "uid-cr-star"},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"get", "list", "watch"}},
		},
	}
}

// buildSAStarSnapshot builds a snapshot with the live SA */* CRB wire
// shape: a ServiceAccount-kind subject bound to the */* ClusterRole.
func buildSAStarSnapshot() *RBACSnapshot {
	saCRB := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "snowplow-krateo-system", UID: "uid-crb-sa"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "snowplow", Namespace: "krateo-system"}},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "snowplow-krateo-system"},
	}
	// A SECOND CRB binding the SAME */* role to a Group, mirroring the live
	// admins→cluster-admin shape (the permitAll=true seed cohort).
	groupCRB := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "admins-cluster-admin", UID: "uid-crb-grp"},
		Subjects:   []rbacv1.Subject{{Kind: "Group", APIGroup: "rbac.authorization.k8s.io", Name: "admins"}},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "snowplow-krateo-system"},
	}
	snap := &RBACSnapshot{
		ClusterRoleBindings: []*rbacv1.ClusterRoleBinding{saCRB, groupCRB},
		RoleBindingsByNS:    map[string][]*rbacv1.RoleBinding{},
		ClusterRolesByName:  map[string]*rbacv1.ClusterRole{"snowplow-krateo-system": ship11StarRole()},
		RolesByNSName:       map[string]*rbacv1.Role{},
	}
	rebuildSubjectIndexes(snap)
	return snap
}

// PROBE A — CohortNSACL under the SA identity. Ship 1.1 FIX VERIFICATION +
// regression guard. PRE-fix this returned permitAll=false (CohortNSACL was
// structurally blind to CRBsByServiceAccount); POST-fix it MUST return
// permitAll=true — the SA's */* CRB is now collected via the
// ServiceAccount-kind landing, mirroring EvaluateRBAC. A regression that
// drops the SA landing flips this back to false and fails here.
func TestShip11CohortNSACL_SA_SeesServiceAccountGrant(t *testing.T) {
	snap := buildSAStarSnapshot()
	gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}

	permitAll, permittedNS := CohortNSACL(snap, "system:serviceaccount:krateo-system:snowplow", nil, gvr)
	t.Logf("PROBE-A CohortNSACL(SA) => permitAll=%v permittedNS=%v", permitAll, len(permittedNS))

	if !permitAll {
		t.Errorf("REGRESSION: CohortNSACL permitAll=false for the snowplow SA despite its */* CRB — "+
			"the ServiceAccount-kind landing (CRBsByServiceAccount[%q]=%d) is not being collected; "+
			"CohortNSACL has diverged from EvaluateRBAC again",
			"krateo-system/snowplow", len(snap.CRBsByServiceAccount["krateo-system/snowplow"]))
	} else {
		t.Logf("FIX VERIFIED: CohortNSACL permitAll=true for the SA — its */* CRB "+
			"(CRBsByServiceAccount[%q]=%d, CRBsByUser=%d) is now collected via the "+
			"ServiceAccount-kind landing, in agreement with EvaluateRBAC",
			"krateo-system/snowplow", len(snap.CRBsByServiceAccount["krateo-system/snowplow"]),
			len(snap.CRBsByUser["system:serviceaccount:krateo-system:snowplow"]))
	}
}

// PROBE B — CohortNSACL under the Group [admins] identity (the live
// permitAll=true seed cohort). EXPECT permitAll=true — proving the SAME
// */* role IS visible via the Group landing. The SA-vs-Group asymmetry
// is the discriminator (candidate 1, NOT a stale/not-warm cache 2/3).
func TestShip11CohortNSACL_Group_SeesGrant(t *testing.T) {
	snap := buildSAStarSnapshot()
	gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}

	permitAll, _ := CohortNSACL(snap, groupOnlyCohortSentinel, []string{"admins", "system:authenticated"}, gvr)
	t.Logf("PROBE-B CohortNSACL(sentinel+[admins]) => permitAll=%v", permitAll)
	if !permitAll {
		t.Errorf("Group [admins] should see the */* role via CRBsByGroup — got permitAll=false")
	}
}

// PROBE C — confirm the snapshot index DID place the SA grant where
// CohortNSACL does NOT look. Proves the grant exists, just unreachable.
func TestShip11Snapshot_SAGrantIndexedUnderServiceAccount(t *testing.T) {
	snap := buildSAStarSnapshot()
	if n := len(snap.CRBsByServiceAccount["krateo-system/snowplow"]); n != 1 {
		t.Errorf("expected SA */* CRB indexed under CRBsByServiceAccount[krateo-system/snowplow], got %d", n)
	}
	if n := len(snap.CRBsByUser["system:serviceaccount:krateo-system:snowplow"]); n != 0 {
		t.Errorf("SA grant must NOT be under CRBsByUser (it is a ServiceAccount-kind subject), got %d", n)
	}
	t.Logf("CONFIRMED: SA grant lives in CRBsByServiceAccount (1), absent from CRBsByUser (0) — "+
		"Ship 1.1 CohortNSACL collects the ServiceAccount-kind landing, so this grant is now reachable "+
		"(verified by PROBE-A returning permitAll=true)")
}
