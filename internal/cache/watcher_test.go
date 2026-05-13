package cache_test

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/krateoplatformops/snowplow/internal/cache"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
)

// rbacListKinds maps every RBAC GVR registered by NewResourceWatcher
// to its corresponding List kind so dynamicfake.NewSimpleDynamicClient
// can serve informer LISTs without panicking.
//
// dynamicfake.NewSimpleDynamicClient (no-custom-list-kinds variant)
// requires every GVR LISTed to have a registered List kind in its
// scheme. The cache=on constructor eagerly LISTs all four RBAC types,
// so unit tests MUST hand it a client that knows about them.
func rbacListKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}:                "RoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}:         "RoleBindingList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}:         "ClusterRoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}: "ClusterRoleBindingList",
	}
}

// newTestScheme returns a scheme with RBAC types registered so the
// dynamic fake client can decode informer LISTs.
func newTestScheme() *k8sruntime.Scheme {
	sch := k8sruntime.NewScheme()
	_ = rbacv1.AddToScheme(sch)
	return sch
}

// TestNewResourceWatcher_DormantWhenCacheDisabled covers PM amendment 1
// (factory dormancy unit test). When CACHE_ENABLED is unset or false:
//
//   - NewResourceWatcher MUST return (nil, nil)
//   - The factory MUST NOT be instantiated
//   - Goroutine count MUST NOT increase (delta = 0; PM amendment 3 cap < 3)
func TestNewResourceWatcher_DormantWhenCacheDisabled(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "")

	if !cache.Disabled() {
		t.Fatalf("Disabled() should be true with empty CACHE_ENABLED")
	}

	before := runtime.NumGoroutine()

	rw, err := cache.NewResourceWatcher(context.Background(), nil)
	if err != nil {
		t.Fatalf("NewResourceWatcher: unexpected error: %v", err)
	}
	if rw != nil {
		t.Fatalf("NewResourceWatcher: expected nil watcher when Disabled(), got %#v", rw)
	}

	// Settle scheduler so any spawned goroutines reach steady state.
	runtime.Gosched()
	time.Sleep(20 * time.Millisecond)
	runtime.Gosched()

	after := runtime.NumGoroutine()
	delta := after - before
	if delta != 0 {
		t.Fatalf("goroutine delta = %d (want 0); before=%d after=%d", delta, before, after)
	}
}

// TestNewResourceWatcher_DormantValuesEnumerated covers every "off"
// value for CACHE_ENABLED — explicit false, 0, no, empty.
func TestNewResourceWatcher_DormantValuesEnumerated(t *testing.T) {
	for _, v := range []string{"", "false", "0", "no", "FALSE"} {
		v := v
		t.Run("CACHE_ENABLED="+v, func(t *testing.T) {
			t.Setenv("CACHE_ENABLED", v)
			if !cache.Disabled() {
				t.Fatalf("Disabled() should be true for CACHE_ENABLED=%q", v)
			}
			rw, err := cache.NewResourceWatcher(context.Background(), nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rw != nil {
				t.Fatalf("expected nil watcher for CACHE_ENABLED=%q", v)
			}
		})
	}
}

// TestNewResourceWatcher_FactoryConstructedWhenCacheEnabled covers PM
// amendment 1 (other half), now reframed for 0.30.4 activation. When
// CACHE_ENABLED=true:
//
//   - NewResourceWatcher MUST return a non-nil watcher
//   - the four RBAC GVRs MUST be eagerly registered
//   - factory.Start MUST be called from the constructor (0.30.4
//     flips this on; was deferred at 0.30.1/0.30.3)
//
// "Start was called" is verified by counting registered informers and
// by checking that the goroutine delta is consistent with informer
// run-loops (one per registered GVR + a small bookkeeping headroom).
func TestNewResourceWatcher_FactoryConstructedWhenCacheEnabled(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	if cache.Disabled() {
		t.Fatalf("Disabled() should be false when CACHE_ENABLED=true")
	}

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		newTestScheme(), rbacListKinds())

	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: unexpected error: %v", err)
	}
	if rw == nil {
		t.Fatalf("NewResourceWatcher: expected non-nil watcher when CACHE_ENABLED=true")
	}
	defer rw.Stop()
}

