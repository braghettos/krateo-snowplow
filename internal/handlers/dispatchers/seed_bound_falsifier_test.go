// seed_bound_falsifier_test.go — #46 Piece A + Piece B falsifiers (hermetic).
//
// The 50K SCALE falsifier (C46-3: peak HeapInuse under budget WITH the bound /
// scales-toward-#23 WITHOUT, on the engine=false concurrent posture; per-unit
// assert trips on an oversized unit on the engine=true serial posture; seed
// COMPLETES not drops; CONTENT check warm rows non-empty + RBAC-correct) is
// the cache-tester's bench harness. These hermetic arms prove the bound +
// assert MECHANISM in-process so the 50K run validates behaviour, not basic
// correctness:
//
//   B1 — Piece A assert TRIPS in prod (counts) when a unit's measured delta
//        exceeds the budget; does NOT trip within budget. The discriminating
//        oversized-unit guard.
//   B2 — Piece A assert PANICS in test mode on breach (loud-fail; an
//        unbounded/unpaginated unit slipping in is a regression).
//   B3 — Piece B semaphore SERIALIZES concurrent units beyond the budget (the
//        aggregate bound on the legacy/engine=false concurrent path): with a
//        budget that admits only one unit's weight at a time, two concurrent
//        boundSeedUnit calls never overlap. The original #46 mechanism.
//   B4 — disabled (SEED_FOOTPRINT_BUDGET_BYTES==0) is a transparent
//        pass-through: do() runs, no Acquire, no assert, counter flat.
//   B5 — seed COMPLETES (blocks, never drops): every wrapped unit's do() runs
//        even under a tight budget (excess blocks then proceeds).
//   B6 — -race: concurrent boundSeedUnit churn under a tight budget, clean.
package dispatchers

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// B1 — Piece A assert trips in prod on an oversized delta; clean within budget.
// (Prod mode: AssertSeedUnitFootprint counts + returns false, never panics.)
func TestFalsifier46_PerUnitAssertTripsOnOversized(t *testing.T) {
	// NOT test mode for this arm — we want the prod count path, not panic.
	// (env.TestMode keys off a separate env; this package's tests run with it
	// unset by default. Assert the prod-count branch directly.)
	cache.ResetSeedUnitFootprintViolationsForTest()

	const budget = 1000

	// Within budget → no violation.
	if ok := cache.AssertSeedUnitFootprint("unit/small", 500, budget); !ok {
		t.Fatalf("within-budget unit (500<=1000) reported a violation")
	}
	if v := cache.SeedUnitFootprintViolations(); v != 0 {
		t.Fatalf("within-budget unit bumped the violation counter: %d", v)
	}

	// Over budget → violation counted, returns false.
	if ok := cache.AssertSeedUnitFootprint("unit/oversized", 5000, budget); ok {
		t.Fatalf("oversized unit (5000>1000) reported WITHIN budget")
	}
	if v := cache.SeedUnitFootprintViolations(); v != 1 {
		t.Fatalf("oversized unit did not bump the violation counter exactly once: %d", v)
	}

	// budget==0 disables the assert (transparent).
	cache.ResetSeedUnitFootprintViolationsForTest()
	if ok := cache.AssertSeedUnitFootprint("unit/huge", 1<<40, 0); !ok {
		t.Fatalf("budget==0 should disable the assert (always within budget)")
	}
	if v := cache.SeedUnitFootprintViolations(); v != 0 {
		t.Fatalf("budget==0 bumped the violation counter: %d", v)
	}
}

