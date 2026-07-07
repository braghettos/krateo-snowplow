// prewarm_first_nav_latch_segment_test.go — #99b FIX-F segment-identity
// falsifier set (docs/fixf-rootindex-redrive-walk-trace-2026-07-06.md §5),
// updated 2026-07-07 for FIX-3 rank hygiene
// (docs/fix3-rank-hygiene-design-2026-07-07.md §7).
//
// Pre-FIX-3 defect: `ranked` was built over the UNION of widget- AND
// restaction-target identities by FIRST-SEEN CollapsedBindings. A machine SA
// present ONLY in a restaction target set (a bench-ns SA, all seeds
// empty_binding-skipped) out-ranked every login-cohort widget-floor identity
// and became ranked[0] — with ZERO RootIndex==0 widget targets. #99b's segKey
// scan then keyed the latch on the FIRST ranked identity with a RootIndex==0
// widget target (not ranked[0]) so /readyz gated on the real dashboard.
//
// FIX-3 changes WHO holds each rank: widgetMax DESC now places every
// widget-capable identity strictly above every widget-less one, so on a
// SINGLE-root topology the segment identity IS ranked[0] and segRank binds 0
// (the machine SA drops to the tail). The #99b scan is retained as a SAFETY NET
// for the MULTI-root case — an identity whose only widget targets are in a
// non-first root (RootIndex!=0) can still take ranked[0] yet have zero
// FIRST-NAV targets → segRank>0. TestFirstNavLatch_SegmentIdentity_MultiRoot_*
// below is the mandatory arm that keeps that scan discriminating post-FIX-3.
//
//   ARM-GREEN-BOOT600 (§5 GREEN, now FIX-3 premise): 2 RootIndex==0 widgets {U1
//     c=5, U2 c=2} + 1 RootIndex==1 widget; 1 restaction whose target set = {M
//     c=50}, M in NO widget set. Post-FIX-3 ranked = [U1(widgetMax 5),
//     U2(widgetMax 2), M(widgetMax 0)] — M's 50 no longer wins because it has
//     zero widget observations. The latch MUST fire reason=segment-complete,
//     first_nav_widgets=2, first_nav_targets=2 (U1's 2 RootIndex==0 pairs),
//     positioned AFTER the 2nd U1 RootIndex==0 widget seed and BEFORE the
//     RootIndex==1 widget (ARM-TAIL). segment_identity == U1, segment_rank == 0
//     (U1 is ranked[0] post-FIX-3, not rank 1).
//
//   ARM-FIX2-REDRIVE (§5 Fix-2 arm): 2 config roots; the boot-walk pass
//     harvests NOTHING (empty subtrees); a SECOND walk pass (redrive shape:
//     fresh walkers, shared harvester) harvests both subtrees. root-0's widgets
//     MUST stamp RootIndex==0 (RED without BeginWalk+BeginRoot in the re-walk:
//     they resume from the boot walk's curRoot and stamp 1). Exercised at the
//     harvester level (BeginWalk/BeginRoot are the unit under test).
//
//   ARM-KEEPWARM-SEGRANK (segRank/keepwarm interaction ruling, FIX-3 divergence
//     fixture): the #102 keepwarm sweep (rank1Only=true) re-seeds ranked[0]'s
//     CELLS, NOT the segment identity's. segRank is a latch-only concept and
//     does NOT retarget the keepwarm loop bound. Post-FIX-3 the pre-FIX-3
//     fixture (ranked[0]=RA-only M) no longer diverges — M drops to the tail
//     and ranked[0]=U1 is BOTH widget-capable AND the segment, so it would not
//     discriminate. TestKeepwarmSweep_RankOne_SeedsRankZeroNotSegment now uses
//     a MULTI-ROOT divergence fixture: W (widgetMax 9, but ALL its widgets are
//     RootIndex==1) is ranked[0]; the segment identity is V (a lower-ranked
//     identity with a RootIndex==0 widget) at segRank>0. The sweep MUST seed W
//     (ranked[0]), NOT V (the segment) — proving the loop bound follows
//     ranked[0], not segKey.
//
// Hermetic, -race, seams only. Serializes on engineLatchTestMu.

