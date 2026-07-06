// prewarm_first_nav_latch_test.go — #99 FIX-F: the first-nav readyz latch
// falsifier set (PM conditions F-C1..F-C4, docs/seed-tail-restactions-budget-
// and-16mb-serve-trace-2026-07-04.md §F.2).
//
// The latch fires when the rank-1 (ri==0) RootIndex==0 first-nav WIDGET
// segment has seeded (or provably has none). engineSeed's select waits on it
// so /readyz flips WITH the dashboard warm, NOT at the PHASE1_TIMEOUT backstop
// with cold cells (the fix3r degeneration §F.0). These arms discriminate the
// fire-POINT, not merely eventual firing:
//
//   ARM-GREEN (§F.2 ARM-TAIL, F-C1): rank-1 first-nav widgets + a heavy
//     NON-first-nav tail (RootIndex>0 widget + a fanout restaction). The latch
//     MUST fire BEFORE any tail seed runs. Asserts ORDERING (latch-fire index
//     < first tail-seed index), not just that it fired.
//   ARM-RED-i (mutation → §F.2 mutation (i)): neuter the segment decrement so
//     the latch can only fire via the zero-targets / rank-boundary path (=
//     "moved to full-scope completion"). Under the SAME fixture the latch then
//     fires only AFTER the whole rank-1 pass incl. the tail → RED.
//   ARM-C3 (§F.2 ARM-COLD / F-C3): a RootIndex>0-ONLY rank-1 (no dashboard
//     widget authorised for rank-1). firstNavRemaining stays 0, so the latch
//     must NOT fire via the segment path early; it fires ONLY through the
//     provably-zero path AFTER the rank-1 seed (never spuriously mid-tail).
//   ARM-BACKSTOP (§F.2 / C2, F-C4): engineSeed's select composition — the
//     latch never firing must fall through to the pctx.Done() backstop, so
//     readiness is never withheld forever. Driven at the select level.
//
// Hermetic, -race, seams only (no cluster / apiserver / informer). Serializes
// on engineLatchTestMu (shares the process latch singleton + seed counters +
// the customerInFlight atomic with the sibling latch tests). Does NOT touch
// ./internal/rbac/... (destructive TestMain).

package dispatchers

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/krateoplatformops/plumbing/endpoints"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// ── recorder that timestamps the latch fire relative to seed events ─────────
type latchSeedEvent struct {
	class    string // "widget" | "restaction" | "LATCH"
	label    string // ns/name, or the fire reason for LATCH
	rootIdx  int    // widget RootIndex (−1 for RA / LATCH)
	identity string
}

type latchRecorder struct {
	mu     sync.Mutex
	events []latchSeedEvent
}

func (r *latchRecorder) rec(e latchSeedEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *latchRecorder) snapshot() []latchSeedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]latchSeedEvent(nil), r.events...)
}

// installLatchFireObserver records the latch-fire INSTANT synchronously at the
// fire site (firstNavFireObserver hook) so the "LATCH" event lands at its true
// position in the ordered seed-event stream. An async wait() watcher races the
// loop's next seed (the fire is synchronous inside seedScopeYielding, but a
// goroutine blocked on wait() may not be scheduled until later seeds run) — so
// we observe synchronously. Cleared on cleanup. Caller holds engineLatchTestMu.
func installLatchFireObserver(t *testing.T, rec *latchRecorder) {
	t.Helper()
	firstNavFireObserver = func(reason string) {
		rec.rec(latchSeedEvent{class: "LATCH", label: reason, rootIdx: -1})
	}
	t.Cleanup(func() { firstNavFireObserver = nil })
}

// latchWidgetGVR gives each fixture widget its own GVR so the seam enumerator
// can hand it a distinct identity set.
func latchWidgetGVR(res string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "widgets.templates.krateo.io", Version: "v1beta1", Resource: res}
}

