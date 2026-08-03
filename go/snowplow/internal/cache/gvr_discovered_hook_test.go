// gvr_discovered_hook_test.go — Ship 2 Stage 2 / 0.30.247. Unit tests
// for the cache-side GVR-discovered hook registry. Package cache so
// the tests reach the unexported notifyGVRDiscoveredForReprewarm.
//
// NON-DESTRUCTIVE — pure in-process registry state, reset before each
// test via ResetGVRDiscoveredHooksForTest.

package cache

import (
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestGVRDiscoveredHook_FiresWithPayload(t *testing.T) {
	ResetGVRDiscoveredHooksForTest()
	defer ResetGVRDiscoveredHooksForTest()

	var got atomic.Value
	RegisterGVRDiscoveredHook(func(gvr schema.GroupVersionResource) {
		got.Store(gvr)
	})

	want := schema.GroupVersionResource{
		Group:    "composition.krateo.io",
		Version:  "v1-2-2",
		Resource: "githubscaffoldingwithcompositionpages",
	}
	notifyGVRDiscoveredForReprewarm(want)

	v := got.Load()
	if v == nil {
		t.Fatal("hook did not fire")
	}
	if v.(schema.GroupVersionResource) != want {
		t.Fatalf("hook payload mismatch: got %+v want %+v", v, want)
	}
}

func TestGVRDiscoveredHook_IdempotentRegistration(t *testing.T) {
	ResetGVRDiscoveredHooksForTest()
	defer ResetGVRDiscoveredHooksForTest()

	var calls atomic.Int64
	fn := func(gvr schema.GroupVersionResource) { calls.Add(1) }

	RegisterGVRDiscoveredHook(fn)
	RegisterGVRDiscoveredHook(fn) // duplicate — must be a no-op
	RegisterGVRDiscoveredHook(fn) // duplicate again

	notifyGVRDiscoveredForReprewarm(schema.GroupVersionResource{Group: "x", Version: "v1", Resource: "y"})

	if got := calls.Load(); got != 1 {
		t.Fatalf("expected idempotent registration: callback fired %d times, want 1", got)
	}
}

func TestGVRDiscoveredHook_MultipleDistinctHooksFire(t *testing.T) {
	ResetGVRDiscoveredHooksForTest()
	defer ResetGVRDiscoveredHooksForTest()

	var a, b atomic.Int64
	RegisterGVRDiscoveredHook(func(gvr schema.GroupVersionResource) { a.Add(1) })
	RegisterGVRDiscoveredHook(func(gvr schema.GroupVersionResource) { b.Add(1) })

	notifyGVRDiscoveredForReprewarm(schema.GroupVersionResource{Group: "x", Version: "v1", Resource: "y"})

	if a.Load() != 1 || b.Load() != 1 {
		t.Fatalf("expected both hooks to fire once: a=%d b=%d", a.Load(), b.Load())
	}
}

func TestGVRDiscoveredHook_NilFnRejected(t *testing.T) {
	ResetGVRDiscoveredHooksForTest()
	defer ResetGVRDiscoveredHooksForTest()

	RegisterGVRDiscoveredHook(nil)

	// Must not panic; no hooks registered.
	notifyGVRDiscoveredForReprewarm(schema.GroupVersionResource{Group: "x", Version: "v1", Resource: "y"})

	gvrDiscoveredHooks.mu.Lock()
	defer gvrDiscoveredHooks.mu.Unlock()
	if len(gvrDiscoveredHooks.hooks) != 0 {
		t.Fatalf("expected nil fn rejected, got %d hooks", len(gvrDiscoveredHooks.hooks))
	}
}

func TestGVRDiscoveredHook_NoHooksRegisteredIsNoop(t *testing.T) {
	ResetGVRDiscoveredHooksForTest()
	defer ResetGVRDiscoveredHooksForTest()

	// Should not panic.
	notifyGVRDiscoveredForReprewarm(schema.GroupVersionResource{Group: "x", Version: "v1", Resource: "y"})
}
