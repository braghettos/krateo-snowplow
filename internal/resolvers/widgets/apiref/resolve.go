// Package apiref resolves a widget's apiRef: it fetches the referenced
// RESTAction object and resolves it (through the restactions resolver),
// returning the resulting data dictionary for the widget to consume.
package apiref

import (
	"context"
	"fmt"
	"log/slog"

	xcontext "github.com/krateoplatformops/plumbing/context"
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
		// Task #272 / 0.30.251 — error-type preservation. Pre-fix,
		// the apiref boundary stripped the upstream apiserver status
		// code with `fmt.Errorf("%s", res.Err.Message)`. The downstream
		// dispatcher's `errors.As(err, *apierrors.StatusError)` check
		// (widgets.go:228-234) then failed and ALL apiRef-resolve
		// errors landed in `response.InternalError` → HTTP 500,
		// regardless of the apiserver's actual response code.
		//
		// Architect trace task-262-s8-cj-tablist-trace-2026-06-09.md
		// §3.3 documents the symptom: a cj `restactions:get` 403 from
		// the apiserver became an HTTP 500 on the SPA wire, so the
		// frontend could not distinguish "you lack permission" from
		// "snowplow exploded" and rendered .ant-result-error.
		//
		// Fix: reconstruct an `*apierrors.StatusError` from the code
		// already preserved in `res.Err` (objects.Get's apiserver
		// branch faithfully sets res.Err.Code per apierrors.IsForbidden
		// / IsNotFound — see internal/objects/get.go:209-214), then
		// wrap with `%w` so the dispatcher can recover the code via
		// errors.As. The wrapped chain also preserves the upstream
		// message + adds a `apiref resolve <group>/<resource>/<name>`
		// context prefix for log-side observability.
		statusErr := statusErrorFromResponse(res.Err, opts.ApiRef)
		wrapped := fmt.Errorf("apiref resolve %s/%s/%s: %w",
			res.GVR.Group, res.GVR.Resource, opts.ApiRef.Name, statusErr)
		// Falsifier slog WARN: the runtime artifact tester / observer
		// uses to verify the StatusError chain is preserved. Single
		// emission per apiref error — no per-request fan-out.
		if log := xcontext.Logger(ctx); log != nil {
			log.Warn("apiref.resolve.error_preserved",
				slog.Int("upstream_code", res.Err.Code),
				slog.String("upstream_reason", string(res.Err.Reason)),
				slog.String("gvr_group", res.GVR.Group),
				slog.String("gvr_resource", res.GVR.Resource),
				slog.String("name", opts.ApiRef.Name),
				slog.String("namespace", opts.ApiRef.Namespace),
			)
		}
		return map[string]any{}, wrapped
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