// harvestWidgetAtRoot stamps a widget through the REAL harvester at a chosen
// config-root index (BeginRoot advances curRoot), so RootIndex comes from the
// real stamping path (condition-3 style — not hand-assigned). rootIdx==0 is
// the first-nav (dashboard) segment.
func harvestWidgetAtRoot(h *navWidgetHarvester, name string, gvr schema.GroupVersionResource, rootIdx int) {
	for h.curRoot < rootIdx {
		h.BeginRoot()
	}
	w := &unstructured.Unstructured{}
	w.SetNamespace("krateo-system")
	w.SetName(name)
	w.SetGroupVersionKind(schema.GroupVersionKind{Group: gvr.Group, Version: gvr.Version, Kind: "W"})
	h.harvestNavWidget(w, gvr, -1, 1, -1, -1)
}

func latchRA(name string) templatesv1.ObjectReference {
	return templatesv1.ObjectReference{
		Reference:  templatesv1.Reference{Name: name, Namespace: "krateo-system"},
		APIVersion: restActionGVR.Group + "/" + restActionGVR.Version,
		Resource:   restActionGVR.Resource,
	}
}

// installLatchSeams wires the enumerator/target-GVR/seed seams for a fixture
// and returns the recorder. All seams record into rec; the widget seam records
// the entry's RootIndex so the assert can see segment vs tail.
func installLatchSeams(t *testing.T, rec *latchRecorder,
	enumerate func(schema.GroupVersionResource) []cache.PrewarmTarget,
	raTargetGVR func(templatesv1.ObjectReference) (schema.GroupVersionResource, bool)) {
	t.Helper()

	prevEnum := enumeratePrewarmTargetsForGVRFn
	enumeratePrewarmTargetsForGVRFn = func(gvr schema.GroupVersionResource, _ string) []cache.PrewarmTarget {
		return enumerate(gvr)
	}
	t.Cleanup(func() { enumeratePrewarmTargetsForGVRFn = prevEnum })

	prevTGVR := restActionTargetGVRFn
	restActionTargetGVRFn = func(_ context.Context, ref templatesv1.ObjectReference) (schema.GroupVersionResource, bool) {
		return raTargetGVR(ref)
	}
	t.Cleanup(func() { restActionTargetGVRFn = prevTGVR })

	prevW := seedOneWidgetFn
	seedOneWidgetFn = func(ctx context.Context, e navWidgetEntry, _ string) error {
		rec.rec(latchSeedEvent{class: "widget", label: e.W.GetName(), rootIdx: e.RootIndex, identity: eIdentityLabel(ctx)})
		return nil
	}
	t.Cleanup(func() { seedOneWidgetFn = prevW })

	prevR := seedOneRestactionFn
	seedOneRestactionFn = func(ctx context.Context, _ string, ref templatesv1.ObjectReference, _ string) error {
		rec.rec(latchSeedEvent{class: "restaction", label: ref.Name, rootIdx: -1, identity: eIdentityLabel(ctx)})
		return nil
	}
	t.Cleanup(func() { seedOneRestactionFn = prevR })
}

func latchTargets(gvr schema.GroupVersionResource, ids ...eID) []cache.PrewarmTarget {
	return eIdentityTargets(gvr, ids...)
}

// firstIndexOf returns the index of the first event matching pred, or len(ev).
func firstIndexOf(ev []latchSeedEvent, pred func(latchSeedEvent) bool) int {
	for i, e := range ev {
		if pred(e) {
			return i
		}
	}
	return len(ev)
}

