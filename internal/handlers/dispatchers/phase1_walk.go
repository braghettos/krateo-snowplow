// phase1_walk.go — 0.30.102 Tag B Part 1 (recursive walker added in
// 0.30.105): the startup SA-credentialed resolution walk that warms the
// navigation-reached informers.
//
// This file lives in the dispatchers package because the widget /
// RESTAction resolution machinery (widgets.Resolve, the api resolver's
// lazyRegisterInnerCallPaths hook) is reachable from here without an
// import cycle. The cache package owns the Phase1Done state, the
// meta-query seed budget, and the CRD-watch (Part 2); main.go owns the
// startup wiring.
//
// THE WALK (Part 1, recursive as of 0.30.105; ConfigMap-derived roots as
// of 0.30.107):
//   1. READ the navigation roots from the frontend ConfigMap (NOT a
//      hardcoded GVR LIST). The frontend ConfigMap's `config.json`
//      declares the two `/call` entry points the frontend itself
//      dispatches on login — `.api.INIT` and `.api.ROUTES_LOADER`. Phase
//      1 parses each `/call?resource=...&apiVersion=...&name=...&
//      namespace=...` URL into an ObjectReference and fetches those EXACT
//      two widget CRs as the navigation roots. The resource names
//      (`navmenus`, `routesloaders`) appear NOWHERE as Go literals — they
//      arrive at runtime from config.json. If the frontend changes its
//      INIT widget, Phase 1 follows automatically. See phase1_roots.go.
//   2. Recursively resolve the navigation widget tree under the snowplow
//      service-account identity through the STANDARD widget resolver.
//      Each resolved widget returns `status.resourcesRefs.items[]`; every
//      item whose `verb == "GET"` is itself a `/call?...` widget endpoint
//      — the walker fetches that child widget CR and recurses into it.
//      Recursion proceeds Root -> Route -> Page -> Row/Column ->
//      DataGrid/Table leaf. A visited-set keyed on the child widget
//      endpoint dedupes shared subtrees and prevents cycles.
//
//      WHY verb == "GET" ONLY (load-bearing — correctness AND safety):
//      a non-GET resourcesRefs item is a mutation/action endpoint
//      (POST/PUT/PATCH/DELETE) bound to a widget's `actions`, never part
//      of the navigation/render tree. Following one would (a) walk an
//      edge that is not navigation and (b) — the SA walk runs with
//      privileged service-account credentials — issue a DESTRUCTIVE
//      apiserver mutation. The walk MUST stay strictly read-only.
//
//      WHY the `allowed` flag is NOT a recursion gate: Phase 1 walks the
//      FULL GET-navigation structure for informer DISCOVERY — informer
//      registration is identity-independent (the composition informer the
//      Compositions Page needs is the same object set no matter which user
//      can see it). The per-user `allowed` RENDER gate (which widgets to
//      show a logged-in user) belongs at real request time, not startup
//      warmup. Note: Phase 1 resolves under the snowplow service account's
//      CANONICAL username (system:serviceaccount:<ns>:<name>, derived from
//      the SA token's JWT `sub` claim — 0.30.108), and that SA holds a
//      native ClusterRoleBinding granting `*/*` get/list/watch, so
//      EvaluateRBAC correctly AUTHORIZES it; `allowed` is therefore true
//      for the navigation widgets anyway. It is still not used as the
//      recursion gate because discovery must not depend on render
//      authorization at all. See walkShouldRecurse for the full rationale.
//   3. As the RESTAction resolver processes inner-call paths (fired when
//      the recursion reaches an apiRef-bearing leaf such as the
//      Compositions Page DataGrid), the flag-independent
//      lazyRegisterInnerCallPaths hook auto-registers an informer for
//      every GVR the inner-call path touches AND feeds the CRD-watch's
//      auto-discover group set (e.g. composition.krateo.io). Informer
//      registration is a free side-effect — no separate GVR-collection
//      step.
//   4. The resolution OUTPUT is DISCARDED — Phase 1 is discovery-only.
//      It does NOT populate L1 (that is Phase 2, deferred). The resolver
//      mutates the in-memory CR copy but never persists status back to
//      the apiserver.
//
// CRITICAL — feedback_no_special_cases.md: Phase 1 seeds ONLY from the
// two resolved navigation roots. There is NO configured widget-GVR list
// and NO configured RESTAction list. RESTActions + downstream GVRs are
// discovered purely by recursively resolving the navigation roots — an
// orphan RESTAction wired to no navigation page is never reached and
// never registers an informer. The two roots themselves are NOT Go
// literals either: they are read from the frontend ConfigMap's
// config.json (.api.INIT / .api.ROUTES_LOADER) — the navigation contract.
// The ConfigMap pointer is config too (FRONTEND_CONFIG_CONFIGMAP env var
// + AUTHN_NAMESPACE). See phase1_roots.go.
//
// BEHAVIOR-NEUTRAL — the whole walk runs only when cache.PrewarmEnabled()
// (PREWARM_ENABLED=true). main.go does not call Phase1Warmup otherwise.

package dispatchers

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/plumbing/maps"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	idynamic "github.com/krateoplatformops/snowplow/internal/dynamic"
	"github.com/krateoplatformops/snowplow/internal/handlers/util"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sdynamic "k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// phase1SAUsername resolves the CANONICAL Kubernetes ServiceAccount
// username for the pod Phase 1 runs as — the form
// `system:serviceaccount:<namespace>:<name>`.
//
// WHY canonical (0.30.108 — the bug 0.30.105–107 misdiagnosed): the
// resolution-context identity flows into snowplow's RBAC evaluator
// (rbac.EvaluateRBAC). Its subject matcher (anySubjectMatches →
// parseServiceAccountUsername) can only match a ServiceAccount-kind RBAC
// subject when the username carries the `system:serviceaccount:` prefix.
// The snowplow SA genuinely holds a native ClusterRoleBinding granting
// `*/*` get/list/watch — but that binding's subject is ServiceAccount-kind
// (name + namespace). A bare label like "snowplow-serviceaccount" has no
// prefix, so parseServiceAccountUsername returns isSA=false, the
// ServiceAccount-kind subject can never fire, and EvaluateRBAC DENIES a
// fully-authorized SA. The fix is to pass the canonical form so the
// evaluator can connect Phase 1's identity to the SA's real binding.
//
// DERIVATION (no hardcoded ns/name literals — feedback_no_special_cases.md):
// the in-cluster projected SA token is a JWT whose `sub` claim is EXACTLY
// `system:serviceaccount:<ns>:<name>`. Phase 1 already loads that token
// (idynamic.ServiceAccountEndpoint puts the raw JWT in saEP.Token), so we
// decode `sub` from it via jwtutil.ExtractUserInfo — the canonical
// username arrives verbatim from the runtime identity, never a Go literal.
// The pod's namespace and SA name are NOT named in code; they are whatever
// the apiserver minted into the token snowplow runs with.
//
// Returns ("", false) when the token is absent or its `sub` is not a
// canonical ServiceAccount username; the caller logs and proceeds (Phase 1
// is best-effort warmup — see resolveNavigationRoot).
func phase1SAUsername(saToken string) (string, bool) {
	if saToken == "" {
		return "", false
	}
	ui, err := jwtutil.ExtractUserInfo(saToken)
	if err != nil {
		return "", false
	}
	if _, _, isSA := splitCanonicalSAUsername(ui.Username); !isSA {
		return "", false
	}
	return ui.Username, true
}

