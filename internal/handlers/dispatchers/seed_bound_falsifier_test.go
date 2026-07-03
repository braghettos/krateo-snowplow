// seed_bound_falsifier_test.go — the ADAPTIVE seed-unit bound falsifiers
// (fold 2026-07-03, docs/prewarm-engine-implicit-on-cache-2026-07-03.md §3/§6,
// binding conditions C4). Hermetic — drives admission by INJECTED byte headroom
// via cache.SetAdmissionRuntimeSeamsForTest (NOT an ambient env, NOT the
// deleted SEED_FOOTPRINT_BUDGET_BYTES).
//
// SUPERSEDES the pre-fold fixed-semaphore arms (SEED_FOOTPRINT_BUDGET_BYTES /
// SEED_EST_UNIT_BYTES_FALLBACK env-driven). Those envs are DELETED; the bound is
// now the adaptive (GOMEMLIMIT − liveHeap) − GOMEMLIMIT/8 headroom gate, a
// SEPARATE instance from the nested bound (C1).
//
//   A1 — ADAPTIVE ADMISSION (the C4 RED arm): M>1 oversized seed units under an
//        injected TIGHT headroom that fits only ONE unit at a time → GREEN:
//        they SERIALIZE (max concurrency == 1), peak in-flight weight <= ceiling,
//        all complete. RED (gate off / per-unit weight ignored): peak = M×unit
//        >> ceiling.
//   A2 — STACKED-UNIT / C1 deadlock-safety: a seed unit whose body synchronously
//        enters a SECOND independent admission (modelling the seed→nested-CR
//        stack) COMPLETES under tight headroom (no self-deadlock), -race clean.
//   A3 — inFlightCount==0 guaranteed-progress: a LONE unit larger than the whole
//        ceiling still admits (runs alone), never parks forever.
//   A4 — TRANSPARENT when GOMEMLIMIT unlimited: no serialization, do() runs,
//        release is a no-op.
//   A5 — SERIALIZE-not-drop: every unit's do() runs under tight headroom.
//   A6 — CALIBRATION lands in a per-UNIT band (granularity discriminator).
//   A7 — the AssertSeedUnitFootprint diagnostic still fires in the release path.
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

// tightHeadroomSeams installs deterministic runtime seams so the shared
// cache.AdmissionCeiling() computes a ceiling that fits exactly `unitsThatFit`
// units of `unitBytes` and no more, with a static (liveHeap==0) denominator so
// the ceiling is stable across admissions (deterministic serialization).
// Returns (restore, actualCeiling) — tests assert peak in-flight weight against
// the ACTUAL computed ceiling, avoiding integer-division off-by-one artifacts.
func tightHeadroomSeams(t *testing.T, unitBytes int64, unitsThatFit int64) (func(), int64) {
	t.Helper()
	// Target a ceiling in the middle of the band [unitsThatFit*unit,
	// (unitsThatFit+1)*unit) so exactly unitsThatFit units fit and no more.
	target := unitsThatFit*unitBytes + unitBytes/2
	// ceiling = limit - limit/8 = limit*7/8  ⇒  limit = target*8/7.
	limit := target * 8 / 7
	restore := cache.SetAdmissionRuntimeSeamsForTest(
		func() int64 { return limit },
		func() int64 { return 0 }, // static live heap — ceiling is deterministic
	)
	ceiling, _ := cache.AdmissionCeiling()
	return restore, ceiling
}

