// binding_set_enumeration_test.go — Ship A.3 / 0.30.179.
//
// Validates:
//
//   TestBindingSetHash_StableUnderEquivalentInput      — same (u, gs) hashes
//                                                         identically across calls.
//   TestBindingSetHash_MatchesCohortRBACGenMechanism   — AC-178.2 byte-equality
//                                                         with the pointer-set
//                                                         hash via
//                                                         collectCohortBindingPtrs.
//   TestBindingSetHash_ShiftsOnBindingMutation         — HG-178.5 invariant:
//                                                         a binding ADD touching
//                                                         this cohort flips
//                                                         the hash.
//   TestEnumerateBindingSetClasses_EmptySnapshot       — nil snapshot returns nil.
//   TestEnumerateBindingSetClasses_BasicDedupe         — two users on the same
//                                                         binding collapse via
//                                                         BindingSetHash.
//   TestEnumerateBindingSetClasses_PrunesSystemAuth    — system:authenticated
//                                                         is not in the powerset
//                                                         domain.
//   TestEnumerateBindingSetClasses_PowersetCapTriggers — > cap relevantGroups
//                                                         falls back to single-
//                                                         group enumeration +
//                                                         bumps counter.
//   TestEnumerateBindingSetClasses_PrunesServiceAccountKind        — Ship 0.30.182:
//                                                                    bindings whose
//                                                                    only subjects
//                                                                    are Kind=SA
//                                                                    produce zero
//                                                                    user/group
//                                                                    cohorts.
//   TestEnumerateBindingSetClasses_PrunesServiceAccountUsernameForm — Ship 0.30.182:
//                                                                    User-kind subjects
//                                                                    whose name carries
//                                                                    the system:service
//                                                                    account: prefix are
//                                                                    pruned from userKeys.

package cache

import (
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
)

// TestBindingSetHash_StableUnderEquivalentInput — calling BindingSetHash
// twice for the same (username, groups) against the same snapshot returns
// the same value. Trivial but load-bearing: ComputeKey folds the hash in
// little-endian uint64; instability would re-bake the L1 key per request.
func TestBindingSetHash_StableUnderEquivalentInput(t *testing.T) {
	resetGenAndSnapshot(t)
	buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{
			mkCRB("admin-bind", userSub("admin")),
			mkCRB("devs-bind", groupSub("devs")),
		},
		nil,
	)

	h1 := BindingSetHash("admin", []string{"devs"})
	h2 := BindingSetHash("admin", []string{"devs"})
	if h1 != h2 {
		t.Fatalf("BindingSetHash not stable: %#x vs %#x", h1, h2)
	}
	if h1 == 0 {
		t.Fatalf("BindingSetHash returned 0 for a cohort with matched bindings")
	}
}

// TestBindingSetHash_MatchesCohortRBACGenMechanism — AC-178.2. The hash
// returned by BindingSetHash MUST equal the value
// fnv64aPointers(collectCohortBindingPtrs(snap, u, gs+implicit-auth)) —
// same helpers, same snapshot. By construction the L1 cell the seed
// populates is the SAME cell the request-time dispatchCacheLookupKey
// hashes for a cohort member.
//
// BindingSetHash injects "system:authenticated" for authenticated users
// (mirrors evaluate.go), so the reference must inject it too.
func TestBindingSetHash_MatchesCohortRBACGenMechanism(t *testing.T) {
	resetGenAndSnapshot(t)
	snap := buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{
			mkCRB("admin-bind", userSub("admin")),
			mkCRB("devs-bind", groupSub("devs")),
		},
		nil,
	)

	want := fnv64aPointers(collectCohortBindingPtrs(snap, "admin",
		[]string{"devs", "system:authenticated"}))
	got := BindingSetHash("admin", []string{"devs"})
	if got != want {
		t.Fatalf("AC-178.2 byte-equality fail: BindingSetHash=%#x; want fnv64aPointers(collectCohortBindingPtrs(... +implicit-auth))=%#x",
			got, want)
	}
}

