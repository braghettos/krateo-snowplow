// rbac_cohort_gen_test.go — Ship GMC.1 / 0.30.175.
//
// Validates the per-cohort RBAC generation primitive:
//
//   Test_CohortRBACGen_StableForSameCohort         — two calls, no mutation, same gen.
//   Test_CohortRBACGen_IndependentMutations        — mutating ONE cohort's binding-set
//                                                     bumps that cohort, leaves others alone.
//   Test_CohortRBACGen_Concurrent                  — -race clean under 10 concurrent callers.
//   Test_CohortKeyHash_GroupOrderInvariant         — group ordering MUST NOT affect the key.
//
// These are the GMC.1 PM-gate ACs (AC-GMC1.1 / AC-GMC1.2 / AC-GMC1.3
// indirectly via -race, AC-GMC1.4 wall-clock is observed in production
// not here).

package cache

import (
	"sync"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
)

// buildSnapshot constructs and publishes a RBACSnapshot from raw CRB/RB
// pointers. Mirrors the test pattern in rbac_snapshot_index_test.go —
// fields are populated, then rebuildSubjectIndexes runs to build the
// CRBs*/RBs* indexes, then PublishRBACSnapshotForTest swaps the global.
func buildSnapshot(t *testing.T, crbs []*rbacv1.ClusterRoleBinding, rbs map[string][]*rbacv1.RoleBinding) *RBACSnapshot {
	t.Helper()
	snap := &RBACSnapshot{
		ClusterRoleBindings: crbs,
		RoleBindingsByNS:    rbs,
		ClusterRolesByName:  map[string]*rbacv1.ClusterRole{},
		RolesByNSName:       map[string]*rbacv1.Role{},
	}
	rebuildSubjectIndexes(snap)
	PublishRBACSnapshotForTest(snap)
	return snap
}

// resetGenAndSnapshot wipes both the cohort generator map AND the
// published snapshot. Used by every test to avoid bleed from a prior
// test's published state (rbacSnap is a package-level atomic.Pointer).
func resetGenAndSnapshot(t *testing.T) {
	t.Helper()
	resetCohortGenMapForTest()
	t.Cleanup(func() {
		resetCohortGenMapForTest()
		PublishRBACSnapshotForTest(nil)
	})
}

// ─────────────────────────────────────────────────────────────────────
// Test 1 — Stable across calls for the same cohort.
// ─────────────────────────────────────────────────────────────────────

func Test_CohortRBACGen_StableForSameCohort(t *testing.T) {
	resetGenAndSnapshot(t)

	adminCRB := mkCRB("admin-bind", userSub("admin"))
	aliceCRB := mkCRB("alice-bind", userSub("alice"))
	buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{adminCRB, aliceCRB},
		nil,
	)

	// First call seeds the cohort's hash; gen bumps from 0 → 1.
	g1 := CohortRBACGen("admin", nil)
	if g1 == 0 {
		t.Fatalf("first call should establish a non-zero gen (got 0)")
	}
	// Second call — no mutation between the two. Hash must match the
	// stored value; gen MUST NOT bump.
	g2 := CohortRBACGen("admin", nil)
	if g1 != g2 {
		t.Fatalf("same-cohort second call bumped gen: %d → %d (expected stable)", g1, g2)
	}
	// A third call cements the contract.
	g3 := CohortRBACGen("admin", nil)
	if g3 != g1 {
		t.Fatalf("same-cohort third call bumped gen: %d → %d (expected stable)", g1, g3)
	}
}

// ─────────────────────────────────────────────────────────────────────
// Test 2 — Independent mutations: one cohort's bump must NOT pull
// another cohort.
// ─────────────────────────────────────────────────────────────────────

func Test_CohortRBACGen_IndependentMutations(t *testing.T) {
	resetGenAndSnapshot(t)

	// Initial snapshot: admin has 1 CRB; alice has 1 CRB.
	adminCRB := mkCRB("admin-bind", userSub("admin"))
	aliceCRB := mkCRB("alice-bind", userSub("alice"))
	buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{adminCRB, aliceCRB},
		nil,
	)

	// Seed gen for both cohorts.
	adminInitial := CohortRBACGen("admin", nil)
	aliceInitial := CohortRBACGen("alice", nil)
	if adminInitial == 0 || aliceInitial == 0 {
		t.Fatalf("expected non-zero initial gens (admin=%d alice=%d)", adminInitial, aliceInitial)
	}

	// Mutate ONLY alice's matched binding: replace aliceCRB with a brand-new
	// pointer holding the same Subject. The pointer-set CHANGES for alice
	// (old pointer gone, new pointer in); admin's pointer-set is unchanged.
	aliceCRBv2 := mkCRB("alice-bind-v2", userSub("alice"))
	buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{adminCRB, aliceCRBv2}, // admin pointer-IDENTICAL
		nil,
	)

	adminAfter := CohortRBACGen("admin", nil)
	aliceAfter := CohortRBACGen("alice", nil)

	// Admin's matched-binding-set is the SAME pointer (adminCRB) — gen MUST stay.
	if adminAfter != adminInitial {
		t.Errorf("admin gen bumped despite no change to admin's matched-binding-set: %d → %d",
			adminInitial, adminAfter)
	}
	// Alice's matched-binding-set has a new pointer — gen MUST bump.
	if aliceAfter == aliceInitial {
		t.Errorf("alice gen did NOT bump after replacing alice's CRB pointer: stayed at %d", aliceAfter)
	}
	// Sanity: alice bumped by exactly 1 (single mutation, one hash mismatch).
	if aliceAfter != aliceInitial+1 {
		t.Errorf("alice gen bumped by %d (expected exactly 1): %d → %d",
			aliceAfter-aliceInitial, aliceInitial, aliceAfter)
	}

	// Calling admin again post-mutation should STILL be stable — admin's
	// pointer-set never changed.
	adminAgain := CohortRBACGen("admin", nil)
	if adminAgain != adminInitial {
		t.Errorf("admin gen drifted on third call: %d (expected %d)", adminAgain, adminInitial)
	}
}

