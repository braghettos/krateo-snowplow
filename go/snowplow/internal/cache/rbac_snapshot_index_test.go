// rbac_snapshot_index_test.go — Ship 0.30.169 falsifier-first correctness gate
// for the RBAC subject→bindings indexes (CRBsByUser/Group/SA + CRBsCatchAll
// + RBs*ByNS).
//
// This file is the LOAD-BEARING falsifier (HG-169-2). It must:
//   - FAIL to compile against pre-fix code (selectCRBCandidates undefined,
//     CRBsByUser et al. undefined) — captured pre-code in
//     /tmp/snowplow-runs/0.30.169/pre-code-falsifier-failure.txt.
//   - PASS post-fix with bit-equal pointer-set equivalence between the
//     linear scan ∩ anySubjectMatches and the index lookup ∩ anySubjectMatches
//     for every test subject.
//
// Lives in package `cache` because the snapshot type, indexes and the
// index-build run here. The post-lookup matcher `anySubjectMatches` lives
// in `internal/rbac/evaluate.go`; this test re-implements the *intent* of
// the matcher inline (a minimal-surface reference that hits the exact same
// case branches: User exact, Group exact + system:authenticated +
// system:serviceaccounts*, ServiceAccount ns/name exact). The reference
// is intentionally NOT a syscall into the rbac package — keeping the test
// self-contained avoids import cycles (rbac→cache, not the other way).
//
// Design anchor: docs/ship-0.30.169-rbac-subject-index-2026-05-22.md §3.
package cache

import (
	"strconv"
	"sync"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ─────────────────────────────────────────────────────────────────────
// Test fixtures — Subject factories matching architect §3 case-walk.
// ─────────────────────────────────────────────────────────────────────

func groupSub(name string) rbacv1.Subject {
	return rbacv1.Subject{Kind: rbacv1.GroupKind, APIGroup: "rbac.authorization.k8s.io", Name: name}
}

func saSub(ns, name string) rbacv1.Subject {
	return rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Namespace: ns, Name: name}
}

func unknownKindSub(name string) rbacv1.Subject {
	// Kind that anySubjectMatches' switch does not recognise — index
	// MUST land it in CRBsCatchAll so the post-lookup matcher sees it.
	return rbacv1.Subject{Kind: "FutureKind", Name: name}
}

// crbWithSubjects is a CRB factory with arbitrary Subjects (the
// existing mkCRB takes a single Subject). Uses a unique name suffix from
// the caller so the test snapshot can carry thousands without collisions.
func crbWithSubjects(name string, subs ...rbacv1.Subject) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Subjects:   subs,
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: name + "-role"},
	}
}

func rbWithSubjects(ns, name string, subs ...rbacv1.Subject) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Subjects:   subs,
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: name + "-role"},
	}
}

// ─────────────────────────────────────────────────────────────────────
// Reference matcher — mirrors evaluate.go:402-431 anySubjectMatches and
// evaluate.go:451-462 effectiveGroups. Self-contained to avoid an
// internal/rbac import cycle (rbac imports cache, not the other way).
// ─────────────────────────────────────────────────────────────────────

// testOpts captures the same fields rbac.EvaluateOptions does for the four
// switch branches anySubjectMatches walks (Username, Groups; SA path is
// derived from Username's canonical "system:serviceaccount:<ns>:<name>"
// prefix). Mirrors evaluate.go but lives in the cache test file.
type testOpts struct {
	Username string
	Groups   []string
}

func parseSAUsername(u string) (ns, name string, ok bool) {
	const prefix = "system:serviceaccount:"
	if len(u) <= len(prefix) || u[:len(prefix)] != prefix {
		return "", "", false
	}
	rest := u[len(prefix):]
	// SplitN(":", 2) without importing strings — keep this minimal.
	for i := 0; i < len(rest); i++ {
		if rest[i] == ':' {
			ns, name = rest[:i], rest[i+1:]
			if ns == "" || name == "" {
				return "", "", false
			}
			return ns, name, true
		}
	}
	return "", "", false
}