// splitCanonicalSAUsername reports whether u is a canonical
// `system:serviceaccount:<ns>:<name>` username and, if so, returns its ns
// and name. It mirrors rbac.parseServiceAccountUsername so phase1SAUsername
// can VERIFY the JWT-decoded subject is the canonical form the RBAC
// evaluator will actually match — keeping the two in lockstep without an
// import cycle.
func splitCanonicalSAUsername(u string) (string, string, bool) {
	const prefix = "system:serviceaccount:"
	if !strings.HasPrefix(u, prefix) {
		return "", "", false
	}
	rest := u[len(prefix):]
	i := strings.Index(rest, ":")
	if i <= 0 || i == len(rest)-1 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

// navigationRoot is a navigation-root widget plus the GVR the lister
// parsed it under — Ship G (0.30.16x). The dispatcher path composes
// widget keys from got.GVR (objects.Get's return), so the lister widens
// its return shape from a bare []*unstructured to []navigationRoot
// so the GVR threads through to the F2 walker's content-L1 Put site
// (populateWidgetContentL1) and the resulting key MATCHES the serve-
// time key. The GVR is parsed from the templatesv1.ObjectReference
// each /call URL decoded into — identical parse to util.ParseGVR.
type navigationRoot struct {
	Root *unstructured.Unstructured
	GVR  schema.GroupVersionResource
}

// rootsLister abstracts the cluster-wide LIST of the navigation-root CRs
// so the no-hardcode falsifier test can substitute an in-memory inventory
// without a cluster. Production lists BOTH routesloaders and navmenus.
type rootsLister func(ctx context.Context) ([]navigationRoot, error)

// rootResolver abstracts resolving a single navigation-root CR (and, in
// production, recursively walking its widget subtree). Production passes
// resolveNavigationRoot (the standard widget resolver + recursive
// walker); the falsifier tests substitute a stub that drives the same
// informer-registration side effects deterministically.
type rootResolver func(ctx context.Context, root navigationRoot) error

// Phase1Warmup runs the Tag B Part 1 SA-credentialed recursive resolution
// walk, then blocks on the Phase 1 sync barrier and signals
// cache.Phase1Done.
//
// Sequence:
//   - register the 8 meta-query seeds (routesloaders / navmenus /
//     restactions / customresourcedefinitions + the 4 RBAC GVRs);
//   - start the CRD-watch (Part 2) so composition informers spawn as
//     their CRDs are observed for navigation-discovered groups;
//   - READ the navigation roots from the frontend ConfigMap (config.json
//     .api.INIT / .api.ROUTES_LOADER) and recursively resolve each
//     navigation tree under SA identity — the resolution auto-registers
//     an informer per touched GVR;
//   - let the registered set settle (the CRD-watch may still be adding
//     composition informers after the walk's last resolve);
//   - WaitAllInformersSynced — block until every registered informer
//     (including the CRD-watch-spawned composition informer) is synced;
//   - cache.MarkPhase1Done — flips the /readyz gate to 200 (Ship 2 /
//     0.30.196: this is the LAST cohort-INDEPENDENT step; readiness is
//     gated only on the substrate above, never on the per-cohort seed);
//   - launch the per-cohort prewarm seed (Step 7.6) as a bounded
//     best-effort BACKGROUND warm — outcome log-only, readiness already 200.
//
// ctx bounds the whole walk + sync barrier (main.go gives it the
// startupProbe budget). On ctx cancellation Phase1Warmup still calls
// MarkPhase1Done in its caller — a pod that cannot warm in the budget
// is better Ready-and-degraded than CrashLoop; main.go owns that
// decision. Phase1Warmup itself returns the first error encountered.
//
// MUST be called only when cache.PrewarmEnabled() — main.go enforces
// this. Calling it with the cache disabled / passthrough is a no-op.
func Phase1Warmup(ctx context.Context, rc *rest.Config, authnNS string) error {
	log := slog.Default()
	rw := cache.Global()
	if rw == nil {
		log.Info("phase1.warmup.skipped",
			slog.String("subsystem", "cache"),
			slog.String("reason", "cache disabled — no informer factory to warm"),
		)
		return nil
	}

	saEP, saErr := idynamic.ServiceAccountEndpoint()
	if saErr != nil {
		log.Warn("phase1.warmup.no_sa_endpoint",
			slog.String("subsystem", "cache"),
			slog.Any("err", saErr),
			slog.String("effect", "Phase 1 cannot resolve under SA identity; lazy register-on-navigation still covers every GVR on first request"),
		)
		return saErr
	}

	dynCli, dynErr := k8sdynamic.NewForConfig(rc)
	if dynErr != nil {
		log.Warn("phase1.warmup.no_dyn_client",
			slog.String("subsystem", "cache"),
			slog.Any("err", dynErr),
		)
		return dynErr
	}

	// The navigation-config namespace: snowplow's control-plane namespace
	// — the same authn-namespace it already runs in / authenticates
	// against. It is NOT a Go constant: it flows from the AUTHN_NAMESPACE
	// chart value via main.go's --authn-namespace flag.
	cfgNamespace := authnNS

	// listNavigationRootsFromConfigMap fetches the frontend ConfigMap and
	// the two named root CRs via objects.Get, which honours the
	// internal-dispatch context. So the lister runs under the same
	// SA-credentialed context the per-root resolver uses — built here
	// once via withPhase1SAContext.
	lister := func(lctx context.Context) ([]navigationRoot, error) {
		return listNavigationRootsFromConfigMap(
			withPhase1SAContext(lctx, *saEP, rc), dynCli, cfgNamespace)
	}

	// Ship F2 (0.30.125): the content-prewarm harvester. When
	// PREWARM_CONTENT_ENABLED is set, the discovery walk harvests every
	// widget's spec.apiRef into this shared set (Step 7.5a) and the
	// contentPrewarm callback below drains it (Step 7.5b). Flag-off the
	// harvester stays nil — the walk's harvestApiRef calls are no-ops and
	// the callback is nil, so startup is byte-identical to 0.30.124.
	var harvester *contentPrewarmHarvester
	var contentPrewarm func(context.Context)
	if cache.PrewarmEnabled() && PrewarmContentEnabled() {
		harvester = newContentPrewarmHarvester()
		contentPrewarm = func(cctx context.Context) {
			runContentPrewarmPass(cctx, harvester, *saEP, rc, authnNS)
		}
	}

	// Ship PIP (0.30.173): the per-identity prewarm seed harvester.
	// Sibling of the content-prewarm harvester. When PIP is on the
	// discovery walk harvests every resolved navigation widget CR + its
	// (GVR, perPage, page) tuple into this set (Step 7.6a); the pipSeed
	// callback below drains it together with the apiRef set to seed the
	// top-level per-user resolved-output L1 for every enumerated RBAC
	// cohort BEFORE phase1Done flips. Flag-off (PIP_ENABLED=false or
	// PREWARM_CONTENT_ENABLED=false) the harvester stays nil — startup
	// is byte-identical to 0.30.172.
	//
	// PIP rides the content-prewarm gate (it depends on the same
	// apiRefHarvester for the restactions seed loop). A future ship may
	// split the gates if the OOM profile justifies it.
	var navHarvester *navWidgetHarvester
	var pipSeed pipSeedFn
	if cache.PrewarmEnabled() && PrewarmContentEnabled() && PrewarmPIPEnabled() {
		navHarvester = newNavWidgetHarvester()
		pipSeed = func(pctx context.Context) error {
			return runPIPSeed(pctx, harvester, navHarvester, *saEP, rc, authnNS)
		}
	}

	// Path 3.2.2.b (0.30.221) — the deferred apiRef pagination collector.
	// The walker writes jobs here during Phase 1 (cheap mutex append);
	// phase1WarmupWith drains them in a background goroutine AFTER
	// MarkPhase1Done so /readyz flips at the pre-3.2.2 baseline wall-clock
	// even at 50K-composition scale (the 0.30.220 boot-blocking inline
	// regression is structurally gone). Always-on when prewarm is enabled
	// (the 0.30.220 mechanism predicates already gate at COLLECT time on
	// widget shape + .slice.continue, so flag-off widgets pay no cost).
	// nil when cache.PrewarmEnabled()==false — walker falls back to
	// byte-identical-to-pre-3.2.2 page-1-only behaviour.
	var pagCollector *apiRefPaginationCollector
	if cache.PrewarmEnabled() {
		pagCollector = newApiRefPaginationCollector()
	}

	resolver := func(rctx context.Context, root navigationRoot) error {
		return resolveNavigationRoot(rctx, root.Root, root.GVR, *saEP, rc, authnNS, harvester, navHarvester, pagCollector)
	}

	// Ship 1 — the unified dynamic cohort-prewarm engine. When ON (and the
	// PIP harvesters exist — the engine shares them), the background seed
	// goroutine routes through the engine: it runs the post-sync re-walk
	// (the boot-race fix), builds the BindingsByGVR index over the
	// navigated GVRs, and seeds per-target-GVR-scoped cohorts — instead of
	// the legacy global runPIPSeed. engineSeed is the background callback
	// phase1WarmupWith invokes at Step 7.6 in place of pipSeed when set.
	var engineSeed pipSeedFn
	if PrewarmEngineEnabled() && navHarvester != nil {
		deps := rePrewarmDeps{
			rw:        rw,
			lister:    lister,
			harvester: harvester,
			navHarv:   navHarvester,
			saEP:      *saEP,
			saRC:      rc,
			authnNS:   authnNS,
		}
		engineSeed = func(pctx context.Context) error {
			// bootDone is closed by the engine's scopeDone callback the
			// instant the BOOT scope finishes — so this goroutine returns at
			// ACTUAL completion (S2), not after the full pipGlobalTimeout.
			bootDone := make(chan struct{})
			var bootErr error
			var closeOnce sync.Once
			StartPrewarmEngine(pctx, makeBootScopeHandler(deps), func(s prewarmScope, err error) {
				if s.kind == scopeKindBoot {
					bootErr = err
					closeOnce.Do(func() { close(bootDone) })
				}
			})
			// Ship 1 enqueues only the BOOT scope. Ship 2 wires runtime
			// triggers (widget/RESTAction CR + RBAC shift) to enqueueScope.
			prewarmEngineSingleton().enqueueScope(prewarmScope{kind: scopeKindBoot})
			// Wait for boot completion OR pctx cancel — whichever first. The
			// engine worker keeps running for any future (Ship 2) enqueues;
			// this background goroutine's job is done once boot is.
			select {
			case <-bootDone:
				return bootErr
			case <-pctx.Done():
				return pctx.Err()
			}
		}
	}

	// Prefer the engine when enabled; else the legacy PIP seed.
	seedFn := pipSeed
	if engineSeed != nil {
		seedFn = engineSeed
	}

	// Path 3.2 / 0.30.218 — Step 7.5 cluster_list cell pre-warm. Runs
	// BEFORE MarkPhase1Done (the cells must be warm by the time
	// /readyz flips so the first customer /call hits warm cells, not
	// the cold-fallback path). Nil-safe: when CACHE_ENABLED=false /
	// PREWARM_CONTENT_ENABLED=false (no harvested RA set), the hook
	// is no-op.
	var clusterListPrewarm clusterListPrewarmFn
	if harvester != nil {
		clusterListPrewarm = makeClusterListPrewarmFn(harvester, *saEP, rc, authnNS)
	}

	// Path 3.2.2.b (0.30.221) — the post-MarkPhase1Done pagination drain.
	// Closure over the collector + SA credentials so phase1WarmupWith can
	// launch the drain goroutine without knowing about endpoints (same
	// shape as pipSeed). nil when the collector is nil (cache /
	// prewarm OFF) — the drain step is a clean no-op.
	var paginationDrain paginationDrainFn
	if pagCollector != nil {
		paginationDrain = func(dctx context.Context) {
			drainApiRefPaginationJobs(dctx, pagCollector.drain(), *saEP, rc)
		}
	}

	return phase1WarmupWith(ctx, rw, lister, resolver, contentPrewarm, clusterListPrewarm, seedFn, paginationDrain)
}

// paginationDrainFn is the Path 3.2.2.b (0.30.221) Step 7.7 callback
// that drains the deferred apiRef pagination jobs collected during the
// Phase 1 walk. phase1WarmupWith invokes it on a bounded background
// goroutine AFTER MarkPhase1Done — readiness is already 200; the drain
// fills identity-free widgetContent L1 cells for items 6..N of each
// apiRef+resourcesRefsTemplate widget without blocking /readyz.
//
// Lifecycle bound: paginationDrainTimeout (5 min). The drain dies with
// the process on shutdown via parent ctx cancellation. Outcome is
// log-only — never withholds readiness, never fail-closes; the page-1
// envelope (Put before MarkPhase1Done) covers items 1..5 for every
// widget regardless of drain progress.
//
// nil disables the step (flag-off / tests); production passes a closure
// over the walker's apiRefPaginationCollector + SA credentials.
type paginationDrainFn func(ctx context.Context)

// phase1WarmupWith is the testable core: it takes the watcher, the
// navigation-roots lister, and the per-root resolver as injected
// dependencies. Production wires the real cluster-backed versions;
// the no-hardcode + premature-Ready falsifier tests wire in-memory
// stubs.
//
// It NEVER calls cache.MarkPhase1Done on the error path before the sync
// barrier — Phase1Done must reflect a truly-warm informer set
// (premature-Ready falsifier). MarkPhase1Done is called exactly once,
// after WaitAllInformersSynced returns, regardless of walk errors:
// informer registration already happened, so the registered set IS what
// /readyz should gate on. A walk error (one root failed to resolve) is
// returned for logging but does not by itself withhold readiness — the
// other roots' informers are warm.
// contentPrewarm is the Ship F2 (0.30.125) Step-7.5 callback:
// phase1WarmupWith invokes it AFTER WaitAllInformersSynced and BEFORE
// MarkPhase1Done — behind the 503 readiness gate — so the SA content-
// population pass completes before the pod goes Ready. nil disables the
// step (flag-off / tests); production passes runContentPrewarmPass.
type contentPrewarm func(ctx context.Context)

// pipSeedFn is the Ship PIP (0.30.173) Step-7.6 callback, RE-WIRED to a
// BACKGROUND best-effort warm at Ship 2 / 0.30.196.
//
// 0.30.196 — COHORT-COUNT-INDEPENDENT READINESS (the not-Ready-forever
// landmine removal). Readiness MUST gate ONLY on the cohort-independent
// substrate (meta-query seeds + CRD-watch settled + all registered
// informers HasSynced) — never on the per-cohort seed, whose work scales
// with cohort count. The architecture gate proved a cold customer nav is
// served entirely from the in-memory informer substrate (0 apiserver
// round-trips), so the per-cohort response seed does NOT carry the cold
// path; the substrate does.
//
// So phase1WarmupWith now calls cache.MarkPhase1Done (Step 8) IMMEDIATELY
// after the informer sync barrier (Step 7) + the content pass (Step 7.5),
// then launches the per-cohort seed (this callback) as a bounded
// best-effort BACKGROUND goroutine (Step 7.6). The seed's outcome is
// log-only — it NEVER withholds readiness and NEVER fail-closes. A pod
// with O(users) cohorts reaches /readyz 200 in the same bounded time as a
// 34-cohort pod; the old PREWARM_PIP_COHORT_CAP fail-closed-forever
// branch is DELETED (see runPIPSeed). nil disables the step (flag-off /
// tests); production passes runPIPSeed.
type pipSeedFn func(ctx context.Context) error

func phase1WarmupWith(ctx context.Context, rw *cache.ResourceWatcher, lister rootsLister, resolve rootResolver, contentWarm contentPrewarm, clusterListPrewarm clusterListPrewarmFn, pipSeed pipSeedFn, paginationDrain paginationDrainFn) error {
	log := slog.Default()
	start := time.Now()

	// Step 1 — register the hardcoded meta-query seeds. This is the ONLY
	// place a hardcoded GVR is handed to the watcher at startup. Ship 0
	// / 0.30.222 removed the customresourcedefinitions GVR from this
	// set; Ship 0.5 / 0.30.223 (v6) DELETED the CRD informer entirely.
	// Composition GVRs are discovered by one-shot apiserver discovery
	// (cache.DiscoverGroupResources) invoked synchronously from the
	// walker the first time it reaches a templated apiserver path.
	rw.RegisterMetaQuerySeeds()

	// Step 2 — (Ship 0 / 0.30.222) the explicit StartCRDWatch call that
	// lived here was DELETED. Ship 0.5 / 0.30.223 (v6) DELETED the CRD
	// informer entirely. Composition GVR discovery is now a synchronous
	// side-effect of the walker's lazyRegisterInnerCallPaths hook
	// (resolve.go:958-961) — for every templated apiserver path the
	// walker invokes cache.AddNavigationDiscoveredGroup(grp) + cache.
	// DiscoverGroupResources(ctx, rc, grp). Diego's invariant 2026-06-01
	// — "no CRD informer if the CRD object itself is not walked in
	// frontend navigation" — is now even more strictly enforced (no
	// CRD informer at all).

	// Step 3 — READ the navigation roots from the frontend ConfigMap
	// (config.json .api.INIT / .api.ROUTES_LOADER → the two named root
	// widget CRs). No hardcoded GVR LIST.
	roots, listErr := lister(ctx)
	if listErr != nil {
		log.Warn("phase1.warmup.roots_list_failed",
			slog.String("subsystem", "cache"),
			slog.Any("err", listErr),
			slog.String("effect", "no roots to walk; lazy register-on-navigation still covers GVRs on first request"),
		)
		// No roots — still run the sync barrier over whatever the
		// meta-query seeds + CRD-watch registered, then signal done.
		_ = rw.WaitAllInformersSynced(ctx)
		cache.MarkPhase1Done()
		return listErr
	}

	log.Info("phase1.warmup.roots_discovered",
		slog.String("subsystem", "cache"),
		slog.Int("roots_count", len(roots)),
	)

	// Step 4 — recursively resolve each navigation root under SA identity.
	// The resolution's inner-call walk auto-registers an informer per
	// touched GVR (lazyRegisterInnerCallPaths) and — for every templated
	// apiserver path — invokes cache.DiscoverGroupResources to register
	// every composition GVR in the encountered group (Ship 0.5 / v6).
	// Output discarded. Resolution errors are collected, not fatal: one
	// broken root must not block warming the rest.
	var walkErr error
	resolved := 0
	for _, root := range roots {
		if ctx.Err() != nil {
			walkErr = ctx.Err()
			break
		}
		if err := resolve(ctx, root); err != nil {
			log.Warn("phase1.warmup.root_resolve_failed",
				slog.String("subsystem", "cache"),
				slog.String("root", rootKey(root.Root)),
				slog.Any("err", err),
			)
			if walkErr == nil {
				walkErr = err
			}
			continue
		}
		resolved++
	}

	// Step 5 — (Ship 0.5 / 0.30.223, v6) DELETED. The pre-v6 path
	// invoked a CRD-store re-scan here to close the CRD-
	// informer initial-LIST replay-vs-discover race. v6 deletes the
	// CRD informer entirely; DiscoverGroupResources is a synchronous
	// transaction inside lazyRegisterInnerCallPaths, so there is no
	// replay window, no race, and nothing to reconcile. Composition
	// informers spawned during Step 4 are already in rw.informers by
	// the time Step 4 returns.

	// Step 6 — let the registered set settle. A composition informer's
	// initial LIST runs asynchronously even though its EnsureResource-
	// Type registration is synchronous. Poll RegisteredGVRs until it
	// stops growing for one settle window, bounded by ctx.
	settleRegisteredSet(ctx, rw)

	// Step 7 — the Phase 1 sync barrier. Block until every registered
	// informer (meta-query seeds + resolution-discovered + CRD-watch-
	// spawned) reaches HasSynced, bounded by ctx.
	syncErr := rw.WaitAllInformersSynced(ctx)

	// Step 7.5 — Ship F2 (0.30.125): the SA content-population pass. Runs
	// AFTER the sync barrier (the informers it resolves against are warm)
	// and BEFORE MarkPhase1Done (still behind the 503 readiness gate — the
	// pod goes Ready only once the content cache is warm). nil when
	// PREWARM_CONTENT_ENABLED is off — flag-off this is byte-identical to
	// 0.30.124. The pass is best-effort: any failure is logged inside
	// runContentPrewarmPass and never blocks readiness.
	if contentWarm != nil {
		contentWarm(ctx)
	}

	// Step 7.5 (Path 3.2 / 0.30.218) — cluster_list cell pre-warm. Runs
	// BEFORE MarkPhase1Done so the first customer /call after readiness
	// flip hits warm cluster_list cells, not the cold-fallback path.
	// Bounded by 60s (api.ClusterListPrewarmTimeout); on timeout
	// MarkPhase1Done fires regardless — the cluster_list cold-fallback
	// path covers any unwarmed cell at /call time. nil-safe when
	// cache off / no harvester.
	if clusterListPrewarm != nil {
		clusterListPrewarm(ctx)
	}

	// Step 8 — signal Phase1Done. /readyz flips to 200.
	//
	// Ship 2 / 0.30.196 — MarkPhase1Done is called HERE, immediately after
	// the cohort-INDEPENDENT substrate is warm (meta-query seeds + CRD-watch
	// settled + every registered informer HasSynced via WaitAllInformersSynced
	// + the content pass), and BEFORE the per-cohort seed (Step 7.6 below).
	// Readiness no longer waits on — and is no longer gated by — the
	// per-cohort seed, whose work scales with cohort count. This removes the
	// not-Ready-forever landmine (the old PREWARM_PIP_COHORT_CAP fail-closed
	// branch) and makes boot wall-clock cohort-count-independent.
	cache.MarkPhase1Done()

	// Step 7.6 — Ship PIP (0.30.173), RE-WIRED to BACKGROUND at Ship 2 /
	// 0.30.196: the per-identity prewarm seed. Seeds the per-user
	// resolved-output L1 (top-level restactions + widgets cache classes)
	// for EVERY enumerated RBAC cohort. nil when PIP is off — flag-off
	// this is byte-identical to 0.30.172.
	//
	// BACKGROUND + BEST-EFFORT (Ship 2 / 0.30.196). The seed runs AFTER
	// MarkPhase1Done on a bounded background goroutine — it NEVER withholds
	// readiness and NEVER fail-closes. Its lifecycle bound is its OWN
	// timeout context (pipGlobalTimeout, 8 min) derived from
	// context.Background() — NOT the Phase1Warmup ctx, which main.go cancels
	// the instant Phase1Warmup returns (the seed would otherwise be killed
	// before it could warm a single cohort). The goroutine is therefore
	// self-terminating (bounded by pipGlobalTimeout) and dies with the
	// process on shutdown; it is not a leak. A panic inside the seed is
	// recovered so a single bad cohort cannot crash the process. The seed's
	// outcome is log-only.
	if pipSeed != nil {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error("phase1.seed.panic",
						slog.String("subsystem", "cache"),
						slog.Any("panic", r),
						slog.String("effect", "per-cohort background seed aborted; "+
							"readiness UNAFFECTED — first /call per cohort falls back to per-user resolve"),
					)
				}
			}()
			seedCtx, seedCancel := context.WithTimeout(context.Background(), pipGlobalTimeout)
			defer seedCancel()
			// 0.30.207 — seedCtx is a bare context.Background() derivative
			// (it must outlive Phase1Warmup's ctx). Like cacheCtx it carries
			// no logger, so the cohort-seed resolves underneath would hit
			// xcontext.Logger's hardcoded slog.LevelDebug fallback and emit
			// full-dict DEBUG lines regardless of DEBUG=false. Inject the
			// level-configured default logger so the seed resolve obeys the
			// flag (level gating only — log content unchanged).
			seedCtx = xcontext.BuildContext(seedCtx, xcontext.WithLogger(slog.Default()))
			seedStart := time.Now()
			if err := pipSeed(seedCtx); err != nil {
				log.Warn("phase1.seed.background_incomplete",
					slog.String("subsystem", "cache"),
					slog.Any("err", err),
					slog.String("effect", "readiness UNAFFECTED (already 200); first /call per "+
						"affected cohort falls back to per-user resolve"),
					slog.Int64("elapsed_ms", time.Since(seedStart).Milliseconds()),
				)
			}
		}()
	}

	// Step 7.7 — Path 3.2.2.b (0.30.221) — the deferred apiRef pagination
	// drain. Runs AFTER MarkPhase1Done (Step 8 above) on a bounded
	// background goroutine — readiness is already 200; the drain fills
	// identity-free widgetContent L1 cells for items 6..N of each
	// apiRef+resourcesRefsTemplate widget without blocking /readyz.
	//
	// Path 3.2.2 (0.30.220) HARD REVERT root cause was that this work
	// ran INLINE on the Phase 1 walker goroutine; at 50K-composition scale
	// the per-widget pagination (up to 500 pages × ~125–500ms wall-clock
	// each) extended the walk past the kubelet startup probe budget so
	// the pod never became Ready. The MECHANISM (iterateApiRefPages) was
	// empirically validated correct — only the scheduling was wrong.
	//
	// Lifecycle bound: paginationDrainTimeout (5 min). The drain goroutine
	// is self-terminating (bounded by paginationDrainTimeout) and dies
	// with the process on shutdown — not a leak. A panic inside the drain
	// is recovered so a single bad job cannot crash the process. Outcome
	// is log-only; the page-1 envelope (Put before MarkPhase1Done) covers
	// items 1..5 for every widget regardless of drain progress.
	//
	// nil when cache.PrewarmEnabled()==false / tests — clean no-op.
	if paginationDrain != nil {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error("phase1.pagination_drain.panic",
						slog.String("subsystem", "cache"),
						slog.Any("panic", r),
						slog.String("effect", "background apiRef pagination drain aborted; "+
							"readiness UNAFFECTED (already 200) — page-1 cells still correct, "+
							"items 6..N fall back to per-user serve-time resolve"),
					)
				}
			}()
			drainCtx, drainCancel := context.WithTimeout(context.Background(), paginationDrainTimeout)
			defer drainCancel()
			// Same logger-injection rationale as the PIP seed above
			// (xcontext.Logger's hardcoded slog.LevelDebug fallback when
			// the ctx carries no logger). Level-only — log content unchanged.
			drainCtx = xcontext.BuildContext(drainCtx, xcontext.WithLogger(slog.Default()))
			paginationDrain(drainCtx)
		}()
	}

	log.Info("phase1.warmup.completed",
		slog.String("subsystem", "cache"),
		slog.Int("roots_total", len(roots)),
		slog.Int("roots_resolved", resolved),
		slog.Int("informers_registered", rw.RegisteredCount()),
		slog.Int64("elapsed_ms", time.Since(start).Milliseconds()),
		slog.Bool("sync_ok", syncErr == nil),
	)

	if walkErr != nil {
		return walkErr
	}
	return syncErr
}

