// prewarm_family_fold_test.go — the 2026-07-03 prewarm-family implicit-on-cache
// fold falsifier (docs/prewarm-engine-implicit-on-cache-2026-07-03.md §1/§6).
// Mirrors internal/cache/resolved_test.go's TestApistageL1Enabled_FoldedUnderResolvedCache
// shape for the four prewarm gate helpers that folded to cache.PrewarmEnabled().
//
// This is the INSTALLER-TEST REGRESSION GUARD: on krateo-installer-test the
// snowplow Deployment was cache-on but had no PREWARM_ENGINE_ENABLED, so the
// engine defaulted OFF and the legacy runPIPSeed OOM-hazard path ran. Post-fold
// all four gates derive from CACHE_ENABLED, so a cache-on deployment with the
// flags forgotten correctly runs the (bounded) engine seed.
package dispatchers

import "testing"

// TestPrewarmFamily_FoldedImplicitOnCache — F-FOLD truth table for all four
// prewarm gate helpers: each is ON iff cache.PrewarmEnabled() (== CACHE_ENABLED
// truthy) and OFF when cache is off. No per-feature env opt-in.
func TestPrewarmFamily_FoldedImplicitOnCache(t *testing.T) {
	gates := []struct {
		name string
		fn   func() bool
	}{
		{"PrewarmContentEnabled", PrewarmContentEnabled},
		{"PrewarmPIPEnabled", PrewarmPIPEnabled},
		{"PrewarmEngineEnabled", PrewarmEngineEnabled},
		{"ProactiveRASeedEnabled", ProactiveRASeedEnabled},
	}

	// CACHE off → every prewarm gate off.
	t.Setenv("CACHE_ENABLED", "false")
	for _, g := range gates {
		if g.fn() {
			t.Fatalf("F-FOLD: %s() active with CACHE_ENABLED=false — must be off (the only off-switch)", g.name)
		}
	}

	// CACHE on → every prewarm gate on (implicit), no per-feature opt-in.
	t.Setenv("CACHE_ENABLED", "true")
	for _, g := range gates {
		if !g.fn() {
			t.Fatalf("F-FOLD: %s() must be ON when CACHE_ENABLED=true (implicit-on-cache) — "+
				"this is the installer-test regression: a cache-on deployment with the flag forgotten "+
				"must run the bounded engine seed, not the deleted legacy path", g.name)
		}
	}
}

// TestPrewarmFamily_RetiredEnvsNoLongerConsulted — with CACHE_ENABLED=true, the
// four retired per-feature envs have NO effect: toggling each through every
// shape leaves the gate invariantly true (proves the fold, not a stale env read).
func TestPrewarmFamily_RetiredEnvsNoLongerConsulted(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	cases := []struct {
		env string
		fn  func() bool
	}{
		{"PREWARM_CONTENT_ENABLED", PrewarmContentEnabled},
		{"PREWARM_PIP_ENABLED", PrewarmPIPEnabled},
		{"PREWARM_ENGINE_ENABLED", PrewarmEngineEnabled},
		{"PROACTIVE_RA_SEED_ENABLED", ProactiveRASeedEnabled},
	}
	for _, c := range cases {
		for _, v := range []string{"", "false", "true", "garbage"} {
			t.Setenv(c.env, v)
			if !c.fn() {
				t.Fatalf("F-FOLD: gate for retired env %s must stay ON regardless of value %q "+
					"(env is no longer consulted; the gate reads cache.PrewarmEnabled())", c.env, v)
			}
		}
	}
}
