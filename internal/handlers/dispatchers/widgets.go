package dispatchers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/plumbing/http/response"
	"github.com/krateoplatformops/plumbing/maps"
	"github.com/krateoplatformops/snowplow/internal/cache"
	hpkg "github.com/krateoplatformops/snowplow/internal/handlers"
	"github.com/krateoplatformops/snowplow/internal/handlers/util"
	"github.com/krateoplatformops/snowplow/internal/profile"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
)

var widgetTracer = otel.Tracer("snowplow/dispatchers")

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
	log := xcontext.Logger(req.Context())

	// Q-5XX-DIAG (0.25.324) — wrap wri above gzip so the deferred exit
	// hook captures the LOGICAL (uncompressed) bytes the handler emitted
	// plus the first WriteHeader status. The wrapper sits inside this
	// ServeHTTP — gzip middleware wraps the mux ABOVE us — so wri here is
	// already the gzip wrapper. Wrapping again is safe (pass-through) and
	// gives counters the unzipped view that maps to JSON-payload size.
	rec := hpkg.NewStatusRecorder(wri)
	wri = rec
	reloadIdx := readReloadIdx(req)
	defer logWidgetDone(req, rec, start, reloadIdx)

	extras, err := util.ParseExtras(req)
	if err != nil {
		bumpWidgetErrorClass(req, "bad_request", reloadIdx, err)
		response.BadRequest(wri, err)
		return
	}
	profile.Mark(req.Context(), "parse_extras")

	// ── Resolved-output cache ─────────────────────────────────────────────────
	// Cache the fully-resolved widget JSON keyed per user + resource.
	// This eliminates both the HTTP fan-out AND all JQ evaluations on repeated
	// requests. Only unpaginated requests without extras are cached.
	perPage, page := paginationInfo(log, req)
	c := cache.FromContext(req.Context())

	var resolvedKey string
	if c != nil && len(extras) == 0 {
		gvr, gerr := util.ParseGVR(req)
		nsn, nerr := util.ParseNamespacedName(req)
		if gerr == nil && nerr == nil {
			user, uerr := xcontext.UserInfo(req.Context())
			if uerr == nil {
				identity := cache.CacheIdentity(req.Context(), user.Username)
				resolvedKey = cache.ResolvedKey(identity, gvr, nsn.Namespace, nsn.Name, page, perPage)
				profile.Mark(req.Context(), "build_key")
				lookupCtx, lookupSpan := widgetTracer.Start(req.Context(), "cache.lookup",
					trace.WithAttributes(
						attribute.String("cache.layer", "l1"),
						attribute.String("cache.key", resolvedKey),
					))
				raw, hit, _ := c.GetRaw(lookupCtx, resolvedKey)
				if lookupSpan.IsRecording() {
					lookupSpan.SetAttributes(attribute.Bool("cache.hit", hit))
				}
				lookupSpan.End()
				if hit {
				if httpSpan := trace.SpanFromContext(req.Context()); httpSpan.IsRecording() {
					httpSpan.AddEvent("cache.hit", trace.WithAttributes(
						attribute.String("cache.key", resolvedKey),
						attribute.String("cache.layer", "l1"),
					))
				}
				profile.Mark(req.Context(), "redis_get")
				cache.GlobalMetrics.Inc(&cache.GlobalMetrics.RawHits, "raw_hits")
				cache.GlobalMetrics.Inc(&cache.GlobalMetrics.L1Hits, "l1_hits")
				profile.Mark(req.Context(), "metrics")
					cache.TouchKey(cache.ResolvedKeyBase(identity, gvr, nsn.Namespace, nsn.Name))
					log.Info("Widget resolved from cache",
						slog.String("key", resolvedKey),
						slog.String("user", user.Username),
						slog.String("resource", gvr.Resource),
						slog.String("name", nsn.Name),
						slog.String("namespace", nsn.Namespace),
						slog.String("source", "L1-cache"),
						slog.String("duration", util.ETA(start)))
					profile.Mark(req.Context(), "log_info")
					wri.Header().Set("Content-Type", "application/json")
					wri.WriteHeader(http.StatusOK)
					profile.Mark(req.Context(), "headers")
					_, writeSpan := widgetTracer.Start(req.Context(), "http.write",
						trace.WithAttributes(
							attribute.Bool("cache.hit", true),
							attribute.Int("http.response.body.size", len(raw)),
						))
					_, _ = wri.Write(raw)
					writeSpan.End()
					profile.Mark(req.Context(), "body_write")
					profile.End(req.Context(), "l1_hit")
					return
				}
		if httpSpan := trace.SpanFromContext(req.Context()); httpSpan.IsRecording() {
			httpSpan.AddEvent("cache.miss", trace.WithAttributes(
				attribute.String("cache.key", resolvedKey),
				attribute.String("cache.layer", "l1"),
			))
		}
		cache.GlobalMetrics.Inc(&cache.GlobalMetrics.RawMisses, "raw_misses")
		cache.GlobalMetrics.Inc(&cache.GlobalMetrics.L1Misses, "l1_misses")
				log.Info("widget: L1 miss", slog.String("key", resolvedKey))
			}
		}
	}
	// ── End resolved-output cache ─────────────────────────────────────────────

	// Fetch the K8s object (needed for resolution). Done outside singleflight
	// because it requires the HTTP request to parse query parameters.
	_, fetchSpan := widgetTracer.Start(req.Context(), "widget.fetch_object")
	got := fetchObject(req)
	fetchSpan.End()
	if got.Err != nil {
		bumpWidgetErrorClass(req, "object_get_failed", reloadIdx, fmt.Errorf("%s", got.Err.Message))
		response.Encode(wri, got.Err)
		return
	}

	// Resolve the widget. L1 keys are per-user so there is no
	// thundering herd — each user resolves their own key.
	res, resolveErr := resolveWidgetFromObjectInstrumented(req.Context(), c, got, resolvedKey, r.authnNS, perPage, page, extras, reloadIdx)
	if resolveErr != nil {
		writeWidgetError(req, wri, resolveErr, classifyResolveError(resolveErr), reloadIdx)
		return
	}
	log.Info("Widget successfully resolved",
		slog.String("duration", util.ETA(start)))
	wri.Header().Set("Content-Type", "application/json")
	wri.WriteHeader(http.StatusOK)
	_, writeSpan := widgetTracer.Start(req.Context(), "http.write",
		trace.WithAttributes(
			attribute.Bool("cache.hit", false),
			attribute.String("path", "inline"),
			attribute.Int("http.response.body.size", len(res.Raw)),
		))
	_, _ = wri.Write(res.Raw)
	writeSpan.End()
}

