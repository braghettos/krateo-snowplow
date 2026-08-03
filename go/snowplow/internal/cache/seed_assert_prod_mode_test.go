// seed_assert_prod_mode_test.go — coverage-audit M14.
//
// AssertSeedUnitFootprint (seed_assert.go) has the serve_assert-style
// TestMode/prod asymmetry. The existing seed_assert_falsifier_test.go covers the
// TEST_MODE=true PANIC path; M14 covers the PRODUCTION posture
// (env.TestMode()==false):
//
//   - over-budget → NO panic, returns false, seedUnitFootprintViolations bumps by
//     exactly one (best-effort warmth: the unit already resolved, the violation
//     is alertable, the seed proceeds).
//   - within-budget → returns true, counter unchanged.
//   - budget==0    → disabled, returns true, no panic, counter unchanged.
//
// RED PROOFS:
//   - a prod impl that PANICS instead of counting: the GREEN "no panic" arm
//     catches it (a panic would fail the test).
//   - a prod impl that NEVER bumps the counter: the GREEN "counter==1" arm
//     catches it. The shadow assertSeedUnitFootprintNeverCounts proves the
//     count arm discriminates.
package cache

import "testing"

// TestAssertSeedUnitFootprint_ProdModeOverBudgetCountsNoPanic is M14 main arm:
// in production posture an over-budget unit returns false, does NOT panic, and
// bumps the violation counter exactly once.
func TestAssertSeedUnitFootprint_ProdModeOverBudgetCountsNoPanic(t *testing.T) {
	t.Setenv("TEST_MODE", "false") // production posture — count, never panic
	ResetSeedUnitFootprintViolationsForTest()
	t.Cleanup(ResetSeedUnitFootprintViolationsForTest)

	// A panic here would crash the test (no recover) → this arm inherently
	// falsifies a "panic in prod" wrong impl.
	if ok := AssertSeedUnitFootprint("unit/oversized", 5000, 1000); ok {
		t.Fatalf("over-budget unit in prod mode must return false (breach observed), got true")
	}
	if v := SeedUnitFootprintViolations(); v != 1 {
		t.Fatalf("over-budget unit in prod mode must bump the violation counter to exactly 1, got %d", v)
	}
}

// TestAssertSeedUnitFootprint_ProdModeWithinBudget is M14 part 2: a within-budget
// unit returns true and leaves the counter at zero.
func TestAssertSeedUnitFootprint_ProdModeWithinBudget(t *testing.T) {
	t.Setenv("TEST_MODE", "false")
	ResetSeedUnitFootprintViolationsForTest()
	t.Cleanup(ResetSeedUnitFootprintViolationsForTest)

	if ok := AssertSeedUnitFootprint("unit/small", 500, 1000); !ok {
		t.Fatalf("within-budget unit must return true")
	}
	if v := SeedUnitFootprintViolations(); v != 0 {
		t.Fatalf("within-budget unit must NOT bump the violation counter, got %d", v)
	}
	// An exactly-at-budget unit is within budget (deltaBytes <= budgetBytes).
	if ok := AssertSeedUnitFootprint("unit/exact", 1000, 1000); !ok {
		t.Fatalf("delta == budget is within budget (<=), must return true")
	}
	if v := SeedUnitFootprintViolations(); v != 0 {
		t.Fatalf("at-budget unit must NOT bump the counter, got %d", v)
	}
}

// TestAssertSeedUnitFootprint_ProdModeBudgetZeroDisabled is M14 part 3: a zero
// budget disables the assertion even in prod mode (transparent-fallback for an
// unset GOMEMLIMIT), no panic, no count.
func TestAssertSeedUnitFootprint_ProdModeBudgetZeroDisabled(t *testing.T) {
	t.Setenv("TEST_MODE", "false")
	ResetSeedUnitFootprintViolationsForTest()
	t.Cleanup(ResetSeedUnitFootprintViolationsForTest)

	if ok := AssertSeedUnitFootprint("unit/huge", 1<<40, 0); !ok {
		t.Fatalf("budget==0 must disable the assert and return true even for a huge delta")
	}
	if v := SeedUnitFootprintViolations(); v != 0 {
		t.Fatalf("budget==0 (disabled) must NOT bump the counter, got %d", v)
	}
}

// --- RED-arm proof ----------------------------------------------------------

// assertSeedUnitFootprintNeverCounts is the M14 WRONG prod impl: it returns
// false on a breach (correct sign) but forgets to bump the violation counter —
// the alertable signal is silently lost.
func assertSeedUnitFootprintNeverCounts(deltaBytes, budgetBytes uint64) bool {
	if budgetBytes == 0 || deltaBytes <= budgetBytes {
		return true
	}
	// defect: no seedUnitFootprintViolations.Add(1)
	return false
}

// TestAssertSeedUnitFootprint_NeverCounts_RedArm proves the M14 counter arm
// discriminates: the never-counts wrong impl leaves the counter at zero on the
// same over-budget input the GREEN arm asserts bumps to 1.
func TestAssertSeedUnitFootprint_NeverCounts_RedArm(t *testing.T) {
	t.Setenv("TEST_MODE", "false")
	ResetSeedUnitFootprintViolationsForTest()
	t.Cleanup(ResetSeedUnitFootprintViolationsForTest)

	if ok := assertSeedUnitFootprintNeverCounts(5000, 1000); ok {
		t.Fatalf("RED-arm sanity: the wrong impl still returns false on breach")
	}
	if v := SeedUnitFootprintViolations(); v != 0 {
		t.Fatalf("RED-arm sanity: the never-counts wrong impl must leave the counter at 0; "+
			"the M14 counter arm is not discriminating otherwise (got %d)", v)
	}
}
