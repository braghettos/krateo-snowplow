diff --git a/internal/handlers/dispatchers/phase1_walk.go b/internal/handlers/dispatchers/phase1_walk.go
index 7257f9c..09fd596 100644
--- a/internal/handlers/dispatchers/phase1_walk.go
+++ b/internal/handlers/dispatchers/phase1_walk.go
@@ -849,14 +849,16 @@ func withPhase1SAContext(ctx context.Context, saEP endpoints.Endpoint, saRC *res
 func resolveNavigationRoot(ctx context.Context, root *unstructured.Unstructured, gvr schema.GroupVersionResource, saEP endpoints.Endpoint, saRC *rest.Config, authnNS string, harvester *contentPrewarmHarvester, navHarvester *navWidgetHarvester, pagCollector *apiRefPaginationCollector) error {
 	rctx := withPhase1SAContext(ctx, saEP, saRC)
 
-	w := &phase1Walker{
-		authnNS:            authnNS,
-		rc:                 saRC,
-		visited:            map[string]struct{}{},
-		apiRefHarvester:    harvester,
-		navWidgetHarvester: navHarvester,
-		pagCollector:       pagCollector,
-	}
+	// Ship 0.30.232: type-safe construction via newPhase1Walker. rc is now
+	// a REQUIRED positional parameter — the compiler enforces every
+	// construction site supplies it, eliminating the recurring nil-rc
+	// surface that produced six HARD REVERTs (0.30.226 → 0.30.231). See
+	// phase1_walker_new.go for the constructor contract.
+	w := newPhase1Walker(saRC, authnNS,
+		withApiRefHarvester(harvester),
+		withNavWidgetHarvester(navHarvester),
+		withPagCollector(pagCollector),
+	)
 	// Ship G (0.30.16x): gvr is threaded from the lister
 	// (listNavigationRootsFromConfigMap, phase1_roots.go) which parses
 	// it from the templatesv1.ObjectReference each /call URL decoded
diff --git a/internal/handlers/dispatchers/phase1_walk_bounds_test.go b/internal/handlers/dispatchers/phase1_walk_bounds_test.go
index 5ba1199..38ebbdf 100644
--- a/internal/handlers/dispatchers/phase1_walk_bounds_test.go
+++ b/internal/handlers/dispatchers/phase1_walk_bounds_test.go
@@ -40,10 +40,11 @@ import (
 // loosens the cap would let walk fall through to widgets.Resolve at
 // unbounded depth and recurse without limit — and fail here.
 func TestPhase1Walk_MaxDepthTruncation(t *testing.T) {
-	w := &phase1Walker{
-		authnNS: "krateo-system",
-		visited: map[string]struct{}{},
-	}
+	// Ship 0.30.232: type-safe construction via newPhase1Walker. The
+	// depth-cap gate fires before any rc consumer runs, so nil rc is
+	// safe here (the constructor accepts nil; runtime safety lives at
+	// the consumer sites). See phase1_walker_new.go for the contract.
+	w := newPhase1Walker(nil, "krateo-system")
 	widget := &unstructured.Unstructured{Object: map[string]any{
 		"apiVersion": "widgets.templates.krateo.io/v1beta1",
 		"kind":       "Widget",
diff --git a/internal/handlers/dispatchers/phase1_walk_pagination_jobs.go b/internal/handlers/dispatchers/phase1_walk_pagination_jobs.go
index bccfe23..09faeb0 100644
--- a/internal/handlers/dispatchers/phase1_walk_pagination_jobs.go
+++ b/internal/handlers/dispatchers/phase1_walk_pagination_jobs.go
@@ -304,15 +304,13 @@ func drainApiRefPaginationJobs(
 			// drain runs AFTER MarkPhase1Done so the content-pass / PIP-
 			// seed harvesters have already been drained; populating them
 			// now would have no consumer.
-			&phase1Walker{
-				authnNS: j.AuthnNS,
-				// Ship 0.30.230 fix-at-root: the drain shell walker MUST
-				// carry the SA *rest.Config so the page-N
-				// widgets.Resolve at phase1_walk_pagination.go's literal
-				// receives non-nil rc downstream of opts.RC.
-				rc:      saRC,
-				visited: map[string]struct{}{},
-			},
+			// Ship 0.30.232: type-safe construction via newPhase1Walker.
+			// Ship 0.30.230 fix-at-root preserved — the drain shell walker
+			// MUST carry the SA *rest.Config so the page-N
+			// widgets.Resolve at phase1_walk_pagination.go's literal
+			// receives non-nil rc downstream of opts.RC. No harvesters
+			// (drain post-MarkPhase1Done; consumers already drained).
+			newPhase1Walker(saRC, j.AuthnNS),
 			j.In,
 			j.GVR,
 			j.Page1Res,
diff --git a/internal/handlers/dispatchers/prewarm_engine_boot.go b/internal/handlers/dispatchers/prewarm_engine_boot.go
index be40a79..ea3dd03 100644
--- a/internal/handlers/dispatchers/prewarm_engine_boot.go
+++ b/internal/handlers/dispatchers/prewarm_engine_boot.go
@@ -121,12 +121,18 @@ func rePrewarmBoot(ctx context.Context, deps rePrewarmDeps) error {
 		// FRESH walker per root — new visited map (phase1_walk.go:679).
 		// Harvesters SHARED BY REFERENCE so the re-walk's harvest lands in
 		// the set the seed drains.
-		w := &phase1Walker{
-			authnNS:            deps.authnNS,
-			visited:            map[string]struct{}{},
-			apiRefHarvester:    deps.harvester,
-			navWidgetHarvester: deps.navHarv,
-		}
+		//
+		// Ship 0.30.232 — THE BUG SITE: prior six HARD REVERTs (0.30.226
+		// → 0.30.231) all traced back to THIS literal omitting rc. Now
+		// constructed via newPhase1Walker, which makes rc a REQUIRED
+		// positional parameter — the compiler refuses a missing
+		// argument. deps.saRC is the same SA *rest.Config the seed pass
+		// uses (set once in Phase1Warmup; non-nil per the rePrewarmDeps
+		// contract).
+		w := newPhase1Walker(deps.saRC, deps.authnNS,
+			withApiRefHarvester(deps.harvester),
+			withNavWidgetHarvester(deps.navHarv),
+		)
 		// Same walk() entrypoint + same root tuple as resolveNavigationRoot
 		// (page 1, perPage prewarmPageLimit(), key tuple (-1,-1)) — the
 		// Change A page-number fix is preserved.
// phase1_walker_new.go — Ship 0.30.232: type-safe phase1Walker constructor.
//
// Background — six HARD REVERTs (0.30.226 → 0.30.231) traced to one
// recurring blind spot: a phase1Walker struct literal that OMITS the rc
// field. Downstream, w.rc is passed into widgets.ResolveOptions.RC and
// consumed by crdschema.ValidateObjectStatus → cache.GVRFor →
// discoverPluralInfo, which fail-closes on nil *rest.Config ("plurals
// discovery: nil *rest.Config"). Site #1 (resolveNavigationRoot) and
// site #2 (drainApiRefPaginationJobs) were fixed in 0.30.230 by adding
// `rc: saRC` to their literals; site #3 (rePrewarmBoot, this file's bug
// site) was MISSED because the same blind-spot pattern recurred.
//
// The architect's Option C fix (ship-0.30.232-attempt7-design.md): make
// rc REQUIRED at the type level via a constructor function. The compiler
// then enforces every construction site supplies rc — no missing-field
// surface possible. The four remaining struct literals (3 production +
// 1 test) are replaced with newPhase1Walker calls in the same ship.
//
// Functional options carry the OPTIONAL fields (harvesters,
// pagCollector). Each call site sets only what it needs:
//   - resolveNavigationRoot: all three harvesters + pagCollector.
//   - drainApiRefPaginationJobs: none (drain runs post-MarkPhase1Done, so
//     harvester writes have no consumer — see the comment at
//     phase1_walk_pagination_jobs.go:296-306).
//   - rePrewarmBoot (THIS SHIP's bug site): apiRefHarvester +
//     navWidgetHarvester only (no pagCollector — boot re-walk is not on
//     the deferred-pagination path).
//   - TestPhase1Walk_MaxDepthTruncation: none (the test's depth-cap gate
//     fires before any rc consumer runs, so nil rc is safe here).
//
// Per [feedback_check_k8s_clientgo_prior_art]: explicit thread-through
// is the right pattern for a long-lived server with per-user credential
// isolation. A silent in-helper fallback (e.g. rest.InClusterConfig()
// at depth) would mask a future per-user code path forgetting rc and
// silently swap in SA creds — a data-exfil risk. Compile-time
// enforcement extends the discipline rather than reverting it.
//
// FOLLOW-UP (non-blocking per peer-review nit 1): the compiler enforces
// rc is SUPPLIED at every construction site, but a future contributor
// can still write a direct &phase1Walker{rc: nil, ...} literal. Defense
// lives at consumer-side `if rc == nil` guards (discoverPluralInfo,
// discoverKindForGVR, discoveryClientBuilder — all return ERROR, not
// panic). A doc-comment marker on the phase1Walker struct discouraging
// direct construction is tracked as a follow-up; out of scope for this
// ship.

package dispatchers

import "k8s.io/client-go/rest"

// phase1WalkerOpt configures optional phase1Walker fields. Required
// fields (rc, authnNS) are positional in newPhase1Walker; optional ones
// (apiRefHarvester, navWidgetHarvester, pagCollector) flow through opts.
type phase1WalkerOpt func(*phase1Walker)

// withApiRefHarvester wires the apiRef content-prewarm harvester. nil-
// safe at consumption sites; pass when PREWARM_CONTENT_ENABLED is on
// (resolveNavigationRoot, rePrewarmBoot).
func withApiRefHarvester(h *contentPrewarmHarvester) phase1WalkerOpt {
	return func(w *phase1Walker) { w.apiRefHarvester = h }
}

// withNavWidgetHarvester wires the navigation-widget harvester drained
// by the PIP seed. nil-safe at consumption sites; pass when PIP is on
// (resolveNavigationRoot, rePrewarmBoot).
func withNavWidgetHarvester(h *navWidgetHarvester) phase1WalkerOpt {
	return func(w *phase1Walker) { w.navWidgetHarvester = h }
}

// withPagCollector wires the deferred apiRef-pagination job collector
// (Path 3.2.2.b / 0.30.221). nil-safe at consumption sites; pass at the
// resolveNavigationRoot site only — the drain shell and the re-walk do
// not enroll new pagination jobs.
func withPagCollector(c *apiRefPaginationCollector) phase1WalkerOpt {
	return func(w *phase1Walker) { w.pagCollector = c }
}

// newPhase1Walker is the SOLE constructor for phase1Walker. rc and
// authnNS are required positional parameters — the compiler refuses a
// missing argument, eliminating the recurring nil-rc surface that
// produced six HARD REVERTs (0.30.226 → 0.30.231).
//
// rc must be the SA *rest.Config the walker passes into
// widgets.ResolveOptions.RC. authnNS is the authentication namespace
// the walker carries through nested per-user resolves.
//
// Callers that do NOT consume rc downstream (e.g. the depth-cap unit
// test, whose phase1MaxWalkDepth gate returns before widgets.Resolve
// fires) MAY pass nil rc explicitly — the constructor accepts nil; the
// runtime safety lives at the consumer sites (discoverPluralInfo,
// discoverKindForGVR, discoveryClientBuilder each guard `if rc == nil`).
// The constructor's job is type-level enforcement that rc is SUPPLIED
// at every construction site, not value-level validation.
func newPhase1Walker(rc *rest.Config, authnNS string, opts ...phase1WalkerOpt) *phase1Walker {
	w := &phase1Walker{
		authnNS: authnNS,
		rc:      rc,
		visited: map[string]struct{}{},
	}
	for _, o := range opts {
		o(w)
	}
	return w
}
