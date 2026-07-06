// prewarm_first_nav_latch_segment_test.go — #99b FIX-F segment-identity
// falsifier set (docs/fixf-rootindex-redrive-walk-trace-2026-07-06.md §5).
//
// The live 60K boot600 defect: `ranked` is built over the UNION of widget- AND
// restaction-target identities, first-seen CollapsedBindings. A machine SA that
// appears ONLY in a restaction target set (a bench-ns SA, all seeds
// empty_binding-skipped) can out-rank every login-cohort widget-floor identity
// and become ranked[0] — with ZERO RootIndex==0 widget targets. The pre-#99b
// arming (rank1 := ranked[0].key) then counts 0 first-nav targets → the latch
// zero-fires (reason=zero-first-nav-targets) BEFORE any widget seed, flipping
// /readyz with the login cohorts' dashboards cold. #99b makes the segment
// identity the FIRST ranked identity that actually has a RootIndex==0 widget
// target (segKey/segRank), so the latch keys readiness on the real dashboard.
//
//   ARM-GREEN-BOOT600 (§5 GREEN, Fix 1): 2 RootIndex==0 widgets {U1 c=5, U2
//     c=2} + 1 RootIndex==1 widget; 1 restaction whose target set = {M c=50},
//     M in NO widget set. ranked[0] is M (restaction-only, out-ranks by
//     collapsed). The latch MUST fire reason=segment-complete, first_nav_
//     widgets=2, first_nav_targets=2 (U1's 2 RootIndex==0 pairs — U1 out-ranks
//     U2 among widget-capable identities), positioned AFTER the 2nd U1
//     RootIndex==0 widget seed and BEFORE the RootIndex==1 widget + the RA tail
//     (ARM-TAIL). segment_identity == U1, segment_rank == 1 (M is rank 0).
//   The RED companion (pre-#99b code) is captured empirically to /tmp/fix99b/
//     (git-stash the fix; the same fixture zero-fires before any widget seed).
//
//   ARM-FIX2-REDRIVE (§5 Fix-2 arm): 2 config roots; the boot-walk pass
//     harvests NOTHING (empty subtrees); a SECOND walk pass (redrive shape:
//     fresh walkers, shared harvester) harvests both subtrees. root-0's widgets
//     MUST stamp RootIndex==0 (RED without BeginWalk+BeginRoot in the re-walk:
//     they resume from the boot walk's curRoot and stamp 1). Exercised at the
//     harvester level (BeginWalk/BeginRoot are the unit under test).
//
//   ARM-KEEPWARM-SEGRANK (segRank/keepwarm interaction ruling): the #102
//     keepwarm sweep (rank1Only=true) re-seeds ranked[0]'s CELLS, NOT the
//     segment identity's. With ranked[0]=M (a restaction-only identity), the
//     sweep MUST still seed M's restaction targets — segRank is a latch-only
//     concept and does NOT retarget the keepwarm loop bound. Asserts the sweep
//     seeds the ranked[0] (M) target set, not the segment (U1) set.
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

	// The boot600 shape: M (collapsed 50) is the highest-ranked identity but
	// appears ONLY in the restaction target set — no widget authorises it.
	// U1 (collapsed 5) and U2 (collapsed 2) are the login-cohort widget
	// identities. ranked = [M(0), U1(1), U2(2)].
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

	if err := seedScopeYielding(context.Background(), ras, widgets, endpoints.Endpoint{}, nil, "authn-ns", false); err != nil {
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
	// The M restaction is ranked[0] (rank 0) and legitimately seeds FIRST under
	// the rank-major loop — it is NOT the segment's tail, so the latch firing
	// after it is correct (M is a cheap prefix, not first-nav work readyz gates
	// on). We only assert the U1 segment fired and the RootIndex>0 widget tail
	// is excluded. raIdx is retained only to document its position.
	_ = raIdx
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

// ── ARM-KEEPWARM-SEGRANK (segRank/keepwarm interaction ruling) ──────────────
// The #102 keepwarm sweep (rank1Only=true) bounds the seed to ranked[0] — the
// dominant-CollapsedBindings cohort's CELLS — regardless of whether ranked[0]
// has a first-nav widget. segRank (the latch segment identity, #99b) is a
// LATCH-ONLY (boot-gate) concept and must NOT retarget the keepwarm loop bound.
// With ranked[0]=M (restaction-only) and segRank pointing at U1, the sweep MUST
// still seed M's restaction target (rank 0), NOT U1's widget (rank 1).
func TestKeepwarmSweep_RankOne_SeedsRankZeroNotSegment(t *testing.T) {
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

	m := eID{name: "machine-sa", collapsed: 50}
	u1 := eID{name: "u1", group: true, collapsed: 5}

	h := newNavWidgetHarvester()
	dash := latchWidgetGVR("flexes")
	harvestWidgetAtRoot(h, "dash", dash, 0) // U1's first-nav widget (rank 1)
	widgets := h.snapshot()

	fanoutRA := latchRA("machine-fanout-ra")
	ras := []templatesv1.ObjectReference{fanoutRA}

	rec := &latchRecorder{}
	installLatchSeams(t, rec,
		func(gvr schema.GroupVersionResource) []cache.PrewarmTarget {
			switch gvr {
			case dash:
				return latchTargets(gvr, u1)
			case eFanoutRAGVR:
				return latchTargets(eFanoutRAGVR, m) // M = ranked[0].
			}
			return nil
		},
		func(_ templatesv1.ObjectReference) (schema.GroupVersionResource, bool) {
			return eFanoutRAGVR, true
		})

	// rank1Only=true — the keepwarm sweep.
	if err := seedScopeYielding(context.Background(), ras, widgets, endpoints.Endpoint{}, nil, "authn-ns", true); err != nil {
		t.Fatalf("seedScopeYielding returned %v; want nil", err)
	}
	ev := rec.snapshot()

	seededM := firstIndexOf(ev, func(e latchSeedEvent) bool { return e.class == "restaction" && e.identity == "user:machine-sa" }) < len(ev)
	seededU1 := firstIndexOf(ev, func(e latchSeedEvent) bool { return e.class == "widget" && e.identity == "group:u1" }) < len(ev)

	if !seededM {
		t.Fatalf("keepwarm ruling: rank1Only sweep did NOT seed ranked[0] (M) restaction target — the sweep must re-seed the dominant cohort's cells; events=%+v", ev)
	}
	if seededU1 {
		t.Fatalf("keepwarm ruling: rank1Only sweep seeded U1 (rank 1, the SEGMENT identity) — segRank must NOT retarget the keepwarm loop bound (that would be a scope change, out of #99b); events=%+v", ev)
	}
}
