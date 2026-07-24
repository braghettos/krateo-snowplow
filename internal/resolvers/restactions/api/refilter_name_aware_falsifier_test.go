// refilter_name_aware_falsifier_test.go — #123 falsifier: the UAF refilter
// serve path must be NAME-aware so a resourceNames-scoped RBAC grant
// (a Role/ClusterRole rule with a non-empty resourceNames, valid only for
// name-specific verbs — get/update/patch/delete) is honoured.
//
// Root cause (#123): pre-fix the refilter built EvaluateOptions WITHOUT the
// object's Name (refilter.go evalSingle passed no Name → ""). The evaluator
// core (internal/rbac/evaluate.go resourceNameMatches) requires opts.Name ∈
// rule.ResourceNames for a resourceNames-scoped rule; with Name=="" every
// resourceNames-scoped grant matches NOTHING → all named objects are silently
// dropped (UNDER-serve / fail-closed). The fix derives the per-object name via
// the additive UAFSpec.NameFrom JQ (default ".metadata.name") and threads it
// into EvaluateOptions.Name — symmetric with the existing NamespaceFrom path.
//
// Arms:
//   - GREEN: a resourceNames-scoped get grant + K>1 distinct-name items → the
//     refilter returns EXACTLY the named object(s), drops the rest.
//   - RED: the pre-fix posture (Name unresolved → "") reproduced by pointing
//     NameFrom at a field the objects don't carry (yields null → "") → the
//     named-object grant drops ALL items (kept:0), proving the Name plumbing
//     is what honours the grant.
//   - INERTNESS: a plain LIST grant (a collection verb, which the evaluator
//     scopes resourceNames AWAY from) still returns all items — Name threading
//     must not change collection-verb behaviour (guards against over-narrowing).

package api

import (
	"testing"

	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// resourceNamesReaderRBAC builds a namespace-scoped Role + RoleBinding that
// grants the "devs" group get/watch on a SINGLE named object of the given
// resource (resourceNames: [name]). cyberjoker is a "devs" member. This is
// the production-shaped narrow grant that #123 was silently dropping.
func resourceNamesReaderRBAC(group, resource, ns, name string) (*rbacv1.Role, *rbacv1.RoleBinding) {
	role := &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{Name: "named-reader", Namespace: ns},
		Rules: []rbacv1.PolicyRule{
			{
				Verbs:         []string{"get", "watch"},
				APIGroups:     []string{group},
				Resources:     []string{resource},
				ResourceNames: []string{name}, // <-- the scoped grant
			},
		},
	}
	binding := &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: "named-reader-binding", Namespace: ns},
		Subjects: []rbacv1.Subject{
			{Kind: "Group", APIGroup: "rbac.authorization.k8s.io", Name: "devs"},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "named-reader",
		},
	}
	return role, binding
}

// nameAwareItems returns K>1 distinct-named objects in the given namespace,
// one of which is the granted name. Object shape carries .metadata.name +
// .metadata.namespace so the default NameFrom (".metadata.name") and
// NamespaceFrom (".metadata.namespace") resolve.
func nameAwareItems(ns string, names ...string) []any {
	out := make([]any, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]any{
			"metadata": map[string]any{"name": n, "namespace": ns},
		})
	}
	return out
}

// TestRefilterNameAware_GREEN — a resourceNames-scoped GET grant keeps EXACTLY
// the named object and drops the rest. K=3 distinct names, grant scoped to one.
func TestRefilterNameAware_GREEN(t *testing.T) {
	const ns = "bench-ns-01"
	role, binding := resourceNamesReaderRBAC("apps", "deployments", ns, "selfservice-krateo")
	newRefilterTestWatcher(t, role, binding)

	apiCall := &templates.API{
		Name: "deployments",
		UserAccessFilter: &templates.UserAccessFilterSpec{
			Verb:     "get",
			Group:    "apps",
			Resource: "deployments",
			// NameFrom + NamespaceFrom omitted → CRD/in-code defaults
			// (.metadata.name / .metadata.namespace) fire.
		},
	}
	dict := map[string]any{
		"deployments": map[string]any{
			"kind":       "DeploymentList",
			"apiVersion": "apps/v1",
			"items":      nameAwareItems(ns, "selfservice-krateo", "other-a", "other-b"),
		},
	}

	res := applyUserAccessFilter(ctxWithUser("cyberjoker", "devs"), dict, apiCall)

	if res.Kept != 1 {
		t.Errorf("GREEN: kept = %d; want 1 (only the resourceNames-granted object)", res.Kept)
	}
	if res.Dropped != 2 {
		t.Errorf("GREEN: dropped = %d; want 2", res.Dropped)
	}
	if res.EvaluateRBACCalls != 3 {
		t.Errorf("GREEN: evaluate_rbac_calls = %d; want 3 (one per object)", res.EvaluateRBACCalls)
	}
	items := dict["deployments"].(map[string]any)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("GREEN: retained items = %d; want 1", len(items))
	}
	got := items[0].(map[string]any)["metadata"].(map[string]any)["name"]
	if got != "selfservice-krateo" {
		t.Errorf("GREEN: retained name = %v; want selfservice-krateo", got)
	}
}