// ── ARM-GREEN + ARM-RED-i (F-C1 / §F.2 ARM-TAIL + mutation (i)) ─────────────
// Fixture: rank-1 (devs, collapsed=442) has a RootIndex==0 dashboard widget
// (first-nav) AND a RootIndex==1 tail widget; plus a rank-1 fanout restaction.
// rank-2 (ops) has targets on both widgets. The latch MUST fire after the
// dashboard widget and BEFORE the tail widget + the restaction.
func TestFirstNavLatch_FiresBeforeTail_GreenAndMutationRed(t *testing.T) {
	engineLatchTestMu.Lock()
	defer engineLatchTestMu.Unlock()
	zeroCustomerInFlight()
	quietLoggingE(t)

	run := func(t *testing.T, mutateNeuterSegment bool) []latchSeedEvent {
		resetFirstNavLatchForTest()
		ensureFirstNavLatch() // build the process latch so seedScopeYielding can fire it

		devs := eID{name: "devs", group: true, collapsed: 442}
		ops := eID{name: "ops", group: true, collapsed: 5}

		h := newNavWidgetHarvester()
		dashGVR := latchWidgetGVR("flexes")
		tailGVR := latchWidgetGVR("estategraphs")
		harvestWidgetAtRoot(h, "dashboard-flex", dashGVR, 0) // RootIndex 0 — first-nav
		harvestWidgetAtRoot(h, "estate-graph", tailGVR, 1)   // RootIndex 1 — tail
		widgets := h.snapshot()

		fanoutRA := latchRA("fanout-ra")
		ras := []templatesv1.ObjectReference{fanoutRA}

		rec := &latchRecorder{}
		installLatchSeams(t, rec,
			func(gvr schema.GroupVersionResource) []cache.PrewarmTarget {
				switch gvr {
				case dashGVR, tailGVR:
					return latchTargets(gvr, devs, ops)
				case eFanoutRAGVR:
					return latchTargets(eFanoutRAGVR, devs, ops, eID{name: "filler", collapsed: 1})
				}
				return nil
			},
			func(_ templatesv1.ObjectReference) (schema.GroupVersionResource, bool) {
				return eFanoutRAGVR, true
			})

		// Mutation (i): neuter the segment decrement so the latch can only fire
		// via the rank-boundary / zero-targets path — the "latch moved to
		// full-scope completion" defect. We simulate by pre-firing NOTHING and
		// instead force firstNavRemaining to never reach zero: the cleanest
		// hermetic neuter is to make the RootIndex==0 widget look like a tail
		// (RootIndex forced >0) via a seam on the harvested entries.
		if mutateNeuterSegment {
			for i := range widgets {
				if widgets[i].RootIndex == 0 {
					widgets[i].RootIndex = 99 // no widget is first-nav anymore → segment count 0
				}
			}
		}

		installLatchFireObserver(t, rec)
		if err := seedScopeYielding(context.Background(), ras, widgets, endpoints.Endpoint{}, nil, "authn-ns", false); err != nil {
			t.Fatalf("seedScopeYielding returned %v; want nil", err)
		}
		return rec.snapshot()
	}

	t.Run("green_fires_before_tail", func(t *testing.T) {
		ev := run(t, false)
		latchIdx := firstIndexOf(ev, func(e latchSeedEvent) bool { return e.class == "LATCH" })
		dashIdx := firstIndexOf(ev, func(e latchSeedEvent) bool { return e.class == "widget" && e.label == "dashboard-flex" && e.identity == "group:devs" })
		tailWidgetIdx := firstIndexOf(ev, func(e latchSeedEvent) bool { return e.class == "widget" && e.label == "estate-graph" })
		raIdx := firstIndexOf(ev, func(e latchSeedEvent) bool { return e.class == "restaction" })

		if latchIdx == len(ev) {
			t.Fatalf("F-C1: latch never fired; events=%+v", ev)
		}
		// Fire AFTER the rank-1 dashboard widget seeds (segment complete)...
		if latchIdx < dashIdx {
			t.Fatalf("F-C1: latch fired at idx %d BEFORE the rank-1 dashboard widget seeded at idx %d — ARM-COLD RED; events=%+v", latchIdx, dashIdx, ev)
		}
		// ...and BEFORE any tail work (RootIndex>0 widget + fanout RA) — ARM-TAIL.
		if tailWidgetIdx < len(ev) && latchIdx > tailWidgetIdx {
			t.Fatalf("F-C1/ARM-TAIL: latch fired at idx %d AFTER the NON-first-nav tail widget seeded at idx %d — readyz would wait on tail work; events=%+v", latchIdx, tailWidgetIdx, ev)
		}
		if raIdx < len(ev) && latchIdx > raIdx {
			t.Fatalf("F-C1/ARM-TAIL: latch fired at idx %d AFTER the fanout restaction seeded at idx %d — readyz would wait on the RA tail; events=%+v", latchIdx, raIdx, ev)
		}
	})

	t.Run("mutation_i_full_scope_completion_red", func(t *testing.T) {
		ev := run(t, true)
		latchIdx := firstIndexOf(ev, func(e latchSeedEvent) bool { return e.class == "LATCH" })
		tailWidgetIdx := firstIndexOf(ev, func(e latchSeedEvent) bool { return e.class == "widget" && e.label == "estate-graph" })
		raIdx := firstIndexOf(ev, func(e latchSeedEvent) bool { return e.class == "restaction" })

		if latchIdx == len(ev) {
			t.Fatalf("mutation setup: latch never fired at all; events=%+v", ev)
		}
		// With the segment neutered, the ONLY fire path is the rank-boundary
		// zero-targets check (fires after the rank-1 loop → after ALL tail work).
		// PROVE the mutation is RED against the GREEN property: the latch now
		// fires AFTER the tail, i.e. it does NOT fire before tail work.
		firedBeforeTail := (tailWidgetIdx == len(ev) || latchIdx < tailWidgetIdx) &&
			(raIdx == len(ev) || latchIdx < raIdx)
		if firedBeforeTail {
			t.Fatalf("mutation (i) NOT RED: with the first-nav segment neutered, the latch STILL fired before the tail (idx %d; tailWidget=%d ra=%d) — the ordering assert does not discriminate; events=%+v",
				latchIdx, tailWidgetIdx, raIdx, ev)
		}
	})
}

