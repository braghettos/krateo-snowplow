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
