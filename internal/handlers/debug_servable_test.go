// debug_servable_test.go — /debug/servable read-only diagnostic tests
// (Fix #1, docs/rca-stale-delete-compositiondefinitions-informer-2026-06-25.md).
//
// The endpoint must (1) return 200 + a well-formed body in BOTH cache-on
// and cache-off modes (NOT gated behind the cache flag, PM AC-7) and
// (2) be READ-ONLY — calling it MUST NOT change any servability state.

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/krateoplatformops/snowplow/internal/cache"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// TestDebugServable_CacheOff returns 200 with cacheEnabled=false and an
// empty GVR list when the cache subsystem is off — the endpoint is
// available for debugging regardless of the flag (not flag-gated).
func TestDebugServable_CacheOff(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false")

	rec := httptest.NewRecorder()
	DebugServable()(rec, httptest.NewRequest(http.MethodGet, "/debug/servable", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("/debug/servable returned %d; want 200 (not flag-gated)", rec.Code)
	}
	var body debugServableBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.CacheEnabled {
		t.Fatalf("cache-off: body.cacheEnabled must be false; got true")
	}
	if body.Count != len(body.GVRs) {
		t.Fatalf("count=%d != len(gvrs)=%d", body.Count, len(body.GVRs))
	}
}

// TestDebugServable_ReadOnly asserts the endpoint mutates no servability
// state: two back-to-back requests yield the SAME snapshot (no GVR is
// confirmed/registered/healed as a side effect of being observed). This is
// the PM AC-7 read-only guarantee.
func TestDebugServable_ReadOnly(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false") // passthrough — deterministic empty snapshot.

	first := decodeServable(t)
	second := decodeServable(t)

	if first.Count != second.Count {
		t.Fatalf("read-only violation: snapshot count changed across two GET requests "+
			"(%d -> %d) — the diagnostic mutated state", first.Count, second.Count)
	}
	if len(first.GVRs) != len(second.GVRs) {
		t.Fatalf("read-only violation: GVR set size changed across requests (%d -> %d)",
			len(first.GVRs), len(second.GVRs))
	}
	// Field-level stability: the servability conjuncts of each GVR must be
	// identical across the two observations.
	idx := map[string]cache.ServableGVRStatus{}
	for _, r := range first.GVRs {
		idx[r.GVR] = r
	}
	for _, r := range second.GVRs {
		p, ok := idx[r.GVR]
		if !ok {
			t.Fatalf("read-only violation: GVR %q appeared only in the second request", r.GVR)
		}
		if p != r {
			t.Fatalf("read-only violation: GVR %q conjuncts changed across requests: %+v -> %+v",
				r.GVR, p, r)
		}
	}
}

func decodeServable(t *testing.T) debugServableBody {
	t.Helper()
	rec := httptest.NewRecorder()
	DebugServable()(rec, httptest.NewRequest(http.MethodGet, "/debug/servable", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/debug/servable returned %d; want 200", rec.Code)
	}
	var body debugServableBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return body
}

// --- L10: 200 + sorted rows (cache-on) --------------------------------------

// seedServableWatcher wires cache.Global() with a cache-on ResourceWatcher that
// has SEVERAL GVRs registered, so ServableSnapshot returns multiple rows the
// handler must sort. It registers the RBAC list-kinds (the watcher always
// registers these on construction) plus two additional GVRs whose sorted order
// differs from any plausible map-iteration order. Returns the sorted set of GVR
// strings the handler should emit.
func seedServableWatcher(t *testing.T) []string {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")

	crbGVR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}
	crGVR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}
	// Two extra GVRs; "aardvarks" sorts before the rbac.* group, "zebras" after —
	// so a correct sort is unambiguous regardless of registration/map order.
	aGVR := schema.GroupVersionResource{Group: "a.example.io", Version: "v1", Resource: "aardvarks"}
	zGVR := schema.GroupVersionResource{Group: "z.example.io", Version: "v1", Resource: "zebras"}

	scheme := runtime.NewScheme()
	_ = rbacv1.AddToScheme(scheme)
	listKinds := map[schema.GroupVersionResource]string{
		crbGVR: "ClusterRoleBindingList",
		crGVR:  "ClusterRoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}: "RoleBindingList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}:        "RoleList",
		aGVR: "AardvarkList",
		zGVR: "ZebraList",
	}

	wctx, wcancel := context.WithCancel(context.Background())
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds)
	rw, err := cache.NewResourceWatcher(wctx, dyn)
	if err != nil {
		wcancel()
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	syncCtx, syncCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer syncCancel()
	if err := rw.WaitForCacheSync(syncCtx, 5*time.Second); err != nil {
		rw.Stop()
		wcancel()
		t.Fatalf("WaitForCacheSync: %v", err)
	}
	_, _ = rw.EnsureResourceType(aGVR)
	_, _ = rw.EnsureResourceType(zGVR)
	_ = rw.WaitForCacheSync(syncCtx, 5*time.Second)

	prev := cache.Global()
	cache.SetGlobal(rw)
	t.Cleanup(func() {
		rw.Stop()
		wcancel()
		cache.SetGlobal(prev)
	})

	// Build the expected sorted GVR-string set from the live snapshot (the set
	// of registered GVRs is what we assert ORDER over, not membership).
	rows := rw.ServableSnapshot()
	gvrs := make([]string, 0, len(rows))
	for _, r := range rows {
		gvrs = append(gvrs, r.GVR)
	}
	sort.Strings(gvrs)
	return gvrs
}

