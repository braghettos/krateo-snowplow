// nested_resolve_bound.go — (c) cold-fan-out aggregate-footprint bound on the
// CUSTOMER nested-resolve path. Sibling of the SHIPPED #46 seed_bound.go, same
// semaphore.Weighted + empirical-calibration mechanism, applied at the
// maybeResolveInProcess choke point instead of the seed primitives.
//
// THE OOM CLASS (#23 cold-fan-out, RE-EXPOSED by #77): a single cold /call of a
// heavy composition RA (fsa-y2-composition-resources) recursively resolves ~20
// nested RA/widget children in-process (resolve:true). Each child envelope is
// decoded-into-map (A1 #30 double-encode + #72 decode-into-map → encoding/json
// objectInterface ~67% of inuse_space). One outer /call drove a monotonic
// 716MB→1172MB climb (heap pprof, traceId C3CtMoBvR follow-up). It is a
// SINGLE-RESOLVE MULTIPLIER (one /call × N nested trees), NOT a concurrent
// burst — so the (a) process-wide customer-entry cap (#25, reverted) is the
// wrong lever; this bounds the AGGREGATE in-flight footprint of the nested
// subtree(s) regardless of how many top-level /calls are concurrent.
//
// PLACEMENT — OUTERMOST nested-resolve ONLY (depth-0):
//   - Acquire fires in maybeResolveInProcess AFTER Gate 4 (a confirmed
//     single-CR RA/widget GET that WILL nest-resolve), and ONLY when
//     NestedCallDepthFromContext(gctx)==0 (the top of a nested subtree). The
//     inner recursive resolves (depth>0) inherit the ONE permit the outermost
//     entry holds → ONE permit per whole subtree, no per-node explosion, and no
//     self-deadlock (a subtree never waits on its own ancestor's permit).
//   - This sits AFTER the apistage content-serve / cache-hit gates in the
//     caller (a warm hit never reaches Gate 4's nestedCallResolver), so it is
//     cost-proportional: only a genuine COLD nested resolve pays
//     (feedback_bounding_mechanism_discipline — after the cache-hit, bound the
//     event that still happens, cost-proportional not flat-worst-case).
//
// WEIGHT (C46-1 self-calibrating, NOT a design-time constant — the 1.5.1 180×
// lesson): the per-subtree Acquire weight is calibrated from the FIRST
// subtree's MEASURED HeapInuse delta (one-shot), reused after. Before
// calibration (and as the conservative fallback) it is
// NESTED_RESOLVE_EST_UNIT_BYTES_FALLBACK = 256 MiB (≥5× the observed ~47MB/tree),
// clamped to the budget so one oversized subtree runs ALONE rather than
// blocking forever (guaranteed-progress: the bounded event still happens).
//
// DEFAULT-OFF / TRANSPARENT: budget=0 (NESTED_RESOLVE_FOOTPRINT_BUDGET_BYTES
// unset) ⇒ Acquire skipped, release is a no-op — byte-identical to pre-(c)
// behaviour (project_caching_is_provisional: cleanly removable). Excess
// BLOCKS, never drops → every nested resolve still completes + is
// content-correct, just serialized under memory pressure.
package api

import (
	"context"
	"runtime"
	"sync"

	"github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"golang.org/x/sync/semaphore"
)

const (
	// envNestedResolveFootprintBudgetBytes caps the aggregate in-flight COLD
	// nested-resolve HeapInuse. 0 (default) DISABLES the bound — the
	// transparent posture. Operators set it from the empirical per-subtree cost
	// × safety (feedback_capacity_caps_empirical_per_entry_cost; the pprof
	// measured ~47MB/tree, the 1172MB peak motivates a GOMEMLIMIT-fraction
	// budget).
	envNestedResolveFootprintBudgetBytes = "NESTED_RESOLVE_FOOTPRINT_BUDGET_BYTES"

	// envNestedResolveEstUnitBytesFallback is the CONSERVATIVE-HIGH per-subtree
	// weight used before the one-shot empirical calibration lands. Over-reserve
	// by design (a too-high fallback serializes a bit more — safe; a too-low one
	// admits too much aggregate — the 1.5.1 under-estimate failure mode). 256
	// MiB ≥5× the observed ~47MB/tree; clamped to the budget at use.
	envNestedResolveEstUnitBytesFallback     = "NESTED_RESOLVE_EST_UNIT_BYTES_FALLBACK"
	defaultNestedResolveEstUnitBytesFallback = 256 * 1024 * 1024 // 256 MiB, conservative-high
)

var (
	// nestedBoundOnce-style lazy build (plain mutex double-check, NOT sync.Once,
	// so resetNestedResolveBoundForTest can rebuild it under t.Setenv).
	nestedBoundMu     sync.Mutex
	nestedBoundSem    *semaphore.Weighted
	nestedBoundBudget int64
	nestedBoundBuilt  bool

	// nestedEstUnitBytes is the calibrated per-subtree Acquire weight. Starts at
	// the conservative-high fallback; replaced ONCE by the first subtree's
	// measured HeapInuse delta (C46-1). Guarded by nestedEstUnitMu.
	nestedEstUnitMu         sync.Mutex
	nestedEstUnitBytes      int64
	nestedEstUnitCalibrated bool
)

// nestedBudgetBytes reads NESTED_RESOLVE_FOOTPRINT_BUDGET_BYTES (0 = disabled).
func nestedBudgetBytes() int64 {
	return int64(env.Int(envNestedResolveFootprintBudgetBytes, 0))
}

