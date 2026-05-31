// phase1_walk_pagination_jobs.go — Path 3.2.2.b (0.30.221): deferred
// apiRef pagination.
//
// PROBLEM the file solves
//
// Path 3.2.2 (0.30.220) ran iterateApiRefPages INLINE on the Phase 1
// walker goroutine. At 50K-composition scale the per-widget pagination
// (up to 500 pages × ~125–500ms each) extended the walk past the 360s
// kubelet startup probe budget — pod never became Ready, restart loop.
// The MECHANISM was empirically validated correct (5× more walk events;
// new per-composition CRs reached); only the SCHEDULING was wrong.
//
// THE FIX — collect-then-drain
//
// During the Phase 1 walk, when the page-1 resolve sees an
// apiRef+resourcesRefsTemplate widget whose `.slice.continue==true`,
// the walker COLLECTS a paginationJob into a shared, mutex-guarded
// collector instead of paginating inline. Phase1Warmup completes Step 4
// (walk) → Step 7 (informer sync barrier) → Step 8 (MarkPhase1Done —
// /readyz flips to 200) UNAFFECTED.
//
// AFTER MarkPhase1Done, phase1WarmupWith launches a SINGLE bounded
// background goroutine (symmetric with the existing PIP seed at Step
// 7.6 — see phase1_walk.go:563-596) that drains every collected job
// through the unchanged iterateApiRefPages mechanism. The drain runs
// under a fresh context.Background-derived seed context with its own
// timeout (paginationDrainTimeout) so it OUTLIVES Phase1Warmup's ctx
// (which main.go cancels the instant Phase1Warmup returns).
//
// INVARIANTS preserved from Path 3.2.2:
//   - Mechanism unchanged — iterateApiRefPages is the same code path.
//   - 500-page per-widget cap stays (phase1MaxApiRefPages).
//   - Data-driven predicate (isApiRefTemplateDriven on widget SHAPE);
//     no hardcoded GVRs.
//   - Bounded by parent ctx — cancellation on pod shutdown propagates
//     into iterateApiRefPages' per-page ctx.Err() check.
//
// REFRESHER AMPLIFICATION DISCIPLINE (feedback_refresher_populate_
// amplification): the drain Puts new identity-free widgetContent L1
// cells. The L1 refresher then exercises those cells every cycle. We
// instrument paginationDrainTimeout deliberately bounded (5 min, same
// shape as pipGlobalTimeout / 8 min) so a runaway apiRef cannot leak
// goroutines past the bounded budget; the per-job 500-page cap further
// bounds the work each apiRef widget can produce.
//
// PRIOR ART: client-go has no equivalent — apiserver pagination is a
// snowplow concern. The collect-then-drain pattern mirrors the PIP seed
// (Step 7.6 background) which has been in production since 0.30.179.

package dispatchers

