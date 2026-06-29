// phase1_pip_seed.go — Ship PIP (0.30.173): the per-identity prewarm
// seed of restactions + widgets top-level L1.
//
// THE NORTH-STAR DEFECT THIS SHIPS AGAINST. After phase1Done=true at
// 0.30.172, admin's first compositions-list /call is l1_hit:"miss" and
// takes ~13.8 s — the per-USER resolved-output L1 (top-level
// restactions / widgets cache classes) is cold for every cohort. F2
// (Ship F2 / 0.30.125) populates only the IDENTITY-FREE apistage
// content L1; the per-user envelope above it is still resolved on the
// first hot path. PIP fills that gap: BEFORE phase1Done flips, seed the
// top-level L1 once per (RBAC cohort, restaction) AND once per (RBAC
// cohort, widget) reached by the Phase-1 walker. The first /call by
// every cohort then returns dispatcher.call.complete l1_hit:"hit" with
// zero resolve.
//
// COHORT ENUMERATION. The cohort set is derived from
// cache.EnumerateBindingSetClasses() — see
// internal/cache/binding_set_enumeration.go for the canonical-dedupe
// contract (two identities are the same cohort iff their union of matched
// binding-pointer-sets is equal).
//
// Ship 2 / 0.30.196 — COHORT-COUNT-INDEPENDENT. The old cohort cap (50)
// + the cohort_cap_exceeded fail-closed branch are DELETED. The product
// owner's invariant: the cache architecture must NOT depend on cohort
// count — a future customer with per-user User-kind bindings could push
// cohort count to O(users), and that must never wedge the pod. Each
// cohort × (N_restactions+N_widgets) is still an L1 entry, but the seed
// is now a BACKGROUND best-effort warm: a large cohort set simply takes
// longer to warm in the background; it never withholds readiness.
//
// BACKGROUND + BEST-EFFORT (Ship 2 / 0.30.196 — supersedes the prior
// FOREGROUND + FAIL-CLOSED contract). phase1WarmupWith launches runPIPSeed
// as Step 7.6 on a bounded background goroutine AFTER MarkPhase1Done
// (Step 8) — readiness is already 200. Any cohort-level error is log-only;
// it NEVER withholds readiness and NEVER fail-closes. The background seed
// is bounded by pipGlobalTimeout and dies with the process. The first
// /call by a not-yet-seeded cohort falls back to a per-user resolve — the
// correct degraded posture, since the architecture gate proved the cold
// nav is served from the informer substrate regardless of the seed.
//
// CONCURRENCY (architect's design §3). The cohort loop runs under a
// bounded errgroup with limit = runtime.GOMAXPROCS(0) — matches the F2
// content-warm's bounded fan-out shape. Each cohort's seed is
// SEQUENTIAL inside the goroutine: it iterates the harvested
// (restaction, widget) sets one at a time. The bound on transient RSS
// per cohort is N_restactions×envelope_bytes + N_widgets×envelope_bytes
// — same OOM profile as the F2 content pass per cohort.
//
// PER-COHORT TIMEOUT (restored 0.30.191 SCOPE CORRECTION). Set via
// context.WithTimeout inside the per-cohort closure. A stuck cohort
// thus cannot wedge Phase 1 past Step 7.6's global budget; the timeout
// firing returns ctx.Err() up the errgroup which propagates as the
// cohort's seed-failure path. The 0.30.190 proportional-timeout model
// (computeCohortTimeout) was REVERTED at 0.30.191: it was an INFERENCE
// from a file header comment ("1.5s/widget × 132 widgets = 198s"), not
// a measurement. Per feedback_data_driven_workflow +
// feedback_empirical_root_cause_trace_before_fix we are NOT raising the
// ceiling until 0.30.191 instrumentation tells us which abort cause
// actually fires for the 0.30.189 sentinel cohort. The 120s fixed
// ceiling is the 0.30.179 value.
//
// FEEDBACK_CHECK_K8S_CLIENTGO_PRIOR_ART: client-go has no equivalent
// for per-RBAC-cohort prewarm. RBAC subject enum is a custom snowplow
// concern (no upstream evaluator exists in client-go; rbac/v1
// authorizer evaluates one request at a time). PIP's cohort
// enumeration is therefore a custom mechanism.
//
// FEEDBACK_NO_SPECIAL_CASES: no hardcoded admin / hardcoded user / no
// resource-name literals. The cohort list is derived purely from the
// published RBAC snapshot; the restaction set is the same harvester
// the F2 content pass drains; the widget set is the new
// navWidgetHarvester populated by the existing walk. Every identifier
// flows from the cluster state, not from Go literals.
//
// FEEDBACK_RESTACTION_NO_WIDGET_LOGIC + FEEDBACK_L1_PER_USER_KEYED_
// NEVER_COHORT: PIP keys every L1 entry per-user via the dispatcher's
// canonical dispatchCacheLookupKey (helpers.go) under a ctx whose
// xcontext.UserInfo carries the cohort's Username + Groups. No cohort
// cross-leak path exists — the cache layer SEES per-user keys; PIP just
// pre-populates one entry per cohort.

package dispatchers

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/snowplow/apis"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/handlers/util"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"github.com/krateoplatformops/snowplow/internal/resolvers/restactions"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets/apiref"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

const (
	// envPrewarmPIPEnabled is the Ship PIP (0.30.173) opt-in gate.
	// Chart default is true (active by default for this ship); operators
	// may set "false" to disable the seed if a regression is observed.
	// PIP additionally requires prewarm-on (#57: implicit-on-cache, i.e.
	// the cache subsystem is on) + PREWARM_CONTENT_ENABLED (the
	// apiRefHarvester depends on the content-prewarm path) — when either
	// is off, PIP stays inert regardless of this knob.
	envPrewarmPIPEnabled = "PREWARM_PIP_ENABLED"

	// Ship 2 / 0.30.196 — the PER-COHORT CAP IS DELETED. pipCohortCapDefault
	// (50), envPrewarmPIPCohortCap ("PREWARM_PIP_COHORT_CAP"), and
	// pipCohortCap() are GONE. The cap was the not-Ready-forever landmine:
	// at cohort #51 runPIPSeed returned a fatal `cohort_cap_exceeded` error
	// and phase1WarmupWith fail-closed (/readyz 503 FOREVER). With readiness
	// now decoupled from the per-cohort seed (the seed runs as a background
	// best-effort warm AFTER MarkPhase1Done — see phase1_walk.go Step 7.6),
	// there is NO storage rationale to fail closed on cohort count, and an
	// O(users)-cohort topology (per-user User-kind bindings) must not wedge
	// the pod. The cap is the forbidden unbounded-cohort landmine and is
	// removed entirely.

	// pipCohortTimeout is the per-cohort hard ceiling. A stuck cohort
	// cannot wedge Phase 1 past Step 7.6's global budget.
	//
	// Ship A.3 / 0.30.179 — raised 20s -> 120s. Binding-set enumeration
	// produces more classes than the prior canonical-cohort dedupe, and
	// each class's restactions seed walks per-namespace LIST calls (a
	// compositions-list RESTAction emits one K8s call per namespace via
	// the namespace iterator). A 50-namespace cluster needs ~30s per
	// cohort to seed cleanly; 120s adds ~4x headroom.
	//
	// Ship 0.30.191 SCOPE CORRECTION restored this fixed value from the
	// 0.30.190 proportional-timeout model (computeCohortTimeout). The
	// 0.30.190 raise was an INFERENCE from a file header comment, not a
	// measurement of the actual 0.30.189 sentinel-cohort abort cause —
	// 0.30.191 ships the instrumentation that will tell us empirically
	// which abort cause fires before any further timeout change.
	pipCohortTimeout = 120 * time.Second

	// pipGlobalTimeout is the absolute Step 7.6 budget. Designed to fit
	// the architect's pod-start→phase1Done projection (baseline + seed
	// ceiling). Ship A.3 / 0.30.179 — raised 40s -> 8 minutes per the
	// PM gate's "baseline + 8 min seed ceiling" target. The per-cohort
	// timeout × cohort cap caps the total at 50 × 120 s = 6000 s but the
	// parallelism + harvest dedup keep the empirical wall-clock well
	// inside 8 min.
	pipGlobalTimeout = 8 * time.Minute
)

