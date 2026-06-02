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
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/krateoplatformops/plumbing/endpoints"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"golang.org/x/sync/errgroup"
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
	if err := seedScopeParallel(rctx, restactionRefs, widgetEntries, deps.saEP, deps.saRC, deps.authnNS); err != nil {
		return err
	}

	log.Info("prewarm.engine.boot.complete",
		slog.String("subsystem", "cache"),
		slog.Int64("elapsed_ms", time.Since(start).Milliseconds()),
	)
	return nil
}

// seedScopeParallel seeds the per-user top-level L1 for the harvested
// restactions + widgets, PER-TARGET-GVR-scoped cohorts, COHORT-PARALLEL
// with serial RA/widget iteration inside each cohort. Replaces the pre-
// 0.30.237 serial seedScopeYielding (outer-RA × inner-cohort) shape that
// silently regressed the legacy runPIPSeed parallelism (phase1_pip_seed.go:
// 382-387 — errgroup.SetLimit(GOMAXPROCS), cohort-parallel goroutines)
// and produced only ~0.68 puts/s wall-clock at 5K scale, missing Gate C.
//
// Ship 0.30.237 Stage 1 — cohort-parallel restoration. The shape matches
// runPIPSeed line-by-line (golang.org/x/sync/errgroup, GOMAXPROCS limit,
// for-range over cohorts schedules one g.Go per cohort). Widget/RA work
// remains serial INSIDE the cohort to bound transient resident bytes per
// cohort (envelope-bytes per (RA+widget) × cohort-count is unchanged) and
// to mirror the legacy seedCohort body's iteration order.
//
// The two inner loops are SYMMETRIC:
//   - widget loop: cohorts scoped on the widget's GVR (e.GVR).
//   - restaction loop: cohorts scoped on the RESTAction's TARGET GVR (the
//     GVR it LISTs, derived from the RA's userAccessFilter). So the
//     apiRef/RAFullList layer is bounded identically (~3-6 not 34).
//
// FALLBACK — when EnumerateResourceCohorts(gvr) yields nothing (index not
// built, or a GVR with no matching bindings, or a runtime-discovered
// target with no static resource) the plan falls back to the global
// EnumerateBindingSetClasses() for that target (safe over-inclusion;
// under-inclusion would be a cold first /call).
//
// CUSTOMER PRIORITY (feedback_customer_priority_over_refresher). The
// engineYieldCheckpoint is invoked:
//   - BEFORE scheduling each new cohort goroutine (in the outer for-range),
//     so a customer burst arriving DURING boot defers UNSCHEDULED cohorts.
//   - BETWEEN each (RA, widget) unit INSIDE a cohort, so an in-flight
//     cohort goroutine also steps aside between units.
//
// In-flight cohort goroutines run to completion OR until their 120s
// per-cohort timeout (pipCohortTimeout). Worst-case in-flight CPU
// contention is GOMAXPROCS background seed goroutines vs customer
// goroutines — the same shape the refresher precedent accepts.
func seedScopeParallel(ctx context.Context,
	restactionRefs []templatesv1.ObjectReference, widgetEntries []navWidgetEntry,
	saEP endpoints.Endpoint, saRC *rest.Config, authnNS string) error {

	log := slog.Default()

	// (1) Build per-cohort work plans up front. This inverts the per-RA
	// and per-widget cohort-scoping into a (cohort → [RAs], [widgets])
	// view so each goroutine drains one cohort's units serially.
	plans, planOrder, plannedUnits := buildCohortPlans(ctx, restactionRefs, widgetEntries)

	// Lever C — publish planned-units BEFORE g.Go schedules, so the
	// operator can read /debug/vars and project ETA before any work
	// completes. Atomic-Store is the single-writer pattern (boot scope
	// runs once per pod lifetime).
	pipSeedUnitsPlannedTotal.Store(uint64(plannedUnits))

	if len(plans) == 0 {
		log.Info("prewarm.engine.seed.no_plans",
			slog.String("subsystem", "cache"),
			slog.Int("restactions", len(restactionRefs)),
			slog.Int("widgets", len(widgetEntries)),
		)
		return nil
	}

	log.Info("prewarm.engine.seed.scope_start",
		slog.String("subsystem", "cache"),
		slog.Int("cohorts", len(plans)),
		slog.Int("restactions", len(restactionRefs)),
		slog.Int("widgets", len(widgetEntries)),
		slog.Int("planned_units", plannedUnits),
		slog.Int("parallelism", runtime.GOMAXPROCS(0)),
	)

	// (2) Bounded errgroup at GOMAXPROCS — restores the legacy
	// phase1_pip_seed.go:382-387 shape. limit < 1 guard mirrors the
	// runPIPSeed clamp.
	g, gctx := errgroup.WithContext(ctx)
	limit := runtime.GOMAXPROCS(0)
	if limit < 1 {
		limit = 1
	}
	g.SetLimit(limit)

	// Iterate planOrder (sorted) so cohort scheduling is deterministic
	// across pod restarts — matches the EnumerateRBACCohorts log-stability
	// guarantee.
	for _, key := range planOrder {
		plan := plans[key]
		cohort := plan.cohort // pin loop variable

		// CUSTOMER PRIORITY — yield BEFORE scheduling a new cohort
		// goroutine. In-flight cohorts complete on their own
		// pipCohortTimeout cap; new cohorts wait while the burst clears.
		engineYieldCheckpoint(gctx)
		if gctx.Err() != nil {
			break
		}

		cohortPlan := plan
		g.Go(func() error {
			// Per-cohort 120s deadline — matches pipCohortTimeout legacy
			// cap. A stuck cohort cannot wedge the pool.
			cctx, ccancel := context.WithTimeout(gctx, pipCohortTimeout)
			defer ccancel()
			cohortCtx := withCohortSeedContext(cctx, cohort, saEP, saRC)
			label := cohortLogLabel(cohort)
			cohortStart := time.Now()

			// RESTActions seed for THIS cohort — serial inside the goroutine.
			raDone := 0
			for _, ref := range cohortPlan.restactionRefs {
				if cohortCtx.Err() != nil {
					// Non-fatal — cohort timeout fires, other cohorts continue.
					return nil
				}
				engineYieldCheckpoint(cohortCtx)
				if err := seedOneRestaction(cohortCtx, label, ref, authnNS); err != nil {
					slog.Warn("prewarm.engine.seed.restaction_skipped",
						slog.String("subsystem", "cache"),
						slog.String("cohort", label),
						slog.String("restaction", ref.Namespace+"/"+ref.Name),
						slog.Any("err", err),
					)
					continue
				}
				// Ship 0.30.236 telemetry — engine path bumps the per-engine
				// counters (the legacy seedCohort loop only fires when the
				// engine is disabled). Required so /debug/vars reflects "did
				// the seed run?" on the engine code path.
				pipSeedRestactionsTotal.Add(1)
				incCohortCounter(&pipSeedRestactionsByCohort, label)
				raDone++
			}

			// Widgets seed for THIS cohort — serial inside the goroutine.
			widgetDone := 0
			for _, e := range cohortPlan.widgetEntries {
				if cohortCtx.Err() != nil {
					return nil
				}
				engineYieldCheckpoint(cohortCtx)
				if err := seedOneWidget(cohortCtx, e, authnNS); err != nil {
					slog.Warn("prewarm.engine.seed.widget_skipped",
						slog.String("subsystem", "cache"),
						slog.String("cohort", label),
						slog.String("widget", e.W.GetNamespace()+"/"+e.W.GetName()),
						slog.Any("err", err),
					)
					continue
				}
				pipSeedWidgetsTotal.Add(1)
				incCohortCounter(&pipSeedWidgetsByCohort, label)
				widgetDone++
			}

			// Lever B — per-cohort progress checkpoint. Lets an operator
			// reading kubectl logs mid-boot project ETA per cohort and tell
			// "slow but progressing" from "stuck".
			slog.Info("prewarm.engine.seed.cohort_progress",
				slog.String("subsystem", "cache"),
				slog.String("cohort", label),
				slog.Int("restactions_done", raDone),
				slog.Int("restactions_planned", len(cohortPlan.restactionRefs)),
				slog.Int("widgets_done", widgetDone),
				slog.Int("widgets_planned", len(cohortPlan.widgetEntries)),
				slog.Int64("elapsed_ms", time.Since(cohortStart).Milliseconds()),
			)
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return err
	}
	return ctx.Err()
}

// cohortPlan is the per-cohort work plan built by buildCohortPlans —
// the set of restactions + widgets to seed for one cohort. The cohort
// field carries the original cache.Cohort so the goroutine can pass it
// to withCohortSeedContext without re-resolving from the map key.
type cohortPlan struct {
	cohort         cache.Cohort
	restactionRefs []templatesv1.ObjectReference
	widgetEntries  []navWidgetEntry
}

// buildCohortPlans inverts (RA → cohorts) and (widget → cohorts) into
// (cohort → [RAs], [widgets]) using the SAME per-target-GVR scoping
// rules as the legacy seedScopeYielding (cohortsFor with global
// fallback). Returns the plan map, a deterministic iteration order, and
// the total planned-unit count (Σ per-cohort (RAs + widgets) for Lever C).
//
// Map keying — Cohort has Username + Groups []string; Go maps cannot
// key on a slice. Mitigation: build a canonical string key
// "username|<sorted groups joined by ,>" and use map[string]*cohortPlan;
// the *cohortPlan carries the original cache.Cohort so the goroutine
// resolves identity without re-decoding the key.
func buildCohortPlans(ctx context.Context,
	restactionRefs []templatesv1.ObjectReference,
	widgetEntries []navWidgetEntry,
) (plans map[string]*cohortPlan, order []string, plannedUnits int) {

	plans = map[string]*cohortPlan{}

	cohortKey := func(c cache.Cohort) string {
		// Sort Groups for stable key — EnumerateRBACCohorts sorts
		// usernames but groups within a cohort may not be sorted.
		gs := append([]string(nil), c.Groups...)
		sort.Strings(gs)
		return c.Username + "|" + strings.Join(gs, ",")
	}

	addRA := func(c cache.Cohort, ref templatesv1.ObjectReference) {
		k := cohortKey(c)
		p, ok := plans[k]
		if !ok {
			p = &cohortPlan{cohort: c}
			plans[k] = p
		}
		p.restactionRefs = append(p.restactionRefs, ref)
	}
	addWidget := func(c cache.Cohort, e navWidgetEntry) {
		k := cohortKey(c)
		p, ok := plans[k]
		if !ok {
			p = &cohortPlan{cohort: c}
			plans[k] = p
		}
		p.widgetEntries = append(p.widgetEntries, e)
	}

	// Global fallback cohort set — computed once, reused for any target
	// the index can't scope. Empty when no snapshot is published.
	globalCohorts := cache.EnumerateBindingSetClasses()

	// Ship 0.30.238 Component A — cohort-set UNION (was fallback-only).
	//
	// Defect: for widget GVRs, EnumerateResourceCohorts(gvr) returns only
	// the K8s-RBAC-bound subjects (cyberjoker + every UAF-only user is
	// structurally excluded — UAF is a snowplow-internal layer, not a K8s
	// (Cluster)RoleBinding, so the BindingsByGVR index does not see it).
	// The pre-0.30.238 code took the rc-non-empty branch unconditionally
	// and never reached globalCohorts → UAF-only users were never seeded.
	//
	// Fix: union the per-GVR scoped set with the global EnumerateBindingSet
	// Classes() snapshot. unionCohorts is symmetric, dedup'd on cohortKey
	// (Username + sorted Groups), preserves the fallback semantics when
	// either input is empty, and is mechanism-uniform (no per-user / per-
	// GVR special-cases). See docs/ship-0.30.238-stage-2-crud-triggered-re-
	// prewarm-2026-06-02.md §4.1.
	cohortsFor := func(gvr schema.GroupVersionResource, haveGVR bool) []cache.Cohort {
		var rc []cache.Cohort
		if haveGVR {
			rc = cache.EnumerateResourceCohorts(gvr)
		}
		unioned := unionCohorts(rc, globalCohorts)
		// Per-call instrumentation — design §4.4.1. One log line per
		// (target-GVR × call site) at boot (~3,700 lines for a full union,
		// single boot pass, then never again). Lets the post-deploy
		// falsifier observe the union directly via `kubectl logs … | grep
		// prewarm.engine.cohortsFor` without inferring from /debug/vars.
		slog.Info("prewarm.engine.cohortsFor",
			slog.String("subsystem", "cache"),
			slog.String("gvr", gvr.String()),
			slog.Bool("have_gvr", haveGVR),
			slog.Int("rc_count", len(rc)),
			slog.Int("global_count", len(globalCohorts)),
			slog.Int("unioned_count", len(unioned)),
		)
		return unioned
	}

	for _, ref := range restactionRefs {
		targetGVR, haveTarget := restActionTargetGVR(ctx, ref)
		cohorts := cohortsFor(targetGVR, haveTarget)
		for _, c := range cohorts {
			addRA(c, ref)
		}
	}
	for _, e := range widgetEntries {
		cohorts := cohortsFor(e.GVR, true)
		for _, c := range cohorts {
			addWidget(c, e)
		}
	}

	// Deterministic iteration order — sort keys so cohort scheduling
	// matches across pod restarts (mirrors EnumerateRBACCohorts's
	// stability guarantee).
	order = make([]string, 0, len(plans))
	for k, p := range plans {
		order = append(order, k)
		plannedUnits += len(p.restactionRefs) + len(p.widgetEntries)
	}
	sort.Strings(order)
	return plans, order, plannedUnits
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

// unionCohorts returns a ∪ b dedup'd by canonical cohort key (Username +
// sorted Groups, joined by '|' and ','). Preserves the fallback semantics
// of the pre-0.30.238 cohortsFor: when either input is empty the other is
// returned UNCHANGED (no copy, no dedup pass). Order is deterministic:
// elements of a first (in input order), then elements of b not already
// keyed.
//
// Used by Ship 0.30.238 Component A — see docs/ship-0.30.238-stage-2-
// crud-triggered-re-prewarm-2026-06-02.md §4.1. The cohortKey shape is
// IDENTICAL to buildCohortPlans's cohortKey closure so the union dedup
// matches the plan map dedup downstream — sentinel-prefixed group-only
// cohorts and real-user-carrying-groups remain DISTINCT identities
// (Username is part of the key).
//
// Cost: O(|a| + |b|) — single allocation of the seen set + the out slice.
// Boot scope only — runs once per pod lifetime.
func unionCohorts(a, b []cache.Cohort) []cache.Cohort {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	key := func(c cache.Cohort) string {
		gs := append([]string(nil), c.Groups...)
		sort.Strings(gs)
		return c.Username + "|" + strings.Join(gs, ",")
	}
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]cache.Cohort, 0, len(a)+len(b))
	for _, c := range a {
		k := key(c)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, c)
	}
	for _, c := range b {
		k := key(c)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, c)
	}
	return out
}