// resolveWidgetFromObject is the legacy unlabelled entry point for L1
// refresh paths that do not flow through the HTTP handler (no
// X-Reload-Idx header). It forwards to resolveWidgetFromObjectInstrumented
// with reloadIdx=-1 so the UAF-tracker counter labels record the same
// "production traffic" bucket the architect's spec calls out for absent
// header.
func resolveWidgetFromObject(ctx context.Context, c cache.Cache, got objects.Result, resolvedKey, authnNS string, perPage, page int, extras map[string]any) (*ResolveWidgetResult, error) {
	return resolveWidgetFromObjectInstrumented(ctx, c, got, resolvedKey, authnNS, perPage, page, extras, -1)
}

// resolveWidgetFromObjectInstrumented performs the full widget resolution: resolve → marshal
// → cache-set → pre-warm children. It is called both from the HTTP handler
// and from L1 refresh (via ResolveWidget).
//
// Returns a *ResolveWidgetResult so singleflight callers can access both the
// raw JSON (for HTTP response) and the resolved unstructured (for child pre-warming).
//
// reloadIdx is the per-request X-Reload-Idx label used to bucket the
// UAFTouching/Non-touching counters at the L1-write gate; -1 marks
// production traffic where the harness header is absent.
func resolveWidgetFromObjectInstrumented(ctx context.Context, c cache.Cache, got objects.Result, resolvedKey, authnNS string, perPage, page int, extras map[string]any, reloadIdx int) (*ResolveWidgetResult, error) {
	ctx, span := widgetTracer.Start(ctx, "widget.resolve",
		trace.WithAttributes(
			attribute.String("widget.kind", widgets.GetKind(got.Unstructured.Object)),
			attribute.String("widget.name", widgets.GetName(got.Unstructured.Object)),
			attribute.String("widget.namespace", widgets.GetNamespace(got.Unstructured.Object)),
		))
	defer span.End()

	if span.IsRecording() {
		span.AddEvent("widget.resolution.started", trace.WithAttributes(
			attribute.String("widget.kind", widgets.GetKind(got.Unstructured.Object)),
			attribute.String("widget.name", widgets.GetName(got.Unstructured.Object)),
		))
	}

	log := xcontext.Logger(ctx)

	log = log.With(
		slog.Group("widget",
			slog.String("name", widgets.GetName(got.Unstructured.Object)),
			slog.String("namespace", widgets.GetNamespace(got.Unstructured.Object)),
			slog.String("apiVersion", widgets.GetAPIVersion(got.Unstructured.Object)),
			slog.String("kind", widgets.GetKind(got.Unstructured.Object)),
		),
	)

	tracker := cache.NewDependencyTracker()
	tctx := cache.WithDependencyTracker(xcontext.BuildContext(ctx), tracker)

	// Q-RBAC-DECOUPLE C(d) v4 — strictly-additive test-only RC
	// fallback. Production NEVER sets cache.WithTestRestConfig; the
	// widget resolver continues to work with opts.RC=nil exactly as
	// before (ValidateObjectStatus's nil-rc + non-TestMode path falls
	// back to InClusterConfig). The fallback is reserved for the
	// §6.5 envtest fixture so it can drive the real widgetsHandler
	// without an in-cluster SA mount.
	var resolveRC *rest.Config
	if rc, ok := cache.TestRestConfigFromContext(ctx).(*rest.Config); ok {
		resolveRC = rc
	}

	res, err := widgets.Resolve(tctx, widgets.ResolveOptions{
		In:      got.Unstructured,
		RC:      resolveRC,
		AuthnNS: authnNS,
		PerPage: perPage,
		Page:    page,
		Extras:  extras,
	})
	if err != nil {
		log.Error("unable to resolve widget", slog.Any("err", err))
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	traceId := xcontext.TraceId(tctx, false)
	if traceId != "" {
		if terr := maps.SetNestedField(res.Object, traceId, "status", "traceId"); terr != nil {
			log.Warn("unable to set traceId in status", slog.Any("err", terr))
		}
	}

	_, marshalSpan := widgetTracer.Start(ctx, "http.marshal")
	raw, merr := json.Marshal(res)
	if merr == nil {
		marshalSpan.SetAttributes(attribute.Int("http.response.body.size", len(raw)))
	}
	marshalSpan.End()
	if merr != nil {
		return nil, fmt.Errorf("marshal_failed: %w", merr)
	}

	// Write resolved output to L1. Register cascade deps so that:
	// 1. The widget appears in the RESTAction's per-resource dep index
	//    (for cascade: compositions-list → piechart)
	// 2. The widget appears in the composition GVR dep index
	//    (for triggerL1RefreshBatch to find compositions-list)
	//
	// Only write deps from the tracker's ResourceRefs that are RESTAction
	// refs (from apiRef resolution). Container widgets without apiRef
	// have no RESTAction refs in the tracker, so nothing is registered.
	//
	// Q-RBAC-DECOUPLE C(d) v4 §2.3 (Fix-W) — SKIP the L1 write when the
	// widget transitively depends on a UAF-protected RESTAction. The
	// widget body inlines the FIRST resolving user's apiref view; widget
	// L1 keys are per binding-identity (NOT per-user), so caching the
	// body would silently leak the first user's RBAC-filtered data to
	// every other user in the same binding-identity group. Affected
	// widgets fall through to Path D on every request (per-user
	// correct). Cascade dep registration + child prewarm are also
	// skipped because they depend on the L1 key that we did not write.
	uafSkip := tracker.UAFTouching()
	bumpUAFGateCounter(got.Unstructured, uafSkip, reloadIdx)
	if c != nil && resolvedKey != "" && !uafSkip {
		_ = c.SetResolvedRaw(ctx, resolvedKey, raw)
		// Touch the key so it starts HOT for refresh priority.
		if rki, ok := cache.ParseResolvedKey(resolvedKey); ok {
			cache.TouchKey(cache.ResolvedKeyBase(rki.Username, rki.GVR, rki.NS, rki.Name))
		}
		// Register per-resource cascade dep (widget → specific RESTAction).
		// The tracker records AddResource(restactionGVR, ns, name) during
		// apiref.Resolve. This puts the widget L1 key in the RESTAction's
		// dep index so refreshSingleL1's cascade finds it.
		refs := tracker.ResourceRefs()
		if len(refs) > 0 {
			// Also register the unpaginated base key so cascade
			// refreshes both paginated and unpaginated variants.
			var baseKey string
			if rki, ok := cache.ParseResolvedKey(resolvedKey); ok && (rki.Page > 0 || rki.PerPage > 0) {
				baseKey = cache.ResolvedKeyBase(rki.Username, rki.GVR, rki.NS, rki.Name)
			}
			for _, ref := range refs {
				key := cache.L1ResourceDepKey(ref.GVRKey, ref.NS, ref.Name)
				_ = c.SAddWithTTL(ctx, key, resolvedKey, cache.ReverseIndexTTL)
				if baseKey != "" {
					_ = c.SAddWithTTL(ctx, key, baseKey, cache.ReverseIndexTTL)
				}
			}
		}
		_, preWarmSpan := widgetTracer.Start(ctx, "widget.prewarm_children")
		preWarmChildWidgets(ctx, c, res, authnNS)
		preWarmSpan.End()
	} else if c != nil && resolvedKey != "" && uafSkip {
		log.Debug("widget L1 write skipped: UAF-touching",
			slog.String("key", resolvedKey))
	}

	return &ResolveWidgetResult{Raw: raw, Resolved: res}, nil
}

// ResolveWidget resolves a widget and writes L1. Used by prewarm and
// background L1 refresh.
func ResolveWidget(ctx context.Context, c cache.Cache, got objects.Result, resolvedKey, authnNS string, perPage, page int) (*ResolveWidgetResult, error) {
	return resolveWidgetFromObject(ctx, c, got, resolvedKey, authnNS, perPage, page, nil)
}

// writeWidgetError encodes err to wri preserving any apierrors.StatusError
// HTTP code (apiserver-conformant) and otherwise downgrading to a 500.
//
// Q-5XX-DIAG (0.25.324) — class is the architect-defined error-class label
// (rbac_forbidden / object_get_failed / apiref_resolve_failed /
// marshal_failed / restaction_dispatch_failed) and is also bumped on the
// WidgetErrorByClass counter via bumpWidgetErrorClass. The companion
// log line carries reload_idx and ctx_err so observers can correlate
// across the deferred logWidgetDone audit emission.
func writeWidgetError(req *http.Request, wri http.ResponseWriter, err error, class string, reloadIdx int) {
	bumpWidgetErrorClass(req, class, reloadIdx, err)
	var statusErr *apierrors.StatusError
	if errors.As(err, &statusErr) {
		code := int(statusErr.Status().Code)
		msg := fmt.Errorf("%s", statusErr.Status().Message)
		response.Encode(wri, response.New(code, msg))
		return
	}
	response.InternalError(wri, err)
}

// classifyResolveError maps a widgets.Resolve return error onto one of
// the architect-defined error-class labels. rbac_forbidden takes
// precedence; marshal_failed is detected via the wrapper sentinel
// added in resolveWidgetFromObjectInstrumented; apiref_resolve_failed
// catches inner-call errors carrying "apiref" in the message; remainder
// falls through to restaction_dispatch_failed.
func classifyResolveError(err error) string {
	if err == nil {
		return ""
	}
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
		return "rbac_forbidden"
	}
	msg := err.Error()
	if strings.HasPrefix(msg, "marshal_failed:") {
		return "marshal_failed"
	}
	if strings.Contains(msg, "apiref") {
		return "apiref_resolve_failed"
	}
	return "restaction_dispatch_failed"
}