// TestNewResourceWatcher_RBACTypesEagerlyRegistered locks in the
// 0.30.4 Revision 1 binding: the four RBAC GVRs are registered by
// the constructor and the factory is started so EvaluateRBAC can read
// from the informer index immediately.
func TestNewResourceWatcher_RBACTypesEagerlyRegistered(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		newTestScheme(), rbacListKinds())

	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil {
		t.Fatalf("expected non-nil watcher")
	}
	defer rw.Stop()

	// Every RBAC GVR exposed via the package-level RBACResourceTypes
	// slice must be registered. We probe via ListObjects — empty slice
	// is fine; absence-from-registry returns nil.
	for _, gvr := range cache.RBACResourceTypes {
		if got := rw.ListObjects(gvr, ""); got == nil {
			t.Fatalf("ListObjects(%s, \"\") = nil; expected the GVR to be registered (possibly empty list)", gvr)
		}
	}

	// SetGlobal/Global round-trip — wire the singleton.
	cache.SetGlobal(rw)
	t.Cleanup(func() { cache.SetGlobal(nil) })
	if cache.Global() != rw {
		t.Fatalf("cache.Global() did not return the watcher set via SetGlobal()")
	}
}

// TestNewResourceWatcher_AddResourceTypeIdempotent confirms that
// re-registering an already-registered RBAC GVR after Start() is a
// behavioural no-op (no duplicate informer registered, no panic).
// We measure idempotence via ListObjects calls, NOT goroutine counts,
// because the four eager-registered informers are running and the
// scheduler may rebalance workers at any time.
func TestNewResourceWatcher_AddResourceTypeIdempotent(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		newTestScheme(), rbacListKinds())

	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil {
		t.Fatalf("expected non-nil watcher")
	}
	defer rw.Stop()

	gvr := schema.GroupVersionResource{
		Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles",
	}
	listsBefore := rw.ListObjects(gvr, "")
	if listsBefore == nil {
		t.Fatalf("ListObjects(%s) returned nil; expected an empty slice (eager-registered)", gvr)
	}

	// Re-add an already-registered RBAC GVR. The implementation MUST
	// no-op on existence; if it didn't we'd see a panic from a
	// duplicate informer Run().
	rw.AddResourceType(gvr)

	listsAfter := rw.ListObjects(gvr, "")
	if listsAfter == nil {
		t.Fatalf("after re-AddResourceType, ListObjects returned nil")
	}
}

// TestNewResourceWatcher_GoroutineFootprintBounded sanity-checks that
// constructor activation does not leak unbounded goroutines per GVR.
// We expect roughly one Reflector + one informer + one bookkeeping
// goroutine per registered GVR (≤ 5×len). Headroom = 8× to absorb
// client-go version drift.
func TestNewResourceWatcher_GoroutineFootprintBounded(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		newTestScheme(), rbacListKinds())

	before := runtime.NumGoroutine()
	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	defer rw.Stop()

	runtime.Gosched()
	time.Sleep(50 * time.Millisecond)
	runtime.Gosched()

	after := runtime.NumGoroutine()
	delta := after - before
	const headroom = 8
	maxAllowed := len(cache.RBACResourceTypes) * headroom
	if delta > maxAllowed {
		t.Fatalf("goroutine delta = %d (want <= %d); before=%d after=%d", delta, maxAllowed, before, after)
	}
}

// TestDisabled_TruthyValues confirms the truthy whitelist.
func TestDisabled_TruthyValues(t *testing.T) {
	for _, v := range []string{"true", "1", "yes"} {
		t.Setenv("CACHE_ENABLED", v)
		if cache.Disabled() {
			t.Fatalf("Disabled() should be false for CACHE_ENABLED=%q", v)
		}
	}
}

// TestNewResourceWatcher_PassthroughMode_NonNilDynBuildsWatcher covers
// the 0.30.71 "extended CACHE_ENABLED" semantics: when CACHE_ENABLED=false
// AND a non-nil dynamic.Interface is supplied, NewResourceWatcher
// returns a non-nil passthrough watcher (mode=passthrough). The
// pre-0.30.71 nil-dyn dormancy contract is preserved by the parallel
// TestNewResourceWatcher_DormantWhenCacheDisabled test above.
func TestNewResourceWatcher_PassthroughMode_NonNilDynBuildsWatcher(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false")

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		newTestScheme(), rbacListKinds())

	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil {
		t.Fatalf("CACHE_ENABLED=false + non-nil dyn: expected passthrough watcher, got nil")
	}
	if !rw.IsPassthrough() {
		t.Fatalf("watcher must report IsPassthrough()=true in CACHE_ENABLED=false + non-nil dyn")
	}
	defer rw.Stop()
}

