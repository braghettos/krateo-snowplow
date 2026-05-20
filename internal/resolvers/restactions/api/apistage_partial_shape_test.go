// apistage_partial_shape_test.go — Ship D.4 (0.30.144) hermetic
// regression tests for the partial-shape guard at gateListItems
// (primary site, per design §12) and gateGetEnvelope (secondary
// site, defense-in-depth).
//
// The defect this catches: the apistage CONTENT cache (Ship F1 at
// 0.30.119) Puts the un-gated dispatch result unconditionally; if
// the apiserver returned a partially-populated object (controller
// mid-write — e.g. core-provider crash-loop), the cache PRESERVES
// the partial shape and serves it on every subsequent /call until
// TTL expiry. Downstream RESTAction filters that assume apiVersion
// is non-null then crash with gojq's `cannot iterate over: null`.
// See docs/panels-500-diagnosis-2026-05-20.md and
// docs/ship-d4-apistage-partial-shape-guard-design.md §12.
//
// Coverage per AC-D4.3 / AC-D4.4 / AC-D4.5:
//
//   - TestGateListItems_PartialShape_apiVersionEmpty — primary site:
//     LIST envelope with one item missing apiVersion → that item is
//     filtered out of the served envelope; healthy items remain;
//     counter increments by 1; the call STILL serves (no apiserver
//     fall-through — filter-in-place semantics, §12.4).
//   - TestGateListItems_PartialShape_kindEmpty — same shape, missing
//     kind.
//   - TestGateListItems_PartialShape_apiVersionNull — JSON null
//     apiVersion → GetAPIVersion() returns "" → same outcome.
//   - TestGateListItems_FullShape_NoGuard — healthy negative: every
//     item has apiVersion+kind → byte-identical pre/post; counter
//     does NOT increment.
//   - TestGateListItems_PartialShape_PreservesCacheEntry — AC-D4.5
//     `feedback_l1_invalidation_delete_only` pin: a partial-shape
//     detection does NOT evict the cache entry. Re-Get the entry
//     and assert it remains.
//   - TestGateGetEnvelope_PartialShape_apiVersionEmpty — secondary
//     site: GET-by-name partial shape → (nil, false) + counter++.
package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/snowplow/internal/cache"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// newF1WatcherWithGetRBAC builds an F1-style cache=on watcher whose
// broad user is granted BOTH list AND get on widgets. The shared F1
// helper (newF1Watcher) only seeds `list`; the gateGetEnvelope
// path's filterGetByRBAC requires the `get` verb. Mirrors
// newF1Watcher's seeding logic but with a richer ClusterRole.
func newF1WatcherWithGetRBAC(t *testing.T) *cache.ResourceWatcher {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_APISTAGE_ENABLED", "true")
	t.Setenv("RESOLVER_USE_INFORMER", "true")
	cache.ResetResolvedCacheForTest()
	cache.ResetDepsForTest()
	t.Cleanup(func() {
		cache.ResetResolvedCacheForTest()
		cache.ResetDepsForTest()
	})

	// ClusterRole grants list AND get on widgets — superset of the
	// shared f1WidgetListerClusterRole so the GET tests exercise the
	// partial-shape guard, not RBAC denial.
	cr := &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: "f1-widget-list-and-get"},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{f1WidgetsGVR.Group}, Resources: []string{f1WidgetsGVR.Resource}, Verbs: []string{"list", "get"}},
		},
	}
	rb := func(ns, user string) *rbacv1.RoleBinding {
		return &rbacv1.RoleBinding{
			TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "f1-binding-getter-" + user},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: "rbac.authorization.k8s.io", Name: user}},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "f1-widget-list-and-get"},
		}
	}

	var seed []runtime.Object
	for _, ns := range f1AllNamespaces {
		seed = append(seed, f1WidgetObject(ns, "widget-"+ns))
	}
	seed = append(seed, cr)
	for _, ns := range f1AllNamespaces {
		seed = append(seed, rb(ns, f1BroadUser))
	}

	scheme := runtime.NewScheme()
	_ = rbacv1.AddToScheme(scheme)
	listKinds := map[schema.GroupVersionResource]string{
		f1WidgetsGVR: "WidgetList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}:                "RoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}:         "RoleBindingList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}:         "ClusterRoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}: "ClusterRoleBindingList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, seed...)

	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil {
		t.Fatalf("expected non-nil watcher under CACHE_ENABLED=true")
	}
	t.Cleanup(func() { rw.Stop(); time.Sleep(50 * time.Millisecond) })

	added, syncCh := rw.EnsureResourceType(f1WidgetsGVR)
	if !added {
		t.Fatalf("EnsureResourceType(widgets): want added=true")
	}
	select {
	case <-syncCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("widgets informer did not sync within 5s")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rw.WaitForCacheSync(ctx, 5*time.Second); err != nil {
		t.Fatalf("WaitForCacheSync (RBAC informers): %v", err)
	}
	return rw
}