// ── ARM-C3 (§F.2 ARM-COLD / F-C3): RootIndex>0-only rank-1 ─────────────────
// No RootIndex==0 widget is authorised for the rank-1 identity → the segment
// count is 0 → the latch must NOT fire via the segment decrement mid-tail; it
// fires ONLY through the provably-zero path AFTER the rank-1 seed. Guards
// against a false-ready when the dashboard segment was never reached.
func TestFirstNavLatch_RootIndexGtZeroOnly_NoEarlySegmentFire(t *testing.T) {
	engineLatchTestMu.Lock()
	defer engineLatchTestMu.Unlock()
	zeroCustomerInFlight()
	quietLoggingE(t)

	resetFirstNavLatchForTest()
	ensureFirstNavLatch() // build the process latch so seedScopeYielding can fire it

	devs := eID{name: "devs", group: true, collapsed: 442}

	h := newNavWidgetHarvester()
	tailA := latchWidgetGVR("tailas")
	tailB := latchWidgetGVR("tailbs")
	harvestWidgetAtRoot(h, "tail-a", tailA, 1) // RootIndex 1 — no first-nav segment
	harvestWidgetAtRoot(h, "tail-b", tailB, 2)
	widgets := h.snapshot()

	rec := &latchRecorder{}
	installLatchSeams(t, rec,
		func(gvr schema.GroupVersionResource) []cache.PrewarmTarget {
			switch gvr {
			case tailA, tailB:
				return latchTargets(gvr, devs)
			}
			return nil
		},
		func(_ templatesv1.ObjectReference) (schema.GroupVersionResource, bool) {
			return schema.GroupVersionResource{}, false
		})

	installLatchFireObserver(t, rec)
	if err := seedScopeYielding(context.Background(), nil, widgets, endpoints.Endpoint{}, nil, "authn-ns", false); err != nil {
		t.Fatalf("seedScopeYielding returned %v; want nil", err)
	}
	ev := rec.snapshot()

	latchIdx := firstIndexOf(ev, func(e latchSeedEvent) bool { return e.class == "LATCH" })
	if latchIdx == len(ev) {
		t.Fatalf("F-C3: latch never fired for a zero-first-nav rank-1 — readyz would hang to the backstop; events=%+v", ev)
	}
	// It MUST NOT fire before both tail widgets have seeded — a segment-path
	// early fire on a RootIndex>0-only rank-1 is the F-C3 false-ready defect.
	// The provably-zero path fires at the rank-1 boundary, i.e. AFTER both
	// tail widgets.
	lastTailIdx := -1
	for i, e := range ev {
		if e.class == "widget" {
			lastTailIdx = i
		}
	}
	if lastTailIdx < 0 {
		t.Fatalf("F-C3 setup: no tail widgets seeded; events=%+v", ev)
	}
	if latchIdx < lastTailIdx {
		t.Fatalf("F-C3: latch fired at idx %d BEFORE the last tail widget seeded at idx %d — false-ready on a RootIndex>0-only rank-1 (segment path fired spuriously); events=%+v", latchIdx, lastTailIdx, ev)
	}
}

