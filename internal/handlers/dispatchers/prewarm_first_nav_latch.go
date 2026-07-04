// prewarm_first_nav_latch.go — #99 FIX-F: the first-nav readyz latch.
//
// THE DEFECT THIS SHIPS AGAINST (docs/seed-tail-restactions-budget-and-16mb-
// serve-trace-2026-07-04.md §F.0). Under shape A (#87) /readyz is seed-gated
// BY WIRING: engineSeed's select (phase1_walk.go) waits on <-bootDone (the
// WHOLE boot scope completing) OR <-pctx.Done() (the PHASE1_TIMEOUT/
// pipGlobalTimeout backstop). On any topology where the full seed exceeds
// PHASE1_TIMEOUT the gate degenerates to a TIME gate and the pod goes Ready
// with COLD cells (the fix3r reality — seed 388s+ >> the 180s chart budget).
//
// FIX-F re-spec §F.1: /readyz should flip when {WaitAllInformersSynced
// good-enough barrier (unchanged) ∧ the FIX-E rank-1 × FIRST-NAV SEGMENT is
// seeded} — the walk-derived default-route/dashboard (RootIndex==0) widgets
// under the rank-1 identity — NOT the full seed (tail excluded by
// construction), NOT nothing (cells, not just substrate).
//
// MECHANISM (§F.1): seedScopeYielding closes firstNavDone the moment the
// rank-1 RootIndex==0 widget segment completes (or provably has zero
// targets). engineSeed's select waits on <-firstNavDone instead of
// <-bootDone; bootDone / scopeDone stay untouched (the boot scope keeps
// running to completion in background — S2 worker-release + engine semantics
// unchanged). The deferred MarkPhase1Done (phase1_walk.go) then fires at
// min(first-nav-complete, PHASE1_TIMEOUT backstop, pipGlobalTimeout child) —
// the C2 "Ready-degraded, never not-Ready-forever" backstop is preserved
// verbatim (the pctx.Done() arm of engineSeed's select is untouched).
//
// SEGMENT-SCOPED, NOT RANK-SCOPED (§F-C2). The segment is the RootIndex==0
// widget set (stamped by the harvester's BeginRoot/RootIndex, phase1_pip_seed.go),
// NOT the whole rank-1 pass. The heavy NON-first-nav tail (RootIndex>0 widgets
// + the rank-1 fanout-tail restactions, ascending-len-sorted LAST by FIX-E) is
// still mid-seed when the latch fires — §F-C2/ARM-TAIL. And a RootIndex>0-only
// rank-1 (no dashboard widget seeded) must NOT fire the latch early via the
// segment path (§F-C3): the completion signal keys on "the RootIndex==0 target
// COUNT was reached", so an empty first-nav set fires ONLY through the
// zero-targets guard below (provably-nothing-to-warm), never spuriously.
//
// NO STATIC LIST / NO MAGIC NUMBER: the segment is 100% walk-derived
// (RootIndex, config.json root order). The latch carries no cap and no env
// knob — the PHASE1_TIMEOUT / pipGlobalTimeout backstops it composes with are
// the EXISTING budget config (§F.4: PREWARM_BOOT_BUDGET_SECONDS bounds the
// ENGINE boot scope only, never the readyz gate).

package dispatchers

import (
	"log/slog"
	"sync"
	"time"
)

// firstNavLatch is the fire-once readiness signal for the rank-1 first-nav
// (RootIndex==0) segment. Closed at most once by seedScopeYielding on the
// boot pass; awaited by engineSeed's select. A process has exactly one
// (firstNavLatchSingleton); post-boot GVR-discovered re-walks reuse the same
// already-fired latch (close is idempotent via sync.Once), so a re-walk never
// re-arms or re-blocks readiness.
type firstNavLatch struct {
	done chan struct{}
	once sync.Once
}

func newFirstNavLatch() *firstNavLatch {
	return &firstNavLatch{done: make(chan struct{})}
}

// fire closes the latch exactly once and logs the transition with the
// segment counts + elapsed the caller measured. reason distinguishes the
// segment-complete path from the provably-zero-targets path (both are
// legitimate "first-nav is warm / there is no first-nav to warm" states);
// segWidgets/segTargets are the RootIndex==0 widget + per-target counts the
// latch waited on (0/0 on the zero-targets path).
func (l *firstNavLatch) fire(reason string, segWidgets, segTargets int, elapsed time.Duration) {
	l.once.Do(func() {
		if firstNavFireObserver != nil {
			// TEST-ONLY synchronous observation of the fire INSTANT (nil in
			// production). The falsifier needs the fire's position in the
			// ordered seed-event stream deterministically — an async wait()
			// watcher races the loop's next seed, so the observer records
			// synchronously at the true fire site.
			firstNavFireObserver(reason)
		}
		close(l.done)
		slog.Info("prewarm.first_nav.latch",
			slog.String("subsystem", "cache"),
			slog.String("reason", reason),
			slog.Int("first_nav_widgets", segWidgets),
			slog.Int("first_nav_targets", segTargets),
			slog.Int64("elapsed_ms", elapsed.Milliseconds()),
			slog.String("effect", "rank-1 RootIndex==0 first-nav segment seeded (or provably empty); "+
				"engineSeed unblocks → /readyz flips Ready with the dashboard warm. The boot scope "+
				"keeps seeding the tail in background (bootDone unaffected)."),
		)
	})
}

// wait returns the channel engineSeed's select reads. Closed when the latch
// has fired.
func (l *firstNavLatch) wait() <-chan struct{} {
	return l.done
}

// firstNavLatchSingleton is the process latch. Built once (set-once at the
// first engineSeed invocation via ensureFirstNavLatch); the boot pass fires
// it, and engineSeed waits on it. Package-level so seedScopeYielding (invoked
// deep inside the engine worker via rePrewarmBoot) can reach the SAME latch
// engineSeed awaits without threading it through rePrewarmDeps (which is
// shared with the post-boot GVR-discovered scope, where there is no readyz
// wait).
var (
	firstNavLatchSingleton *firstNavLatch
	firstNavLatchOnce       sync.Once
)

// ensureFirstNavLatch returns the process first-nav latch, building it once.
// engineSeed calls this before enqueuing the boot scope so the latch exists
// before seedScopeYielding can reach it.
func ensureFirstNavLatch() *firstNavLatch {
	firstNavLatchOnce.Do(func() {
		firstNavLatchSingleton = newFirstNavLatch()
	})
	return firstNavLatchSingleton
}

// currentFirstNavLatch returns the process latch if it has been built, else
// nil. seedScopeYielding uses this: the latch exists on the boot path (built
// by engineSeed before the boot enqueue), and is nil under the pure-unit seed
// tests that call seedScopeYielding directly without an engineSeed wrapper —
// in which case the fire is a no-op (nil-safe), so those tests are unchanged.
func currentFirstNavLatch() *firstNavLatch {
	return firstNavLatchSingleton
}

// firstNavFireObserver is a TEST-ONLY synchronous hook invoked at the fire
// site (inside the once.Do, before close(done)) so a falsifier can record the
// latch's exact position in the ordered seed-event stream without racing an
// async wait() watcher. nil in production. Set/cleared under engineLatchTestMu
// by the falsifier.
var firstNavFireObserver func(reason string)

// resetFirstNavLatchForTest rebuilds the singleton so a test can observe a
// fresh arm→fire transition. TEST-ONLY — production's lifecycle is set-once
// at boot. Pairs with cache.ResetPhase1DoneForTest.
func resetFirstNavLatchForTest() {
	firstNavLatchOnce = sync.Once{}
	firstNavLatchSingleton = nil
}