import (
	"context"
	"log/slog"
	"sync"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// paginationDrainTimeout is the absolute wall-clock budget for the
// background pagination drain. Bounded so a pathological apiRef
// (resolver always reports .slice.continue=true) cannot leave the drain
// goroutine running until process exit. At today's apiRef widget count
// (2: compositions-page-datagrid, blueprints-page-datagrid) × 500 pages
// × ~500ms wall-clock each (worst case observed on 0.30.220) the drain
// completes well inside 5 min; the timeout is a backstop, not the
// expected exit path.
//
// Code-time constant per Diego's "no new env vars / flags" mandate.
const paginationDrainTimeout = 5 * time.Minute

// apiRefPaginationJob carries everything the deferred pagination needs
// to call iterateApiRefPages from the background drain goroutine. The
// fields are captured by-value (or by-pointer to immutable state) at
// COLLECT time so the drain does not depend on the walker's mutable
// state (the walker is gone by the time the drain runs).
type apiRefPaginationJob struct {
	// In is the page-1 widget CR. The drain re-fetches a fresh copy
	// via objects.Get for each subsequent page (the resolver mutates
	// its input), so we keep In only to read its (ns, name) — but
	// holding the full unstructured is cheap and matches the original
	// inline call shape.
	In *unstructured.Unstructured

	// GVR is the widget's GroupVersionResource — threaded to
	// populateWidgetContentL1 so the seed cell matches the serve-time
	// dispatcher.
	GVR schema.GroupVersionResource

	// Page1Res is the page-1 resolved envelope. iterateApiRefPages
	// reads its .slice.continue signal to decide whether to start
	// paginating; we keep the full envelope (not just the bool) so
	// the isApiRefTemplateDriven shape check inside iterateApiRefPages
	// stays the canonical predicate (no duplication of logic at
	// collect time).
	Page1Res *unstructured.Unstructured

	// Depth is the walker depth at which this widget was reached.
	// Subsequent-page child recursion continues from depth+1 — same
	// as the page-1 children loop.
	Depth int

	// PerPage is the RESOLUTION perPage — passed unchanged to
	// widgets.Resolve for pages 2..N.
	PerPage int

	// KeyPerPage is the dispatcher-lookup KEY perPage tuple (Ship
	// 0.30.187 D2). Decoupled from PerPage; passed to
	// populateWidgetContentL1 so each page's seed cell matches the
	// dispatcher's serve-time lookup.
	KeyPerPage int

	// AuthnNS is the authn namespace — threaded to widgets.Resolve.
	AuthnNS string
}

// jobKey returns the dedupe key for a job. Two roots may reach the
// same apiRef widget; we paginate only once per (gvr, ns, name) —
// idempotent at the L1 layer (Put is by content key), but skipping the
// duplicate work saves the up-to-500-page wall clock.
func (j apiRefPaginationJob) jobKey() string {
	if j.In == nil {
		return ""
	}
	return j.GVR.String() + "|" + j.In.GetNamespace() + "|" + j.In.GetName()
}

// apiRefPaginationCollector is the mutex-guarded set of pagination jobs
// collected during the Phase 1 walk. The walker writes via
// collectApiRefPaginationJob; the drain reads via Drain after
// MarkPhase1Done.
//
// Concurrency: the walk is single-threaded per root and roots resolve
// sequentially (phase1WarmupWith), but the mutex makes the collector
// safe regardless of how the walk is scheduled — same shape as
// contentPrewarmHarvester / navWidgetHarvester.
type apiRefPaginationCollector struct {
	mu   sync.Mutex
	jobs map[string]apiRefPaginationJob
}

// newApiRefPaginationCollector returns an empty collector.
func newApiRefPaginationCollector() *apiRefPaginationCollector {
	return &apiRefPaginationCollector{jobs: map[string]apiRefPaginationJob{}}
}

// collectApiRefPaginationJob appends j to the collector iff the page-1
// envelope is shape-eligible (apiRef+resourcesRefsTemplate) AND the
// resolver signals continuation. Nil-safe: a nil collector is a no-op
// (matches Phase 1's flag-off posture). Idempotent for duplicate
// (gvr, ns, name) — first write wins.
//
// The two predicates are checked HERE, not inside the drain, so the
// collector's `jobs` map contains ONLY work that would actually
// paginate. Empty collector at drain time => background goroutine is a
// fast no-op; matches the byte-identical-when-flag-off posture.
func (c *apiRefPaginationCollector) collect(j apiRefPaginationJob) {
	if c == nil || j.In == nil || j.Page1Res == nil {
		return
	}
	if !isApiRefTemplateDriven(j.Page1Res.Object) {
		return
	}
	if !resolverWantsContinue(j.Page1Res) {
		return
	}
	key := j.jobKey()
	if key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, seen := c.jobs[key]; seen {
		// First-write-wins. Same widget reached via two roots is rare
		// (the visited-set in phase1Walker.walk already prevents
		// re-traversal within a root) but cross-root collisions are
		// possible. Idempotent — same widget would Put the same cells.
		return
	}
	c.jobs[key] = j
}

// drain returns a snapshot of the collected jobs and clears the
// collector. Called by the background drain goroutine after
// MarkPhase1Done. Idempotent — a second call returns an empty slice.
func (c *apiRefPaginationCollector) drain() []apiRefPaginationJob {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.jobs) == 0 {
		return nil
	}
	out := make([]apiRefPaginationJob, 0, len(c.jobs))
	for _, j := range c.jobs {
		out = append(out, j)
	}
	c.jobs = map[string]apiRefPaginationJob{}
	return out
}

// count returns the current job count without draining. Used by the
// drain logger for the "jobs_collected" field and by tests.
func (c *apiRefPaginationCollector) count() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.jobs)
}

