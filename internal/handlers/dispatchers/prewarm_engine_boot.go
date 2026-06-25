// prewarm_engine_boot.go — Ship 1: the rePrewarm core (boot scope) — the
// post-sync re-walk + the per-target-GVR-scoped per-cohort seed, unified
// into one scope-parameterised function.
//
// This is the function StartPrewarmEngine's worker invokes per dequeued
// scope. For the BOOT scope it:
//
//  1. RE-WALKS the 2 nav roots AFTER the sync barrier, with a FRESH
//     phase1Walker per root (new visited map) and the SAME walk() — so
//     harvestApiRef + harvestNavWidget re-fire over the now-populated
//     dynamic navmenu children (the boot-race fix). The harvesters are
//     SHARED BY REFERENCE with the boot pass so the re-walk's harvest
//     lands in the set the seed drains.
//  2. SETTLES the registered set once (settleRegisteredSet — single pass,
//     not a loop) so a CRD discovered by the re-walk has its informer
//     registered before the seed.
//  3. BUILDS the BindingsByGVR index over RegisteredGVRs() (the cohort
//     scoping substrate) — Ship 1 builds it here, on the engine path,
//     after the re-walk has discovered the navigated GVRs.
//  4. SEEDS per cohort with PER-TARGET-GVR scoping: the widget loop scopes
//     cohorts on the widget's GVR; the restaction loop scopes on the
//     RESTAction's TARGET GVR (the GVR it LISTs). Falls back to the global
//     EnumerateBindingSetClasses when the index yields nothing for a GVR
//     (safe over-inclusion).
//
// CUSTOMER PRIORITY. The seed loop calls engineYieldCheckpoint(ctx)
// between cohorts so a customer /call burst arriving mid-seed defers the
// remaining cohorts (project_c3_design_2026_05_27 B1). The seed itself
// keeps the per-cohort timeout (seedCohort) and per-target error
// containment unchanged.
//
// NO STATIC LIST (feedback_dynamic_cohort_prewarm_no_static_no_cold_fill).
// The roots come from the frontend config.json; the cohort set comes from
// the live BindingsByGVR index; the navigated GVRs come from the walk.
// Nothing is a Go literal.

package dispatchers

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/krateoplatformops/plumbing/endpoints"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/objects"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// rePrewarmDeps bundles the dependencies the rePrewarm core needs — the
// watcher, the navigation-roots lister + per-root resolver (so the
// re-walk uses the EXACT same walk() entrypoint as the boot pass), the
// shared harvesters the seed drains, and the SA config for the per-cohort
// seed context. Built once by Phase1Warmup and closed into the boot scope
// handler.
type rePrewarmDeps struct {
	rw        *cache.ResourceWatcher
	lister    rootsLister
	harvester *contentPrewarmHarvester
	navHarv   *navWidgetHarvester
	saEP      endpoints.Endpoint
	saRC      *rest.Config
	authnNS   string
}

// makeBootScopeHandler returns the engine scope handler closure for the
// boot scope, bound to deps. The engine worker calls it per dequeued
// scope; Ship 1 enqueued only scopeKindBoot. Ship 2 Stage 2 (0.30.247)
// adds scopeKindGVRDiscovered dispatch.
func makeBootScopeHandler(deps rePrewarmDeps) func(ctx context.Context, s prewarmScope) error {
	return func(ctx context.Context, s prewarmScope) error {
		switch s.kind {
		case scopeKindBoot:
			return rePrewarmBoot(ctx, deps)
		case scopeKindGVRDiscovered:
			return rePrewarmGVRDiscovered(ctx, deps, s.gvr)
		default:
			// Ship 2 future scopes (scopeKindWidgetCR / scopeKindRBACShift)
			// land here when unwired.
			slog.Warn("prewarm.engine.unknown_scope",
				slog.String("subsystem", "cache"),
				slog.String("scope", s.key()),
			)
			return nil
		}
	}
}

