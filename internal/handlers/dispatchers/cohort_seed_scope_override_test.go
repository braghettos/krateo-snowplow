// cohort_seed_scope_override_test.go — #42 Option-2 rework (PM-gate-caught).
//
// PROVES the PRODUCTION withCohortSeedContext OVERRIDES the inherited
// discovery-walk fallthrough scope (ScopeBootPrewarmWalk) with the seed scope
// (ScopeBootPrewarmSeed). This is the load-bearing invariant behind the #42
// Option-2 fix: apiref.shouldServeRAFullList excludes the discovery walk from
// Ship-4a's unpaginated first-sight, but the SEED must STILL engage 4a (its
// seedRAFullListForWidget resolve exists to pin the per-cohort full-list cell —
// the warm-hit the first /dashboard /call serves).
//
// The hazard the PM caught: rePrewarmBootScoped hands seedScopeYielding the SAME
// walk-scoped rctx (withPhase1SAContext stamped ScopeBootPrewarmWalk), and
// xcontext.BuildContext layers values WITHOUT stripping the inherited scope. So
// without the explicit override in withCohortSeedContext, the seed cohort ctx
// would carry ScopeBootPrewarmWalk and be WRONGLY excluded from 4a — breaking
// the cure. This test RED-verifies against d06dca2 (the pre-rework SHA where the
// override is absent): there the seed ctx carries ScopeBootPrewarmWalk.

package dispatchers

import (
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/snowplow/internal/cache"
)

// TestWithCohortSeedContext_OverridesWalkScope — the seed cohort ctx derived
// from a walk-scoped parent must carry ScopeBootPrewarmSeed, NOT the inherited
// ScopeBootPrewarmWalk. This is what lets apiref.shouldServeRAFullList serve the
// seed's 4a resolve while still excluding the discovery walk.
func TestWithCohortSeedContext_OverridesWalkScope(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true") // else WithFallthroughScope is a no-op (Disabled())

	// Exactly rePrewarmBootScoped's handoff: the walk-scoped rctx that
	// seedScopeYielding (→ withCohortSeedContext) runs under.
	walkScoped := cache.WithFallthroughScope(
		xcontext.BuildContext(t.Context()),
		cache.ScopeBootPrewarmWalk)

	// Premise guard: the parent genuinely carries the walk scope.
	if fs := cache.FallthroughScope(walkScoped); fs == nil || fs.Path != cache.ScopeBootPrewarmWalk {
		t.Fatalf("premise broken: walk-scoped parent scope = %v, want ScopeBootPrewarmWalk", fs)
	}

	seedCtx := withCohortSeedContext(walkScoped, seedTarget{Username: "admin"}, endpoints.Endpoint{}, nil)

	fs := cache.FallthroughScope(seedCtx)
	if fs == nil {
		t.Fatal("RED: seed cohort ctx carries NO fallthrough scope — expected ScopeBootPrewarmSeed")
	}
	if fs.Path == cache.ScopeBootPrewarmWalk {
		t.Fatal("RED (the PM-caught defect): the seed cohort ctx INHERITED ScopeBootPrewarmWalk from the walk-scoped parent — apiref.shouldServeRAFullList would WRONGLY exclude the seed's seedRAFullListForWidget 4a serve, so the per-cohort full-list cell is never pinned and the first /dashboard /call is COLD. withCohortSeedContext must OVERRIDE the inherited walk scope with ScopeBootPrewarmSeed.")
	}
	if fs.Path != cache.ScopeBootPrewarmSeed {
		t.Fatalf("RED: seed cohort ctx scope = %q, want ScopeBootPrewarmSeed", fs.Path)
	}
}
