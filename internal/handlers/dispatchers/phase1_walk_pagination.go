// phase1_walk_pagination.go — Path 3.2.2 (0.30.220): walker pagination
// for apiRef+resourcesRefsTemplate widgets.
//
// PROBLEM the file solves
//
// The F2 INIT walker (phase1_walk.go) resolves each navigation widget
// exactly ONCE at the declared/default `(perPage, page)` tuple. For an
// apiRef+resourcesRefsTemplate-driven widget the resolver fans the
// template over the apiRef RA's sliced result — page 1 only — and the
// walker reads `status.resourcesRefs.items[]` to discover children. With
// perPage=5 (the bounded `PREWARM_PAGE_LIMIT` default per Ship 0.30.127),
// the compositions-page-datagrid produces 5 of ~50K composition rows; the
// downstream per-composition widget CRs (panels, buttons, markdowns) are
// never reached, never resolved, never populated in `widgetContent` L1.
// Customer hits manifest as `widget-content-miss-per-user-fallback`
// fallthrough cells (3,437 buttons + 3,401 markdowns observed on 0.30.219).
//
// THE FIX — page iteration over apiRef-template-driven widgets
//
// After the existing page-1 resolve+populate, IF the widget is
// apiRef+resourcesRefsTemplate-driven AND the resolver's own
// `status.resourcesRefs.slice.continue == true` signal fires, the walker
// continues with `page = 2, 3, …` until ANY of:
//   - the resolver returns `.slice.continue == false`,
//   - the resolved items list is empty,
//   - `page > phase1MaxApiRefPages` (the safety cap),
//   - the parent context is cancelled / timed out,
//   - `objects.Get` fails to re-fetch a fresh CR copy for the next page.
//
// For each additional page, the walker:
//   1. Calls `widgets.Resolve` on a FRESH CR copy (the resolver mutates
//      its input — we must NOT reuse the page-1 copy).
//   2. Calls `populateWidgetContentL1` to Put the page's envelope under
//      the matching identity-free key (KEY tuple decoupled from the
//      RESOLUTION tuple per Ship 0.30.187 D2 — the seed must match the
//      dispatcher's serve-time lookup).
//   3. Walks each new child ref (`status.resourcesRefs.items[]`)
//      discovered on this page, honouring the SAME `visited` set the
//      page-1 walk uses so a shared subtree is resolved once.
//
// DATA-DRIVEN — NO HARDCODED GVRs
//
// The pagination fires on the SHAPE predicate `isApiRefTemplateDriven`:
// both `spec.apiRef.name != ""` AND `spec.resourcesRefsTemplate` non-
// empty. This is the SAME shape `shouldSkipEmptyWidgetShell` and
// `isRBACSensitiveApiRefWidget` already key on (`widget_content.go`).
// No widget-name, no GVR literal. New per-composition widget GVRs the
// page reveals are discovered by parsing each child ref's `/call?` Path
// via the existing `util.ParseCallPathToObjectRef` — the same machinery
// the page-1 recursion already uses.
//
// COST BOUNDS
//
//   - Number of widgets paginated: only the apiRef+resourcesRefsTemplate
//     widgets the walker reaches from the INIT roots. At today's
//     cluster: compositions-page-datagrid, blueprints-page-datagrid,
//     plus any nested template-driven widgets that themselves drive a
//     list. Counted at runtime via `phase1.walk.apiref_pagination`.
//   - Per-widget pages: bounded by `phase1MaxApiRefPages` (constant in
//     this file). Today's default = 500 → at perPage=5, covers 2,500
//     items per apiRef widget. Raising the cap is a code edit + ship,
//     consistent with Diego's "no new env vars / flags" mandate
//     (project_single_cache_flag_direction).
//   - Per-page recursion: unchanged from today — `walkShouldRecurse`
//     gate (verb=="GET"), `w.visited` cycle-set, `phase1MaxWalkDepth`
//     cap. Each new composition reached spawns the same depth-bounded
//     subtree the page-1 walk would have produced for it.
//   - REFRESHER AMPLIFICATION (the 0.30.185 HARD REVERT lesson): the
//     resulting L1 entries are visible to the refresher. We instrument
//     refresher load deltas in Phase 3 bench probe BEFORE raising the
//     cap.
//
// CONCURRENCY
//
// The walker is single-threaded per root (phase1_walk.go's
// resolveNavigationRoot does not goroutine-fan). `iterateApiRefPages`
// runs strictly in-line on the same goroutine, so `w.visited` access
// is safe without a mutex — same property the page-1 recursion relies
// on. Returns nothing: a pagination error is logged + the loop exits;
// the page-1 envelope already populated remains correct.
//
// PREWARM GATE
//
// Wired through `cache.PrewarmEnabled()` — the same env gate that
// controls whether Phase1Warmup runs at all (main.go won't even
// schedule the walker otherwise). No new env var.