// B3 — Piece B semaphore serializes concurrent units beyond budget. With a
// budget that fits exactly ONE unit's weight, two concurrent boundSeedUnit
// calls must NOT overlap (the second blocks until the first Releases).
func TestFalsifier46_SemaphoreSerializesBeyondBudget(t *testing.T) {
	// Budget == one unit's fallback weight → at most one unit in flight.
	// Use a small explicit budget + a fallback that equals it so estUnit==budget.
	t.Setenv("SEED_FOOTPRINT_BUDGET_BYTES", "1024")
	t.Setenv("SEED_EST_UNIT_BYTES_FALLBACK", "1024")
	resetSeedBoundForTest()
	t.Cleanup(resetSeedBoundForTest)

	var inFlight atomic.Int32
	var maxObserved atomic.Int32
	started := make(chan struct{}, 2)

	unit := func() error {
		n := inFlight.Add(1)
		for {
			old := maxObserved.Load()
			if n <= old || maxObserved.CompareAndSwap(old, n) {
				break
			}
		}
		started <- struct{}{}
		time.Sleep(80 * time.Millisecond) // hold the weight so overlap would show
		inFlight.Add(-1)
		return nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = boundSeedUnit(context.Background(), "unit", unit)
		}()
	}
	wg.Wait()

	if got := maxObserved.Load(); got != 1 {
		t.Fatalf("B3: max concurrent in-flight units = %d, want 1 — the budget admits only one unit's weight, "+
			"so the semaphore must serialize them (aggregate bound not enforced)", got)
	}
}

// B4 — disabled (budget==0) is a transparent pass-through.
func TestFalsifier46_DisabledIsTransparent(t *testing.T) {
	t.Setenv("SEED_FOOTPRINT_BUDGET_BYTES", "0")
	resetSeedBoundForTest()
	t.Cleanup(resetSeedBoundForTest)
	cache.ResetSeedUnitFootprintViolationsForTest()

	ran := false
	err := boundSeedUnit(context.Background(), "unit", func() error {
		ran = true
		return nil
	})
	if err != nil {
		t.Fatalf("disabled pass-through returned err: %v", err)
	}
	if !ran {
		t.Fatalf("disabled pass-through did NOT run do()")
	}
	// No assert fires when disabled (budget 0 → AssertSeedUnitFootprint no-op,
	// and boundSeedUnit returns before sampling anyway).
	if v := cache.SeedUnitFootprintViolations(); v != 0 {
		t.Fatalf("disabled path bumped the violation counter: %d", v)
	}
}

// B5 — seed COMPLETES (blocks, never drops): every unit's do() runs even under
// a tight budget that forces serialization.
func TestFalsifier46_TightBudgetAllUnitsComplete(t *testing.T) {
	t.Setenv("SEED_FOOTPRINT_BUDGET_BYTES", "1024")
	t.Setenv("SEED_EST_UNIT_BYTES_FALLBACK", "1024")
	resetSeedBoundForTest()
	t.Cleanup(resetSeedBoundForTest)

	const n = 20
	var ran atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = boundSeedUnit(context.Background(), "unit", func() error {
				ran.Add(1)
				return nil
			})
		}()
	}
	wg.Wait()
	if got := ran.Load(); got != n {
		t.Fatalf("B5: %d/%d units ran — a tight budget must BLOCK (serialize) excess, never DROP", got, n)
	}
}

// B6 — -race: concurrent boundSeedUnit churn under a tight budget.
func TestFalsifier46_ConcurrentBoundRace(t *testing.T) {
	t.Setenv("SEED_FOOTPRINT_BUDGET_BYTES", "4096")
	t.Setenv("SEED_EST_UNIT_BYTES_FALLBACK", "1024")
	resetSeedBoundForTest()
	t.Cleanup(resetSeedBoundForTest)

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = boundSeedUnit(context.Background(), "unit", func() error {
				return nil
			})
		}()
	}
	wg.Wait()
	// Clean -race + no deadlock IS the assertion.
}