func referenceEffectiveGroups(opts testOpts) []string {
	saNS, _, isSA := parseSAUsername(opts.Username)
	if !isSA {
		return opts.Groups
	}
	g := make([]string, 0, len(opts.Groups)+2)
	g = append(g, opts.Groups...)
	g = append(g, "system:serviceaccounts", "system:serviceaccounts:"+saNS)
	return g
}

// referenceAnySubjectMatches mirrors anySubjectMatches' exact switch
// branches. The cache-internal `selectCRBCandidates` MUST produce a
// candidate set whose intersection with this matcher equals the full
// linear-scan intersection.
func referenceAnySubjectMatches(subjects []rbacv1.Subject, opts testOpts) bool {
	saNS, saName, isSA := parseSAUsername(opts.Username)
	groups := referenceEffectiveGroups(opts)
	for _, s := range subjects {
		switch s.Kind {
		case rbacv1.UserKind:
			if s.Name == opts.Username {
				return true
			}
		case rbacv1.GroupKind:
			for _, g := range groups {
				if s.Name == g {
					return true
				}
			}
			if s.Name == "system:authenticated" && opts.Username != "" {
				return true
			}
		case rbacv1.ServiceAccountKind:
			if isSA && s.Namespace == saNS && s.Name == saName {
				return true
			}
		}
	}
	return false
}

// linearScanMatch — current path: walk every CRB, post-filter with
// referenceAnySubjectMatches.
func linearScanCRBMatch(snap *RBACSnapshot, opts testOpts) []*rbacv1.ClusterRoleBinding {
	var out []*rbacv1.ClusterRoleBinding
	for _, crb := range snap.ClusterRoleBindings {
		if referenceAnySubjectMatches(crb.Subjects, opts) {
			out = append(out, crb)
		}
	}
	return out
}