// registerEngineGVRDiscoveredHook subscribes the engine to the
// cache-side GVR-discovered hook. Called once per process from
// StartPrewarmEngine inside startedOnce — BEFORE the engine worker
// spawns — so a GVR discovery firing during boot is queued, not
// dropped.
//
// The hook callback is intentionally TINY: it builds a prewarmScope
// and calls enqueueScope. The enqueue is O(1) (sync.Mutex critical
// section + buffered=1 channel send) and never blocks. The actual
// re-prewarm work runs on the engine worker goroutine, scoped through
// the customer-priority yield + bounded queue.
//
// CONTRACT: the cache fires this hook synchronously inside its
// discovery goroutine. Keep the callback non-blocking — see the
// RegisterGVRDiscoveredHook doc-comment for the cache-side guarantee
// that hook handlers must not block.
//
// Per task-194-ship-2-stage-2-empirical-trace-2026-06-04.md §5.2
// commit-4.
func registerEngineGVRDiscoveredHook(e *prewarmEngine) {
	cache.RegisterGVRDiscoveredHook(func(gvr schema.GroupVersionResource) {
		e.enqueueScope(prewarmScope{
			kind: scopeKindGVRDiscovered,
			gvr:  gvr,
		})
	})
}

// rePrewarmGVRDiscovered is the Ship 2 Stage 2 sub-handler for
// scopeKindGVRDiscovered. Invoked once per (distinct) GVR discovered
// post-boot via the cache→dispatchers hook
// (cache.RegisterGVRDiscoveredHook). The bounded dedup queue at
// prewarm_engine.go:213-225 coalesces repeated enqueues for the same
// GVR within a short window.
//
// MECHANISM: invokes the SAME rePrewarmBoot core — no new harvester
// wiring, no parallel seed mechanism. Per the trace at §5.5 Step F,
// the rePrewarm core ALREADY:
//
//   - Re-walks the nav tree with a fresh visited set (so the iterator
//     at resolve.go:377-381 re-runs over the now-populated crds.items[]
//     — the H4 root cause site that previously short-circuited and
//     skipped dep recording is now non-empty).
//   - Re-builds + re-reads the BindingsByGVR index (now widened by the
//     AddNavigatedGVR call in discovery_lookup.go) — narrow-RBAC
//     cohorts are enumerated.
//   - Re-seeds each cohort via seedOneRestaction/seedOneWidget under
//     the cohort's identity → admin's BindingUID-keyed cell (and every
//     other cohort's cell) is re-resolved with non-empty tmp[] →
//     dep edge against the new GVR IS recorded → subsequent CR events
//     match.
//
// The discovered GVR is logged but not propagated through ctx today
// (no per-GVR filtering inside rePrewarmBoot at this point). A future
// optimisation could narrow the re-walk to roots that template-path
// against this GVR; until then the broad re-walk is the structurally-
// correct mechanism.
//
// SAFETY (R2 install-burst quantification per §5.3): the dedup keys
// distinct GVRs as distinct work items. A 10-CRD install burst
// produces 10 sequential rePrewarms (each yielding to customer /call
// between cohorts via engineYieldCheckpoint). Each rePrewarmBoot is
// bounded by ctx + the engine-side timeouts; the per-cohort
// seedCohort timeout (pipCohortTimeout) caps individual stalls.
func rePrewarmGVRDiscovered(ctx context.Context, deps rePrewarmDeps, gvr schema.GroupVersionResource) error {
	slog.Info("prewarm.engine.gvr_discovered.start",
		slog.String("subsystem", "cache"),
		slog.String("gvr", gvr.String()),
		slog.String("note", "Ship 2 Stage 2 — re-walk under cohort identities so iterator-empty short-circuit (resolve.go:377-381) no longer skips dep recording for this GVR"),
	)
	err := rePrewarmBoot(ctx, deps)
	if err != nil {
		slog.Warn("prewarm.engine.gvr_discovered.incomplete",
			slog.String("subsystem", "cache"),
			slog.String("gvr", gvr.String()),
			slog.Any("err", err),
		)
		return err
	}
	slog.Info("prewarm.engine.gvr_discovered.complete",
		slog.String("subsystem", "cache"),
		slog.String("gvr", gvr.String()),
	)
	return nil
}

