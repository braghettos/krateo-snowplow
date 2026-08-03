// match_subject_test.go — Ship 0.30.242 H.c-layered Phase 3.
//
// Unit tests for the SOT primitives in match_subject.go:
//
//   - BindingUIDFromCRB
//   - BindingUIDFromRB
//   - pickRepresentativeFromSubjects
//
// Coverage axes:
//
//   (1) Happy path — apiserver-stamped metadata.uid present → "C:<uid>" /
//       "R:<ns>/<uid>" canonical shape.
//
//   (2) Empty-UID fallback — synthetic / test-fixture bindings (and
//       theoretically any apiserver pre-stamp gap) → content-tuple shape
//       "C:fallback/<name>\x1f<group>/<kind>/<role>" / R: analogue.
//       Cross-binding-distinct (two empty-UID CRBs with different RoleRef
//       MUST hash-distinct under BindingUIDFromCRB).
//
//   (3) Nil-pointer defensive — returns "" rather than panic.
//
//   (4) Cross-Kind isolation — a CRB and an RB with IDENTICAL UID
//       produce DIFFERENT BindingUIDs ("C:" vs "R:<ns>/" prefix).
//       Defends against the prefix-collision class of bug.
//
//   (5) Cross-namespace RB isolation — two RBs in DIFFERENT namespaces
//       with the same UID produce DIFFERENT BindingUIDs (defensive;
//       apiserver doesn't reuse UIDs across namespaces but the prefix
//       shape carries scope information into the identifier directly).
//
// pickRepresentativeFromSubjects coverage:
//
//   - User-kind:        {Username: <name>}
//   - Group-kind:       {Groups: [<name>]} (Username empty — the group-
//                        only representative semantic agreed at 2a R4).
//   - SA-kind:          {Username: "system:serviceaccount:<ns>:<name>"}
//   - Empty subjects:   zero-value SubjectIdentity.
//   - First-match wins: a multi-subject binding returns the first
//                        non-empty subject's representative.
//   - Skip empty-Name subjects.
//   - Skip SA-kind missing Namespace.

package cache

import (
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ──────────────────────────────────────────────────────────────────────
// BindingUIDFromCRB
// ──────────────────────────────────────────────────────────────────────

func TestBindingUIDFromCRB_HappyPath(t *testing.T) {
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "admin-bind",
			UID:  types.UID("crb-uid-001"),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "admin",
		},
	}
	got := BindingUIDFromCRB(crb)
	want := "C:crb-uid-001"
	if got != want {
		t.Fatalf("BindingUIDFromCRB happy path: got %q, want %q", got, want)
	}
}

func TestBindingUIDFromCRB_EmptyUIDFallback(t *testing.T) {
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "synthetic-admin",
			// UID intentionally empty.
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "admin",
		},
	}
	got := BindingUIDFromCRB(crb)
	want := "C:fallback/synthetic-admin\x1frbac.authorization.k8s.io/ClusterRole/admin"
	if got != want {
		t.Fatalf("BindingUIDFromCRB empty-UID fallback: got %q, want %q", got, want)
	}
}

// Defends against the empty-UID-collision class of bug. Two synthetic
// CRBs with the same Name MUST NOT collide if their RoleRef differs.
func TestBindingUIDFromCRB_EmptyUIDDistinctOnRoleRef(t *testing.T) {
	crbA := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "same-name"},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "role-A"},
	}
	crbB := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "same-name"},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "role-B"},
	}
	if BindingUIDFromCRB(crbA) == BindingUIDFromCRB(crbB) {
		t.Fatalf("two empty-UID CRBs with different RoleRef MUST hash-distinct; both produced %q", BindingUIDFromCRB(crbA))
	}
}

func TestBindingUIDFromCRB_NilDefensive(t *testing.T) {
	if got := BindingUIDFromCRB(nil); got != "" {
		t.Fatalf("BindingUIDFromCRB(nil) MUST return \"\"; got %q", got)
	}
}

// ──────────────────────────────────────────────────────────────────────
// BindingUIDFromRB
// ──────────────────────────────────────────────────────────────────────

func TestBindingUIDFromRB_HappyPath(t *testing.T) {
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "demo-system",
			Name:      "devs-bind",
			UID:       types.UID("rb-uid-001"),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     "compositions-reader",
		},
	}
	got := BindingUIDFromRB(rb)
	want := "R:demo-system/rb-uid-001"
	if got != want {
		t.Fatalf("BindingUIDFromRB happy path: got %q, want %q", got, want)
	}
}

func TestBindingUIDFromRB_EmptyUIDFallback(t *testing.T) {
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "demo-system",
			Name:      "synthetic-devs",
			// UID intentionally empty.
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     "compositions-reader",
		},
	}
	got := BindingUIDFromRB(rb)
	want := "R:demo-system/fallback/synthetic-devs\x1frbac.authorization.k8s.io/Role/compositions-reader"
	if got != want {
		t.Fatalf("BindingUIDFromRB empty-UID fallback: got %q, want %q", got, want)
	}
}

// Two RBs in DIFFERENT namespaces with the same UID MUST produce
// DIFFERENT BindingUIDs (defensive — apiserver doesn't reuse UIDs
// across namespaces but the prefix shape carries scope explicitly).
func TestBindingUIDFromRB_CrossNamespaceIsolation(t *testing.T) {
	rbA := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns-A", Name: "same", UID: types.UID("same-uid")},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "reader"},
	}
	rbB := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns-B", Name: "same", UID: types.UID("same-uid")},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "reader"},
	}
	if BindingUIDFromRB(rbA) == BindingUIDFromRB(rbB) {
		t.Fatalf("RBs in different namespaces with same UID MUST hash-distinct; both produced %q", BindingUIDFromRB(rbA))
	}
}

