// prewarm_test.go — Tag 0.30.99 (Tag B) tests for the startup
// navigation GVR-walk (PrewarmRegisterFromNavigation).
//
// The walk's contract:
//   - It registers an informer for every GVR the cluster's RESTActions
//     reach via spec.api[*].path — a navigation-derived, zero-hardcoded
//     GVR set.
//   - It is FIRE-AND-FORGET: it returns without blocking on informer
//     sync. A request landing before an informer syncs falls through to
//     apiserver via Tag A's four-conjunct servable() gate.
//   - It is idempotent against the lazy register-on-navigation paths: a
//     GVR it registers reports added=false on a later EnsureResourceType.
//
// Per feedback_no_special_cases.md: every GVR exercised here is a
// generic customer-style resource derived from a test RESTAction's
// path — there is no hardcoded business-GVR list under test.

package cache_test

import (
	"context"
	"testing"
	"time"

	"github.com/krateoplatformops/snowplow/internal/cache"

	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// TestPrewarmRegisterFromNavigation_RegistersInventoryGVRs is the Part 2
// acceptance test. It seeds the fake cluster with RESTActions whose
// spec.api[*].path entries reach three distinct GVRs, runs the startup
// walk, and asserts every navigation-derived GVR ends up registered.
func TestPrewarmRegisterFromNavigation_RegistersInventoryGVRs(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	// Two RESTActions reaching namespaces + pods + deployments. The
	// inventory walker dedupes — three distinct GVRs total.
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		buildSchemeWithRestActions(),
		inventoryListKinds(),
		makeRestAction("ra-nav-1", "demo",
			"/api/v1/namespaces",
			"/api/v1/namespaces/kube-system/pods",
		),
		makeRestAction("ra-nav-2", "demo",
			"/apis/apps/v1/deployments",
		),
	)

	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	t.Cleanup(func() {
		rw.Stop()
		time.Sleep(50 * time.Millisecond)
	})

	navGVRs := []schema.GroupVersionResource{
		{Group: "", Version: "v1", Resource: "namespaces"},
		{Group: "", Version: "v1", Resource: "pods"},
		{Group: "apps", Version: "v1", Resource: "deployments"},
	}

	// Pre-condition: none of the navigation GVRs is registered (only the
	// RBAC bootstrap set is). EnsureResourceType would report added=true
	// for each — we do NOT call it here; the walk must do the work.
	for _, gvr := range navGVRs {
		if rw.IsSynced(gvr) {
			t.Fatalf("pre-condition: %s should not be registered before the walk", gvr)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	registered, alreadyPresent, walkErr := rw.PrewarmRegisterFromNavigation(ctx, dyn)
	if walkErr != nil {
		t.Fatalf("PrewarmRegisterFromNavigation: %v", walkErr)
	}

	// All three navigation GVRs are distinct and never registered before
	// the walk → registered==3, alreadyPresent==0.
	if registered != 3 {
		t.Fatalf("walk: want registered=3, got %d (alreadyPresent=%d)", registered, alreadyPresent)
	}
	if alreadyPresent != 0 {
		t.Fatalf("walk: want alreadyPresent=0, got %d", alreadyPresent)
	}

	// Every navigation GVR is now registered: EnsureResourceType reports
	// added=false (the walk already registered it). This is the
	// idempotence property that makes the walk + the lazy
	// register-on-navigation paths coexist safely.
	for _, gvr := range navGVRs {
		added, syncCh := rw.EnsureResourceType(gvr)
		if added {
			t.Fatalf("walk did not register %s — EnsureResourceType reports added=true", gvr)
		}
		// The walk is fire-and-forget, but the informers do sync
		// asynchronously over the (empty) fake apiserver — wait so the
		// test's cleanup does not race a half-synced informer.
		select {
		case <-syncCh:
		case <-time.After(5 * time.Second):
			t.Fatalf("informer for %s did not sync within 5s", gvr)
		}
	}
}

// TestPrewarmRegisterFromNavigation_FireAndForget asserts the walk does
// NOT block on informer sync. The walk must return promptly; the
// informers it registered may still be syncing afterward. We assert the
// walk returns well under a WaitForCacheSync-class timeout.
func TestPrewarmRegisterFromNavigation_FireAndForget(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		buildSchemeWithRestActions(),
		inventoryListKinds(),
		makeRestAction("ra-faf", "demo",
			"/api/v1/namespaces",
			"/api/v1/pods",
			"/apis/apps/v1/deployments",
		),
	)

	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	t.Cleanup(func() {
		rw.Stop()
		time.Sleep(50 * time.Millisecond)
	})

	start := time.Now()
	_, _, walkErr := rw.PrewarmRegisterFromNavigation(context.Background(), dyn)
	elapsed := time.Since(start)
	if walkErr != nil {
		t.Fatalf("PrewarmRegisterFromNavigation: %v", walkErr)
	}

	// The walk is registration-only: a LIST of RESTActions + N
	// EnsureResourceType calls (each sub-millisecond). It must not block
	// on the informers' initial LISTs syncing — that would be a
	// WaitForCacheSync-class wait (seconds at scale). 2s is a generous
	// ceiling for the fake-client registration path.
	if elapsed > 2*time.Second {
		t.Fatalf("walk blocked too long (%s) — fire-and-forget contract broken; "+
			"did it WaitForCacheSync?", elapsed)
	}
}

// TestPrewarmRegisterFromNavigation_NilDynClient asserts the defensive
// no-op: a nil dynamic client cannot be walked. The walk must return
// (0, 0, nil) without panicking — the lazy register-on-navigation
// fallback still covers every GVR on first request.
func TestPrewarmRegisterFromNavigation_NilDynClient(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		buildSchemeWithRestActions(), inventoryListKinds())
	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	t.Cleanup(func() {
		rw.Stop()
		time.Sleep(50 * time.Millisecond)
	})

	registered, alreadyPresent, walkErr := rw.PrewarmRegisterFromNavigation(
		context.Background(), nil)
	if walkErr != nil {
		t.Fatalf("nil dynClient: want nil error, got %v", walkErr)
	}
	if registered != 0 || alreadyPresent != 0 {
		t.Fatalf("nil dynClient: want (0,0), got (%d,%d)", registered, alreadyPresent)
	}
}

// TestPrewarmRegisterFromNavigation_NilReceiver asserts the walk is
// nil-receiver safe — cache.Global() returns nil under CACHE_ENABLED
// =false, and callers should not need to nil-check before calling.
func TestPrewarmRegisterFromNavigation_NilReceiver(t *testing.T) {
	var rw *cache.ResourceWatcher
	registered, alreadyPresent, walkErr := rw.PrewarmRegisterFromNavigation(
		context.Background(), nil)
	if walkErr != nil || registered != 0 || alreadyPresent != 0 {
		t.Fatalf("nil receiver: want (0,0,nil), got (%d,%d,%v)",
			registered, alreadyPresent, walkErr)
	}
}