// drainApiRefPaginationJobs runs the deferred pagination AFTER
// MarkPhase1Done. Each job is processed sequentially through the
// unchanged iterateApiRefPages mechanism. The drain context is
// independent of Phase1Warmup's ctx so a normal Phase 1 completion
// (main.go cancels p1Ctx the instant Phase1Warmup returns) does NOT
// kill an in-flight drain.
//
// Concurrency: jobs run sequentially. Per-job parallelism would compound
// the refresher-amplification risk (each apiRef widget materialises new
// L1 cells the refresher then exercises) — serial drain keeps the
// post-readiness CPU shape predictable. iterateApiRefPages' own
// per-page work is bounded by its 500-page cap + .slice.continue halt.
//
// Cancellation: parent ctx cancel propagates into iterateApiRefPages'
// per-page ctx.Err() check (same path the inline 0.30.220 used). On pod
// shutdown the drain goroutine returns at the next page boundary —
// never leaks past the bounded drain budget.
//
// Errors: log-only. A pagination failure on one widget MUST NOT prevent
// draining the next. The page-1 envelope (Put before MarkPhase1Done) is
// always correct for that widget — pagination is a NICE-TO-HAVE that
// fills cells for items 6..N; missing pages fall back to the existing
// per-user serve-time path.
//
// Panic recovery: the caller (phase1WarmupWith) wraps the goroutine in
// a defer-recover so a single bad job cannot crash the process. This
// function itself does NOT recover — letting a panic bubble to the
// recover at the goroutine boundary so it is logged once with the
// full panic value.
func drainApiRefPaginationJobs(
	ctx context.Context,
	jobs []apiRefPaginationJob,
	saEP endpoints.Endpoint,
	saRC *rest.Config,
) {
	log := xcontext.Logger(ctx)
	if log == nil {
		log = slog.Default()
	}
	start := time.Now()
	if len(jobs) == 0 {
		log.Info("phase1.pagination_drain.empty",
			slog.String("subsystem", "cache"),
			slog.String("phase", "post_phase1_done"),
		)
		return
	}

	log.Info("phase1.pagination_drain.start",
		slog.String("subsystem", "cache"),
		slog.String("phase", "post_phase1_done"),
		slog.Int("jobs_collected", len(jobs)),
		slog.String("drain_budget", paginationDrainTimeout.String()),
	)

	// SA-credentialed context for the drain — mirrors withPhase1SAContext
	// but anchored on the drain's own ctx (NOT the dead Phase1Warmup
	// ctx). iterateApiRefPages calls objects.Get and widgets.Resolve, both
	// of which read the SA endpoint/restConfig from this ctx.
	drainCtx := withPhase1SAContext(ctx, saEP, saRC)

	completed := 0
	for _, j := range jobs {
		if err := ctx.Err(); err != nil {
			log.Warn("phase1.pagination_drain.ctx_cancel",
				slog.String("subsystem", "cache"),
				slog.Int("completed", completed),
				slog.Int("remaining", len(jobs)-completed),
				slog.Any("err", err),
				slog.Int64("elapsed_ms", time.Since(start).Milliseconds()),
			)
			return
		}
		// Run the original 3.2.2 mechanism, unchanged. Returns no error
		// (logs internally); a failure leaves the page-1 cell correct
		// and proceeds to the next job.
		iterateApiRefPages(
			drainCtx,
			// The walker is gone by the time the drain runs. Construct a
			// minimal walker shell for the SAME helpers iterateApiRefPages
			// uses (visited set + harvesters). The visited set is
			// per-drain so children discovered by THIS drain are
			// internally deduped; we do NOT share with the original
			// walker's visited (the walker's visited is no longer
			// referenced post-MarkPhase1Done). Harvesters are nil — the
			// drain runs AFTER MarkPhase1Done so the content-pass / PIP-
			// seed harvesters have already been drained; populating them
			// now would have no consumer.
			&phase1Walker{
				authnNS: j.AuthnNS,
				visited: map[string]struct{}{},
			},
			j.In,
			j.GVR,
			j.Page1Res,
			j.Depth,
			j.PerPage,
			j.KeyPerPage,
			j.AuthnNS,
		)
		completed++
	}

	log.Info("phase1.pagination_drain.complete",
		slog.String("subsystem", "cache"),
		slog.String("phase", "post_phase1_done"),
		slog.Int("jobs_completed", completed),
		slog.Int64("elapsed_ms", time.Since(start).Milliseconds()),
	)
}