// phase1SettleWindow is how long RegisteredGVRs must stay constant
// before the walk's registered set is considered settled.
const phase1SettleWindow = 750 * time.Millisecond

// phase1SettlePoll is the poll cadence for the settle check.
const phase1SettlePoll = 150 * time.Millisecond

// settleRegisteredSet polls the watcher's registered-GVR count until it
// holds steady for phase1SettleWindow, or ctx is cancelled. This lets
// the CRD-watch's asynchronous per-GVR registrations land before the
// sync barrier snapshots the informer set.
func settleRegisteredSet(ctx context.Context, rw *cache.ResourceWatcher) {
	last := rw.RegisteredCount()
	stableSince := time.Now()
	t := time.NewTicker(phase1SettlePoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := rw.RegisteredCount()
			if now != last {
				last = now
				stableSince = time.Now()
				continue
			}
			if time.Since(stableSince) >= phase1SettleWindow {
				return
			}
		}
	}
}

// rootKey renders a navigation-root CR's namespace/name for logging.
func rootKey(root *unstructured.Unstructured) string {
	if root == nil {
		return "<nil>"
	}
	ns := root.GetNamespace()
	if ns == "" {
		return root.GetName()
	}
	return ns + "/" + root.GetName()
}

// phase1MaxWalkDepth bounds the recursive widget-tree descent. The portal
// navigation tree is shallow (Root -> Route -> Page -> Row/Column ->
// DataGrid/Table is ~5 levels); this cap is a defensive guard against a
// pathological CR graph that the visited-set somehow fails to dedupe. It
// is NOT a per-resource policy — it is a uniform recursion-safety bound.
const phase1MaxWalkDepth = 32