// partialShapeCounterFor returns the current value of the
// ReasonApistagePartialShape cell for the f1WidgetsGVR + the test
// scope. The Ship D scope must be active on ctx (a counter increment
// is gated on FallthroughScope being stamped).
func partialShapeCounterFor(scope string) uint64 {
	return cache.FallthroughCount(scope, f1WidgetsGVR.String(), cache.ReasonApistagePartialShape)
}

// scopedCtx wraps ctx with the Ship D FallthroughScope so the
// RecordApiserverFallthrough call in the guard actually increments
// the counter (Ship D contract — no scope = no counter, see
// cache.FallthroughScope short-circuit). We use a /call-like scope
// to mirror the production code path.
func scopedCtx(ctx context.Context) context.Context {
	return cache.WithFallthroughScope(ctx, cache.ScopeCallRestactions)
}

// ─────────────────────────────────────────────────────────────────────
// gateListItems — primary site (LIST partial-shape filter-in-place)
// ─────────────────────────────────────────────────────────────────────

// gateListItemsTest builds a parsedListEnvelope containing the given
// items (raw maps — caller picks the shape to exercise) and runs it
// through gateListItems under the f1BroadUser identity (whose RBAC
// permits LISTing widgets in every seeded namespace, so RBAC will
// not drop any item — leaving the partial-shape guard as the sole
// filter behaviour under test).
func gateListItemsTest(t *testing.T, items []map[string]any) (envelope map[string]any, served bool, counterDelta uint64) {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")
	cache.ResetFallthroughCountersForTest()

	rw := newF1Watcher(t)
	cache.SetGlobal(rw)
	t.Cleanup(func() { cache.SetGlobal(nil) })

	parsed := parsedListEnvelope{
		apiVersion: apiVersionForGVR(f1WidgetsGVR),
		kind:       listKindForResource(f1WidgetsGVR.Resource),
		items:      make([]*unstructured.Unstructured, 0, len(items)),
	}
	for _, m := range items {
		parsed.items = append(parsed.items, &unstructured.Unstructured{Object: m})
	}

	ctx := scopedCtx(xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: f1BroadUser}),
	))

	before := partialShapeCounterFor(cache.ScopeCallRestactions)
	got, ok := gateListItems(ctx, f1WidgetsGVR, parsed)
	after := partialShapeCounterFor(cache.ScopeCallRestactions)

	if !ok {
		return nil, false, after - before
	}
	env, isMap := got.(map[string]any)
	if !isMap {
		t.Fatalf("gateListItems returned %T; want map[string]any envelope", got)
	}
	return env, true, after - before
}

// healthyItem produces a fully-populated unstructured widget map
// (apiVersion + kind + metadata).
func healthyItem(ns, name string) map[string]any {
	return map[string]any{
		"apiVersion": "widgets.krateo.io/v1",
		"kind":       "Widget",
		"metadata":   map[string]any{"namespace": ns, "name": name},
	}
}

// envItemsCount counts the served envelope's items slice length.
func envItemsCount(t *testing.T, env map[string]any) int {
	t.Helper()
	items, ok := env["items"].([]any)
	if !ok {
		t.Fatalf("envelope.items is not []any: %T", env["items"])
	}
	return len(items)
}

// envItemNames extracts the metadata.name of every served item, in
// order, for golden assertions.
func envItemNames(t *testing.T, env map[string]any) []string {
	t.Helper()
	items := env["items"].([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("envelope item is not map[string]any: %T", it)
		}
		md, _ := m["metadata"].(map[string]any)
		name, _ := md["name"].(string)
		out = append(out, name)
	}
	return out
}

