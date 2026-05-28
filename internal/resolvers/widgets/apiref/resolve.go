package apiref

import (
	"context"
	"fmt"

	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"github.com/krateoplatformops/snowplow/internal/resolvers/restactions"
	"k8s.io/client-go/rest"
)

type ResolveOptions struct {
	RC      *rest.Config
	ApiRef  templatesv1.ObjectReference
	AuthnNS string
	PerPage int
	Page    int
	Extras  map[string]any
}

func Resolve(ctx context.Context, opts ResolveOptions) (map[string]any, error) {
	if opts.ApiRef.Name == "" || opts.ApiRef.Namespace == "" {
		return map[string]any{}, nil
	}

	res := objects.Get(ctx, opts.ApiRef)
	if res.Err != nil {
		return map[string]any{}, fmt.Errorf("%s", res.Err.Message)
	}

	ra, err := convertToRESTAction(res.Unstructured.Object)
	if res.Err != nil {
		return map[string]any{}, err
	}

	// resolveRA is the page-keyed resolve seam: it runs the SAME
	// restactions.Resolve pipeline at the given pagination and returns the RA
	// Status map. A fresh shallow copy of the RA (Status reset) is resolved
	// each call so the unpaginated + page-keyed resolves of Ship 4a's
	// byte-verify do not clobber each other's Status (restactions.Resolve
	// mutates In.Status in place).
	//
	// The rctx parameter lets Ship 4a swap the L1-key context for the
	// UNPAGINATED resolve so the RA's inner-call dep edges attach to the
	// RAFullList key (the cell the refresher re-resolves + re-pins on a
	// dirty-mark). Dep recording is idempotent (sync.Map LoadOrStore), so a
	// page-keyed resolve under the widget's own L1 key and an unpaginated
	// resolve under the RAFullList key coexist safely.
	resolveRA := func(rctx context.Context, perPage, page int) (map[string]any, error) {
		local := ra
		local.Status = nil
		raopts := restactions.ResolveOptions{
			In:      &local,
			SArc:    opts.RC,
			AuthnNS: opts.AuthnNS,
			PerPage: perPage,
			Page:    page,
			Extras:  opts.Extras,
		}
		if _, rerr := restactions.Resolve(rctx, raopts); rerr != nil {
			return nil, rerr
		}
		return rawExtensionToMap(local.Status)
	}

	// Ship 4a (0.30.198) — page-independent RAFullList serve at the apiRef
	// chokepoint. Engaged ONLY when the cache is on AND the request is
	// paginated (perPage>0 && page>0). On a hit / verified-sliceable shape it
	// serves a cheap Go-slice over the cached full list, shared across pages
	// AND widgets. On a miss / not-cleanly-sliceable shape it transparently
	// falls back to today's page-keyed resolve below — NEVER a wrong result.
	//
	// Flag-off (CACHE_ENABLED=false → ResolvedCacheEnabled()=false) raFullListServe
	// returns served=false on its first nil-cache check and this whole branch
	// is byte-identical to pre-4a.
	if opts.PerPage > 0 && opts.Page > 0 && cache.ResolvedCacheEnabled() {
		if served, ok, serr := raFullListServe(ctx, res.GVR, opts.ApiRef.Namespace,
			opts.ApiRef.Name, &ra, opts.PerPage, opts.Page, opts.Extras, resolveRA); serr != nil {
			return map[string]any{}, serr
		} else if ok {
			return served, nil
		}
		// served=false, no error — fall through to the page-keyed resolve.
	}

	return resolveRA(ctx, opts.PerPage, opts.Page)
}
