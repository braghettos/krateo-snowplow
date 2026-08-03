// seed_assert_falsifier_test.go — #46 Piece A cache-side falsifiers for
// AssertSeedUnitFootprint (the per-unit seed-footprint invariant).
package cache

import "testing"

// B2 — test mode PANICS on an oversized unit (loud-fail; an
// unbounded/unpaginated unit slipping in is a regression).
func TestSeedAssert_TestModePanicsOnBreach(t *testing.T) {
	t.Setenv("TEST_MODE", "true")
	ResetSeedUnitFootprintViolationsForTest()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("B2: AssertSeedUnitFootprint did NOT panic in test mode on an over-budget unit")
		}
	}()
	// 5000 > 1000 budget → panic in test mode.
	AssertSeedUnitFootprint("unit/oversized", 5000, 1000)
}

// Within budget in test mode must NOT panic.
func TestSeedAssert_TestModeWithinBudgetNoPanic(t *testing.T) {
	t.Setenv("TEST_MODE", "true")
	ResetSeedUnitFootprintViolationsForTest()
	if ok := AssertSeedUnitFootprint("unit/small", 500, 1000); !ok {
		t.Fatal("within-budget unit reported a violation in test mode")
	}
}

// budget==0 disables the assert even in test mode (transparent).
func TestSeedAssert_DisabledNoPanicTestMode(t *testing.T) {
	t.Setenv("TEST_MODE", "true")
	ResetSeedUnitFootprintViolationsForTest()
	if ok := AssertSeedUnitFootprint("unit/huge", 1<<40, 0); !ok {
		t.Fatal("budget==0 should disable the assert (no panic, within budget)")
	}
}
