// rbac_memo_falsifier_test.go — Ship 0.30.111 pre-flight falsifier F1.
//
// Team rule feedback_falsifier_first_before_ship: written BEFORE the
// production fix; MUST fail against the unfixed filterListByRBAC.
//
//   F1 — namespace-keyed EvaluateRBAC memo. A filterListByRBAC pass over
//        a LIST spanning N distinct namespaces (with multiple items per
//        namespace) must make exactly N EvaluateRBAC calls — the
//        verb:"list" decision is namespace-scoped and name-independent,
//        so two items in the same namespace need ONE evaluation.
//
//        FAILS today: filterListByRBAC calls EvaluateRBAC once PER ITEM
//        (passing Name: it.GetName()), so the count is N_items, not N.
//
// Plus the AC-6 name-independence leg: two items in the SAME namespace
// with DIFFERENT names yield ONE EvaluateRBAC call and an identical
// verdict.

package api

import (
	"testing"

	"github.com/krateoplatformops/snowplow/internal/rbac"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// memoItem builds a minimal *unstructured.Unstructured carrying just the
// (namespace, name) identity filterListByRBAC's per-item loop reads.
func memoItem(ns, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetNamespace(ns)
	u.SetName(name)
	u.SetUnstructuredContent(map[string]any{
		"apiVersion": "templates.krateo.io/v1",
		"kind":       "RestAction",
		"metadata": map[string]any{
			"namespace": ns,
			"name":      name,
		},
	})
	_ = metav1.ObjectMeta{}
	return u
}

// TestFalsifierF1_RBACMemoIsNamespaceKeyed asserts a filterListByRBAC
// pass over N distinct namespaces × M items each makes exactly N
// EvaluateRBAC calls.
//
// FAILS on the unfixed code: per-item evaluation → N*M calls.
func TestFalsifierF1_RBACMemoIsNamespaceKeyed(t *testing.T) {
	_, _ = newRBACNarrowWatcher(t) // wires a cache=on watcher + RBAC store

	const itemsPerNS = 3
	namespaces := []string{"team-a", "team-b", "bench-ns-1", "bench-ns-2", "kube-system"}
	wantCalls := uint64(len(namespaces)) // one EvaluateRBAC per namespace

	var items []*unstructured.Unstructured
	for _, ns := range namespaces {
		for i := 0; i < itemsPerNS; i++ {
			items = append(items, memoItem(ns, ns+"-item-"+itoaMemo(i)))
		}
	}

	ctx := ctxWithUser(narrowUser)
	rbac.ResetEvaluateRBACCallCount()

	_, served := filterListByRBAC(ctx, dispatchTestGVR, items)
	if !served {
		t.Fatalf("F1: filterListByRBAC served=false (expected an identity-present pass)")
	}

	got := rbac.EvaluateRBACCallCount()
	if got != wantCalls {
		t.Fatalf("F1: filterListByRBAC over %d namespaces × %d items made %d EvaluateRBAC calls; "+
			"want exactly %d (one per namespace) — the per-item evaluation is not namespace-memoized",
			len(namespaces), itemsPerNS, got, wantCalls)
	}
}

// TestFalsifierF1_RBACMemoNameIndependence is the AC-6 name-independence
// leg: two items in the SAME namespace with DIFFERENT names produce ONE
// EvaluateRBAC call and the same keep/drop verdict for both.
func TestFalsifierF1_RBACMemoNameIndependence(t *testing.T) {
	_, authorized := newRBACNarrowWatcher(t)
	_ = authorized

	// team-a is an authorized namespace — both items must be kept.
	items := []*unstructured.Unstructured{
		memoItem("team-a", "alpha"),
		memoItem("team-a", "omega"),
	}

	ctx := ctxWithUser(narrowUser)
	rbac.ResetEvaluateRBACCallCount()

	kept, served := filterListByRBAC(ctx, dispatchTestGVR, items)
	if !served {
		t.Fatalf("F1-name: filterListByRBAC served=false")
	}
	if got := rbac.EvaluateRBACCallCount(); got != 1 {
		t.Fatalf("F1-name: two same-namespace items made %d EvaluateRBAC calls; want 1 "+
			"(verb:list is name-independent — one namespace, one evaluation)", got)
	}
	if len(kept) != 2 {
		t.Fatalf("F1-name: name-independence broken — %d/2 items kept; the verdict must be "+
			"identical for both same-namespace items", len(kept))
	}
}

// itoaMemo is a tiny int→string helper local to this file.
func itoaMemo(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
