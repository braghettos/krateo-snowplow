// Q-OOM-FIX (v0.25.313 RCA, 2026-05-08) — unit tests for the EvaluateRBAC
// memoisation cache.
//
// The OOM autopsy showed `crbLister.List(labels.Everything())` accounting
// for 56% of cumulative allocations (~4.15 TB) — every per-item RBAC
// check materialised the entire CRB set. Caching the boolean decision
// per (user, verb, gr, namespace) collapses the lister fan-out to one
// call per unique key.
//
// The two assertions here lock the contract that makes the fix work:
//   1. Cache hit short-circuits BEFORE the lister.
//   2. Existing invalidation paths (broad invalidate + per-user purge)
//      drop entries so subsequent calls re-execute.
package cache

import (
	"context"
	"sync/atomic"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	rbaclisters "k8s.io/client-go/listers/rbac/v1"
	k8scache "k8s.io/client-go/tools/cache"
)

// countingCRBLister wraps a ClusterRoleBindingLister to record List() calls.
type countingCRBLister struct {
	rbaclisters.ClusterRoleBindingLister
	listCalls int64
}

func (c *countingCRBLister) List(selector labels.Selector) ([]*rbacv1.ClusterRoleBinding, error) {
	atomic.AddInt64(&c.listCalls, 1)
	return c.ClusterRoleBindingLister.List(selector)
}

func (c *countingCRBLister) calls() int64 { return atomic.LoadInt64(&c.listCalls) }

// newRBACWatcherWithListers builds a synced RBACWatcher backed by in-memory
// lister indexers seeded with the supplied CRBs/CRs.
func newRBACWatcherWithListers(t *testing.T, crbs []*rbacv1.ClusterRoleBinding, crs []*rbacv1.ClusterRole) (*RBACWatcher, *countingCRBLister) {
	t.Helper()

	crbIndexer := k8scache.NewIndexer(k8scache.MetaNamespaceKeyFunc, k8scache.Indexers{})
	for _, c := range crbs {
		if err := crbIndexer.Add(c); err != nil {
			t.Fatalf("seed crb: %v", err)
		}
	}
	crIndexer := k8scache.NewIndexer(k8scache.MetaNamespaceKeyFunc, k8scache.Indexers{})
	for _, c := range crs {
		if err := crIndexer.Add(c); err != nil {
			t.Fatalf("seed cr: %v", err)
		}
	}
	rbIndexer := k8scache.NewIndexer(k8scache.MetaNamespaceKeyFunc, k8scache.Indexers{})
	rIndexer := k8scache.NewIndexer(k8scache.MetaNamespaceKeyFunc, k8scache.Indexers{})

	counting := &countingCRBLister{ClusterRoleBindingLister: rbaclisters.NewClusterRoleBindingLister(crbIndexer)}

	rw := &RBACWatcher{
		crbLister:  counting,
		rbLister:   rbaclisters.NewRoleBindingLister(rbIndexer),
		crLister:   rbaclisters.NewClusterRoleLister(crIndexer),
		roleLister: rbaclisters.NewRoleLister(rIndexer),
		synced:     true,
		evalCache:  newEvalLRU(evalCacheCap),
	}
	return rw, counting
}

// fixtureClusterAdminBinding builds a CRB+CR pair granting the named user
// cluster-wide list/get on the given resource.
func fixtureClusterAdminBinding(user, group, resource string) (*rbacv1.ClusterRoleBinding, *rbacv1.ClusterRole) {
	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cr-" + user},
		Rules: []rbacv1.PolicyRule{{
			Verbs:     []string{"get", "list"},
			APIGroups: []string{group},
			Resources: []string{resource},
		}},
	}
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "test-crb-" + user},
		Subjects: []rbacv1.Subject{{
			Kind: rbacv1.UserKind,
			Name: user,
		}},
		RoleRef: rbacv1.RoleRef{
			Kind: "ClusterRole",
			Name: cr.Name,
		},
	}
	return crb, cr
}

