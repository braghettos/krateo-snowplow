// seed_bound.go — #46 Piece B: the prewarm-seed aggregate-footprint bound +
// the Piece-A per-unit footprint sample, applied at the seed-unit choke point.
//
// THE #23 OOM CLASS: a prewarm seed unit (one (target, layer) resolve) can
// materialize a multi-hundred-MB UNPAGINATED full-list envelope
// (seedRAFullListForWidget). Running many such units CONCURRENTLY (the legacy
// runPIPSeed errgroup at GOMAXPROCS) summed to the ~8 GiB #23 boot OOM. The
// engine path (seedScopeYielding) is SERIAL today, so its present risk is a
// single oversized unit; the legacy errgroup path — reachable via the
// PREWARM_ENGINE_ENABLED=false back-out lever — is the live concurrent
// aggregate. Both are wrapped here (C46-2: bound the path that runs AND the
// reachable back-out path).
//
// TWO mechanisms, ONE wrapper (boundSeedUnit):
//   - Piece B (the BOUND): a process-wide semaphore.Weighted sized by
//     SEED_FOOTPRINT_BUDGET_BYTES. Each unit Acquires its estimated weight
//     before resolving and Releases after. Excess BLOCKS (never drops → the
//     seed still completes, just serialized under memory pressure). On the
//     serial engine path this is a no-op admission gate (one unit at a time);
//     on the concurrent errgroup path it caps the aggregate in-flight
//     footprint. Disabled (no-op) when the budget is 0.
//   - Piece A (the ASSERT): sample HeapInuse delta around the resolve and call
//     cache.AssertSeedUnitFootprint — panics in test / logs+counts in prod
//     when a SINGLE unit exceeds the budget (the oversized-unit signal that
//     fires on both postures).
//
// C46-1 (EMPIRICAL estUnitBytes, NOT a design-time constant — the 1.5.1 180×
// lesson): the per-unit Acquire weight is calibrated from the FIRST unit's
// MEASURED HeapInuse delta (one-shot), then reused. Before calibration (and as
// the conservative fallback) the weight is SEED_EST_UNIT_BYTES_FALLBACK, which
// MUST be conservative-HIGH (over-reserve, never under) so we never admit more
// aggregate than the budget on the strength of a too-small guess.
//
// SEED-SCOPED (customer /call UNTOUCHED): boundSeedUnit is called ONLY from the
// seed choke points (seedOneTarget engine + the legacy errgroup closure), both
// AFTER the per-binding identity short-circuit. The customer dispatcher never
// routes here (feedback_bounding_mechanism_discipline: after the cache-hit,
// seed-scoped, cost-proportional).
package dispatchers

import (
	"context"
	"runtime"
	"sync"

	"github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"golang.org/x/sync/semaphore"
)

const (
	// envSeedFootprintBudgetBytes caps the aggregate in-flight prewarm-seed
	// HeapInuse. 0 (default) DISABLES the bound + the per-unit assert — the
	// transparent posture (project_caching_is_provisional: cleanly
	// removable). Operators set it from the empirical per-entry cost × safety
	// (feedback_capacity_caps_empirical_per_entry_cost).
	envSeedFootprintBudgetBytes = "SEED_FOOTPRINT_BUDGET_BYTES"

	// envSeedEstUnitBytesFallback is the CONSERVATIVE-HIGH per-unit weight
	// used before the one-shot empirical calibration lands (and when
	// calibration is somehow unavailable). Over-reserve by design: a too-high
	// fallback serializes a bit more aggressively (safe); a too-low one would
	// admit too much aggregate (the 1.5.1 180× under-estimate failure mode).
	// 256 MiB ≈ a large unpaginated full-list unit; never larger than the
	// whole budget would allow (clamped at use).
	envSeedEstUnitBytesFallback        = "SEED_EST_UNIT_BYTES_FALLBACK"
	defaultSeedEstUnitBytesFallback    = 256 * 1024 * 1024 // 256 MiB, conservative-high
)

var (
	// seedBoundOnce lazily builds the semaphore from the budget on first use
	// (env is populated before any seed runs). A plain mutex-guarded
	// double-check (NOT sync.Once) so resetSeedBoundForTest can rebuild it.
	seedBoundMu      sync.Mutex
	seedBoundSem     *semaphore.Weighted
	seedBoundBudget  int64
	seedBoundBuilt   bool

	// estUnitBytes is the calibrated per-unit Acquire weight. Starts at the
	// conservative-high fallback; replaced ONCE by the first unit's measured
	// HeapInuse delta (C46-1). Guarded by estUnitMu.
	estUnitMu       sync.Mutex
	estUnitBytes    int64
	estUnitCalibrated bool
)

// seedBudgetBytes reads SEED_FOOTPRINT_BUDGET_BYTES (0 = disabled).
func seedBudgetBytes() int64 {
	return int64(env.Int(envSeedFootprintBudgetBytes, 0))
}

// seedEstUnitFallback reads the conservative-high fallback per-unit weight.
func seedEstUnitFallback() int64 {
	return int64(env.Int(envSeedEstUnitBytesFallback, defaultSeedEstUnitBytesFallback))
}

// seedBound lazily builds (once) the process-wide seed semaphore from the
// budget, returning (sem, budget). Returns (nil, 0) when the budget is 0
// (disabled → boundSeedUnit is a transparent pass-through).
func seedBound() (*semaphore.Weighted, int64) {
	seedBoundMu.Lock()
	defer seedBoundMu.Unlock()
	if !seedBoundBuilt {
		seedBoundBudget = seedBudgetBytes()
		if seedBoundBudget > 0 {
			seedBoundSem = semaphore.NewWeighted(seedBoundBudget)
		}
		seedBoundBuilt = true
	}
	return seedBoundSem, seedBoundBudget
}