// resolveNavigationRoot resolves one navigation-root CR through the
// STANDARD widget resolver under the snowplow SA identity, then
// RECURSIVELY walks the resolved widget tree (0.30.105). The resolved
// output is discarded — Phase 1 is discovery-only. The resolution's side
// effects (informer auto-registration via lazyRegisterInnerCallPaths,
// CRD-watch group feed) are the whole point.
//
// The SA credentials are injected so the standard resolver runs unchanged
// under SA identity:
//   - xcontext.WithUserConfig(saEP): the endpoint shape the resolver
//     expects on the context (it carries the SA apiserver URL).
//   - xcontext.WithUserInfo({snowplow SA}): the resolver requires an
//     identity on the context.
//   - cache.WithInternalEndpoint(&saEP): the RESTAction resolver's
//     non-UAF inner-call endpoint resolution returns the SA endpoint
//     instead of looking up a (non-existent) `<sa>-clientconfig` Secret.
//   - cache.WithInternalRESTConfig(saRC): the LOAD-BEARING 0.30.103 fix.
//     saRC is the SA *rest.Config built by main.go from
//     rest.InClusterConfig — it carries the SA bearer token and CA with
//     the correct in-cluster semantics. The resolver's object-fetch sites
//     (objects.Get, resourcesrefs.Resolve) use it verbatim via
//     cache.ClientConfigFor instead of rebuilding a client from saEP
//     through kubeconfig.NewClientConfig.
// withPhase1SAContext builds the SA-credentialed context Phase 1
// resolution runs under. It is the SINGLE place the SA identity +
// internal-dispatch markers are installed, shared by the navigation-root
// lister (the ConfigMap read + root-CR fetch) and resolveNavigationRoot
// (the per-root recursive walk) so both run under exactly one identity.
//
// The context it installs:
//   - xcontext.WithUserConfig(saEP)   — the endpoint shape the resolver
//     expects on the context.
//   - xcontext.WithUserInfo({canonical SA username}) — the identity. The
//     username is the CANONICAL `system:serviceaccount:<ns>:<name>` form
//     (0.30.108) so rbac.EvaluateRBAC's ServiceAccount-kind subject
//     matcher can connect Phase 1's identity to the snowplow SA's real
//     ClusterRoleBinding (`*/*` get/list/watch). Derived from the SA
//     token's JWT `sub` claim — see phase1SAUsername.
//   - cache.WithInternalEndpoint(&saEP) — the RESTAction resolver's
//     non-UAF inner-call endpoint resolution returns the SA endpoint
//     instead of looking up a (non-existent) `<sa>-clientconfig` Secret.
//   - cache.WithInternalRESTConfig(saRC) — the SA *rest.Config built by
//     main.go from rest.InClusterConfig; the resolver's object-fetch
//     sites (objects.Get, resourcesrefs.Resolve) use it verbatim instead
//     of rebuilding a client from saEP through kubeconfig.NewClientConfig
//     (the LOAD-BEARING 0.30.103 fix — the SA's raw-PEM CA cannot survive
//     the base64/cert-only kubeconfig path).
func withPhase1SAContext(ctx context.Context, saEP endpoints.Endpoint, saRC *rest.Config) context.Context {
	opts := []xcontext.WithContextFunc{
		xcontext.WithUserConfig(saEP),
		xcontext.WithLogger(slog.Default()),
	}
	// The CANONICAL SA username: derived from the JWT `sub` claim of the
	// projected SA token saEP already carries. Without it (token absent /
	// malformed) Phase 1 still runs — it is best-effort warmup — but the
	// RBAC evaluator then has no SA subject to match; that degraded case
	// is logged at the call site (resolveNavigationRoot / the lister).
	if u, ok := phase1SAUsername(saEP.Token); ok {
		opts = append(opts, xcontext.WithUserInfo(jwtutil.UserInfo{Username: u}))
	}
	rctx := xcontext.BuildContext(ctx, opts...)
	rctx = cache.WithInternalEndpoint(rctx, &saEP)
	rctx = cache.WithInternalRESTConfig(rctx, saRC)
	// Gate 1 (0.30.201-diag) — stamp the DIAGNOSTIC boot-prewarm-walk
	// fallthrough scope so the existing RecordApiserverFallthrough calls
	// on the discovery-walk path (KindFor at resourcesrefs/resolve.go,
	// informer-not-synced/not-servable at informer_dispatch.go) become
	// LIVE during boot and land in snowplow_apiserver_fallthrough_cells
	// keyed "boot-prewarm-walk|<gvr>|<reason>". Without this the walk
	// context carries no scope (fallthrough_ctx.go lists "Phase 1 walker"
	// as a non-/call path) so those recorders no-op and the Gate-1
	// sub-cause discrimination (discovery vs informer-sync vs
	// resolved-but-empty) would be blind. Telemetry-only; no behaviour
	// change (RecordApiserverFallthrough never short-circuits the walk).
	rctx = cache.WithFallthroughScope(rctx, cache.ScopeBootPrewarmWalk)
	// Ship 0.30.127 — FORK B (deliberate, Diego-confirmed). The
	// discovery walk does NOT mark its context cache.WithPrewarmIterSerial,
	// so the RESTAction resolver runs its inner-call iterator fan-out at
	// the DEFAULT bounded parallelism — iterParallelism(ctx) = GOMAXPROCS,
	// hard-capped at 32 (resolve.go). With phase1IteratorCap deleted this
	// ship, that bounded errgroup (g.SetLimit(iterParallelism(ctx))) IS
	// the storm guard for the now-uncapped iterator expansion — a real
	// [1,32] bound, not unbounded. The F2 content pass is the opposite:
	// it DOES set WithPrewarmIterSerial (iterParallelism = 1) because it
	// materializes the 49K-row JSON and is the genuine OOM risk. Fork B:
	// discovery = default-bounded-parallel, content pass = serial. The
	// cache.WithPhase1Resolution marker was REMOVED this ship — its sole
	// consumer (phase1IteratorCap) is gone, so the marker is dead.
	return rctx
}

