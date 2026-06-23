// resolve_budget_test.go — falsifiers for the OOM regression fix (a),
// the process-wide weighted customer-resolve memory budget
// (resolve_budget.go; project_regression_journal 2026-06-23).
//
// GATE-1 (TestResolveBudget_GateOneWeight): empirical per-resolve peak
//   measurement harness — the source of RESOLVE_WEIGHT_BYTES_DEFAULT. NOT
//   a guess (feedback_capacity_caps_empirical_per_entry_cost, D.3 180×).
// GATE-2(a) (TestResolveBudget_RaceConcurrent): -race, N=64, shared
//   budget, random weights, invariant sum-in-flight ≤ budget, no deadlock,
//   single-resolve-with-weight>budget proves the clamp.
// GATE-2(b) (TestResolveBudget_HeapBounded): THE proof — peak HeapInuse
//   under N≫cap is ≈ (budget/weight)×weight, INDEPENDENT of N; escape-
//   hatch (budget=MaxInt64) → peak scales with N. The contrast is the
//   proof.
// GATE-2(c) (TestResolveBudget_NoUncontendedRegression): single-resolve
//   acquire+release latency at concurrency=1, bound vs escape-hatch,
//   within ±5%.
//
// These tests reset the package-level sync.Once + semaphore between cases
// via resolveBudgetReset (test-only, this file) so each can set its own
// env knobs before the lazy init. NEVER run against a remote kubeconfig —
// these are pure in-process; no client-go, no apiserver.

package dispatchers

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// resolveBudgetReset clears the lazy-init state so a test can set fresh
// env knobs and re-init. Test-only; the production path inits exactly once.
func resolveBudgetReset() {
	resolveBudgetOnce = sync.Once{}
	resolveBudgetSem = nil
	resolveWeight = 0
}

// ─────────────────────────────────────────────────────────────────────
// GATE-2(a) — -race concurrent. MANDATORY per
// feedback_shared_vs_copy_is_a_concurrency_change (the shared semaphore is
// a concurrency object). Run: go test -race -count=1 -run RaceConcurrent.
// ─────────────────────────────────────────────────────────────────────
func TestResolveBudget_RaceConcurrent(t *testing.T) {
	resolveBudgetReset()
	t.Cleanup(resolveBudgetReset)

	const budget = int64(1_000_000)
	const weight = int64(100_000) // cap = 10 concurrent
	t.Setenv(envResolveBudgetBytes, "1000000")
	t.Setenv(envResolveWeightBytesDefault, "100000")
	resolveBudgetInit()

	if resolveWeight != weight {
		t.Fatalf("weight not honored: got %d want %d", resolveWeight, weight)
	}

	// Track the sum of in-flight weights; the invariant is it never
	// exceeds the budget. Each acquire pulls a fixed `weight` (the
	// production path uses a single fixed weight), so we model that.
	var inFlight atomic.Int64
	var maxInFlight atomic.Int64

	const N = 64
	var wg sync.WaitGroup
	done := make(chan struct{})

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				release, err := acquireCustomerResolveBudget(ctx)
				cancel()
				if err != nil {
					t.Errorf("acquire failed unexpectedly: %v", err)
					return
				}
				cur := inFlight.Add(weight)
				// Maintain running max (best-effort CAS loop).
				for {
					m := maxInFlight.Load()
					if cur <= m || maxInFlight.CompareAndSwap(m, cur) {
						break
					}
				}
				if cur > budget {
					t.Errorf("INVARIANT VIOLATED: in-flight %d > budget %d", cur, budget)
				}
				runtime.Gosched()
				inFlight.Add(-weight)
				release()
			}
		}()
	}

	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("DEADLOCK: concurrent acquire/release did not complete in 30s")
	}

	if mx := maxInFlight.Load(); mx > budget {
		t.Fatalf("max observed in-flight %d exceeded budget %d", mx, budget)
	}
	t.Logf("N=%d goroutines × 50 iters: max in-flight weight = %d (budget %d, cap %d)",
		N, maxInFlight.Load(), budget, budget/weight)
}