// A1 — ADAPTIVE ADMISSION serializes M oversized units under a headroom that
// fits ONE at a time. This is the C4 RED arm: with the adaptive gate ON, max
// concurrency is 1 and peak in-flight weight <= ceiling; with the gate off it
// would be M×unit >> ceiling.
func TestSeedBoundAdaptive_A1_SerializesUnderTightHeadroom(t *testing.T) {
	const (
		unitBytes = int64(64 * 1024 * 1024) // 64 MiB per unit
		M         = 6                        // units
	)
	resetSeedBoundForTest()
	t.Cleanup(resetSeedBoundForTest)
	t.Cleanup(cache.ResetAdmissionRuntimeSeamsForTest)
	// Headroom fits exactly ONE unit at a time.
	restore, ceiling := tightHeadroomSeams(t, unitBytes, 1)
	t.Cleanup(restore)

	// Force the estUnit to the known unitBytes so admission math is deterministic:
	// pre-calibrate by injecting a measured delta via a first sacrificial unit is
	// noisy; instead rely on the code-constant fallback (256 MiB) being clamped
	// to the ceiling. With ceiling == 1*unitBytes == 64 MiB, currentEstUnit
	// clamps the 256 MiB fallback DOWN to the ceiling (64 MiB) — so each unit's
	// weight == ceiling, and exactly one fits. That IS the serialization driver.

	var inFlight atomic.Int32
	var maxConc atomic.Int32
	var peakWeight atomic.Int64
	var completed atomic.Int32

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
		// Record the real in-flight weight the gate is holding.
		w, _ := inFlightSeedWeightForTest()
		for {
			old := peakWeight.Load()
			if w <= old || peakWeight.CompareAndSwap(old, w) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond) // hold the slot so overlap would show
		inFlight.Add(-1)
		release()
		completed.Add(1)
	}

	var wg sync.WaitGroup
	for i := 0; i < M; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); runUnit() }()
	}
	wg.Wait()

	if got := maxConc.Load(); got != 1 {
		t.Fatalf("A1 GRANULARITY/RED: max concurrent seed units = %d, want 1 — the adaptive gate must "+
			"SERIALIZE units to a headroom that fits one at a time. A value of M (%d) means the gate is OFF "+
			"or the per-unit weight is ignored (peak = M×unit >> ceiling).", got, M)
	}
	if pw := peakWeight.Load(); pw > ceiling {
		t.Fatalf("A1: peak in-flight weight %d B exceeded the ceiling %d B — the aggregate bound leaked", pw, ceiling)
	}
	if c := completed.Load(); int(c) != M {
		t.Fatalf("A1: %d/%d units completed — serialize must never DROP", c, M)
	}
}

// A2 — STACKED seed→nested / C1 deadlock-safety. The real production stack is:
// the seed gate (enterSeedUnit, this package) OUTER, and — when a seed unit's
// resolve triggers a depth-0 nested-CR resolve — the SEPARATE nested gate
// (api.enterNestedResolveUnit) INNER, both on ONE goroutine in strict nest
// order. C1 requires SEPARATE instances (own mutex/cond/counters) so the stack
// cannot self-deadlock.
//
// This arm proves the load-bearing structural property that makes the stack
// deadlock-free WITHOUT reaching into the api package's unexported gate: (1) a
// seed unit's BODY runs while holding the admission but NOT the seed mutex
// (enterSeedUnit releases b.mu before returning the release closure), so the
// inner nested-gate admission (which takes the nested gate's OWN mutex + the
// stateless shared cache.AdmissionCeiling) can never contend with or wait on
// the seed mutex; and (2) the seed gate's OWN park→proceed liveness (broadcast
// on release), so even under a tight headroom a parked unit proceeds. If the
// two gates shared a lock (the C1 violation), (1) would deadlock: a unit body
// calling AdmissionCeiling / a second admission would re-enter a held mutex.
//
// Concretely: goroutine A takes a seed unit and, INSIDE its held body,
// synchronously calls cache.AdmissionCeiling() (exactly what the inner nested
// gate does at admission) — this must not deadlock (proves no seed mutex is
// held during the body). Meanwhile goroutine B must PARK on the tight headroom
// then PROCEED when A releases (proves broadcast liveness). A shared-lock C1
// violation, or a body that held the seed mutex, would hang → test times out.
func TestSeedBoundAdaptive_A2_StackedSeedNestedNoDeadlock(t *testing.T) {
	const unitBytes = int64(64 * 1024 * 1024)
	resetSeedBoundForTest()
	t.Cleanup(resetSeedBoundForTest)
	t.Cleanup(cache.ResetAdmissionRuntimeSeamsForTest)
	restore, _ := tightHeadroomSeams(t, unitBytes, 1) // fits one at a time
	t.Cleanup(restore)

	aReleased := make(chan struct{})
	bDone := make(chan struct{})

	go func() {
		release, err := enterSeedUnit(context.Background(), "A")
		if err != nil {
			t.Errorf("A enterSeedUnit: %v", err)
			close(aReleased)
			return
		}
		// INNER (nested-gate proxy): while holding the seed admission, do exactly
		// what api.enterNestedResolveUnit does at admission — sample the shared
		// ceiling. If the seed gate held its own mutex across the body, or the two
		// gates shared a lock, this would deadlock.
		for i := 0; i < 100; i++ {
			_, _ = cache.AdmissionCeiling()
			_ = cache.AdmissionLiveHeapSample()
		}
		time.Sleep(40 * time.Millisecond) // hold so B genuinely parks
		release()
		close(aReleased)
	}()

	go func() {
		defer close(bDone)
		time.Sleep(10 * time.Millisecond) // let A admit first
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		release, err := enterSeedUnit(ctx, "B")
		if err != nil {
			t.Errorf("B enterSeedUnit parked past ctx (deadlock?): %v", err)
			return
		}
		release()
	}()

	select {
	case <-bDone:
		<-aReleased
	case <-time.After(8 * time.Second):
		t.Fatal("A2 DEADLOCK: the stacked seed→nested case hung — C1 violated (separate instances + " +
			"no seed mutex held during the unit body + broadcast-on-release must let the stack proceed)")
	}
}

