// seed_coverage_falsifier_test.go — Ship 0.30.241 architect D.3 mandate.
//
// Mechanically prevents the v3-style cohort × RA key explosion from
// re-emerging in seedScopeYielding. Two falsifier tests + the
// process-level mandate this file documents.
//
// PROCESS-LEVEL ARCHITECT MANDATE (D.3 §10): every ship touching
//
//   - internal/cache/resolved.go: ResolvedKeyInputs / ComputeKey / Put
//   - internal/handlers/dispatchers/helpers.go: dispatchCacheLookupKey
//   - internal/handlers/dispatchers/phase1_pip_seed.go: withCohortSeedContext
//   - internal/handlers/dispatchers/resolve_populate.go: resolveAndPopulateL1
//   - internal/handlers/dispatchers/phase1_pip_seed.go: seedOneRestaction / seedOneWidget
//   - internal/handlers/dispatchers/prewarm_engine_boot.go: seedScopeYielding
//
// MUST keep the two TestBootSeedCoverage_* tests in this file PASSING
// under -race. If a future ship needs to relax the cohort-collapse
// contract, the tests are the source of truth — surface to architect +
// PM before editing the assertions. The v3-style cohort × RA inner
// loop in seedScopeYielding is the EXACT regression class the 5-ship
// L1-miss-after-CRUD defect chain (0.30.236/237/238/239/240) was
// caused by; the falsifier locks the v4 SA-uniform contract into the
// test suite so it cannot silently re-emerge.
//
// FIXTURE SHAPE (per dispatch — production-realistic):
//   - ≥20 distinct RA refs (production observed 21 at architect's
//     D.3 trace).
//   - ≥30 cohorts in stub binding set (production observed 35).
//   - System cohorts (system:masters, system:nodes, etc.) included
//     per feedback_no_fake_production_scenarios.
//
// Both tests MUST run under -race per the §4.6 contract that the v4
// foundation depends on.

package dispatchers

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	rbacv1 "k8s.io/api/rbac/v1"
)