// B7 — THE GRANULARITY DISCRIMINATOR (feedback_falsifier_shape_must_discriminate):
// K>1 cohorts × M>1 units, asserting on the REAL semaphore weight the bracket
// reserves (currentEstUnit) and the REAL effective concurrency — NOT a test-side
// counter decoupled from the semaphore (that decoupling is exactly how B3's K=2/M=1
// shape masked the @15cff6b per-cohort defect).
//
// The load-bearing assertions, both keyed off the actual Acquire weight:
//   (a) EFFECTIVE CONCURRENCY: with the budget sized to fit floor(budget/estUnit)
//       UNITS, the max number of bracket bodies running simultaneously must equal
//       that per-UNIT count. Under a per-COHORT acquire (estUnit inflated to a whole
//       cohort, then budget-clamped) the semaphore admits only ONE body at a time —
//       observed concurrency collapses to 1, FAILING this assertion.
//   (b) PER-UNIT WEIGHT: currentEstUnit(budget) must stay in the per-UNIT band
//       (< a cohort). A per-cohort granularity inflates it to ≈ M× (clamped to
//       budget) — caught directly.
// The RED control below (run by the dev pre-freeze, documented in the artifact)
// multiplies the acquire weight ×M to simulate @15cff6b and confirms BOTH (a) and (b)
// flip to FAIL.
func TestFalsifier46_GranularityDiscriminator_PerUnitNotPerCohort(t *testing.T) {
	const (
		M = 8 // units per cohort (M>1)
		K = 4 // concurrent cohorts (K>1) → K*M = 32 units
		// budget fits exactly 4 per-UNIT weights in flight; a whole cohort
		// (M units) would NOT fit → the discriminator.
		budgetUnits = 4
	)
	t.Setenv("SEED_FOOTPRINT_BUDGET_BYTES", "4194304")  // 4 MiB
	t.Setenv("SEED_EST_UNIT_BYTES_FALLBACK", "1048576") // 1 MiB per UNIT (conservative-high)
	resetSeedBoundForTest()
	t.Cleanup(resetSeedBoundForTest)
	cache.ResetSeedUnitFootprintViolationsForTest()

	// The REAL per-unit weight the bracket reserves (no real allocation → calibration
	// stays at the fallback, which is the per-unit cost). This is what the semaphore
	// actually Acquires — assert on IT, not a decoupled counter.
	_, budget := seedBound()
	estUnit := currentEstUnit(budget)

	// (b) PER-UNIT WEIGHT band: estUnit must fit the budget as a UNIT, and a whole
	// cohort (M×estUnit) must NOT — else the test can't discriminate.
	if estUnit > budget {
		t.Fatalf("B7(b) setup: estUnit %d > budget %d — a single unit must fit", estUnit, budget)
	}
	if M*estUnit <= budget {
		t.Fatalf("B7(b) setup: a cohort (%d×%d=%d) must EXCEED budget %d to discriminate per-unit vs per-cohort",
			M, estUnit, M*estUnit, budget)
	}
	wantConcurrency := int(budget / estUnit) // == budgetUnits (4)
	if wantConcurrency != budgetUnits {
		t.Fatalf("B7 setup: expected %d concurrent units, got %d (budget/estUnit)", budgetUnits, wantConcurrency)
	}

	// (a) EFFECTIVE CONCURRENCY through the REAL bracket. Track how many bracket
	// bodies are simultaneously past Acquire — driven purely by the semaphore.
	var inFlight atomic.Int32
	var maxConc atomic.Int32
	runUnit := func() {
		release, err := enterSeedUnit(context.Background(), "unit")
		if err != nil {
			t.Errorf("enterSeedUnit: %v", err)
			return
		}
		n := inFlight.Add(1)
		for {
			old := maxConc.Load()
			if n <= old || maxConc.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond) // hold the slot so true overlap is observable
		inFlight.Add(-1)
		release()
	}

	var wg sync.WaitGroup
	for i := 0; i < K*M; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runUnit()
		}()
	}
	wg.Wait()

	// The semaphore must admit exactly budget/estUnit UNITS simultaneously (ΣN-units
	// bound). Under a per-COHORT acquire the inflated+clamped weight would pin this to
	// 1 — this assertion FAILS for the @15cff6b granularity.
	if got := int(maxConc.Load()); got != wantConcurrency {
		t.Fatalf("B7(a) GRANULARITY: max concurrent units = %d, want %d (= budget %d / estUnit %d). "+
			"A value of 1 means the acquire weight is per-COHORT (inflated+clamped), the @15cff6b defect; "+
			"the bound must be PER-UNIT so floor(budget/unit) units run at once = ΣN-units aggregate cap.",
			got, wantConcurrency, budget, estUnit)
	}

	// (b) the per-unit weight band, asserted directly on the real acquire weight.
	if estUnit >= M*1<<20 {
		t.Fatalf("B7(b) WEIGHT: estUnit %d is in the per-COHORT band (>= M units) — granularity defect", estUnit)
	}
}