// TestEvaluateRBAC_CacheHit verifies the first EvaluateRBAC call hits the
// lister but the second call for the same key short-circuits via cache.
func TestEvaluateRBAC_CacheHit(t *testing.T) {
	user := "alice"
	gr := schema.GroupResource{Group: "core.krateo.io", Resource: "compositions"}

	crb, cr := fixtureClusterAdminBinding(user, gr.Group, gr.Resource)
	rw, counting := newRBACWatcherWithListers(t, []*rbacv1.ClusterRoleBinding{crb}, []*rbacv1.ClusterRole{cr})

	// First call: lister scanned, allow returned, decision cached.
	if !rw.EvaluateRBAC(user, nil, "list", gr, "") {
		t.Fatalf("first call: expected allow")
	}
	if got := counting.calls(); got != 1 {
		t.Fatalf("first call: lister calls = %d, want 1", got)
	}

	// Second call (same key): MUST short-circuit before the lister.
	if !rw.EvaluateRBAC(user, nil, "list", gr, "") {
		t.Fatalf("second call: expected allow from cache")
	}
	if got := counting.calls(); got != 1 {
		t.Fatalf("second call: lister calls = %d, want 1 (cache hit expected)", got)
	}

	// A different namespace key SHOULD miss and re-scan (proves the key
	// shape includes namespace and that not every miss is masked).
	if !rw.EvaluateRBAC(user, nil, "list", gr, "ns-other") {
		t.Fatalf("ns-scoped call: expected allow (cluster-wide CRB covers all namespaces)")
	}
	if got := counting.calls(); got != 2 {
		t.Fatalf("ns-scoped call: lister calls = %d, want 2", got)
	}
}

// TestEvaluateRBAC_InvalidatedOnRBACChange verifies invalidate() drops the
// cache so a subsequent call re-scans the lister.
func TestEvaluateRBAC_InvalidatedOnRBACChange(t *testing.T) {
	user := "bob"
	gr := schema.GroupResource{Group: "core.krateo.io", Resource: "compositions"}

	crb, cr := fixtureClusterAdminBinding(user, gr.Group, gr.Resource)
	rw, counting := newRBACWatcherWithListers(t, []*rbacv1.ClusterRoleBinding{crb}, []*rbacv1.ClusterRole{cr})

	// Prime the cache.
	if !rw.EvaluateRBAC(user, nil, "get", gr, "") {
		t.Fatalf("prime: expected allow")
	}
	if got := counting.calls(); got != 1 {
		t.Fatalf("prime: lister calls = %d, want 1", got)
	}

	// Invalidate (simulates a Role/ClusterRole change). invalidate() also
	// fans out to L1 evictions; we wire a no-op cache so the SMembers /
	// Delete legs don't panic on the bare watcher.
	rw.cache = &nopCache{}
	rw.invalidate(context.Background())

	// Re-call: lister MUST be re-scanned.
	if !rw.EvaluateRBAC(user, nil, "get", gr, "") {
		t.Fatalf("post-invalidate: expected allow")
	}
	if got := counting.calls(); got != 2 {
		t.Fatalf("post-invalidate: lister calls = %d, want 2 (cache should be flushed)", got)
	}
}

// TestEvaluateRBAC_PerUserPurge verifies purgeUserCacheData() drops only
// the user's entries — other users' cached decisions survive.
func TestEvaluateRBAC_PerUserPurge(t *testing.T) {
	gr := schema.GroupResource{Group: "core.krateo.io", Resource: "compositions"}
	crbA, crA := fixtureClusterAdminBinding("alice", gr.Group, gr.Resource)
	crbB, crB := fixtureClusterAdminBinding("bob", gr.Group, gr.Resource)

	rw, counting := newRBACWatcherWithListers(t,
		[]*rbacv1.ClusterRoleBinding{crbA, crbB},
		[]*rbacv1.ClusterRole{crA, crB},
	)
	rw.cache = &nopCache{}

	// Prime both users.
	rw.EvaluateRBAC("alice", nil, "list", gr, "")
	rw.EvaluateRBAC("bob", nil, "list", gr, "")
	if got := counting.calls(); got != 2 {
		t.Fatalf("prime: lister calls = %d, want 2", got)
	}

	// Purge only alice. ComputeBindingIdentity inside purgeUserCacheData
	// itself lists CRBs once; capture that overhead in the expected count
	// so the assertion stays focused on the EvaluateRBAC cache contract.
	rw.purgeUserCacheData(context.Background(), "alice")
	postPurgeListerCalls := counting.calls()

	// Bob's decision MUST still be cached: re-call adds no lister call
	// beyond what purgeUserCacheData itself triggered.
	rw.EvaluateRBAC("bob", nil, "list", gr, "")
	if got := counting.calls(); got != postPurgeListerCalls {
		t.Fatalf("bob post-purge-of-alice: lister calls = %d, want %d (bob's cache must survive)", got, postPurgeListerCalls)
	}

	// Alice's decision MUST be invalidated: re-call adds exactly one
	// lister call.
	rw.EvaluateRBAC("alice", nil, "list", gr, "")
	if got := counting.calls(); got != postPurgeListerCalls+1 {
		t.Fatalf("alice post-purge: lister calls = %d, want %d (alice's cache must be flushed)", got, postPurgeListerCalls+1)
	}
}