// A3 — inFlightCount==0 guaranteed-progress: a LONE unit whose weight exceeds
// the whole ceiling still admits (runs alone) rather than parking forever.
func TestSeedBoundAdaptive_A3_LoneOversizedAdmits(t *testing.T) {
	const unitBytes = int64(64 * 1024 * 1024)
	resetSeedBoundForTest()
	t.Cleanup(resetSeedBoundForTest)
	t.Cleanup(cache.ResetAdmissionRuntimeSeamsForTest)
	// Ceiling SMALLER than one unit (fits 0 whole units): the lone unit "won't
	// fit" but inFlightCount==0 ⇒ admits.
	restore := cache.SetAdmissionRuntimeSeamsForTest(
		func() int64 { return unitBytes / 2 * 8 / 7 }, // ceiling ~= unitBytes/2 < unitBytes
		func() int64 { return 0 },
	)
	t.Cleanup(restore)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	release, err := enterSeedUnit(ctx, "lone-oversized")
	if err != nil {
		t.Fatalf("A3: a lone oversized unit must admit (inFlightCount==0 escape), got err %v — "+
			"it parked forever instead of running alone", err)
	}
	release()
}

// A4 — TRANSPARENT when GOMEMLIMIT is unlimited (the runtime default). The gate
// no-ops: no serialization, do() runs, release is a no-op.
func TestSeedBoundAdaptive_A4_UnlimitedIsTransparent(t *testing.T) {
	resetSeedBoundForTest()
	t.Cleanup(resetSeedBoundForTest)
	t.Cleanup(cache.ResetAdmissionRuntimeSeamsForTest)
	// math.MaxInt64 limit ⇒ AdmissionCeiling returns unlimited==true.
	restore := cache.SetAdmissionRuntimeSeamsForTest(
		func() int64 { return 1<<63 - 1 },
		func() int64 { return 0 },
	)
	t.Cleanup(restore)

	ran := false
	err := boundSeedUnit(context.Background(), "unit", func() error { ran = true; return nil })
	if err != nil {
		t.Fatalf("A4: transparent pass-through returned err: %v", err)
	}
	if !ran {
		t.Fatalf("A4: transparent pass-through did NOT run do()")
	}
}

// A5 — SERIALIZE-not-drop: every unit's do() runs under a tight headroom that
// forces serialization.
func TestSeedBoundAdaptive_A5_AllUnitsComplete(t *testing.T) {
	const unitBytes = int64(64 * 1024 * 1024)
	resetSeedBoundForTest()
	t.Cleanup(resetSeedBoundForTest)
	t.Cleanup(cache.ResetAdmissionRuntimeSeamsForTest)
	restore, _ := tightHeadroomSeams(t, unitBytes, 1)
	t.Cleanup(restore)

	const n = 20
	var ran atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = boundSeedUnit(context.Background(), "unit", func() error { ran.Add(1); return nil })
		}()
	}
	wg.Wait()
	if got := ran.Load(); int(got) != n {
		t.Fatalf("A5: %d/%d units ran — a tight headroom must BLOCK (serialize) excess, never DROP", got, n)
	}
}

