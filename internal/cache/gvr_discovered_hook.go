// gvr_discovered_hook.go — Ship 2 Stage 2 / 0.30.247. Cache-side
// observer registry that fires when a GVR is first registered post-boot
// via the synchronous discovery path (discovery_lookup.go's EnsureResourceType
// added==true branch).
//
// WHY A REGISTRY (NOT A DIRECT CALL). The import graph is one-way:
// `dispatchers → cache`. Cache CANNOT directly import dispatchers
// without creating a circular dependency. The dispatchers-side prewarm
// engine subscribes here at boot, before its worker spawns, and the
// cache fires this hook synchronously inside discovery_lookup.go's
// `if added` branch — the same site that already fires the FD1
// dirty-mark (OnResourceTypeAvailable). The hook handler downstream
// is non-blocking (engine.enqueueScope is a tight critical section
// behind a sync.Mutex + a buffered=1 signal channel).
//
// MIRRORS NotifyGVRRegistered (registered_gvrs_expvar.go:79-89). The
// shape is identical to the existing GVR-registration notification
// surface, differing only in that the discovered GVR payload is carried
// to the hook (the consumer scopes the re-prewarm to that GVR).
//
// IDEMPOTENT REGISTRATION. RegisterGVRDiscoveredHook compares the
// pointer of the registered function (via reflect.ValueOf(fn).Pointer())
// against already-registered hooks. A second registration of the SAME
// fn pointer is a no-op. This guards against accidental double-wire
// from a future StartPrewarmEngine re-entry; production today calls it
// exactly once via prewarm_engine_boot.go.

package cache

import (
	"reflect"
	"sync"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// gvrDiscoveredHooks is the package-level registry. Guarded by hooksMu.
// Holds the registered callbacks plus a snapshot of their function
// pointers for idempotent registration. The dual storage (hooks slice +
// pointers map) trades a tiny constant memory cost for O(1) duplicate
// detection — registration is rare (boot-time), but the check still
// runs under the mutex so keep it cheap.
var gvrDiscoveredHooks struct {
	mu       sync.Mutex
	hooks    []func(gvr schema.GroupVersionResource)
	pointers map[uintptr]struct{}
}

// RegisterGVRDiscoveredHook adds a callback that fires when a new GVR
// is registered post-boot via DiscoverGroupResources. The callback runs
// SYNCHRONOUSLY on the discovery goroutine — keep it cheap. The
// dispatchers-side hook handler invokes engine.enqueueScope which is
// O(1) under a sync.Mutex (the bounded dedup queue) + a non-blocking
// signal-channel send.
//
// IDEMPOTENT: registering the same fn pointer twice is a no-op. This
// guards against accidental double-registration (e.g. a future engine
// restart). Production today wires this exactly once at boot.
//
// CONTRACT: callers must NOT block inside fn. Anything that could
// block (apiserver call, long compute) must dispatch to its own
// goroutine.
func RegisterGVRDiscoveredHook(fn func(gvr schema.GroupVersionResource)) {
	if fn == nil {
		return
	}
	ptr := reflect.ValueOf(fn).Pointer()
	gvrDiscoveredHooks.mu.Lock()
	defer gvrDiscoveredHooks.mu.Unlock()
	if gvrDiscoveredHooks.pointers == nil {
		gvrDiscoveredHooks.pointers = map[uintptr]struct{}{}
	}
	if _, dup := gvrDiscoveredHooks.pointers[ptr]; dup {
		return
	}
	gvrDiscoveredHooks.pointers[ptr] = struct{}{}
	gvrDiscoveredHooks.hooks = append(gvrDiscoveredHooks.hooks, fn)
}

// notifyGVRDiscoveredForReprewarm fires every registered hook with the
// discovered GVR. Called from discovery_lookup.go inside the `if added`
// branch after the existing OnResourceTypeAvailable / cache.discovery.gvr_registered
// emit, AFTER AddNavigatedGVR has widened the BindingsByGVR navigated
// set (the ordering is load-bearing — see discovery_lookup.go for
// rationale).
//
// SNAPSHOT-UNDER-LOCK + FIRE-UNLOCKED: copies the hook slice while
// holding the mutex, then releases before invoking — keeps the
// critical section O(N) on copy-cost only and avoids holding the
// mutex across user-supplied callbacks.
func notifyGVRDiscoveredForReprewarm(gvr schema.GroupVersionResource) {
	gvrDiscoveredHooks.mu.Lock()
	hooks := append([]func(schema.GroupVersionResource){}, gvrDiscoveredHooks.hooks...)
	gvrDiscoveredHooks.mu.Unlock()
	for _, fn := range hooks {
		fn(gvr)
	}
}

// ResetGVRDiscoveredHooksForTest clears the registry. TEST-ONLY — the
// production registry is append-only (boot-time wiring).
func ResetGVRDiscoveredHooksForTest() {
	gvrDiscoveredHooks.mu.Lock()
	gvrDiscoveredHooks.hooks = nil
	gvrDiscoveredHooks.pointers = nil
	gvrDiscoveredHooks.mu.Unlock()
}
