// phase1_cohort_independent_readiness_test.go — Ship 2 / 0.30.196
// falsifiers for the cohort-count-INDEPENDENT readiness redesign.
//
// THE LANDMINE THIS SHIPS AGAINST. Pre-0.30.196, phase1WarmupWith called
// the per-cohort PIP seed (Step 7.6) SYNCHRONOUSLY and BEFORE
// MarkPhase1Done (Step 8), and runPIPSeed FAIL-CLOSED when the enumerated
// cohort count exceeded a hardcoded cap (pipCohortCapDefault=50): it
// returned a `cohort_cap_exceeded` error, phase1WarmupWith returned
// WITHOUT calling MarkPhase1Done, and /readyz stayed 503 FOREVER. A
// customer with per-user User-kind RBAC bindings (cohort count = O(users))
// would hit cohort #51 and the pod would never go Ready.
//
// THE FIX (Ship 2 / 0.30.196):
//   - MarkPhase1Done is called immediately after the cohort-INDEPENDENT
//     substrate is warm (sync barrier + content pass) and BEFORE the
//     per-cohort seed.
//   - the per-cohort seed runs as a bounded best-effort BACKGROUND warm
//     whose outcome is log-only — it never withholds readiness.
//   - the cohort cap + the cohort_cap_exceeded fail-closed branch are
//     DELETED.
//
// THREE FALSIFIERS, each exercising a DISTINCT real code path:
//
//  1. TestRunPIPSeed_NoCapWhenClassesExceedFifty — the CAP-DELETION
//     falsifier. Drives the ACTUAL runPIPSeed (not a stub) against a
//     published RBAC snapshot whose EnumerateBindingSetClasses yields > 50
//     binding-set classes, and asserts it returns no error and emits no
//     `cohort_cap_exceeded` line. Against OLD code this FAILS (the
//     `if len(cohorts) > cap` branch fires). PM review caught that the
//     prior >50 test stubbed pipSeed with a nil-returning closure and so
//     never reached the cap check — a tautology; this replaces it.
//
//  2. TestPhase1_ReadinessFlips_BeforeBackgroundSeedCompletes — the
//     DECOUPLING falsifier. While the seed is blocked, readiness is
//     already 200. Against OLD code phase1WarmupWith HANGS on the seed.
//
//  3. TestPhase1_ReadinessFlips_WhenSeedErrors — readiness flips even
//     when the seed errors. Against OLD code a non-nil seed return skips
//     MarkPhase1Done.

package dispatchers

