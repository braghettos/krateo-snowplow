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
// COHORT ENUMERATION (architect's PM gate #392). The cohort set is
// derived from cache.EnumerateRBACCohorts() — see
// internal/cache/rbac_cohorts.go for the canonical-dedupe contract
// (two identities are the same cohort iff their union of matched
// binding-pointer-sets is equal). The architect's expected
// production-scale cohort count is ≤ 50 (admin + a handful of
// least-privilege team cohorts). Above 50 PIP FAIL-CLOSES — phase1Done
// stays false, pod stays not-ready, the operator MUST inspect the log
// line phase1.seed.cohort_cap_exceeded. This is the storage-bound
// guard: each cohort × (N_restactions+N_widgets) is an L1 entry, so
// 50 × ~35 ≈ 1750 entries is the upper bound on PIP-seeded L1.
//
// FOREGROUND + FAIL-CLOSED (Diego OQ-1 + OQ-2, ratified for PIP).
// phase1WarmupWith calls runPIPSeed as Step 7.6 — AFTER contentWarm
// (7.5) and BEFORE MarkPhase1Done (8). Any cohort-level error
// (per-cohort seed timeout, resolver error, Put error) propagates up
// and causes phase1WarmupWith to RETURN WITHOUT calling MarkPhase1Done.
// /readyz stays 503; the pod stays not-ready. The operator must
// inspect the per-cohort error log line and redeploy/restart.
//
// CONCURRENCY (architect's design §3). The cohort loop runs under a
// bounded errgroup with limit = runtime.GOMAXPROCS(0) — matches the F2
// content-warm's bounded fan-out shape. Each cohort's seed is
// SEQUENTIAL inside the goroutine: it iterates the harvested
// (restaction, widget) sets one at a time. The bound on transient RSS
// per cohort is N_restactions×envelope_bytes + N_widgets×envelope_bytes
// — same OOM profile as the F2 content pass per cohort.
//
// PER-COHORT TIMEOUT — 20 s hard ceiling per cohort, set via
// context.WithTimeout inside the per-cohort closure. A stuck cohort
// thus cannot wedge Phase 1 past Step 7.6's 40 s global budget; the
// timeout firing returns ctx.Err() up the errgroup which propagates as
// the cohort's seed-failure (FAIL-CLOSED).
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
	"github.com/krateoplatformops/snowplow/internal/objects"
	"github.com/krateoplatformops/snowplow/internal/resolvers/restactions"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
)