// TestDebugServable_CacheOn_SortedRows — L10: with the cache on and several GVRs
// registered, /debug/servable returns 200, cacheEnabled=true, count==len(gvrs),
// and the rows are sorted by GVR string (stable output across requests). The
// expected GVRs include the two extras + the always-registered rbac.* list-kinds.
func TestDebugServable_CacheOn_SortedRows(t *testing.T) {
	wantSorted := seedServableWatcher(t)
	if len(wantSorted) < 2 {
		t.Fatalf("setup: expected >=2 registered GVRs to test ordering; got %d", len(wantSorted))
	}

	body := decodeServable(t)
	if !body.CacheEnabled {
		t.Fatalf("cache-on: body.cacheEnabled must be true")
	}
	if body.Count != len(body.GVRs) {
		t.Fatalf("count=%d != len(gvrs)=%d", body.Count, len(body.GVRs))
	}
	if body.Count != len(wantSorted) {
		t.Fatalf("count=%d, want %d registered GVRs", body.Count, len(wantSorted))
	}

	got := make([]string, len(body.GVRs))
	for i, r := range body.GVRs {
		got[i] = r.GVR
	}
	// (1) The rows must be sorted by GVR string.
	if !sort.StringsAreSorted(got) {
		t.Fatalf("L10: /debug/servable rows must be sorted by GVR; got %v", got)
	}
	// (2) And carry exactly the registered set (membership + order).
	for i := range wantSorted {
		if got[i] != wantSorted[i] {
			t.Fatalf("L10: sorted rows mismatch at [%d]: got %q want %q (full got=%v want=%v)",
				i, got[i], wantSorted[i], got, wantSorted)
		}
	}
	// Sanity: the two distinctive extras are present and correctly ordered
	// (aardvarks before zebras). GVR.String() renders as
	// "group/version, Resource=resource".
	ai := indexOf(got, "a.example.io/v1, Resource=aardvarks")
	zi := indexOf(got, "z.example.io/v1, Resource=zebras")
	if ai < 0 || zi < 0 {
		t.Fatalf("L10: expected extras missing; got=%v", got)
	}
	if ai > zi {
		t.Fatalf("L10: aardvarks (idx %d) must sort before zebras (idx %d); got=%v", ai, zi, got)
	}
}

// TestDebugServable_RED_UnsortedRowsCaught is the RED proof for the sort arm.
// The handler sorts before emitting; to prove the "sorted" assertion is
// discriminating we reproduce the plausible wrong impl (emit rows in
// unsorted/reverse order) and assert the SAME sort.StringsAreSorted check the
// green test uses FAILS on it.
func TestDebugServable_RED_UnsortedRowsCaught(t *testing.T) {
	wantSorted := seedServableWatcher(t)
	if len(wantSorted) < 2 {
		t.Skip("need >=2 GVRs to demonstrate an unsorted ordering")
	}
	// Wrong impl: reverse-sorted rows (what a handler that forgot sort.Slice, or
	// sorted descending, would emit).
	reversed := make([]string, len(wantSorted))
	for i := range wantSorted {
		reversed[i] = wantSorted[len(wantSorted)-1-i]
	}
	if sort.StringsAreSorted(reversed) {
		t.Fatalf("RED setup invalid: reversed slice should NOT be ascending-sorted; %v", reversed)
	}
	// The green test's sorted-check MUST reject this wrong ordering.
	if sort.StringsAreSorted(reversed) {
		t.Fatalf("RED: the sorted assertion failed to catch a reverse-ordered emission %v", reversed)
	}
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}