// A6 — CALIBRATION lands in a per-UNIT band (granularity discriminator, C4
// per-unit assertion preserved). Drive units that each allocate + retain a
// KNOWN per-unit footprint through the bracket; calibrateEstUnit must land < a
// whole cohort (M units). A per-cohort placement would calibrate ≈ M×.
func TestSeedBoundAdaptive_A6_CalibrationPerUnitBand(t *testing.T) {
	const (
		M          = 8
		perUnitMiB = 4
	)
	resetSeedBoundForTest()
	t.Cleanup(resetSeedBoundForTest)
	t.Cleanup(cache.ResetAdmissionRuntimeSeamsForTest)
	// Generous headroom so the bracket never parks; we test calibration, which
	// reads the SHARED live-heap sampler. Use a live-heap counter that grows with
	// each unit's real retained allocation so the release-time delta is real.
	var liveHeap atomic.Int64
	restore := cache.SetAdmissionRuntimeSeamsForTest(
		func() int64 { return 8 * 1024 * 1024 * 1024 }, // 8 GiB — never binds
		func() int64 { return liveHeap.Load() },
	)
	t.Cleanup(restore)

	var sink [][]byte
	runUnit := func() {
		release, err := enterSeedUnit(context.Background(), "unit")
		if err != nil {
			t.Errorf("enterSeedUnit: %v", err)
			return
		}
		buf := make([]byte, perUnitMiB*(1<<20))
		for i := range buf {
			buf[i] = byte(i)
		}
		sink = append(sink, buf)
		liveHeap.Add(int64(len(buf))) // model live-heap growth for the release-time delta
		release()
	}
	for i := 0; i < M; i++ {
		runUnit()
	}
	runtime.KeepAlive(sink)

	est, calibrated := calibratedEstUnitForTest()
	if !calibrated {
		t.Fatalf("A6: calibration did not latch — the first unit's live-heap delta must calibrate the weight")
	}
	perUnitCeil := int64(M) * perUnitMiB * (1 << 20) / 2 // half a cohort — generous per-unit ceiling
	if est >= perUnitCeil {
		t.Fatalf("A6: calibrated estUnit %d B >= per-unit ceiling %d B — calibration measured a PER-COHORT "+
			"footprint, not one unit (granularity defect)", est, perUnitCeil)
	}
	// And it must reflect ~one unit's allocation (perUnitMiB), not near-zero.
	if est < int64(perUnitMiB)*(1<<20)/2 {
		t.Fatalf("A6: calibrated estUnit %d B is implausibly small (< half a unit) — calibration under-measured", est)
	}
}

// A7 — the AssertSeedUnitFootprint diagnostic still fires from the adaptive
// release path when a unit's measured delta exceeds the ceiling it was admitted
// against (re-homed from the old semaphore release). Under a tight ceiling +
// an injected live-heap jump larger than the ceiling, the per-unit violation
// counter bumps.
func TestSeedBoundAdaptive_A7_OversizeAssertFires(t *testing.T) {
	resetSeedBoundForTest()
	t.Cleanup(resetSeedBoundForTest)
	t.Cleanup(cache.ResetAdmissionRuntimeSeamsForTest)
	cache.ResetSeedUnitFootprintViolationsForTest()

	// Ceiling ~= 8 MiB. The unit's release samples a live-heap delta of ~64 MiB
	// (injected) >> ceiling → AssertSeedUnitFootprint counts a violation.
	const ceilingBytes = int64(8 * 1024 * 1024)
	var liveHeap atomic.Int64
	restore := cache.SetAdmissionRuntimeSeamsForTest(
		func() int64 { return ceilingBytes * 8 / 7 },
		func() int64 { return liveHeap.Load() },
	)
	t.Cleanup(restore)

	release, err := enterSeedUnit(context.Background(), "unit/oversized")
	if err != nil {
		t.Fatalf("A7: enterSeedUnit (lone, inFlightCount==0 escape) must admit: %v", err)
	}
	// Jump live heap by 64 MiB so the release-time delta >> ceiling.
	liveHeap.Add(64 * 1024 * 1024)
	release()

	if v := cache.SeedUnitFootprintViolations(); v == 0 {
		t.Fatalf("A7: an oversized unit (delta >> ceiling) did not bump the AssertSeedUnitFootprint " +
			"violation counter — the diagnostic did not fire from the adaptive release path")
	}
}

// A8 — -race: concurrent seed-unit churn under a tight headroom, clean + no
// deadlock (the assertion is the clean -race run).
func TestSeedBoundAdaptive_A8_ConcurrentRace(t *testing.T) {
	const unitBytes = int64(16 * 1024 * 1024)
	resetSeedBoundForTest()
	t.Cleanup(resetSeedBoundForTest)
	t.Cleanup(cache.ResetAdmissionRuntimeSeamsForTest)
	restore, _ := tightHeadroomSeams(t, unitBytes, 2) // fits two at a time
	t.Cleanup(restore)

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = boundSeedUnit(context.Background(), "unit", func() error { return nil })
		}()
	}
	wg.Wait()
}