// TestEvaluateRBAC_DistinguishesGroups — Q-OOM-FIX patch B-prime fixup
// (architect 2nd-opinion ship-blocker, 2026-05-08).
//
// Three production code paths call EvaluateRBAC with the SAME username
// but DIFFERENT groups slices (HTTP/JWT, prewarm cert OUs, L1 background
// refresh hard-coded [system:masters]). Without the groups in the cache
// key, whichever caller hits first poisons the cache for the others —
// purgeUserCacheData does NOT fire because no RBAC binding mutated, so
// the stale authz decision sticks.
//
// This test pins the contract that the cache key DISTINGUISHES disjoint
// group sets:
//   1. EvaluateRBAC(alice, [admins], ...)      — lister called once.
//   2. EvaluateRBAC(alice, [developers], ...)  — lister called AGAIN
//      because the (user, groupsHash) tuple differs.
//   3. EvaluateRBAC(alice, [admins], ...)      — cache HIT, lister NOT
//      called again.
//
// Sort-order independence is tested by passing the same set in two
// orders and asserting cache hit.
func TestEvaluateRBAC_DistinguishesGroups(t *testing.T) {
	user := "alice"
	gr := schema.GroupResource{Group: "core.krateo.io", Resource: "compositions"}

	crb, cr := fixtureClusterAdminBinding(user, gr.Group, gr.Resource)
	rw, counting := newRBACWatcherWithListers(t,
		[]*rbacv1.ClusterRoleBinding{crb}, []*rbacv1.ClusterRole{cr})

	// 1. First call with [admins] — populates cache.
	if !rw.EvaluateRBAC(user, []string{"admins"}, "list", gr, "") {
		t.Fatalf("call#1 [admins]: expected allow")
	}
	if got := counting.calls(); got != 1 {
		t.Fatalf("call#1 [admins]: lister calls = %d, want 1", got)
	}

	// 2. Same user, DIFFERENT groups — MUST miss the cache and re-scan.
	if !rw.EvaluateRBAC(user, []string{"developers"}, "list", gr, "") {
		t.Fatalf("call#2 [developers]: expected allow")
	}
	if got := counting.calls(); got != 2 {
		t.Fatalf("call#2 [developers]: lister calls = %d, want 2 "+
			"(SHIP BLOCKER: cache key must distinguish groups slices)", got)
	}

	// 3. Repeat call#1's tuple — MUST hit the cache.
	if !rw.EvaluateRBAC(user, []string{"admins"}, "list", gr, "") {
		t.Fatalf("call#3 [admins] repeat: expected allow")
	}
	if got := counting.calls(); got != 2 {
		t.Fatalf("call#3 [admins] repeat: lister calls = %d, want 2 (cache hit expected)", got)
	}

	// 4. Multi-element set [admins, devs] (sorted form) — distinct
	// from both prior tuples, MUST miss.
	if !rw.EvaluateRBAC(user, []string{"admins", "devs"}, "list", gr, "") {
		t.Fatalf("call#4 [admins,devs]: expected allow")
	}
	if got := counting.calls(); got != 3 {
		t.Fatalf("call#4 [admins,devs]: lister calls = %d, want 3", got)
	}

	// 5. Same multi-element set in REVERSE order — sort-key MUST treat
	// it as identical to call#4 (cache hit).
	if !rw.EvaluateRBAC(user, []string{"devs", "admins"}, "list", gr, "") {
		t.Fatalf("call#5 [devs,admins] reversed: expected allow")
	}
	if got := counting.calls(); got != 3 {
		t.Fatalf("call#5 [devs,admins] reversed: lister calls = %d, want 3 "+
			"(sort-stable hash should produce cache HIT)", got)
	}

	// 6. nil vs empty-slice groups — MUST hash identically (both → 0
	// per the len(groups) > 0 guard in evalCacheKey).
	if !rw.EvaluateRBAC(user, nil, "list", gr, "") {
		t.Fatalf("call#6 nil-groups: expected allow")
	}
	listerCallsAfterNil := counting.calls()
	if !rw.EvaluateRBAC(user, []string{}, "list", gr, "") {
		t.Fatalf("call#7 empty-groups: expected allow")
	}
	if got := counting.calls(); got != listerCallsAfterNil {
		t.Fatalf("call#7 empty-groups: lister calls = %d, want %d "+
			"(nil and empty groups MUST hash identically)", got, listerCallsAfterNil)
	}
}

