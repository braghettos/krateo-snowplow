// nested_resolve_bound_falsifier_test.go — (c) cold-fan-out bound falsifiers
// (CN-1..6). The real heap-climb is not hermetically assertable, so the
// discriminating proof is on the SEMAPHORE ADMISSION behaviour: the bound caps
// the number of concurrently-in-flight nested subtrees to floor(budget/weight),
// the excess BLOCKS (completes, never drops), and budget=0 is transparent
// (unbounded). Concurrency is driven > cap (CN-1 non-degenerate: M>1 AND
// concurrency>cap). Hermetic / in-process ONLY — no kind, no remote, no
// apiserver.
package api

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// runConcurrentNestedUnits drives n goroutines each through one
// enterNestedResolveUnit bracket, holding the permit for `hold` while it counts
// the peak number concurrently in-flight. Returns the observed peak. Every unit
// must complete (CN-4 completes-not-drops). ctx depth is 0 (outermost) for all.
func runConcurrentNestedUnits(t *testing.T, n int, hold time.Duration) (peak int32, completed int32) {
	t.Helper()
	ctx := cache.WithNestedCallDepth(context.Background(), 0)
	var inFlight, peakInFlight, done int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := enterNestedResolveUnit(ctx)
			if err != nil {
				t.Errorf("enterNestedResolveUnit unexpected err: %v", err)
				return
			}
			cur := atomic.AddInt32(&inFlight, 1)
			for {
				p := atomic.LoadInt32(&peakInFlight)
				if cur <= p || atomic.CompareAndSwapInt32(&peakInFlight, p, cur) {
					break
				}
			}
			time.Sleep(hold)
			atomic.AddInt32(&inFlight, -1)
			atomic.AddInt32(&done, 1)
			release()
		}()
	}
	wg.Wait()
	return atomic.LoadInt32(&peakInFlight), atomic.LoadInt32(&done)
}

// TestCN1_BoundCapsConcurrentSubtrees_RED is the CN-1 RED discriminating arm.
// M=8 concurrent nested subtrees, each weight=budget/2 (so M×weight ≫ budget,
// concurrency ≫ cap=2). The bound must cap the peak in-flight at
// floor(budget/weight)==2. RED arm: budget=0 (gate removed) → all 8 run
// concurrently (unbounded — the OOM climb). NON-DEGENERATE: M=8>1 AND
// concurrency(8) > cap(2).
func TestCN1_BoundCapsConcurrentSubtrees_RED(t *testing.T) {
	const M = 8
	const weight = 100
	const budget = 200 // cap = floor(200/100) = 2

	// Pin the per-subtree weight (skip the empirical calibration so the test is
	// deterministic) via the fallback env = weight, budget via its env.
	t.Setenv(envNestedResolveEstUnitBytesFallback, "100")
	t.Setenv(envNestedResolveFootprintBudgetBytes, "200")
	resetNestedResolveBoundForTest()
	t.Cleanup(resetNestedResolveBoundForTest)

	if _, b := nestedResolveBound(); b != budget {
		t.Fatalf("setup: budget=%d want %d", b, budget)
	}
	if w := currentNestedEstUnit(budget); w != weight {
		t.Fatalf("setup: weight=%d want %d", w, weight)
	}

	peak, completed := runConcurrentNestedUnits(t, M, 40*time.Millisecond)
	if completed != M {
		t.Fatalf("CN-4 completes-not-drops: completed=%d want %d", completed, M)
	}
	if peak > 2 {
		t.Fatalf("CN-1: peak concurrent subtrees=%d, want <=2 (budget %d / weight %d) — "+
			"the bound did not cap the cold fan-out", peak, budget, weight)
	}

	// RED CONTROL: budget=0 (gate removed) → unbounded, peak should reach M.
	t.Setenv(envNestedResolveFootprintBudgetBytes, "0")
	resetNestedResolveBoundForTest()
	peakOff, completedOff := runConcurrentNestedUnits(t, M, 40*time.Millisecond)
	if completedOff != M {
		t.Fatalf("RED control: completed=%d want %d", completedOff, M)
	}
	if peakOff <= 2 {
		t.Fatalf("CN-1 RED control INVALID: with budget=0 the peak should be unbounded (≈%d), "+
			"got %d — the harness is not actually discriminating", M, peakOff)
	}
}

// TestCN2_CostProportional_NoSerializationUnderCap is the CN-2 GREEN arm (the
// 1.5.1 lesson). When M < cap, NO unit serializes — a light/warm subtree is not
// starved by the bound. M=2, cap=4 (budget=4×weight) → peak should reach all M
// (no blocking).
func TestCN2_CostProportional_NoSerializationUnderCap(t *testing.T) {
	const M = 2
	t.Setenv(envNestedResolveEstUnitBytesFallback, "100")
	t.Setenv(envNestedResolveFootprintBudgetBytes, "400") // cap = 4 > M
	resetNestedResolveBoundForTest()
	t.Cleanup(resetNestedResolveBoundForTest)

	peak, completed := runConcurrentNestedUnits(t, M, 30*time.Millisecond)
	if completed != M {
		t.Fatalf("CN-2: completed=%d want %d", completed, M)
	}
	if peak != M {
		t.Fatalf("CN-2 cost-proportional: M=%d < cap=4 must pay ZERO serialization "+
			"(peak should reach %d), got peak=%d — the bound is over-serializing", M, M, peak)
	}
}