// TestBindingSetHash_ShiftsOnBindingMutation — HG-178.5. Adding a new
// CRB whose Subjects include the cohort's user MUST change the hash for
// that cohort. A cohort whose binding-set is unchanged keeps the same
// hash.
func TestBindingSetHash_ShiftsOnBindingMutation(t *testing.T) {
	resetGenAndSnapshot(t)
	// Initial: admin matches one CRB; alice matches none.
	buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{mkCRB("admin-bind", userSub("admin"))},
		nil,
	)
	hAdminBefore := BindingSetHash("admin", nil)
	hAliceBefore := BindingSetHash("alice", nil)

	// Mutate: add a SECOND CRB matching admin. alice still matches none.
	buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{
			mkCRB("admin-bind", userSub("admin")),
			mkCRB("admin-bind-v2", userSub("admin")),
		},
		nil,
	)
	hAdminAfter := BindingSetHash("admin", nil)
	hAliceAfter := BindingSetHash("alice", nil)

	if hAdminBefore == hAdminAfter {
		t.Fatalf("HG-178.5: admin's BindingSetHash did NOT shift on a matching binding add (%#x stable)",
			hAdminAfter)
	}
	if hAliceBefore != hAliceAfter {
		t.Fatalf("HG-178.5: alice's BindingSetHash shifted despite no matching binding change (%#x -> %#x)",
			hAliceBefore, hAliceAfter)
	}
}

// TestEnumerateBindingSetClasses_EmptySnapshot — nil snapshot returns
// nil. The PIP seed caller treats nil as "no cohorts to seed".
func TestEnumerateBindingSetClasses_EmptySnapshot(t *testing.T) {
	resetGenAndSnapshot(t)
	PublishRBACSnapshotForTest(nil)
	got := EnumerateBindingSetClasses()
	if got != nil {
		t.Fatalf("EnumerateBindingSetClasses on nil snapshot: got %d classes; want nil", len(got))
	}
}

// TestEnumerateBindingSetClasses_BasicDedupe — two users on the SAME
// matching binding collapse via BindingSetHash dedupe. Two users on
// disjoint bindings stay distinct.
func TestEnumerateBindingSetClasses_BasicDedupe(t *testing.T) {
	resetGenAndSnapshot(t)
	// Single CRB binding two distinct users in the SAME Subjects list:
	// both users share the SAME matched binding-pointer-set, so they
	// collapse to ONE class.
	sharedCRB := mkCRB("shared", userSub("alice"))
	sharedCRB.Subjects = append(sharedCRB.Subjects, userSub("bob"))

	// A separate CRB for carol only.
	carolCRB := mkCRB("carol-bind", userSub("carol"))

	buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{sharedCRB, carolCRB},
		nil,
	)

	out := EnumerateBindingSetClasses()
	if len(out) == 0 {
		t.Fatalf("EnumerateBindingSetClasses returned 0 classes; expected at least 2")
	}

	// Hash equality is the dedupe contract. Compute alice/bob/carol hashes
	// directly and assert alice == bob (same class) and alice != carol.
	hAlice := BindingSetHash("alice", nil)
	hBob := BindingSetHash("bob", nil)
	hCarol := BindingSetHash("carol", nil)
	if hAlice != hBob {
		t.Fatalf("BindingSetHash dedupe: alice=%#x bob=%#x; want equal (same shared CRB)", hAlice, hBob)
	}
	if hAlice == hCarol {
		t.Fatalf("BindingSetHash dedupe: alice and carol collide (%#x); want distinct", hAlice)
	}

	// Output must contain at most ONE class per distinct hash (the dedupe
	// invariant). We don't assert exact count because the empty-user
	// group-only enumeration may produce additional classes.
	seenHashes := map[uint64]int{}
	for _, c := range out {
		h := BindingSetHash(c.Username, c.Groups)
		seenHashes[h]++
	}
	for h, n := range seenHashes {
		if n > 1 {
			t.Fatalf("EnumerateBindingSetClasses produced %d classes for hash %#x; expected ≤ 1 (dedupe failed)",
				n, h)
		}
	}
}

// TestEnumerateBindingSetClasses_PrunesSystemAuth — the implicit
// system:authenticated group is INJECTED into every authenticated tuple's
// effective groups, but NOT part of the powerset domain. The enumerator
// produces (user, []) tuples whose hash already accounts for
// system:authenticated.
func TestEnumerateBindingSetClasses_PrunesSystemAuth(t *testing.T) {
	resetGenAndSnapshot(t)
	// system:authenticated is in CRBsByGroup (some CRB binds it). It
	// MUST NOT show up as an enumeration discriminant for users; instead
	// it gets injected at hash-time as the implicit-group rule.
	buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{
			mkCRB("auth-bind", groupSub("system:authenticated")),
			mkCRB("admin-bind", userSub("admin")),
		},
		nil,
	)

	out := EnumerateBindingSetClasses()
	// Find the admin cohort.
	var adminCohort *Cohort
	for i := range out {
		if out[i].Username == "admin" {
			adminCohort = &out[i]
			break
		}
	}
	if adminCohort == nil {
		t.Fatalf("EnumerateBindingSetClasses did not include admin cohort; got %+v", out)
	}
	// system:authenticated MUST be in admin's Groups (injected post-prune).
	hasAuth := false
	for _, g := range adminCohort.Groups {
		if g == "system:authenticated" {
			hasAuth = true
			break
		}
	}
	if !hasAuth {
		t.Fatalf("admin cohort missing implicit system:authenticated; got Groups=%v", adminCohort.Groups)
	}
}