// TestEvalCacheKey_GroupsAreSortStable — direct unit test for the key
// derivation. Belt-and-braces complement to TestEvaluateRBAC_Distinguishes
// Groups: locks the sort+hash contract at the function boundary so a
// future regression in evalCacheKey would surface here even without
// going through EvaluateRBAC.
func TestEvalCacheKey_GroupsAreSortStable(t *testing.T) {
	gr := schema.GroupResource{Group: "core.krateo.io", Resource: "compositions"}
	k1 := evalCacheKey("alice", "list", gr, "ns1", []string{"admins", "devs", "ops"})
	k2 := evalCacheKey("alice", "list", gr, "ns1", []string{"ops", "admins", "devs"})
	if k1 != k2 {
		t.Fatalf("evalCacheKey not sort-stable:\n k1=%q\n k2=%q", k1, k2)
	}

	k3 := evalCacheKey("alice", "list", gr, "ns1", []string{"admins"})
	if k1 == k3 {
		t.Fatalf("evalCacheKey collapsed disjoint group sets to the same key:\n k1=%q\n k3=%q", k1, k3)
	}

	kNil := evalCacheKey("alice", "list", gr, "ns1", nil)
	kEmpty := evalCacheKey("alice", "list", gr, "ns1", []string{})
	if kNil != kEmpty {
		t.Fatalf("nil vs empty groups should hash identically:\n nil=%q\n empty=%q", kNil, kEmpty)
	}

	// Username-prefix invariant: the per-user purge in purgeUserCacheData
	// uses HasPrefix on "username|", which MUST still match every key
	// for that user regardless of groups.
	for _, key := range []string{k1, k2, k3, kNil, kEmpty} {
		if !startsWithPrefix(key, "alice|") {
			t.Fatalf("evalCacheKey lost username-prefix invariant: %q", key)
		}
	}
}

// startsWithPrefix is a tiny stand-in for strings.HasPrefix to keep this
// test file's import list trivially small. Same semantics as
// strings.HasPrefix.
func startsWithPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// TestEvaluateRBAC_LRUEvictsOldestUnderPressure verifies the bounded LRU
// drops the oldest entry once cap is exceeded. Architect requirement
// (re-review 2026-05-08): the cache must NOT grow unbounded under
// production tenant density (worst case 50M keys × 120 B = ~6 GB).
//
// Uses a tiny cap (16) directly against the evalLRU type rather than
// inserting evalCacheCap+1 entries through EvaluateRBAC, which would
// require seeding fixtures for 200001 distinct users. The contract
// under test is the LRU eviction itself; the EvaluateRBAC integration
// is covered by TestEvaluateRBAC_CacheHit.
func TestEvaluateRBAC_LRUEvictsOldestUnderPressure(t *testing.T) {
	const cap = 16
	lru := newEvalLRU(cap)

	// Insert cap+1 entries.
	for i := 0; i < cap+1; i++ {
		lru.Add(keyFromInt(i), true)
	}

	if got := lru.Len(); got != cap {
		t.Fatalf("len after cap+1 inserts = %d, want %d", got, cap)
	}

	// The first-inserted key (index 0) must be evicted (oldest).
	if lru.Contains(keyFromInt(0)) {
		t.Fatalf("oldest key %q still present after eviction pressure", keyFromInt(0))
	}

	// Every other key (1..cap) must still be resident.
	for i := 1; i <= cap; i++ {
		if !lru.Contains(keyFromInt(i)) {
			t.Fatalf("key %q evicted prematurely (only oldest should be dropped)", keyFromInt(i))
		}
	}

	// Verify the production cap value is the architect-mandated 200_000.
	if evalCacheCap != 200_000 {
		t.Fatalf("evalCacheCap = %d, want 200000 (architect re-review 2026-05-08)", evalCacheCap)
	}
}