// rePrewarmBoot runs the boot re-walk + per-target-GVR-scoped seed. The
// SAME walk() is used (via the deps.lister + a fresh phase1Walker per
// root); the harvesters are shared by reference with the boot pass.
func rePrewarmBoot(ctx context.Context, deps rePrewarmDeps) error {
	log := slog.Default()
	start := time.Now()

	if deps.rw == nil || deps.lister == nil {
		return nil
	}

	// LATENT HAZARD (gate build-obligation #1). This engine boot seed runs
	// the SERIAL single-worker loop (seedScopeYielding — a plain for-loop
	// with one restactions.Resolve in flight at a time, yielding to
	// customers via engineYieldCheckpoint). That serial shape is what
	// memory-bounds the seed: warm-peak is bounded by a SINGLE resolve's
	// weight, NOT a concurrent fan-out, so it CANNOT reproduce the
	// concurrent-fan-out OOM that (a)/#46 fixed.
	//
	// BUT: the DEAD errgroup seed path (runPIPSeed, phase1_pip_seed.go —
	// GOMAXPROCS-bounded g.SetLimit(runtime.GOMAXPROCS(0)) concurrent
	// cohort fan-out) RE-ACTIVATES if PREWARM_ENGINE_ENABLED=false
	// (phase1_walk.go:409 picks runPIPSeed when the engine is off). That
	// concurrent seed BYPASSES the (a) process-wide weighted memory budget
	// (which is on the CUSTOMER resolve entry, not the legacy seed loop) →
	// it can re-OOM on the seed. The proactive RA union here WIDENS the
	// seed source, so a fallback to the concurrent path would fan out over
	// MORE refs.
	//
	// GUARD: production MUST keep PREWARM_ENGINE_ENABLED=true (the helm
	// overlay sets it true — main-chart-reconciliation). F-3 asserts the
	// production-on posture. Deleting the dead errgroup path is a separate
	// BACKLOG item (NOT this ship).
	if !PrewarmEngineEnabled() {
		// Defensive: rePrewarmBoot is only reached when the engine path is
		// selected (phase1_walk.go:482), so this branch should be
		// unreachable in production. Emit loudly if it ever fires.
		log.Warn("prewarm.engine.boot.hazard_engine_disabled_but_boot_reached",
			slog.String("subsystem", "cache"),
			slog.String("effect", "rePrewarmBoot reached with PREWARM_ENGINE_ENABLED=false — the concurrent "+
				"runPIPSeed errgroup path (which BYPASSES the (a) memory budget) may also be active; "+
				"this is the documented latent re-OOM hazard — keep the engine enabled"),
		)
	}

	// ── (1) RE-WALK the nav roots AFTER the sync barrier. A FRESH walker
	// per root (new visited map — reusing the boot pass's visited would
	// short-circuit every child and descend nothing). The SAME walk() so
	// harvestApiRef + harvestNavWidget re-fire over the now-populated
	// dynamic navmenu children.
	rctx := withPhase1SAContext(ctx, deps.saEP, deps.saRC)
	roots, listErr := deps.lister(rctx)
	if listErr != nil {
		log.Warn("prewarm.engine.boot.roots_list_failed",
			slog.String("subsystem", "cache"),
			slog.Any("err", listErr),
			slog.String("effect", "boot re-walk has no roots; first /call per cohort falls back to per-user resolve"),
		)
		return listErr
	}

	rewalked := 0
	for _, root := range roots {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// FRESH walker per root — new visited map (phase1_walk.go:679).
		// Harvesters SHARED BY REFERENCE so the re-walk's harvest lands in
		// the set the seed drains.
		//
		// Ship 0.30.232 — THE BUG SITE: prior six HARD REVERTs (0.30.226
		// → 0.30.231) all traced back to THIS literal omitting rc. Now
		// constructed via newPhase1Walker, which makes rc a REQUIRED
		// positional parameter — the compiler refuses a missing
		// argument. deps.saRC is the same SA *rest.Config the seed pass
		// uses (set once in Phase1Warmup; non-nil per the rePrewarmDeps
		// contract).
		w := newPhase1Walker(deps.saRC, deps.authnNS,
			withApiRefHarvester(deps.harvester),
			withNavWidgetHarvester(deps.navHarv),
		)
		// Same walk() entrypoint + same root tuple as resolveNavigationRoot
		// (page 1, perPage prewarmPageLimit(), key tuple (-1,-1)) — the
		// Change A page-number fix is preserved.
		if err := w.walk(rctx, root.Root, root.GVR, 0, 1, prewarmPageLimit(), -1, -1); err != nil {
			log.Warn("prewarm.engine.boot.root_rewalk_failed",
				slog.String("subsystem", "cache"),
				slog.String("root", rootKey(root.Root)),
				slog.Any("err", err),
			)
			continue
		}
		rewalked++
	}

	// ── (2) SETTLE the registered set once (single pass, not a loop) so a
	// CRD the re-walk discovered has its informer registered before the
	// seed reads RegisteredGVRs().
	settleRegisteredSet(ctx, deps.rw)

	// ── (3) BUILD the BindingsByGVR index over the navigated GVRs. Ship 1
	// builds it here on the engine path, AFTER the re-walk discovered the
	// navigated GVRs. The build is ONCE (Gate-2); steady-state maintenance
	// is the delta hooks (bindings_by_gvr_delta.go).
	navigated := deps.rw.RegisteredGVRs()
	enrolled := cache.BuildBindingsByGVRIndex(navigated)
	log.Info("prewarm.engine.boot.index_built",
		slog.String("subsystem", "cache"),
		slog.Int("navigated_gvrs", len(navigated)),
		slog.Int("bindings_enrolled", enrolled),
	)

	// ── (4) SEED per cohort with PER-TARGET-GVR scoping.
	restactionRefs := deps.harvester.snapshot()
	widgetEntries := deps.navHarv.snapshot()

	// Proactive composition-page RESTAction seed (Option A) — UNION the
	// RBAC-reachable RESTAction refs into the harvester snapshot so the
	// per-composition click-through detail RESTActions (never reached by
	// the nav walk — the harvester gap) are warmed at boot. Default-OFF
	// (PROACTIVE_RA_SEED_ENABLED); flag-off the union is a no-op and the
	// snapshot is harvester-only (F-6 transparent). The added refs flow
	// through the SAME seedScopeYielding loop — each ref is scoped to its
	// own per-binding targets via restActionTargetGVR + per-RA target GVR,
	// so the per-binding cell-key sharing invariant is unchanged.
	if ProactiveRASeedEnabled() {
		restactionRefs = unionProactiveRARefs(restactionRefs, deps.rw, log)
	}

	log.Info("prewarm.engine.boot.rewalk_complete",
		slog.String("subsystem", "cache"),
		slog.Int("roots", len(roots)),
		slog.Int("rewalked", rewalked),
		slog.Int("restactions", len(restactionRefs)),
		slog.Int("widgets", len(widgetEntries)),
		slog.Int64("elapsed_ms", time.Since(start).Milliseconds()),
	)

	// P0 — seed under the SA-credentialed rctx (NOT the bare engine ctx).
	// restActionTargetGVR's objects.Get must carry the SA identity or
	// filterGetByRBAC fail-closes on a missing identity (objects/get.go:99-141)
	// → returns (zero,false) for EVERY restaction → cohortsFor silently
	// reverts to the global 34-cohort set, defeating the per-target-GVR
	// scoping. rctx is derived from ctx so the engine's cancel/timeout still
	// propagates; withCohortSeedContext OVERRIDES identity per cohort for the
	// actual seed, so the SA base is correct for both the derivation fetch
	// and as the per-cohort seed base.
	if err := seedScopeYielding(rctx, restactionRefs, widgetEntries, deps.saEP, deps.saRC, deps.authnNS); err != nil {
		return err
	}

	log.Info("prewarm.engine.boot.complete",
		slog.String("subsystem", "cache"),
		slog.Int64("elapsed_ms", time.Since(start).Milliseconds()),
	)
	return nil
}