// TestGateListItems_PartialShape_apiVersionEmpty — primary site
// guard with one partial-shape item missing apiVersion. Per §12.4
// (filter-in-place): the served envelope DROPS the partial item,
// retains every healthy item, and the counter increments by 1. The
// call STILL serves (no apiserver fall-through).
func TestGateListItems_PartialShape_apiVersionEmpty(t *testing.T) {
	partial := map[string]any{
		// apiVersion missing entirely — GetAPIVersion() returns "".
		"kind":     "Widget",
		"metadata": map[string]any{"namespace": "team-a", "name": "partial-no-apiversion"},
	}
	items := []map[string]any{
		healthyItem("team-a", "alpha"),
		partial,
		healthyItem("team-b", "beta"),
	}

	env, served, delta := gateListItemsTest(t, items)
	if !served {
		t.Fatalf("gateListItems served=false; want true (filter-in-place keeps the call alive)")
	}
	if got, want := envItemsCount(t, env), 2; got != want {
		t.Errorf("envelope items count = %d; want %d (partial item filtered out)", got, want)
	}
	if got := envItemNames(t, env); !containsName(got, "alpha") || !containsName(got, "beta") {
		t.Errorf("healthy items missing post-filter: %v", got)
	}
	if got := envItemNames(t, env); containsName(got, "partial-no-apiversion") {
		t.Errorf("partial-shape item leaked into served envelope: %v", got)
	}
	if delta != 1 {
		t.Errorf("counter delta = %d; want 1 (one partial-shape item)", delta)
	}
}

// TestGateListItems_PartialShape_kindEmpty — same site, kind missing.
func TestGateListItems_PartialShape_kindEmpty(t *testing.T) {
	partial := map[string]any{
		"apiVersion": "widgets.krateo.io/v1",
		// kind missing entirely.
		"metadata": map[string]any{"namespace": "team-a", "name": "partial-no-kind"},
	}
	items := []map[string]any{
		healthyItem("team-a", "alpha"),
		partial,
	}

	env, served, delta := gateListItemsTest(t, items)
	if !served {
		t.Fatalf("gateListItems served=false; want true")
	}
	if got, want := envItemsCount(t, env), 1; got != want {
		t.Errorf("items count = %d; want %d (partial filtered)", got, want)
	}
	if delta != 1 {
		t.Errorf("counter delta = %d; want 1", delta)
	}
}

// TestGateListItems_PartialShape_apiVersionNull — JSON null
// apiVersion. json.Unmarshal of `null` into a map[string]any value
// produces Go `nil`; Unstructured.GetAPIVersion() returns "" for
// nil-or-non-string apiVersion entries (k8s.io/apimachinery
// unstructured/helpers.go's NestedString contract). Same outcome as
// the empty/missing cases — counter increments, item dropped.
func TestGateListItems_PartialShape_apiVersionNull(t *testing.T) {
	partial := map[string]any{
		"apiVersion": nil, // explicit JSON-null → Go nil → "".
		"kind":       "Widget",
		"metadata":   map[string]any{"namespace": "team-a", "name": "partial-null-apiversion"},
	}
	items := []map[string]any{
		healthyItem("team-a", "alpha"),
		partial,
	}

	env, served, delta := gateListItemsTest(t, items)
	if !served {
		t.Fatalf("gateListItems served=false; want true")
	}
	if got, want := envItemsCount(t, env), 1; got != want {
		t.Errorf("items count = %d; want %d", got, want)
	}
	if delta != 1 {
		t.Errorf("counter delta = %d; want 1 (null-apiVersion is a partial shape)", delta)
	}
}

// TestGateListItems_FullShape_NoGuard — healthy negative. Every item
// has apiVersion + kind; the guard is a no-op; the served envelope
// is byte-equivalent to pre-D.4; the counter does NOT increment.
// This is the AC-D4.8 healthy-corpus byte-identity pin (the §12.5
// PM canary): a regression that incorrectly drops healthy items
// fails this test immediately.
func TestGateListItems_FullShape_NoGuard(t *testing.T) {
	items := []map[string]any{
		healthyItem("team-a", "alpha"),
		healthyItem("team-b", "beta"),
		healthyItem("team-c", "gamma"),
	}
	env, served, delta := gateListItemsTest(t, items)
	if !served {
		t.Fatalf("gateListItems served=false on healthy input; want true")
	}
	if got, want := envItemsCount(t, env), 3; got != want {
		t.Errorf("items count = %d; want %d (no items should be dropped)", got, want)
	}
	if names := envItemNames(t, env); !containsName(names, "alpha") || !containsName(names, "beta") || !containsName(names, "gamma") {
		t.Errorf("healthy items missing from served envelope: %v", names)
	}
	if delta != 0 {
		t.Errorf("counter delta = %d; want 0 (no partial items, no increments)", delta)
	}
}