// readReloadIdx parses the harness-emitted X-Reload-Idx header into an int.
// Returns -1 (production-traffic sentinel per architect) when the header
// is absent or unparseable.
func readReloadIdx(req *http.Request) int {
	v := req.Header.Get("X-Reload-Idx")
	if v == "" {
		return -1
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return -1
	}
	return n
}

// widgetGVRLabel returns the "{group}/{resource}" portion of the per-resource
// counter key for a widget request. Falls back to "unknown/unknown" when
// query parameters are absent (bench traffic always supplies them; only
// degenerate paths reach this branch).
func widgetGVRLabel(req *http.Request) string {
	gvr, err := util.ParseGVR(req)
	if err != nil {
		return "unknown/unknown"
	}
	return fmt.Sprintf("%s/%s", gvr.Group, gvr.Resource)
}

// widgetGVRLabelFromObject returns the same per-resource label as
// widgetGVRLabel but reads it from the unstructured.Unstructured returned
// by fetchObject. Used at the UAF-tracker gate inside
// resolveWidgetFromObjectInstrumented where no *http.Request is in scope
// (L1-refresh paths invoke the resolver without an HTTP request).
func widgetGVRLabelFromObject(uns *unstructured.Unstructured) string {
	if uns == nil {
		return "unknown/unknown"
	}
	apiVersion := widgets.GetAPIVersion(uns.Object)
	kind := widgets.GetKind(uns.Object)
	group := apiVersion
	if i := strings.IndexByte(apiVersion, '/'); i >= 0 {
		group = apiVersion[:i]
	}
	return fmt.Sprintf("%s/%s", group, kind)
}