func resolveNavigationRoot(ctx context.Context, root *unstructured.Unstructured, gvr schema.GroupVersionResource, saEP endpoints.Endpoint, saRC *rest.Config, authnNS string, harvester *contentPrewarmHarvester, navHarvester *navWidgetHarvester, pagCollector *apiRefPaginationCollector) error {
	rctx := withPhase1SAContext(ctx, saEP, saRC)

	// Ship 0.30.232: type-safe construction via newPhase1Walker. rc is now
	// a REQUIRED positional parameter — the compiler enforces every
	// construction site supplies it, eliminating the recurring nil-rc
	// surface that produced six HARD REVERTs (0.30.226 → 0.30.231). See
	// phase1_walker_new.go for the constructor contract.
	w := newPhase1Walker(saRC, authnNS,
		withApiRefHarvester(harvester),
		withNavWidgetHarvester(navHarvester),
		withPagCollector(pagCollector),
	)
	// Ship G (0.30.16x): gvr is threaded from the lister
	// (listNavigationRootsFromConfigMap, phase1_roots.go) which parses
	// it from the templatesv1.ObjectReference each /call URL decoded
	// into. This is the EXACT same parse objects.Get + the dispatcher
	// use (util.ParseGVR), so the content-key shape MATCHES the
	// serve-time key composed by widgets.go from got.GVR — admin and
	// cyberjoker requesting the same root via dispatcher will hit the
	// SAME cell.
	//
	// The root has no /call Path of its own, so it resolves under the
	// bounded PREWARM_PAGE_LIMIT default; each descended child overrides
	// with its own declared slice when present (Ship 0.30.127).
	//
	// Ship 0.30.187 D2: the seed-key tuple for a root navigation widget
	// is (-1, -1). The frontend's first request URL for a root widget
	// carries no slice params so the dispatcher's paginationInfo returns
	// (-1, -1); the seed Put must use the same tuple. Resolution tuple
	// stays = prewarmPageLimit() (the 0.30.127 storm guard).
	//
	// Ship 0.30.199 (Change A): the page NUMBER must be 1, NOT
	// prewarmPageLimit(). injectSlice (widgets/resolve.go:268) computes the
	// list-windowing `offset = (page-1)*perPage`. Passing page == perPage ==
	// prewarmPageLimit() (=5) windowed the children at offset (5-1)*5 = 20,
	// so a small nav root (e.g. the 3-item sidebar-nav-menu) sliced at
	// offset 20 yielded ZERO children and the walk never descended below the
	// roots. The page SIZE stays prewarmPageLimit() — the 0.30.127 bounded
	// fan-out guard is untouched; only the page-NUMBER overshoot is fixed.
	return w.walk(rctx, root, gvr, 0, 1, prewarmPageLimit(), -1, -1)
}