// PrewarmPIPEnabled reports whether the Ship PIP per-identity prewarm
// seed is opted in. Defaults FALSE as of 0.30.176 (Phase A.1): the
// PIP seed is opt-in via PREWARM_PIP_ENABLED=true.
func PrewarmPIPEnabled() bool {
	v := env.String(envPrewarmPIPEnabled, "false")
	return v == "true"
}

// navWidgetEntry is one navigation widget CR captured during the
// Phase-1 walk together with the GVR + pagination tuple it resolved
// under. The seed loop re-resolves the SAME CR per cohort under per-
// cohort identity and Puts the per-user widgets L1 entry.
//
// Ship 0.30.187 D2: the RESOLUTION tuple (PerPage, Page — passed to
// widgets.Resolve) is DECOUPLED from the dispatcher-lookup KEY tuple
// (KeyPerPage, KeyPage — passed to dispatchCacheLookupKey). Pre-0.30.187
// both used the walker's prewarmPageLimit() default for no-slice
// widgets, but the dispatcher's serve-time paginationInfo defaults to
// (-1, -1) when the request URL carries no ?page/?perPage params —
// seed→serve cells thus missed on every no-slice widget. The seed-key
// tuple is now derived via deriveSeedKeyTuple from the /call Path the
// walker reached the widget through; the resolution tuple stays
// bounded by prewarmPageLimit() as the 0.30.127 storm guard.
type navWidgetEntry struct {
	W       *unstructured.Unstructured
	GVR     schema.GroupVersionResource
	PerPage int // resolution tuple — passed to widgets.Resolve
	Page    int // resolution tuple — passed to widgets.Resolve

	// Ship 0.30.187 D2 — dispatcher-lookup KEY tuple. Set to (-1, -1)
	// for widgets reached via a /call Path with no slice declared (the
	// dispatcher's paginationInfo default), or to the declared (page,
	// perPage) when the Path carries them. See deriveSeedKeyTuple.
	KeyPerPage int
	KeyPage    int
}

// navWidgetHarvester accumulates the deduplicated navigation widget
// set the Phase-1 walker reaches under SA identity (Step 7.6a). The
// phase1Walker writes into it as it resolves each widget; the Step
// 7.6 seed pass drains it once per cohort. Dedupe key is the per-
// (gvr, ns, name, perPage, page) tuple — admin and cyberjoker hitting
// the same widget under the same pagination land on the same harvested
// entry, and the seed produces one per-user Put per cohort.
//
// Concurrency: the walk is single-threaded per root and roots resolve
// sequentially (phase1WarmupWith), but the mutex makes the harvester
// safe regardless of how the walk is scheduled — same shape as
// contentPrewarmHarvester.
type navWidgetHarvester struct {
	mu      sync.Mutex
	entries map[string]navWidgetEntry
}

// newNavWidgetHarvester returns an empty harvester.
func newNavWidgetHarvester() *navWidgetHarvester {
	return &navWidgetHarvester{entries: map[string]navWidgetEntry{}}
}

// harvestNavWidget records a navigation widget CR plus the GVR +
// pagination tuples it was reached under. Nil-safe: a nil harvester /
// nil widget is a no-op (flag-off Phase 1 passes no harvester).
//
// Ship 0.30.187 D2: TWO pagination tuples are now passed.
//   - resolvePerPage/resolvePage: what the walker passes to
//     widgets.Resolve (bounded by prewarmPageLimit() for no-slice
//     widgets — the 0.30.127 storm guard).
//   - keyPerPage/keyPage: what the per-cohort seed loop passes to
//     dispatchCacheLookupKey — derived from the /call Path the walker
//     reached the widget through so the cell matches the dispatcher's
//     serve-time lookup. See deriveSeedKeyTuple.
//
// Dedupe is over (gvr, ns, name, keyPerPage, keyPage) — the dispatcher-
// key tuple — because that is the cell the seed populates. Two
// different roots reaching the same widget via the same key tuple yield
// one Put (idempotent — the resolver output is per-cohort identical for
// a given key tuple).
func (h *navWidgetHarvester) harvestNavWidget(w *unstructured.Unstructured, gvr schema.GroupVersionResource,
	resolvePerPage, resolvePage, keyPerPage, keyPage int) {
	if h == nil || w == nil {
		return
	}
	key := navWidgetHarvestKey(gvr, w.GetNamespace(), w.GetName(), keyPerPage, keyPage)
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, seen := h.entries[key]; seen {
		// First-write-wins. The dedupe is intentional: the walk's
		// visited-set in phase1Walker.walk already prevents re-traversal,
		// so a second harvest for the same key only happens across roots
		// (idempotent — same CR + same key tuple yields identical Put).
		return
	}
	// Deep-copy the CR so a downstream resolver mutation does not race
	// with the original walker's `in` (the resolver mutates the
	// in-memory object during resolve — widgets.Resolve sets
	// status.widgetData etc.). The seed loop runs its own Resolve per
	// cohort against the CR; concurrent cohort resolves MUST NOT share
	// a single *unstructured. The DeepCopy is bounded by the widget CR
	// size (small) and runs once per distinct widget.
	h.entries[key] = navWidgetEntry{
		W:          w.DeepCopy(),
		GVR:        gvr,
		PerPage:    resolvePerPage,
		Page:       resolvePage,
		KeyPerPage: keyPerPage,
		KeyPage:    keyPage,
	}
}

// deriveSeedKeyTuple computes the dispatcher-lookup key tuple
// (perPage, page) the per-cohort seed Put MUST use for a widget the
// walker reached via the given /call Path. Ship 0.30.187 D2.
//
// CONTRACT: the returned tuple MUST equal what the dispatcher's
// paginationInfo (helpers.go:50-76) returns at serve time for a request
// with that Path's query parameters.
//
//   - Empty path (root navigation widget — fetched directly via
//     objects.Get, no /call Path) → the frontend's first request
//     URL carries no slice params → paginationInfo returns (-1, -1) →
//     seed-key tuple = (-1, -1).
//   - Path with no page/perPage params → ParseCallPathPagination
//     returns ok=false → paginationInfo returns (-1, -1) →
//     seed-key tuple = (-1, -1).
//   - Path with explicit ?page=N&perPage=M → ParseCallPathPagination
//     returns the declared values → paginationInfo at serve time
//     returns the same → seed-key tuple = (perPage=M, page=N).
//
// The returned order is (perPage, page) — matches the seedOneWidget
// argument order to dispatchCacheLookupKey.
func deriveSeedKeyTuple(callPath string) (perPage, page int) {
	if callPath == "" {
		return -1, -1
	}
	p, pp, ok := util.ParseCallPathPagination(callPath)
	if !ok {
		return -1, -1
	}
	return pp, p
}

// navWidgetHarvestKey is the canonical dedup key for harvested
// navigation widgets. The tuple matches the dispatcher's serve-time
// key shape for widgets (dispatchCacheLookupKey at helpers.go:174 takes
// the same fields).
func navWidgetHarvestKey(gvr schema.GroupVersionResource, ns, name string, perPage, page int) string {
	return gvr.String() + "|" + ns + "|" + name + "|" + fmt.Sprintf("%d|%d", perPage, page)
}

// snapshot returns a stable list of harvested widget entries.
func (h *navWidgetHarvester) snapshot() []navWidgetEntry {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]navWidgetEntry, 0, len(h.entries))
	for _, e := range h.entries {
		out = append(out, e)
	}
	return out
}

// seedTarget is the dispatcher-package equivalent of cache.PrewarmTarget,
// shaped for the seed loop's per-target dispatch. Carries a representative
// SubjectIdentity (Username + Groups, k8s-shape) so the cohort ctx setup
// flow (withCohortSeedContext) can lift it into xcontext.WithUserInfo
// byte-equivalently to the pre-ship cache.Cohort path.
//
// Ship 0.30.242 H.c-layered Phase 2b replacement for the deleted
// cache.Cohort type. Identical surface (Username + Groups); production
// callers in this file consume only those two fields. The BindingUID
// dimension is carried but not used by the seed loop directly — it
// flows through the cell key at populate time via dispatchCacheLookupKey
// (helpers.go) per Path B.
type seedTarget struct {
	BindingUID string
	Username   string
	Groups     []string
}