// bumpWidgetErrorClass increments the WidgetErrorByClass counter and
// emits the architect-required slog.Info("widget.error", ...) line.
// All sites take the same shape so a future log-grep correlation across
// reload_idx is mechanical.
func bumpWidgetErrorClass(req *http.Request, class string, reloadIdx int, err error) {
	if class == "" {
		return
	}
	cache.IncMapKey(&cache.GlobalMetrics.WidgetErrorByClass, class)
	var ctxErr string
	if e := req.Context().Err(); e != nil {
		ctxErr = e.Error()
	}
	var errStr string
	if err != nil {
		errStr = err.Error()
	}
	slog.Info("widget.error",
		slog.String("class", class),
		slog.String("url", req.URL.Path),
		slog.Int("reload_idx", reloadIdx),
		slog.String("ctx_err", ctxErr),
		slog.String("err", errStr),
	)
}

// bumpUAFGateCounter records both the unlabelled UAFTouching/NonTouching
// totals and the per-resource label "{group}/{resource}/{reload_idx}/{true|false}"
// so observers can verify whether the failing CRs (bench-app-05-{906,909,
// 916}-composition-panel) are systematically UAFTouching=false while
// siblings are UAFTouching=true.
func bumpUAFGateCounter(uns *unstructured.Unstructured, uafTouching bool, reloadIdx int) {
	if uafTouching {
		cache.GlobalMetrics.UAFTouchingCount.Add(1)
	} else {
		cache.GlobalMetrics.UAFNonTouchingCount.Add(1)
	}
	key := fmt.Sprintf("%s/%d/%t", widgetGVRLabelFromObject(uns), reloadIdx, uafTouching)
	cache.IncMapKey(&cache.GlobalMetrics.UAFTouchingByResource, key)
}