func TestBindingUIDFromRB_NilDefensive(t *testing.T) {
	if got := BindingUIDFromRB(nil); got != "" {
		t.Fatalf("BindingUIDFromRB(nil) MUST return \"\"; got %q", got)
	}
}

// ──────────────────────────────────────────────────────────────────────
// Cross-Kind isolation — CRB and RB UIDs MUST NEVER alias
// ──────────────────────────────────────────────────────────────────────

// A CRB and an RB with IDENTICAL `metadata.uid` MUST produce DIFFERENT
// BindingUIDs because the "C:" vs "R:<ns>/" prefix scope-separates the
// two kinds. Defends against the prefix-collision class of bug.
func TestBindingUID_CRBvsRB_NeverAlias(t *testing.T) {
	uid := types.UID("identical-uid-deadbeef")
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "crb-name", UID: uid},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "any"},
	}
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns-x", Name: "rb-name", UID: uid},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "any"},
	}
	if BindingUIDFromCRB(crb) == BindingUIDFromRB(rb) {
		t.Fatalf("CRB+RB with identical UID %q MUST NOT alias; both produced %q",
			uid, BindingUIDFromCRB(crb))
	}
}

// ──────────────────────────────────────────────────────────────────────
// pickRepresentativeFromSubjects
// ──────────────────────────────────────────────────────────────────────

func TestPickRepresentativeFromSubjects_UserKind(t *testing.T) {
	subjects := []rbacv1.Subject{
		{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: "alice"},
	}
	got := pickRepresentativeFromSubjects(subjects)
	if got.Username != "alice" {
		t.Fatalf("User-kind: got Username=%q, want %q", got.Username, "alice")
	}
	if len(got.Groups) != 0 {
		t.Fatalf("User-kind: Groups MUST be empty; got %v", got.Groups)
	}
}

func TestPickRepresentativeFromSubjects_GroupKind(t *testing.T) {
	subjects := []rbacv1.Subject{
		{Kind: "Group", APIGroup: "rbac.authorization.k8s.io", Name: "devs"},
	}
	got := pickRepresentativeFromSubjects(subjects)
	if got.Username != "" {
		t.Fatalf("Group-kind: Username MUST be empty; got %q", got.Username)
	}
	if len(got.Groups) != 1 || got.Groups[0] != "devs" {
		t.Fatalf("Group-kind: got Groups=%v, want [devs]", got.Groups)
	}
}

func TestPickRepresentativeFromSubjects_SAKind(t *testing.T) {
	subjects := []rbacv1.Subject{
		{Kind: "ServiceAccount", Namespace: "krateo-system", Name: "snowplow"},
	}
	got := pickRepresentativeFromSubjects(subjects)
	want := "system:serviceaccount:krateo-system:snowplow"
	if got.Username != want {
		t.Fatalf("SA-kind: got Username=%q, want %q", got.Username, want)
	}
	if len(got.Groups) != 0 {
		t.Fatalf("SA-kind: Groups MUST be empty; got %v", got.Groups)
	}
}

func TestPickRepresentativeFromSubjects_EmptySubjects(t *testing.T) {
	got := pickRepresentativeFromSubjects(nil)
	if got.Username != "" {
		t.Fatalf("empty subjects: Username MUST be empty; got %q", got.Username)
	}
	if got.Groups != nil {
		t.Fatalf("empty subjects: Groups MUST be nil (zero-value SubjectIdentity); got %v", got.Groups)
	}
}

// First-match wins: a multi-subject binding (e.g., a CRB with both
// User and Group subjects) returns the FIRST non-empty subject's
// representative. The cache cell is keyed by the BINDING's UID, not
// by which subject won — the representative is consumed only at
// prewarm-dispatch time.
func TestPickRepresentativeFromSubjects_FirstMatchWins(t *testing.T) {
	subjects := []rbacv1.Subject{
		{Kind: "Group", APIGroup: "rbac.authorization.k8s.io", Name: "devs"},
		{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: "alice"},
	}
	got := pickRepresentativeFromSubjects(subjects)
	// First non-empty subject is Group-kind → group-only representative.
	if got.Username != "" || len(got.Groups) != 1 || got.Groups[0] != "devs" {
		t.Fatalf("first-match wins: expected group-only [devs] (first subject); got %+v", got)
	}
}

// A subject with empty Name MUST NOT be returned as a representative;
// the loop SKIPS such subjects and continues to the next.
func TestPickRepresentativeFromSubjects_SkipsEmptyNames(t *testing.T) {
	subjects := []rbacv1.Subject{
		{Kind: "User", Name: ""}, // skip
		{Kind: "Group", Name: "devs"},
	}
	got := pickRepresentativeFromSubjects(subjects)
	if got.Username != "" || len(got.Groups) != 1 || got.Groups[0] != "devs" {
		t.Fatalf("skip-empty-Name: expected group-only [devs]; got %+v", got)
	}
}

// An SA-kind subject with empty Namespace MUST NOT produce a
// SubjectIdentity (the canonical "system:serviceaccount:<ns>:<name>"
// form requires both). The loop SKIPS and continues.
func TestPickRepresentativeFromSubjects_SkipsSAMissingNamespace(t *testing.T) {
	subjects := []rbacv1.Subject{
		{Kind: "ServiceAccount", Name: "snowplow"}, // no Namespace → skip
		{Kind: "User", Name: "alice"},
	}
	got := pickRepresentativeFromSubjects(subjects)
	if got.Username != "alice" {
		t.Fatalf("skip-SA-no-namespace: expected User alice (second subject); got %+v", got)
	}
}