// enumerateAggregatePrewarmTargets unions per-GVR prewarm targets across
// every registered GVR + dedupes by (BindingUID, Username, Groups[0]).
// One seed dispatch per unique target; each dispatch resolves whatever
// restactions/widgets its identity authorises (the per-layer BindingUID
// is derived at populate time, design §7.2 + Path B).
//
// Returns nil when the resource watcher is not wired (cache=off / pre-
// readiness) or when no binding authorises any navigated GVR. The seed
// loop treats nil as a clean no-op (logs phase1.seed.skipped).
func enumerateAggregatePrewarmTargets() []seedTarget {
	rw := cache.Global()
	if rw == nil {
		return nil
	}
	gvrs := rw.RegisteredGVRs()
	if len(gvrs) == 0 {
		return nil
	}
	// Dedupe by (BindingUID, Username, first Group) — the seed dispatch's
	// identity dimensions. Two GVRs sharing the same authorising binding +
	// representative identity produce ONE seed dispatch.
	type dedupeKey struct {
		BindingUID string
		Username   string
		Group      string
	}
	seen := make(map[dedupeKey]struct{}, 32)
	out := make([]seedTarget, 0, 32)
	for _, gvr := range gvrs {
		// Verb "list" — restactions/widgets nav LIST is the dominant
		// authz question; matches the BindingsByGVR index's enrolment
		// criterion (rulesGrantGetList covers both get+list).
		targets := cache.EnumeratePrewarmTargetsForGVR(gvr, "list")
		for _, t := range targets {
			var firstGroup string
			if len(t.Subject.Groups) > 0 {
				firstGroup = t.Subject.Groups[0]
			}
			k := dedupeKey{
				BindingUID: t.BindingUID,
				Username:   t.Subject.Username,
				Group:      firstGroup,
			}
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, seedTarget{
				BindingUID: t.BindingUID,
				Username:   t.Subject.Username,
				Groups:     append([]string(nil), t.Subject.Groups...),
			})
		}
	}
	return out
}

// enumerateAggregatePrewarmTargetsFn is a test seam over
// enumerateAggregatePrewarmTargets — same pattern as seedCohortFn /
// phase1MaxApiRefPagesForTest. The #158 discrimination falsifier swaps it
// to inject a fixed cohort list so runPIPSeed's classification + retry
// branches can be driven end-to-end without a live cache/RBAC snapshot.
// Production always uses the real enumerator.
var enumerateAggregatePrewarmTargetsFn = enumerateAggregatePrewarmTargets

