// gates_widget_content_test.go — coverage-audit M10.
//
// WidgetContentL1Enabled (resolved.go) is a THREE-gate predicate:
//   1. CACHE_ENABLED truthy      (Disabled() == false)
//   2. RESOLVED_CACHE_ENABLED != {"false","0","no"}
//   3. WIDGET_CONTENT_L1_ENABLED != {"false","0","no"}   (the sub-gate)
//
// The sub-gate (3) is default-ON: any value NOT in the explicit-off set
// ("", "true", "1", "yes", "garbage") enables it. This test drives the sub-gate
// truth table over the {'', 'true', '1', 'yes', 'garbage'} → true and
// {'false','0','no'} → false shapes, plus the two master-gate off cases.
//
// RED PROOF: an impl that returns ResolvedCacheEnabled() directly (ignoring the
// WIDGET_CONTENT_L1_ENABLED sub-gate) would report ENABLED even when
// WIDGET_CONTENT_L1_ENABLED=false — proven by the shadow
// widgetContentEnabledIgnoringSubgate below.
package cache

import "testing"

// TestWidgetContentL1Enabled_SubgateTruthTable is M10: with both master gates
// open, the WIDGET_CONTENT_L1_ENABLED sub-gate follows the default-ON parse.
func TestWidgetContentL1Enabled_SubgateTruthTable(t *testing.T) {
	// Master gates open for the whole sub-gate table.
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")

	cases := []struct {
		env  string
		want bool
	}{
		{"", true},        // unset-ish default → ON
		{"true", true},    // any non-off value → ON
		{"1", true},       // default-ON parse does NOT special-case "1"
		{"yes", true},     // ditto
		{"garbage", true}, // unrecognised → ON (fail-open sub-gate, matches ResolvedCacheEnabled)
		{"false", false},  // explicit off
		{"0", false},      // explicit off
		{"no", false},     // explicit off
	}
	for _, tc := range cases {
		t.Run("WIDGET_CONTENT_L1_ENABLED="+tc.env, func(t *testing.T) {
			t.Setenv("WIDGET_CONTENT_L1_ENABLED", tc.env)
			if got := WidgetContentL1Enabled(); got != tc.want {
				t.Fatalf("WidgetContentL1Enabled() with WIDGET_CONTENT_L1_ENABLED=%q = %v, want %v",
					tc.env, got, tc.want)
			}
		})
	}
}

// TestWidgetContentL1Enabled_MasterGatesGoverning is M10 part 2: either master
// gate off forces the whole predicate false, REGARDLESS of the sub-gate value.
func TestWidgetContentL1Enabled_MasterGatesGoverning(t *testing.T) {
	// The sub-gate is set to its most-permissive value throughout so any
	// "true" result could ONLY come from the sub-gate being consulted alone.
	t.Setenv("WIDGET_CONTENT_L1_ENABLED", "true")

	// CACHE_ENABLED off → false.
	t.Setenv("CACHE_ENABLED", "false")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	if WidgetContentL1Enabled() {
		t.Fatalf("CACHE_ENABLED=false must force WidgetContentL1Enabled() false")
	}

	// CACHE_ENABLED on but RESOLVED_CACHE_ENABLED off → false.
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "false")
	if WidgetContentL1Enabled() {
		t.Fatalf("RESOLVED_CACHE_ENABLED=false must force WidgetContentL1Enabled() false")
	}

	// Both master gates open + sub-gate on → true (control).
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	if !WidgetContentL1Enabled() {
		t.Fatalf("both master gates + sub-gate open must be true (control)")
	}
}

// --- RED-arm proof ----------------------------------------------------------

// widgetContentEnabledIgnoringSubgate is the M10 WRONG impl: it returns the
// resolved-cache gate directly, ignoring the WIDGET_CONTENT_L1_ENABLED sub-gate.
func widgetContentEnabledIgnoringSubgate() bool {
	return ResolvedCacheEnabled()
}

// TestWidgetContentL1Enabled_IgnoreSubgate_RedArm proves the M10 sub-gate arm
// discriminates: with both master gates open but WIDGET_CONTENT_L1_ENABLED=false
// the CORRECT predicate is false while the sub-gate-ignoring shadow is true.
func TestWidgetContentL1Enabled_IgnoreSubgate_RedArm(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	t.Setenv("WIDGET_CONTENT_L1_ENABLED", "false")

	if !widgetContentEnabledIgnoringSubgate() {
		t.Fatalf("RED-arm sanity: the sub-gate-ignoring wrong impl SHOULD report enabled " +
			"(it only reads the resolved-cache gate)")
	}
	if WidgetContentL1Enabled() {
		t.Fatalf("the CORRECT WidgetContentL1Enabled() must be FALSE when the sub-gate is off; " +
			"if it matched the wrong impl the M10 sub-gate arm would not discriminate")
	}
}