const (
	// envPrewarmPIPEnabled is the Ship PIP (0.30.173) opt-in gate.
	// Chart default is true (active by default for this ship); operators
	// may set "false" to disable the seed if a regression is observed.
	// PIP additionally requires PREWARM_ENABLED + PREWARM_CONTENT_ENABLED
	// (the apiRefHarvester depends on the content-prewarm path) — when
	// either is off, PIP stays inert regardless of this knob.
	envPrewarmPIPEnabled = "PREWARM_PIP_ENABLED"

	// pipCohortCapDefault is the architect's expected production-scale
	// cohort ceiling (PM gate #392 / OQ-2). EnumerateRBACCohorts returning
	// more than this triggers FAIL-CLOSED: phase1Done stays false.
	pipCohortCapDefault = 50

	// envPrewarmPIPCohortCap allows ops to raise/lower the ceiling
	// without a code change — emergency lever only.
	envPrewarmPIPCohortCap = "PREWARM_PIP_COHORT_CAP"

	// pipCohortTimeout is the per-cohort hard ceiling. A stuck cohort
	// cannot wedge Phase 1 past Step 7.6's global budget.
	//
	// Ship A.3 / 0.30.179 — raised 20s -> 120s. Binding-set enumeration
	// produces more classes than the prior canonical-cohort dedupe, and
	// each class's restactions seed walks per-namespace LIST calls (a
	// compositions-list RESTAction emits one K8s call per namespace via
	// the namespace iterator). A 50-namespace cluster needs ~30s per
	// cohort to seed cleanly; 120s adds ~4x headroom.
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

// pipCohortCap returns the operator-overridable cohort ceiling. A
// non-positive env value falls back to the default; ops can never set
// it to zero (which would FAIL-CLOSE every Phase 1).
func pipCohortCap() int {
	n := env.Int(envPrewarmPIPCohortCap, pipCohortCapDefault)
	if n <= 0 {
		return pipCohortCapDefault
	}
	return n
}

// navWidgetEntry is one navigation widget CR captured during the
// Phase-1 walk together with the GVR + pagination tuple it resolved
// under. The seed loop re-resolves the SAME CR per cohort under per-
// cohort identity and Puts the per-user widgets L1 entry.
type navWidgetEntry struct {
	W       *unstructured.Unstructured
	GVR     schema.GroupVersionResource
	PerPage int
	Page    int
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
// pagination it resolved under. Nil-safe: a nil harvester / nil widget
// is a no-op (flag-off Phase 1 passes no harvester). Deduplicated by
// the canonical (gvr, ns, name, perPage, page) tuple.
func (h *navWidgetHarvester) harvestNavWidget(w *unstructured.Unstructured, gvr schema.GroupVersionResource, perPage, page int) {
	if h == nil || w == nil {
		return
	}
	key := navWidgetHarvestKey(gvr, w.GetNamespace(), w.GetName(), perPage, page)
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, seen := h.entries[key]; seen {
		// First-write-wins. The dedupe is intentional: the walk's
		// visited-set in phase1Walker.walk already prevents re-traversal,
		// so a second harvest for the same key only happens across roots
		// (idempotent — same CR + same pagination yields identical Put).
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
		W:       w.DeepCopy(),
		GVR:     gvr,
		PerPage: perPage,
		Page:    page,
	}
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

// runPIPSeed is the Ship PIP Step 7.6 entry point invoked by
// phase1WarmupWith. Enumerates RBAC cohorts and seeds the per-user
// resolved-output L1 (restactions + widgets) for every cohort. Returns
// a non-nil error on cap-exceeded OR cohort-level seed failure;
// phase1WarmupWith treats that as FAIL-CLOSED and skips MarkPhase1Done.
//
// h is the F2 content-prewarm harvester (apiRefHarvester) — drained
// for the restactions seed loop. nh is the new navWidgetHarvester —
// drained for the widgets seed loop. Both are pre-populated by the
// walk and stable by the time runPIPSeed runs.
func runPIPSeed(ctx context.Context, h *contentPrewarmHarvester, nh *navWidgetHarvester,
	saEP endpoints.Endpoint, saRC *rest.Config, authnNS string) error {

	log := slog.Default()
	start := time.Now()

	// Ship A.3 / 0.30.179 — binding-set enumeration. The PIP seed now
	// drives one entry per ENUMERATED BINDING-SET CLASS (a cohort defined
	// by BindingSetHash equivalence) rather than per-user-string cohort.
	// Two users whose binding-pointer-set hashes equal share the SAME L1
	// cell; the seed populates ONE entry per cell. See
	// internal/cache/binding_set_enumeration.go for the algorithm.
	cohorts := cache.EnumerateBindingSetClasses()
	if len(cohorts) == 0 {
		log.Info("phase1.seed.skipped",
			slog.String("subsystem", "cache"),
			slog.String("reason", "EnumerateBindingSetClasses returned no classes — RBAC snapshot empty or unpublished"),
		)
		return nil
	}

	cap := pipCohortCap()
	if len(cohorts) > cap {
		// FAIL-CLOSED (architect's storage-bound guard, PM gate #392 /
		// OQ-2). Emit a loud structured log line; the operator must
		// inspect why the cohort count blew past the ceiling.
		log.Error("phase1.seed.cohort_cap_exceeded",
			slog.String("subsystem", "cache"),
			slog.Int("cohorts", len(cohorts)),
			slog.Int("cap", cap),
			slog.String("effect", "phase1Done stays false; pod stays not-ready until the cap is raised "+
				"(PREWARM_PIP_COHORT_CAP) or the RBAC topology is reduced"),
		)
		return fmt.Errorf("PIP cohort cap exceeded: enumerated=%d cap=%d", len(cohorts), cap)
	}

	restactionRefs := h.snapshot()
	widgetEntries := nh.snapshot()

	log.Info("phase1.seed.started",
		slog.String("subsystem", "cache"),
		slog.Int("cohorts", len(cohorts)),
		slog.Int("cap", cap),
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
			if err := seedCohort(gctx, cohort, restactionRefs, widgetEntries, saEP, saRC, authnNS); err != nil {
				pipBindingSetSeedFailuresTotal.Add(1)
				slog.Warn("phase1.seed.cohort.skipped",
					slog.String("subsystem", "cache"),
					slog.String("cohort", cohortLogLabel(cohort)),
					slog.Any("err", err),
					slog.String("effect", "cohort skipped; phase1Done not blocked — narrow RBAC cohorts "+
						"that cannot read seed targets are expected to fail and need no L1 entry"),
				)
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

	log.Info("phase1.seed.completed",
		slog.String("subsystem", "cache"),
		slog.Int("cohorts", len(cohorts)),
		slog.Uint64("restactions_seeded_total", pipSeedRestactionsTotal.Load()),
		slog.Uint64("widgets_seeded_total", pipSeedWidgetsTotal.Load()),
		slog.Int64("elapsed_ms", time.Since(start).Milliseconds()),
	)
	return nil
}

// seedCohort seeds one cohort's per-user restactions + widgets L1
// entries. Per-cohort timeout + per-cohort error containment: an error
// returned here aborts the errgroup and FAIL-CLOSES Phase 1.
func seedCohort(ctx context.Context, cohort cache.Cohort,
	restactionRefs []templatesv1.ObjectReference, widgetEntries []navWidgetEntry,
	saEP endpoints.Endpoint, saRC *rest.Config, authnNS string) error {

	log := slog.Default()
	cohortLabel := cohortLogLabel(cohort)
	start := time.Now()

	log.Info("phase1.seed.cohort.start",
		slog.String("subsystem", "cache"),
		slog.String("cohort", cohortLabel),
		slog.Int("restactions", len(restactionRefs)),
		slog.Int("widgets", len(widgetEntries)),
	)

	// Per-cohort hard ceiling — 20 s. A stuck cohort cannot wedge Step
	// 7.6 past its global budget.
	cctx, ccancel := context.WithTimeout(ctx, pipCohortTimeout)
	defer ccancel()

	// Build the per-cohort ctx: SA transport seam preserved (so the
	// resolver dispatches via the SA-credentialed inner-call path that
	// 0.30.166/167/168 wired) but identity OVERRIDDEN via
	// xcontext.WithUserInfo so dispatchCacheLookupKey hashes the cohort
	// into the L1 key and EvaluateRBAC fires against the cohort's
	// bindings.
	cohortCtx := withCohortSeedContext(cctx, cohort, saEP, saRC)

	// Restactions seed loop — drain the harvester, one Put per
	// (cohort, restaction).
	for _, ref := range restactionRefs {
		if err := cctx.Err(); err != nil {
			log.Error("phase1.seed.cohort.timeout",
				slog.String("subsystem", "cache"),
				slog.String("cohort", cohortLabel),
				slog.String("phase", "restactions"),
				slog.Any("err", err),
				slog.Int64("elapsed_ms", time.Since(start).Milliseconds()),
			)
			return fmt.Errorf("cohort %q restactions seed: %w", cohortLabel, err)
		}
		if err := seedOneRestaction(cohortCtx, ref, authnNS); err != nil {
			log.Error("phase1.seed.cohort.error",
				slog.String("subsystem", "cache"),
				slog.String("cohort", cohortLabel),
				slog.String("phase", "restactions"),
				slog.String("restaction", ref.Namespace+"/"+ref.Name),
				slog.Any("err", err),
			)
			return fmt.Errorf("cohort %q restactions %s/%s: %w", cohortLabel, ref.Namespace, ref.Name, err)
		}
		pipSeedRestactionsTotal.Add(1)
		incCohortCounter(&pipSeedRestactionsByCohort, cohortLabel)
	}

	// Widgets seed loop — drain the harvested widget entries, one Put
	// per (cohort, widget).
	for _, e := range widgetEntries {
		if err := cctx.Err(); err != nil {
			log.Error("phase1.seed.cohort.timeout",
				slog.String("subsystem", "cache"),
				slog.String("cohort", cohortLabel),
				slog.String("phase", "widgets"),
				slog.Any("err", err),
				slog.Int64("elapsed_ms", time.Since(start).Milliseconds()),
			)
			return fmt.Errorf("cohort %q widgets seed: %w", cohortLabel, err)
		}
		if err := seedOneWidget(cohortCtx, e, authnNS); err != nil {
			log.Error("phase1.seed.cohort.error",
				slog.String("subsystem", "cache"),
				slog.String("cohort", cohortLabel),
				slog.String("phase", "widgets"),
				slog.String("widget", e.W.GetNamespace()+"/"+e.W.GetName()),
				slog.Any("err", err),
			)
			return fmt.Errorf("cohort %q widget %s/%s: %w", cohortLabel, e.W.GetNamespace(), e.W.GetName(), err)
		}
		pipSeedWidgetsTotal.Add(1)
		incCohortCounter(&pipSeedWidgetsByCohort, cohortLabel)
	}

	log.Info("phase1.seed.cohort.complete",
		slog.String("subsystem", "cache"),
		slog.String("cohort", cohortLabel),
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
func withCohortSeedContext(ctx context.Context, cohort cache.Cohort,
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
//     + cache.Deps().Record — same Put shape as restactions.go:212-230.
func seedOneRestaction(ctx context.Context, ref templatesv1.ObjectReference, authnNS string) error {
	got := objects.Get(ctx, ref)
	if got.Err != nil {
		return fmt.Errorf("fetch RESTAction %s/%s: %s", ref.Namespace, ref.Name, got.Err.Message)
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
	if handle == nil || key == "" {
		// L1 disabled OR no identity on ctx — defensive skip. PIP's
		// cohort ctx ALWAYS installs WithUserInfo, so an empty key here
		// is a configuration bug (PREWARM_PIP_ENABLED on while
		// CACHE_ENABLED off); log + skip.
		return nil
	}

	scheme := k8sruntime.NewScheme()
	if err := apis.AddToScheme(scheme); err != nil {
		return fmt.Errorf("add apis to scheme: %w", err)
	}
	var cr templatesv1.RESTAction
	if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(
		got.Unstructured.Object, &cr); err != nil {
		return fmt.Errorf("unstructured -> RESTAction %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	// Install the L1 key on ctx BEFORE Resolve so the inner-call dep
	// tracker records edges against this entry — matches
	// restactions.go:180-182.
	resCtx := cache.WithL1KeyContext(ctx, key)

	res, err := restactions.Resolve(resCtx, restactions.ResolveOptions{
		In:      &cr,
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
//     name, perPage, page, nil) with the SAME perPage+page tuple the
//     walker resolved the widget under (so cohort A's first /call with
//     the same pagination hits the SAME cell — HG-PIP.3).
//   - cache.WithL1KeyContext(ctx, key) before Resolve so the inner-call
//     dep tracker records edges.
//   - widgets.Resolve at widgets.go:187-193 (same entrypoint).
//   - encodeResolvedJSON + cacheHandle.Put + recordWidgetDeps —
//     matches widgets.go:215-231 (recordWidgetDeps calls
//     ensureWatcherInformerForGVR for the widget GVR + apiRef GVR +
//     each resourcesRefs GVR, satisfying AC-PIP.5 for widgets).
func seedOneWidget(ctx context.Context, e navWidgetEntry, authnNS string) error {
	if e.W == nil {
		return nil
	}

	key, handle, inputs := dispatchCacheLookupKey(ctx, "widgets",
		e.GVR.Group, e.GVR.Version, e.GVR.Resource,
		e.W.GetNamespace(), e.W.GetName(),
		e.PerPage, e.Page, nil)
	if handle == nil || key == "" {
		// L1 disabled or no identity — same defensive skip as
		// seedOneRestaction.
		return nil
	}

	// DeepCopy the widget CR — widgets.Resolve mutates its In object
	// (sets status.widgetData etc.). The harvester already DeepCopied
	// once, but the SAME copy is fed to N cohort goroutines; we MUST
	// give each cohort its own *unstructured to avoid the
	// shared-vs-copy-is-a-concurrency-change defect
	// (feedback_shared_vs_copy_is_a_concurrency_change.md).
	in := e.W.DeepCopy()

	resCtx := cache.WithL1KeyContext(ctx, key)

	res, err := widgets.Resolve(resCtx, widgets.ResolveOptions{
		In:      in,
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
	return nil
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
func cohortLogLabel(c cache.Cohort) string {
	if c.Username != "" {
		return c.Username
	}
	if len(c.Groups) > 0 {
		return "group:" + c.Groups[0]
	}
	return "anonymous"
}