import (
	"context"
	"testing"
	"time"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// Ship 0.30.242 H.c-layered Phase 2c — publishNUserCohortSnapshot + itoa
// helpers DELETED alongside the cap-deletion falsifier they supported
// (TestRunPIPSeed_NoCapWhenClassesExceedFifty — see note below). The
// surviving TestPhase1_ReadinessFlips_* tests use the simpler
// phase1TestWatcher fixture (no per-cohort snapshot needed).

// Ship 0.30.242 H.c-layered Phase 2c — TestRunPIPSeed_NoCapWhenClassesExceedFifty
// DELETED. The test verified absence of a `cohort_cap_exceeded` log line
// when the enumerated cohort count exceeded 50 — but the underlying
// EnumerateBindingSetClasses + cohort-cap machinery is gone (deleted in
// commit 1d93d02). The per-binding-target enumeration (Phase 2b
// enumerateAggregatePrewarmTargets) has no cap. Coverage gap: ZERO —
// the asserted ABSENCE invariant is now structural (no cap = no
// cap-exceeded line possible).

// --- PREWARM-GATED READINESS successors (2026-07-02, shape A) ----------------
//
// LINEAGE / INVARIANT REVERSAL. The two tests below REPLACE the Ship-2/0.30.196
// falsifiers TestPhase1_ReadinessFlips_BeforeBackgroundSeedCompletes +
// _WhenSeedErrors, which asserted the OPPOSITE invariant (Ready flips
// BEFORE/despite the seed — the seed was background + non-gating). The
// prewarm-complete gate (docs/readiness-gate-prewarm-complete-2026-07-02.md,
// shape A) makes the seed run SYNCHRONOUSLY before MarkPhase1Done, so Ready now
// gates ON prewarm-complete. The successors preserve the Ship-2 503-forever
// LANDMINE GUARD (the deleted PREWARM_PIP_COHORT_CAP fail-closed-forever branch)
// by proving the backstop still flips Ready when the seed never completes.

// TestPhase1_NotReady_WhileSyncSeedInFlight is the (i) arm: while the sync seed
// is in flight (blocked), /readyz STAYS 503 (IsPhase1Done()==false). This is the
// prewarm-gate invariant — the pod does not advertise Ready until the per-cohort
// first-nav L1 is warm.
//
// RED on the wrong impl: if the flip did NOT move downstream of the seed (the
// old Ship-2 background structure), Phase1Done would be true DURING the seed →
// this FAILS. Discriminates the moved-flip.
func TestPhase1_NotReady_WhileSyncSeedInFlight(t *testing.T) {
	rw := phase1TestWatcher(t)
	cache.ResetPhase1DoneForTest()
	cache.ResetNavigationDiscoveredGroupsForTest()
	t.Cleanup(cache.ResetPhase1DoneForTest)
	t.Cleanup(cache.ResetNavigationDiscoveredGroupsForTest)

	lister := func(ctx context.Context) ([]navigationRoot, error) {
		return []navigationRoot{{Root: routesLoaderCR("ns-a", "main"), GVR: gvrReached}}, nil
	}
	resolver := func(ctx context.Context, root navigationRoot) error {
		rw.EnsureResourceType(gvrReached)
		return nil
	}

	// The seed blocks until the test releases it. Under shape A,
	// phase1WarmupWith BLOCKS on the seed — so run it in a goroutine and probe
	// readiness while the seed is held.
	release := make(chan struct{})
	seedEntered := make(chan struct{})
	pipSeed := func(ctx context.Context) error {
		close(seedEntered)
		<-release
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	warmupReturned := make(chan struct{})
	go func() {
		_ = phase1WarmupWith(ctx, rw, lister, resolver, nil, nil, pipSeed, nil)
		close(warmupReturned)
	}()

	// Wait until the seed is genuinely in flight.
	select {
	case <-seedEntered:
	case <-time.After(5 * time.Second):
		t.Fatalf("prewarm-gate: sync seed never entered")
	}

	// INVARIANT (i): readiness is STILL 503 while the seed is in flight.
	if cache.IsPhase1Done() {
		t.Fatalf("PREWARM-GATE FAIL: Phase1Done is TRUE while the sync seed is still in "+
			"flight — readiness must gate ON prewarm-complete (the flip did not move "+
			"downstream of the seed). RED = old Ship-2 background structure.")
	}
	// phase1WarmupWith must NOT have returned yet (it blocks on the seed).
	select {
	case <-warmupReturned:
		t.Fatalf("PREWARM-GATE FAIL: phase1WarmupWith returned while the seed is still "+
			"blocked — the seed is not synchronous")
	default:
	}

	// Release → seed completes → flip fires → warmup returns → Ready.
	close(release)
	select {
	case <-warmupReturned:
	case <-time.After(5 * time.Second):
		t.Fatalf("prewarm-gate: phase1WarmupWith did not return after seed release")
	}
	if !cache.IsPhase1Done() {
		t.Fatalf("prewarm-gate: Phase1Done still false after the seed completed — the "+
			"post-seed flip did not fire")
	}
}

// TestPhase1_Ready_WhenSeedNeverCompletes_Backstop is the (ii) arm — the
// MANDATORY anti-landmine guard (the Ship-2 503-forever landmine, re-expressed
// for shape A). A seed that NEVER completes on its own must NOT wedge readiness
// forever: the backstop (the seed's pipGlobalTimeout child-ctx off the warmup
// ctx) cancels the seed, it returns, and MarkPhase1Done fires REGARDLESS →
// Ready-degraded, not not-Ready-forever.
//
// The real backstop is pipGlobalTimeout (8 min); the test drives the SAME
// timeout PATH via a short WARMUP ctx (the seed ctx is its child, so warmup-ctx
// expiry cancels the seed ctx exactly as pipGlobalTimeout would). The seed
// blocks on its ctx and returns ctx.Err() on cancel — modelling a stuck seed.
//
// RED (no backstop / flip not fire-regardless): phase1WarmupWith never flips →
// IsPhase1Done() stays false → this FAILS (permanent-outage, the landmine).
func TestPhase1_Ready_WhenSeedNeverCompletes_Backstop(t *testing.T) {
	rw := phase1TestWatcher(t)
	cache.ResetPhase1DoneForTest()
	cache.ResetNavigationDiscoveredGroupsForTest()
	t.Cleanup(cache.ResetPhase1DoneForTest)
	t.Cleanup(cache.ResetNavigationDiscoveredGroupsForTest)

	lister := func(ctx context.Context) ([]navigationRoot, error) {
		return []navigationRoot{{Root: routesLoaderCR("ns-a", "main"), GVR: gvrReached}}, nil
	}
	resolver := func(ctx context.Context, root navigationRoot) error {
		rw.EnsureResourceType(gvrReached)
		return nil
	}

	// A stuck seed: blocks until its ctx (the pipGlobalTimeout child of the
	// warmup ctx) is cancelled, then returns the ctx error — it NEVER completes
	// on its own.
	seedObservedCancel := make(chan struct{})
	pipSeed := func(ctx context.Context) error {
		<-ctx.Done()
		close(seedObservedCancel)
		return ctx.Err()
	}

	// Short WARMUP ctx = the backstop-path proxy (drives the same seed-ctx-cancel
	// that pipGlobalTimeout would at 8 min). 2s so the test is fast.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	// phase1WarmupWith blocks until the backstop fires; it must RETURN (not hang)
	// and flip Ready.
	_ = phase1WarmupWith(ctx, rw, lister, resolver, nil, nil, pipSeed, nil)
	elapsed := time.Since(start)

	// The seed observed its ctx cancel (the backstop fired).
	select {
	case <-seedObservedCancel:
	case <-time.After(3 * time.Second):
		t.Fatalf("backstop: the stuck seed never observed a ctx cancel — the backstop path did not fire")
	}
	// LANDMINE GUARD: readiness FLIPPED despite the seed never completing.
	if !cache.IsPhase1Done() {
		t.Fatalf("BACKSTOP FAIL (503-FOREVER LANDMINE): Phase1Done is FALSE after a "+
			"never-completing seed — the pod would be not-Ready FOREVER. MarkPhase1Done "+
			"must fire REGARDLESS (the fire-regardless defer / backstop is missing).")
	}
	// Sanity: it flipped at ~the backstop, not instantly (proves it waited for
	// the seed's timeout path, not a premature flip).
	if elapsed < 1*time.Second {
		t.Fatalf("backstop: flipped in %v — too fast; the seed was not actually gated "+
			"(readiness must wait for the backstop, ~the warmup-ctx/pipGlobalTimeout deadline)", elapsed)
	}
}
