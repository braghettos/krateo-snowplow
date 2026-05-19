// refilter_scalar_falsifier_test.go — Ship 0.30.111 pre-flight
// falsifier F3 (Part 1 fix) + the AC-7 permanent regression test.
//
// Team rule feedback_falsifier_first_before_ship: written BEFORE the
// refilter.go fix; F3-scalar MUST fail against the unfixed
// refilterSlice.
//
// THE BUG: the namespaces-stage userAccessFilter keeps
// `filter: "[.namespaces.items[] | .metadata.name]"`. jsonHandler
// applies that filter and stores its RESULT — a bare-string array
// ["ns-01","ns-02",…] — in dict["namespaces"]. applyUserAccessFilter →
// refilterSlice then does `item.(map[string]any)`, which FAILS for a
// bare string → conservative-deny → every namespace dropped → empty
// Compositions page for everyone. namespaceFrom:"." never runs.
//
//   F3-scalar — refilterSlice over a bare-string array with
//        namespaceFrom:"." and RBAC granting only ns-a/ns-b must keep
//        exactly [ns-a, ns-b], dropped==1, calls==3. FAILS today: all 3
//        dropped, 0 EvaluateRBAC calls.
//
//   F3-object — the existing object-array path (namespaceFrom:
//        ".metadata.name") still works — no-regression.
//
//   F3-deny — a namespaceFrom that errors on a scalar denies that item;
//        an unhandleable item type (int, nil) is denied.
//
// AC-7 (PM gate #113 amended): TestACR7_NamespacesStageUAFNarrows is the
// PERMANENT regression test that the namespaces-stage UAF actually
// narrows a name-string array to the RBAC-permitted names. F3-scalar IS
// that test — it is kept, not a throwaway.

package api