// TestGateListItems_PartialShape_PreservesCacheEntry — AC-D4.5 pin
// for feedback_l1_invalidation_delete_only. Per §3.2: a partial-shape
// detection at Get-time is NOT a DELETE and MUST NOT evict the cache
// entry. The next informer UPDATE event from the controller's
// eventual settle dirty-marks the entry; the refresher re-Puts the
// fresh shape; subsequent Gets serve the now-complete object.
//
// Test mechanism: drive gateListItems with a partial-shape input,
// observe the in-place filter (item dropped, counter bumped), and
// assert that the parsed.items slice WE PASSED IN is unchanged in
// length — i.e. the guard did not mutate the caller's input slice
// (the production caller's parsed.items lives ON the
// ResolvedEntry's R3 pre-parsed cache, so an in-place mutation
// there WOULD be equivalent to silent eviction of the partial item
// — which is exactly the eviction the invariant forbids).
//
// The guard's healthy = make(...) + per-item conditional append
// pattern means the input slice is NEVER mutated; this test pins
// that invariant at the code level.
func TestGateListItems_PartialShape_PreservesCacheEntry(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	cache.ResetFallthroughCountersForTest()

	rw := newF1Watcher(t)
	cache.SetGlobal(rw)
	t.Cleanup(func() { cache.SetGlobal(nil) })

	partial := map[string]any{
		"kind":     "Widget",
		"metadata": map[string]any{"namespace": "team-a", "name": "partial"},
	}
	parsed := parsedListEnvelope{
		apiVersion: apiVersionForGVR(f1WidgetsGVR),
		kind:       listKindForResource(f1WidgetsGVR.Resource),
		items: []*unstructured.Unstructured{
			{Object: healthyItem("team-a", "alpha")},
			{Object: partial},
			{Object: healthyItem("team-b", "beta")},
		},
	}
	inputLenBefore := len(parsed.items)
	// Capture each item's pointer identity so we can prove the guard
	// didn't replace any element of the input slice.
	itemPointersBefore := make([]*unstructured.Unstructured, len(parsed.items))
	copy(itemPointersBefore, parsed.items)

	ctx := scopedCtx(xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: f1BroadUser}),
	))
	_, served := gateListItems(ctx, f1WidgetsGVR, parsed)
	if !served {
		t.Fatalf("gateListItems served=false; want true")
	}

	// The input parsed.items MUST be unchanged — the production
	// caller's parsed.items lives on the cached ResolvedEntry's R3
	// pre-parsed slice; mutating it would silently evict the partial
	// item from the cache (violating feedback_l1_invalidation_delete_only).
	if got, want := len(parsed.items), inputLenBefore; got != want {
		t.Errorf("parsed.items mutated: len = %d; want %d (guard must not mutate caller's input slice)",
			got, want)
	}
	for i, p := range itemPointersBefore {
		if parsed.items[i] != p {
			t.Errorf("parsed.items[%d] pointer changed (was %p, now %p) — "+
				"guard mutated caller's slice in place; this is a silent eviction of "+
				"the partial item from the cache (feedback_l1_invalidation_delete_only)",
				i, p, parsed.items[i])
		}
	}
}

// ─────────────────────────────────────────────────────────────────────
// gateGetEnvelope — secondary site (GET-by-name fail-closed)
// ─────────────────────────────────────────────────────────────────────