// statusClass returns the 2xx/4xx/5xx bucket for an HTTP status code.
// Used to key the WidgetResponsesByResource counter so observers can
// see the per-CR shape of healthy vs failing responses without the
// counter cardinality of distinct codes (200, 201, 401, 403, 500, 502).
func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 200 && code < 300:
		return "2xx"
	case code == 0:
		// No header was written — handler returned with body but never
		// called WriteHeader (or panicked before). Mirror the deferred
		// log line's status=0 emission with a distinct bucket so the
		// counter stays consistent with the audit channel.
		return "no_header"
	default:
		return "other"
	}
}

// logWidgetDone is the deferred Q-5XX-DIAG exit hook for widgetsHandler.
// Mirrors call.go's logCallDone but additionally bumps the
// WidgetResponsesByResource counter keyed by "{group}/{resource}/
// {reload_idx}/{class}" so observers can attribute per-CR 5xx rates.
func logWidgetDone(req *http.Request, rec *hpkg.StatusRecorder, start time.Time, reloadIdx int) {
	ctx := req.Context()
	ctxErr := ctx.Err()
	var causeStr, ctxErrStr, writeErrStr string
	if ctxErr != nil {
		ctxErrStr = ctxErr.Error()
		if cause := context.Cause(ctx); cause != nil && cause != ctxErr {
			causeStr = cause.Error()
		}
	}
	if rec.WriteErr != nil {
		writeErrStr = rec.WriteErr.Error()
	}
	class := statusClass(rec.HeaderStatus)
	gvrLabel := widgetGVRLabel(req)
	key := fmt.Sprintf("%s/%d/%s", gvrLabel, reloadIdx, class)
	cache.IncMapKey(&cache.GlobalMetrics.WidgetResponsesByResource, key)
	slog.Info("widget.done",
		slog.String("url", req.URL.Path),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()),
		slog.Int("status", rec.HeaderStatus),
		slog.Int64("bytes", rec.BytesWritten),
		slog.Int("reload_idx", reloadIdx),
		slog.String("class", class),
		slog.String("write_err", writeErrStr),
		slog.String("ctx_err", ctxErrStr),
		slog.String("cause", causeStr),
	)
}

// ResolveWidgetResult holds the resolved unstructured widget and its serialized
// JSON. Used by L1 refresh to pass the result to preWarmChildWidgets.
type ResolveWidgetResult struct {
	Raw      []byte
	Resolved *unstructured.Unstructured
}