// seedScopeYielding seeds the per-user top-level L1 for the harvested
// restactions + widgets, PER-TARGET-GVR-scoped per-binding targets,
// yielding to customer /call between targets. Replaces the pre-ship
// per-cohort fan-out with per-binding fan-out via
// cache.EnumeratePrewarmTargetsForGVR.
//
// The two loops are SYMMETRIC:
//   - widget loop: targets scoped on the widget's GVR (e.GVR).
//   - restaction loop: targets scoped on the RESTAction's TARGET GVR
//     (derived from the RA's userAccessFilter). The apiRef/RAFullList
//     layer is bounded identically (~3-6 targets, not the v3 global ~34
//     cohort universe).
//
// Ship 0.30.242 H.c-layered Phase 2b — the v3 global-cohort fallback
// (`globalCohorts := cache.EnumerateBindingSetClasses()`) is REMOVED.
// Design §22.1.A NACK item #2: when the per-GVR enumerator returns
// empty (genuinely no authorising bindings), the seed loop SKIPS that
// GVR rather than fall back to a global universe — a fallback would
// have included identities that can't authorise the GVR (wasted seed +
// cells under wrong identity).
//
// Runtime-discovered RA targets (ResourcesFrom — no static literal) are
// skipped by the seed engine here; the customer /call resolves them
// cold-then-warm via the on-demand dispatcher (helpers.go's
// dispatchCacheLookupKey populates the cell at first call).
//
// enumeratePrewarmTargetsForGVRFn / seedOneWidgetFn are test seams over the
// two external dependencies seedScopeYielding consumes — the per-binding
// target SOURCE (cache.EnumeratePrewarmTargetsForGVR) and the per-target
// seed PRIMITIVE (seedOneWidget). Same 1-LOC `var fooFn = foo` pattern as
// seedCohortFn (phase1_pip_seed.go:640) and enumerateAggregatePrewarmTargetsFn
// (phase1_pip_seed.go:404). The engine-path re-enqueue-latch falsifier
// (#316) swaps them to drive the widget seed loop's classifyEngineSeedErr
// branch + reEnqueued latch end-to-end without a live cache/RBAC snapshot
// or apiserver. Production ALWAYS uses the real functions; the restaction
// loop and restActionTargetGVR are left direct (the widget loop exercises
// the SAME shared classifier closure + latch — design §3.1).
var enumeratePrewarmTargetsForGVRFn = cache.EnumeratePrewarmTargetsForGVR

