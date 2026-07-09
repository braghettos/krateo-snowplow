// boundedsyncwait_c1_scaffold_test.go — #121 1a C1 "never-worse" harness
// (SCAFFOLD).
//
// STATUS: site-independent scaffold, built AHEAD of the architect's confirmed
// 1a gate placement (team-lead hold, 2026-07-09). It proves the ONE property
// the C1 "never-worse" arm hinges on and that does NOT depend on where the
// gate is wired: a BOUNDED WaitForCacheSync over a registered-but-never-synced
// GVR RETURNS after ~the bound (it does NOT hang), so the caller can fall
// through to the live LIST exactly as today. When the architect confirms the
// 1a gate function (bounded per-GVR sync-wait → serve-from-informer-else-
// fall-through), the C1 arm plugs the REAL gate into this harness's fixture
// (newNeverSyncsWatcher) and adds the two remaining assertions:
//
//   C1(a) — the walk's LIST dispatch, when the bounded wait expires, FALLS
//           THROUGH to the live paged LIST and COMPLETES (no new stall/hang).
//   C1(b) — the bounded-wait budget is SMALL relative to the ~136s headroom
//           1a frees, so a never-syncing GVR cannot itself push the seed into
//           a NEW deadline-cut. Asserted here by pinning the bound is a small
//           constant and the wait returns within bound+slack.
//
// This file uses ONLY production machinery (WaitForCacheSync over a real
// SharedIndexInformer that is never Run()), so the bounded-return property is
// proven against the same primitive 1a will reuse — not a mock of it.

package cache

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/informers"
	clientcache "k8s.io/client-go/tools/cache"
)

// newNeverSyncsWatcher builds a modeInformer ResourceWatcher with a single
// registered GVR whose informer is NEVER Run() — so HasSynced() stays false
// forever, exactly the boot-walk race the #121 1a gate addresses (the
// benchapps informer registered mid-walk but not yet synced when the walk's
// LIST dispatches). IsServable(gvr) is therefore false, and a bounded
// WaitForCacheSync over it MUST time out rather than block indefinitely.
//
// Reuses the genericInformerForTest wrapper + fakeListWatch from
// bytesobject_falsifier_test.go (same package). The informer is constructed
// but its Run() goroutine is deliberately NOT started, so the reflector never
// performs the initial LIST and HasSynced never flips true.
func newNeverSyncsWatcher(t *testing.T, gvr schema.GroupVersionResource) *ResourceWatcher {
	t.Helper()

	inf := clientcache.NewSharedIndexInformer(
		fakeListWatch{},
		&metav1.PartialObjectMetadata{},
		0,
		clientcache.Indexers{clientcache.NamespaceIndex: clientcache.MetaNamespaceIndexFunc},
	)
	// DELIBERATELY NOT Run() — HasSynced() stays false.

	rw := &ResourceWatcher{
		mode:      modeInformer,
		informers: map[schema.GroupVersionResource]informers.GenericInformer{},
	}
	rw.informers[gvr] = &genericInformerForTest{inf: inf}
	return rw
}

// TestC1Scaffold_BoundedSyncWaitReturnsOnNeverSynced proves the C1 never-worse
// invariant's load-bearing half: a bounded sync-wait over a
// registered-but-never-synced GVR RETURNS (with the not-synced signal) after
// ~the bound, so the 1a gate can fall through to the live LIST. A regression
// that made the wait unbounded (blocking on HasSynced with no deadline) would
// hang here and trip the test's own timeout — the exact "never worse" failure
// the C1 arm forbids.
func TestC1Scaffold_BoundedSyncWaitReturnsOnNeverSynced(t *testing.T) {
	gvr := compositionGVR
	rw := newNeverSyncsWatcher(t, gvr)

	// Sanity: the never-synced GVR is NOT servable (the precondition that
	// sends the boot walk down the live-LIST fall-through today).
	if rw.IsServable(gvr) {
		t.Fatal("precondition: a never-Run() informer must not be servable")
	}

	// A SMALL bound (C1(b) — small relative to the ~136s headroom 1a frees).
	// The scaffold uses a deliberately tiny bound so the test is fast; the
	// real 1a gate's const plugs in here unchanged.
	const bound = 150 * time.Millisecond
	const slack = 3 * time.Second // generous CI slack; the assertion is
	// "returns near the bound, NOT hangs" — not a tight latency SLO.

	start := time.Now()
	err := rw.WaitForCacheSync(context.Background(), bound)
	elapsed := time.Since(start)

	// C1 half 1 — it RETURNED (did not hang). A nil err would mean the GVR
	// somehow synced (it can't — never Run()); the real signal is a
	// timeout error, which is exactly what tells the 1a gate to fall through.
	if err == nil {
		t.Fatal("WaitForCacheSync returned nil over a never-synced GVR; expected a bounded timeout so the gate falls through to the live LIST")
	}

	// C1 half 2 (bound proportionality) — returned within bound+slack, so a
	// never-syncing GVR cannot itself consume enough scope budget to trigger
	// a NEW seed deadline-cut.
	if elapsed > bound+slack {
		t.Fatalf("bounded wait took %v; want ≤ bound(%v)+slack(%v) — an over-budget wait would re-create the deadline-cut C1 forbids", elapsed, bound, slack)
	}
}

// TestWaitForGVRSync_Bounded is the direct unit test for the #121 1a
// per-GVR bounded sync primitive: (a) an unregistered GVR returns false
// immediately (nothing to wait on → caller falls through); (b) a
// registered-but-never-synced GVR returns false after ~the bound (never
// hangs). The already-synced fast path is exercised by the api-package
// informer-serve arms (a real synced GVR returns true immediately).
func TestWaitForGVRSync_Bounded(t *testing.T) {
	gvr := compositionGVR

	t.Run("unregistered GVR returns false immediately", func(t *testing.T) {
		rw := &ResourceWatcher{
			mode:      modeInformer,
			informers: map[schema.GroupVersionResource]informers.GenericInformer{},
		}
		start := time.Now()
		if rw.WaitForGVRSync(context.Background(), gvr, 2*time.Second) {
			t.Fatal("unregistered GVR must return false (nothing to wait on)")
		}
		if el := time.Since(start); el > 200*time.Millisecond {
			t.Fatalf("unregistered GVR wait took %v; must be ~immediate (no informer to wait on)", el)
		}
	})

	t.Run("registered-but-never-synced returns false after the bound", func(t *testing.T) {
		rw := newNeverSyncsWatcher(t, gvr)
		const bound = 150 * time.Millisecond
		start := time.Now()
		got := rw.WaitForGVRSync(context.Background(), gvr, bound)
		el := time.Since(start)
		if got {
			t.Fatal("never-synced GVR must return false (it never reaches HasSynced)")
		}
		if el < bound {
			t.Fatalf("returned in %v, before the %v bound — did it actually wait?", el, bound)
		}
		if el > bound+3*time.Second {
			t.Fatalf("returned in %v — the bound (%v) must cap the wait (no hang)", el, bound)
		}
	})
}

// NOTE: the C1 gate-INVOCATION arm (the walk's LIST dispatch falls through to
// the live paged LIST and COMPLETES when the GVR is unsynced, + the bounded
// wait cannot re-create the seed deadline-cut) is REALIZED in the api package
// against the real dispatch site: see
// TestServe1a_C1_NeverWorse_FallsThroughToLiveList in
// internal/resolvers/restactions/api/internal_dispatch_informer_serve_1a_test.go
// (that is where dispatchViaInternalRESTConfig + WithServeWatcher live). This
// file keeps the site-independent primitive + fixture proofs.