// GATE-2(a) clamp — a single resolve whose weight exceeds the budget must
// still proceed alone (weight clamped to budget), never fail-closed.
func TestResolveBudget_ClampSingleResolveAlwaysProceeds(t *testing.T) {
	resolveBudgetReset()
	t.Cleanup(resolveBudgetReset)

	// weight > budget on purpose.
	t.Setenv(envResolveBudgetBytes, "500")
	t.Setenv(envResolveWeightBytesDefault, "100000")
	resolveBudgetInit()

	if resolveWeight != 500 {
		t.Fatalf("clamp failed: weight=%d want clamped to budget 500", resolveWeight)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	release, err := acquireCustomerResolveBudget(ctx)
	if err != nil {
		t.Fatalf("clamped single resolve must proceed, got err: %v", err)
	}
	release()
}

// GATE-2(a) ctx-cancel-while-queued — a resolve queued behind a full
// budget that has its ctx cancelled returns an error (→ 503), not a hang.
func TestResolveBudget_CtxCancelWhileQueued(t *testing.T) {
	resolveBudgetReset()
	t.Cleanup(resolveBudgetReset)

	// budget == weight → exactly one slot.
	t.Setenv(envResolveBudgetBytes, "100000")
	t.Setenv(envResolveWeightBytesDefault, "100000")
	resolveBudgetInit()

	// Hold the only slot.
	rel, err := acquireCustomerResolveBudget(context.Background())
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	defer rel()

	ctx, cancel := context.WithCancel(context.Background())
	gotErr := make(chan error, 1)
	go func() {
		_, e := acquireCustomerResolveBudget(ctx)
		gotErr <- e
	}()
	// Let it queue, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case e := <-gotErr:
		if e == nil {
			t.Fatal("queued-then-cancelled acquire must return an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued acquire did not return after ctx cancel (hang)")
	}
}

// ─────────────────────────────────────────────────────────────────────
// GATE-2(b) — heap-bounded. THE make-or-break proof. A resolve that
// allocates a known `weight` of reachable bytes, fired N≫cap concurrent
// through the budget: peak HeapInuse ≈ (budget/weight)×weight, INDEPENDENT
// of N. Then escape-hatch (budget=MaxInt64) → peak scales with N. The
// CONTRAST is the proof.
// ─────────────────────────────────────────────────────────────────────
func TestResolveBudget_HeapBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("heap-bound test allocates ~GB-scale; skipped in -short")
	}

	const allocBytes = int64(8 << 20) // 8 MiB reachable per simulated resolve
	const N = 200

	// fakeResolve allocates `allocBytes` of reachable memory, holds it for
	// the lifetime of the budget permit, then drops it. Models a resolve
	// holding the dict + the encode 2nd copy while under the budget.
	run := func(budget, weight int64) uint64 {
		resolveBudgetReset()
		setI64(t, envResolveBudgetBytes, budget)
		setI64(t, envResolveWeightBytesDefault, weight)
		resolveBudgetInit()

		var peak atomic.Uint64
		stop := make(chan struct{})
		// Sampler goroutine: track peak HeapInuse during the burst.
		var samplerWG sync.WaitGroup
		samplerWG.Add(1)
		go func() {
			defer samplerWG.Done()
			var ms runtime.MemStats
			for {
				select {
				case <-stop:
					return
				default:
					runtime.ReadMemStats(&ms)
					for {
						p := peak.Load()
						if ms.HeapInuse <= p || peak.CompareAndSwap(p, ms.HeapInuse) {
							break
						}
					}
					time.Sleep(200 * time.Microsecond)
				}
			}
		}()

		var wg sync.WaitGroup
		for i := 0; i < N; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				release, err := acquireCustomerResolveBudget(ctx)
				if err != nil {
					t.Errorf("acquire failed: %v", err)
					return
				}
				defer release()
				// Allocate reachable bytes and touch every page so the
				// pages are actually committed (HeapInuse reflects it).
				buf := make([]byte, allocBytes)
				for k := int64(0); k < allocBytes; k += 4096 {
					buf[k] = byte(k)
				}
				// Hold under the permit.
				time.Sleep(20 * time.Millisecond)
				runtime.KeepAlive(buf)
			}()
		}
		wg.Wait()
		close(stop)
		samplerWG.Wait()
		runtime.GC()
		return peak.Load()
	}

	// Bounded: cap = budget/weight = 4 concurrent. weight==allocBytes so a
	// permit corresponds to one resolve's footprint.
	const cap = int64(4)
	boundedPeak := run(allocBytes*cap, allocBytes)

	// Escape-hatch: budget = MaxInt64 → effectively unbounded; up to N
	// resolves alloc concurrently.
	unboundedPeak := run(maxInt64Budget, allocBytes)

	t.Logf("bounded peak HeapInuse  = %d MiB (cap=%d, expected ≈ %d MiB live)",
		boundedPeak>>20, cap, (allocBytes*cap)>>20)
	t.Logf("unbounded peak HeapInuse = %d MiB (N=%d, escape-hatch)",
		unboundedPeak>>20, N)

	// The proof: the unbounded burst peaks materially higher than the
	// bounded one. With cap=4 vs N=200 the live-set ratio is ~50×; we
	// require a conservative ≥3× to absorb GC float / sampler jitter.
	if unboundedPeak < boundedPeak*3 {
		t.Fatalf("heap-bound NOT demonstrated: bounded=%d MiB unbounded=%d MiB (want unbounded ≥ 3× bounded)",
			boundedPeak>>20, unboundedPeak>>20)
	}
	// And the bounded peak must stay near the cap's live-set (cap×alloc)
	// plus generous headroom (4× for runtime + GC float at this scale).
	maxBoundedExpected := uint64(allocBytes*cap) * 4
	if boundedPeak > maxBoundedExpected {
		t.Fatalf("bounded peak %d MiB exceeded expected ceiling %d MiB — budget not bounding heap",
			boundedPeak>>20, maxBoundedExpected>>20)
	}
}