// TestEnumerateBindingSetClasses_PrunesServiceAccountKind — Ship 0.30.182.
// A binding whose ONLY subject is Kind=ServiceAccount produces NO user-
// or group-typed cohort: SAs don't issue /call traffic and are
// structurally out of scope for the PIP prewarm seed. The Subject-walk
// in the relevantGroups construction skips Kind=SA; the snapshot
// indexer routes Kind=SA into CRBsByServiceAccount (not CRBsByUser), so
// the SA never enters userKeys. The end-to-end invariant is: zero user
// or group cohorts emerge from a binding whose only subject is SA-kind.
func TestEnumerateBindingSetClasses_PrunesServiceAccountKind(t *testing.T) {
	resetGenAndSnapshot(t)
	// Single CRB binding a Kind=ServiceAccount subject (canonical K8s
	// SA-binding pattern). No User-kind or Group-kind subjects at all.
	saCRB := mkCRB("sa-only-bind", saSub("krateo-system", "snowplow"))

	buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{saCRB},
		nil,
	)

	out := EnumerateBindingSetClasses()
	// Expectation: zero user-cohorts AND zero group-cohorts. The SA-kind
	// subject lands in CRBsByServiceAccount (which the enumeration never
	// reads). userKeys + groupKeys are both empty, so no powerset
	// emission and no group-only emission occurs.
	if len(out) != 0 {
		// Render the result for diagnosis if the assertion fails.
		t.Fatalf("EnumerateBindingSetClasses on SA-only snapshot: got %d cohorts (%+v); want 0",
			len(out), out)
	}
}

// TestEnumerateBindingSetClasses_PrunesServiceAccountUsernameForm —
// Ship 0.30.182. A binding whose Subject.Kind=User carries the canonical
// K8s SA username pattern "system:serviceaccount:<ns>:<name>" is pruned
// from userKeys at collection time. This is the primary production
// observation behind the 0.30.182 ship: synthetic-bench clusters wire
// per-namespace SAs as User-kind bindings whose name carries the
// upstream-defined prefix, and those identities do NOT issue /call
// traffic. The prune is generic by prefix (the K8s standard convention),
// not by per-name special-case (feedback_no_special_cases).
//
// Mixed-binding invariant: a real User (no SA prefix) coexisting with
// the SA-prefix User remains in the enumeration. Only the SA-prefix
// names drop out.
func TestEnumerateBindingSetClasses_PrunesServiceAccountUsernameForm(t *testing.T) {
	resetGenAndSnapshot(t)
	// One CRB binds a real User; another binds a SA-prefix User-kind
	// subject (synthetic bench pattern that produced the 0.30.181
	// 16-of-34 SA cohort count).
	buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{
			mkCRB("admin-bind", userSub("admin")),
			mkCRB("sa-prefix-bind", userSub("system:serviceaccount:bench-ns-1:bench-sa")),
		},
		nil,
	)

	out := EnumerateBindingSetClasses()
	// The admin cohort MUST survive; the SA-prefix User-kind cohort MUST
	// be pruned. The result MAY contain group-only cohorts derived from
	// system:authenticated bindings, but no cohort with the SA-prefix
	// username may appear.
	var sawAdmin, sawSAPrefix bool
	for _, c := range out {
		if c.Username == "admin" {
			sawAdmin = true
		}
		if c.Username == "system:serviceaccount:bench-ns-1:bench-sa" {
			sawSAPrefix = true
		}
	}
	if !sawAdmin {
		t.Fatalf("admin cohort was pruned along with the SA-prefix cohort; got %+v", out)
	}
	if sawSAPrefix {
		t.Fatalf("SA-prefix User-kind cohort NOT pruned; got %+v", out)
	}
}
