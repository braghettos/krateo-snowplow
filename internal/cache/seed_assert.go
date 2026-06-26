// seed_assert.go — #46 Piece A: the prewarm seed per-unit aggregate-footprint
// assertion (solidity hardening, the #23 boot-OOM class).
//
// THE INVARIANT: no single prewarm seed unit (one (target, layer) resolve —
// e.g. a seedRAFullListForWidget UNPAGINATED full-list materialization) may
// allocate more than the configured seed-footprint budget. The #23 OOM was
// the legacy errgroup running GOMAXPROCS such units CONCURRENTLY (ΣN ≈ 8 GiB);
// the engine path is serial today, so its present-day risk is a SINGLE
// oversized unit — exactly what this assertion catches. Piece B's semaphore
// is the aggregate bound for the (reachable, engine=false back-out) concurrent
// path; this assertion is the per-unit guard that fires on BOTH postures.
//
// APPARATUS: mirrors serve_assert.go's TestMode/prod asymmetry + the
// snowplow_assertion_violations_total expvar map (fallthrough_meter_expvar.go),
// check="seed_aggregate_footprint":
//
//   - env.TestMode()==true  → panic. A test whose seed unit blows the budget
//     fails loud (a real regression — an unbounded/unpaginated materialization
//     slipped in).
//   - env.TestMode()==false → ERROR log + seedUnitFootprintViolations.Add(1).
//     The seed unit still completed (we sample AFTER it resolved); the
//     violation is alertable. We do NOT abort the seed — best-effort warmth,
//     never a boot-fail (feedback_no_park_broken_behind_flag: this is an
//     OBSERVABILITY assert, the BOUND is Piece B's semaphore which blocks).
//
// Cost-proportional + seed-scoped: sampled once per seed unit, AFTER the unit
// resolved, on the seed goroutine only — never on the customer /call path
// (feedback_bounding_mechanism_discipline). The HeapInuse delta is captured at
// the call site (prewarm_engine_boot.go seedOneTarget); this helper only
// evaluates the breach so the threshold logic + counter live next to the
// sibling serve-assert.
package cache

import (
	"log/slog"
	"sync/atomic"

	"github.com/krateoplatformops/plumbing/env"
)

// seedUnitFootprintViolations is the production-mode counter bumped when a
// single prewarm seed unit's measured HeapInuse delta exceeded the seed
// footprint budget. Exposed via snowplow_assertion_violations_total under
// check="seed_aggregate_footprint".
var seedUnitFootprintViolations atomic.Uint64

// SeedUnitFootprintViolations returns the cumulative count of seed-unit
// footprint-budget violations observed in production mode. Exported for the
// falsifier gate.
func SeedUnitFootprintViolations() uint64 {
	return seedUnitFootprintViolations.Load()
}

// ResetSeedUnitFootprintViolationsForTest zeroes the counter (test-only).
func ResetSeedUnitFootprintViolationsForTest() {
	seedUnitFootprintViolations.Store(0)
}

// AssertSeedUnitFootprint evaluates the per-unit seed-footprint invariant for
// one resolved seed unit. deltaBytes is the measured HeapInuse delta the
// caller sampled around the unit's resolve; budgetBytes is the configured
// SEED_FOOTPRINT_BUDGET_BYTES. label is a short seed-unit descriptor for the
// log (kind + target — never a per-name special-case).
//
// budgetBytes <= 0 disables the assertion (returns true, no-op) — the
// transparent-fallback posture when the budget is unset.
//
// Returns true when within budget (the common path). On breach:
//   - test mode → panic (loud; an oversized/unpaginated unit is a regression).
//   - prod mode → count + ERROR log + return false (the unit already resolved;
//     this is observability, the seed proceeds — Piece B's semaphore is the
//     mechanism that actually blocks/serializes).
func AssertSeedUnitFootprint(label string, deltaBytes, budgetBytes uint64) bool {
	if budgetBytes == 0 || deltaBytes <= budgetBytes {
		return true
	}

	if env.TestMode() {
		panic("cache.AssertSeedUnitFootprint: a single prewarm seed unit exceeded the seed footprint budget" +
			" (label=" + label + ") — a seed unit MUST stay within SEED_FOOTPRINT_BUDGET_BYTES;" +
			" an unbounded/unpaginated materialization (e.g. seedRAFullListForWidget full-list)" +
			" is the #23 boot-OOM class")
	}

	seedUnitFootprintViolations.Add(1)
	slog.Error("cache.seed_aggregate_footprint.violation",
		slog.String("subsystem", "cache"),
		slog.String("check", "seed_aggregate_footprint"),
		slog.String("seed_unit", label),
		slog.Uint64("delta_bytes", deltaBytes),
		slog.Uint64("budget_bytes", budgetBytes),
		slog.String("hint", "a single prewarm seed unit's HeapInuse delta exceeded SEED_FOOTPRINT_BUDGET_BYTES "+
			"— an oversized/unpaginated materialization; the seed proceeds (best-effort warmth) but this is "+
			"the #23 boot-OOM-class signal, alertable. Piece B's semaphore is the blocking bound."),
	)
	return false
}