// seedOneWidgetFn is a test seam over seedOneWidget — see
// enumeratePrewarmTargetsForGVRFn.
var seedOneWidgetFn = seedOneWidget

func seedScopeYielding(ctx context.Context,
	restactionRefs []templatesv1.ObjectReference, widgetEntries []navWidgetEntry,
	saEP endpoints.Endpoint, saRC *rest.Config, authnNS string) error {

	log := slog.Default()

	// #158 (design §1.4 + §1.5 engine path) — classify per-target seed
	// failures instead of swallowing them. RBAC-deny → Info + rbac_deny
	// counter (NO re-enqueue). Operational → Warn + operational counter +
	// re-enqueue a fresh scopeKindBoot. The engine queue dedups on
	// key()=="boot" (prewarm_engine.go:184-188,251-260) so N operational
	// failures during one run coalesce to AT MOST ONE pending re-walk
	// (design §3.1 storm bound); reEnqueued makes us enqueue at most once
	// per seedScopeYielding invocation so enqueuedTotal stays honest. The
	// re-walk re-runs seedScopeYielding, which yields to customers between
	// every target — a target that failed on transient apiserver pressure
	// is re-seeded after the pressure clears. The back-compat grand total
	// pipBindingSetSeedFailuresTotal is bumped for parity with the legacy
	// path (= rbac_deny + operational).
	reEnqueued := false
	classifyEngineSeedErr := func(kind, label, target string, err error) {
		pipBindingSetSeedFailuresTotal.Add(1)
		if classifySeedErr(err) == seedFailRBACDeny {
			pipSeedRBACDenyTotal.Add(1)
			slog.Info("prewarm.engine.seed.expected_deny",
				slog.String("subsystem", "cache"),
				slog.String("kind", kind),
				slog.String("target", target),
				slog.String(kind, label),
				slog.Any("err", err),
			)
			return
		}
		// Operational (incl. fail-loud default).
		pipSeedOperationalFailTotal.Add(1)
		slog.Warn("prewarm.engine.seed.operational_failure",
			slog.String("subsystem", "cache"),
			slog.String("kind", kind),
			slog.String("target", target),
			slog.String(kind, label),
			slog.Any("err", err),
			slog.String("effect", "operational seed failure (NOT an RBAC deny); a coalesced boot "+
				"re-walk is enqueued (dedup on key()==\"boot\") to retry after pressure clears"),
		)
		if !reEnqueued {
			reEnqueued = true
			prewarmEngineSingleton().enqueueScope(prewarmScope{kind: scopeKindBoot})
		}
	}

	// targetsFor resolves the per-binding target set for a target GVR.
	// Empty when (a) the index is not built (pre-readiness), (b) no
	// binding authorises (gvr, list), or (c) haveGVR=false (runtime-
	// discovered target — handled on-demand at /call time, no global
	// fallback per design §22.1.A item #2). The bool reports whether the
	// result was index-scoped (telemetry).
	targetsFor := func(gvr schema.GroupVersionResource, haveGVR bool) ([]seedTarget, bool) {
		if !haveGVR {
			return nil, false
		}
		raw := enumeratePrewarmTargetsForGVRFn(gvr, "list")
		if len(raw) == 0 {
			return nil, false
		}
		out := make([]seedTarget, 0, len(raw))
		for _, t := range raw {
			out = append(out, seedTarget{
				BindingUID: t.BindingUID,
				Username:   t.Subject.Username,
				Groups:     append([]string(nil), t.Subject.Groups...),
			})
		}
		return out, true
	}

	// seedOneTarget runs one (target, layer) seed under a per-target
	// timeout (pipCohortTimeout — matches seedCohort's stuck-cohort
	// guard) and the cohort seed context. The per-target seed primitive
	// (seedOneRestaction / seedOneWidget) is passed as a closure so the
	// restaction + widget loops share the timeout + yield + error-
	// containment wrapper.
	seedOneTarget := func(c seedTarget, do func(cohortCtx context.Context) error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		engineYieldCheckpoint(ctx)
		cctx, cancel := context.WithTimeout(ctx, pipCohortTimeout)
		defer cancel()
		cohortCtx := withCohortSeedContext(cctx, c, saEP, saRC)
		return do(cohortCtx)
	}

	// ── RESTActions seed — per-binding targets scoped on each RA's TARGET GVR.
	for _, ref := range restactionRefs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		engineYieldCheckpoint(ctx)

		targetGVR, haveTarget := restActionTargetGVR(ctx, ref)
		targets, scoped := targetsFor(targetGVR, haveTarget)
		log.Info("prewarm.engine.seed.restaction_targets",
			slog.String("subsystem", "cache"),
			slog.String("restaction", ref.Namespace+"/"+ref.Name),
			slog.String("target_gvr", targetGVR.String()),
			slog.Bool("scoped", scoped),
			slog.Int("targets", len(targets)),
		)
		for _, c := range targets {
			err := seedOneTarget(c, func(cohortCtx context.Context) error {
				return seedOneRestaction(cohortCtx, cohortLogLabel(c), ref, authnNS)
			})
			if err != nil && ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				// #158 — classify (was: blanket Warn, no counter).
				classifyEngineSeedErr("restaction", ref.Namespace+"/"+ref.Name, cohortLogLabel(c), err)
			}
		}
	}

	// ── Widgets seed — per-binding targets scoped on each widget's GVR.
	for _, e := range widgetEntries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		engineYieldCheckpoint(ctx)

		targets, scoped := targetsFor(e.GVR, true)
		log.Info("prewarm.engine.seed.widget_targets",
			slog.String("subsystem", "cache"),
			slog.String("widget", e.W.GetNamespace()+"/"+e.W.GetName()),
			slog.String("gvr", e.GVR.String()),
			slog.Bool("scoped", scoped),
			slog.Int("targets", len(targets)),
		)
		for _, c := range targets {
			err := seedOneTarget(c, func(cohortCtx context.Context) error {
				return seedOneWidgetFn(cohortCtx, e, authnNS)
			})
			if err != nil && ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				// #158 — classify (was: blanket Warn, no counter).
				classifyEngineSeedErr("widget", e.W.GetNamespace()+"/"+e.W.GetName(), cohortLogLabel(c), err)
			}
		}
	}
	return nil
}

