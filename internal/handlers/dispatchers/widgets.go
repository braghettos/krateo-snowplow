package dispatchers

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/plumbing/http/response"
	"github.com/krateoplatformops/plumbing/maps"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/handlers/util"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// [panel500-instr] site=10 helper — see writeResolvedJSON call site
// below. Strips the per-request traceId from the response bytes
// before hashing so the hash captures CONTENT EQUIVALENCE
// (decoupled from per-request identity). The regex is conservative
// — matches "traceId":"<anything>" — and replaces with a fixed
// placeholder before sha256.
//
// Architect §6 design rationale: tester's D.4.1 validation showed
// "all 4 named canaries + 6 other corpus items diffed" even though
// resolver-nil-merge counter never fired. Site 10's diagnostic
// question: was the diff content-meaningful, or was it just
// methodology (traceId/timestamps in the body)? A matching
// hash_no_traceid across pre-D.4.1 and D.4.1 captures confirms
// methodology; a mismatching hash confirms content change.
var traceIdRegex = regexp.MustCompile(`"traceId":"[^"]*"`)

func hashWithoutTraceId(b []byte) string {
	s := traceIdRegex.ReplaceAllString(string(b), `"traceId":"<stripped>"`)
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func Widgets() http.Handler {
	return &widgetsHandler{
		authnNS: env.String("AUTHN_NAMESPACE", ""),
		verbose: env.True("DEBUG"),
	}
}

type widgetsHandler struct {
	authnNS string
	verbose bool
}

var _ http.Handler = (*widgetsHandler)(nil)

func (r *widgetsHandler) ServeHTTP(wri http.ResponseWriter, req *http.Request) {
	start := time.Now()

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
			emitResolvedCacheLookup(log, "widgets", cacheKey, true, len(entry.RawJSON))
			writeResolvedJSON(wri, entry.RawJSON)
			log.Info("Widget successfully resolved",
				slog.String("duration", util.ETA(start)),
				slog.String("l1", "hit"),
			)
			return
		}
		emitResolvedCacheLookup(log, "widgets", cacheKey, false, 0)
	}

	ctx := xcontext.BuildContext(req.Context())
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

	// [panel500-instr] site=10 — widget-response-byte capture
	// (D.4.1 universal-corpus-diff discriminator). Architect §6:
	// "during the D.4.1-style universal-corpus-diff run, do
	// encoded_hash_no_traceid values match the same widget's hash
	// on a pre-D.4.1 capture? If YES, (a) confirmed — the diff is
	// methodology (traceId/timestamps); D.4.1's resolver-merge code
	// change was behaviour-equivalent. If NO, (b) or (c) — the
	// change ACTUALLY produced different bytes, and we re-diagnose."
	// Fires per widget /call response, BEFORE writeResolvedJSON so
	// the bytes captured match exactly what the wire sees.
	slog.Info("[panel500-instr] site=10 tag=widget_response_bytes",
		slog.String("widget_name", widgets.GetName(res.Object)),
		slog.String("widget_namespace", widgets.GetNamespace(res.Object)),
		slog.String("traceId", xcontext.TraceId(ctx, false)),
		slog.Int("encoded_len", len(encoded)),
		slog.String("encoded_hash_no_traceid", hashWithoutTraceId(encoded)),
	)

	writeResolvedJSON(wri, encoded)
}
