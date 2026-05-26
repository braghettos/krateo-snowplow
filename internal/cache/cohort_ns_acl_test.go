// cohort_ns_acl_test.go — Ship 0.30.178 A.2 unit tests for the per-cohort
// namespace ACL fast-path. Asserts:
//
//   - permitAll=true when a ClusterRoleBinding grants list cluster-wide
//   - permitAll=false + permittedNS = {ns} when a RoleBinding grants
//     list in ns
//   - empty result when no binding matches
//   - resourceNames-scoped rule does NOT grant list (a collection verb)
//   - wildcard apiGroup / resource / verb match correctly
//   - nil-snapshot returns (false, nil) — fail-closed

package cache

import (
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var testGVR = schema.GroupVersionResource{
	Group: "composition.krateo.io", Version: "v1", Resource: "compositions",
}

func TestCohortNSACL_NilSnapshot(t *testing.T) {
	permitAll, ns := CohortNSACL(nil, "alice", []string{"devs"}, testGVR)
	if permitAll || len(ns) != 0 {
		t.Fatalf("CohortNSACL(nil) = (%v, %v), want (false, nil)", permitAll, ns)
	}
}

func TestCohortNSACL_ClusterWidePermitAll(t *testing.T) {
	// alice via CRB → ClusterRole granting "list" on compositions cluster-wide.
	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "compositions-admin"},
		Rules: []rbacv1.PolicyRule{{
			Verbs:     []string{"list", "get"},
			APIGroups: []string{"composition.krateo.io"},
			Resources: []string{"compositions"},
		}},
	}
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "alice-admin"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: "alice"}},
		RoleRef:    rbacv1.RoleRef{Kind: "ClusterRole", Name: cr.Name},
	}

	snap := &RBACSnapshot{
		ClusterRolesByName: map[string]*rbacv1.ClusterRole{cr.Name: cr},
		CRBsByUser:         map[string][]*rbacv1.ClusterRoleBinding{"alice": {crb}},
	}

	permitAll, ns := CohortNSACL(snap, "alice", nil, testGVR)
	if !permitAll {
		t.Fatalf("CohortNSACL: permitAll=false, want true; ns=%v", ns)
	}
	if len(ns) != 0 {
		t.Fatalf("CohortNSACL: permittedNS=%v on permitAll path, want empty", ns)
	}
}

func TestCohortNSACL_ClusterWideWildcardVerb(t *testing.T) {
	// Verbs "*" must match "list".
	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "wild"},
		Rules: []rbacv1.PolicyRule{{
			Verbs:     []string{"*"},
			APIGroups: []string{"composition.krateo.io"},
			Resources: []string{"compositions"},
		}},
	}
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "wild-binding"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.GroupKind, Name: "system:masters"}},
		RoleRef:    rbacv1.RoleRef{Kind: "ClusterRole", Name: cr.Name},
	}
	snap := &RBACSnapshot{
		ClusterRolesByName: map[string]*rbacv1.ClusterRole{cr.Name: cr},
		CRBsByGroup:        map[string][]*rbacv1.ClusterRoleBinding{"system:masters": {crb}},
	}

	permitAll, _ := CohortNSACL(snap, "alice", []string{"system:masters"}, testGVR)
	if !permitAll {
		t.Fatalf("wildcard-verb CRB: permitAll=false, want true")
	}
}

func TestCohortNSACL_NamespaceScopedRoleBinding(t *testing.T) {
	// bob via RB in ns "team-a" → Role granting list on compositions.
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "lister"},
		Rules: []rbacv1.PolicyRule{{
			Verbs:     []string{"list"},
			APIGroups: []string{"composition.krateo.io"},
			Resources: []string{"compositions"},
		}},
	}
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "bob"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: "bob"}},
		RoleRef:    rbacv1.RoleRef{Kind: "Role", Name: role.Name},
	}

	snap := &RBACSnapshot{
		RolesByNSName:    map[string]*rbacv1.Role{"team-a/lister": role},
		RBsByUserByNS:    map[string]map[string][]*rbacv1.RoleBinding{"team-a": {"bob": {rb}}},
		RoleBindingsByNS: map[string][]*rbacv1.RoleBinding{"team-a": {rb}},
	}

	permitAll, ns := CohortNSACL(snap, "bob", nil, testGVR)
	if permitAll {
		t.Fatalf("RoleBinding-only path: permitAll=true, want false")
	}
	if _, ok := ns["team-a"]; !ok {
		t.Fatalf("permittedNS does not contain team-a: %v", ns)
	}
	if len(ns) != 1 {
		t.Fatalf("permittedNS = %v, want exactly {team-a}", ns)
	}
}