// unionProactiveRARefs unions the RBAC-reachable RESTAction refs
// (cache.RBACReachableRestActionRefs — Option A: the RESTAction CRs some
// published binding grants `get` on, intersected with the boot-anchored
// RESTActions informer) into the nav-walk harvester snapshot, deduped by
// {ns,name} EXACTLY as the harvester dedups (phase1_content_prewarm.go).
//
// The proactive refs carry the SAME fixed RESTAction GVR coordinates the
// harvester writes (restActionGVR group/version/resource), so the
// downstream seedScopeYielding loop + objects.Get treat them identically.
//
// SEED-TARGETING ONLY (no special-case): the source is purely
// RBAC-derived (zero resource/name/path literal). Over-inclusion = wasted
// seed (benign); the per-request authz boundary is unchanged.
func unionProactiveRARefs(harvested []templatesv1.ObjectReference,
	rw *cache.ResourceWatcher, log *slog.Logger) []templatesv1.ObjectReference {

	proactive := cache.RBACReachableRestActionRefs(rw)
	if len(proactive) == 0 {
		log.Info("prewarm.engine.boot.proactive_ra_seed",
			slog.String("subsystem", "cache"),
			slog.Int("harvested", len(harvested)),
			slog.Int("proactive_reachable", 0),
			slog.Int("added", 0),
			slog.String("note", "no RBAC-reachable RESTAction refs (no binding grants get on restactions, or informer empty) — seed source unchanged"),
		)
		return harvested
	}

	out := unionRefsForTest(harvested, proactive)

	log.Info("prewarm.engine.boot.proactive_ra_seed",
		slog.String("subsystem", "cache"),
		slog.Int("harvested", len(harvested)),
		slog.Int("proactive_reachable", len(proactive)),
		slog.Int("added", len(out)-len(harvested)),
		slog.String("note", "RBAC-reachable RESTAction refs unioned into the boot seed source (Option A); the per-composition detail RESTActions the nav walk never reaches are now seeded"),
	)
	return out
}

