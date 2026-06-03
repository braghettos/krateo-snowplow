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
	"errors"
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

// TestPhase1_ReadinessFlips_BeforeBackgroundSeedCompletes proves the
// decoupling directly: while the per-cohort seed is still in flight
// (blocked), readiness is ALREADY 200. This is the cohort-count-
// INDEPENDENT boot-wall-clock invariant — the pod goes Ready on the
// substrate alone and the seed warms behind it.
//
// A regression that runs the seed synchronously before MarkPhase1Done
// would block phase1WarmupWith on the seed; the assertion that
// phase1WarmupWith has returned AND Phase1Done is true WHILE the seed is
// still blocked would FAIL.
func TestPhase1_ReadinessFlips_BeforeBackgroundSeedCompletes(t *testing.T) {
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

	// The seed blocks until the test releases it. If readiness were gated
	// on the seed (the old synchronous path), phase1WarmupWith would block
	// here and the test would hit its own deadline.
	release := make(chan struct{})
	seedEntered := make(chan struct{})
	pipSeed := func(ctx context.Context) error {
		close(seedEntered)
		<-release
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// phase1WarmupWith MUST return promptly — it launches the seed in the
	// background and does not wait on it.
	if err := phase1WarmupWith(ctx, rw, lister, resolver, nil, nil, pipSeed, nil); err != nil {
		t.Fatalf("Ship 2: phase1WarmupWith returned error: %v", err)
	}

	// Readiness is already flipped while the background seed is still blocked.
	if !cache.IsPhase1Done() {
		t.Fatalf("Ship 2 FAIL: Phase1Done is false after phase1WarmupWith returned, "+
			"while the background seed is still blocked — readiness must not wait on the seed")
	}

	// Confirm the seed is genuinely in flight (entered, not yet released) —
	// proving phase1WarmupWith returned WITHOUT waiting for the seed.
	select {
	case <-seedEntered:
		// good — the background goroutine is running and blocked.
	case <-time.After(5 * time.Second):
		t.Fatalf("Ship 2: background seed goroutine never started")
	}

	// Release the seed so the background goroutine can exit cleanly before
	// test teardown.
	close(release)
}

// TestPhase1_ReadinessFlips_WhenSeedErrors proves that even a per-cohort
// seed that ERRORS (e.g. a pathological large topology that times out)
// does not withhold readiness. The seed's outcome is log-only.
//
// Pre-0.30.196 a non-nil pipSeed return caused phase1WarmupWith to return
// WITHOUT calling MarkPhase1Done — this assertion would FAIL.
func TestPhase1_ReadinessFlips_WhenSeedErrors(t *testing.T) {
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

	seedDone := make(chan struct{})
	pipSeed := func(ctx context.Context) error {
		defer close(seedDone)
		return errors.New("simulated large-topology seed failure")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := phase1WarmupWith(ctx, rw, lister, resolver, nil, nil, pipSeed, nil); err != nil {
		t.Fatalf("Ship 2: phase1WarmupWith must NOT propagate a background-seed error as "+
			"a fatal return: %v", err)
	}
	if !cache.IsPhase1Done() {
		t.Fatalf("Ship 2 FAIL: Phase1Done is false after a seed error — the background "+
			"seed must never withhold readiness")
	}

	// Let the background goroutine observe its error + exit before teardown.
	select {
	case <-seedDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("Ship 2: background seed goroutine never ran")
	}
}