// Ship 0.30.127: the per-(parent,GVR) sample cap — phase1PerGVRSampleLimit,
// parentScopedGVRKey, the phase1Walker.gvrSamples field, and the FAL-126
// falsifier — were ALL DELETED. That count-cap heuristic (0.30.105,
// re-keyed 0.30.126) was the wrong mechanism: it pruned distinct
// navigation widgets by a sibling-count guess. The real data-fan-out
// bound is now the DECLARED per-widget pagination — each widget resolves
// under the `slice` it declares (carried on its `/call` Path) or the
// bounded PREWARM_PAGE_LIMIT default — so the walk recurses EVERY
// distinct navigation child and never the per-row data fan-out, with no
// count heuristic. See walk()'s page/perPage threading.

// phase1Walker carries the per-root recursive-walk state. A fresh walker
// is created per navigation root so the dedupe state never crosses roots
// — but because the two roots can share Page subtrees, dedupe WITHIN a
// root is what matters for cycle-safety; cross-root re-resolves are
// harmless (idempotent informer registration) and rare.
type phase1Walker struct {
	authnNS string
	// rc is the SA *rest.Config (the snowplow ServiceAccount transport)
	// the walker passes into widgets.ResolveOptions.RC at the construction
	// site. Ship 0.30.230 fix-at-root: prior to this ship the literal at
	// w.walk's widgets.Resolve call omitted RC entirely, so downstream
	// crdschema.ValidateObjectStatus → cache.GVRFor → discoverPluralInfo
	// received nil rc and 500'd ("plurals discovery: nil *rest.Config").
	// Set once at resolveNavigationRoot from the same saRC the walker's
	// SA ctx carries (withPhase1SAContext attaches it to ctx; here we
	// thread it explicitly for clarity).
	rc *rest.Config
	// visited dedupes by the child widget endpoint (resource+apiVersion+
	// namespace+name) so a shared subtree is resolved once and a cyclic
	// reference cannot loop forever. With the per-GVR sample cap removed
	// (Ship 0.30.127) the visited-set + phase1MaxWalkDepth are the walk's
	// only recursion bounds.
	visited map[string]struct{}
	// apiRefHarvester accumulates each resolved widget's spec.apiRef —
	// Ship F2 (0.30.125) Step 7.5a. Harvesting rides the EXISTING walk
	// (no second traversal). nil when PREWARM_CONTENT_ENABLED is off —
	// harvestApiRef is nil-safe so flag-off is a clean no-op.
	apiRefHarvester *contentPrewarmHarvester
	// navWidgetHarvester accumulates each resolved navigation widget CR
	// together with the GVR/pagination it was resolved under — Ship PIP
	// (0.30.173) Step 7.6a. Sibling of apiRefHarvester: harvested
	// alongside in walk() (no second traversal). Drained by runPIPSeed
	// for the per-cohort widgets top-level L1 seed loop. nil when PIP is
	// off — harvestNavWidget is nil-safe so the flag-off path is a clean
	// no-op.
	navWidgetHarvester *navWidgetHarvester
	// pagCollector is the Path 3.2.2.b (0.30.221) deferred apiRef
	// pagination collector. The walker writes a job per shape-eligible
	// apiRef+resourcesRefsTemplate widget whose `.slice.continue==true`;
	// the collector is drained AFTER MarkPhase1Done by a background
	// goroutine spawned in phase1WarmupWith. nil-safe: when nil the
	// walker falls back to byte-identical-to-pre-3.2.2 page-1-only
	// behaviour (no pagination). See phase1_walk_pagination_jobs.go for
	// the full rationale (deferred-not-inline scheduling fix for the
	// 0.30.220 boot-blocking inline pagination).
	pagCollector *apiRefPaginationCollector
}