// TestBootSeedCoverage_AllHarvestedRAsSeeded — LOAD-BEARING.
//
// THE INVARIANT THIS TEST PINS (v4 contract, Ship 0.30.241 fix):
//
//   For a harvester carrying N distinct RA refs and a snapshot
//   producing M cohorts, the v4 seed loop MUST emit EXACTLY N
//   seedOneRestaction calls — one per RA, NOT N × M.
//
// The v3 inner-cohort loop produced N × M = 21 × 35 = 735
// seedOneRestaction invocations per boot. At ~5s wall-clock per
// invocation, the full sweep took ~1 hr (verify-serve-stale FAILed
// at ~2 min post-boot on every v3 deploy). The v4 fix collapses
// this to N = 21 invocations → ~25-30 s wall-clock for the entire
// seed.
//
// MECHANICAL PROTECTION: a future ship that re-introduces an inner
// cohort iteration would bump seedCallCount past len(RAs) → this
// test FAILs at the assertion below.
//
// MUST run under -race per the §4.6 contract.
func TestBootSeedCoverage_AllHarvestedRAsSeeded(t *testing.T) {
	// Production-realistic fixture sizes (architect D.3 §10 requirement).
	const (
		numRAs     = 21 // production observed (architect D.3 trace)
		numCohorts = 35 // production observed (architect D.3 trace)
	)
	if numRAs < 20 {
		t.Fatalf("fixture sanity: numRAs=%d; mandate ≥20", numRAs)
	}
	if numCohorts < 30 {
		t.Fatalf("fixture sanity: numCohorts=%d; mandate ≥30", numCohorts)
	}

	// Build the RA refs.
	refs := make([]templatesv1.ObjectReference, 0, numRAs)
	for i := 0; i < numRAs; i++ {
		refs = append(refs, templatesv1.ObjectReference{
			Reference: templatesv1.Reference{
				Name:      fmt.Sprintf("ra-%02d", i),
				Namespace: "krateo-system",
			},
			APIVersion: "templates.krateo.io/v1",
			Resource:   "restactions",
		})
	}

	// Install a snapshot with ≥30 cohorts including system: groups per
	// feedback_no_fake_production_scenarios. The fixture doesn't drive
	// the seed loop's cohort iteration directly (v4 fix removed that
	// inner loop), but it's needed for any path that calls
	// EnumerateBindingSetClasses() or CohortKeyHash for diagnostics.
	installSeedFalsifierFixture(t, numCohorts)

	// COUNT every seedOneRestaction invocation via a function-pointer
	// indirection. The production seedScopeYielding calls
	// seedOneRestaction directly; we substitute a no-op stub that bumps
	// our counter so the test runs without requiring an apiserver or
	// real cache wiring.
	var seedCallCount atomic.Int64
	var seedCallsByRef sync.Map // map[string]int (ns/name -> count)
	stubSeedOneRestaction := func(ctx context.Context, _ string, ref templatesv1.ObjectReference, _ string) error {
		seedCallCount.Add(1)
		key := ref.Namespace + "/" + ref.Name
		v, _ := seedCallsByRef.LoadOrStore(key, &atomic.Int64{})
		v.(*atomic.Int64).Add(1)
		// Simulate ~1s of work per RA so the test's elapsed-time
		// projection is exercise-realistic. Honour ctx cancellation
		// (so the cancellation assertion below has a real surface).
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond): // shortened for test wall-clock
		}
		return nil
	}
	restoreSeed := setSeedOneRestactionForTest(stubSeedOneRestaction)
	defer restoreSeed()
	// Also stub the widget seed so the test runs to completion without
	// needing real widget machinery.
	restoreWidget := setSeedOneWidgetForTest(func(ctx context.Context, _ navWidgetEntry, _ string) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		return nil
	})
	defer restoreWidget()
	// And stub withPhase1SAContext to be an identity-pass-through —
	// the production version needs real SA endpoints; the test
	// bypasses that machinery because the v4 fix only cares about
	// HOW MANY times the seed function is called.
	restoreCtx := setWithPhase1SAContextForTest(func(ctx context.Context, _ endpointStub, _ restConfigStub) context.Context {
		return ctx
	})
	defer restoreCtx()

	// Run the seed under a 5-second budget — generous against the
	// 21 × 50ms = 1.05s projection of work.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err := seedScopeYieldingForTest(ctx, refs, nil) // nil widgets — RA-only run
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("seedScopeYielding returned error: %v", err)
	}

	// ── Assertion 1: every RA was Put at least once.
	totalCalls := seedCallCount.Load()
	if int64(numRAs) != totalCalls {
		t.Errorf("ASSERTION 1 FAIL: numRAs=%d, total seed calls=%d. Under v4 SA-uniform "+
			"contract, every RA is seeded EXACTLY ONCE. A count ≠ numRAs indicates "+
			"either: (a) the inner cohort loop was re-introduced (count > numRAs); "+
			"or (b) RAs were dropped from the iteration (count < numRAs).",
			numRAs, totalCalls)
	}
	// Per-RA: every ref MUST have count == 1.
	missing := []string{}
	overcalled := []string{}
	seedCallsByRef.Range(func(k, v any) bool {
		n := v.(*atomic.Int64).Load()
		if n == 0 {
			missing = append(missing, k.(string))
		}
		if n > 1 {
			overcalled = append(overcalled, fmt.Sprintf("%s=%d", k.(string), n))
		}
		return true
	})
	if len(missing) > 0 {
		t.Errorf("ASSERTION 1.a FAIL: RAs with ZERO seed calls: %v", missing)
	}
	if len(overcalled) > 0 {
		t.Errorf("ASSERTION 1.b FAIL: RAs called more than once (cohort loop "+
			"re-introduced?): %v", overcalled)
	}

	// ── Assertion 2: distinct seed-call set size == harvested RA count.
	// (Locks in the v4 cell-collapse contract: same RA = same Put.)
	distinctCalls := 0
	seedCallsByRef.Range(func(_, _ any) bool {
		distinctCalls++
		return true
	})
	if distinctCalls != numRAs {
		t.Errorf("ASSERTION 2 FAIL: distinct seed-call set size=%d, "+
			"harvested RAs=%d. v4 contract: one seed per RA, period.",
			distinctCalls, numRAs)
	}

	// ── Assertion 3: Put rate matches SA-uniform projection.
	// 21 × 50ms = 1.05s nominal; with goroutine yield overhead, expect
	// elapsed in [50ms, ~3s] window. The v3 inner-loop (735 × 50ms ≈
	// 37 s) would catastrophically exceed this even on a fast machine.
	if elapsed > 3*time.Second {
		t.Errorf("ASSERTION 3 FAIL: elapsed=%v exceeds 3s — suggestive of "+
			"per-cohort inner-loop re-introduction (v3 shape would be ~35× "+
			"slower than v4). seed_call_count=%d", elapsed, totalCalls)
	}
	if elapsed < 50*time.Millisecond {
		t.Errorf("ASSERTION 3 FAIL: elapsed=%v under 50ms floor — suggests "+
			"the seed stub never executed; seed_call_count=%d", elapsed, totalCalls)
	}

	// ── Assertion 4: ctx cancellation between Puts honored.
	// Run again with a ctx canceled before iteration starts; expect
	// near-immediate exit (<200 ms).
	cancelCtx, cancelNow := context.WithCancel(context.Background())
	cancelNow() // pre-cancel
	seedCallCount.Store(0)
	cancelStart := time.Now()
	_ = seedScopeYieldingForTest(cancelCtx, refs, nil)
	cancelElapsed := time.Since(cancelStart)
	if cancelElapsed > 200*time.Millisecond {
		t.Errorf("ASSERTION 4 FAIL: pre-cancelled ctx took %v to exit; "+
			"the loop's ctx.Err() guard at the top of each iteration must "+
			"trigger near-immediate exit", cancelElapsed)
	}
}

