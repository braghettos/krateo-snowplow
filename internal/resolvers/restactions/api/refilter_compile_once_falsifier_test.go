// refilter_compile_once_falsifier_test.go — #121 1b C3 falsifier.
//
// 1b hoists the per-item NamespaceFrom gojq.Parse+Compile out of the
// refilterSlice loop (docs/boot-walk-deadline-rootcause-2026-07-09.md §4 +
// Option 1b). EvalValue pre-1b re-Parse+Compiled the SAME query on every item,
// so an N-item RBAC filter paid N compile cycles; at the 50K composition boot
// walk that was ~0.3s of pure recompile on the death-rattle path.
//
// The PM's C3 gate requires the falsifier to DISCRIMINATE — prove the compile
// happens ONCE per filter (compile-count == 1 across N items, NOT merely
// "faster"), AND prove refilterSlice output is BYTE-IDENTICAL pre/post (this is
// the RBAC/security boundary — 1b must be behaviour-neutral, no row added or
// dropped differently).
//
// RED arm (documented, verified by hand against the pre-1b tree): the pre-1b
// refilterSlice → evalSingle → evalJQString → EvalValue path compiled inside
// EvalValue per call, so JQCompileCountForTest() delta would be == N (one per
// item), FAILING the ==1 assertion. The compile-count seam (jqCompileCount in
// jqvalue.go) is what makes "once" observable rather than inferred from timing.

package api

import (
	"fmt"
	"reflect"
	"testing"

	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestRefilter1b_CompileOncePerFilter_C3 drives an N-item refilterSlice through
// ONE NamespaceFrom filter and asserts (a) exactly ONE CompileJQ call for the
// whole slice (the discriminating arm), and (b) the kept subset is exactly the
// RBAC-permitted namespaces — byte-identical to an independent recomputation of
// the expected keep-set (the security-boundary arm).
func TestRefilter1b_CompileOncePerFilter_C3(t *testing.T) {
	// Same narrow-RBAC fixture as TestApplyUserAccessFilter_DropsDeniedNamespaces:
	// "devs" get on compositions in bench-ns-01 ONLY.
	role := &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{Name: "ns01-comp-reader", Namespace: "bench-ns-01"},
		Rules: []rbacv1.PolicyRule{{
			Verbs:     []string{"get", "list", "watch"},
			APIGroups: []string{"composition.krateo.io"},
			Resources: []string{"compositions"},
		}},
	}
	binding := &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: "ns01-comp-reader-binding", Namespace: "bench-ns-01"},
		Subjects: []rbacv1.Subject{
			{Kind: "Group", APIGroup: "rbac.authorization.k8s.io", Name: "devs"},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "ns01-comp-reader",
		},
	}
	newRefilterTestWatcher(t, role, binding)

	// NamespaceFrom:".metadata.name" — the namespaces-list refilter shape
	// (namespace == the namespace object's own name).
	uaf := &templates.UserAccessFilterSpec{
		Verb:          "get",
		Group:         "composition.krateo.io",
		Resource:      "compositions",
		NamespaceFrom: ".metadata.name",
	}
	ctx := ctxWithUser("cyberjoker", "devs")

	// N > 1 by construction (the C3 gate + feedback_falsifier_shape_must_discriminate
	// require a multi-item slice so "compile once" is distinguishable from
	// "compile per item"). Mix the ONE permitted namespace among many denied
	// ones so the keep-set is a strict, non-trivial subset.
	const denied = 24
	items := make([]any, 0, denied+1)
	items = append(items, nsItem("bench-ns-01")) // the only permitted one
	for i := 0; i < denied; i++ {
		items = append(items, nsItem(deniedNS(i)))
	}

	// (a) DISCRIMINATING ARM — compile-count delta == 1 across all N items.
	before := JQCompileCountForTest()
	kept, dropped, calls := refilterSlice(ctx, discardLogger(),
		"cyberjoker", []string{"devs"}, uaf, []string{"compositions"}, items)
	delta := JQCompileCountForTest() - before

	if delta != 1 {
		t.Fatalf("CompileJQ delta = %d across %d items; want exactly 1 (compile-once-per-filter). "+
			"delta==N would be the pre-1b per-item recompile RED.", delta, len(items))
	}

	// (b) SECURITY-BOUNDARY ARM — the kept subset is EXACTLY the permitted set,
	// byte-identical to an independent expected list. 1b must not add/drop a
	// single row versus the RBAC decision.
	wantKept := []any{nsItem("bench-ns-01")}
	if !reflect.DeepEqual(kept, wantKept) {
		t.Fatalf("kept = %#v; want %#v (byte-identical RBAC-permitted subset)", kept, wantKept)
	}
	if dropped != denied {
		t.Errorf("dropped = %d; want %d", dropped, denied)
	}
	if calls != len(items) {
		t.Errorf("evaluate_rbac_calls = %d; want %d (one RBAC eval per item — unchanged by 1b)", calls, len(items))
	}
}

// nsItem builds the {"metadata":{"name":ns}} shape the namespaces-stage feeds
// refilterSlice (namespaceFrom:".metadata.name").
func nsItem(ns string) any {
	return map[string]any{"metadata": map[string]any{"name": ns}}
}

func deniedNS(i int) string {
	// Deterministic distinct denied names, none == bench-ns-01.
	return fmt.Sprintf("bench-ns-denied-%02d", i)
}
