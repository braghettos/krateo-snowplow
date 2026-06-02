// test_helpers_export.go — Ship 0.30.240.
//
// Exports a minimal set of test helpers so the T8 falsifier can live
// in the dispatchers package (where it can call the production v4
// serve gate gateWidgetsServeBytes / gateRestactionsServeBytes /
// gateRAFullListServeBytes without creating a cache → dispatchers
// import cycle).
//
// This file is intentionally NOT a _test.go file so the helpers are
// accessible to external test packages. Each exported helper is a
// thin wrapper around the cache-package internal-test helper of the
// same name (rbac_snapshot_test.go + rbac_cohort_gen_test.go); the
// production code path NEVER touches these — invocations of
// PublishRBACSnapshotForTest panic in the build if accidentally
// reached from a non-test path because rebuildSubjectIndexes panics
// without a snapshot scaffold.
//
// Why an export-test helpers file rather than a sub-package:
//
//   - The helpers are package-private (resetCohortGenMapForTest,
//     rebuildSubjectIndexes) and need cross-test access for the v4
//     T8 falsifier. A sub-package would force an export of these
//     too, widening the public-test surface beyond what we need.
//   - The pattern matches PublishRBACSnapshotForTest /
//     RBACSnapshotForTest already present in rbac_snapshot.go.

package cache

import (
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MkCRBForTest constructs a ClusterRoleBinding with one Subject. Test
// helper — production code must not call this.
//
// Ship 0.30.240: exported wrapper around the package-private mkCRB in
// rbac_snapshot_test.go (mkCRB lives in a _test.go and is therefore
// not visible to other test packages).
func MkCRBForTest(name string, sub rbacv1.Subject) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Subjects:   []rbacv1.Subject{sub},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: name + "-role"},
	}
}

// MkRBForTest constructs a namespaced RoleBinding with one Subject.
// Test helper — production code must not call this. Ship 0.30.240.
func MkRBForTest(ns, name string, sub rbacv1.Subject) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Subjects:   []rbacv1.Subject{sub},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: name + "-role"},
	}
}

// UserSubForTest constructs a User-kind Subject. Test helper. Ship 0.30.240.
func UserSubForTest(name string) rbacv1.Subject {
	return rbacv1.Subject{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: name}
}

// GroupSubForTest constructs a Group-kind Subject. Test helper. Ship 0.30.240.
func GroupSubForTest(name string) rbacv1.Subject {
	return rbacv1.Subject{Kind: rbacv1.GroupKind, APIGroup: "rbac.authorization.k8s.io", Name: name}
}

// MkCRForTest constructs a ClusterRole granting `*`/`*`/`*` (wildcard
// every-verb every-resource grant — equivalent to cluster-admin).
// Test helper. Ship 0.30.240.
func MkCRForTest(name string) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Rules:      []rbacv1.PolicyRule{{Verbs: []string{"*"}, APIGroups: []string{"*"}, Resources: []string{"*"}}},
	}
}

// MkRForTest constructs a namespaced Role granting `get` on configmaps.
// Test helper. Ship 0.30.240.
func MkRForTest(ns, name string) *rbacv1.Role {
	return &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Rules:      []rbacv1.PolicyRule{{Verbs: []string{"get"}, APIGroups: []string{""}, Resources: []string{"configmaps"}}},
	}
}

// ResetCohortGenMapForTest clears the package-level cohort generator
// map. Test helper — must be paired with a t.Cleanup that re-calls
// ResetCohortGenMapForTest + PublishRBACSnapshotForTest(nil). Ship
// 0.30.240 — exported wrapper around the package-private
// resetCohortGenMapForTest in rbac_cohort_gen.go.
func ResetCohortGenMapForTest() {
	resetCohortGenMapForTest()
}

// SetGlobalStubWatcherForTest installs a minimal *ResourceWatcher as
// the process-scoped global so rbac.UserCan / EvaluateRBAC pass their
// `cache.Global() != nil` gate in unit tests that bypass the dynamic-
// fake-driven NewSnapshotTestWatcher pattern. The stub watcher's
// Snapshot() method reads the package-level rbacSnap atomic.Pointer —
// the SAME source PublishRBACSnapshotForTest writes to — so a test
// that publishes a snapshot + sets the stub watcher gets a fully-
// functional EvaluateRBAC evaluation against that snapshot.
//
// Ship 0.30.240. Pair with t.Cleanup that calls
// SetGlobalStubWatcherForTest(nil) to restore the singleton.
//
// Production code MUST NOT call this; SetGlobal (watcher.go) is the
// production entrypoint with a fully-wired *ResourceWatcher.
func SetGlobalStubWatcherForTest() *ResourceWatcher {
	stub := &ResourceWatcher{}
	SetGlobal(stub)
	return stub
}

// ClearGlobalWatcherForTest clears the global ResourceWatcher.
// Pair with SetGlobalStubWatcherForTest via t.Cleanup. Ship 0.30.240.
func ClearGlobalWatcherForTest() {
	SetGlobal(nil)
}

// BuildAndPublishSnapshotForTest constructs and publishes a
// RBACSnapshot from the supplied CRBs + per-namespace RBs + per-name
// ClusterRoles + per-(ns,name) Roles. Convenience wrapper that calls
// rebuildSubjectIndexes + PublishRBACSnapshotForTest. Test helper.
// Ship 0.30.240.
func BuildAndPublishSnapshotForTest(
	crbs []*rbacv1.ClusterRoleBinding,
	rbs map[string][]*rbacv1.RoleBinding,
	crs map[string]*rbacv1.ClusterRole,
	rs map[string]*rbacv1.Role,
) *RBACSnapshot {
	if crs == nil {
		crs = map[string]*rbacv1.ClusterRole{}
	}
	if rs == nil {
		rs = map[string]*rbacv1.Role{}
	}
	snap := &RBACSnapshot{
		ClusterRoleBindings: crbs,
		RoleBindingsByNS:    rbs,
		ClusterRolesByName:  crs,
		RolesByNSName:       rs,
	}
	rebuildSubjectIndexes(snap)
	PublishRBACSnapshotForTest(snap)
	return snap
}
