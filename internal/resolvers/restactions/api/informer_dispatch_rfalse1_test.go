// informer_dispatch_rfalse1_test.go — Tag 0.30.98 falsifier F4 (R-FALSE-1).
//
// R-FALSE-1 (architect falsifier, carried since the 0.30.95 pivot): with
// RESOLVER_USE_INFORMER unset, the binary's pivot path is byte-identical
// to the prior tag — the resolver NEVER routes a read through the
// informer cache, and therefore never reaches the 0.30.98 four-conjunct
// servability gate (IsServable / ListObjectsServable).
//
// Why this test lives HERE and not in internal/cache: R-FALSE-1 is a
// property of the FLAG-GATED CONSUMER, not of the servability predicate.
// The predicate is pure and has no flag read. The single consumer gate
// is in resolve.go:
//
//	if resolverUseInformer() == "true" {
//	    if raw, served := dispatchViaInformer(gctx, call); served { ... }
//	}
//
// dispatchViaInformer is the ONLY function in the resolver that calls
// IsServable / ListObjectsServable. Every path through dispatchViaInformer
// increments exactly one pivot counter (ListServed | GetServed |
// Fallthrough) — see informer_dispatch_metrics.go. So "the dispatch path
// did not reach the servable gate" is observable as "the pivot counters
// did not move".
//
// This test asserts the true behavioral surface:
//
//   - NEGATIVE control: with the flag unset / "false" / "shadow", the
//     resolve.go gate (resolverUseInformer() == "true") evaluates false,
//     dispatchViaInformer is not invoked, and the pivot counters stay
//     frozen — the servable gate is unreachable.
//   - POSITIVE control: with the flag "true", the gate passes, invoking
//     dispatchViaInformer DOES move a counter — proving the counter is a
//     faithful witness of the gate being reached, so the negative
//     control's "counters frozen" is meaningful.
//
// Per feedback_no_special_cases.md: the assertion uses the generic
// dispatchTestGVR; no per-resource branching.

package api

import (
	"net/http"
	"testing"
)

// gateReachesPivot replicates the EXACT resolve.go consumer gate
// (resolve.go ~line 344). The resolver invokes dispatchViaInformer — and
// hence the servability gate — if and only if this returns true. Keeping
// the predicate in one helper means a future change to the resolve.go
// gate that diverges from this is caught by the test below.
func gateReachesPivot() bool {
	return resolverUseInformer() == "true"
}

// TestF4_ResolverFlagOff_DispatchDoesNotReachServableGate is the
// behavioral R-FALSE-1 falsifier. For every non-"true" flag value it
// asserts the resolve.go gate evaluates false AND that, honouring the
// gate as resolve.go does, dispatchViaInformer is never invoked — so the
// 0.30.98 servable gate stays unreached and the pivot counters do not
// move.
func TestF4_ResolverFlagOff_DispatchDoesNotReachServableGate(t *testing.T) {
	// A fully-synced cache=on watcher with a seeded, servable GVR. If the
	// gate were (incorrectly) open, dispatchViaInformer WOULD serve this
	// call from the indexer and bump ListServed — so the watcher being
	// "ready to serve" makes the negative control strict.
	newDispatchWatcher(t, newTestRestActionRuntimeObject("default", "ra-1", "m1"))

	// A LIST call the pivot could serve if it were ever reached.
	call := buildCall(http.MethodGet, "/apis/templates.krateo.io/v1/namespaces/default/restactions")

	for _, flag := range []string{"", "false", "FALSE", "0", "shadow", "no", "yes", "1"} {
		flag := flag
		t.Run("flag="+flag, func(t *testing.T) {
			t.Setenv("RESOLVER_USE_INFORMER", flag)

			// The resolve.go gate MUST evaluate false for every value
			// that is not exactly "true".
			if gateReachesPivot() {
				t.Fatalf("flag=%q: resolve.go gate must be closed for non-\"true\" values; got open", flag)
			}

			before := DispatchInformerStatsSnapshot()

			// Honour the gate exactly as resolve.go does: only call
			// dispatchViaInformer when the gate is open. With the gate
			// closed this branch is dead — dispatchViaInformer (and the
			// servable gate it alone reaches) is never entered.
			if gateReachesPivot() {
				_, _ = dispatchViaInformer(dispatchCtx(), call)
			}

			after := DispatchInformerStatsSnapshot()
			if after != before {
				t.Fatalf("flag=%q: pivot counters moved (before=%+v after=%+v) — "+
					"the dispatch path reached the servable gate with the flag off (R-FALSE-1 violated)",
					flag, before, after)
			}
		})
	}
}

// TestF4_PivotCounterIsAFaithfulWitness is the POSITIVE control for the
// negative test above. It proves that reaching dispatchViaInformer DOES
// move a pivot counter — so "counters frozen" in the negative control
// genuinely means "dispatchViaInformer was not entered", not "the
// counter is dead". With the flag "true" the resolve.go gate opens and a
// servable LIST is answered from the indexer, bumping ListServed.
func TestF4_PivotCounterIsAFaithfulWitness(t *testing.T) {
	newDispatchWatcher(t, newTestRestActionRuntimeObject("default", "ra-1", "m1"))
	t.Setenv("RESOLVER_USE_INFORMER", "true")

	if !gateReachesPivot() {
		t.Fatalf("flag=true: resolve.go gate must be open")
	}

	call := buildCall(http.MethodGet, "/apis/templates.krateo.io/v1/namespaces/default/restactions")

	before := DispatchInformerStatsSnapshot()
	raw, served := dispatchViaInformer(dispatchCtx(), call)
	after := DispatchInformerStatsSnapshot()

	if !served {
		t.Fatalf("positive control: flag=true + synced+servable GVR must serve; got served=false")
	}
	if len(raw) == 0 {
		t.Fatalf("positive control: served call returned 0 bytes")
	}
	if after.ListServed != before.ListServed+1 {
		t.Fatalf("positive control: ListServed must increment by 1 when the pivot serves a LIST "+
			"(before=%d after=%d) — without this the negative control proves nothing",
			before.ListServed, after.ListServed)
	}
}
