// ra_full_list.go — Ship 4a (0.30.198): the page-independent RAFullList
// serve path at the apiRef chokepoint.
//
// THE CHOKEPOINT. Every widget→RESTAction read funnels through
// apiref.Resolve (resolve.go), which resolves the RA at the request's
// PerPage/Page and returns the RA's Status map. Today each page re-resolves
// the whole RA (62-namespace fan-out + 49K sort + deep-copy + gojq
// recompile) because nothing caches the RA result page-independently. Ship
// 4a caches the RA's FULL result ONCE per (RA × cohort × non-slice extras)
// resolved UNPAGINATED, and serves each page as a cheap Go-slice over that
// one cached array.
//
// PER-/CALL SLICEABILITY VERDICT (design §3). On first sight of a
// (RAFullList-key × sliceShape) we byte-VERIFY: the Go-slice over the
// freshly-resolved full F must byte-match a fresh page-keyed resolve, AND
// the full F itself must byte-match a fresh unpaginated resolve. Match →
// memo sliceable, serve the Go-slice. Differ → memo NOT-sliceable for THIS
// shape → fall back to the page-keyed resolve forever (never a wrong
// result). The verdict is process-local; re-evaluated per new shape + on
// pod restart.
//
// REMOVABILITY (project_caching_is_provisional): this file + the single
// branch in resolve.go + the cache-package resident region are the entire
// 4a surface — wholesale-deletable. Gated under cache.ResolvedCacheEnabled()
// (CACHE_ENABLED); flag-off it is never entered.

package apiref