// ── ARM-BACKSTOP (§F.2 / C2, F-C4): select composition ─────────────────────
// engineSeed's select waits on the FIRST of {firstNav, bootDone, pctx.Done()}.
// When the latch never fires (seed aborted before the segment) AND bootDone
// stays open, the pctx.Done() backstop MUST unblock readiness — never hang
// forever. This reproduces engineSeed's select shape in isolation (the real
// closure is not directly callable without the full Phase1Warmup wiring; the
// ── CONDITION 1 (arch pre-commit): cross-goroutine fire/wait -race arm ──────
// All the arms above fire AND observe the latch on ONE goroutine (the fire
// site records synchronously into the recorder), so -race never exercises the
// ACTUAL production handoff: the ENGINE BOOT goroutine closes the latch (deep
// inside seedScopeYielding → fireFirstNav("segment-complete")) while a SEPARATE
// goroutine (engineSeed's select) is parked on firstNav.wait(). This arm
// reproduces that exact concurrency: a waiter goroutine blocks on wait() BEFORE
// the seed starts, and the REAL production fire site (seedScopeYielding, not a
// direct latch.fire poke) unblocks it from the seed goroutine. Run under -race,
// this is the only arm that flags a data race on the done-channel close/read or
// on any state the fire path touches concurrently with the waiter.
func TestFirstNavLatch_ConcurrentFireWait_RealSegmentPath_Race(t *testing.T) {
	engineLatchTestMu.Lock()
	defer engineLatchTestMu.Unlock()
	zeroCustomerInFlight()
	quietLoggingE(t)

	resetFirstNavLatchForTest()
	latch := ensureFirstNavLatch()

	devs := eID{name: "devs", group: true, collapsed: 442}
	h := newNavWidgetHarvester()
	dashGVR := latchWidgetGVR("flexes")
	harvestWidgetAtRoot(h, "dashboard-flex", dashGVR, 0) // RootIndex 0 — first-nav
	widgets := h.snapshot()

	rec := &latchRecorder{}
	installLatchSeams(t, rec,
		func(gvr schema.GroupVersionResource) []cache.PrewarmTarget {
			if gvr == dashGVR {
				return latchTargets(gvr, devs)
			}
			return nil
		},
		func(_ templatesv1.ObjectReference) (schema.GroupVersionResource, bool) {
			return schema.GroupVersionResource{}, false
		})

	// WAITER goroutine — parked on wait() before the seed runs, mirroring
	// engineSeed's select arm. Records the observed transition on a channel so
	// the main goroutine can assert the handoff completed (and -race can see
	// the cross-goroutine close→read).
	waiterSaw := make(chan struct{})
	go func() {
		<-latch.wait()
		close(waiterSaw)
	}()

	// SEED goroutine — the REAL production fire site closes the latch.
	seedErr := make(chan error, 1)
	go func() {
		seedErr <- seedScopeYielding(context.Background(), nil, widgets, endpoints.Endpoint{}, nil, "authn-ns", false)
	}()

	if err := <-seedErr; err != nil {
		t.Fatalf("seedScopeYielding returned %v; want nil", err)
	}
	select {
	case <-waiterSaw:
		// Cross-goroutine handoff completed: the seed goroutine's
		// segment-complete fire unblocked the parked waiter.
	case <-time.After(2 * time.Second):
		t.Fatal("CONDITION 1: the waiter parked on firstNav.wait() was NOT unblocked by the real segment-path fire — cross-goroutine handoff broken")
	}
}

