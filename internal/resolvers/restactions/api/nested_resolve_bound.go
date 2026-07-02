// nested_resolve_bound.go — (c) cold-fan-out AGGREGATE-footprint bound on the
// nested-resolve path. Successor to the #78 per-outermost-tree byte-budget
// semaphore (magic env NESTED_RESOLVE_FOOTPRINT_BUDGET_BYTES), which was
// per-tree and therefore did NOT bound the compositions-page fan-out: the page
// fires MANY concurrent outermost trees, each admitted under the per-tree
// budget, whose AGGREGATE simultaneously-live footprint blew the 8Gi cgroup
// ceiling → OOMKill(137) on 1.5.27 (regression-journal 2026-07-02). This
// replaces the fixed byte budget with a PROCESS-WIDE, self-adapting admission
// gate keyed off the runtime's ACTUAL headroom (GOMEMLIMIT − live heap),
// sampled synchronously at each admission (docs/oom-aggregate-adaptive-bound-2026-07-02.md).
//
// MECHANISM (Q3 shape A — recompute-per-admission, NOT a fixed-capacity
// semaphore):
//   - At each OUTERMOST nested resolve (depth-0), sample headroom =
//     GOMEMLIMIT(debug.SetMemoryLimit(-1)) − liveHeap(/memory/classes/heap/objects:bytes
//     via runtime/metrics — a cheap sample, no ReadMemStats stop-the-world).
//   - Admit the (N+1)th tree IFF inFlightWeight + estUnit <= headroom − reserve.
//   - inFlight == 0 ⇒ ADMIT UNCONDITIONALLY (anti-deadlock: a tree already in
//     flight holds the live footprint and WILL release; blocking the only tree
//     forever is a deadlock — mirrors the #78 clamp). Guaranteed progress.
//   - Otherwise SERIALIZE: wait (ctx-bounded) until an in-flight tree releases
//     and re-check. On ctx cancel/deadline the caller surfaces an HONEST error
//     (the outer /call returns a 503-class error, NOT empty/raw content) — the
//     Q4 SERIALIZE→honest-503 posture. The bound BLOCKS, never DROPS: every
//     admitted tree resolves fully + content-correct, just later.
//
// PLACEMENT — OUTERMOST nested-resolve ONLY (depth-0), AFTER the apistage
// cache-hit serve. A warm apistage/L1 hit is served before nestedCallResolver
// is ever reached, so it NEVER enters the gate → cost-proportional: only a
// genuine COLD nested resolve pays (feedback_bounding_mechanism_discipline).
// Inner recursive resolves (depth>0) inherit the ONE admission the outermost
// entry holds — no per-node accounting, no self-deadlock. Same placement
// invariant #78 satisfied; inherited unchanged.
//
// WEIGHT (estUnit, C46-1 self-calibrating — NOT a design constant, the 1.5.1
// 180× lesson): the per-tree admission weight is calibrated from the FIRST
// tree's MEASURED live-heap delta (one-shot), reused after. Before calibration
// (and as the conservative fallback) it is defaultNestedEstUnitBytes = 256 MiB
// (≥5× the observed ~47MB/tree). This is an empirical-per-entry-cost weight
// (feedback_capacity_caps_empirical_per_entry_cost), not an operator dial.
//
// ZERO-KNOB (Diego 2026-07-02): no NESTED_RESOLVE_* env remains. reserve is a
// documented code-constant FRACTION of GOMEMLIMIT (a dimensionless
// headroom-share, not a byte budget); the est-unit fallback is a documented
// code constant. GOMEMLIMIT is read via debug.SetMemoryLimit(-1) (respects the
// env/cgroup-derived value) — no env re-parse.
//
// TRANSPARENT when GOMEMLIMIT is effectively unlimited (unset → the runtime
// default math.MaxInt64): headroom is unbounded, admission is always granted,
// release is a no-op — byte-identical to no bound (project_caching_is_provisional:
// cleanly removable). A pod SHOULD set GOMEMLIMIT (the chart does, 7GiB); with
// it unset there is no soft ceiling to protect and the gate correctly no-ops.
package api