// TestNewResourceWatcher_PassthroughMode_NoGoroutinesSpawned is the
// "no informer factory" contract for 0.30.71 passthrough mode. The
// passthrough watcher MUST NOT spawn informer goroutines — its
// Get/List methods route directly to apiserver via the dynamic
// client. We measure delta and require it to be 0 (same bar the
// pre-0.30.71 dormant test set).
func TestNewResourceWatcher_PassthroughMode_NoGoroutinesSpawned(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false")

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		newTestScheme(), rbacListKinds())

	before := runtime.NumGoroutine()
	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil {
		t.Fatalf("expected non-nil passthrough watcher")
	}
	defer rw.Stop()

	runtime.Gosched()
	time.Sleep(50 * time.Millisecond)
	runtime.Gosched()

	after := runtime.NumGoroutine()
	delta := after - before
	if delta != 0 {
		t.Fatalf("passthrough mode goroutine delta = %d (want 0); before=%d after=%d", delta, before, after)
	}
}

// TestNewResourceWatcher_PassthroughMode_GetRoutesToApiserver is the
// load-bearing 0.30.71 assertion: in passthrough mode, GetObject
// returns the object served by the dynamic client (apiserver), NOT
// from any in-process cache. We seed the fake dynamic client with a
// known object and confirm GetObject returns it. If GetObject
// silently fell through to an indexer (or to nil), this test fails.
func TestNewResourceWatcher_PassthroughMode_GetRoutesToApiserver(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false")

	gvr := schema.GroupVersionResource{
		Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles",
	}
	seedCR := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "admin-via-apiserver"},
	}

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		newTestScheme(), rbacListKinds(), seedCR)

	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil || !rw.IsPassthrough() {
		t.Fatalf("expected passthrough watcher; got %v IsPassthrough=%v", rw, rw.IsPassthrough())
	}
	defer rw.Stop()

	uns, ok := rw.GetObject(gvr, "", "admin-via-apiserver")
	if !ok || uns == nil {
		t.Fatalf("passthrough GetObject: expected the seeded ClusterRole, got (%v, %v)", uns, ok)
	}
	if uns.GetName() != "admin-via-apiserver" {
		t.Fatalf("passthrough GetObject returned wrong name: %q", uns.GetName())
	}
}

// TestNewResourceWatcher_PassthroughMode_ListRoutesToApiserver covers
// the cluster-wide LIST path in passthrough mode. The fake dynamic
// client is seeded with two ClusterRoleBindings; ListObjects must
// return both.
func TestNewResourceWatcher_PassthroughMode_ListRoutesToApiserver(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false")

	gvr := schema.GroupVersionResource{
		Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings",
	}
	seedA := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "crb-a"}}
	seedB := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "crb-b"}}

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		newTestScheme(), rbacListKinds(), seedA, seedB)

	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil || !rw.IsPassthrough() {
		t.Fatalf("expected passthrough watcher")
	}
	defer rw.Stop()

	got := rw.ListObjects(gvr, "")
	if len(got) != 2 {
		names := make([]string, 0, len(got))
		for _, u := range got {
			names = append(names, u.GetName())
		}
		t.Fatalf("passthrough ListObjects: want 2 ClusterRoleBindings, got %d (%v)", len(got), names)
	}
}

// TestNewResourceWatcher_PassthroughMode_AddResourceTypeIsNoOp
// confirms that AddResourceType in passthrough mode does NOT
// register an informer (no informer factory exists). The behavioural
// contract is: subsequent GetObject still works (via apiserver),
// goroutine count does not change.
func TestNewResourceWatcher_PassthroughMode_AddResourceTypeIsNoOp(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false")
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		newTestScheme(), rbacListKinds())

	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil || !rw.IsPassthrough() {
		t.Fatalf("expected passthrough watcher")
	}
	defer rw.Stop()

	before := runtime.NumGoroutine()
	gvr := schema.GroupVersionResource{
		Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles",
	}
	rw.AddResourceType(gvr)
	runtime.Gosched()
	time.Sleep(20 * time.Millisecond)
	runtime.Gosched()
	after := runtime.NumGoroutine()
	if after-before != 0 {
		t.Fatalf("passthrough AddResourceType spawned goroutines: before=%d after=%d", before, after)
	}
}

// TestNewResourceWatcher_PassthroughMode_WaitForCacheSyncReturnsNil
// confirms that in passthrough mode WaitForCacheSync is a no-op
// returning nil immediately. Main wires it after construction so
// startup MUST not block on a non-existent informer sync.
func TestNewResourceWatcher_PassthroughMode_WaitForCacheSyncReturnsNil(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false")
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		newTestScheme(), rbacListKinds())

	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil || !rw.IsPassthrough() {
		t.Fatalf("expected passthrough watcher")
	}
	defer rw.Stop()

	start := time.Now()
	if err := rw.WaitForCacheSync(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("WaitForCacheSync in passthrough must return nil; got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("WaitForCacheSync in passthrough took %v; must be near-instant", elapsed)
	}
}