// select is the load-bearing composition and is asserted verbatim here).
func TestFirstNavLatch_BackstopUnblocksWhenLatchNeverFires(t *testing.T) {
	engineLatchTestMu.Lock()
	defer engineLatchTestMu.Unlock()

	resetFirstNavLatchForTest()
	firstNav := ensureFirstNavLatch()
	bootDone := make(chan struct{}) // stays open — boot scope still running

	// PHASE1_TIMEOUT / pipGlobalTimeout backstop, compressed for the test.
	pctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	got := make(chan string, 1)
	go func() {
		// VERBATIM the engineSeed select (phase1_walk.go). firstNav never
		// fires; bootDone never closes; pctx must win.
		select {
		case <-firstNav.wait():
			got <- "firstNav"
		case <-bootDone:
			got <- "bootDone"
		case <-pctx.Done():
			got <- "backstop"
		}
	}()

	select {
	case res := <-got:
		if res != "backstop" {
			t.Fatalf("F-C4/C2: select returned %q; want the pctx.Done() backstop when the latch never fires and boot is still running", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("F-C4/C2: readiness HUNG — the backstop did not unblock the select; /readyz would never flip (the not-Ready-forever landmine)")
	}
}

// ── mutation (ii) proof (§F.2): fire-before-segment-complete → ARM-COLD RED ─
// Directly exercises the ARM-COLD assert from the GREEN arm by FORCING an
// early fire (mutation (ii): "latch fired before the segment completes") and
// showing the GREEN arm's dashIdx guard flags it. We fire the latch before
// seeding anything, then run the same fixture; the recorded LATCH lands before
// the dashboard widget → the GREEN arm's `latchIdx < dashIdx` guard is RED.
func TestFirstNavLatch_MutationII_FireBeforeSegment_IsRedUnderGreenGuard(t *testing.T) {
	engineLatchTestMu.Lock()
	defer engineLatchTestMu.Unlock()
	zeroCustomerInFlight()
	quietLoggingE(t)

	resetFirstNavLatchForTest()
	latch := ensureFirstNavLatch()

	devs := eID{name: "devs", group: true, collapsed: 442}
	h := newNavWidgetHarvester()
	dashGVR := latchWidgetGVR("flexes")
	harvestWidgetAtRoot(h, "dashboard-flex", dashGVR, 0)
	widgets := h.snapshot()

	rec := &latchRecorder{}
	installLatchSeams(t, rec,
		func(gvr schema.GroupVersionResource) []cache.PrewarmTarget {
			if gvr == dashGVR {
				return latchTargets(gvr, devs)
			}
			return nil
		},
		func(_ templatesv1.ObjectReference) (schema.GroupVersionResource, bool) {
			return schema.GroupVersionResource{}, false
		})

	installLatchFireObserver(t, rec)
	// MUTATION (ii): fire BEFORE the segment completes (before any seed).
	latch.fire("mutation-ii-premature", 0, 0, 0)
	if err := seedScopeYielding(context.Background(), nil, widgets, endpoints.Endpoint{}, nil, "authn-ns", false); err != nil {
		t.Fatalf("seedScopeYielding returned %v; want nil", err)
	}
	ev := rec.snapshot()

	latchIdx := firstIndexOf(ev, func(e latchSeedEvent) bool { return e.class == "LATCH" })
	dashIdx := firstIndexOf(ev, func(e latchSeedEvent) bool { return e.class == "widget" && e.label == "dashboard-flex" })
	// The GREEN arm's ARM-COLD guard is `latchIdx < dashIdx → FAIL`. Prove that
	// a premature fire triggers exactly that condition (i.e. the guard
	// discriminates mutation (ii)).
	if !(latchIdx < dashIdx) {
		t.Fatalf("mutation (ii) NOT caught: premature fire did not land before the dashboard-widget seed (latch=%d dash=%d) — the GREEN ARM-COLD guard would not flag it; events=%+v", latchIdx, dashIdx, ev)
	}
}