import (
	"context"
	"math"
	"runtime/debug"
	"runtime/metrics"
	"sync"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

const (
	// defaultNestedEstUnitBytes is the CONSERVATIVE-HIGH per-tree admission
	// weight used before the one-shot empirical calibration lands (C46-1).
	// Over-reserve by design: a too-high fallback serializes a touch more
	// (safe); a too-low one admits too much aggregate (the 1.5.1 under-estimate
	// failure mode). 256 MiB ≥5× the observed ~47MB/tree; documented code
	// constant, NOT an env (zero-knob, Diego 2026-07-02).
	defaultNestedEstUnitBytes int64 = 256 * 1024 * 1024

	// reserveFractionNum/reserveFractionDen express `reserve` as a FRACTION of
	// GOMEMLIMIT held back for NON-resolve work (informer caches, HTTP buffers,
	// the L1/apistage stores, GC working set). 1/8 = 12.5% headroom-share. This
	// is a dimensionless share of the pod's own limit — it scales with pod size
	// and is defensible as "keep headroom for the rest of the process", unlike
	// an absolute byte budget. Documented code constant (Option 1a, zero-knob).
	reserveFractionNum int64 = 1
	reserveFractionDen int64 = 8
)

// runtimeLimitFn / liveHeapFn are the runtime-headroom TEST SEAMS. Production
// wires them to debug.SetMemoryLimit(-1) and the runtime/metrics live-heap
// sample; falsifiers inject deterministic GOMEMLIMIT / liveHeap WITHOUT a
// resurrected env (C4). Guarded by boundMu when swapped in tests.
var (
	runtimeLimitFn = prodRuntimeMemoryLimit
	liveHeapFn     = prodLiveHeapBytes
)

// prodRuntimeMemoryLimit reads the effective GOMEMLIMIT (soft limit) via
// debug.SetMemoryLimit(-1), which returns the current limit without changing
// it (respects the env/cgroup-derived value). The runtime default when unset is
// math.MaxInt64 (effectively unlimited).
func prodRuntimeMemoryLimit() int64 {
	return debug.SetMemoryLimit(-1)
}

// prodLiveHeapBytes samples live heap objects via runtime/metrics
// (/memory/classes/heap/objects:bytes) — a cheap read, no stop-the-world
// (unlike runtime.ReadMemStats). This is the live (in-use) heap, the quantity
// GC cannot shrink while trees are in flight — the OOM-relevant denominator.
func prodLiveHeapBytes() int64 {
	const key = "/memory/classes/heap/objects:bytes"
	sample := []metrics.Sample{{Name: key}}
	metrics.Read(sample)
	if sample[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	v := sample[0].Value.Uint64()
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

// aggregateBound is the process-wide admission gate. inFlightWeight is the sum
// of the estUnit weights of currently-admitted outermost trees; cond wakes
// waiters on each release so they re-check headroom. waitingCustomers counts
// CUSTOMER trees currently blocked on the gate — a BACKGROUND tree will NOT
// admit ahead of a waiting customer (C5 no-new-background-ahead-of-a-waiting-
// customer rule → customer /call has absolute admission priority).
type aggregateBound struct {
	mu               sync.Mutex
	cond             *sync.Cond
	inFlightWeight   int64
	inFlightCount    int
	waitingCustomers int
}

var (
	boundMu    sync.Mutex
	boundOnce  *aggregateBound

	// estUnit calibration (C46-1). Starts at the conservative-high fallback;
	// replaced ONCE by the first tree's measured live-heap delta.
	estUnitMu         sync.Mutex
	estUnitBytes      int64
	estUnitCalibrated bool
)

func theBound() *aggregateBound {
	boundMu.Lock()
	defer boundMu.Unlock()
	if boundOnce == nil {
		boundOnce = &aggregateBound{}
		boundOnce.cond = sync.NewCond(&boundOnce.mu)
	}
	return boundOnce
}

// currentNestedEstUnit returns the per-tree admission weight: the calibrated
// value once available, else the conservative-high code-constant fallback.
func currentNestedEstUnit() int64 {
	estUnitMu.Lock()
	est := estUnitBytes
	calibrated := estUnitCalibrated
	estUnitMu.Unlock()
	if !calibrated || est <= 0 {
		return defaultNestedEstUnitBytes
	}
	return est
}

// calibrateNestedEstUnit records the first tree's MEASURED live-heap delta as
// the empirical per-tree weight (C46-1, one-shot). A delta <= 0 (GC ran
// mid-tree, or a trivial tree) is ignored so we don't calibrate to an
// unrealistically small weight.
func calibrateNestedEstUnit(measuredDelta int64) {
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

// admissionCeiling returns (ceiling, unlimited). ceiling = headroom − reserve =
// (GOMEMLIMIT − liveHeap) − GOMEMLIMIT/reserveFraction, the max aggregate
// in-flight weight admittable right now. unlimited==true when GOMEMLIMIT is the
// runtime's unset default (math.MaxInt64) → no soft ceiling to protect, the
// gate is transparent.
func admissionCeiling() (ceiling int64, unlimited bool) {
	limit := runtimeLimitFn()
	if limit <= 0 || limit == math.MaxInt64 {
		return 0, true // GOMEMLIMIT unset/unlimited — transparent pass-through.
	}
	reserve := limit / reserveFractionDen * reserveFractionNum
	headroom := limit - liveHeapFn()
	ceiling = headroom - reserve
	if ceiling < 0 {
		ceiling = 0
	}
	return ceiling, false
}

// enterNestedResolveUnit is the aggregate cold-fan-out admission gate, applied
// as a lifecycle bracket at the OUTERMOST nested resolve. The caller MUST gate
// it on the depth-0 condition (isOutermostNestedResolve) so the whole subtree
// holds exactly ONE admission — inner recursive resolves (depth>0) do NOT
// re-enter. `background` (from cache.BackgroundResolveFromContext) marks a
// refresher/prewarm tree; a customer /call is background=false.
//
// Admission (shape A, recompute-per-admission) + C5 customer priority:
//   - unlimited GOMEMLIMIT ⇒ transparent: admit immediately, release is a no-op.
//   - inFlightCount == 0 ⇒ admit unconditionally (guaranteed-progress /
//     anti-deadlock: the only tree runs ALONE even if it "won't fit").
//   - a CUSTOMER tree admits iff inFlightWeight + estUnit <= ceiling; while it
//     waits it increments waitingCustomers so background trees yield to it.
//   - a BACKGROUND tree admits iff (inFlightWeight + estUnit <= ceiling) AND
//     waitingCustomers == 0 — it NEVER admits ahead of a waiting customer
//     (C5 no-new-background-ahead rule → customer absolute priority). A
//     background tree has no browser deadline: it waits ctx-bounded by its OWN
//     ctx and never gets an honest-503 terminal from the gate (only its own
//     ctx cancellation ends its wait).
//   - either kind, if it cannot admit, WAITS (ctx-bounded) on a release, then
//     re-checks. On ctx cancel/deadline → return the ctx error (the caller
//     surfaces an honest 503-class error, never empty/raw content).
//
// COUNTS-WHILE-IN-FLIGHT (C5 load-bearing): once ADMITTED, a background tree
// weighs against inFlightWeight/inFlightCount EXACTLY like a customer tree — the
// OOM floor is identical for both; background differs ONLY at admission, never
// in accounting.
//
// On admission, inFlightWeight += estUnit, inFlightCount++, and a live-heap
// sample is taken; release() re-samples, calibrates the per-tree weight
// (one-shot), decrements, and broadcasts to wake waiters.
func enterNestedResolveUnit(ctx context.Context) (release func(), err error) {
	b := theBound()
	est := currentNestedEstUnit()
	// BACKGROUND = the refresher (WithBackgroundResolve) OR any prewarm/SA
	// content-population pass (WithApistagePrewarm) — both are non-customer and
	// yield to a waiting customer /call. A customer /call carries neither.
	background := cache.BackgroundResolveFromContext(ctx) || cache.ApistagePrewarmFromContext(ctx)

	b.mu.Lock()
	countedWaiting := false
	// unwait clears a customer's waiting-mark on any exit from the wait loop and
	// broadcasts so a background tree that was yielding to this waiter re-checks
	// (esp. the ctx-error exit of the last waiting customer, which frees no
	// capacity but lifts the yield). b.mu is held by the caller.
	unwait := func() {
		if countedWaiting {
			b.waitingCustomers--
			if b.waitingCustomers < 0 {
				b.waitingCustomers = 0
			}
			countedWaiting = false
			b.cond.Broadcast()
		}
	}
	for {
		ceiling, unlimited := admissionCeiling()
		fits := b.inFlightWeight+est <= ceiling
		// C5 priority: a BACKGROUND tree yields to a waiting customer on EVERY
		// admit path — including the inFlightCount==0 escape (else a background
		// tree winning the b.mu race for a just-freed slot would jump the queue
		// ahead of the waiting customer). A CUSTOMER keeps the unconditional
		// escapes (transparent / empty-gate progress / fits).
		//   customer  : admit iff unlimited || inFlightCount==0 || fits
		//   background: admit iff waitingCustomers==0 AND (unlimited ||
		//               inFlightCount==0 || fits)
		// The empty-gate (inFlightCount==0) admit preserves guaranteed progress:
		// if NO customer is waiting, a lone background tree still runs (and a
		// waiting customer, being a customer, always gets the empty gate first).
		baseAdmit := unlimited || b.inFlightCount == 0 || fits
		admit := baseAdmit && (!background || b.waitingCustomers == 0)
		if admit {
			unwait()
			b.inFlightWeight += est
			b.inFlightCount++
			b.mu.Unlock()
			break
		}
		// Cannot admit → wait. A CUSTOMER marks itself waiting so background
		// trees yield to it (decremented on any exit via unwait). A background
		// tree does NOT mark (it must never make another background tree yield).
		if !background && !countedWaiting {
			b.waitingCustomers++
			countedWaiting = true
		}
		if werr := b.waitForReleaseOrCtx(ctx); werr != nil {
			unwait()
			b.mu.Unlock()
			return nil, werr
		}
		if cerr := ctx.Err(); cerr != nil {
			unwait()
			b.mu.Unlock()
			return nil, cerr
		}
		// woken (release or ctx) — loop re-checks ceiling + priority + ctx.
	}

	before := liveHeapFn()
	var released bool
	return func() {
		b.mu.Lock()
		if released {
			b.mu.Unlock()
			return
		}
		released = true
		after := liveHeapFn()
		if after > before {
			calibrateNestedEstUnit(after - before)
		}
		b.inFlightWeight -= est
		if b.inFlightWeight < 0 {
			b.inFlightWeight = 0
		}
		b.inFlightCount--
		if b.inFlightCount < 0 {
			b.inFlightCount = 0
		}
		b.cond.Broadcast()
		b.mu.Unlock()
	}, nil
}

// waitForReleaseOrCtx blocks on b.cond until a release Broadcasts OR ctx is
// done. b.mu is held on entry and on return. Because sync.Cond has no ctx
// integration, a watcher goroutine Broadcasts on ctx.Done() to wake the Wait;
// the caller's loop then re-checks ctx.Err(). Returns ctx.Err() only if ctx is
// already done on entry (fast path); otherwise returns nil (woken → re-check).
func (b *aggregateBound) waitForReleaseOrCtx(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Arm a one-shot ctx watcher that Broadcasts on cancellation so a blocked
	// Wait cannot miss the ctx deadline. stop closes when this wait resolves so
	// the watcher goroutine always exits (no leak).
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			b.mu.Lock()
			b.cond.Broadcast()
			b.mu.Unlock()
		case <-stop:
		}
	}()
	b.cond.Wait() // releases b.mu while blocked; re-acquires on wake
	close(stop)
	return nil
}

// isOutermostNestedResolve reports whether ctx is at the TOP of a nested
// subtree (depth 0) — the single point where enterNestedResolveUnit admits.
func isOutermostNestedResolve(ctx context.Context) bool {
	return cache.NestedCallDepthFromContext(ctx) == 0
}

// --- test seams (C4: drive the gate via injected headroom, NOT an env) -------

// resetNestedResolveBoundForTest clears the process-wide gate + calibration and
// restores the production runtime seams. Test-only.
func resetNestedResolveBoundForTest() {
	boundMu.Lock()
	boundOnce = nil
	boundMu.Unlock()
	estUnitMu.Lock()
	estUnitBytes = 0
	estUnitCalibrated = false
	estUnitMu.Unlock()
	runtimeLimitFn = prodRuntimeMemoryLimit
	liveHeapFn = prodLiveHeapBytes
}

// setRuntimeSeamsForTest injects deterministic GOMEMLIMIT + live-heap samplers
// so a falsifier drives admission by BYTE headroom without a resurrected env
// (C4). limitFn/liveFn may close over test state (e.g. a live-heap counter that
// tracks admitted weight). Test-only.
func setRuntimeSeamsForTest(limitFn, liveFn func() int64) {
	runtimeLimitFn = limitFn
	liveHeapFn = liveFn
}

// calibratedNestedEstUnitForTest returns (estUnitBytes, calibrated). Test-only.
func calibratedNestedEstUnitForTest() (int64, bool) {
	estUnitMu.Lock()
	defer estUnitMu.Unlock()
	return estUnitBytes, estUnitCalibrated
}

// inFlightWeightForTest returns the current aggregate in-flight weight + count.
// Test-only.
func inFlightWeightForTest() (int64, int) {
	b := theBound()
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inFlightWeight, b.inFlightCount
}
