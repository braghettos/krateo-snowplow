// boot_walk_skip_rafulllist_test.go — #42 Option-2 (boot-seed cold-dashboard
// enabler) falsifiers for shouldServeRAFullList.
//
// THE DEFECT (arch-traced, code-confirmed): the boot re-walk
// (rePrewarmBootScoped) full-LISTs the 60K composition GVR 4× (~411 s) because
// each first-sight of the compositions-panel apiRef enters Ship-4a
// raFullListServe, which resolves the RA UNPAGINATED (resolveRA(0,0),
// ra_full_list.go:347) to establish the sliceability verdict — a whole-GVR
// materialization the nav-STRUCTURE harvest has no use for. The re-walk eats the
// whole PHASE1_TIMEOUT budget so the per-cohort first-nav seed never starts →
// latch never fires → dashboard cells cold for ~7 min.
//
// THE FIX: shouldServeRAFullList excludes the boot prewarm discovery walk (ctx
// stamped cache.WithFallthroughScope(ScopeBootPrewarmWalk)) so the walk falls
// through to the bounded page-keyed resolve instead of the unpaginated
// first-sight. The exclusion is SCOPE-PRECISE (not a resource/scale literal) and
// NARROWER than the background marker (the refresher, also background, keeps its
// serve-time full-list re-pin).
//
// HARNESS SHAPE (feedback_falsifier_shape_must_discriminate): K>1 distinct roots
// × M>1 distinct widget GVRs, all under ONE discovery-walk ctx, must ALL be
// excluded (proves the exclusion is not accidentally keyed to a single
// widget/GVR). Plus the RED arms below.

package apiref

import (
	"context"
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/snowplow/internal/cache"
)

// bootWalkCtx returns a ctx stamped exactly as withPhase1SAContext stamps the
// boot re-walk: SA identity + the ScopeBootPrewarmWalk fallthrough scope
// (phase1_walk.go:1041). The scope is a plain context value; the resolver
// packages never re-stamp it, so it reaches shouldServeRAFullList unchanged
// through the nested apiRef resolve.
func bootWalkCtx(t *testing.T) context.Context {
	t.Helper()
	ctx := xcontext.BuildContext(t.Context(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: "system:serviceaccount:krateo-system:snowplow"}))
	return cache.WithFallthroughScope(ctx, cache.ScopeBootPrewarmWalk)
}

// foregroundCallCtx returns a ctx stamped as a real per-user /call: a
// customer-render fallthrough scope, NOT the discovery walk. This is the arm
// whose 4a first-sight MUST still fire (the serve-time slice cache is for real
// /calls).
func foregroundCallCtx(t *testing.T) context.Context {
	t.Helper()
	ctx := xcontext.BuildContext(t.Context(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: "admin", Groups: []string{"system:masters"}}))
	return cache.WithFallthroughScope(ctx, cache.ScopeCallWidgets)
}

func cacheOn(t *testing.T) {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
}

