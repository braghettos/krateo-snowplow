package evaltest

// Ship-1.1 ground-truth probe — reproduce the LIVE 0.30.202 divergence:
// EvaluateRBAC(snowplow-SA, list, "", namespaces, "") DENIES in-process
// despite the SA's real `*/*` get/list/watch CRB (apiserver can-i = yes).
//
// This is a NON-DESTRUCTIVE black-box probe under the evaltest harness
// (fake dynamic client + cache=on watcher) — it does NOT touch the remote
// kubeconfig (feedback_no_go_test_against_remote_kubeconfig). The CRB +
// ClusterRole wire shape is COPIED VERBATIM from the live cluster:
//   CRB snowplow-krateo-system -> ClusterRole snowplow-krateo-system
//     subject {Kind:ServiceAccount, Name:snowplow, Namespace:krateo-system}
//   ClusterRole rule {apiGroups:[*] resources:[*] verbs:[get list watch]}
//
// The probe asserts the SA-subject + User-subject + Group-subject verdicts
// for the EXACT failing tuple to PIN the mechanism and BOUND the blast
// radius (symmetry across subject Kinds).

import (
	"context"
	"testing"

	"github.com/krateoplatformops/snowplow/internal/rbac"
	rbacv1 "k8s.io/api/rbac/v1"
)

// the live SA */* ClusterRole, verbatim.
func snowplowStarRole() *rbacv1.ClusterRole {
	return clusterRole("snowplow-krateo-system",
		rule([]string{"*"}, []string{"*"}, []string{"get", "list", "watch"}),
	)
}

// PROBE 1 — the SA subject on the failing tuple. EXPECT permit (apiserver
// says yes). If this FAILS (deny) the SA-subject path is the bug.
func TestShip11Probe_SA_ListNamespaces_ClusterScope(t *testing.T) {
	newTestWatcher(t,
		snowplowStarRole(),
		clusterRoleBinding("snowplow-krateo-system", "snowplow-krateo-system",
			saSubject("krateo-system", "snowplow"),
		),
	)
	ok, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
		Username:  "system:serviceaccount:krateo-system:snowplow",
		Verb:      "list",
		Group:     "",
		Resource:  "namespaces",
		Namespace: "", // cluster-scoped namespaces object → item.GetNamespace()=="" (filterListByRBAC:147)
	})
	if err != nil {
		t.Fatalf("EvaluateRBAC: %v", err)
	}
	t.Logf("PROBE-1 SA list namespaces ns='' => permit=%v (apiserver=yes)", ok)
	if !ok {
		t.Errorf("FAIL-REPRODUCED: SA */* CRB denies list namespaces — the live divergence")
	}
}

// PROBE 1b — same SA, but per-item ns set to a concrete namespace name
// (the verb=list memo keys on item.GetNamespace(); a namespace LIST item's
// GetNamespace() is "" — but probe the concrete-ns variant too to see if
// the cluster-scoped ns="" handling is the discriminator).
func TestShip11Probe_SA_ListNamespaces_ConcreteNS(t *testing.T) {
	newTestWatcher(t,
		snowplowStarRole(),
		clusterRoleBinding("snowplow-krateo-system", "snowplow-krateo-system",
			saSubject("krateo-system", "snowplow"),
		),
	)
	ok, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
		Username:  "system:serviceaccount:krateo-system:snowplow",
		Verb:      "list",
		Group:     "",
		Resource:  "namespaces",
		Namespace: "demo-system",
	})
	if err != nil {
		t.Fatalf("EvaluateRBAC: %v", err)
	}
	t.Logf("PROBE-1b SA list namespaces ns='demo-system' => permit=%v", ok)
}

// PROBE 2 — SYMMETRY: a User subject on the SAME */* ClusterRole + same
// tuple. EXPECT permit. If User permits but SA denies, the bug is
// SA-subject-specific (blast radius bounded). If User ALSO denies, the
// wildcard/ns="" handling is broken for ALL kinds (wider blast radius).
func TestShip11Probe_User_ListNamespaces_ClusterScope(t *testing.T) {
	newTestWatcher(t,
		snowplowStarRole(),
		clusterRoleBinding("user-star", "snowplow-krateo-system",
			userSubject("alice"),
		),
	)
	ok, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
		Username:  "alice",
		Verb:      "list",
		Group:     "",
		Resource:  "namespaces",
		Namespace: "",
	})
	if err != nil {
		t.Fatalf("EvaluateRBAC: %v", err)
	}
	t.Logf("PROBE-2 User list namespaces ns='' => permit=%v", ok)
	if !ok {
		t.Errorf("User */* CRB also denies list namespaces — blast radius is NOT SA-only")
	}
}

// PROBE 3 — SYMMETRY: a Group subject (admin's cohort = group admins) on
// the SAME */* ClusterRole + same tuple. EXPECT permit (matches the live
// sentinel+[admins] keeps-62 observation). Confirms the group path is sound.
func TestShip11Probe_Group_ListNamespaces_ClusterScope(t *testing.T) {
	newTestWatcher(t,
		snowplowStarRole(),
		clusterRoleBinding("group-star", "snowplow-krateo-system",
			groupSubject("admins"),
		),
	)
	ok, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
		Username:  "system:cohort:group-only:v1",
		Groups:    []string{"admins", "system:authenticated"},
		Verb:      "list",
		Group:     "",
		Resource:  "namespaces",
		Namespace: "",
	})
	if err != nil {
		t.Fatalf("EvaluateRBAC: %v", err)
	}
	t.Logf("PROBE-3 Group[admins] list namespaces ns='' => permit=%v (live: sentinel+[admins] kept 62)", ok)
	if !ok {
		t.Errorf("Group */* CRB denies list namespaces — admin cohort path broken")
	}
}
