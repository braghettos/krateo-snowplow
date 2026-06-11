// register_on_navigation_f4_test.go — Tag 0.30.99 (Tag B) falsifier F4
// (R-FALSE-1) for the resolver register-on-navigation path, re-framed for
// the #57 implicit-on-cache fold.
//
// R-FALSE-1 for Tag B has TWO halves, and they are deliberately
// asymmetric — the asymmetry is the load-bearing finding:
//
//   1. The pivot SERVING path (dispatchViaInformer) is gated. With the
//      pivot inactive the resolver never serves a read from the informer.
//      #57: that gate was the standalone RESOLVER_USE_INFORMER flag, now
//      folded into CACHE_ENABLED (pivot inactive == cache OFF). This half
//      is covered by informer_dispatch_rfalse1_test.go
//      (TestF4_CacheOff_DispatchDoesNotReachServableGate +
//      TestF4_PivotCounterIsAFaithfulWitness) — not re-implemented here.
//
//   2. Register-on-navigation (lazyRegisterInnerCallPaths in resolve.go)
//      is NOT pivot-gated, and MUST NOT be. It is called unconditionally
//      on every API stage — see resolve.go ~line 258, before the
//      pivot-gated `resolverUseInformer()` dispatch branch. Rationale:
//      lazyRegisterInnerCallPaths registers the informer so the 0.30.8
//      dep-tracker DELETE-evict edges fire; that L1 invalidation contract
//      runs on the cache-on path independent of the pivot. Coupling
//      register-on-navigation to the (now-retired) RESOLVER_USE_INFORMER
//      toggle would have silently broken DELETE-evict — a regression.
//
// Tag B's F4 contract is therefore: register-on-navigation stays active
// on the cache-on path regardless of any (now-retired) pivot toggle — it
// is not a pivot behaviour; it predates the pivot and powers DELETE-evict.
// This test captures half 2: register-on-navigation is independent of the
// retired RESOLVER_USE_INFORMER flag's value (stale values are ignored;
// the watcher is cache-on, which is the only state where the walk runs).
//
// Per feedback_no_special_cases.md: a generic customer-style apiserver
// path is used; no per-resource branching.

package api

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/krateoplatformops/snowplow/internal/cache"

	httpcall "github.com/krateoplatformops/plumbing/http/request"
	"github.com/krateoplatformops/plumbing/ptr"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// f4NavPath is a fully-resolved apiserver LIST path the resolver would
// derive for an inner call. ParseAPIServerPathToGVR maps it to the
// dispatch test GVR (restactions.templates.krateo.io/v1) — the same GVR
// dispatchTestGVR resolves to — so it is registrable on a cache=on
// watcher whose fake dynamic client knows the List kind.
const f4NavPath = "/apis/templates.krateo.io/v1/namespaces/default/restactions"

// TestF4_RegisterOnNavigation_FlagIndependent asserts the Tag B F4
// contract for register-on-navigation: lazyRegisterInnerCallPaths
// registers the informer for a navigated GVR REGARDLESS of the (now
// retired, #57) RESOLVER_USE_INFORMER flag value. For every stale flag
// value, on a cache-on watcher, a never-walked GVR derived from an
// inner-call path must become registered.
//
// If register-on-navigation were (incorrectly) coupled to the pivot
// toggle, a stale flag value would leave the GVR unregistered — and the
// 0.30.8 DELETE-evict dep edges for it would never fire. The test fails
// in exactly that scenario. (The watcher is cache-on throughout, which is
// the only state where the walk runs under the #57 fold.)
func TestF4_RegisterOnNavigation_FlagIndependent(t *testing.T) {
	gvr, ok := cache.ParseAPIServerPathToGVR(f4NavPath)
	if !ok {
		t.Fatalf("test setup: f4NavPath %q must parse to a GVR", f4NavPath)
	}

	// Stale RESOLVER_USE_INFORMER values — all retired/ignored under #57.
	for _, staleFlag := range []string{"", "false", "FALSE", "0", "shadow", "true"} {
		staleFlag := staleFlag
		t.Run("stale_flag="+staleFlag, func(t *testing.T) {
			// A pristine cache=on watcher (newPristineFlagWatcher sets
			// CACHE_ENABLED=true). The stale RESOLVER_USE_INFORMER value
			// must not change register-on-navigation behaviour.
			rw := newPristineFlagWatcher(t)
			t.Setenv("RESOLVER_USE_INFORMER", staleFlag)

			// Pre-condition: the navigated GVR is not yet registered.
			// EnsureResourceType on a fresh watcher returns added=true
			// only for a never-registered GVR — we assert that below by
			// observing the register-on-navigation call's effect, not by
			// pre-registering anything here.

			// Drive the resolver's register-on-navigation entry point
			// with a single inner-call RequestOptions. This is the exact
			// call resolve.go makes unconditionally on every API stage
			// (resolve.go ~line 258), before the pivot-gated dispatch
			// branch.
			opts := []httpcall.RequestOptions{{
				RequestInfo: httpcall.RequestInfo{
					Path: f4NavPath,
					Verb: ptr.To("GET"),
				},
			}}
			lazyRegisterInnerCallPaths(context.Background(), slog.Default(), opts)

			// The GVR MUST now be registered regardless of the stale flag.
			// EnsureResourceType returning added=false proves the GVR is
			// already registered (register-on-navigation registered it);
			// added=true would mean lazyRegisterInnerCallPaths did NOT
			// register it — a pivot-coupling regression.
			added, _ := rw.EnsureResourceType(gvr)
			if added {
				t.Fatalf("stale RESOLVER_USE_INFORMER=%q: register-on-navigation must register "+
					"the navigated GVR independent of the retired flag; the GVR was still "+
					"unregistered after lazyRegisterInnerCallPaths (DELETE-evict would "+
					"silently break)", staleFlag)
			}
		})
	}
}

// newPristineFlagWatcher builds a cache=on ResourceWatcher with NO GVR
// pre-registered beyond the constructor's RBAC bootstrap set, installs
// it as the global watcher, and wires cleanup. Unlike newDispatchWatcher
// it does not register or sync dispatchTestGVR — so a GVR derived from
// f4NavPath is genuinely never-walked when the sub-test begins.
func newPristineFlagWatcher(t *testing.T) *cache.ResourceWatcher {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")

	// Reuse the dispatch test scheme + List-kind registrations (which
	// cover restactions.templates.krateo.io) so a register-on-navigation
	// EnsureResourceType for the f4NavPath GVR can run its informer LIST
	// without panicking. No seed objects — the GVR is genuinely
	// never-walked until lazyRegisterInnerCallPaths touches it.
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		dispatchTestScheme(), dispatchTestListKinds())
	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil {
		t.Fatalf("expected non-nil watcher under CACHE_ENABLED=true")
	}
	t.Cleanup(func() {
		rw.Stop()
		time.Sleep(50 * time.Millisecond)
	})
	cache.SetGlobal(rw)
	t.Cleanup(func() { cache.SetGlobal(nil) })
	return rw
}