// runPIPSeed is the Ship PIP Step 7.6 entry point invoked by
// phase1WarmupWith. Enumerates per-binding targets and seeds the
// resolved-output L1 (restactions + widgets) for every target.
//
// Ship 2 / 0.30.196 — runPIPSeed is invoked on a BACKGROUND goroutine
// AFTER MarkPhase1Done; its return value is log-only. There is no longer
// a cohort cap and no fail-closed path: per-cohort errors are swallowed
// (each cohort goroutine returns nil), so a non-nil return here is now
// vacuously rare. Readiness is NEVER affected by this function's outcome.
//
// h is the F2 content-prewarm harvester (apiRefHarvester) — drained
// for the restactions seed loop. nh is the new navWidgetHarvester —
// drained for the widgets seed loop. Both are pre-populated by the
// walk and stable by the time runPIPSeed runs.
func runPIPSeed(ctx context.Context, h *contentPrewarmHarvester, nh *navWidgetHarvester,
	saEP endpoints.Endpoint, saRC *rest.Config, authnNS string) error {

	log := slog.Default()
	start := time.Now()

	// Ship 0.30.242 H.c-layered Phase 2b — per-binding seed enumeration.
	// The PIP seed drives one entry per per-binding target (a binding
	// authorising get/list on a navigated GVR) derived from the
	// BindingsByGVR reverse index via cache.EnumeratePrewarmTargetsForGVR.
	// Replaces the pre-ship EnumerateBindingSetClasses cohort enumerator
	// (deleted in commit 1d93d02). Cell sharing is now per-binding —
	// design §7.2.
	//
	// Aggregation across GVRs: enumerate targets per navigated GVR, then
	// dedupe by (BindingUID, Username, Groups[0]) so a binding that
	// grants multiple GVRs produces ONE seed dispatch (the dispatch
	// resolves widgets+restactions covering whatever cells its identity
	// authorises). Path B (Phase 2b deferral): seed dispatch derives its
	// own per-layer BindingUID at populate time via direct rbac.EvaluateRBAC.
	targets := enumerateAggregatePrewarmTargetsFn()
	if len(targets) == 0 {
		log.Info("phase1.seed.skipped",
			slog.String("subsystem", "cache"),
			slog.String("reason", "EnumeratePrewarmTargetsForGVR returned no targets across registered GVRs — RBAC snapshot empty or unpublished, or no bindings authorise the navigated GVRs"),
		)
		return nil
	}
	// Adapt targets to the legacy cohort dispatch surface. cohorts type
	// is preserved at the loop level for minimum-churn migration in 2b;
	// per-binding KEY semantics now flow through the cell-key BindingUID
	// derived at populate time (helpers.go:dispatchCacheLookupKey via
	// direct rbac.EvaluateRBAC — Path B).
	cohorts := targets

	// Ship 2 / 0.30.196 — the cohort cap check is DELETED. There is no
	// fail-closed-on-cohort-count branch: runPIPSeed runs as a background
	// best-effort warm (phase1_walk.go Step 7.6) AFTER readiness is already
	// 200, so a high cohort count can never withhold readiness. An
	// O(users)-cohort topology simply takes longer to warm in the
	// background, bounded by pipGlobalTimeout — it never wedges the pod.

	restactionRefs := h.snapshot()
	widgetEntries := nh.snapshot()

	log.Info("phase1.seed.started",
		slog.String("subsystem", "cache"),
		slog.Int("cohorts", len(cohorts)),
		slog.Int("restactions", len(restactionRefs)),
		slog.Int("widgets", len(widgetEntries)),
	)

	// Step 7.6 global budget — phase1WarmupWith already passes a ctx
	// bound by PHASE1_TIMEOUT_SECONDS; layer the PIP-specific 40 s
	// ceiling on top so a stuck cohort cannot eat the whole Phase 1
	// budget.
	pctx, pcancel := context.WithTimeout(ctx, pipGlobalTimeout)
	defer pcancel()

	g, gctx := errgroup.WithContext(pctx)
	limit := runtime.GOMAXPROCS(0)
	if limit < 1 {
		limit = 1
	}
	g.SetLimit(limit)

	// #158 (design §1.5) — legacy-path bounded re-enqueue. Operational
	// failures (NOT RBAC denies) are collected here during the errgroup,
	// then drained ONCE after g.Wait() with a per-retry customer-priority
	// yield (engineYieldCheckpoint — the SAME customerInFlight() predicate
	// the engine uses; NOT a private busy loop, NOT a shared budget). Bound:
	// ≤1 retry per cohort per run — a cohort that fails operationally TWICE
	// is logged + dropped (the customer's first /call lazily warms it). The
	// engine path (PrewarmEngineEnabled()==true) does NOT reach runPIPSeed;
	// it re-enqueues a coalesced scopeKindBoot instead (seedScopeYielding).
	var (
		retryMu      sync.Mutex
		retryCohorts []seedTarget
	)

	for _, c := range cohorts {
		cohort := c // pin loop variable
		g.Go(func() error {
			// Ship A.3 / 0.30.179 — count every per-class seed resolve
			// (one cohort goroutine = one resolve unit). Failures bump
			// the dedicated failure counter so the operator sees a
			// non-zero `snowplow_phase1_bindingset_seed_failures_total`
			// when the seed loop drops a class.
			//
			// PER-COHORT ERRORS ARE NON-FATAL — Ship A.3 / 0.30.180
			// followup. Binding-set enumeration produces cohort classes
			// for EVERY (user, group-subset) binding-set, including
			// narrow ServiceAccount identities that genuinely cannot
			// read RESTActions/widgets (their bindings permit only
			// scoped resources). A per-cohort RBAC denial during seed
			// is EXPECTED for narrow cohorts: those cohorts don't need
			// a seeded L1 entry — their first /call would deny anyway.
			// Log + count + return nil so the global seed loop completes
			// and phase1Done flips. The cluster-wide PIP mechanism stays
			// FOREGROUND (still gates phase1Done) but per-cohort
			// failures no longer FAIL-CLOSE the whole pod.
			pipBindingSetSeedResolvesTotal.Add(1)
			// #46: NO bound wrap here — the footprint bound lives in the SHARED
			// primitives seedOneWidget/seedOneRestaction (which this errgroup's
			// seedCohortFn funnels through), so the aggregate is bounded at the
			// shared mechanism, NOT per-caller. The errgroup's SetLimit
			// (GOMAXPROCS) is the OUTER semaphore; Piece-B's is INNER (in the
			// primitive) — strict outer→inner ordering, deadlock-free.
			if err := seedCohortFn(gctx, cohort, restactionRefs, widgetEntries, saEP, saRC, authnNS); err != nil {
				// #158 — classify the failure instead of blanket-labelling
				// it "expected RBAC." pipBindingSetSeedFailuresTotal is kept
				// as the back-compat grand total (= rbac_deny + operational).
				pipBindingSetSeedFailuresTotal.Add(1)
				switch classifySeedErr(err) {
				case seedFailRBACDeny:
					// EXPECTED: a narrow cohort that genuinely cannot read the
					// seed target. Info, not Warn; NOT re-enqueued (it would
					// deny again — nothing to retry).
					pipSeedRBACDenyTotal.Add(1)
					slog.Info("phase1.seed.cohort.expected_deny",
						slog.String("subsystem", "cache"),
						slog.String("cohort", cohortLogLabel(cohort)),
						slog.Any("err", err),
						slog.String("effect", "cohort skipped; phase1Done not blocked — this cohort's "+
							"identity is forbidden from the seed target (403/401), which is the expected "+
							"narrow-RBAC posture and needs no L1 entry"),
					)
				default:
					// OPERATIONAL (incl. the fail-loud default): ctx
					// timeout/cancel, 5xx, transport, panic, or any
					// unclassified error. Warn + dedicated counter + queue
					// for a bounded single retry after g.Wait().
					pipSeedOperationalFailTotal.Add(1)
					slog.Warn("phase1.seed.cohort.operational_failure",
						slog.String("subsystem", "cache"),
						slog.String("cohort", cohortLogLabel(cohort)),
						slog.Any("err", err),
						slog.String("effect", "cohort seed failed operationally (NOT an RBAC deny); queued "+
							"for one bounded re-attempt after the seed loop — see "+
							"snowplow_phase1_seed_operational_fail_total at /debug/vars"),
					)
					retryMu.Lock()
					retryCohorts = append(retryCohorts, cohort)
					retryMu.Unlock()
				}
				// Non-fatal — return nil so the global seed loop completes.
				return nil
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		// g.Wait error should never fire now (per-cohort errors are swallowed
		// above), but keep the failure-path log + counter intact so any future
		// genuinely-fatal error mode is surfaced.
		log.Error("phase1.seed.failed",
			slog.String("subsystem", "cache"),
			slog.Any("err", err),
			slog.Int64("elapsed_ms", time.Since(start).Milliseconds()),
		)
		return err
	}

	// #158 (design §1.5) — drain the operational-failure retry slice ONCE,
	// serially, with a customer-priority yield before each re-attempt. This
	// is the legacy path's bounded re-enqueue (no engine queue exists when
	// PrewarmEngineEnabled()==false). ≤1 retry per cohort: a cohort that
	// fails operationally AGAIN here is logged + dropped (the customer's
	// first /call lazily warms it — the documented degraded posture). The
	// yield reuses engineYieldCheckpoint (the SAME customerInFlight()
	// predicate the engine uses) so a customer burst arriving mid-retry
	// defers the remaining retries — never a private busy loop, never a
	// shared budget.
	retryMu.Lock()
	pending := retryCohorts
	retryMu.Unlock()
	if len(pending) > 0 {
		log.Info("phase1.seed.retry.started",
			slog.String("subsystem", "cache"),
			slog.Int("operational_failed_cohorts", len(pending)),
		)
		retried := 0
		for _, cohort := range pending {
			if err := pctx.Err(); err != nil {
				// Step 7.6 budget exhausted — stop retrying; remaining
				// cohorts warm lazily on first /call.
				break
			}
			engineYieldCheckpoint(pctx)
			if err := seedCohortFn(pctx, cohort, restactionRefs, widgetEntries, saEP, saRC, authnNS); err != nil {
				// Second operational failure → drop (bound = ≤1 retry).
				slog.Warn("phase1.seed.cohort.retry_exhausted",
					slog.String("subsystem", "cache"),
					slog.String("cohort", cohortLogLabel(cohort)),
					slog.Any("err", err),
					slog.String("effect", "cohort still failing after one bounded re-attempt; dropped — "+
						"the cohort's first /call cold-resolves + warms lazily"),
				)
				continue
			}
			retried++
		}
		log.Info("phase1.seed.retry.completed",
			slog.String("subsystem", "cache"),
			slog.Int("operational_failed_cohorts", len(pending)),
			slog.Int("retry_succeeded", retried),
		)
	}

	log.Info("phase1.seed.completed",
		slog.String("subsystem", "cache"),
		slog.Int("cohorts", len(cohorts)),
		slog.Int64("elapsed_ms", time.Since(start).Milliseconds()),
	)
	return nil
}

// seedCohortFn is a test seam over seedCohort — same pattern as
// phase1MaxApiRefPagesForTest (phase1_walk_pagination.go:124). The
// #158 discrimination falsifier (phase1_seed_classify_test.go) swaps this
// to inject a fake seedCohort returning a controlled error class without a
// live cluster, then asserts the call site's branch (counter + log + the
// re-enqueue delta). Production always uses the real seedCohort.
var seedCohortFn = seedCohort

// seedCohort seeds one cohort's per-user restactions + widgets L1
// entries. Per-cohort timeout + per-cohort error containment.
//
// Ship 0.30.187 D1: per-target errors are NON-FATAL — they bump the
// per-(cohort, target) failure counter (visible at /debug/vars) and
// mark the cohort status as "partial", but the loop continues so the
// remaining seed targets land. The cohort's final status (success /
// partial / failed) is published via recordCohortSeedStatus. A
// timeout / outer-ctx cancel still returns an error (the cohort status
// is "failed") so the caller can surface that mode separately.
//
// RATIONALE for non-fatal per-target errors: pre-0.30.187 the FIRST
// widget-seed error for a cohort returned immediately. cyberjoker is in
// group:devs; that cohort's restactions seeded but widgets seed
// errored on widget #1 (likely RBAC denial on a narrow nav widget),
// and seedCohort returned after the first widget so the remaining 16
// widgets of group:devs were never seeded — the 14/17 first-nav cold
// miss on the 0.30.186 cyberjoker run. With per-target containment a
// single bad widget broke ONE cell, not 16.
//
// Ship 0.30.191 Fix C — abort-cause instrumentation. Every path that
// flips cohort status to "failed" now also emits a single greppable
// log line `phase1.cohort.abort` carrying which phase fired the abort
// (restactions / widgets / panic / pre-flight), the abort cause
// (ctx_err with the underlying ctx.Err string, panic with the recovered
// value, etc.), how many targets the cohort had processed by then, the
// elapsed wall-clock, and the per-cohort timeout in milliseconds. The
// log line is emitted by a deferred reporter so a panic mid-loop is
// also captured (a recover() in the same defer prevents the goroutine
// from crashing the errgroup). All instrumentation fields are uniform
// across cohorts (feedback_no_special_cases): no per-cohort branching.
//
// LEGACY SEED PATH (#341 ruling 2026-06-12): reached ONLY when
// PREWARM_ENGINE_ENABLED=false — the documented single-knob back-out
// lever for the unified prewarm engine (0.30.247/248); prod runs =true.
// Kept per the #57 lever framework; deliberately NOT unified with the
// engine's seedScopeYielding (independent by design, so the lever stays
// a clean revert). Do not refactor or delete while the lever stands.
func seedCohort(ctx context.Context, cohort seedTarget,
	restactionRefs []templatesv1.ObjectReference, widgetEntries []navWidgetEntry,
	saEP endpoints.Endpoint, saRC *rest.Config, authnNS string) error {

	log := slog.Default()
	cohortLabel := cohortLogLabel(cohort)
	start := time.Now()

	// Ship 0.30.191 Fix C — local counters threaded through the loops.
	// Updated INSIDE the for-bodies (goroutine-scoped, no concurrency).
	// The deferred reporter reads them after the function body exits.
	var (
		abortPhase           string // "init" / "restactions" / "widgets" / "panic"
		abortCause           string // free-form short tag — ctx_err / panic / none
		ctxErrString         string // ctx.Err().Error() at abort site, or ""
		processedRestactions int
		processedWidgets     int
		emittedFinalAbortLog bool
		recoveredPanic       any
	)
	abortPhase = "init"

	log.Info("phase1.seed.cohort.start",
		slog.String("subsystem", "cache"),
		slog.String("cohort", cohortLabel),
		slog.Int("restactions", len(restactionRefs)),
		slog.Int("widgets", len(widgetEntries)),
	)

	// Per-cohort hard ceiling. A stuck cohort cannot wedge Step 7.6
	// past its global budget. Fixed 120s (0.30.179 value); the
	// 0.30.190 proportional-timeout model was REVERTED at 0.30.191 per
	// the SCOPE CORRECTION — we instrument first, then fix.
	cctx, ccancel := context.WithTimeout(ctx, pipCohortTimeout)
	defer ccancel()

	// Ship 0.30.191 Fix C — deferred abort-cause reporter. Runs whether
	// the body returns normally, returns an error, or panics. Emits one
	// `phase1.cohort.abort` log line when the cohort's status is
	// "failed" (timeout, ctx cancel, panic). Pure observational —
	// returns no value, mutates no shared state.
	//
	// The recover() catches a downstream panic in seedOneRestaction /
	// seedOneWidget — pre-0.30.191 such a panic crashed the cohort
	// goroutine + (potentially) the pod via the errgroup propagation.
	// Now the panic is logged + the cohort is marked failed + the
	// error is returned up the errgroup; the caller's runPIPSeed loop
	// already treats per-cohort errors as non-fatal (0.30.180), so a
	// panic in one cohort no longer wedges all of Phase 1.
	defer func() {
		if r := recover(); r != nil {
			recoveredPanic = r
			abortPhase = "panic"
			abortCause = "panic"
			ctxErrString = ""
			// We're recovering INSIDE the deferred closure — emit the
			// abort log line below and mark cohort failed. The caller
			// (errgroup) will not see an error because we're swallowing
			// the panic here, but recordCohortSeedStatus and the abort
			// log line make the failure visible.
			recordCohortSeedStatus(cohortLabel, cohortStatusFailed)
		}
		// Only emit the abort log line for cohorts that ACTUALLY failed
		// (the inline abort branches set abortCause non-empty before
		// returning; success / partial paths leave it ""). This avoids
		// log spam for the success path.
		if abortCause == "" {
			return
		}
		if emittedFinalAbortLog {
			return
		}
		emittedFinalAbortLog = true
		fields := []any{
			slog.String("subsystem", "cache"),
			slog.String("cohort", cohortLabel),
			slog.String("phase", abortPhase),
			slog.String("abort_cause", abortCause),
			slog.String("ctx_err", ctxErrString),
			slog.Int("restactions_total", len(restactionRefs)),
			slog.Int("widgets_total", len(widgetEntries)),
			slog.Int("restactions_processed", processedRestactions),
			slog.Int("widgets_processed", processedWidgets),
			slog.Int64("elapsed_ms", time.Since(start).Milliseconds()),
			slog.Int64("cohort_timeout_ms", pipCohortTimeout.Milliseconds()),
		}
		if recoveredPanic != nil {
			fields = append(fields, slog.Any("panic", recoveredPanic))
		}
		log.Info("phase1.cohort.abort", fields...)
	}()

	// Build the per-cohort ctx: SA transport seam preserved (so the
	// resolver dispatches via the SA-credentialed inner-call path that
	// 0.30.166/167/168 wired) but identity OVERRIDDEN via
	// xcontext.WithUserInfo so dispatchCacheLookupKey hashes the cohort
	// into the L1 key and EvaluateRBAC fires against the cohort's
	// bindings.
	cohortCtx := withCohortSeedContext(cctx, cohort, saEP, saRC)

	// Track whether ANY per-target Put errored — if so the cohort
	// status is "partial"; if not it's "success". A timeout / ctx
	// cancel below sets "failed" and short-circuits.
	hadFailure := false

	// Restactions seed loop — drain the harvester, one Put per
	// (cohort, restaction).
	abortPhase = "restactions"
	for _, ref := range restactionRefs {
		if err := cctx.Err(); err != nil {
			abortCause = "ctx_err"
			ctxErrString = err.Error()
			log.Error("phase1.seed.cohort.timeout",
				slog.String("subsystem", "cache"),
				slog.String("cohort", cohortLabel),
				slog.String("phase", "restactions"),
				slog.Any("err", err),
				slog.Int("restactions_processed", processedRestactions),
				slog.Int("widgets_processed", processedWidgets),
				slog.Int64("elapsed_ms", time.Since(start).Milliseconds()),
				slog.Int64("cohort_timeout_ms", pipCohortTimeout.Milliseconds()),
			)
			recordCohortSeedStatus(cohortLabel, cohortStatusFailed)
			return fmt.Errorf("cohort %q restactions seed: %w", cohortLabel, err)
		}
		if err := seedOneRestaction(cohortCtx, cohortLabel, ref, authnNS); err != nil {
			// Ship 0.30.187 D1: per-target containment. Bump the
			// per-(cohort, target) failure counter and continue with
			// the next restaction so a single bad target does not
			// abort the cohort.
			hadFailure = true
			incFailureCounter(&pipRestactionSeedFailureByKey,
				cohortLabel+"|"+ref.Namespace+"/"+ref.Name)
			log.Warn("phase1.seed.cohort.target_skipped",
				slog.String("subsystem", "cache"),
				slog.String("cohort", cohortLabel),
				slog.String("phase", "restactions"),
				slog.String("restaction", ref.Namespace+"/"+ref.Name),
				slog.Any("err", err),
				slog.String("effect", "this target skipped; cohort continues — see "+
					"snowplow_phase1_restaction_seed_failure_total at /debug/vars"),
			)
			continue
		}
		processedRestactions++
	}

	// Widgets seed loop — drain the harvested widget entries, one Put
	// per (cohort, widget).
	abortPhase = "widgets"
	for _, e := range widgetEntries {
		if err := cctx.Err(); err != nil {
			abortCause = "ctx_err"
			ctxErrString = err.Error()
			log.Error("phase1.seed.cohort.timeout",
				slog.String("subsystem", "cache"),
				slog.String("cohort", cohortLabel),
				slog.String("phase", "widgets"),
				slog.Any("err", err),
				slog.Int("restactions_processed", processedRestactions),
				slog.Int("widgets_processed", processedWidgets),
				slog.Int64("elapsed_ms", time.Since(start).Milliseconds()),
				slog.Int64("cohort_timeout_ms", pipCohortTimeout.Milliseconds()),
			)
			recordCohortSeedStatus(cohortLabel, cohortStatusFailed)
			return fmt.Errorf("cohort %q widgets seed: %w", cohortLabel, err)
		}
		if err := seedOneWidget(cohortCtx, e, authnNS); err != nil {
			// Ship 0.30.187 D1: per-target containment for widget
			// seeds — see the restactions block above. Composite key:
			// "cohort|widget_name|gvr".
			hadFailure = true
			incFailureCounter(&pipWidgetSeedFailureByKey,
				cohortLabel+"|"+e.W.GetNamespace()+"/"+e.W.GetName()+"|"+e.GVR.String())
			log.Warn("phase1.seed.cohort.target_skipped",
				slog.String("subsystem", "cache"),
				slog.String("cohort", cohortLabel),
				slog.String("phase", "widgets"),
				slog.String("widget", e.W.GetNamespace()+"/"+e.W.GetName()),
				slog.String("gvr", e.GVR.String()),
				slog.Any("err", err),
				slog.String("effect", "this widget skipped for this cohort; loop continues — "+
					"see snowplow_phase1_widget_seed_failure_total at /debug/vars"),
			)
			continue
		}
		processedWidgets++
	}

	// Publish the cohort's final status.
	if hadFailure {
		recordCohortSeedStatus(cohortLabel, cohortStatusPartial)
	} else {
		recordCohortSeedStatus(cohortLabel, cohortStatusSuccess)
	}

	log.Info("phase1.seed.cohort.complete",
		slog.String("subsystem", "cache"),
		slog.String("cohort", cohortLabel),
		slog.Bool("had_per_target_failure", hadFailure),
		slog.Int("restactions_processed", processedRestactions),
		slog.Int("widgets_processed", processedWidgets),
		slog.Int64("elapsed_ms", time.Since(start).Milliseconds()),
	)
	return nil
}

// withCohortSeedContext builds the per-cohort seed context. Mirrors
// withContentPrewarmSAContext (phase1_content_prewarm.go) for the SA
// transport seam (WithUserConfig / WithInternalEndpoint /
// WithInternalRESTConfig) but installs the COHORT's identity via
// xcontext.WithUserInfo instead of the SA's canonical username. Same
// inner-call iterator-serial marker (WithPrewarmIterSerial) so the
// seed pass shares the F2 content-warm's OOM profile.
//
// NOT marked WithApistagePrewarm — the apistage content L1 was already
// populated in Step 7.5; here we are populating the TOP-LEVEL
// per-user L1 (restactions + widgets dispatcher classes).
func withCohortSeedContext(ctx context.Context, cohort seedTarget,
	saEP endpoints.Endpoint, saRC *rest.Config) context.Context {

	opts := []xcontext.WithContextFunc{
		xcontext.WithUserConfig(saEP),
		xcontext.WithLogger(slog.Default()),
		xcontext.WithUserInfo(jwtutil.UserInfo{
			Username: cohort.Username,
			Groups:   cohort.Groups,
		}),
	}
	rctx := xcontext.BuildContext(ctx, opts...)
	rctx = cache.WithInternalEndpoint(rctx, &saEP)
	rctx = cache.WithInternalRESTConfig(rctx, saRC)
	rctx = cache.WithPrewarmIterSerial(rctx)
	return rctx
}

// seedOneRestaction resolves ONE RESTAction under the cohort ctx and
// Puts the resolved JSON into the per-user restactions L1 under the
// dispatcher's canonical key. STRUCTURALLY MATCHES the per-request
// dispatch at restactions.go:117-230 (architect's
// feedback_claim_vs_code_identity_at_diff_review):
//
//   - dispatchCacheLookupKey("restactions", group, version, resource,
//     ns, name, -1, -1, nil) — identical args (PerPage:-1, Page:-1,
//     extras:nil match the dispatcher's first /call by a cohort that
//     supplies no per-call pagination/extras; HG-PIP.3 byte-identity
//     gate verifies SHA-256 between seed Put and serve hit).
//   - cache.WithL1KeyContext(ctx, key) before Resolve so the inner-call
//     dep tracker records edges against the L1 key.
//   - restactions.Resolve same entrypoint at restactions.go:183-189.
//   - encodeResolvedJSON + cacheHandle.Put + ensureWatcherInformerForGVR
//   - cache.Deps().Record — same Put shape as restactions.go:212-230.
func seedOneRestaction(ctx context.Context, cohortLabel string, ref templatesv1.ObjectReference, authnNS string) error {
	got := objects.Get(ctx, ref)
	if got.Err != nil {
		// #158 (design §1.3): preserve the typed status error so the call
		// site's classifySeedErr sees 403 (RBAC deny) vs 5xx (operational)
		// instead of an opaque string. statusErrFromResponse lifts the
		// plumbing *response.Status back into an *apierrors.StatusError
		// carrying Code+Reason. Wrapped with %w so the RESTAction identity
		// is visible in logs while errors.As/Is still thread through to the
		// embedded StatusError.
		return fmt.Errorf("fetch RESTAction %s/%s: %w", ref.Namespace, ref.Name, statusErrFromResponse(got.Err))
	}
	if got.Unstructured == nil {
		return fmt.Errorf("fetch RESTAction %s/%s: nil object", ref.Namespace, ref.Name)
	}

	// Compute the per-user dispatcher key — IDENTICAL shape to
	// restactions.go:117. ctx already carries the cohort's UserInfo, so
	// dispatchCacheLookupKey reads it and hashes Username + Groups into
	// the key.
	key, handle, inputs := dispatchCacheLookupKey(ctx, "restactions",
		got.GVR.Group, got.GVR.Version, got.GVR.Resource,
		got.Unstructured.GetNamespace(), got.Unstructured.GetName(),
		-1, -1, nil)
	// Ship 0.30.188 — diagnostic slog: emit the seed-side cache key +
	// its components so it can be diff'd against the dispatcher_get and
	// per_user_fallback_put log lines at widgets.go / restactions.go.
	emitDispatchCacheKeyDiag(slog.Default(), "seed", ctx,
		key, inputs, "restactions",
		got.GVR.Group, got.GVR.Version, got.GVR.Resource,
		got.Unstructured.GetNamespace(), got.Unstructured.GetName(),
		-1, -1, nil)
	if handle == nil || key == "" {
		// L1 disabled OR no identity on ctx — defensive skip. PIP's
		// cohort ctx ALWAYS installs WithUserInfo, so an empty key here
		// is a configuration bug (PREWARM_PIP_ENABLED on while
		// CACHE_ENABLED off); log + skip.
		return nil
	}

	// #46: bound this seed unit's footprint (semaphore admission + per-unit
	// HeapInuse assert), AFTER the identity short-circuit so the customer
	// /call path is untouched. seedOneRestaction is a SHARED primitive — both
	// the engine (serial) and legacy errgroup (concurrent) seed paths funnel
	// here, so this one bracket bounds BOTH aggregates. Transparent when
	// SEED_FOOTPRINT_BUDGET_BYTES==0.
	seedRelease, seedErr := enterSeedUnit(ctx, "restaction/"+ref.Namespace+"/"+ref.Name)
	if seedErr != nil {
		return seedErr // ctx cancelled while blocked on the bound
	}
	defer seedRelease()

	scheme := k8sruntime.NewScheme()
	if err := apis.AddToScheme(scheme); err != nil {
		return fmt.Errorf("add apis to scheme: %w", err)
	}
	var cr templatesv1.RESTAction
	if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(
		got.Unstructured.Object, &cr); err != nil {
		return fmt.Errorf("unstructured -> RESTAction %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	// Ship 0.30.192 — pure-additive per-stage timing sink for cost
	// attribution. The 0.30.179 cluster-list-deny / per-NS iterator
	// fallback at iter_serial=1 is the architect's TRACED hypothesis
	// for the 46s/restaction wall-clock on the four 0.30.189-sentinel
	// cohorts — but the "5K namespaces × ~10ms" projection was
	// invalidated by the cluster reality (62 ns actual). This sink lets
	// the resolver record per-stage ElapsedMs + ClusterListUsed +
	// ClusterListDenyGate + IteratorCalls + IteratorElapsedMs so the
	// failing cohort's 46s can be attributed to a real code path.
	//
	// SINK ISOLATION (feedback_shared_vs_copy_is_a_concurrency_change):
	// one sink per seedOneRestaction invocation; never shared across
	// cohorts. The sink's sync.Mutex is defensive — the resolver writes
	// only from the parent goroutine (between stages) — but a future
	// path that records from an errgroup worker stays race-safe.
	stageTimingSink := cache.NewPIPStageTimingSink()
	restactionStart := time.Now()
	defer func() {
		snapshot := stageTimingSink.Snapshot()
		slog.Default().Info("phase1.seed.restaction.timing",
			slog.String("subsystem", "cache"),
			slog.String("cohort", cohortLabel),
			slog.String("restaction", ref.Namespace+"/"+ref.Name),
			slog.Int64("elapsed_ms_total", time.Since(restactionStart).Milliseconds()),
			slog.Int("stages_total", len(snapshot)),
			slog.Any("stages", snapshot),
		)
	}()

	// Install the L1 key on ctx BEFORE Resolve so the inner-call dep
	// tracker records edges against this entry — matches
	// restactions.go:180-182.
	resCtx := cache.WithL1KeyContext(ctx, key)
	resCtx = cache.WithPIPStageTimingSink(resCtx, stageTimingSink)

	res, err := restactions.Resolve(resCtx, restactions.ResolveOptions{
		In: &cr,
		// Ship 0.30.230 fix-at-root: SArc is the SA *rest.Config carried
		// on ctx by withCohortSeedContext upstream. Threading it here
		// ensures the inner endpointReferenceMapper has a non-nil rc for
		// the `<user>-clientconfig` Secret fetch.
		SArc:    rcFromCtx(resCtx),
		AuthnNS: authnNS,
		PerPage: -1,
		Page:    -1,
	})
	if err != nil {
		return fmt.Errorf("resolve RESTAction %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	encoded, err := encodeResolvedJSON(res)
	if err != nil {
		return fmt.Errorf("encode RESTAction %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	// Put under the per-user key — exactly the shape restactions.go
	// :212-216 puts under at serve time.
	handle.Put(key, &cache.ResolvedEntry{
		RawJSON: encoded,
		Inputs:  inputs,
	})

	// Record the self-dep + ensure the informer for the RESTAction GVR
	// is wired (AC-PIP.5 — without this the refresher never wakes for
	// the seeded entry; falsifier #5 triggers). Matches
	// restactions.go:229-230.
	ensureWatcherInformerForGVR(got.GVR)
	cache.Deps().Record(key, got.GVR, got.Unstructured.GetNamespace(), got.Unstructured.GetName())
	return nil
}

// seedOneWidget resolves ONE navigation widget under the cohort ctx
// and Puts the resolved JSON into the per-user widgets L1 under the
// dispatcher's canonical key. STRUCTURALLY MATCHES widgets.go:148-231:
//
//   - dispatchCacheLookupKey("widgets", group, version, resource, ns,
//     name, KeyPerPage, KeyPage, nil) with the DISPATCHER-LOOKUP key
//     tuple (Ship 0.30.187 D2 decoupling) so cohort A's first /call with
//     no URL slice params hits the SAME cell as the seed Put. Pre-D2
//     this used the RESOLUTION tuple (prewarmPageLimit()) which never
//     matched the dispatcher's paginationInfo default of (-1, -1) and
//     caused the 0.30.186 14/17 first-nav-hit defect.
//   - cache.WithL1KeyContext(ctx, key) before Resolve so the inner-call
//     dep tracker records edges.
//   - widgets.Resolve at widgets.go:187-193 (same entrypoint). The
//     RESOLUTION tuple (e.PerPage, e.Page) stays bounded by
//     prewarmPageLimit() — the 0.30.127 storm guard. For no-slice
//     navigation widgets the resolved output is structurally invariant
//     under pagination (no row fan-out at the top widget level — row
//     data flows from declared-slice child resourcesRefs which carry
//     their own URL-matching pagination).
//   - encodeResolvedJSON + cacheHandle.Put + recordWidgetDeps —
//     matches widgets.go:215-231 (recordWidgetDeps calls
//     ensureWatcherInformerForGVR for the widget GVR + apiRef GVR +
//     each resourcesRefs GVR, satisfying AC-PIP.5 for widgets).
func seedOneWidget(ctx context.Context, e navWidgetEntry, authnNS string) error {
	if e.W == nil {
		return nil
	}

	// inline-extras design P §5 / MUST-FIX #1 — PIP-seed key parity at the
	// per-cohort widget cell. The dispatcher keys this cell on the UNION of
	// both inline maps + request (§1); the seed has NO request extras, so the
	// union degenerates to unionForKey(apiRefInline, rrtInline, nil) read off
	// the widget CR (e.W). Without this fold the seed would key on extras=nil
	// while the dispatcher keys on the non-empty union → every widget
	// declaring inline extras MISSES its prewarmed cell and resolves cold on
	// first paint (the #317 seed/serve key-mismatch class). The seed BODY
	// parity is AUTOMATIC: widgets.Resolve reads the inline maps off in.Object
	// (the seeded CR) itself (resolveApiRef §4.1 + the §4.2 fold), so no
	// ResolveOptions.Extras field is needed — only this KEY arg is computed
	// outside Resolve and must be hand-threaded. Absent both blocks ⇒ {} + {}
	// ⇒ a fresh empty union ⇒ byte-identical to the pre-inline-extras nil arg
	// (HG-PIP.3 backward-compat). Falsifier #6 asserts the resulting HIT.
	seedKeyExtras := unionForKey(
		widgets.GetApiRefExtras(e.W.Object),
		widgets.GetResourcesRefsExtras(e.W.Object),
		nil,
	)

	// Ship 0.30.187 D2: the dispatcher-lookup key uses the KEY tuple
	// (KeyPerPage, KeyPage) — derived from the /call Path the walker
	// reached this widget through so the cell matches the dispatcher's
	// serve-time paginationInfo. The resolution tuple (e.PerPage,
	// e.Page) is still used for widgets.Resolve below (the 0.30.127
	// storm guard).
	key, handle, inputs := dispatchCacheLookupKey(ctx, "widgets",
		e.GVR.Group, e.GVR.Version, e.GVR.Resource,
		e.W.GetNamespace(), e.W.GetName(),
		e.KeyPerPage, e.KeyPage, seedKeyExtras)
	// Ship 0.30.188 — diagnostic slog: emit the widget seed Put cache
	// key + components so it can be diff'd against the dispatcher_get
	// and per_user_fallback_put log lines at widgets.go.
	emitDispatchCacheKeyDiag(slog.Default(), "seed", ctx,
		key, inputs, "widgets",
		e.GVR.Group, e.GVR.Version, e.GVR.Resource,
		e.W.GetNamespace(), e.W.GetName(),
		e.KeyPerPage, e.KeyPage, seedKeyExtras)
	if handle == nil || key == "" {
		// L1 disabled or no identity — same defensive skip as
		// seedOneRestaction.
		return nil
	}

	// #46: bound this seed unit's footprint (semaphore admission + per-unit
	// HeapInuse assert), AFTER the identity short-circuit so the customer
	// /call path is untouched. seedOneWidget is a SHARED primitive — both the
	// engine (serial) and legacy errgroup (concurrent) seed paths funnel here,
	// so this one bracket bounds BOTH aggregates. Transparent when
	// SEED_FOOTPRINT_BUDGET_BYTES==0.
	seedRelease, seedErr := enterSeedUnit(ctx, "widget/"+e.W.GetNamespace()+"/"+e.W.GetName())
	if seedErr != nil {
		return seedErr // ctx cancelled while blocked on the bound
	}
	defer seedRelease()

	// DeepCopy the widget CR — widgets.Resolve mutates its In object
	// (sets status.widgetData etc.). The harvester already DeepCopied
	// once, but the SAME copy is fed to N cohort goroutines; we MUST
	// give each cohort its own *unstructured to avoid the
	// shared-vs-copy-is-a-concurrency-change defect
	// (feedback_shared_vs_copy_is_a_concurrency_change.md).
	in := e.W.DeepCopy()

	// Ship 0.30.193 Checkpoint 1 — install per-widget PIP timing sink.
	// Mirrors the restaction shape at lines 802-813: sink lives for the
	// duration of THIS widget's resolve; the deferred log emits a
	// phase1.seed.widget.timing line with widget identity + total
	// wall-clock + stages (the widget's apiref phase re-enters
	// restactions.Resolve which itself appends per-stage entries to the
	// SAME sink, so per-restaction stage breakdowns flow through here).
	//
	// SINK ISOLATION (feedback_shared_vs_copy_is_a_concurrency_change):
	// one sink per seedOneWidget invocation; never shared across
	// widgets or cohorts.
	stageTimingSink := cache.NewPIPStageTimingSink()
	widgetStart := time.Now()
	defer func() {
		snapshot := stageTimingSink.Snapshot()
		slog.Default().Info("phase1.seed.widget.timing",
			slog.String("subsystem", "cache"),
			slog.String("widget", e.W.GetNamespace()+"/"+e.W.GetName()),
			slog.String("gvr", e.GVR.String()),
			slog.Int64("elapsed_ms_total", time.Since(widgetStart).Milliseconds()),
			slog.Int("stages_total", len(snapshot)),
			slog.Any("stages", snapshot),
		)
	}()

	resCtx := cache.WithL1KeyContext(ctx, key)
	resCtx = cache.WithPIPStageTimingSink(resCtx, stageTimingSink)

	res, err := widgets.Resolve(resCtx, widgets.ResolveOptions{
		In: in,
		// Ship 0.30.230 fix-at-root: RC is the SA *rest.Config carried
		// on ctx by withCohortSeedContext upstream. Threading it here
		// fixes the nil-rc crash at crdschema.ValidateObjectStatus →
		// cache.GVRFor → discoverPluralInfo (the four-revert root cause).
		RC:      rcFromCtx(resCtx),
		AuthnNS: authnNS,
		PerPage: e.PerPage,
		Page:    e.Page,
	})
	if err != nil {
		return fmt.Errorf("resolve widget %s/%s: %w", e.W.GetNamespace(), e.W.GetName(), err)
	}

	encoded, err := encodeResolvedJSON(res)
	if err != nil {
		return fmt.Errorf("encode widget %s/%s: %w", e.W.GetNamespace(), e.W.GetName(), err)
	}

	handle.Put(key, &cache.ResolvedEntry{
		RawJSON: encoded,
		Inputs:  inputs,
	})

	// Record widget deps — self + apiRef + render-eligible
	// resourcesRefs. Matches widgets.go:230. recordWidgetDeps ensures
	// the informer for every recorded GVR is wired (AC-PIP.5 / falsifier
	// #5).
	recordWidgetDeps(slog.Default(), key, e.GVR, res)

	// Ship 4a (0.30.198) — prewarm + PIN the page-independent RAFullList
	// cell for this (widget→RESTAction × cohort). The cell survives LRU
	// thrash (resident region) so the cohort's FIRST paginated /call hits a
	// warm full-list and is served as a cheap Go-slice — the zero-cold-nav
	// requirement (feedback_zero_cold_navigations_hard_requirement). Best-
	// effort: a prewarm error is log-only and never fails the widget seed
	// (the cohort's per-user widget cell above already seeded; RAFullList is
	// an accelerator). NON-FATAL by design.
	seedRAFullListForWidget(resCtx, in, authnNS, e.W.GetNamespace(), e.W.GetName())
	return nil
}

// seedRAFullListForWidget prewarms + pins the page-independent RAFullList
// cell for a widget's underlying RESTAction, under the cohort ctx — Ship 4a
// (0.30.198). It resolves the widget's apiRef at a PAGINATED tuple
// (prewarmPageLimit, page 1) so apiref.Resolve engages raFullListServe,
// which: resolves the RA UNPAGINATED, byte-verifies sliceability for the
// apiRef shape, Puts the full cell (pinned when the cost predicate fires —
// envelope bytes ≥ threshold), and records the verdict. Reusing the serve
// path means ZERO duplicated slice/verify/pin logic and guarantees the
// prewarmed cell is byte-identical to what the first /call would build.
//
// Best-effort: the function swallows errors (log-only). A widget with no
// apiRef (e.g. a static-data widget) is a no-op. CACHE off → apiref.Resolve's
// raFullListServe nil-checks the cache and returns served=false → the resolve
// is a harmless extra read (only reached when ResolvedCacheEnabled, see the
// guard below). The whole block is gated under cache.ResolvedCacheEnabled()
// so a cache-off process never runs the extra resolve.
func seedRAFullListForWidget(ctx context.Context, w *unstructured.Unstructured, authnNS, ns, name string) {
	if !cache.ResolvedCacheEnabled() {
		return
	}
	apiRef, err := widgets.GetApiRef(w.Object)
	if err != nil || apiRef.Name == "" || apiRef.Namespace == "" {
		// No apiRef (static widget) or unparseable — nothing to prewarm.
		return
	}
	// inline-extras design P §5 / MUST-FIX #1 — PIP-seed key parity at the
	// RAFullList sub-cell. The dispatcher's apiRef path keys this sub-cell on
	// the apiRef-EFFECTIVE map (merge(apiRefInline, request)) via
	// apiref.Resolve → RAFullListKeyInputs (ra_full_list.go). The seed has no
	// request extras, so the effective map degenerates to the apiRef-inline
	// map read off the widget CR. Threading it into apiref.Resolve's Extras
	// below makes the seed's RAFullList key fold the SAME apiRef-inline map the
	// dispatcher's first /call will → seed-key == serve-key on this sub-cell.
	// The resourcesRefsTemplateExtras map is correctly NOT folded here — it
	// does not affect the apiRef fetch (§1). Absent ⇒ {} ⇒ byte-identical to
	// the pre-inline-extras nil Extras (extrasMinusSlice({}) → nil → no fold).
	apiRefInline := widgets.GetApiRefExtras(w.Object)
	// Resolve at a paginated tuple so raFullListServe engages (it requires
	// perPage>0 && page>0). page 1 + prewarmPageLimit is sufficient: the
	// byte-verify + pin are per-(RA × shape), NOT per-page, so this single
	// prewarm populates+verifies+pins the cell for EVERY subsequent (page,
	// perPage) /call by this cohort. The result is discarded (we only want
	// the cell-populating side-effect).
	pp := prewarmPageLimit()
	if pp <= 0 {
		pp = 1
	}
	if _, rerr := apiref.Resolve(ctx, apiref.ResolveOptions{
		ApiRef: apiRef,
		// Ship 0.30.230 fix-at-root: thread the SA rc explicitly so the
		// downstream restactions.ResolveOptions SArc field chain inside
		// apiref.Resolve carries a non-nil rc. The ctx upstream
		// (seedOneWidget's resCtx) already carries it via
		// withCohortSeedContext / WithInternalRESTConfig — this makes
		// the option-struct propagation explicit and matches the rest
		// of the construction-site fixes.
		RC:      rcFromCtx(ctx),
		AuthnNS: authnNS,
		PerPage: pp,
		Page:    1,
		// inline-extras P §5 — fold the apiRef-inline map so the RAFullList
		// key matches the dispatcher's apiRef-effective key (seed has no
		// request extras). NOT the union — rrt-inline must not key this cell.
		Extras: apiRefInline,
	}); rerr != nil {
		slog.Default().Warn("phase1.seed.rafulllist.skipped",
			slog.String("subsystem", "cache"),
			slog.String("widget", ns+"/"+name),
			slog.String("apiref", apiRef.Namespace+"/"+apiRef.Name),
			slog.Any("err", rerr),
			slog.String("effect", "RAFullList prewarm skipped for this (widget,cohort); first /call cold-resolves + pins lazily"),
		)
	}
}

// cohortLogLabel renders a cohort into a stable log/metric label. The
// label is used in structured log fields AND as the expvar map key for
// the per-cohort counters; it MUST be stable across pod restarts (which
// EnumerateRBACCohorts's sort ordering guarantees).
//
// User-kind cohort: the canonical Username (e.g. "system:admin",
// "alice@example.com"). Group-kind cohort: "group:" + the group name.
// A cohort with neither (defensive — should never happen post-enum)
// falls back to "anonymous".
func cohortLogLabel(c seedTarget) string {
	if c.Username != "" {
		return c.Username
	}
	if len(c.Groups) > 0 {
		return "group:" + c.Groups[0]
	}
	return "anonymous"
}