// TestRefilterNameAware_RED — reproduces the PRE-FIX posture: when the derived
// Name is empty (NameFrom points at a field the objects don't carry → jq
// yields null → ""), the resourceNames-scoped grant matches NOTHING and the
// refilter drops ALL items. This is the exact #123 symptom (under-serve /
// fail-closed) and proves the Name plumbing is load-bearing: the ONLY
// difference vs the GREEN arm is that Name resolves to "" instead of the
// object's real name.
//
// If a future refactor drops the Name from EvaluateOptions, the GREEN arm
// collapses to THIS behaviour for every object → GREEN turns red, catching the
// regression.
func TestRefilterNameAware_RED_EmptyNameDropsGrantedObject(t *testing.T) {
	const ns = "bench-ns-01"
	role, binding := resourceNamesReaderRBAC("apps", "deployments", ns, "selfservice-krateo")
	newRefilterTestWatcher(t, role, binding)

	apiCall := &templates.API{
		Name: "deployments",
		UserAccessFilter: &templates.UserAccessFilterSpec{
			Verb:     "get",
			Group:    "apps",
			Resource: "deployments",
			// Force the pre-fix Name="" posture: a path the objects don't
			// carry yields null → "" (identical to no-Name-plumbed).
			NameFrom: ".metadata.thisFieldDoesNotExist",
		},
	}
	dict := map[string]any{
		"deployments": map[string]any{
			"kind":       "DeploymentList",
			"apiVersion": "apps/v1",
			"items":      nameAwareItems(ns, "selfservice-krateo", "other-a", "other-b"),
		},
	}

	res := applyUserAccessFilter(ctxWithUser("cyberjoker", "devs"), dict, apiCall)

	if res.Kept != 0 {
		t.Errorf("RED: kept = %d; want 0 — an empty derived Name must drop even the granted object (the #123 defect)", res.Kept)
	}
	if res.Dropped != 3 {
		t.Errorf("RED: dropped = %d; want 3 (all items dropped)", res.Dropped)
	}
}

// TestRefilterNameAware_INERTNESS_ListGrantUnaffected — a plain LIST grant
// (no resourceNames; collection verb) must return ALL items regardless of the
// threaded Name. The evaluator scopes resourceNames to name-specific verbs
// only, so threading a name into a list-verb check is inert. This guards
// against Name threading over-narrowing collection-verb behaviour.
func TestRefilterNameAware_INERTNESS_ListGrantUnaffected(t *testing.T) {
	const ns = "bench-ns-01"
	// UNSCOPED list grant: no resourceNames → matches every object.
	role := &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{Name: "list-reader", Namespace: ns},
		Rules: []rbacv1.PolicyRule{
			{Verbs: []string{"list"}, APIGroups: []string{"apps"}, Resources: []string{"deployments"}},
		},
	}
	binding := &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: "list-reader-binding", Namespace: ns},
		Subjects:   []rbacv1.Subject{{Kind: "Group", APIGroup: "rbac.authorization.k8s.io", Name: "devs"}},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "list-reader"},
	}
	newRefilterTestWatcher(t, role, binding)

	apiCall := &templates.API{
		Name: "deployments",
		UserAccessFilter: &templates.UserAccessFilterSpec{
			Verb:     "list", // collection verb — resourceNames does not scope it
			Group:    "apps",
			Resource: "deployments",
		},
	}
	dict := map[string]any{
		"deployments": map[string]any{
			"kind":       "DeploymentList",
			"apiVersion": "apps/v1",
			"items":      nameAwareItems(ns, "d-a", "d-b", "d-c"),
		},
	}

	res := applyUserAccessFilter(ctxWithUser("cyberjoker", "devs"), dict, apiCall)

	if res.Kept != 3 {
		t.Errorf("INERTNESS: kept = %d; want 3 — a plain list grant must return all items (Name threading is inert for collection verbs)", res.Kept)
	}
	if res.Dropped != 0 {
		t.Errorf("INERTNESS: dropped = %d; want 0", res.Dropped)
	}
}