import (
	"context"
	"encoding/json"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/ptr"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// raFullListCallerClass is the sliceShape caller-class marker for the
// apiRef-chokepoint serve path. It keeps the apiRef path's sliceability
// verdict SEPARATE from the direct-RA dispatcher path (which uses
// "restactions") so a verdict verified on one path never auto-applies to
// the other — the two paths inject the slice at different layers, so their
// byte-verify must be independent (design §3 "different sliceShape").
const raFullListCallerClass = "apiref"

// raFullListServe implements the Ship 4a page-independent serve at the
// apiRef chokepoint. It is called ONLY when cache is on AND the request is
// paginated (perPage>0 && page>0) — the unpaginated path keeps today's
// behaviour byte-identically (a paginated cell is never built for an
// unpaginated /call).
//
// Returns (result, true, nil) when it served a Go-slice over the cached
// full list; (nil, false, nil) when the caller MUST fall back to today's
// page-keyed resolve (no identity, not sliceable, cache miss path declined,
// or a defensive bail); (nil, false, err) on a hard resolve error.
//
// gvr/ns/name identify the RESTAction CR (res.GVR + opts.ApiRef). ra is the
// typed RESTAction (its Spec.Filter is the slice-jq for the sliceShape).
// resolveRA(perPage, page) is the page-keyed resolve seam — a closure over
// restactions.Resolve so this file does not duplicate the apiref Resolve
// plumbing; it returns the RA Status map for the given pagination.
func raFullListServe(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	namespace, name string,
	ra *templatesv1.RESTAction,
	perPage, page int,
	extras map[string]any,
	resolveRA func(ctx context.Context, perPage, page int) (map[string]any, error),
) (map[string]any, bool, error) {
	c := cache.ResolvedCache()
	if c == nil {
		return nil, false, nil // cache off — fall back
	}

	ui, err := xcontext.UserInfo(ctx)
	if err != nil {
		// No identity — cannot key the per-cohort cell safely. Fall back to
		// the page-keyed resolve (which itself runs under the request
		// identity). NEVER serve a cohort cell without an identity.
		return nil, false, nil
	}
	bindingSetHash := cache.BindingSetHash(ui.Username, ui.Groups)

	keyInputs := cache.RAFullListKeyInputs(gvr.Group, gvr.Version, gvr.Resource,
		namespace, name, bindingSetHash, extras)
	raKey := cache.ComputeKey(keyInputs)

	// fullCtx scopes the UNPAGINATED resolves' inner-call dep edges to the
	// RAFullList key, so an informer event on any object the RA reads
	// dirty-marks THIS cell and the refresher re-resolves + re-pins it
	// (stale-while-revalidate). Idempotent dep recording lets this coexist
	// with the widget cell's own deps recorded under the request ctx.
	fullCtx := cache.WithL1KeyContext(ctx, raKey)

	sliceJQ := ptr.Deref(ra.Spec.Filter, "")
	shape := cache.SliceShapeHash(raFullListCallerClass, gvr.Group, gvr.Version,
		gvr.Resource, namespace, name, sliceJQ)

	offset := (page - 1) * perPage

	// --- Fast path: known-sliceable verdict + cell hit -> Go-slice -------
	sliceable, known := cache.SliceabilityLookup(raKey, shape)
	if known && !sliceable {
		// This (RA × shape) was proven NOT cleanly sliceable. Always fall
		// back — never a wrong result.
		return nil, false, nil
	}
	if known && sliceable {
		if entry, ok := c.Get(raKey); ok {
			full, derr := decodeRAFullList(entry.RawJSON)
			if derr == nil {
				if sliced, sok := cache.GoSliceFullList(full, offset, perPage); sok {
					cache.RecordRAFullListServe(cache.RAFullListServeHit)
					return sliced, true, nil
				}
			}
			// Decode / shape mismatch on a cached cell (corrupt / shape
			// drift). Fall back this /call; do NOT poison the verdict.
			return nil, false, nil
		}
		// Cell miss under a known-sliceable verdict: resolve unpaginated,
		// re-Put the cell, then Go-slice. No re-verify needed (the verdict
		// is already established for this shape).
		full, rerr := resolveRA(fullCtx, 0, 0)
		if rerr != nil {
			return nil, false, rerr
		}
		sliced, sok := cache.GoSliceFullList(full, offset, perPage)
		if !sok {
			// The full result no longer slices cleanly (shape drift). Fall
			// back this /call without poisoning the established verdict.
			return nil, false, nil
		}
		c.PutRAFullList(raKey, keyInputs, full)
		cache.Deps().Record(raKey, gvr, namespace, name)
		cache.RecordRAFullListServe(cache.RAFullListServeRepopulateSlice)
		return sliced, true, nil
	}

	// --- First sight of (RA × shape): byte-VERIFY, then memoise ---------
	// 1. Resolve UNPAGINATED -> full F (deps scoped to the RAFullList key).
	full, err := resolveRA(fullCtx, 0, 0)
	if err != nil {
		return nil, false, err
	}
	// 2. Resolve the OLD page-keyed way -> S_ra (the fall-back reference).
	//    Under the ORIGINAL ctx so its deps belong to the widget cell,
	//    unchanged from today.
	sRA, err := resolveRA(ctx, perPage, page)
	if err != nil {
		return nil, false, err
	}

	// EMPTY-FULL GUARD (0.30.208) — self-healing refusal to freeze on empty.
	//
	// When the freshly-resolved full list's single array-valued key has
	// length 0, an empty Go-slice byte-equals an empty page-keyed S_ra, so
	// the byte-verify below would (wrongly) record verdict=sliceable and Put
	// the EMPTY cell — freezing the fast path on an empty result FOREVER (it
	// never re-verifies). But an empty full is INDISTINGUISHABLE from a
	// not-yet-synced / continueOnError-degraded resolve (panels informer not
	// synced at boot → panel LISTs degrade to []), so it is NOT an
	// authoritative sliceable verdict. Refuse to record OR Put: leave the
	// verdict UNKNOWN so the NEXT request re-runs first-sight once the
	// informer is synced (self-healing), and serve the (correct, empty)
	// page-keyed S_ra meanwhile.
	//
	// PERF: this guard adds ZERO resolves — `full` is ALREADY resolved on
	// this first-sight path for the byte-verify, so the emptiness check is
	// free. We never RecordSliceability on empty, so the cheap fast path
	// (known&&sliceable → Get → GoSlice) is never entered for an empty cell;
	// every request stays on first-sight only while the full resolves EMPTY
	// (cheap — a not-synced fan-out yields []), NOT a 48s/163MB full. The
	// one-time expensive full resolve happens exactly once, AFTER sync, when
	// the full is non-empty and the verdict is correctly recorded → cheap
	// hits thereafter. Mechanism-uniform: keyed off "the full is empty"
	// (FullListIsEmpty), NO resource/name/GVR literal.
	if cache.FullListIsEmpty(full) {
		cache.RecordRAFullListServe(cache.RAFullListServeFallback)
		return sRA, true, nil
	}
	// 3. Go-slice the full F -> S_go.
	sGo, sok := cache.GoSliceFullList(full, offset, perPage)

	// 4. Byte-compare: full F must equal a fresh unpaginated resolve (it IS
	//    `full`, so identity holds by construction — re-encode both to
	//    canonical JSON to be safe) AND S_go must byte-equal S_ra.
	verdict := sok && canonicalJSONEqual(sGo, sRA)

	// Ship #91 / 0.30.211 — Class C heuristic. When the byte-verify FAILED
	// AND `full` is shape-identical to S_ra modulo length-vs-perPage, this
	// is an aggregation RA (or otherwise structurally non-sliceable RA)
	// whose verdict will never flip. Mark it permanent so the retry cap
	// drops from 3 to 1. Mechanism-uniform: derived from (full, sRA, perPage),
	// no resource/name/GVR literal (feedback_no_special_cases).
	permanent := false
	if !verdict {
		permanent = cache.IsStructurallyNonSliceable(full, sRA, perPage)
	}

	// Record the verdict + the caller-CR identity labels so the
	// snowplow_ra_full_list_memo expvar can describe the entry by its caller
	// (e.g. compositions-page-datagrid) instead of by the raKey/sliceShape
	// sha256 hashes alone. The labels are READ-SIDE ONLY (they do not change
	// the memo key) — see RecordSliceabilityWithLabels.
	cache.RecordSliceabilityClassified(raKey, shape, verdict, permanent, cache.SliceabilityLabels{
		CallerClass:     raFullListCallerClass,
		CallerGroup:     gvr.Group,
		CallerVersion:   gvr.Version,
		CallerResource:  gvr.Resource,
		CallerNamespace: namespace,
		CallerName:      name,
	})

	// Ship #91 / 0.30.211 — Lever C symmetric dep-record at BOTH verdict
	// branches. Pre-Ship-#91 the RA-CR self-dep was recorded ONLY on the
	// verdict=true branch (line 217), so a verdict=false entry got no dep
	// edge — meaning a panels-informer event on the underlying objects
	// could not invalidate a stuck-false memo entry via the refresher hook.
	// Symmetric record() at this site lets the refresher's RAFullList
	// class-prefix hook (Lever C) call InvalidateSliceabilityForKey(raKey)
	// when the dep-tuple fires, clearing the memo and letting the next
	// /call re-enter first-sight. THIS IS THE WIRING THE ARCHITECT GUARDS.
	cache.Deps().Record(raKey, gvr, namespace, name)

	if !verdict {
		// NOT cleanly sliceable for this shape — serve the page-keyed S_ra
		// (already resolved) and never try the Go-slice for this shape until
		// Lever A's TTL expires + retryCount permits OR Lever C invalidates.
		cache.RecordRAFullListServe(cache.RAFullListServeFallback)
		return sRA, true, nil
	}

	// Sliceable — Put the full cell (possibly pinned by cost predicate),
	// then serve the verified Go-slice. The RA-CR self-dep was already
	// recorded above (symmetric with the false branch — Ship #91).
	c.PutRAFullList(raKey, keyInputs, full)
	cache.RecordRAFullListServe(cache.RAFullListServeVerifiedSlice)
	return sGo, true, nil
}

// decodeRAFullList unmarshals a cached RAFullList envelope back to a map.
func decodeRAFullList(raw []byte) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// canonicalJSONEqual reports whether two maps marshal to byte-identical
// canonical JSON. encoding/json sorts map keys, so this is order-stable.
func canonicalJSONEqual(a, b map[string]any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}