// TestEvaluateRBAC_LRUGetPromotesToMRU verifies Get() moves an entry to
// the MRU end so it survives subsequent eviction. This is the load-
// bearing LRU semantics check — without it the cache would behave like
// FIFO (architect re-review 2026-05-08 specified strict LRU).
func TestEvaluateRBAC_LRUGetPromotesToMRU(t *testing.T) {
	const cap = 4
	lru := newEvalLRU(cap)

	for i := 0; i < cap; i++ {
		lru.Add(keyFromInt(i), true)
	}

	// Get the oldest entry — should promote it to MRU.
	if _, ok := lru.Get(keyFromInt(0)); !ok {
		t.Fatalf("Get(0) miss before any eviction")
	}

	// Add one more entry; the eviction victim should now be index 1
	// (oldest after the Get() promotion of index 0).
	lru.Add(keyFromInt(99), true)

	if !lru.Contains(keyFromInt(0)) {
		t.Fatalf("key 0 evicted despite Get() promotion (FIFO behaviour, not LRU)")
	}
	if lru.Contains(keyFromInt(1)) {
		t.Fatalf("key 1 should have been evicted (LRU after key 0 promotion)")
	}
}

// TestEvaluateRBAC_LRUPurgeAndPrefix verifies Purge and RemoveWithPrefix
// fully drop matching entries.
func TestEvaluateRBAC_LRUPurgeAndPrefix(t *testing.T) {
	lru := newEvalLRU(64)
	lru.Add("alice|list||compositions|", true)
	lru.Add("alice|get||compositions|ns1", true)
	lru.Add("bob|list||compositions|", true)

	lru.RemoveWithPrefix("alice|")
	if lru.Contains("alice|list||compositions|") || lru.Contains("alice|get||compositions|ns1") {
		t.Fatalf("RemoveWithPrefix(alice|) failed to evict alice keys")
	}
	if !lru.Contains("bob|list||compositions|") {
		t.Fatalf("RemoveWithPrefix(alice|) accidentally evicted bob's entry")
	}

	lru.Purge()
	if lru.Len() != 0 {
		t.Fatalf("Purge: len = %d, want 0", lru.Len())
	}
}

// keyFromInt builds a deterministic LRU key for the eviction tests.
// Format mirrors evalCacheKey shape so any future packing assumptions
// stay consistent.
func keyFromInt(i int) string {
	return "user-" + evalLRUItoa(i) + "|list||compositions|"
}

// evalLRUItoa avoids dragging strconv into a tiny test helper.
// Named to avoid colliding with crd_register_evict_test's `itoa` in the
// same package.
func evalLRUItoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// nopCache is a Cache implementation whose only contract here is to make
// invalidate() / purgeUserCacheData() reach the eval-cache eviction code
// without panicking. SMembers / Delete / DeleteUserRBAC return zero-value
// successes; ScanKeys returns an empty slice.
type nopCache struct{ Cache }

func (nopCache) SMembers(_ context.Context, _ string) ([]string, error) { return nil, nil }
func (nopCache) ScanKeys(_ context.Context, _ string) ([]string, error) { return nil, nil }
func (nopCache) Delete(_ context.Context, _ ...string) error            { return nil }
func (nopCache) DeleteUserRBAC(_ context.Context, _ string) error       { return nil }