func TestCohortNSACL_ResourceNamesDoesNotGrantList(t *testing.T) {
	// A resourceNames-scoped rule must NOT grant list (collection verb).
	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "named"},
		Rules: []rbacv1.PolicyRule{{
			Verbs:         []string{"list", "get"},
			APIGroups:     []string{"composition.krateo.io"},
			Resources:     []string{"compositions"},
			ResourceNames: []string{"foo", "bar"},
		}},
	}
	crb := &rbacv1.ClusterRoleBinding{
		Subjects: []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: "alice"}},
		RoleRef:  rbacv1.RoleRef{Kind: "ClusterRole", Name: cr.Name},
	}
	snap := &RBACSnapshot{
		ClusterRolesByName: map[string]*rbacv1.ClusterRole{cr.Name: cr},
		CRBsByUser:         map[string][]*rbacv1.ClusterRoleBinding{"alice": {crb}},
	}

	permitAll, ns := CohortNSACL(snap, "alice", nil, testGVR)
	if permitAll || len(ns) != 0 {
		t.Fatalf("resourceNames-scoped CRB granted list: permitAll=%v ns=%v", permitAll, ns)
	}
}

func TestCohortNSACL_NoMatchingBinding(t *testing.T) {
	// Cohort has no binding referencing the gvr.
	snap := &RBACSnapshot{
		ClusterRolesByName: map[string]*rbacv1.ClusterRole{},
	}
	permitAll, ns := CohortNSACL(snap, "alice", []string{"devs"}, testGVR)
	if permitAll || len(ns) != 0 {
		t.Fatalf("empty snapshot: permitAll=%v ns=%v, want (false, empty)", permitAll, ns)
	}
}

func TestCohortNSACL_WildcardGroupAndResource(t *testing.T) {
	// Verbs ["*"], APIGroups ["*"], Resources ["*"] must match every gvr.
	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-admin"},
		Rules: []rbacv1.PolicyRule{{
			Verbs:     []string{"*"},
			APIGroups: []string{"*"},
			Resources: []string{"*"},
		}},
	}
	crb := &rbacv1.ClusterRoleBinding{
		Subjects: []rbacv1.Subject{{Kind: rbacv1.GroupKind, Name: "admins"}},
		RoleRef:  rbacv1.RoleRef{Kind: "ClusterRole", Name: cr.Name},
	}
	snap := &RBACSnapshot{
		ClusterRolesByName: map[string]*rbacv1.ClusterRole{cr.Name: cr},
		CRBsByGroup:        map[string][]*rbacv1.ClusterRoleBinding{"admins": {crb}},
	}

	for _, gvr := range []schema.GroupVersionResource{
		{Group: "composition.krateo.io", Version: "v1", Resource: "compositions"},
		{Group: "", Version: "v1", Resource: "configmaps"},
		{Group: "apps", Version: "v1", Resource: "deployments"},
	} {
		permitAll, _ := CohortNSACL(snap, "anyone", []string{"admins"}, gvr)
		if !permitAll {
			t.Fatalf("cluster-admin wildcard rule: permitAll=false on gvr=%v", gvr)
		}
	}
}

func TestCohortNSACL_RoleBindingWithClusterRoleRef(t *testing.T) {
	// RoleBinding can reference a ClusterRole — the grant is namespace-scoped
	// (the RB's namespace) per Kubernetes semantics.
	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "lister"},
		Rules: []rbacv1.PolicyRule{{
			Verbs:     []string{"list"},
			APIGroups: []string{"composition.krateo.io"},
			Resources: []string{"compositions"},
		}},
	}
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-b", Name: "bob-team-b"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: "bob"}},
		RoleRef:    rbacv1.RoleRef{Kind: "ClusterRole", Name: cr.Name},
	}
	snap := &RBACSnapshot{
		ClusterRolesByName: map[string]*rbacv1.ClusterRole{cr.Name: cr},
		RBsByUserByNS:      map[string]map[string][]*rbacv1.RoleBinding{"team-b": {"bob": {rb}}},
		RoleBindingsByNS:   map[string][]*rbacv1.RoleBinding{"team-b": {rb}},
	}

	permitAll, ns := CohortNSACL(snap, "bob", nil, testGVR)
	if permitAll {
		t.Fatalf("RB(ClusterRole-ref): permitAll=true, want false")
	}
	if _, ok := ns["team-b"]; !ok || len(ns) != 1 {
		t.Fatalf("permittedNS = %v, want {team-b}", ns)
	}
}