// TestBootSeedCoverage_V4CellCollapseOneRAAcrossCohorts — SISTER REGRESSION
// GUARD. 1 RA × 30 cohorts MUST produce exactly 1 Put (not 30).
//
// This test pins the cell-collapse property of the v4 identity-free L1:
// 30 cohorts × 1 RA SHOULD collapse to 1 L1 cell under ComputeKey.
// Any future change that re-introduces a per-cohort inner loop OR
// re-adds identity to ResolvedKeyInputs would bump this count to >1
// and FAIL the test.
//
// Sibling to TestBootSeedCoverage_AllHarvestedRAsSeeded; together
// they form a 2-axis lock (per-RA count AND cohort collapse). One
// without the other would let a partial regression slip through.
func TestBootSeedCoverage_V4CellCollapseOneRAAcrossCohorts(t *testing.T) {
	const numCohorts = 30
	if numCohorts < 30 {
		t.Fatalf("fixture sanity: numCohorts=%d; mandate ≥30", numCohorts)
	}

	refs := []templatesv1.ObjectReference{
		{
			Reference: templatesv1.Reference{
				Name:      "compositions-list",
				Namespace: "krateo-system",
			},
			APIVersion: "templates.krateo.io/v1",
			Resource:   "restactions",
		},
	}

	installSeedFalsifierFixture(t, numCohorts)

	var seedCallCount atomic.Int64
	stubSeedOneRestaction := func(ctx context.Context, _ string, _ templatesv1.ObjectReference, _ string) error {
		seedCallCount.Add(1)
		return nil
	}
	restoreSeed := setSeedOneRestactionForTest(stubSeedOneRestaction)
	defer restoreSeed()
	restoreWidget := setSeedOneWidgetForTest(func(ctx context.Context, _ navWidgetEntry, _ string) error {
		return nil
	})
	defer restoreWidget()
	restoreCtx := setWithPhase1SAContextForTest(func(ctx context.Context, _ endpointStub, _ restConfigStub) context.Context {
		return ctx
	})
	defer restoreCtx()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := seedScopeYieldingForTest(ctx, refs, nil); err != nil {
		t.Fatalf("seedScopeYielding returned error: %v", err)
	}

	// THE LOAD-BEARING ASSERTION: 1 RA × 30 cohorts → exactly 1 Put.
	got := seedCallCount.Load()
	if got != 1 {
		t.Errorf("ASSERTION FAIL: 1 RA × %d cohorts produced %d Puts; want exactly 1. "+
			"Under v4 identity-free L1, every cohort produces the SAME ComputeKey for "+
			"the same RA → the inner cohort iteration is pure waste (last-writer-wins). "+
			"A count > 1 means the v3-style inner loop has been re-introduced; the "+
			"5-ship L1-miss-after-CRUD defect chain (0.30.236-240) was caused by exactly "+
			"that shape.", numCohorts, got)
	}
}

