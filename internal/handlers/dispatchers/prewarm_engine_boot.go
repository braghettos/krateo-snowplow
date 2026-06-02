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
// scope; Ship 1 enqueues only scopeKindBoot.
func makeBootScopeHandler(deps rePrewarmDeps) func(ctx context.Context, s prewarmScope) error {
	return func(ctx context.Context, s prewarmScope) error {
		switch s.kind {
		case scopeKindBoot:
			return rePrewarmBoot(ctx, deps)
		default:
			// Ship 2 scopes land here. Ship 1 has none.
			slog.Warn("prewarm.engine.unknown_scope",
				slog.String("subsystem", "cache"),
				slog.String("scope", s.key()),
			)
			return nil
		}
	}
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
// restactions + widgets, PER-TARGET-GVR-scoped cohorts, yielding to
// customer /call between cohorts. Replaces runPIPSeed's global single
// EnumerateBindingSetClasses() cohort source with the resource-driven
// EnumerateResourceCohorts(targetGVR) per harvested target.
//
// The two loops are SYMMETRIC:
//   - widget loop: cohorts scoped on the widget's GVR (e.namWidgetEntry.GVR).
//   - restaction loop: cohorts scoped on the RESTAction's TARGET GVR (the
//     GVR it LISTs, derived from the RA's userAccessFilter). So the
//     apiRef/RAFullList layer is bounded identically (~3-6 not 34).
//
// FALLBACK — when EnumerateResourceCohorts(gvr) yields nothing (index not
// built, or a GVR with no matching bindings, or a runtime-discovered
// target with no static resource) the loop falls back to the global
// EnumerateBindingSetClasses() for that target (safe over-inclusion;
// under-inclusion would be a cold first /call).
func seedScopeYielding(ctx context.Context,
	restactionRefs []templatesv1.ObjectReference, widgetEntries []navWidgetEntry,
	saEP endpoints.Endpoint, saRC *rest.Config, authnNS string) error {

	log := slog.Default()

	// Ship 0.30.241 — `globalCohorts` + `cohortsFor` + `seedTarget` helpers
	// removed. Under the v4 identity-free L1 key contract there is NO per-
	// cohort cell to enumerate — all cohorts share the same key per RA
	// tuple. The resource-driven cohort scoping below was sound under the
	// v3 per-cohort key shape but is dead weight under v4.

	// Ship 0.30.241 — SA-UNIFORM SEED (architect D.3 §7 fix; closes the
	// 5-ship L1-miss-after-CRUD defect chain).
	//
	// V4 contract (design 2026-06-02 §5): ResolvedKeyInputs is identity-
	// free. ComputeKey produces the SAME L1 key for the same (CEC, GVR,
	// ns, name, perPage, page, extras, stage) tuple REGARDLESS of which
	// cohort dispatches. The v3 inner-cohort iteration that lived here
	// pre-0.30.241 emitted N_cohort identical Puts per RA — same key,
	// last-writer-wins, pure waste.
	//
	// Concrete cost of the inner loop (measured on 0.30.240 prod):
	//   - 21 RAs × 35 cohorts = 735 seed invocations.
	//   - Each seedOneRestaction wall-clock ≈ 5s (per phase1.seed.restaction.
	//     timing on production).
	//   - Total seed wall-clock: 735 × 5s / GOMAXPROCS yielding = ~1 hr
	//     until ALL cohort × RA tuples Put. The verify-serve-stale gate
	//     fires WELL before that — D.1.B empirically witnessed 0.30.235
	//     fresh-boot AND 0.30.240 both FAIL the gate at ~2 min post-boot
	//     because `krateo-system/compositions-list` hadn't been reached
	//     in the inner loop yet.
	//
	// V4 seed (this fix): ONE seedOneRestaction per RA under SA identity.
	//   - 21 RAs × 1 SA call = 21 seed invocations.
	//   - 21 × 5s / GOMAXPROCS yielding = ~25 s wall-clock to seed every
	//     harvested cell. The 5-ship verify-serve-stale gate is empirically
	//     PASSable.
	//
	// SA identity matches the v4 refresher's SA-uniform contract at
	// resolve_populate.go:164-174 — the refresher uniformly re-resolves
	// every v4 cell under SA. The seed populating under SA produces a
	// cell the refresher's SA refresh produces byte-identically; the cell
	// stays valid across refresh cycles without identity re-keying.
	//
	// PER-USER NARROWING moves entirely to SERVE time per design §4:
	// gateWidgetsServeBytes / gateRestactionsServeBytes / gateRAFullList-
	// ServeBytes (serve_gate.go). The cached cell is SA-maximal; each
	// /call narrows on the wire to the request's identity.
	//
	// PROCESS-LEVEL ARCHITECT MANDATE (D.3 §10): every ship touching
	// ResolvedKeyInputs / dispatchCacheLookupKey / withCohortSeedContext /
	// resolveAndPopulateL1 / seedOne* / resolved.go Put|ComputeKey MUST
	// include the seed-coverage falsifier shape from
	// seed_coverage_falsifier_test.go (TestBootSeedCoverage_AllHarvestedRAsSeeded
	// + TestBootSeedCoverage_V4CellCollapseOneRAAcrossCohorts). The
	// falsifier mechanically prevents the v3-style key explosion from
	// re-emerging.

	// ── RESTActions seed — ONE Put per RA under SA identity (v4 contract).
	//
	// SEAM: production calls go through seedOneRestactionFn /
	// withPhase1SAContextFn (see prewarm_engine_boot_test_seam.go).
	// Production binary's defaults are the real functions; tests swap
	// via setXxxForTest helpers. Function-pointer overhead is ~1 ns
	// per call — irrelevant vs the seed's per-RA ~5s wall-clock.
	for _, ref := range restactionRefs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		engineYieldCheckpoint(ctx)

		log.Info("prewarm.engine.seed.restaction_sa_uniform",
			slog.String("subsystem", "cache"),
			slog.String("restaction", ref.Namespace+"/"+ref.Name),
		)

		cctx, cancel := context.WithTimeout(ctx, pipCohortTimeout)
		saCtx := withPhase1SAContextFn(cctx, saEP, saRC)
		if err := seedOneRestactionFn(saCtx, "sa-uniform", ref, authnNS); err != nil {
			if ctx.Err() != nil {
				cancel()
				return ctx.Err()
			}
			slog.Warn("prewarm.engine.seed.restaction_skipped",
				slog.String("subsystem", "cache"),
				slog.String("cohort", "sa-uniform"),
				slog.String("restaction", ref.Namespace+"/"+ref.Name),
				slog.Any("err", err),
			)
		}
		cancel()
	}

	// ── Widgets seed — ONE Put per widget under SA identity (v4 contract).
	for _, e := range widgetEntries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		engineYieldCheckpoint(ctx)

		log.Info("prewarm.engine.seed.widget_sa_uniform",
			slog.String("subsystem", "cache"),
			slog.String("widget", e.W.GetNamespace()+"/"+e.W.GetName()),
			slog.String("gvr", e.GVR.String()),
		)

		cctx, cancel := context.WithTimeout(ctx, pipCohortTimeout)
		saCtx := withPhase1SAContextFn(cctx, saEP, saRC)
		if err := seedOneWidgetFn(saCtx, e, authnNS); err != nil {
			if ctx.Err() != nil {
				cancel()
				return ctx.Err()
			}
			slog.Warn("prewarm.engine.seed.widget_skipped",
				slog.String("subsystem", "cache"),
				slog.String("cohort", "sa-uniform"),
				slog.String("widget", e.W.GetNamespace()+"/"+e.W.GetName()),
				slog.Any("err", err),
			)
		}
		cancel()
	}
	return nil
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