// currentEstUnit returns the current per-unit Acquire weight, clamped to the
// budget (a single Acquire can never exceed the semaphore capacity, else it
// would deadlock forever). Uses the calibrated value once available, else the
// conservative-high fallback.
func currentEstUnit(budget int64) int64 {
	estUnitMu.Lock()
	est := estUnitBytes
	calibrated := estUnitCalibrated
	estUnitMu.Unlock()
	if !calibrated || est <= 0 {
		est = seedEstUnitFallback()
	}
	if budget > 0 && est > budget {
		est = budget // clamp: one unit may use up to the whole budget, never more
	}
	if est < 1 {
		est = 1
	}
	return est
}

// calibrateEstUnit records the first unit's MEASURED HeapInuse delta as the
// empirical per-unit weight (C46-1, one-shot). Subsequent units reuse it.
// A measured delta of 0 (GC ran mid-unit, or a static no-op unit) is ignored
// so we don't calibrate to an unrealistically small weight.
func calibrateEstUnit(measuredDelta int64) {
	if measuredDelta <= 0 {
		return
	}
	estUnitMu.Lock()
	if !estUnitCalibrated {
		estUnitBytes = measuredDelta
		estUnitCalibrated = true
	}
	estUnitMu.Unlock()
}

// heapInuse samples the current HeapInuse. ReadMemStats stops the world
// briefly; called at most twice per seed unit on the seed goroutine — never
// on the customer path.
func heapInuse() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse
}

// calibratedEstUnitForTest returns (estUnitBytes, calibrated) so the
// granularity-discriminating falsifier can assert calibration landed in a
// per-UNIT band (not N× a whole cohort). Test-only.
func calibratedEstUnitForTest() (int64, bool) {
	estUnitMu.Lock()
	defer estUnitMu.Unlock()
	return estUnitBytes, estUnitCalibrated
}

// enterSeedUnit is the seed-unit footprint bound, applied as a lifecycle
// bracket at the SHARED seed primitives (seedOneWidget / seedOneRestaction),
// AFTER their identity short-circuit. Both the engine (serial) and the legacy
// errgroup (concurrent) paths funnel through those primitives, so bracketing
// there bounds BOTH aggregates with one insertion (feedback_no_special_cases —
// bound the shared mechanism, not each caller).
//
// Usage at each primitive, right after `if handle==nil || key=="" { return }`:
//
//	release, err := enterSeedUnit(ctx, "widget/"+ns+"/"+name)
//	if err != nil { return err }   // ctx cancelled while blocked on the bound
//	defer release()
//
// label is a short seed-unit descriptor (kind + identity) for the assert log —
// never a per-name special-case (feedback_no_special_cases).
//
// When the budget is 0 it is a transparent pass-through: Acquire is skipped and
// release() is a no-op (no semaphore, no assert). Otherwise: Acquire(estUnit)
// [blocks if the aggregate in-flight weight would exceed the budget] + sample
// HeapInuse on entry; release() samples HeapInuse again, asserts the delta is
// within budget, calibrates estUnit (one-shot), and Releases the weight.
//
// The Acquire weight is clamped to the budget (currentEstUnit) so a single unit
// larger than the whole budget runs ALONE rather than blocking forever — the
// guaranteed-progress / "bounded event still happens" property. Nesting is
// deadlock-free: the legacy errgroup SetLimit(GOMAXPROCS) is the OUTER
// semaphore (acquired at g.Go spawn), this is INNER (acquired in the
// primitive) — strict outer→inner, no lock-order inversion.
func enterSeedUnit(ctx context.Context, label string) (release func(), err error) {
	sem, budget := seedBound()
	if sem == nil || budget == 0 {
		return func() {}, nil // disabled — transparent pass-through.
	}

	est := currentEstUnit(budget)
	if aerr := sem.Acquire(ctx, est); aerr != nil {
		// ctx cancelled/expired while blocked on the bound — propagate (the
		// seed unit is abandoned for this run; the seed yields).
		return nil, aerr
	}

	before := heapInuse()
	return func() {
		after := heapInuse()
		// HeapInuse can shrink across a GC mid-unit; treat a negative delta as
		// 0 (no measurable growth — a static/no-op unit).
		var delta int64
		if after > before {
			delta = int64(after - before)
		}
		calibrateEstUnit(delta)
		cache.AssertSeedUnitFootprint(label, uint64(delta), uint64(budget))
		sem.Release(est)
	}, nil
}

// boundSeedUnit is the closure form of enterSeedUnit, kept for the falsifier
// tests (the shared-primitive call sites use enterSeedUnit's bracket form
// directly because their resolve+Put bodies are long).
func boundSeedUnit(ctx context.Context, label string, do func() error) error {
	release, err := enterSeedUnit(ctx, label)
	if err != nil {
		return err
	}
	defer release()
	return do()
}

// resetSeedBoundForTest rebuilds the lazy semaphore + clears the calibration
// so a falsifier can drive a fresh budget via t.Setenv. Test-only.
func resetSeedBoundForTest() {
	seedBoundMu.Lock()
	seedBoundBuilt = false
	seedBoundSem = nil
	seedBoundBudget = 0
	seedBoundMu.Unlock()
	estUnitMu.Lock()
	estUnitBytes = 0
	estUnitCalibrated = false
	estUnitMu.Unlock()
}