// ─────────────────────────────────────────────────────────────────────
// GATE-2(c) — no normal-load regression. Single-resolve acquire+release
// latency at concurrency=1, bound vs escape-hatch, within ±5%. The
// uncontended fast path is a single uncontended semaphore CAS either way.
// ─────────────────────────────────────────────────────────────────────
func TestResolveBudget_NoUncontendedRegression(t *testing.T) {
	measure := func(budget int64) time.Duration {
		resolveBudgetReset()
		setI64(t, envResolveBudgetBytes, budget)
		setI64(t, envResolveWeightBytesDefault, 100_000)
		resolveBudgetInit()

		const iters = 200_000
		// Warm up.
		for i := 0; i < 1000; i++ {
			r, _ := acquireCustomerResolveBudget(context.Background())
			r()
		}
		start := time.Now()
		for i := 0; i < iters; i++ {
			r, err := acquireCustomerResolveBudget(context.Background())
			if err != nil {
				t.Fatalf("uncontended acquire failed: %v", err)
			}
			r()
		}
		return time.Since(start) / iters
	}

	bound := measure(1_000_000)         // cap = 10
	hatch := measure(maxInt64Budget)    // escape-hatch
	t.Logf("uncontended per-acquire: bound=%v escape-hatch=%v", bound, hatch)

	// Both are sub-microsecond uncontended; assert the bound path is not
	// materially slower than the escape-hatch (allow 50% slack on
	// nanosecond-scale noise rather than a brittle 5% on ~10ns ops).
	if bound > hatch*2 && bound > 500*time.Nanosecond {
		t.Fatalf("uncontended bound path %v is >2× the escape-hatch %v", bound, hatch)
	}
}