// walk resolves widget `in` through the standard widget resolver under
// the SA-credentialed ctx, then recurses into every resolved
// `status.resourcesRefs.items[]` child whose verb == "GET" (and which is
// allowed). Resolution side effects (informer registration) are the
// point; the resolved output is read only to discover children, never
// persisted.
//
// Errors are NON-FATAL and not propagated upward past the immediate
// resolve: a single broken child widget must not abort warming the rest
// of the navigation tree. The function returns an error ONLY for the
// top-level root resolve failure, so the caller (phase1WarmupWith) can
// log a root as failed.
//
// page/perPage are the pagination declared for THIS widget — extracted
// from the `/call` Path that led to it (Ship 0.30.127). They are passed
// to widgets.Resolve so the walk honours each widget's own declared
// `slice` instead of the old hardcoded -1/-1. The root call passes the
// PREWARM_PAGE_LIMIT default; a child whose `/call` Path carries explicit
// page/perPage overrides it.
//
// keyPerPage/keyPage are the dispatcher-lookup KEY tuple (Ship 0.30.187
// D2). They are DECOUPLED from page/perPage: for a widget reached via a
// /call Path with no declared slice, page/perPage = prewarmPageLimit()
// (the 0.30.127 storm guard) but keyPerPage/keyPage = (-1, -1) — what
// the dispatcher's paginationInfo returns at serve time for a request
// with no URL slice params. For a widget reached via a /call Path with
// declared page/perPage the two tuples are equal. The decoupling fixes
// the 0.30.186 14/17 first-nav-hit defect where the PIP seed Put landed
// in cell (5, 5) but the serve-time dispatcher looked up (-1, -1).
//
// gvr is THIS widget's GroupVersionResource (Ship G, 0.30.16x) — threaded
// from the root site (passed by resolveNavigationRoot) and from the
// recursive site (got.GVR from objects.Get at the child fetch). It feeds
// populateWidgetContentL1's identity-free cache key so the F2 walker's
// Put MATCHES the serve-time dispatcher's key composition. The content
// L1 key uses the KEY tuple symmetrically (same dispatcher-match
// invariant).
func (w *phase1Walker) walk(ctx context.Context, in *unstructured.Unstructured, gvr schema.GroupVersionResource, depth int, page, perPage int, keyPerPage, keyPage int) error {
	log := slog.Default()
	if in == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if depth > phase1MaxWalkDepth {
		log.Warn("phase1.walk.max_depth",
			slog.String("subsystem", "cache"),
			slog.Int("depth", depth),
			slog.String("widget", rootKey(in)),
			slog.String("effect", "recursion capped — deeper navigation widgets covered by lazy register-on-navigation"),
		)
		return nil
	}

	// Ship F2 (0.30.125) Step 7.5a — harvest this widget's spec.apiRef
	// into the content-prewarm data-source set. This rides the EXISTING
	// walk — no second traversal. nil-safe: when PREWARM_CONTENT_ENABLED
	// is off the harvester is nil and this is a no-op.
	w.apiRefHarvester.harvestApiRef(in)

	// Ship PIP (0.30.173) Step 7.6a — harvest this navigation widget CR
	// together with the GVR + pagination tuples it was reached under so
	// the per-cohort seed loop (runPIPSeed) can Put a widgets top-level
	// L1 entry per cohort × widget. Sibling of apiRefHarvester; rides
	// the EXISTING walk, no second traversal. nil-safe — flag-off is a
	// clean no-op.
	//
	// Ship 0.30.187 D2: TWO tuples are passed — the RESOLUTION tuple
	// (perPage, page) is the bounded prewarm pagination used by
	// widgets.Resolve; the KEY tuple (keyPerPage, keyPage) is the
	// dispatcher-lookup tuple the seed Put uses so the cell matches
	// serve-time.
	w.navWidgetHarvester.harvestNavWidget(in, gvr, perPage, page, keyPerPage, keyPage)

	// Ship G defect-fix (AC-G.5): install the widgetContent L1 key on the
	// ctx BEFORE widgets.Resolve. The resolver's inner-call dep recording
	// site (resolvers/restactions/api/resolve.go:467-485) reads
	// L1KeyFromContext and records each inner K8s call's dep edge against
	// THIS L1 key. Without this install the resolver sees "" and records
	// nothing — the content entry would be TTL-only stale-forever even
	// though populateWidgetContentL1 Puts it (AC-G.5 defect caught at
	// architect diff-review). Mirrors widgets.go:148-150 per-user path
	// exactly: cacheKey computed BEFORE Resolve, ctx decorated with
	// WithL1KeyContext, then Resolve called. The key MUST match the key
	// populateWidgetContentL1 Puts under — both call widgetContentL1Key
	// with the SAME tuple.
	//
	// Ship 0.30.187 D2: widgetContentL1Key uses the KEY tuple
	// (keyPerPage, keyPage) — symmetric with the per-user PIP seed —
	// so the content cell matches the dispatcher's serve-time lookup
	// (which composes its key from paginationInfo's URL-derived tuple).
	wcKey, _ := widgetContentL1Key(gvr, in.GetNamespace(), in.GetName(), keyPerPage, keyPage)
	resolveCtx := ctx
	if wcKey != "" {
		resolveCtx = cache.WithL1KeyContext(ctx, wcKey)
	}

	// Resolve this widget. The resolver recursively reaches this widget's
	// apiRef RESTAction (firing lazyRegisterInnerCallPaths on any
	// apiRef-bearing leaf such as the Compositions Page DataGrid) and
	// returns status.resourcesRefs.items[] — the child widget endpoints.
	//
	// Ship 0.30.127: page/perPage are the pagination DECLARED for this
	// widget — its own `slice` carried on the `/call` Path that led here,
	// or the PREWARM_PAGE_LIMIT bounded default. The old hardcoded -1/-1
	// resolved every widget unbounded, which (with the iterator cap
	// removed) is the 49K-row storm. Honouring the declared slice keeps
	// the discovery walk's fan-out bounded by what the portal itself
	// declared per widget.
	res, err := widgets.Resolve(resolveCtx, widgets.ResolveOptions{
		In:      in,
		RC:      w.rc,
		AuthnNS: w.authnNS,
		PerPage: perPage,
		Page:    page,
	})
	if err != nil {
		// A resolution error at depth>0 is non-fatal — log and stop
		// descending this branch. At depth 0 the caller treats a non-nil
		// return as a failed root.
		if depth == 0 {
			return err
		}
		log.Warn("phase1.walk.child_resolve_failed",
			slog.String("subsystem", "cache"),
			slog.Int("depth", depth),
			slog.String("widget", rootKey(in)),
			slog.Any("err", err),
		)
		return nil
	}
	if res == nil {
		return nil
	}

	// Ship G (0.30.16x) — populate the identity-free widget content L1
	// layer as a free side-effect of widgets.Resolve. The walker resolves
	// under the SA identity (withPhase1SAContext); the stored body carries
	// SA-evaluated allowed=true flags on its resourcesRefs.items[] (the
	// snowplow SA's */* get/list/watch grant). They are NEVER served
	// verbatim — gateWidgetEnvelope at widgets.go re-derives every
	// `allowed` flag per-request under the request identity before the
	// body leaves the pod. Gated on cache.WidgetContentL1Enabled() —
	// flag-off this is a clean no-op and startup is byte-identical to
	// pre-Ship-G.
	//
	// extras=nil at prewarm — the walker does not receive user-supplied
	// extras; extras-bearing serve-time requests will MISS the prewarmed
	// entry and fall through to the existing per-user L1, the correct
	// degraded posture.
	//
	// Ship 0.30.187 D2: populateWidgetContentL1 uses the KEY tuple — the
	// content L1 cell must match the dispatcher's serve-time lookup
	// (which composes its key from paginationInfo's URL-derived tuple).
	populateWidgetContentL1(ctx, gvr, in, keyPerPage, keyPage, res)

	// Path 3.2.2.b (0.30.221) — DEFERRED apiRef pagination. Path 3.2.2
	// (0.30.220) ran iterateApiRefPages INLINE here on the Phase 1
	// walker goroutine; at 50K-composition scale the per-widget
	// pagination (up to 500 pages × ~125–500ms wall-clock each) extended
	// the walk past the 360s kubelet startup probe budget so the pod
	// never became Ready (HARD REVERT). The MECHANISM was empirically
	// validated correct; only the scheduling was wrong.
	//
	// Path 3.2.2.b fix: COLLECT a pagination job here (nil-cost mutex
	// append) and DRAIN them through the unchanged iterateApiRefPages
	// mechanism in a background goroutine launched by phase1WarmupWith
	// AFTER MarkPhase1Done. /readyz flips at the same wall-clock as the
	// pre-3.2.2 baseline; the pagination work runs post-readiness with
	// its own bounded budget (paginationDrainTimeout). nil-safe — when
	// pagCollector is nil the walker is byte-identical to pre-3.2.2.
	//
	// The collector enforces both predicates (isApiRefTemplateDriven +
	// resolverWantsContinue) at collect time, so the collected set
	// contains ONLY work that would actually paginate.
	w.pagCollector.collect(apiRefPaginationJob{
		In:         in,
		GVR:        gvr,
		Page1Res:   res,
		Depth:      depth,
		PerPage:    perPage,
		KeyPerPage: keyPerPage,
		AuthnNS:    w.authnNS,
	})

	// Read status.resourcesRefs.items[] — the child widget endpoints.
	children := extractResourcesRefsItems(res.Object)

	// Gate 1 (0.30.201-diag) — record the boot children-count for THIS
	// walked widget so the navmenu's boot-walk children-count is
	// deterministically readable over the LB at /debug/vars (expvar) AND
	// emitted as a bounded structured log line. recordWalkChildren
	// computes the recurse-pass + parse-pass counts over the same slice
	// the descent loop below consumes, so the published counts are
	// byte-faithful to what the walk actually descends. Telemetry-only —
	// no behaviour change.
	recordWalkChildren(gvr, in.GetNamespace(), in.GetName(), depth, children)
	{
		recurseCount, parseCount := 0, 0
		for _, child := range children {
			if walkShouldRecurse(child) {
				recurseCount++
			}
			if _, ok := util.ParseCallPathToObjectRef(child.Path); ok {
				parseCount++
			}
		}
		log.Info("phase1.walk.children_observed",
			slog.String("subsystem", "cache"),
			slog.String("gvr", gvr.String()),
			slog.String("ns", in.GetNamespace()),
			slog.String("name", in.GetName()),
			slog.Int("depth", depth),
			slog.Int("children", len(children)),
			slog.Int("recurse", recurseCount),
			slog.Int("parse", parseCount),
		)
	}

	for _, child := range children {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// SAFETY + CORRECTNESS gate: recurse ONLY into verb=="GET" refs
		// that carry a path. walkShouldRecurse is the single auditable
		// predicate — see its doc for why verb=="GET" is the load-bearing
		// read-only invariant and why `allowed` is deliberately NOT a
		// gate here (Phase 1 is informer DISCOVERY, which is identity-
		// independent; the per-user `allowed` render gate belongs at real
		// request time).
		if !walkShouldRecurse(child) {
			continue
		}

		ref, ok := util.ParseCallPathToObjectRef(child.Path)
		if !ok {
			// A child path that is not a /call?... widget endpoint
			// (external link, malformed) — nothing to recurse into.
			continue
		}
		key := navWidgetEndpointKey(ref)
		if _, seen := w.visited[key]; seen {
			// Already walked (cycle-safety + shared-subtree dedup). The
			// visited-set + phase1MaxWalkDepth are the ONLY recursion
			// bounds — Ship 0.30.127 removed the per-(parent,GVR) sample
			// cap; fan-out is bounded by the declared slice / the
			// PREWARM_PAGE_LIMIT default each widget resolves under.
			continue
		}
		w.visited[key] = struct{}{}

		// Ship 0.30.127 — honour the child's DECLARED pagination. The
		// child's `/call` Path carries page/perPage when the parent
		// declared a `slice` (resourcesrefs writes them). Extract them;
		// when absent, fall back to the bounded PREWARM_PAGE_LIMIT default
		// — NEVER the unbounded -1 (which, with the iterator cap removed,
		// is the 49K-row storm).
		//
		// Ship 0.30.199 (Change A): the no-declared-slice default page
		// NUMBER is 1, NOT prewarmPageLimit(). Same overshoot as the root
		// site: injectSlice's `offset = (page-1)*perPage` windowed a child
		// nav list at offset (5-1)*5 = 20, dropping every child of a small
		// list to ZERO so the recursion never descended. The page SIZE
		// stays prewarmPageLimit() (the 0.30.127 bounded fan-out guard). A
		// child whose /call Path DOES declare a slice still overrides both
		// values below.
		childPage, childPerPage := 1, prewarmPageLimit()
		if p, pp, hasPg := util.ParseCallPathPagination(child.Path); hasPg {
			childPage, childPerPage = p, pp
		}

		// Ship 0.30.187 D2 — derive the dispatcher-lookup KEY tuple from
		// the child's /call Path. The KEY tuple is what paginationInfo
		// returns at serve time for a request hitting the same URL:
		// (-1, -1) for no-slice paths, the declared (perPage, page) for
		// sliced paths. The resolution tuple stays bounded by
		// prewarmPageLimit() (above) — the 0.30.127 storm guard.
		childKeyPerPage, childKeyPage := deriveSeedKeyTuple(child.Path)

		// Fetch the child widget CR under the SA-credentialed ctx. The
		// resolver mutates the object in place, so a fresh fetch per
		// child is required.
		got := objects.Get(ctx, ref)
		if got.Err != nil {
			log.Warn("phase1.walk.child_fetch_failed",
				slog.String("subsystem", "cache"),
				slog.Int("depth", depth),
				slog.String("child", key),
				slog.Any("err", got.Err),
			)
			continue
		}
		if got.Unstructured == nil {
			continue
		}
		// Recurse into the child widget subtree. childPage/childPerPage
		// are the pagination the child resolves under — its declared
		// `slice` from the `/call` Path, or the PREWARM_PAGE_LIMIT default
		// (Ship 0.30.127). childKeyPerPage/childKeyPage are the
		// dispatcher-lookup KEY tuple — decoupled from the resolution
		// tuple per Ship 0.30.187 D2 so the seed cell matches serve-time.
		//
		// Ship G (0.30.16x): got.GVR is the child widget's GVR — threaded
		// from objects.Get's return shape (internal/objects/get.go:22-26)
		// so populateWidgetContentL1 down the recursion has the GVR the
		// serve-time dispatcher will compose its key under.
		_ = w.walk(ctx, got.Unstructured, got.GVR, depth+1, childPage, childPerPage, childKeyPerPage, childKeyPage)
	}
	return nil
}