// unionRefsForTest is the pure dedup core of unionProactiveRARefs — it
// appends the proactive refs (built as RESTAction ObjectReferences with the
// fixed GVR coordinates the harvester uses) onto the harvested slice, deduped
// by {ns,name} EXACTLY as the harvester dedups
// (phase1_content_prewarm.go). RBAC-source-independent so a falsifier can
// exercise the dedup without a live cluster. Named *ForTest because the
// falsifier is its only direct external caller; production reaches it through
// unionProactiveRARefs.
func unionRefsForTest(harvested []templatesv1.ObjectReference,
	proactive []cache.RestActionRef) []templatesv1.ObjectReference {

	seen := make(map[string]struct{}, len(harvested)+len(proactive))
	out := make([]templatesv1.ObjectReference, 0, len(harvested)+len(proactive))
	for _, r := range harvested {
		key := r.Namespace + "/" + r.Name
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, r)
	}
	for _, p := range proactive {
		key := p.Namespace + "/" + p.Name
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, templatesv1.ObjectReference{
			Reference:  templatesv1.Reference{Name: p.Name, Namespace: p.Namespace},
			APIVersion: restActionGVR.Group + "/" + restActionGVR.Version,
			Resource:   restActionGVR.Resource,
		})
	}
	return out
}

// restActionTargetGVR derives the GVR a RESTAction LISTs from its
// userAccessFilter (verb/group/resource) — the no-special-case signal of
// what GVR the RA gates LIST on (e.g. compositions-panels' RA has
// userAccessFilter:{group:composition.krateo.io, resource:compositions}).
// Returns (gvr, true) when a static {group, resource} is declared on any
// api stanza; (zero, false) for a runtime-discovered target
// (ResourcesFrom — no static literal) or no userAccessFilter (the caller
// falls back to the global cohort set).
//
// The version is left "" — the BindingsByGVR index keys on {group,
// resource} (RBAC rules carry no version), so the version is irrelevant
// to cohort scoping.
func restActionTargetGVR(ctx context.Context, ref templatesv1.ObjectReference) (schema.GroupVersionResource, bool) {
	got := objects.Get(ctx, ref)
	if got.Err != nil || got.Unstructured == nil {
		return schema.GroupVersionResource{}, false
	}
	var cr templatesv1.RESTAction
	if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(got.Unstructured.Object, &cr); err != nil {
		return schema.GroupVersionResource{}, false
	}
	// Pick the first api stanza declaring a static userAccessFilter
	// resource. Multiple stanzas would each have their own target; the
	// dominant LIST target (the one the apiRef/RAFullList layer bounds on)
	// is the one carrying a userAccessFilter — they share the cohort set
	// because cohorts union over the bindings, and the index is scoped per
	// {group,resource}. Deterministic: sort stanzas by name first.
	apis := append([]*templatesv1.API(nil), cr.Spec.API...)
	sort.Slice(apis, func(i, j int) bool { return apis[i].Name < apis[j].Name })
	for _, a := range apis {
		if a == nil || a.UserAccessFilter == nil {
			continue
		}
		if a.UserAccessFilter.Resource == "" {
			// Runtime-discovered (ResourcesFrom) — no static literal to
			// scope on. Fall back to the global cohort set.
			continue
		}
		return schema.GroupVersionResource{
			Group:    a.UserAccessFilter.Group,
			Resource: a.UserAccessFilter.Resource,
		}, true
	}
	return schema.GroupVersionResource{}, false
}
