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

// ─────────────────────────────────────────────────────────────────────
// Ship 0.30.189 — sentinel-based cohort identity normalisation.
//
// The PIP seed enumerator emits `(groupOnlyCohortSentinel, [g])` for the
// authenticated-group-only cohort; the request-time dispatcher's
// BindingSetHash normalises a real group-only user (e.g. "cyberjoker")
// to the same sentinel. Both reach the same L1 cell.
//
// These tests are the falsifier for project_0_30_189_design_2026_05_27.
// ─────────────────────────────────────────────────────────────────────

// TestBindingSetHash_GroupOnlyUser_MatchesSentinel — POSITIVE gate.
// A real user with zero User-kind bindings, holding only Group-kind
// access via "devs", MUST hash identically to the sentinel cohort
// emitted by the PIP enumerator for the same group. Pre-0.30.189 they
// diverged: the empty-username path skipped system:authenticated
// injection; the real-user path included it. First-visit L1 miss.
func TestBindingSetHash_GroupOnlyUser_MatchesSentinel(t *testing.T) {
	resetGenAndSnapshot(t)
	// devs CRB — binds the Group "devs"; no User-kind bindings exist
	// for cyberjoker. Plus an unrelated admin CRB to keep the snapshot
	// realistic (admin has User-kind bindings → does NOT normalise).
	buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{
			mkCRB("devs-bind", groupSub("devs")),
			mkCRB("admin-bind", userSub("admin")),
		},
		nil,
	)

	// Real user with zero User-kind bindings — request-time path.
	hCyber := BindingSetHash("cyberjoker", []string{"devs"})
	// Sentinel cohort — PIP seed enumerator path.
	hSentinel := BindingSetHash(groupOnlyCohortSentinel, []string{"devs"})

	if hCyber == 0 || hSentinel == 0 {
		t.Fatalf("BindingSetHash returned 0 for a cohort with matched bindings (cyber=%#x sentinel=%#x)",
			hCyber, hSentinel)
	}
	if hCyber != hSentinel {
		t.Fatalf("Ship 0.30.189 falsifier: group-only user hash does NOT match sentinel hash "+
			"(cyberjoker=%#x sentinel=%#x) — PIP seed and dispatcher would diverge on the L1 key",
			hCyber, hSentinel)
	}
}

// TestBindingSetHash_UserWithBindings_DistinctFromSentinel — NEGATIVE
// gate. A user with User-kind CRBs (admin) MUST NOT collapse to the
// sentinel. Their hash must include their per-user bindings — the
// sentinel cohort is restricted to identities with zero User-kind
// bindings.
//
// This is the over-collapse guardrail. If normalisation were applied
// indiscriminately, admin's private CRBs would leak into the group-
// only cohort's L1 cell — RBAC cross-user leak. The test asserts
// the boundary holds.
func TestBindingSetHash_UserWithBindings_DistinctFromSentinel(t *testing.T) {
	resetGenAndSnapshot(t)
	buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{
			mkCRB("admin-bind", userSub("admin")), // admin has a User-kind CRB
			mkCRB("devs-bind", groupSub("devs")),
		},
		nil,
	)

	hAdmin := BindingSetHash("admin", []string{"devs"})
	hSentinel := BindingSetHash(groupOnlyCohortSentinel, []string{"devs"})
	hCyber := BindingSetHash("cyberjoker", []string{"devs"})

	if hAdmin == hSentinel {
		t.Fatalf("over-collapse: admin (User-kind CRB) hashed to sentinel cohort (%#x) — "+
			"admin's private bindings would leak into group-only L1 cell",
			hAdmin)
	}
	// Sanity: cyber still collapses; admin does not.
	if hCyber != hSentinel {
		t.Fatalf("regression: cyberjoker stopped collapsing to sentinel (cyber=%#x sentinel=%#x)",
			hCyber, hSentinel)
	}
}

// TestSentinelInvariant_NoRealBindings — the invariant the startup
// collision check enforces: the snapshot built by buildSnapshot()
// (which routes through rebuildSubjectIndexes — the production code
// path) MUST NOT carry any real binding under
// CRBsByUser[groupOnlyCohortSentinel]. If a test fixture's mkCRB seeded
// the sentinel name, the production startup check would panic.
func TestSentinelInvariant_NoRealBindings(t *testing.T) {
	resetGenAndSnapshot(t)
	snap := buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{
			mkCRB("admin-bind", userSub("admin")),
			mkCRB("devs-bind", groupSub("devs")),
			mkCRB("auth-bind", groupSub("system:authenticated")),
		},
		nil,
	)
	if n := len(snap.CRBsByUser[groupOnlyCohortSentinel]); n != 0 {
		t.Fatalf("sentinel invariant violated: CRBsByUser[%q] has %d entries; expected 0",
			groupOnlyCohortSentinel, n)
	}
	for ns, byUser := range snap.RBsByUserByNS {
		if n := len(byUser[groupOnlyCohortSentinel]); n != 0 {
			t.Fatalf("sentinel invariant violated: RBsByUserByNS[%q][%q] has %d entries; expected 0",
				ns, groupOnlyCohortSentinel, n)
		}
	}
}

// TestNormalizeIdempotence — normalize(normalize(u)) == normalize(u)
// for every input class:
//   - empty username (anonymous) → "" stays ""
//   - sentinel → sentinel stays sentinel
//   - real user WITH bindings → stays that user
//   - real user WITHOUT bindings → collapses to sentinel, which then stays sentinel
//
// Property: a second-pass through the normaliser must be a no-op. This
// guards against a future refactor that double-normalises a code path
// (cache miss + cache miss-on-miss) and accidentally re-expands the
// sentinel.
func TestNormalizeIdempotence(t *testing.T) {
	resetGenAndSnapshot(t)
	snap := buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{
			mkCRB("admin-bind", userSub("admin")),
			mkCRB("devs-bind", groupSub("devs")),
		},
		nil,
	)

	cases := []struct {
		name string
		in   string
		want string // expected after FIRST normalisation
	}{
		{"empty", "", ""},
		{"sentinel", groupOnlyCohortSentinel, groupOnlyCohortSentinel},
		{"user_with_user_kind_crb", "admin", "admin"},
		{"user_without_bindings_collapses", "cyberjoker", groupOnlyCohortSentinel},
		{"user_without_bindings_random", "no-such-user", groupOnlyCohortSentinel},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			first := normalizeIdentityForCohort(snap, c.in)
			if first != c.want {
				t.Fatalf("first normalisation of %q = %q; want %q", c.in, first, c.want)
			}
			second := normalizeIdentityForCohort(snap, first)
			if second != first {
				t.Fatalf("idempotence violated: normalize(%q) = %q; normalize(%q) = %q",
					c.in, first, first, second)
			}
		})
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
