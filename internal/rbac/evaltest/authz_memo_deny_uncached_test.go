// authz_memo_deny_uncached_test.go — Ship 0.30.254 / Task #301 corrective.
//
// L2 (0.30.253) cached BOTH permits and denies in the snapshot-generation
// authz memo. A deny can be TRANSIENTLY WRONG under snapshot churn
// (fail-closed-per-call walk against a momentarily-incoherent rebuild);
// caching it pinned the wrong deny for the whole RBAC generation on a hot
// key (the snowplow SA wildcard CRB that can never be correctly denied),
// starving the RAFullList refresh → admin compositions-list stuck empty
// (#301 §G).
//
// The fix: NEVER cache a deny. These tests are the falsifier set:
//   (a) a deny is NOT stored — the same tuple re-evaluates (misses
//       increment, deny-uncached counter increments, entries stays 0).
//   (b) a permit IS stored (regression guard for the preserved behavior).
//   (c) self-heal: a transient deny followed by a permit WITHIN THE SAME
//       generation returns the permit on the second call — the property
//       L2 broke (deny pinned for the generation). Modelled by swapping
//       the underlying snapshot's binding set WITHOUT bumping PublishSeq.
//
// evaltest package only — never `go test ./internal/rbac/...` against the
// remote kubeconfig (feedback_no_go_test_against_remote_kubeconfig).

package evaltest

import (
	"context"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"

	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/rbac"
)

// TestL2Fix_DenyNotCached_ReEvaluatesEveryCall asserts a deny verdict is
// NOT memoised: repeated identical deny tuples each MISS (re-walk) and the
// deny-uncached counter rises, while no entry is created for the deny.
func TestL2Fix_DenyNotCached_ReEvaluatesEveryCall(t *testing.T) {
	rbac.ResetAuthzMemoForTest()

	// alice is bound; bob is NOT — bob's get is a deny.
	newTestWatcher(t,
		clusterRole("reader", rule([]string{""}, []string{"configmaps"}, []string{"get"})),
		clusterRoleBindingWithUID("alice-bind", "reader", "uid-alice", userSubject("alice")),
	)

	denyOpts := rbac.EvaluateOptions{
		Username: "bob", Verb: "get", Group: "", Resource: "configmaps", Namespace: "default",
		SkipBindingUID: true,
	}

	const n = 5
	for i := 0; i < n; i++ {
		allowed, uid, err := rbac.EvaluateRBAC(context.Background(), denyOpts)
		if err != nil {
			t.Fatalf("deny call %d: err=%v", i, err)
		}
		if allowed {
			t.Fatalf("deny call %d: bob is unbound, MUST be denied; got allowed=true uid=%q", i, uid)
		}
	}

	hits, misses, _, _, entries := rbac.AuthzMemoStatsForTest()
	denyUncached := rbac.AuthzMemoDenyUncachedForTest()

	// Every deny call must MISS (a cached deny would HIT after the first).
	if hits != 0 {
		t.Fatalf("L2-FIX BROKEN: a deny was cached — got %d hits over %d deny calls (want 0; denies must re-walk every call)", hits, n)
	}
	if misses != n {
		t.Fatalf("L2-FIX: expected %d misses (one walk per deny call); got %d", n, misses)
	}
	if denyUncached != uint64(n) {
		t.Fatalf("L2-FIX: expected deny_uncached counter = %d; got %d", n, denyUncached)
	}
	// No deny entry may exist in the shard.
	if entries != 0 {
		t.Fatalf("L2-FIX BROKEN: deny created %d memo entries (want 0 — denies are never stored)", entries)
	}
}

// TestL2Fix_PermitStillCached is the regression guard: permits MUST still
// be cached (the L2 CPU win). A repeated permit tuple hits after the first.
func TestL2Fix_PermitStillCached(t *testing.T) {
	rbac.ResetAuthzMemoForTest()

	newTestWatcher(t,
		clusterRole("reader", rule([]string{""}, []string{"configmaps"}, []string{"get"})),
		clusterRoleBindingWithUID("alice-bind", "reader", "uid-alice", userSubject("alice")),
	)
	permitOpts := rbac.EvaluateOptions{
		Username: "alice", Verb: "get", Group: "", Resource: "configmaps", Namespace: "default",
		SkipBindingUID: true,
	}

	const n = 10
	for i := 0; i < n; i++ {
		allowed, _, err := rbac.EvaluateRBAC(context.Background(), permitOpts)
		if err != nil || !allowed {
			t.Fatalf("permit call %d: allowed=%v err=%v", i, allowed, err)
		}
	}
	hits, misses, _, _, entries := rbac.AuthzMemoStatsForTest()
	if misses != 1 {
		t.Fatalf("L2-FIX permit: expected exactly 1 miss (cold walk) then hits; got %d misses", misses)
	}
	if hits != uint64(n-1) {
		t.Fatalf("L2-FIX permit: expected %d hits; got %d (permits must still be cached)", n-1, hits)
	}
	if entries != 1 {
		t.Fatalf("L2-FIX permit: expected 1 cached permit entry; got %d", entries)
	}
	if du := rbac.AuthzMemoDenyUncachedForTest(); du != 0 {
		t.Fatalf("L2-FIX permit: deny_uncached should be 0 on an all-permit workload; got %d", du)
	}
}