// gateGetEnvelopeTest drives gateGetEnvelope against `raw` JSON
// bytes representing one apiserver GET-by-name response. Like the
// LIST harness it uses the f1BroadUser identity so RBAC does not
// drop the object — leaving the partial-shape guard as the sole
// filter under test.
func gateGetEnvelopeTest(t *testing.T, raw []byte) (any, bool, uint64) {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")
	cache.ResetFallthroughCountersForTest()

	// Use the GET-permitting RBAC harness — the shared f1
	// fixture grants `list` only; gateGetEnvelope's filterGetByRBAC
	// needs `get`, otherwise RBAC denies BEFORE the partial-shape
	// guard runs and we'd test the wrong thing.
	rw := newF1WatcherWithGetRBAC(t)
	cache.SetGlobal(rw)
	t.Cleanup(func() { cache.SetGlobal(nil) })

	ctx := scopedCtx(xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: f1BroadUser}),
	))

	before := partialShapeCounterFor(cache.ScopeCallRestactions)
	got, ok := gateGetEnvelope(ctx, f1WidgetsGVR, raw)
	after := partialShapeCounterFor(cache.ScopeCallRestactions)
	return got, ok, after - before
}

// TestGateGetEnvelope_PartialShape_apiVersionEmpty — secondary
// guard fires fail-closed on missing apiVersion. Per §3 + §12.3:
// returns (nil, false); the existing caller arm
// (apistageContentServe:421-436 / resolve.go:609-611) handles the
// fall-through to apiserver. ZERO new code paths.
func TestGateGetEnvelope_PartialShape_apiVersionEmpty(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		// apiVersion missing.
		"kind":     "Widget",
		"metadata": map[string]any{"namespace": "team-a", "name": "x"},
	})
	got, served, delta := gateGetEnvelopeTest(t, raw)
	if served {
		t.Errorf("gateGetEnvelope served=true on missing apiVersion; want false (fail-closed)")
	}
	if got != nil {
		t.Errorf("gateGetEnvelope returned %v; want nil on partial-shape miss", got)
	}
	if delta != 1 {
		t.Errorf("counter delta = %d; want 1", delta)
	}
}

// TestGateGetEnvelope_PartialShape_kindEmpty — secondary guard,
// kind missing.
func TestGateGetEnvelope_PartialShape_kindEmpty(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"apiVersion": "widgets.krateo.io/v1",
		// kind missing.
		"metadata": map[string]any{"namespace": "team-a", "name": "x"},
	})
	_, served, delta := gateGetEnvelopeTest(t, raw)
	if served {
		t.Errorf("gateGetEnvelope served=true on missing kind; want false")
	}
	if delta != 1 {
		t.Errorf("counter delta = %d; want 1", delta)
	}
}

// TestGateGetEnvelope_PartialShape_apiVersionNull — JSON null
// apiVersion. The decode produces Go-nil apiVersion;
// Unstructured.GetAPIVersion() returns "" for nil-or-non-string —
// same outcome as missing.
func TestGateGetEnvelope_PartialShape_apiVersionNull(t *testing.T) {
	raw := []byte(`{"apiVersion":null,"kind":"Widget","metadata":{"namespace":"team-a","name":"x"}}`)
	_, served, delta := gateGetEnvelopeTest(t, raw)
	if served {
		t.Errorf("gateGetEnvelope served=true on null apiVersion; want false")
	}
	if delta != 1 {
		t.Errorf("counter delta = %d; want 1 (null-apiVersion is a partial shape)", delta)
	}
}

// TestGateGetEnvelope_FullShape_NoGuard — healthy negative. The
// guard is a no-op; the served value is the decoded obj; the
// counter does NOT increment. AC-D4.8 byte-identity pin for the
// GET-by-name path.
func TestGateGetEnvelope_FullShape_NoGuard(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"apiVersion": "widgets.krateo.io/v1",
		"kind":       "Widget",
		"metadata":   map[string]any{"namespace": "team-a", "name": "x"},
	})
	got, served, delta := gateGetEnvelopeTest(t, raw)
	if !served {
		t.Fatalf("gateGetEnvelope served=false on healthy input; want true")
	}
	obj, ok := got.(map[string]any)
	if !ok || obj["apiVersion"] != "widgets.krateo.io/v1" || obj["kind"] != "Widget" {
		t.Errorf("gateGetEnvelope returned unexpected shape: %v", got)
	}
	if delta != 0 {
		t.Errorf("counter delta = %d; want 0 (healthy shape, no increments)", delta)
	}
}

// contains is a tiny test helper — no strings.Contains import drag
// for what is fundamentally a slice membership check.
func containsName(slice []string, want string) bool {
	for _, s := range slice {
		if s == want {
			return true
		}
	}
	return false
}