// ─────────────────────────────────────────────────────────────────────
// Test 3 — Concurrent callers, -race must stay clean. Sister contract
// to feedback_shared_vs_copy_is_a_concurrency_change.md.
// ─────────────────────────────────────────────────────────────────────

func Test_CohortRBACGen_Concurrent(t *testing.T) {
	resetGenAndSnapshot(t)

	adminCRB := mkCRB("admin-bind", userSub("admin"))
	buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{adminCRB},
		nil,
	)

	const goroutines = 10
	const callsPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < callsPerGoroutine; j++ {
				_ = CohortRBACGen("admin", nil)
			}
		}()
	}
	wg.Wait()

	// Smoke check post-concurrency: another call still returns a
	// non-zero gen and is stable on a repeat.
	g1 := CohortRBACGen("admin", nil)
	g2 := CohortRBACGen("admin", nil)
	if g1 == 0 {
		t.Errorf("gen is 0 post-concurrent burst (expected non-zero)")
	}
	if g1 != g2 {
		t.Errorf("post-burst calls not stable: %d vs %d", g1, g2)
	}
}

// ─────────────────────────────────────────────────────────────────────
// Test 4 — Cohort key is group-order-invariant.
// ─────────────────────────────────────────────────────────────────────

func Test_CohortKeyHash_GroupOrderInvariant(t *testing.T) {
	cases := []struct {
		name     string
		username string
		groups   []string
	}{
		{"ab", "alice", []string{"a", "b"}},
		{"ba", "alice", []string{"b", "a"}},
		{"abc", "alice", []string{"a", "b", "c"}},
		{"cba", "alice", []string{"c", "b", "a"}},
		{"bca", "alice", []string{"b", "c", "a"}},
	}
	hashes := make(map[string]string, len(cases))
	for _, c := range cases {
		hashes[c.name] = CohortKeyHash(c.username, c.groups)
	}

	if hashes["ab"] != hashes["ba"] {
		t.Errorf("group order changed key: ab=%q ba=%q", hashes["ab"], hashes["ba"])
	}
	if hashes["abc"] != hashes["cba"] || hashes["abc"] != hashes["bca"] {
		t.Errorf("3-group order changed key: abc=%q cba=%q bca=%q",
			hashes["abc"], hashes["cba"], hashes["bca"])
	}

	// Different username MUST yield a different key.
	if k1, k2 := CohortKeyHash("alice", []string{"a"}), CohortKeyHash("bob", []string{"a"}); k1 == k2 {
		t.Errorf("different usernames hashed to same key: %q", k1)
	}

	// Different group sets MUST yield different keys.
	if k1, k2 := CohortKeyHash("alice", []string{"a"}), CohortKeyHash("alice", []string{"b"}); k1 == k2 {
		t.Errorf("different group sets hashed to same key: %q", k1)
	}

	// Empty identity MUST yield a deterministic non-empty key.
	if k := CohortKeyHash("", nil); k == "" {
		t.Errorf("empty identity yielded empty key")
	}
}

// ─────────────────────────────────────────────────────────────────────
// Bonus: nil snapshot smoke.
// ─────────────────────────────────────────────────────────────────────

func Test_CohortRBACGen_NilSnapshotReturnsCurrent(t *testing.T) {
	resetGenAndSnapshot(t)

	// Snapshot is explicitly nil (resetGenAndSnapshot's cleanup will
	// also publish nil, but here we're pre-state; ensure nil).
	PublishRBACSnapshotForTest(nil)

	g := CohortRBACGen("admin", nil)
	if g != 0 {
		t.Errorf("nil-snapshot first call should return gen=0; got %d", g)
	}
	// Second call MUST still return 0 — no hash recompute against the
	// missing snapshot.
	if g2 := CohortRBACGen("admin", nil); g2 != 0 {
		t.Errorf("nil-snapshot second call should still return 0; got %d", g2)
	}
}