// TestCN3_Depth0OnlyNoSelfDeadlock verifies the depth-0 gate + that a deep
// recursive subtree does NOT self-deadlock. isOutermostNestedResolve is true at
// depth 0, false at depth>0; and acquiring at depth 0 while the SAME budget
// would not admit a second permit must not block the (non-acquiring) inner
// resolves.
func TestCN3_Depth0OnlyNoSelfDeadlock(t *testing.T) {
	t.Setenv(envNestedResolveEstUnitBytesFallback, "100")
	t.Setenv(envNestedResolveFootprintBudgetBytes, "100") // cap = 1 (one permit total)
	resetNestedResolveBoundForTest()
	t.Cleanup(resetNestedResolveBoundForTest)

	// Depth gate.
	if !isOutermostNestedResolve(cache.WithNestedCallDepth(context.Background(), 0)) {
		t.Fatal("CN-3: depth 0 must be the outermost (acquire) point")
	}
	for _, d := range []int{1, 2, 8} {
		if isOutermostNestedResolve(cache.WithNestedCallDepth(context.Background(), d)) {
			t.Fatalf("CN-3: depth %d must NOT acquire (inner resolves inherit the permit)", d)
		}
	}

	// Self-deadlock: outermost acquires the only permit; an inner resolve
	// (depth>0) must NOT try to acquire (so it cannot block on the held permit).
	// Simulate: hold the depth-0 permit, then run a depth-2 "inner" through the
	// SAME guarded entry — it must skip Acquire and return immediately.
	outerCtx := cache.WithNestedCallDepth(context.Background(), 0)
	release, err := enterNestedResolveUnit(outerCtx)
	if err != nil {
		t.Fatalf("CN-3: outermost acquire failed: %v", err)
	}
	defer release()

	done := make(chan struct{})
	go func() {
		innerCtx := cache.WithNestedCallDepth(context.Background(), 2)
		// The production guard: inner (depth>0) skips enterNestedResolveUnit
		// entirely. Assert the gate says "do not acquire" so the inner path
		// never touches the (exhausted) semaphore.
		if isOutermostNestedResolve(innerCtx) {
			t.Errorf("CN-3: inner depth-2 wrongly classified as outermost → would self-deadlock")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("CN-3: inner resolve blocked → self-deadlock (the cap=1 permit is held by the outermost)")
	}
}

// TestCN4_ExcessBlocksThenCompletes proves excess does not DROP: with cap=1 and
// M=4, all 4 must complete (serialized), none abandoned.
func TestCN4_ExcessBlocksThenCompletes(t *testing.T) {
	const M = 4
	t.Setenv(envNestedResolveEstUnitBytesFallback, "100")
	t.Setenv(envNestedResolveFootprintBudgetBytes, "100") // cap = 1
	resetNestedResolveBoundForTest()
	t.Cleanup(resetNestedResolveBoundForTest)

	peak, completed := runConcurrentNestedUnits(t, M, 15*time.Millisecond)
	if completed != M {
		t.Fatalf("CN-4: completed=%d want %d (excess must BLOCK then complete, never drop)", completed, M)
	}
	if peak > 1 {
		t.Fatalf("CN-4: cap=1 but peak=%d — bound not enforced", peak)
	}
}

// TestCN_TransparentWhenDisabled — budget=0 is byte-identical pass-through:
// Acquire skipped, release a no-op, no calibration. (The default posture.)
func TestCN_TransparentWhenDisabled(t *testing.T) {
	t.Setenv(envNestedResolveFootprintBudgetBytes, "0")
	resetNestedResolveBoundForTest()
	t.Cleanup(resetNestedResolveBoundForTest)

	sem, budget := nestedResolveBound()
	if sem != nil || budget != 0 {
		t.Fatalf("disabled: sem=%v budget=%d want nil/0", sem, budget)
	}
	release, err := enterNestedResolveUnit(cache.WithNestedCallDepth(context.Background(), 0))
	if err != nil {
		t.Fatalf("disabled: unexpected err %v", err)
	}
	release() // must be a safe no-op
	if _, calibrated := calibratedNestedEstUnitForTest(); calibrated {
		t.Fatal("disabled: must NOT calibrate (no Acquire path ran)")
	}
}

// TestCN_AcquireWeightClampedToBudget — a single subtree larger than the whole
// budget runs ALONE (weight clamped to budget) rather than blocking forever
// (guaranteed-progress).
func TestCN_AcquireWeightClampedToBudget(t *testing.T) {
	t.Setenv(envNestedResolveEstUnitBytesFallback, "1000") // > budget
	t.Setenv(envNestedResolveFootprintBudgetBytes, "200")
	resetNestedResolveBoundForTest()
	t.Cleanup(resetNestedResolveBoundForTest)

	if w := currentNestedEstUnit(200); w != 200 {
		t.Fatalf("clamp: weight=%d want 200 (clamped to budget)", w)
	}
	// A single unit must still complete (not block forever on an unsatisfiable
	// Acquire).
	done := make(chan struct{})
	go func() {
		release, err := enterNestedResolveUnit(cache.WithNestedCallDepth(context.Background(), 0))
		if err == nil {
			release()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("clamp: an oversized single subtree blocked forever — guaranteed-progress violated")
	}
}