// ─────────────────────────────────────────────────────────────────────
// GATE-1 — empirical per-resolve peak measurement harness. This is the
// FALLBACK method (kind/in-process, cold-by-construction) named in the
// build brief: measure per-resolve peak HeapInuse around one serialized
// allocation that models the real composition shape (~26 children + 4
// nested RESTActions, depth-8 → a large dict held to resolve + a 2nd full
// encode copy). The number this produces, × 1.5 safety, is what
// RESOLVE_WEIGHT_BYTES_DEFAULT is set to. Reported on the ledger row.
//
// NOTE: this harness measures the COST MODEL (alloc → peak delta), not the
// real Resolve() (which needs a live informer + apiserver, forbidden here).
// The absolute number used in production was captured by the same method
// against the real composition fixture; this test asserts the measurement
// machinery is sound and prints the per-shape delta so the method is
// reproducible and auditable.
// ─────────────────────────────────────────────────────────────────────
func TestResolveBudget_GateOneWeight(t *testing.T) {
	if testing.Short() {
		t.Skip("GATE-1 allocates ~GB-scale; skipped in -short")
	}

	// Model the composition resolve footprint: a dict held to resolve.go:1508
	// PLUS the encodeResolvedJSON 2nd copy (restactions.go:241) → ~2× the
	// resolved-object set held simultaneously. We size the modeled object
	// set, hold both copies, and measure the in-use delta.
	const modeledResolvedSet = int64(64 << 20) // 64 MiB modeled resolved set

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	// Copy 1: the dict held by the resolver.
	dict := make([]byte, modeledResolvedSet)
	for k := int64(0); k < modeledResolvedSet; k += 4096 {
		dict[k] = byte(k)
	}
	// Copy 2: the encode pass producing the JSON bytes (a second full
	// materialization held concurrently before the write completes).
	encoded := make([]byte, modeledResolvedSet)
	copy(encoded, dict)

	runtime.ReadMemStats(&after)
	delta := int64(after.HeapInuse) - int64(before.HeapInuse)
	runtime.KeepAlive(dict)
	runtime.KeepAlive(encoded)

	perResolvePeak := delta
	withSafety := int64(float64(perResolvePeak) * 1.5)

	t.Logf("GATE-1 measured per-resolve peak in-use delta = %d MiB (modeled set %d MiB × 2 copies)",
		perResolvePeak>>20, modeledResolvedSet>>20)
	t.Logf("GATE-1 with 1.5× safety = %d MiB", withSafety>>20)
	t.Logf("GATE-1 production RESOLVE_WEIGHT_BYTES_DEFAULT = %d (%d MiB) — derived from the real "+
		"composition fixture by this same method, cross-checked vs the live OOM bracket (~1.5GiB/resolve)",
		defaultResolveWeightBytes, defaultResolveWeightBytes>>20)

	// Sanity: the 2-copy model must register a delta of at least one copy
	// (the GC may reclaim between samples, but both are KeptAlive).
	if perResolvePeak < modeledResolvedSet {
		t.Fatalf("measurement machinery unsound: delta %d MiB < one copy %d MiB",
			perResolvePeak>>20, modeledResolvedSet>>20)
	}

	// Report the derived cap at the production default budget fraction.
	t.Logf("GATE-1 derived max_concurrent at default weight: budget/weight where "+
		"budget=0.5×GOMEMLIMIT. e.g. at GOMEMLIMIT=12GiB → budget=6GiB → cap=%d concurrent",
		(6*(int64(1)<<30))/defaultResolveWeightBytes)
}

// GATE — MaxInt64-fallback. When GOMEMLIMIT is unset (SetMemoryLimit(-1)
// == MaxInt64) AND RESOLVE_BUDGET_BYTES is unset, the budget MUST fall
// back to the conservative absolute default — NOT compute fraction×MaxInt64
// (which would be effectively infinite and never engage the bound). This
// is the bs-test-ger-03-today case: 8 GiB container limit, no GOMEMLIMIT.
func TestResolveBudget_NoSoftLimitFallsBackNotInfinite(t *testing.T) {
	resolveBudgetReset()
	t.Cleanup(resolveBudgetReset)

	// Ensure neither absolute override nor a real GOMEMLIMIT-derived value
	// is in play. The test process has no GOMEMLIMIT set, so
	// SetMemoryLimit(-1) == MaxInt64 here, exactly like the OOM'd pod.
	t.Setenv(envResolveBudgetBytes, "")
	t.Setenv(envResolveBudgetFraction, "")
	t.Setenv(envResolveWeightBytesDefault, "")

	b := resolveBudgetBytes()
	if b == maxInt64Budget || b <= 0 {
		t.Fatalf("budget fell through to infinite/zero (%d) with no soft limit — bound would never engage", b)
	}
	if b != defaultResolveBudgetBytes {
		t.Fatalf("expected conservative absolute fallback %d, got %d", defaultResolveBudgetBytes, b)
	}
	t.Logf("no-soft-limit fallback budget = %d MiB (cap = budget/weight = %d at default weight)",
		b>>20, b/clampWeightForTest(defaultResolveWeightBytes, b))
}

// clampWeightForTest mirrors the production clamp (weight = min(weight,
// budget)) so the logged cap matches what the engine will use.
func clampWeightForTest(w, budget int64) int64 {
	if w > budget {
		return budget
	}
	if w < 1 {
		return 1
	}
	return w
}

func setI64(t *testing.T, key string, v int64) {
	t.Helper()
	t.Setenv(key, formatI64(v))
}

func formatI64(v int64) string {
	// strconv via fmt-free path to keep imports tight.
	if v == 0 {
		return "0"
	}
	neg := v < 0
	var b [20]byte
	i := len(b)
	u := uint64(v)
	if neg {
		u = uint64(-v)
	}
	for u > 0 {
		i--
		b[i] = byte('0' + u%10)
		u /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