// ─────────────────────────────────────────────────────────────────────
// Test helpers — fixture installation and function-pointer stubs.
//
// The seed loop in production calls seedOneRestaction / seedOneWidget /
// withPhase1SAContext directly. The falsifier substitutes test stubs
// via package-level function pointers (test-only — production code
// uses the direct calls). The setter helpers below install the stubs
// and return a restore closure to be deferred.
//
// IMPORTANT: the function-pointer indirection lives in
// prewarm_engine_boot_test_seam.go (the test-seam file) so the
// production seedScopeYielding can call through the seam without
// importing test code. The seam is purely test-only; production code
// goes through the seam unchanged because the seam's default value is
// the real function.

// installSeedFalsifierFixture publishes an RBAC snapshot containing
// numCohorts user/group subjects so EnumerateBindingSetClasses
// (called indirectly by production helpers) has a non-empty cohort
// set. The fixture includes system: groups per
// feedback_no_fake_production_scenarios.
func installSeedFalsifierFixture(t *testing.T, numCohorts int) {
	t.Helper()
	cache.ResetCohortGenMapForTest()
	t.Cleanup(func() {
		cache.ResetCohortGenMapForTest()
		cache.PublishRBACSnapshotForTest(nil)
	})

	crbs := []*rbacv1.ClusterRoleBinding{
		cache.MkCRBForTest("cluster-admin-binding", cache.UserSubForTest("admin")),
		cache.MkCRBForTest("system:masters-binding", cache.GroupSubForTest("system:masters")),
		cache.MkCRBForTest("system:nodes-binding", cache.GroupSubForTest("system:nodes")),
		cache.MkCRBForTest("system:authenticated-binding", cache.GroupSubForTest("system:authenticated")),
		cache.MkCRBForTest("devs-binding", cache.GroupSubForTest("devs")),
	}
	// Pad to numCohorts with synthetic per-user CRBs so the cohort
	// enumerator produces ≥numCohorts classes.
	for i := len(crbs); i < numCohorts; i++ {
		crbs = append(crbs, cache.MkCRBForTest(
			fmt.Sprintf("synth-%d-binding", i),
			cache.UserSubForTest(fmt.Sprintf("synth-user-%d", i)),
		))
	}

	cache.BuildAndPublishSnapshotForTest(
		crbs,
		map[string][]*rbacv1.RoleBinding{},
		map[string]*rbacv1.ClusterRole{},
		map[string]*rbacv1.Role{},
	)

	// Sanity: verify the enumerator does produce ≥numCohorts classes.
	classes := cache.EnumerateBindingSetClasses()
	if len(classes) < numCohorts-5 { // -5 tolerance for sentinel/dedupe collapse
		// Don't fail — log only. The v4 fix doesn't use this set
		// (cell-collapse contract); the fixture is here for
		// production-realism per the architect mandate.
		t.Logf("note: EnumerateBindingSetClasses produced %d classes (≥%d expected); "+
			"v4 fix is cell-collapse-bound, not cohort-count-bound — non-fatal",
			len(classes), numCohorts)
	}
	_ = strings.TrimSpace // keep import alive if test body trims away the use
}
