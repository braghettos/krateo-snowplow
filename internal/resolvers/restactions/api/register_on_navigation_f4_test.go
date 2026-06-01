// register_on_navigation_f4_test.go — Tag 0.30.99 (Tag B) falsifier F4
// (R-FALSE-1) for the resolver register-on-navigation path.
//
// R-FALSE-1 for Tag B has TWO halves, and they are deliberately
// asymmetric — the asymmetry is the load-bearing finding:
//
//   1. The pivot SERVING path (dispatchViaInformer) is flag-gated. With
//      RESOLVER_USE_INFORMER unset the resolver never serves a read from
//      the informer. This half is fully covered by the existing
//      informer_dispatch_rfalse1_test.go
//      (TestF4_ResolverFlagOff_DispatchDoesNotReachServableGate +
//      TestF4_PivotCounterIsAFaithfulWitness) — not re-implemented here.
//
//   2. Register-on-navigation (lazyRegisterInnerCallPaths in resolve.go)
//      is NOT flag-gated, and MUST NOT be. It is called unconditionally
//      on every API stage — see resolve.go ~line 258, before the
//      flag-gated `resolverUseInformer() == "true"` dispatch branch.
//      Rationale: lazyRegisterInnerCallPaths registers the informer so
//      the 0.30.8 dep-tracker DELETE-evict edges fire; that L1
//      invalidation contract is active at flag-OFF. Coupling
//      register-on-navigation to RESOLVER_USE_INFORMER would silently
//      break DELETE-evict whenever the pivot is off — a regression.
//
// Tag B's F4 contract is therefore: flag OFF ⇒ the pivot SERVE path is
// inert (binary serves byte-identically to the prior tag on the pivot
// path), while register-on-navigation stays active (it is not a pivot
// behaviour — it predates the pivot and powers DELETE-evict). This test
// captures half 2: register-on-navigation is flag-independent.
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
// registers the informer for a navigated GVR REGARDLESS of the
// RESOLVER_USE_INFORMER flag value. For every non-"true" flag value
// (and for "true"), a never-walked GVR derived from an inner-call path
// must become registered.
//
// If register-on-navigation were (incorrectly) flag-gated, the flag-OFF
// sub-tests would leave the GVR unregistered — and the 0.30.8
// DELETE-evict dep edges for it would never fire. The test fails in
// exactly that scenario.
func TestF4_RegisterOnNavigation_FlagIndependent(t *testing.T) {
	gvr, ok := cache.ParseAPIServerPathToGVR(f4NavPath)
	if !ok {
		t.Fatalf("test setup: f4NavPath %q must parse to a GVR", f4NavPath)
	}

	for _, flag := range []string{"", "false", "FALSE", "0", "shadow", "true"} {
		flag := flag
		t.Run("flag="+flag, func(t *testing.T) {
			t.Setenv("RESOLVER_USE_INFORMER", flag)

			// A pristine cache=on watcher. dispatchTestGVR is the only
			// GVR newDispatchWatcher pre-registers; we use a SEPARATE
			// pristine watcher here so the navigated GVR is genuinely
			// never-walked at the start of the sub-test.
			rw := newPristineFlagWatcher(t)

			// Pre-condition: the navigated GVR is not yet registered.
			// EnsureResourceType on a fresh watcher returns added=true
			// only for a never-registered GVR — we assert that below by
			// observing the register-on-navigation call's effect, not by
			// pre-registering anything here.

			// Drive the resolver's register-on-navigation entry point
			// with a single inner-call RequestOptions. This is the exact
			// call resolve.go makes unconditionally on every API stage
			// (resolve.go ~line 258), before the flag-gated dispatch
			// branch.
			opts := []httpcall.RequestOptions{{
				RequestInfo: httpcall.RequestInfo{
					Path: f4NavPath,
					Verb: ptr.To("GET"),
				},
			}}
			lazyRegisterInnerCallPaths(context.Background(), slog.Default(), opts)

			// The GVR MUST now be registered regardless of the flag.
			// EnsureResourceType returning added=false proves the GVR is
			// already registered (register-on-navigation registered it);
			// added=true would mean lazyRegisterInnerCallPaths did NOT
			// register it — a flag-gating regression.
			added, _ := rw.EnsureResourceType(gvr)
			if added {
				t.Fatalf("flag=%q: register-on-navigation must register the navigated "+
					"GVR independent of RESOLVER_USE_INFORMER; the GVR was still "+
					"unregistered after lazyRegisterInnerCallPaths (DELETE-evict would "+
					"silently break)", flag)
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
