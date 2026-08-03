// otel_accessors.go — exported, read-only accessors over the dispatchers
// package's existing prewarm / phase-1 / dispatch-L1 atomic counters, so
// the internal/metrics OTLP mirror (a separate package) can observe the
// SAME live snapshots the expvar `.Func` closures read.
//
// ADDITIVE: these are pure read accessors. They do NOT change any
// incrementer, populate path, or the expvar surface. Each reads the same
// package-private atomics / sync.Maps the existing expvar publishers in
// this package read (prewarm_engine_metrics.go, phase1_walk_pagination_metrics.go,
// phase1_walk_metrics.go, phase1_pip_metrics.go, l1_lookup_metrics.go).
//
// The metrics package reads these at OTLP collection time only (the
// observable-instrument callback), so there is zero per-/call cost — same
// "computed-on-read" semantics as expvar.

package dispatchers

// PrewarmEngineSnapshot returns the prewarm-engine worker counters,
// mirroring snowplow_prewarm_engine_{enqueued,processed,yield}_total and
// snowplow_prewarm_engine_pending_depth. pendingDepth is the workqueue's own
// Len() (F.4 / R1 — internally synchronized, race-free against Add/Get).
// Safe before the engine has started: prewarmEngineSingleton() lazily
// constructs the engine (queue included), so a pre-start read returns zeros
// from the freshly-allocated queue.
func PrewarmEngineSnapshot() (enqueued, processed, yield uint64, pendingDepth int64) {
	e := prewarmEngineSingleton()
	if e == nil {
		return 0, 0, 0, 0
	}
	pendingDepth = int64(e.queue.Len())
	return e.enqueuedTotal.Load(), e.processedTotal.Load(), e.yieldTotal.Load(), pendingDepth
}

// Phase1PaginationSnapshot returns the apiRef pagination-coverage grand
// totals, mirroring snowplow_phase1_units_planned / _units_seeded /
// _apiref_pages_total / _eligible_no_continue_total.
func Phase1PaginationSnapshot() (unitsPlanned, unitsSeeded, apiRefPages, eligibleNoContinue uint64) {
	return prewarmUnitsPlanned.Load(),
		prewarmUnitsSeeded.Load(),
		prewarmApiRefPagesTotal.Load(),
		prewarmEligibleNoContinueTotal.Load()
}

// Phase1WalkSnapshot returns the boot-walk fan-out totals, mirroring
// snowplow_phase1_walk_zero_children_total / _walk_observations_total. The
// per-root walk_children map is high-cardinality + diagnostic-only and is
// deliberately NOT mirrored to OTLP.
func Phase1WalkSnapshot() (zeroChildren, observations uint64) {
	return phase1WalkZeroChildrenTotal.Load(), phase1WalkObservationsTotal.Load()
}

// Phase1SeedSnapshot returns the per-target phase-1 seed outcome totals,
// mirroring snowplow_phase1_bindingset_seed_resolves_total /
// _bindingset_seed_failures_total / _seed_rbac_deny_total /
// _seed_operational_fail_total. The per-(cohort,target) failure maps and
// the per-cohort status map are high-cardinality + diagnostic-only and are
// deliberately NOT mirrored to OTLP.
func Phase1SeedSnapshot() (resolves, failures, rbacDeny, operationalFail uint64) {
	return pipBindingSetSeedResolvesTotal.Load(),
		pipBindingSetSeedFailuresTotal.Load(),
		pipSeedRBACDenyTotal.Load(),
		pipSeedOperationalFailTotal.Load()
}

// DispatchL1LookupTotals returns the cluster-wide aggregate hit/miss totals
// across every (handlerKind, gvr) dispatch-L1 cell, mirroring the
// snowplow_dispatch_l1_lookups expvar map collapsed to two grand totals.
// The per-(handlerKind|gvr) breakdown is intentionally aggregated here to
// keep OTLP cardinality bounded — the expvar map remains the per-cell
// drill-down surface.
func DispatchL1LookupTotals() (hit, miss uint64) {
	l1LookupCells.Range(func(_, v any) bool {
		cell, _ := v.(*l1LookupCell)
		if cell == nil {
			return true
		}
		hit += cell.hit.Load()
		miss += cell.miss.Load()
		return true
	})
	return hit, miss
}