// TestL2Fix_TransientDenySelfHealsSameGeneration is the self-heal
// falsifier — the property L2 broke. A first call denies (snapshot A has
// no binding for alice). Then the underlying snapshot is swapped to B
// (alice IS bound) WITHOUT bumping PublishSeq — modelling a transient
// fail-closed deny that becomes a permit a moment later WITHIN THE SAME
// generation. The second call MUST return the permit. Pre-fix L2 would
// have returned the pinned deny (same gen, cached false).
//
// Both snapshots carry the SAME PublishSeq so the memo's whole-shard swap
// does NOT fire — the only reason the second call can return permit is
// that the deny was never stored and the walk re-ran against B.
func TestL2Fix_TransientDenySelfHealsSameGeneration(t *testing.T) {
	rbac.ResetAuthzMemoForTest()

	const gen = uint64(4242)

	// Snapshot A — alice NOT bound (transient deny).
	snapA := &cache.RBACSnapshot{
		PublishSeq: gen,
		ClusterRolesByName: map[string]*rbacv1.ClusterRole{
			"reader": clusterRole("reader", rule([]string{""}, []string{"configmaps"}, []string{"get"})),
		},
		ClusterRoleBindings: nil, // alice unbound -> deny
	}
	cache.RebuildSubjectIndexesForTest(snapA)
	// A watcher must exist so EvaluateRBAC takes the in-process path
	// (cache=on, rw != nil). newTestWatcher publishes its own snapshot;
	// we override it with snapA via PublishRBACSnapshotForTest.
	newTestWatcher(t,
		clusterRole("reader", rule([]string{""}, []string{"configmaps"}, []string{"get"})),
	)
	cache.PublishRBACSnapshotForTest(snapA)

	opts := rbac.EvaluateOptions{
		Username: "alice", Verb: "get", Group: "", Resource: "configmaps", Namespace: "default",
		SkipBindingUID: true,
	}

	// First call against A -> deny (and, post-fix, NOT cached).
	a1, _, err := rbac.EvaluateRBAC(context.Background(), opts)
	if err != nil {
		t.Fatalf("call 1 (snapA): err=%v", err)
	}
	if a1 {
		t.Fatalf("call 1: alice is unbound in snapA, MUST be denied; got allowed=true")
	}

	// Swap to snapshot B — alice IS bound — keeping the SAME PublishSeq so
	// the memo shard is NOT swapped. The deny must self-heal because it was
	// never cached.
	snapB := &cache.RBACSnapshot{
		PublishSeq: gen, // SAME generation — shard does NOT swap
		ClusterRolesByName: map[string]*rbacv1.ClusterRole{
			"reader": clusterRole("reader", rule([]string{""}, []string{"configmaps"}, []string{"get"})),
		},
		ClusterRoleBindings: []*rbacv1.ClusterRoleBinding{
			clusterRoleBindingWithUID("alice-bind", "reader", "uid-alice", userSubject("alice")),
		},
	}
	cache.RebuildSubjectIndexesForTest(snapB)
	cache.PublishRBACSnapshotForTest(snapB)

	// Second call, SAME generation, snapshot now permits -> MUST allow.
	a2, _, err := rbac.EvaluateRBAC(context.Background(), opts)
	if err != nil {
		t.Fatalf("call 2 (snapB, same gen): err=%v", err)
	}
	if !a2 {
		t.Fatalf("L2-FIX SELF-HEAL BROKEN: a transient deny was pinned for the generation — call 2 against a now-permitting snapshot (SAME PublishSeq=%d) returned deny. The deny must NOT be cached so the walk re-runs and self-heals.", gen)
	}

	// No swap should have occurred (same gen throughout) — proves the
	// self-heal came from not-caching the deny, not from a shard swap.
	_, _, swaps, _, _ := rbac.AuthzMemoStatsForTest()
	// swaps may be >0 from the initial shard creation on first lookup; the
	// load-bearing assertion is the permit on call 2 above. We only assert
	// the deny was uncached.
	if du := rbac.AuthzMemoDenyUncachedForTest(); du == 0 {
		t.Fatalf("L2-FIX SELF-HEAL: expected the call-1 deny to be counted uncached; got 0 (swaps=%d)", swaps)
	}
}