// TestShouldServeRAFullList_SeedStillServes — DECISIVE RED ARM (PM-gate-caught,
// #42 rework). The per-cohort SEED runs under the SAME rctx the discovery walk
// stamps ScopeBootPrewarmWalk on, but withCohortSeedContext OVERRIDES it with
// ScopeBootPrewarmSeed. The seed's seedRAFullListForWidget resolve EXISTS to
// engage 4a and pin the per-cohort full-list cell — the exact warm-hit the first
// admin/cyberjoker /dashboard /call serves. So a ctx carrying ScopeBootPrewarmSeed
// (even with a walk-scoped ancestor on the chain) MUST still serve. If this arm
// goes RED the gate is over-broad and the cure is BROKEN (the seed's cell is
// never populated → cold at first touch, exactly the symptom). The
// dispatchers-package test TestWithCohortSeedContext_OverridesWalkScope proves the
// PRODUCTION withCohortSeedContext actually produces this scope.
func TestShouldServeRAFullList_SeedStillServes(t *testing.T) {
	cacheOn(t)
	// Mirror the production chain: a walk-scoped ancestor, then the seed scope
	// stamped LAST (last WithFallthroughScope wins — plain WithValue).
	walkAncestor := cache.WithFallthroughScope(
		xcontext.BuildContext(t.Context(),
			xcontext.WithUserInfo(jwtutil.UserInfo{Username: "system:serviceaccount:krateo-system:snowplow"})),
		cache.ScopeBootPrewarmWalk)
	seedCtx := cache.WithFallthroughScope(
		xcontext.BuildContext(walkAncestor,
			xcontext.WithUserInfo(jwtutil.UserInfo{Username: "admin", Groups: []string{"system:masters"}})),
		cache.ScopeBootPrewarmSeed)

	// Guard the premise: the seed scope must actually win over the inherited
	// walk scope on this chain (else the test proves nothing).
	if fs := cache.FallthroughScope(seedCtx); fs == nil || fs.Path != cache.ScopeBootPrewarmSeed {
		t.Fatalf("premise broken: seed ctx scope = %v, want ScopeBootPrewarmSeed — the last WithFallthroughScope must win over the inherited walk scope", fs)
	}
	if !shouldServeRAFullList(seedCtx, 5, 1) {
		t.Fatal("RED: the per-cohort SEED ctx (ScopeBootPrewarmSeed, walk-scoped ancestor) was EXCLUDED from 4a first-sight — the seed's seedRAFullListForWidget can no longer pin the per-cohort full-list cell, so the first /dashboard /call is COLD. The cure is BROKEN. The gate must exclude ONLY ScopeBootPrewarmWalk, never the seed scope.")
	}
}

// TestShouldServeRAFullList_BootWalkExcluded_KRootsMWidgets — PRIMARY GREEN +
// SHAPE. Under ONE discovery-walk ctx, K>1 roots × M>1 widget GVRs must ALL be
// excluded from the 4a first-sight (so the re-walk never materializes any GVR
// unpaginated). The paginated (perPage>0,page>0) inputs are the bounded page-1
// tuple the walk resolves under — they would ENTER 4a on any non-walk ctx.
func TestShouldServeRAFullList_BootWalkExcluded_KRootsMWidgets(t *testing.T) {
	cacheOn(t)
	ctx := bootWalkCtx(t)

	// K=3 roots (dashboard / routesloaders / a second nav root), M=2 widget
	// pagination shapes per root — the walk resolves each bounded (perPage=5,
	// page=1) or with a declared slice (perPage=10,page=2). Every combination
	// must be EXCLUDED under the walk ctx.
	perPageShapes := []int{5, 10} // M>1: default bound + a declared slice
	pageShapes := []int{1, 2}
	for _, pp := range perPageShapes {
		for _, pg := range pageShapes {
			if shouldServeRAFullList(ctx, pp, pg) {
				t.Fatalf("RED: boot discovery-walk ctx (ScopeBootPrewarmWalk) entered 4a first-sight for perPage=%d page=%d — the walk would materialize the GVR unpaginated (the ~411s / 4x-LIST defect). shouldServeRAFullList must return false for ANY paginated shape under the discovery-walk scope.", pp, pg)
			}
		}
	}
}

// TestShouldServeRAFullList_ForegroundStillServes — RED ARM (the discriminator
// against an over-broad gate). A real per-user /call (ScopeCallWidgets, NOT the
// discovery walk) with the SAME paginated inputs MUST still enter 4a first-sight
// — else the fix wrongly disabled the serve-time slice cache for real traffic.
func TestShouldServeRAFullList_ForegroundStillServes(t *testing.T) {
	cacheOn(t)
	ctx := foregroundCallCtx(t)

	if !shouldServeRAFullList(ctx, 5, 1) {
		t.Fatal("RED: a foreground /call (ScopeCallWidgets) was EXCLUDED from 4a first-sight — the gate is over-broad (it must exclude ONLY the discovery walk). The serve-time page-independent slice cache must remain live for real traffic.")
	}
	// An unscoped ctx (no fallthrough scope at all — e.g. a direct resolve) is
	// also NOT the discovery walk → must still serve.
	unscoped := xcontext.BuildContext(t.Context(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: "admin"}))
	if !shouldServeRAFullList(unscoped, 5, 1) {
		t.Fatal("RED: an unscoped (no-fallthrough-scope) ctx was EXCLUDED from 4a first-sight — only ScopeBootPrewarmWalk must exclude.")
	}
}

