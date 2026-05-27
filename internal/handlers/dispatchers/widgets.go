package dispatchers

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/plumbing/http/response"
	"github.com/krateoplatformops/plumbing/maps"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/handlers/util"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"
)

func Widgets() http.Handler {
	// Ship 0.30.167 — Option 2 parallelism regression fix. Same shape
	// as RESTAction() above and RegisterRefreshHandlers at
	// dispatchers.go:56-66 (the load-bearing prior art): resolve the
	// SA transport pair ONCE at handler construction, capture into
	// struct fields, attach in ServeHTTP via a cheap nil-check + field
	// read. Eliminates the per-request snowplowSACtx() helper call
	// that serialised dispatches through the SA singletons' mutexes.
	saEP, saRC := snowplowSACtx()
	return &widgetsHandler{
		authnNS: env.String("AUTHN_NAMESPACE", ""),
		verbose: env.True("DEBUG"),
		saEP:    saEP,
		saRC:    saRC,
	}
}

type widgetsHandler struct {
	authnNS string
	verbose bool
	// saEP + saRC: see restActionHandler.saEP / saRC. Same shape;
	// captured at handler construction.
	saEP *endpoints.Endpoint
	saRC *rest.Config
}

var _ http.Handler = (*widgetsHandler)(nil)

func (r *widgetsHandler) ServeHTTP(wri http.ResponseWriter, req *http.Request) {
	start := time.Now()

	// Ship 0.30.171-debug — per-/call structured timing log; see
	// restactions.go for the rationale. Emits dispatcher.call.complete
	// into the existing stdout->otel-daemonset->ClickHouse pipeline.
	pcs, pcEmit := beginPerCall(req, "widgets")
	defer pcEmit()

	extras, err := util.ParseExtras(req)
	if err != nil {
		response.BadRequest(wri, err)
		return
	}

	got := fetchObject(req)
	if got.Err != nil {
		response.Encode(wri, got.Err)
		return
	}
	pcs.gvr = got.GVR.String()

	log := xcontext.Logger(req.Context()).
		With(
			slog.Group("widget",
				slog.String("name", widgets.GetName(got.Unstructured.Object)),
				slog.String("namespace", widgets.GetNamespace(got.Unstructured.Object)),
				slog.String("apiVersion", widgets.GetAPIVersion(got.Unstructured.Object)),
				slog.String("kind", widgets.GetKind(got.Unstructured.Object)),
			),
		)

	// Revision 2 binding (0.30.4): cache=on mode gates every widget
	// dispatch by EvaluateRBAC. Cache=off skips the gate — fetchObject
	// already ran per-user against apiserver.
	if !cache.Disabled() {
		if !checkDispatchRBAC(req.Context(), got.GVR, got.Unstructured.GetNamespace()) {
			log.Warn("widget dispatch denied by EvaluateRBAC",
				slog.String("gvr", got.GVR.String()),
			)
			response.Encode(wri, response.New(http.StatusForbidden,
				fmt.Errorf("forbidden: cannot get %s in namespace %q",
					got.GVR.Resource, got.Unstructured.GetNamespace())))
			return
		}
	}

	perPage, page := paginationInfo(log, req)

	// Ship G (0.30.16x) — identity-free widget content L1 lookup runs
	// FIRST. Same gating semantics as the per-user lookup below
	// (strictly after EvaluateRBAC at :62-72). The content key is
	// (gvr, ns, name, perPage, page, extras) — Username/Groups
	// OMITTED — so admin and cyberjoker hit the SAME cell. The
	// cached body carries SA-evaluated `allowed=true` flags
	// (the F2 walker resolved under the snowplow SA); gateWidgetEnvelope
	// OVERWRITES every status.resourcesRefs.items[].allowed per-request
	// via rbac.UserCan under THIS request's identity, so the body that
	// leaves the pod is per-user-narrowed. The body in the cache is
	// the SHELL — never served verbatim.
	//
	// On MISS we fall through to the existing per-user widget L1 lookup
	// below — the expected path when F2 has not warmed this
	// (gvr, ns, name, perPage, page) tuple.
	contentKey, contentHandle, _ := dispatchWidgetContentKey(req.Context(),
		got.GVR.Group, got.GVR.Version, got.GVR.Resource,
		got.Unstructured.GetNamespace(), got.Unstructured.GetName(),
		perPage, page, extras)
	if contentHandle != nil {
		if entry, ok := contentHandle.Get(contentKey); ok {
			if gated, served := gateWidgetEnvelope(req.Context(), entry.RawJSON); served {
				cache.RecordApiserverFallthrough(req.Context(),
					cache.ReasonWidgetContentHit, got.GVR.String())
				emitResolvedCacheLookup(log, "widgetContent", got.GVR.String(),
					contentKey, true, len(gated))
				pcs.l1Hit = "content-hit"
				writeResolvedJSON(wri, gated)
				log.Info("Widget successfully resolved",
					slog.String("duration", util.ETA(start)),
					slog.String("l1", "content-hit"),
				)
				return
			}
			// served==false — fail-closed (no identity / malformed
			// stored envelope). Fall through to the existing per-user
			// L1 lookup, which symmetrically nil-checks UserInfo at
			// dispatchCacheLookupKey.
		}
		// Content-layer MISS — fall through to the per-user L1 lookup
		// below. Record the diagnostic counter.
		cache.RecordApiserverFallthrough(req.Context(),
			cache.ReasonWidgetContentMissPerUserFallback, got.GVR.String())
	}

	// Tag 0.30.7: L1 resolved-output cache lookup. Same gating
	// semantics as restactions.go — strictly after EvaluateRBAC.
	// 0.30.8: cacheInputs is returned so we can stash it on the L1
	// entry for the refresher to drive a re-resolve on UPDATE.
	cacheKey, cacheHandle, cacheInputs := dispatchCacheLookupKey(req.Context(), "widgets",
		got.GVR.Group, got.GVR.Version, got.GVR.Resource,
		got.Unstructured.GetNamespace(), got.Unstructured.GetName(),
		perPage, page, extras)
	if cacheHandle != nil {
		if entry, ok := cacheHandle.Get(cacheKey); ok {
			emitResolvedCacheLookup(log, "widgets", got.GVR.String(), cacheKey, true, len(entry.RawJSON))
			pcs.l1Hit = "hit"
			writeResolvedJSON(wri, entry.RawJSON)
			log.Info("Widget successfully resolved",
				slog.String("duration", util.ETA(start)),
				slog.String("l1", "hit"),
			)
			return
		}
		emitResolvedCacheLookup(log, "widgets", got.GVR.String(), cacheKey, false, 0)
		pcs.l1Hit = "miss"
	}

	ctx := xcontext.BuildContext(req.Context())
	// Ship 0.30.167 — Option 2 parallelism regression fix. Symmetric
	// with restactions.go: read the SA transport pair from struct
	// fields populated once at Widgets() construction. AC-307.7 byte-
	// identical out-of-cluster: snowplowSACtx returned (nil, nil) so
	// both fields are nil and the attach below SKIPS.
	if r.saEP != nil && r.saRC != nil {
		ctx = cache.WithInternalEndpoint(ctx, r.saEP)
		ctx = cache.WithInternalRESTConfig(ctx, r.saRC)
	}
	// 0.30.94 Edge type 3: attach the L1 key being populated so the
	// underlying restactions resolver (called transitively via apiRef)
	// records dep edges against each inner K8s call. Widget L1 key
	// flows through into the inner resolver — Edge type 3 correctly
	// records against the widget L1 key because the widget cache entry
	// depends on every K8s object its underlying RestActions touch.
	if cacheKey != "" {
		ctx = cache.WithL1KeyContext(ctx, cacheKey)
	}

	res, err := widgets.Resolve(ctx, widgets.ResolveOptions{
		In:      got.Unstructured,
		AuthnNS: r.authnNS,
		PerPage: perPage,
		Page:    page,
		Extras:  extras,
	})
	if err != nil {
		log.Error("unable to resolve widget", slog.Any("err", err))
		var statusErr *apierrors.StatusError
		if errors.As(err, &statusErr) {
			code := int(statusErr.Status().Code)
			msg := fmt.Errorf("%s", statusErr.Status().Message)
			response.Encode(wri, response.New(code, msg))
			return
		}
		response.InternalError(wri, err)
		return
	}

	traceId := xcontext.TraceId(ctx, false)
	if traceId != "" {
		err := maps.SetNestedField(res.Object, traceId, "status", "traceId")
		if err != nil {
			log.Warn("unable to set traceId in status", slog.Any("err", err))
		}
	}

	encoded, err := encodeResolvedJSON(res)
	if err != nil {
		log.Error("unable to encode widget response", slog.Any("err", err))
		response.InternalError(wri, err)
		return
	}
	if cacheHandle != nil && cacheKey != "" {
		cacheHandle.Put(cacheKey, &cache.ResolvedEntry{
			RawJSON: encoded,
			Inputs:  cacheInputs,
		})
		// 0.30.8: record dep edges. Widget self-dep, apiRef→RestAction
		// dep, and render-eligible resourcesRefs deps (action-only
		// refs filtered out per Revision 14). Edge type 3 (inner K8s
		// calls inside the RestAction) is OUT OF SCOPE at this tag.
		recordWidgetDeps(log, cacheKey, got.GVR, res)
	}

	log.Info("Widget successfully resolved",
		slog.String("duration", util.ETA(start)),
		slog.String("l1", "miss"),
	)

	writeResolvedJSON(wri, encoded)
}