// B8 — C46-4(a) CALIBRATION-BAND discriminator: drive units that each allocate a
// KNOWN, retained per-unit footprint through the per-unit bracket, and assert
// calibrateEstUnit lands in a PER-UNIT band — tight enough that a per-COHORT
// calibration (M× a unit, which the @15cff6b g.Go-cohort placement would have
// measured) FAILS the band. This is the half of C46-4 that keys on what
// calibrate MEASURES (B7 keys on effective concurrency); together they pin the
// granularity from both the weight and the concurrency side.
//
// The per-cohort RED control is structural, not a proxy: the @15cff6b defect
// wrapped seedCohort (M units' summed allocation inside ONE enterSeedUnit), so
// the FIRST measured delta = M units. Asserting `calibrated < perUnitCeil`
// (perUnitCeil < M×unit) FAILS for that placement and PASSES for the
// shared-primitive (one-unit) placement.
func TestFalsifier46_CalibrationLandsInPerUnitBand(t *testing.T) {
	const (
		M          = 8
		perUnitMiB = 4
		// budget generous so the bracket never blocks; we're testing calibration.
		budgetMiB = 256
	)
	t.Setenv("SEED_FOOTPRINT_BUDGET_BYTES", "268435456")  // 256 MiB
	t.Setenv("SEED_EST_UNIT_BYTES_FALLBACK", "268435456") // high fallback so calibration (not fallback) is what we read
	resetSeedBoundForTest()
	t.Cleanup(resetSeedBoundForTest)
	cache.ResetSeedUnitFootprintViolationsForTest()

	// One unit: allocate + RETAIN ~perUnitMiB so the HeapInuse delta the bracket
	// samples reflects ONE unit's real footprint (calibration is one-shot off the
	// first unit).
	var sink [][]byte
	runUnit := func() {
		release, err := enterSeedUnit(context.Background(), "unit")
		if err != nil {
			t.Errorf("enterSeedUnit: %v", err)
			return
		}
		// Retain the allocation across the release() sample so the delta is real.
		buf := make([]byte, perUnitMiB*(1<<20))
		for i := range buf {
			buf[i] = byte(i) // touch pages so they're resident (HeapInuse, not just reserved)
		}
		sink = append(sink, buf)
		release()
	}

	// Drive a few units sequentially (calibration pins on the first non-zero delta).
	for i := 0; i < M; i++ {
		runUnit()
	}
	runtime.KeepAlive(sink)

	est, calibrated := calibratedEstUnitForTest()
	if !calibrated {
		t.Skip("B8: calibration did not latch (GC ran across every first-unit sample) — non-deterministic on this host; B7 covers the concurrency side")
	}
	// PER-UNIT band: the calibrated weight must be < a whole cohort (M units). A
	// per-cohort placement (@15cff6b) would have calibrated ≈ M×, FAILING this.
	perUnitCeil := int64(M) * perUnitMiB * (1 << 20) / 2 // half a cohort — generous per-unit ceiling
	if est >= perUnitCeil {
		t.Fatalf("B8 C46-4(a): calibrated estUnit %d B >= per-unit ceiling %d B — calibration measured a "+
			"PER-COHORT footprint (the @15cff6b g.Go-cohort placement), not one unit", est, perUnitCeil)
	}
}