package dispatchers

import (
	"context"
	"log/slog"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/maps"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/handlers/util"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// phase1MaxApiRefPages caps how many pages the walker will iterate for a
// single apiRef+resourcesRefsTemplate widget. Bounds the worst-case fan-out
// AND prevents an unbounded apiRef RA (one that always reports
// .slice.continue=true) from looping forever. A code-time constant per
// Diego's "no new env vars / flags" mandate.
//
// At perPage=5 (PREWARM_PAGE_LIMIT default), 500 pages = up to 2,500 rows
// materialised per apiRef widget. For the compositions-page-datagrid at
// ~50K compositions this covers ~5% of the population on first ship; the
// rest stays on the existing per-user fallback path until the cap is
// raised in a follow-up. The constant is exposed via a test-only
// override (phase1MaxApiRefPagesForTest) so unit tests don't have to
// materialise 500-page mock paginations.
const phase1MaxApiRefPages = 500

// phase1MaxApiRefPagesForTest, when non-zero, overrides phase1MaxApiRefPages.
// Production code MUST NOT set this; the only caller is the test in
// phase1_walk_pagination_test.go. Kept as a file-level var (not a build
// tag) so the test does not have to vendor a copy of iterateApiRefPages.
var phase1MaxApiRefPagesForTest int

// maxApiRefPages returns the per-widget page cap honouring the test
// override. Production callers always observe phase1MaxApiRefPages.
func maxApiRefPages() int {
	if phase1MaxApiRefPagesForTest > 0 {
		return phase1MaxApiRefPagesForTest
	}
	return phase1MaxApiRefPages
}

// isApiRefTemplateDriven reports whether a widget CR (the resolver's
// input shape — its `.Object` map at the start of widgets.Resolve, OR
// the resolved-envelope shape which still carries the original spec)
// is an apiRef-driven widget whose resourcesRefs list is FANNED OVER a
// template (so paginating apiRef pages produces new resourcesRefs items).
//
// SHAPE-BASED (feedback_no_special_cases): true IFF
//   - spec.apiRef.name != "" (a non-empty external RA data source), AND
//   - spec.resourcesRefsTemplate non-empty (the items list is built by
//     fanning a template over the apiRef RA's data — so each apiRef page
//     produces a new chunk of items).
//
// A widget with apiRef but no resourcesRefsTemplate renders entirely
// from status.widgetData (e.g. a piechart/table over an aggregating RA)
// — paginating apiRef pages would re-populate the same identity-free
// cell with cumulative widgetData rather than new children to recurse
// into; that shape is correctly handled by the existing
// `isRBACSensitiveApiRefWidget` per-cohort routing.
//
// Accessor errors are treated as "absent" (the conservative direction:
// no pagination, fall back to the current page-1-only behaviour).
func isApiRefTemplateDriven(obj map[string]any) bool {
	if obj == nil {
		return false
	}
	apiRef, err := widgets.GetApiRef(obj)
	if err != nil || apiRef.Name == "" {
		return false
	}
	rrt, _ := widgets.GetResourcesRefsTemplate(obj)
	return len(rrt) > 0
}

// resolverWantsContinue reads `status.resourcesRefs.slice.continue` from
// a resolved widget envelope. The resolver sets this to true when the
// resolved items count equals or exceeds the requested perPage —
// internal/resolvers/widgets/resolve.go:124-134. Returns false on any
// shape mismatch (absent map, wrong type) — the conservative direction
// stops pagination.
func resolverWantsContinue(res *unstructured.Unstructured) bool {
	if res == nil {
		return false
	}
	slice, ok, err := maps.NestedMap(res.Object, "status", "resourcesRefs", "slice")
	if !ok || err != nil {
		return false
	}
	cont, ok := slice["continue"].(bool)
	if !ok {
		return false
	}
	return cont
}

// iterateApiRefPages — Path 3.2.2 (0.30.220) — drives apiRef pagination
// AFTER the existing page-1 walk has resolved + populated. Inputs:
//
//   - ctx           — the SA-credentialed walker context (the same one
//                     the caller's page-1 walk runs under). Pagination
//                     stops on ctx cancel / deadline.
//   - w             — the per-root walker, providing `visited` cycle
//                     state and the harvesters (kept consistent with
//                     the page-1 path).
//   - in            — the page-1 widget CR. Used ONLY to read its
//                     metadata (namespace, name) — we re-fetch a fresh
//                     copy for each subsequent page via objects.Get
//                     because the resolver mutates its input.
//   - gvr           — the widget's GVR; threaded to populateWidgetContentL1
//                     so the seed cell matches the serve-time dispatcher.
//   - page1Res      — the page-1 resolved envelope. Used to read the
//                     resolver's `.slice.continue` signal; we do NOT
//                     re-Put it (the caller already did).
//   - depth         — the walker depth; subsequent-page recursion
//                     continues from depth+1, the same as the page-1
//                     children loop.
//   - perPage       — the RESOLUTION perPage (passed unchanged for
//                     pages 2..N — the apiRef RA is paginated by adv-
//                     ancing PAGE, not by changing perPage).
//   - keyPerPage    — the KEY perPage tuple. Per Ship 0.30.187 D2,
//                     decoupled from `perPage`; passed straight through
//                     to `populateWidgetContentL1` so each page's seed
//                     cell matches the dispatcher's serve-time lookup
//                     for the SAME page.
//   - authnNS       — the authn namespace; threaded to widgets.Resolve.
//
// Returns no error: any pagination failure is logged and stops the
// loop; the page-1 envelope (already Put) remains correct. The walker's
// primary purpose (informer discovery) is unaffected.
//
// SAFETY: gated on isApiRefTemplateDriven(page1Res) AND
// resolverWantsContinue(page1Res). If either is false, the function is
// a no-op — page-1-only behaviour, byte-identical to the pre-3.2.2
// posture.
func iterateApiRefPages(
	ctx context.Context,
	w *phase1Walker,
	in *unstructured.Unstructured,
	gvr schema.GroupVersionResource,
	page1Res *unstructured.Unstructured,
	depth int,
	perPage int,
	keyPerPage int,
	authnNS string,
) {
	if in == nil || page1Res == nil || w == nil {
		return
	}
	// Predicate: only widgets whose items list grows with apiRef pages.
	// Use the page-1 RESOLVED envelope — it still carries the original
	// spec.apiRef + spec.resourcesRefsTemplate (the resolver augments
	// status without stripping spec).
	if !isApiRefTemplateDriven(page1Res.Object) {
		return
	}
	if !resolverWantsContinue(page1Res) {
		return
	}

	log := xcontext.Logger(ctx)
	if log == nil {
		log = slog.Default()
	}

	ns := in.GetNamespace()
	name := in.GetName()
	maxPages := maxApiRefPages()

	// pagesWalked is the count of EXTRA pages beyond page 1 the walker
	// resolves for this widget — emitted at completion for observability.
	pagesWalked := 0

	// The page-1 walk ran at PAGE=1. We start at PAGE=2 and increment.
	// Each iteration: re-fetch a fresh CR copy (resolver mutates input),
	// resolve at this page, populate, recurse children, decide whether
	// to continue. The loop bound `maxPages` is the per-widget cap.
	for page := 2; page <= maxPages; page++ {
		if err := ctx.Err(); err != nil {
			log.Info("phase1.walk.apiref_pagination.ctx_cancel",
				slog.String("subsystem", "cache"),
				slog.String("gvr", gvr.String()),
				slog.String("ns", ns),
				slog.String("name", name),
				slog.Int("page", page),
				slog.Int("pages_walked", pagesWalked),
				slog.Any("err", err),
			)
			return
		}

		// Re-fetch a FRESH CR copy. The resolver mutates opts.In in
		// place (status.* nested fields), so the previous page's
		// envelope cannot be reused. We fetch by (ns, name) via the
		// SAME ObjectReference shape the page-1 path used; objects.Get
		// rides the SA-credentialed transport on ctx.
		ref := templatesv1.ObjectReference{
			Reference:  templatesv1.Reference{Name: name, Namespace: ns},
			Resource:   gvr.Resource,
			APIVersion: gvr.GroupVersion().String(),
		}
		got := objects.Get(ctx, ref)
		if got.Err != nil {
			log.Warn("phase1.walk.apiref_pagination.fetch_failed",
				slog.String("subsystem", "cache"),
				slog.String("gvr", gvr.String()),
				slog.String("ns", ns),
				slog.String("name", name),
				slog.Int("page", page),
				slog.Any("err", got.Err),
			)
			return
		}
		if got.Unstructured == nil {
			log.Warn("phase1.walk.apiref_pagination.fetch_nil",
				slog.String("subsystem", "cache"),
				slog.String("gvr", gvr.String()),
				slog.String("ns", ns),
				slog.String("name", name),
				slog.Int("page", page),
			)
			return
		}

		// Install the widgetContent L1 key on the resolveCtx (Ship G
		// AC-G.5 — see phase1_walk.go:910-931 for the rationale). The
		// key tuple uses (keyPerPage, page) — symmetric with the
		// page-1 site, decoupled from the resolution tuple per Ship
		// 0.30.187 D2.
		wcKey, _ := widgetContentL1Key(gvr, ns, name, keyPerPage, page)
		resolveCtx := ctx
		if wcKey != "" {
			resolveCtx = cache.WithL1KeyContext(resolveCtx, wcKey)
		}

		res, err := widgets.Resolve(resolveCtx, widgets.ResolveOptions{
			In:      got.Unstructured,
			RC:      w.rc,
			AuthnNS: authnNS,
			PerPage: perPage,
			Page:    page,
		})
		if err != nil {
			log.Warn("phase1.walk.apiref_pagination.resolve_failed",
				slog.String("subsystem", "cache"),
				slog.String("gvr", gvr.String()),
				slog.String("ns", ns),
				slog.String("name", name),
				slog.Int("page", page),
				slog.Any("err", err),
			)
			return
		}
		if res == nil {
			return
		}

		// Put this page's envelope under the matching content L1 key.
		// populateWidgetContentL1 idempotently handles the RBAC-
		// sensitive + empty-shell guards (already enforced for page 1)
		// — for page N these are the same conditions, so behaviour is
		// uniform.
		populateWidgetContentL1(ctx, gvr, got.Unstructured, perPage, page, res)
		pagesWalked++

		// Recurse into the children this page produced. Honours the
		// SAME w.visited set as the page-1 children loop, so a child
		// that already appeared (e.g. a widget shared across pages of
		// a paginated apiRef RA — rare but possible) is not re-walked.
		children := extractResourcesRefsItems(res.Object)
		for _, child := range children {
			if err := ctx.Err(); err != nil {
				return
			}
			if !walkShouldRecurse(child) {
				continue
			}
			childRef, ok := objects.ParseCallPathToObjectRef(child.Path)
			if !ok {
				continue
			}
			key := navWidgetEndpointKey(childRef)
			if _, seen := w.visited[key]; seen {
				continue
			}
			w.visited[key] = struct{}{}

			// Child pagination defaults — same posture as the page-1
			// children loop in phase1_walk.go:1057-1083: honour the
			// child's declared slice, else use page=1 + bounded
			// perPage. Key tuple via the same deriveSeedKeyTuple
			// helper the page-1 path uses (paginationInfo-symmetric).
			childPage, childPerPage := 1, prewarmPageLimit()
			if p, pp, hasPg := util.ParseCallPathPagination(child.Path); hasPg {
				childPage, childPerPage = p, pp
			}
			childKeyPerPage, childKeyPage := deriveSeedKeyTuple(child.Path)

			childGot := objects.Get(ctx, childRef)
			if childGot.Err != nil || childGot.Unstructured == nil {
				continue
			}
			_ = w.walk(ctx, childGot.Unstructured, childGot.GVR,
				depth+1, childPage, childPerPage,
				childKeyPerPage, childKeyPage)
		}

		// Decide whether to continue. The resolver's
		// `.slice.continue == true` is the canonical signal — when it
		// flips to false (or the items list comes back empty) we have
		// reached the end of the apiRef pagination.
		if !resolverWantsContinue(res) {
			break
		}
	}

	log.Info("phase1.walk.apiref_pagination.completed",
		slog.String("subsystem", "cache"),
		slog.String("gvr", gvr.String()),
		slog.String("ns", ns),
		slog.String("name", name),
		slog.Int("pages_walked", pagesWalked),
		slog.Int("max_pages", maxPages),
	)
}