import (
	"log/slog"
	"os"
	"testing"

	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// scalarFalsifierLogger is a quiet logger for the falsifier tests.
func scalarFalsifierLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// scalarTestUAF is the namespaces-stage UAF stanza exactly as the
// shipped portal YAML declares it: verb:list, core group, namespaces,
// namespaceFrom:"." (the returned items ARE namespace name strings, so
// jq `.` on the string yields the namespace itself).
func scalarTestUAF() *templates.UserAccessFilterSpec {
	return &templates.UserAccessFilterSpec{
		Verb:          "list",
		Group:         "",
		Resource:      "namespaces",
		NamespaceFrom: ".",
	}
}

// seedNSListerRBAC builds Role+RoleBinding runtime.Objects granting the
// "devs" group `list namespaces` in each ns — the realistic narrow-RBAC
// shape for the namespaces-stage UAF.
func seedNSListerRBAC(namespaces ...string) []runtime.Object {
	var out []runtime.Object
	for _, ns := range namespaces {
		role := &rbacv1.Role{
			TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
			ObjectMeta: metav1.ObjectMeta{Name: "ns-lister", Namespace: ns},
			Rules: []rbacv1.PolicyRule{
				{Verbs: []string{"list"}, APIGroups: []string{""}, Resources: []string{"namespaces"}},
			},
		}
		binding := &rbacv1.RoleBinding{
			TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
			ObjectMeta: metav1.ObjectMeta{Name: "ns-lister-binding", Namespace: ns},
			Subjects: []rbacv1.Subject{
				{Kind: "Group", APIGroup: "rbac.authorization.k8s.io", Name: "devs"},
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "ns-lister",
			},
		}
		out = append(out, role, binding)
	}
	return out
}

// TestFalsifierF3_RefilterScalarItems / TestACR7_NamespacesStageUAFNarrows
// — F3-scalar AND the AC-7 permanent regression test. refilterSlice over
// a bare-string namespace array with the shipped namespaces-stage UAF
// keeps exactly the RBAC-permitted names.
//
// FAILS today: refilterSlice's `item.(map[string]any)` fails for every
// bare string → all 3 dropped, 0 EvaluateRBAC calls.
func TestFalsifierF3_RefilterScalarItems(t *testing.T) {
	// RBAC grants the "devs" group `list namespaces` in ns-a and ns-b
	// only — ns-c is denied.
	newRefilterTestWatcher(t, seedNSListerRBAC("ns-a", "ns-b")...)

	items := []any{"ns-a", "ns-b", "ns-c"}
	ctx := ctxWithUser("cyberjoker", "devs")

	kept, dropped, calls := refilterSlice(ctx, scalarFalsifierLogger(),
		"cyberjoker", []string{"devs"}, scalarTestUAF(), []string{"namespaces"}, items)

	if calls != 3 {
		t.Fatalf("F3-scalar: EvaluateRBAC calls = %d; want 3 (one per scalar item) "+
			"— scalar items never reached the evaluator", calls)
	}
	if dropped != 1 {
		t.Fatalf("F3-scalar: dropped = %d; want 1 (ns-c only)", dropped)
	}
	if len(kept) != 2 {
		t.Fatalf("F3-scalar: kept = %d items; want 2 ([ns-a, ns-b]) — got %v", len(kept), kept)
	}
	keptSet := map[string]bool{}
	for _, k := range kept {
		s, ok := k.(string)
		if !ok {
			t.Fatalf("F3-scalar: kept item %v is not a string — refilterSlice mangled the scalar", k)
		}
		keptSet[s] = true
	}
	if !keptSet["ns-a"] || !keptSet["ns-b"] {
		t.Fatalf("F3-scalar: kept set = %v; want exactly {ns-a, ns-b}", keptSet)
	}
	if keptSet["ns-c"] {
		t.Fatalf("F3-scalar: LEAK — ns-c kept; cyberjoker has no `list namespaces` grant there")
	}
}

// TestFalsifierF3_RefilterObjectItems is the no-regression leg: the
// existing object-array path (namespaceFrom: ".metadata.name") still
// keeps exactly the RBAC-permitted objects.
func TestFalsifierF3_RefilterObjectItems(t *testing.T) {
	newRefilterTestWatcher(t, seedNSListerRBAC("ns-a", "ns-b")...)

	items := []any{
		map[string]any{"metadata": map[string]any{"name": "ns-a"}},
		map[string]any{"metadata": map[string]any{"name": "ns-b"}},
		map[string]any{"metadata": map[string]any{"name": "ns-c"}},
	}
	uaf := &templates.UserAccessFilterSpec{
		Verb: "list", Group: "", Resource: "namespaces", NamespaceFrom: ".metadata.name",
	}
	ctx := ctxWithUser("cyberjoker", "devs")

	kept, dropped, calls := refilterSlice(ctx, scalarFalsifierLogger(),
		"cyberjoker", []string{"devs"}, uaf, []string{"namespaces"}, items)

	if calls != 3 {
		t.Fatalf("F3-object: calls = %d; want 3", calls)
	}
	if dropped != 1 || len(kept) != 2 {
		t.Fatalf("F3-object: object-array path regressed — kept=%d dropped=%d want kept=2 dropped=1",
			len(kept), dropped)
	}
}

// TestFalsifierF3_RefilterDenyPaths covers the fail-closed legs:
//   - a namespaceFrom expression that errors on a scalar → that scalar
//     is denied;
//   - an unhandleable item type (int) → denied.
func TestFalsifierF3_RefilterDenyPaths(t *testing.T) {
	newRefilterTestWatcher(t) // no RBAC seeded — every eval would deny anyway

	ctx := ctxWithUser("cyberjoker", "devs")
	log := scalarFalsifierLogger()

	// Leg 1 — namespaceFrom that errors on a scalar: `.metadata.name`
	// on a bare string is a jq type error → that item is denied.
	badUAF := &templates.UserAccessFilterSpec{
		Verb: "list", Group: "", Resource: "namespaces", NamespaceFrom: ".metadata.name",
	}
	kept, dropped, _ := refilterSlice(ctx, log, "cyberjoker", []string{"devs"},
		badUAF, []string{"namespaces"}, []any{"ns-a"})
	if len(kept) != 0 || dropped != 1 {
		t.Fatalf("F3-deny: a namespaceFrom error on a scalar must deny; got kept=%d dropped=%d",
			len(kept), dropped)
	}

	// Leg 2 — an unhandleable item type (int, nil) → conservative-deny.
	kept2, dropped2, _ := refilterSlice(ctx, log, "cyberjoker", []string{"devs"},
		scalarTestUAF(), []string{"namespaces"}, []any{42, nil})
	if len(kept2) != 0 || dropped2 != 2 {
		t.Fatalf("F3-deny: unhandleable item types must be conservative-denied; got kept=%d dropped=%d",
			len(kept2), dropped2)
	}
}