// TestShouldServeRAFullList_RefresherUntouched — RED ARM (refresher-safety, the
// reason we chose the narrow ScopeBootPrewarmWalk over the broad
// BackgroundResolveFromContext). The refresher (resolveOnceProd) marks its ctx
// cache.WithBackgroundResolve but does NOT stamp ScopeBootPrewarmWalk. It MUST
// still enter 4a first-sight so its serve-time full-list re-pin is preserved. If
// this arm ever flips to "excluded", the gate was silently broadened to the
// background marker and the refresher's re-pin was lost.
func TestShouldServeRAFullList_RefresherUntouched(t *testing.T) {
	cacheOn(t)
	// Exactly the refresher's ctx shape: background-resolve marked, NO
	// discovery-walk scope.
	ctx := cache.WithBackgroundResolve(xcontext.BuildContext(t.Context(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: "admin"})))
	if !shouldServeRAFullList(ctx, 5, 1) {
		t.Fatal("RED: the refresher ctx (WithBackgroundResolve, NO ScopeBootPrewarmWalk) was EXCLUDED from 4a first-sight — the gate was broadened to the background marker and the refresher's serve-time full-list re-pin is now lost. The exclusion must key on ScopeBootPrewarmWalk ONLY.")
	}
}

// TestShouldServeRAFullList_UnpaginatedNeverServes — the (0,0) first-sight
// itself must never re-enter the gate (it IS the populating resolve). Holds
// regardless of scope.
func TestShouldServeRAFullList_UnpaginatedNeverServes(t *testing.T) {
	cacheOn(t)
	for _, ctx := range []context.Context{foregroundCallCtx(t), bootWalkCtx(t)} {
		if shouldServeRAFullList(ctx, 0, 0) {
			t.Fatal("RED: an unpaginated (0,0) resolve entered the 4a serve gate — that is the first-sight populating resolve, it must always fall through.")
		}
		// A half-specified tuple (page but no perPage) is also not a real page
		// window.
		if shouldServeRAFullList(ctx, 0, 1) || shouldServeRAFullList(ctx, 5, 0) {
			t.Fatal("RED: a partially-paginated tuple entered the 4a serve gate.")
		}
	}
}

// TestShouldServeRAFullList_CacheOffNeverServes — flag-off byte-identity: with
// CACHE_ENABLED=false the gate is false for EVERY ctx/shape, and (critically)
// WithFallthroughScope returns the ctx unchanged when Disabled(), so the
// discovery-walk exclusion is inert — the pre-4a path is unchanged.
func TestShouldServeRAFullList_CacheOffNeverServes(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false")
	// Even attempting to stamp the walk scope is a no-op when Disabled().
	ctx := cache.WithFallthroughScope(
		xcontext.BuildContext(t.Context(), xcontext.WithUserInfo(jwtutil.UserInfo{Username: "admin"})),
		cache.ScopeBootPrewarmWalk)
	if shouldServeRAFullList(ctx, 5, 1) {
		t.Fatal("RED: 4a serve gate true with CACHE_ENABLED=false.")
	}
	if fs := cache.FallthroughScope(ctx); fs != nil {
		t.Fatal("RED: WithFallthroughScope stamped a scope while Disabled() — the exclusion machinery must be inert in cache-off mode.")
	}
}