func linearScanRBMatch(snap *RBACSnapshot, ns string, opts testOpts) []*rbacv1.RoleBinding {
	var out []*rbacv1.RoleBinding
	for _, rb := range snap.RoleBindingsByNS[ns] {
		if referenceAnySubjectMatches(rb.Subjects, opts) {
			out = append(out, rb)
		}
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────
// Test-local index lookup (mirrors the production selectCRBCandidates
// shape so the cache-package test stays self-contained). The PRODUCTION
// selectCRBCandidates lives in internal/rbac/evaluate.go; the rbac
// package's own equivalence test calls THAT one directly. This local
// helper exercises the snapshot fields without crossing the package
// boundary.
// ─────────────────────────────────────────────────────────────────────

func indexLookupCRBMatch(snap *RBACSnapshot, opts testOpts) []*rbacv1.ClusterRoleBinding {
	saNS, saName, isSA := parseSAUsername(opts.Username)
	groups := referenceEffectiveGroups(opts)

	// Union — pointer-keyed set, order-independent.
	seen := make(map[*rbacv1.ClusterRoleBinding]struct{})
	add := func(crbs []*rbacv1.ClusterRoleBinding) {
		for _, c := range crbs {
			seen[c] = struct{}{}
		}
	}

	if opts.Username != "" {
		add(snap.CRBsByUser[opts.Username])
	}
	for _, g := range groups {
		add(snap.CRBsByGroup[g])
	}
	// system:authenticated is added implicitly for every authenticated
	// caller; mirrors anySubjectMatches' implicit branch.
	if opts.Username != "" {
		add(snap.CRBsByGroup["system:authenticated"])
	}
	if isSA {
		add(snap.CRBsByServiceAccount[saNS+"/"+saName])
	}
	add(snap.CRBsCatchAll)

	// Apply post-lookup matcher.
	var out []*rbacv1.ClusterRoleBinding
	for crb := range seen {
		if referenceAnySubjectMatches(crb.Subjects, opts) {
			out = append(out, crb)
		}
	}
	return out
}

func indexLookupRBMatch(snap *RBACSnapshot, ns string, opts testOpts) []*rbacv1.RoleBinding {
	saNS, saName, isSA := parseSAUsername(opts.Username)
	groups := referenceEffectiveGroups(opts)

	seen := make(map[*rbacv1.RoleBinding]struct{})
	add := func(rbs []*rbacv1.RoleBinding) {
		for _, r := range rbs {
			seen[r] = struct{}{}
		}
	}

	if opts.Username != "" {
		add(snap.RBsByUserByNS[ns][opts.Username])
	}
	for _, g := range groups {
		add(snap.RBsByGroupByNS[ns][g])
	}
	if opts.Username != "" {
		add(snap.RBsByGroupByNS[ns]["system:authenticated"])
	}
	if isSA {
		add(snap.RBsByServiceAccountByNS[ns][saNS+"/"+saName])
	}
	add(snap.RBsCatchAllByNS[ns])

	var out []*rbacv1.RoleBinding
	for rb := range seen {
		if referenceAnySubjectMatches(rb.Subjects, opts) {
			out = append(out, rb)
		}
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────
// Set equality (pointer-keyed, order-independent).
// ─────────────────────────────────────────────────────────────────────

func crbSetEqual(a, b []*rbacv1.ClusterRoleBinding) (bool, []*rbacv1.ClusterRoleBinding, []*rbacv1.ClusterRoleBinding) {
	am := make(map[*rbacv1.ClusterRoleBinding]struct{}, len(a))
	bm := make(map[*rbacv1.ClusterRoleBinding]struct{}, len(b))
	for _, x := range a {
		am[x] = struct{}{}
	}
	for _, x := range b {
		bm[x] = struct{}{}
	}
	var missingFromB, extraInB []*rbacv1.ClusterRoleBinding
	for x := range am {
		if _, ok := bm[x]; !ok {
			missingFromB = append(missingFromB, x)
		}
	}
	for x := range bm {
		if _, ok := am[x]; !ok {
			extraInB = append(extraInB, x)
		}
	}
	return len(missingFromB) == 0 && len(extraInB) == 0, missingFromB, extraInB
}

func rbSetEqual(a, b []*rbacv1.RoleBinding) (bool, []*rbacv1.RoleBinding, []*rbacv1.RoleBinding) {
	am := make(map[*rbacv1.RoleBinding]struct{}, len(a))
	bm := make(map[*rbacv1.RoleBinding]struct{}, len(b))
	for _, x := range a {
		am[x] = struct{}{}
	}
	for _, x := range b {
		bm[x] = struct{}{}
	}
	var missingFromB, extraInB []*rbacv1.RoleBinding
	for x := range am {
		if _, ok := bm[x]; !ok {
			missingFromB = append(missingFromB, x)
		}
	}
	for x := range bm {
		if _, ok := am[x]; !ok {
			extraInB = append(extraInB, x)
		}
	}
	return len(missingFromB) == 0 && len(extraInB) == 0, missingFromB, extraInB
}

// ─────────────────────────────────────────────────────────────────────
// Synthetic snapshot builder — 8533-scale, every §3 Kind covered.
// ─────────────────────────────────────────────────────────────────────

const syntheticCRBScale = 8533

// buildSyntheticSnapshot returns a snapshot whose ClusterRoleBindings
// slice (and the parallel RB[ns="default"] slice) match the architect's
// §3 case-walk at production scale (~8533 CRBs). Composition:
//   - 100 CRBs with User subjects (varied names)
//   - 100 CRBs with Group subjects (varied names + system:authenticated
//     + system:serviceaccounts + system:serviceaccounts:<ns>)
//   - 100 CRBs with ServiceAccount subjects (varied ns/name)
//   - 10  CRBs with unknown Kind subjects (catch-all path)
//   - 5   CRBs with multi-subject mix (User + Group + SA in one)
//   - balance to 8533 with empty-subjects CRBs (the production tail)
//
// rebuildIndexes(snap) is called by the test BEFORE running the
// equivalence comparison so the post-fix index fields are populated.
func buildSyntheticSnapshot(t *testing.T) *RBACSnapshot {
	t.Helper()
	snap := &RBACSnapshot{
		RoleBindingsByNS:   map[string][]*rbacv1.RoleBinding{},
		ClusterRolesByName: map[string]*rbacv1.ClusterRole{},
		RolesByNSName:      map[string]*rbacv1.Role{},
	}

	// 100 User-subject CRBs
	for i := 0; i < 100; i++ {
		snap.ClusterRoleBindings = append(snap.ClusterRoleBindings,
			crbWithSubjects("user-crb-"+strconv.Itoa(i), userSub("user-"+strconv.Itoa(i))))
	}

	// 100 Group-subject CRBs — mix of named groups + 3 system groups.
	for i := 0; i < 100; i++ {
		var name string
		switch i % 10 {
		case 0:
			name = "system:authenticated"
		case 1:
			name = "system:serviceaccounts"
		case 2:
			name = "system:serviceaccounts:krateo-system"
		default:
			name = "group-" + strconv.Itoa(i)
		}
		snap.ClusterRoleBindings = append(snap.ClusterRoleBindings,
			crbWithSubjects("group-crb-"+strconv.Itoa(i), groupSub(name)))
	}

	// 100 SA-subject CRBs
	for i := 0; i < 100; i++ {
		ns := "ns-" + strconv.Itoa(i%5)
		name := "sa-" + strconv.Itoa(i)
		snap.ClusterRoleBindings = append(snap.ClusterRoleBindings,
			crbWithSubjects("sa-crb-"+strconv.Itoa(i), saSub(ns, name)))
	}

	// 10 unknown-Kind CRBs (catch-all path)
	for i := 0; i < 10; i++ {
		snap.ClusterRoleBindings = append(snap.ClusterRoleBindings,
			crbWithSubjects("unknown-crb-"+strconv.Itoa(i), unknownKindSub("future-"+strconv.Itoa(i))))
	}

	// 5 multi-subject CRBs — exercises pointer-dedup at union.
	for i := 0; i < 5; i++ {
		snap.ClusterRoleBindings = append(snap.ClusterRoleBindings,
			crbWithSubjects("multi-crb-"+strconv.Itoa(i),
				userSub("multi-user-"+strconv.Itoa(i)),
				groupSub("multi-group-"+strconv.Itoa(i)),
				saSub("multi-ns", "multi-sa-"+strconv.Itoa(i)),
			))
	}

	// Balance with empty-subjects CRBs (the production tail).
	remaining := syntheticCRBScale - len(snap.ClusterRoleBindings)
	for i := 0; i < remaining; i++ {
		snap.ClusterRoleBindings = append(snap.ClusterRoleBindings,
			crbWithSubjects("empty-crb-"+strconv.Itoa(i) /* no subjects */))
	}

	if got, want := len(snap.ClusterRoleBindings), syntheticCRBScale; got != want {
		t.Fatalf("synthetic snapshot CRB count = %d; want %d", got, want)
	}

	// Mirror a small RB set in ns "default" so the RB equivalence test
	// has something to range over. Use the same shape (User + Group + SA
	// + unknown + empty) at 1/10 the scale.
	const rbScale = 853
	for i := 0; i < 10; i++ {
		snap.RoleBindingsByNS["default"] = append(snap.RoleBindingsByNS["default"],
			rbWithSubjects("default", "user-rb-"+strconv.Itoa(i), userSub("user-"+strconv.Itoa(i))))
	}
	for i := 0; i < 10; i++ {
		var name string
		switch i % 5 {
		case 0:
			name = "system:authenticated"
		case 1:
			name = "system:serviceaccounts"
		default:
			name = "group-" + strconv.Itoa(i)
		}
		snap.RoleBindingsByNS["default"] = append(snap.RoleBindingsByNS["default"],
			rbWithSubjects("default", "group-rb-"+strconv.Itoa(i), groupSub(name)))
	}
	for i := 0; i < 10; i++ {
		snap.RoleBindingsByNS["default"] = append(snap.RoleBindingsByNS["default"],
			rbWithSubjects("default", "sa-rb-"+strconv.Itoa(i), saSub("default", "sa-"+strconv.Itoa(i))))
	}
	for i := 0; i < 5; i++ {
		snap.RoleBindingsByNS["default"] = append(snap.RoleBindingsByNS["default"],
			rbWithSubjects("default", "unknown-rb-"+strconv.Itoa(i), unknownKindSub("u-"+strconv.Itoa(i))))
	}
	tail := rbScale - len(snap.RoleBindingsByNS["default"])
	for i := 0; i < tail; i++ {
		snap.RoleBindingsByNS["default"] = append(snap.RoleBindingsByNS["default"],
			rbWithSubjects("default", "empty-rb-"+strconv.Itoa(i)))
	}

	// Populate the indexes (the function under test).
	rebuildSubjectIndexes(snap)
	return snap
}

// ─────────────────────────────────────────────────────────────────────
// HG-169-2 — Equivalence (LOAD-BEARING falsifier)
// ─────────────────────────────────────────────────────────────────────

// TestRBACSnapshot_SubjectIndex_Equivalence is the HG-169-2 correctness
// gate. For every test subject, the set of CRBs and RBs returned by the
// linear scan ∩ anySubjectMatches MUST equal the set returned by the
// index lookup ∩ anySubjectMatches (pointer-set equality).
//
// Any divergence is a permit-loss bug (under-inclusion) or — less
// catastrophic but still flagged — over-inclusion that the post-lookup
// matcher should have filtered. The test catches both.
func TestRBACSnapshot_SubjectIndex_Equivalence(t *testing.T) {
	snap := buildSyntheticSnapshot(t)

	testSubjects := []testOpts{
		// admin-like
		{Username: "admin", Groups: []string{"system:masters", "system:authenticated"}},
		// cyberjoker (narrow user)
		{Username: "cyberjoker", Groups: []string{"system:authenticated"}},
		// canonical SA
		{
			Username: "system:serviceaccount:krateo-system:snowplow",
			Groups:   []string{"system:authenticated"},
		},
		// SA matching one of the synthetic sa-crb-* entries
		{
			Username: "system:serviceaccount:ns-0:sa-0",
			Groups:   []string{"system:authenticated"},
		},
		// SA matching one of the multi-subject CRBs
		{
			Username: "system:serviceaccount:multi-ns:multi-sa-2",
			Groups:   []string{"system:authenticated"},
		},
		// user matching one of the synthetic user-crb-* entries
		{Username: "user-42", Groups: []string{"system:authenticated"}},
		// user matching a named group
		{Username: "alice", Groups: []string{"group-5", "system:authenticated"}},
		// edge: empty username, no groups (no subject should match)
		{Username: "", Groups: nil},
		// edge: user with no matches anywhere
		{Username: "nobody", Groups: []string{"random-group-xyz"}},
		// edge: multiple groups, one matches
		{Username: "alice", Groups: []string{"group-1", "group-7", "group-99", "system:authenticated"}},
	}

	for _, opts := range testSubjects {
		opts := opts
		t.Run("CRB/"+opts.Username, func(t *testing.T) {
			L := linearScanCRBMatch(snap, opts)
			I := indexLookupCRBMatch(snap, opts)
			eq, missing, extra := crbSetEqual(L, I)
			if !eq {
				t.Errorf("HG-169-2 VIOLATION: linear-scan vs index-lookup differ for opts=%+v\n"+
					"  linear-scan size = %d, index-lookup size = %d\n"+
					"  missing from index (under-inclusion): %d\n"+
					"  extra in index (over-inclusion):       %d",
					opts, len(L), len(I), len(missing), len(extra))
				if len(missing) > 0 {
					for i, c := range missing {
						if i >= 5 {
							t.Logf("  ... and %d more", len(missing)-5)
							break
						}
						t.Logf("  missing CRB[%d] name=%q subjects=%+v", i, c.Name, c.Subjects)
					}
				}
			}
		})
		t.Run("RB/"+opts.Username, func(t *testing.T) {
			L := linearScanRBMatch(snap, "default", opts)
			I := indexLookupRBMatch(snap, "default", opts)
			eq, missing, extra := rbSetEqual(L, I)
			if !eq {
				t.Errorf("HG-169-2 VIOLATION: linear-scan vs index-lookup differ for opts=%+v ns=default\n"+
					"  linear-scan size = %d, index-lookup size = %d\n"+
					"  missing from index: %d, extra in index: %d",
					opts, len(L), len(I), len(missing), len(extra))
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────
// IndexCoverage — per-Kind landing (PM CHECK #6)
// ─────────────────────────────────────────────────────────────────────

// TestRBACSnapshot_IndexCoverage_AllSubjectKinds asserts that each
// supported Subject.Kind lands in the correct index, and that an
// unrecognised Kind falls into CRBsCatchAll. Mirrors the architect's
// §3 case-walk.
func TestRBACSnapshot_IndexCoverage_AllSubjectKinds(t *testing.T) {
	cases := []struct {
		label     string
		subject   rbacv1.Subject
		assertIn  func(t *testing.T, snap *RBACSnapshot, crb *rbacv1.ClusterRoleBinding)
		assertNot func(t *testing.T, snap *RBACSnapshot, crb *rbacv1.ClusterRoleBinding)
	}{
		{
			label:   "User",
			subject: userSub("alice"),
			assertIn: func(t *testing.T, snap *RBACSnapshot, crb *rbacv1.ClusterRoleBinding) {
				if !containsCRB(snap.CRBsByUser["alice"], crb) {
					t.Errorf("CRBsByUser[alice] missing the test CRB")
				}
			},
			assertNot: func(t *testing.T, snap *RBACSnapshot, crb *rbacv1.ClusterRoleBinding) {
				if containsCRB(snap.CRBsCatchAll, crb) {
					t.Errorf("CRBsCatchAll should NOT contain a User-only CRB")
				}
			},
		},
		{
			label:   "Group",
			subject: groupSub("devs"),
			assertIn: func(t *testing.T, snap *RBACSnapshot, crb *rbacv1.ClusterRoleBinding) {
				if !containsCRB(snap.CRBsByGroup["devs"], crb) {
					t.Errorf("CRBsByGroup[devs] missing the test CRB")
				}
			},
			assertNot: func(t *testing.T, snap *RBACSnapshot, crb *rbacv1.ClusterRoleBinding) {
				if containsCRB(snap.CRBsCatchAll, crb) {
					t.Errorf("CRBsCatchAll should NOT contain a Group-only CRB")
				}
			},
		},
		{
			label:   "ServiceAccount",
			subject: saSub("krateo-system", "snowplow"),
			assertIn: func(t *testing.T, snap *RBACSnapshot, crb *rbacv1.ClusterRoleBinding) {
				if !containsCRB(snap.CRBsByServiceAccount["krateo-system/snowplow"], crb) {
					t.Errorf("CRBsByServiceAccount[krateo-system/snowplow] missing the test CRB")
				}
			},
			assertNot: func(t *testing.T, snap *RBACSnapshot, crb *rbacv1.ClusterRoleBinding) {
				if containsCRB(snap.CRBsCatchAll, crb) {
					t.Errorf("CRBsCatchAll should NOT contain a SA-only CRB")
				}
			},
		},
		{
			label:   "Unknown",
			subject: unknownKindSub("future-thing"),
			assertIn: func(t *testing.T, snap *RBACSnapshot, crb *rbacv1.ClusterRoleBinding) {
				if !containsCRB(snap.CRBsCatchAll, crb) {
					t.Errorf("CRBsCatchAll missing the unknown-Kind test CRB — under-inclusion")
				}
			},
			assertNot: func(t *testing.T, snap *RBACSnapshot, crb *rbacv1.ClusterRoleBinding) {
				// no per-kind index should hold an unknown-Kind subject
				for _, slice := range snap.CRBsByUser {
					if containsCRB(slice, crb) {
						t.Errorf("CRBsByUser should NOT contain unknown-Kind CRB")
					}
				}
				for _, slice := range snap.CRBsByGroup {
					if containsCRB(slice, crb) {
						t.Errorf("CRBsByGroup should NOT contain unknown-Kind CRB")
					}
				}
				for _, slice := range snap.CRBsByServiceAccount {
					if containsCRB(slice, crb) {
						t.Errorf("CRBsByServiceAccount should NOT contain unknown-Kind CRB")
					}
				}
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.label, func(t *testing.T) {
			crb := crbWithSubjects("test-"+tc.label, tc.subject)
			snap := &RBACSnapshot{
				ClusterRoleBindings: []*rbacv1.ClusterRoleBinding{crb},
				RoleBindingsByNS:    map[string][]*rbacv1.RoleBinding{},
				ClusterRolesByName:  map[string]*rbacv1.ClusterRole{},
				RolesByNSName:       map[string]*rbacv1.Role{},
			}
			rebuildSubjectIndexes(snap)
			tc.assertIn(t, snap, crb)
			tc.assertNot(t, snap, crb)
		})
	}
}

func containsCRB(slice []*rbacv1.ClusterRoleBinding, want *rbacv1.ClusterRoleBinding) bool {
	for _, c := range slice {
		if c == want {
			return true
		}
	}
	return false
}

// TestRBACSnapshot_IndexCoverage_EmptySubjects asserts that a CRB with
// no Subjects lands in NO index (not even CRBsCatchAll) — matches
// nothing in linear scan, must match nothing in index lookup.
func TestRBACSnapshot_IndexCoverage_EmptySubjects(t *testing.T) {
	crb := crbWithSubjects("empty-subjects" /* no subjects */)
	snap := &RBACSnapshot{
		ClusterRoleBindings: []*rbacv1.ClusterRoleBinding{crb},
		RoleBindingsByNS:    map[string][]*rbacv1.RoleBinding{},
		ClusterRolesByName:  map[string]*rbacv1.ClusterRole{},
		RolesByNSName:       map[string]*rbacv1.Role{},
	}
	rebuildSubjectIndexes(snap)

	if containsCRB(snap.CRBsCatchAll, crb) {
		t.Errorf("empty-subjects CRB should NOT be in CRBsCatchAll (matches nothing)")
	}
	for _, slice := range snap.CRBsByUser {
		if containsCRB(slice, crb) {
			t.Errorf("empty-subjects CRB should NOT be in CRBsByUser")
		}
	}
	for _, slice := range snap.CRBsByGroup {
		if containsCRB(slice, crb) {
			t.Errorf("empty-subjects CRB should NOT be in CRBsByGroup")
		}
	}
}

// TestRBACSnapshot_IndexCoverage_MultiSubjectDedup asserts that a CRB
// with multiple matching subjects appears EXACTLY ONCE in the
// post-union candidate set (pointer dedup).
func TestRBACSnapshot_IndexCoverage_MultiSubjectDedup(t *testing.T) {
	crb := crbWithSubjects("multi",
		userSub("alice"),
		groupSub("devs"),
		groupSub("system:authenticated"),
	)
	snap := &RBACSnapshot{
		ClusterRoleBindings: []*rbacv1.ClusterRoleBinding{crb},
		RoleBindingsByNS:    map[string][]*rbacv1.RoleBinding{},
		ClusterRolesByName:  map[string]*rbacv1.ClusterRole{},
		RolesByNSName:       map[string]*rbacv1.Role{},
	}
	rebuildSubjectIndexes(snap)

	opts := testOpts{Username: "alice", Groups: []string{"devs", "system:authenticated"}}
	got := indexLookupCRBMatch(snap, opts)
	if len(got) != 1 {
		t.Errorf("multi-matching CRB should dedup to 1 candidate; got %d", len(got))
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-169.6 — Concurrent read with -race
// ─────────────────────────────────────────────────────────────────────

// TestRBACSnapshot_SubjectIndex_ConcurrentRead builds a snapshot once,
// then 8 goroutines × 100 reads each iterate the index maps in
// parallel. Indexes are IMMUTABLE post-publish (§3.1), so this must be
// -race clean. The reader workload mimics selectCRBCandidates exactly:
// map lookups + slice iteration, no mutation.
func TestRBACSnapshot_SubjectIndex_ConcurrentRead(t *testing.T) {
	snap := buildSyntheticSnapshot(t)

	const readers = 8
	const itersPerReader = 100
	opts := []testOpts{
		{Username: "admin", Groups: []string{"system:masters", "system:authenticated"}},
		{Username: "user-10", Groups: []string{"system:authenticated"}},
		{Username: "system:serviceaccount:ns-0:sa-0", Groups: []string{"system:authenticated"}},
	}

	var wg sync.WaitGroup
	wg.Add(readers)
	for r := 0; r < readers; r++ {
		go func(r int) {
			defer wg.Done()
			for i := 0; i < itersPerReader; i++ {
				_ = indexLookupCRBMatch(snap, opts[i%len(opts)])
				_ = indexLookupRBMatch(snap, "default", opts[i%len(opts)])
			}
		}(r)
	}
	wg.Wait()
}

// ─────────────────────────────────────────────────────────────────────
// AC-169.7 — Index-build benchmark
// ─────────────────────────────────────────────────────────────────────

// BenchmarkRBACSnapshot_RebuildAt8533Scale measures rebuildSubjectIndexes
// cost at production scale. AC target: ≤ 100 ms per snapshot.
// The pre-population of ClusterRoleBindings is outside the timer.
func BenchmarkRBACSnapshot_RebuildAt8533Scale(b *testing.B) {
	// Build the 8533 CRBs once, outside the timer.
	snap := &RBACSnapshot{
		RoleBindingsByNS:   map[string][]*rbacv1.RoleBinding{},
		ClusterRolesByName: map[string]*rbacv1.ClusterRole{},
		RolesByNSName:      map[string]*rbacv1.Role{},
	}
	for i := 0; i < 100; i++ {
		snap.ClusterRoleBindings = append(snap.ClusterRoleBindings,
			crbWithSubjects("user-crb-"+strconv.Itoa(i), userSub("user-"+strconv.Itoa(i))))
	}
	for i := 0; i < 100; i++ {
		snap.ClusterRoleBindings = append(snap.ClusterRoleBindings,
			crbWithSubjects("group-crb-"+strconv.Itoa(i), groupSub("group-"+strconv.Itoa(i))))
	}
	for i := 0; i < 100; i++ {
		snap.ClusterRoleBindings = append(snap.ClusterRoleBindings,
			crbWithSubjects("sa-crb-"+strconv.Itoa(i), saSub("ns-"+strconv.Itoa(i%5), "sa-"+strconv.Itoa(i))))
	}
	for i := 0; i < 10; i++ {
		snap.ClusterRoleBindings = append(snap.ClusterRoleBindings,
			crbWithSubjects("unknown-crb-"+strconv.Itoa(i), unknownKindSub("u-"+strconv.Itoa(i))))
	}
	remaining := syntheticCRBScale - len(snap.ClusterRoleBindings)
	for i := 0; i < remaining; i++ {
		snap.ClusterRoleBindings = append(snap.ClusterRoleBindings,
			crbWithSubjects("empty-crb-"+strconv.Itoa(i)))
	}

	// Likewise pre-build the RB slice (mirrors live cluster shape).
	for i := 0; i < 853; i++ {
		snap.RoleBindingsByNS["default"] = append(snap.RoleBindingsByNS["default"],
			rbWithSubjects("default", "rb-"+strconv.Itoa(i), userSub("u-"+strconv.Itoa(i))))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rebuildSubjectIndexes(snap)
	}
	b.StopTimer()

	// AC-169.7 surface — log avg ns/op so the run is self-grading.
	if b.N > 0 {
		avg := time.Duration(b.Elapsed().Nanoseconds() / int64(b.N))
		b.Logf("AC-169.7: avg rebuildSubjectIndexes = %v (target ≤ 100ms at %d CRBs)",
			avg, syntheticCRBScale)
		if avg > 100*time.Millisecond {
			b.Errorf("AC-169.7 FAIL: avg = %v > 100ms target", avg)
		}
	}
}