package dispatchers

import (
	"context"
	"testing"

	"github.com/krateoplatformops/plumbing/endpoints"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// ── ARM-GREEN-BOOT600 (§5 GREEN, Fix 1) ─────────────────────────────────────
func TestFirstNavLatch_SegmentIdentity_RestactionOnlyRank0_FiresOnWidgetSegment(t *testing.T) {
	engineLatchTestMu.Lock()
	defer engineLatchTestMu.Unlock()
	zeroCustomerInFlight()
	quietLoggingE(t)

	resetFirstNavLatchForTest()
	ensureFirstNavLatch()

	// The boot600 shape: M (collapsed 50) appears ONLY in the restaction target
	// set — no widget authorises it, so post-FIX-3 its widgetMax is 0. U1
	// (widgetMax 5) and U2 (widgetMax 2) are the login-cohort widget identities.
	// FIX-3 ranked = [U1(0), U2(1), M(2)] — widgetMax DESC drops the RA-only M
	// to the tail; U1 is ranked[0] and segRank binds 0.
	m := eID{name: "machine-sa", collapsed: 50}
	u1 := eID{name: "u1", group: true, collapsed: 5}
	u2 := eID{name: "u2", group: true, collapsed: 2}

	h := newNavWidgetHarvester()
	dashA := latchWidgetGVR("flexes")      // RootIndex 0 — first-nav, U1 target
	dashB := latchWidgetGVR("estategraphs") // RootIndex 0 — first-nav, U1 target
	tail := latchWidgetGVR("tailwidgets")   // RootIndex 1 — NON-first-nav, U2 target
	harvestWidgetAtRoot(h, "dash-a", dashA, 0)
	harvestWidgetAtRoot(h, "dash-b", dashB, 0)
	harvestWidgetAtRoot(h, "tail-w", tail, 1)
	widgets := h.snapshot()

	fanoutRA := latchRA("machine-fanout-ra")
	ras := []templatesv1.ObjectReference{fanoutRA}

	rec := &latchRecorder{}
	installLatchSeams(t, rec,
		func(gvr schema.GroupVersionResource) []cache.PrewarmTarget {
			switch gvr {
			case dashA, dashB:
				// Both RootIndex==0 dashboard widgets authorise U1 (2 targets each
				// would over-count; give ONE U1 target per widget so first_nav_
				// targets == 2 == first_nav_widgets).
				return latchTargets(gvr, u1)
			case tail:
				return latchTargets(gvr, u2)
			case eFanoutRAGVR:
				return latchTargets(eFanoutRAGVR, m) // M appears ONLY here.
			}
			return nil
		},
		func(_ templatesv1.ObjectReference) (schema.GroupVersionResource, bool) {
			return eFanoutRAGVR, true
		})

	// Capture the fire reason + segment fields via a richer observer.
	var fireReason string
	firstNavFireObserver = func(reason string) {
		fireReason = reason
		rec.rec(latchSeedEvent{class: "LATCH", label: reason, rootIdx: -1})
	}
	t.Cleanup(func() { firstNavFireObserver = nil })

	if err := seedScopeYielding(context.Background(), ras, widgets, endpoints.Endpoint{}, nil, "authn-ns", seedModeBoot); err != nil {
		t.Fatalf("seedScopeYielding returned %v; want nil", err)
	}
	ev := rec.snapshot()

	// GREEN: fired segment-complete, NOT zero-first-nav-targets (the pre-#99b
	// RED reason). This is the load-bearing discriminator.
	if fireReason != "segment-complete" {
		t.Fatalf("§5 GREEN: latch fired reason=%q; want \"segment-complete\" — the pre-#99b code would fire \"zero-first-nav-targets\" (ranked[0]=M has no first-nav widget); events=%+v", fireReason, ev)
	}

	latchIdx := firstIndexOf(ev, func(e latchSeedEvent) bool { return e.class == "LATCH" })
	// The two RootIndex==0 U1 widgets seed under identity group:u1.
	dashAIdx := firstIndexOf(ev, func(e latchSeedEvent) bool { return e.class == "widget" && e.label == "dash-a" && e.identity == "group:u1" })
	dashBIdx := firstIndexOf(ev, func(e latchSeedEvent) bool { return e.class == "widget" && e.label == "dash-b" && e.identity == "group:u1" })
	tailIdx := firstIndexOf(ev, func(e latchSeedEvent) bool { return e.class == "widget" && e.label == "tail-w" })
	raIdx := firstIndexOf(ev, func(e latchSeedEvent) bool { return e.class == "restaction" })

	if latchIdx == len(ev) {
		t.Fatalf("§5 GREEN: latch never fired; events=%+v", ev)
	}
	// Fire AFTER both U1 RootIndex==0 widgets seed (segment complete)...
	lastSegIdx := dashAIdx
	if dashBIdx > lastSegIdx {
		lastSegIdx = dashBIdx
	}
	if latchIdx < lastSegIdx {
		t.Fatalf("§5 GREEN: latch fired at idx %d BEFORE the U1 first-nav segment completed (last U1 widget at idx %d) — ARM-COLD; events=%+v", latchIdx, lastSegIdx, ev)
	}
	// ...and BEFORE the RootIndex==1 NON-first-nav tail widget (U2, rank 2) —
	// the segment identity's own tail. ARM-TAIL: readyz must not wait on it.
	if tailIdx < len(ev) && latchIdx > tailIdx {
		t.Fatalf("§5 GREEN/ARM-TAIL: latch fired at idx %d AFTER the RootIndex==1 tail widget (U2, rank 2) at idx %d — readyz would wait on non-first-nav tail; events=%+v", latchIdx, tailIdx, ev)
	}
	// Post-FIX-3 the M restaction is ranked[2] (widgetMax 0) and legitimately
	// seeds LAST under the rank-major loop — it is NOT the segment (U1 rank 0),
	// so the latch fires at U1's segment before M's RA tail ever runs. ARM-TAIL:
	// readyz must not wait on the RA-only tail. Assert the latch fired at or
	// before the RA (M) seed to pin the segment-before-tail order under FIX-3.
	if raIdx < len(ev) && latchIdx > raIdx {
		t.Fatalf("§5 GREEN/ARM-TAIL: latch fired at idx %d AFTER the RA-only tail (M, rank 2) at idx %d — post-FIX-3 M is the widget-less tail, readyz must not wait on it; events=%+v", latchIdx, raIdx, ev)
	}
}

// ── COND-2 (FIX-3 mandatory): MULTI-ROOT segKey scan stays discriminating ───
// Post-FIX-3, ranked[0] is widget-capable by construction — but "widget-capable"
// means >=1 widget target ANYWHERE, not >=1 RootIndex==0 (first-nav) target. On
// a MULTI-ROOT config an identity whose ONLY widget targets live in a non-first
// root can take ranked[0] yet have ZERO first-nav targets. The #99b segKey scan
// is the guard that walks past it to the first identity with a genuine
// RootIndex==0 widget. This arm re-covers the retired #99b "scan walks past
// ranked[0]" property under FIX-3:
//   (i) ranked[0] = W (widgetMax 9) but ALL W's widgets are RootIndex==1;
//   (ii) V (widgetMax 2) has a RootIndex==0 widget → segKey==V, segRank>0;
//   (iii) the latch fires reason=segment-complete on V's FIRST-NAV segment.
// RED (captured to /tmp/r3/ empirically): neuter the scan (segRank hard-bound
// to 0) → the latch counts 0 first-nav targets on W → fires
// "zero-first-nav-targets" BEFORE V's RootIndex==0 widget → the fire-after-V's-
// widget + reason=segment-complete asserts below both fail.
func TestFirstNavLatch_SegmentIdentity_MultiRoot_ScanFindsFirstNavWidget(t *testing.T) {
	engineLatchTestMu.Lock()
	defer engineLatchTestMu.Unlock()
	zeroCustomerInFlight()
	quietLoggingE(t)

	resetFirstNavLatchForTest()
	ensureFirstNavLatch()

	// W (widgetMax 9) is ranked[0], widget lives in RootIndex==1 only. V
	// (widgetMax 2) has a RootIndex==0 widget → the segment identity, segRank>0.
	w := eID{name: "w", group: true, collapsed: 9}
	v := eID{name: "v", group: true, collapsed: 2}

	h := newNavWidgetHarvester()
	firstNavW := latchWidgetGVR("flexes")     // RootIndex 0 — V's first-nav widget
	nonFirstNavW := latchWidgetGVR("tailwid") // RootIndex 1 — W's only widget
	harvestWidgetAtRoot(h, "first-nav", firstNavW, 0)
	harvestWidgetAtRoot(h, "non-first-nav", nonFirstNavW, 1)
	widgets := h.snapshot()

	rec := &latchRecorder{}
	installLatchSeams(t, rec,
		func(gvr schema.GroupVersionResource) []cache.PrewarmTarget {
			switch gvr {
			case firstNavW:
				return latchTargets(gvr, v) // V, RootIndex 0 → segment
			case nonFirstNavW:
				return latchTargets(gvr, w) // W, RootIndex 1 → ranked[0], not first-nav
			}
			return nil
		},
		func(_ templatesv1.ObjectReference) (schema.GroupVersionResource, bool) {
			return schema.GroupVersionResource{}, false
		})

	var fireReason string
	firstNavFireObserver = func(reason string) {
		fireReason = reason
		rec.rec(latchSeedEvent{class: "LATCH", label: reason, rootIdx: -1})
	}
	t.Cleanup(func() { firstNavFireObserver = nil })

	if err := seedScopeYielding(context.Background(), nil, widgets, endpoints.Endpoint{}, nil, "authn-ns", seedModeBoot); err != nil {
		t.Fatalf("seedScopeYielding returned %v; want nil", err)
	}
	ev := rec.snapshot()

	// (iii) fired segment-complete — NOT zero-first-nav-targets (the neutered-
	// scan RED reason: W has no first-nav widget so a segRank-hard-0 impl would
	// find nothing and fire the provably-zero path).
	if fireReason != "segment-complete" {
		t.Fatalf("COND-2: latch fired reason=%q; want \"segment-complete\" — with the #99b scan neutered (segRank hard-bound to ranked[0]=W, which has zero RootIndex==0 widgets) the latch would fire \"zero-first-nav-targets\"; events=%+v", fireReason, ev)
	}
	// (ii)+(iii) the latch fired AFTER V's RootIndex==0 widget seeded — i.e. it
	// keyed on the true first-nav segment (segRank>0, segKey==V), not ranked[0].
	latchIdx := firstIndexOf(ev, func(e latchSeedEvent) bool { return e.class == "LATCH" })
	vWidgetIdx := firstIndexOf(ev, func(e latchSeedEvent) bool { return e.class == "widget" && e.identity == "group:v" && e.rootIdx == 0 })
	if latchIdx == len(ev) {
		t.Fatalf("COND-2: latch never fired; events=%+v", ev)
	}
	if vWidgetIdx == len(ev) {
		t.Fatalf("COND-2 setup: V's RootIndex==0 widget never seeded; events=%+v", ev)
	}
	if latchIdx < vWidgetIdx {
		t.Fatalf("COND-2: latch fired at idx %d BEFORE V's first-nav (RootIndex==0) widget at idx %d — the scan did NOT walk past ranked[0]=W to the real first-nav segment; events=%+v", latchIdx, vWidgetIdx, ev)
	}
}

// ── ARM-FIX2-REDRIVE (§5 Fix-2 arm) ─────────────────────────────────────────
// Two config roots; the boot-walk pass harvests nothing; a SECOND walk pass
// (redrive shape) harvests both subtrees. Without BeginWalk()+per-root
// BeginRoot() on the re-walk, the second pass resumes from the boot walk's
// final curRoot and stamps root-0 widgets with the WRONG RootIndex. Exercised
// directly on the harvester (the unit BeginWalk/BeginRoot govern).
func TestNavWidgetHarvester_RedriveWalk_StampsRootIndexZeroForRootZero(t *testing.T) {
	h := newNavWidgetHarvester()
	r0w := latchWidgetGVR("root0widgets")
	r1w := latchWidgetGVR("root1widgets")

	// PASS 1 (boot walk): two roots, but both subtrees are empty (the boot
	// walk reached no widget — the 50K+ boot-race shape where the effective
	// harvest is the config-vars redrive). Advance curRoot per root anyway
	// (mirrors the boot walk's resolver closure) so PASS 1 ends at curRoot=1.
	h.BeginWalk()
	h.BeginRoot() // root 0
	h.BeginRoot() // root 1
	// (no harvestWidgetAtRoot — subtrees empty)

	// PASS 2 (engine re-walk / redrive): fresh walkers, SAME harvester. With
	// Fix 2, the re-walk calls BeginWalk() (curRoot→-1) then BeginRoot() per
	// root, so root-0's widget stamps RootIndex 0.
	h.BeginWalk() // Fix 2: reset to -1
	h.BeginRoot() // root 0 → curRoot 0
	harvestWidgetAtRootNoReset(h, "r0-widget", r0w)
	h.BeginRoot() // root 1 → curRoot 1
	harvestWidgetAtRootNoReset(h, "r1-widget", r1w)

	got := map[string]int{}
	for _, e := range h.snapshot() {
		got[e.W.GetName()] = e.RootIndex
	}
	if got["r0-widget"] != 0 {
		t.Fatalf("Fix-2: root-0 widget stamped RootIndex %d; want 0 (RED without the re-walk BeginWalk reset: it resumes at curRoot=1 and stamps 2). got=%+v", got["r0-widget"], got)
	}
	if got["r1-widget"] != 1 {
		t.Fatalf("Fix-2: root-1 widget stamped RootIndex %d; want 1; got=%+v", got["r1-widget"], got)
	}
}

// harvestWidgetAtRootNoReset stamps a widget through the REAL harvester WITHOUT
// advancing curRoot (the caller controls BeginRoot). It records at the
// harvester's CURRENT curRoot — so the assert sees the root index the
// BeginWalk/BeginRoot sequence produced, not a hand-assigned value.
func harvestWidgetAtRootNoReset(h *navWidgetHarvester, name string, gvr schema.GroupVersionResource) {
	eHarvestWidget(h, name, gvr)
}

// ── ARM-KEEPWARM-SEGRANK (segRank/keepwarm interaction ruling, FIX-3 + c2) ───
// keepwarm c2 WIDENS this arm (design §6 c1-arm table): the keepwarm sweep now
// bounds the seed to the WIDGET-CAPABLE PREFIX (widgetMax>=1), not ranked[0]
// only — so it seeds W AND V (both widget-capable). The #99b ruling this arm
// encodes SURVIVES the widening: the loop bound is the widgetMax TIER, still NOT
// segRank. segRank (the latch segment identity, #99b) is a LATCH-ONLY (boot-gate)
// concept and must NOT retarget the keepwarm loop bound — this arm pins that the
// sweep's coverage follows the widgetMax tier regardless of which identity is
// the first-nav segment.
//
// DIVERGENCE fixture (ranked[0] != segKey): W (widgetMax 9) is ranked[0] but ALL
// its widgets are RootIndex==1 → segRank>0 (the segment falls to V, a
// lower-ranked identity with a RootIndex==0 widget). Both are widget-capable, so
// c2 sweeps BOTH; the assert is that the sweep coverage is the widget-capable
// tier (W and V), NOT keyed on segRank (which would, wrongly, retarget the loop
// to only V or reorder around it). (The multi-root first-nav LATCH behaviour
// itself is pinned by TestFirstNavLatch_SegmentIdentity_MultiRoot_ScanFindsFirstNavWidget
// below.)
func TestKeepwarmSweep_WidgetCapablePrefix_SeedsBothNotSegmentOnly(t *testing.T) {
	engineLatchTestMu.Lock()
	defer engineLatchTestMu.Unlock()
	zeroCustomerInFlight()
	quietLoggingE(t)

	// The latch has already fired at boot in production; a keepwarm sweep runs
	// with the singleton already closed. Reset+prefire so the sweep's fire
	// calls are the idempotent no-ops they are in prod.
	resetFirstNavLatchForTest()
	latch := ensureFirstNavLatch()
	latch.fire("boot-already-fired", 0, 0, "", -1, 0)

	// W (widgetMax 9) is ranked[0], but its widget is RootIndex==1 (non-first-
	// nav) → segRank>0. V (widgetMax 2) has a RootIndex==0 widget → V is the
	// segment identity, at a lower rank. ranked = [W(0), V(1)]; BOTH widget-capable.
	w := eID{name: "w", group: true, collapsed: 9}
	v := eID{name: "v", group: true, collapsed: 2}

	h := newNavWidgetHarvester()
	firstNavW := latchWidgetGVR("flexes")     // RootIndex 0 — V's first-nav widget
	nonFirstNavW := latchWidgetGVR("tailwid") // RootIndex 1 — W's only widget
	harvestWidgetAtRoot(h, "first-nav", firstNavW, 0)
	harvestWidgetAtRoot(h, "non-first-nav", nonFirstNavW, 1)
	widgets := h.snapshot()

	rec := &latchRecorder{}
	installLatchSeams(t, rec,
		func(gvr schema.GroupVersionResource) []cache.PrewarmTarget {
			switch gvr {
			case firstNavW:
				return latchTargets(gvr, v) // V, RootIndex 0 → the segment
			case nonFirstNavW:
				return latchTargets(gvr, w) // W, RootIndex 1 → ranked[0], not first-nav
			}
			return nil
		},
		func(_ templatesv1.ObjectReference) (schema.GroupVersionResource, bool) {
			return schema.GroupVersionResource{}, false
		})

	// seedModeKeepwarm — the c2 keepwarm sweep (widget-capable prefix).
	if err := seedScopeYielding(context.Background(), nil, widgets, endpoints.Endpoint{}, nil, "authn-ns", seedModeKeepwarm); err != nil {
		t.Fatalf("seedScopeYielding returned %v; want nil", err)
	}
	ev := rec.snapshot()

	seededW := firstIndexOf(ev, func(e latchSeedEvent) bool { return e.class == "widget" && e.identity == "group:w" }) < len(ev)
	seededV := firstIndexOf(ev, func(e latchSeedEvent) bool { return e.class == "widget" && e.identity == "group:v" }) < len(ev)

	if !seededW {
		t.Fatalf("c2 keepwarm ruling: sweep did NOT seed ranked[0] (W, widgetMax 9) — the widget-capable prefix must include ranked[0]; events=%+v", ev)
	}
	if !seededV {
		t.Fatalf("c2 keepwarm ruling: sweep did NOT seed V (widgetMax 2, widget-capable) — c2 WIDENS coverage to the whole widget-capable prefix, not ranked[0] only; the #99b property (bound is the widgetMax tier, NOT segRank) must still seed V; events=%+v", ev)
	}
}