// navChildRef is the subset of a resolved status.resourcesRefs item the
// walker needs: the navigation edge to a child widget. It mirrors the
// templatesv1.ResourceRefResult fields (id/path/verb/allowed) — the same
// shape the frontend ResourceRef carries.
type navChildRef struct {
	ID      string
	Path    string
	Verb    string
	Allowed bool
}

// walkShouldRecurse is the single, auditable predicate the recursive
// walk applies before descending into a resourcesRefs child.
//
// THE LOAD-BEARING GATE — verb == "GET" (case-insensitive):
//
//	A non-GET resourcesRefs item is a mutation/action endpoint
//	(POST/PUT/PATCH/DELETE) bound to a widget's `actions`, never part of
//	the navigation/render tree. Recursing into it is wrong navigation —
//	AND, because the Phase 1 walk runs with the snowplow service
//	account's PRIVILEGED credentials, following such a ref would issue a
//	DESTRUCTIVE apiserver mutation. verb == "GET" alone fully guarantees
//	the walk stays strictly read-only: a GET is non-destructive
//	regardless of any RBAC verdict.
//
// WHY `allowed` is DELIBERATELY NOT a recursion gate:
//
//	The `allowed` flag a resolved resourcesRefs item carries is set by
//	resourcesrefs.resolveOne via rbac.UserCan -> EvaluateRBAC — snowplow's
//	in-process evaluator of NATIVE Kubernetes RBAC (Role / RoleBinding /
//	ClusterRole / ClusterRoleBinding) keyed on the resolution-context
//	identity. It is the same answer the apiserver's RBAC would give, just
//	served from the informer cache.
//
//	Phase 1 resolves under the snowplow service account's CANONICAL
//	username (system:serviceaccount:<ns>:<name>, derived from the SA
//	token's JWT `sub` claim — 0.30.108 — see phase1SAUsername). That SA
//	holds a native ClusterRoleBinding granting `*/*` get/list/watch, and
//	with the canonical username EvaluateRBAC's ServiceAccount-kind subject
//	matcher connects the identity to that binding — so EvaluateRBAC
//	correctly AUTHORIZES every navigation read and `allowed` is true for
//	the navigation widgets.
//
//	`allowed` is STILL not used as the recursion gate: Phase 1 is informer
//	DISCOVERY, and discovery is identity-independent — the composition
//	informer the Compositions Page needs is the same object set no matter
//	which user can see it. The walk must register the informer for the
//	full GET-navigation STRUCTURE regardless of any per-user render
//	verdict. The frontend WidgetRenderer applies
//	items.filter(({allowed})=>allowed) because a denied widget must not
//	RENDER for a logged-in user — that per-user render gate is correctly
//	applied later, at real request time, not during startup warmup.
//	Gating discovery on a render verdict would couple two concerns that
//	must stay independent. Dropping `allowed` here does NOT weaken the
//	read-only guarantee — verb == "GET" is the sole safety invariant and
//	it is independent of RBAC.
//
//	(HISTORICAL: 0.30.105 misdiagnosed this as "the SA walk is
//	RBAC-denied because it carries no Krateo RBAC CRs" — there are no
//	"Krateo RBAC CRs"; EvaluateRBAC evaluates native Kubernetes RBAC. The
//	actual 0.30.105–107 defect was a MALFORMED SA username
//	("snowplow-serviceaccount", no system:serviceaccount: prefix) that
//	parseServiceAccountUsername could not resolve, so the SA's real
//	ClusterRoleBinding never matched and a fully-authorized SA was
//	silently denied. 0.30.108 fixes the username; see phase1SAUsername.)
//
// Also requires a non-empty path — nothing to fetch/recurse into
// otherwise.
func walkShouldRecurse(child navChildRef) bool {
	return strings.EqualFold(child.Verb, "GET") && child.Path != ""
}

// extractResourcesRefsItems reads status.resourcesRefs.items[] from a
// resolved widget object and returns the navigation child refs. The
// resolver stores items as a []any of map[string]any (the marshalled
// ResourceRefResult slice); this reads them defensively without coupling
// to the resolver's internal marshalling.
func extractResourcesRefsItems(obj map[string]any) []navChildRef {
	items, ok, err := maps.NestedSlice(obj, "status", "resourcesRefs", "items")
	if !ok || err != nil {
		return nil
	}
	out := make([]navChildRef, 0, len(items))
	for _, raw := range items {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		ref := navChildRef{}
		if v, ok := m["id"].(string); ok {
			ref.ID = v
		}
		if v, ok := m["path"].(string); ok {
			ref.Path = v
		}
		if v, ok := m["verb"].(string); ok {
			ref.Verb = v
		}
		if v, ok := m["allowed"].(bool); ok {
			ref.Allowed = v
		}
		out = append(out, ref)
	}
	return out
}

// parseCallPathToObjectRef was LIFTED to internal/handlers/util/callpath.go
// at Ship 0.30.123 (#155) — util.ParseCallPathToObjectRef — so the
// resolver package (which cannot import dispatchers) can share the same
// /call decoder. Call sites here now use util.ParseCallPathToObjectRef.

// navWidgetEndpointKey renders an ObjectReference into the stable dedupe
// key the visited-set is keyed on.
func navWidgetEndpointKey(ref templatesv1.ObjectReference) string {
	return ref.APIVersion + "|" + ref.Resource + "|" + ref.Namespace + "|" + ref.Name
}
