//go:build unit
// +build unit

// external_seed_bound_marker_liveness_test.go — MARKER-LIVENESS falsifier for
// the boot-seed external-fetch wall-clock bound
// (docs/boot-seed-external-fetch-bound-design-2026-07-24.md).
//
// PROVES the bound fires under ALL THREE readiness-critical vectors — the
// per-cohort SEED, the discovery WALK, and the content-prewarm PASS — by
// driving the REAL PRODUCTION context builders (withCohortSeedContext,
// withPhase1SAContext, withContentPrewarmSAContext) and handing each produced
// ctx to the REAL gating predicate api.ExternalSeedFetchBounded (which is the
// single source of truth for resolve.go:1106's wrap decision).
//
// This is NOT a hand-stamped ctx: each arm re-invokes the production builder
// end-to-end and asserts the boundary is bounded through the SAME predicate
// production uses (feedback_falsifier_must_drive_real_boundary_not_install_crossed_state).
// The CONTENT-PASS ARM is the load-bearing one — it is the exact vector the PM
// caught (withContentPrewarmSAContext carried no scope before this fix, so the
// bound would no-op on it and leave it unbounded to overrun 480s). A test green
// on seed+walk but blind to the content pass would be INADMISSIBLE.

package dispatchers

import (
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/resolvers/restactions/api"
)

// TestMarkerLiveness_AllThreeReadinessVectorsBounded — the three real prod
// context builders each produce a ctx that the external-fetch bound wraps at
// resolve.go:1106. Default (5000ms) bound is ON.
func TestMarkerLiveness_AllThreeReadinessVectorsBounded(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true") // else WithFallthroughScope is a no-op (Disabled())
	// Leave RESOLVER_SEED_EXTERNAL_FETCH_TIMEOUT_MS unset → default 5000 → bound ON.

	// The base ctx production hands the builders: the discovery-walk-scoped
	// parent that rePrewarmBootScoped threads into the seed, exactly as in prod
	// (withPhase1SAContext stamps it, then the seed builder runs under it).
	base := xcontext.BuildContext(t.Context())

	t.Run("seed_ctx_real_withCohortSeedContext", func(t *testing.T) {
		// The seed runs under the walk-scoped parent (rePrewarmBootScoped handoff).
		walkScoped := cache.WithFallthroughScope(base, cache.ScopeBootPrewarmWalk)
		seedCtx := withCohortSeedContext(walkScoped, seedTarget{Username: "admin"}, endpoints.Endpoint{}, nil)

		if fs := cache.FallthroughScope(seedCtx); fs == nil || fs.Path != cache.ScopeBootPrewarmSeed {
			t.Fatalf("premise: seed ctx scope = %v, want ScopeBootPrewarmSeed", fs)
		}
		if !api.ExternalSeedFetchBounded(seedCtx) {
			t.Fatal("RED: the REAL seed ctx (withCohortSeedContext) is NOT bounded at resolve.go:1106 — a degraded external widget in the seed loop stays unbounded (up to ~120s/unit) and overruns the 480s readiness budget.")
		}
	})

	t.Run("walk_ctx_real_withPhase1SAContext", func(t *testing.T) {
		walkCtx := withPhase1SAContext(base, endpoints.Endpoint{}, nil)

		if fs := cache.FallthroughScope(walkCtx); fs == nil || fs.Path != cache.ScopeBootPrewarmWalk {
			t.Fatalf("premise: walk ctx scope = %v, want ScopeBootPrewarmWalk", fs)
		}
		if !api.ExternalSeedFetchBounded(walkCtx) {
			t.Fatal("RED: the REAL discovery-walk ctx (withPhase1SAContext) is NOT bounded — the pre-latch SA discovery walk resolves external widgets before MarkPhase1Done and would pay an unbounded round-trip per external widget.")
		}
	})

	t.Run("content_pass_ctx_real_withContentPrewarmSAContext_THE_PM_CAUGHT_VECTOR", func(t *testing.T) {
		contentCtx := withContentPrewarmSAContext(base, endpoints.Endpoint{}, nil)

		fs := cache.FallthroughScope(contentCtx)
		if fs == nil {
			t.Fatal("RED (the PM-caught defect): the content-prewarm pass ctx (withContentPrewarmSAContext) carries NO fallthrough scope — the external-fetch bound no-ops on it, so the content pass (which resolves the obs-* ClickHouse apiRef data sources BEFORE MarkPhase1Done) stays UNBOUNDED and can overrun the 480s backstop alone. withContentPrewarmSAContext must stamp a boot-readiness scope.")
		}
		if !api.ExternalSeedFetchBounded(contentCtx) {
			t.Fatalf("RED (the PM-caught defect): the content-prewarm pass ctx carries scope %q but the external-fetch bound does NOT wrap it — that scope is not in the readiness-critical gated set. The content-pass external fetch stays unbounded.", fs.Path)
		}
	})
}