// nestedEstUnitFallback reads the conservative-high fallback per-subtree weight.
func nestedEstUnitFallback() int64 {
	return int64(env.Int(envNestedResolveEstUnitBytesFallback, defaultNestedResolveEstUnitBytesFallback))
}

// nestedResolveBound lazily builds (once) the process-wide nested-resolve
// semaphore from the budget, returning (sem, budget). Returns (nil, 0) when the
// budget is 0 (disabled → enterNestedResolveUnit is a transparent pass-through).
func nestedResolveBound() (*semaphore.Weighted, int64) {
	nestedBoundMu.Lock()
	defer nestedBoundMu.Unlock()
	if !nestedBoundBuilt {
		nestedBoundBudget = nestedBudgetBytes()
		if nestedBoundBudget > 0 {
			nestedBoundSem = semaphore.NewWeighted(nestedBoundBudget)
		}
		nestedBoundBuilt = true
	}
	return nestedBoundSem, nestedBoundBudget
}

// currentNestedEstUnit returns the per-subtree Acquire weight, clamped to the
// budget (a single Acquire can never exceed the semaphore capacity, else it
// would block forever). Uses the calibrated value once available, else the
// conservative-high fallback.
func currentNestedEstUnit(budget int64) int64 {
	nestedEstUnitMu.Lock()
	est := nestedEstUnitBytes
	calibrated := nestedEstUnitCalibrated
	nestedEstUnitMu.Unlock()
	if !calibrated || est <= 0 {
		est = nestedEstUnitFallback()
	}
	if budget > 0 && est > budget {
		est = budget // clamp: one subtree may use up to the whole budget, never more
	}
	if est < 1 {
		est = 1
	}
	return est
}

// calibrateNestedEstUnit records the first subtree's MEASURED HeapInuse delta as
// the empirical per-subtree weight (C46-1, one-shot). A delta of 0 (GC ran
// mid-subtree, or a trivial subtree) is ignored so we don't calibrate to an
// unrealistically small weight.
func calibrateNestedEstUnit(measuredDelta int64) {
	if measuredDelta <= 0 {
		return
	}
	nestedEstUnitMu.Lock()
	if !nestedEstUnitCalibrated {
		nestedEstUnitBytes = measuredDelta
		nestedEstUnitCalibrated = true
	}
	nestedEstUnitMu.Unlock()
}

// nestedHeapInuse samples the current HeapInuse. ReadMemStats stops the world
// briefly; called at most twice per OUTERMOST nested resolve (depth-0 only) —
// never on a warm hit, never on an inner recursive resolve.
func nestedHeapInuse() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse
}

// enterNestedResolveUnit is the cold-fan-out footprint bound, applied as a
// lifecycle bracket at the OUTERMOST nested resolve. The caller MUST gate it on
// the depth-0 condition (it is acquired ONLY when
// cache.NestedCallDepthFromContext(gctx)==0) so the whole nested subtree holds
// exactly ONE permit — inner recursive resolves (depth>0) do NOT re-enter
// (no per-node explosion, no self-deadlock).
//
// When the budget is 0 it is a transparent pass-through: Acquire is skipped and
// release() is a no-op. Otherwise: Acquire(weight) [blocks if the aggregate
// in-flight weight would exceed the budget — completes, never drops] + sample
// HeapInuse on entry; release() samples HeapInuse again, calibrates the
// per-subtree weight (one-shot), and Releases.
//
// The Acquire weight is clamped to the budget (currentNestedEstUnit) so a single
// subtree larger than the whole budget runs ALONE rather than blocking forever
// — the guaranteed-progress / "bounded event still happens" property.
func enterNestedResolveUnit(ctx context.Context) (release func(), err error) {
	sem, budget := nestedResolveBound()
	if sem == nil || budget == 0 {
		return func() {}, nil // disabled — transparent pass-through.
	}

	est := currentNestedEstUnit(budget)
	if aerr := sem.Acquire(ctx, est); aerr != nil {
		// ctx cancelled/expired while blocked on the bound — propagate so the
		// outer /call surfaces the cancellation rather than resolving unbounded.
		return nil, aerr
	}

	before := nestedHeapInuse()
	return func() {
		after := nestedHeapInuse()
		// HeapInuse can shrink across a GC mid-subtree; a negative delta → 0.
		var delta int64
		if after > before {
			delta = int64(after - before)
		}
		calibrateNestedEstUnit(delta)
		sem.Release(est)
	}, nil
}

// isOutermostNestedResolve reports whether gctx is at the TOP of a nested
// subtree (depth 0) — the single point where enterNestedResolveUnit acquires.
// Extracted for the falsifier (CN-3 depth-0 verification).
func isOutermostNestedResolve(ctx context.Context) bool {
	return cache.NestedCallDepthFromContext(ctx) == 0
}

// resetNestedResolveBoundForTest rebuilds the lazy semaphore + clears the
// calibration so a falsifier can drive a fresh budget via t.Setenv. Test-only.
func resetNestedResolveBoundForTest() {
	nestedBoundMu.Lock()
	nestedBoundBuilt = false
	nestedBoundSem = nil
	nestedBoundBudget = 0
	nestedBoundMu.Unlock()
	nestedEstUnitMu.Lock()
	nestedEstUnitBytes = 0
	nestedEstUnitCalibrated = false
	nestedEstUnitMu.Unlock()
}

// calibratedNestedEstUnitForTest returns (estUnitBytes, calibrated) for the
// falsifier. Test-only.
func calibratedNestedEstUnitForTest() (int64, bool) {
	nestedEstUnitMu.Lock()
	defer nestedEstUnitMu.Unlock()
	return nestedEstUnitBytes, nestedEstUnitCalibrated
}
