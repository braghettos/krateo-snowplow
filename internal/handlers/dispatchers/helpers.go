package dispatchers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/plumbing/http/response"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	idynamic "github.com/krateoplatformops/snowplow/internal/dynamic"
	"github.com/krateoplatformops/snowplow/internal/handlers/util"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"github.com/krateoplatformops/snowplow/internal/rbac"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

func fetchObject(req *http.Request) (got objects.Result) {
	log := xcontext.Logger(req.Context())

	gvr, err := util.ParseGVR(req)
	if err != nil {
		got.Err = response.New(http.StatusBadRequest, err)
		return
	}
	log.Debug("GVR from request query parameters", slog.Any("gvr", gvr))

	nsn, err := util.ParseNamespacedName(req)
	if err != nil {
		got.Err = response.New(http.StatusBadRequest, err)
		return
	}
	log.Debug("Name and Namespace from request query parameters", slog.Any("nsn", nsn))

	return objects.Get(req.Context(), templatesv1.ObjectReference{
		Reference: templatesv1.Reference{
			Name: nsn.Name, Namespace: nsn.Namespace,
		},
		APIVersion: gvr.GroupVersion().String(),
		Resource:   gvr.Resource,
	})
}

func paginationInfo(log *slog.Logger, req *http.Request) (perPage, page int) {
	perPage, page = -1, -1

	if val := req.URL.Query().Get("perPage"); val != "" {
		var err error
		perPage, err = strconv.Atoi(val)
		if err != nil {
			log.Error("unable convert perPage parameter to int",
				slog.Any("err", err))
		}
	}

	if val := req.URL.Query().Get("page"); val != "" {
		var err error
		page, err = strconv.Atoi(val)
		if err != nil {
			log.Error("unable convert page parameter to int",
				slog.Any("err", err))
		}
	}

	if perPage > 0 && page <= 0 {
		page = 1
	}

	return
}

// checkDispatchRBAC is the cache=on permission gate (Revision 2
// binding, Tag 0.30.4). Returns true iff the user identified by ctx is
// permitted to GET the dispatched CR in namespace.
//
// The check runs against the *dispatch target* (RestAction or Widget
// CR) — the same object the cache=off fetchObject branch hits the
// apiserver for. In cache=on mode fetchObject does not enforce RBAC
// for that GET, so the gate must run explicitly here.
//
// Callers MUST only invoke this in cache=on mode (`!cache.Disabled()`).
// In cache=off mode the gate is implicit in fetchObject's per-user
// apiserver call.
func checkDispatchRBAC(ctx context.Context, gvr schema.GroupVersionResource, namespace string) bool {
	log := xcontext.Logger(ctx)

	ui, err := xcontext.UserInfo(ctx)
	if err != nil {
		log.Error("checkDispatchRBAC: unable to extract UserInfo",
			slog.Any("err", err),
		)
		return false
	}

	allowed, evalErr := rbac.EvaluateRBAC(ctx, rbac.EvaluateOptions{
		Username:  ui.Username,
		Groups:    ui.Groups,
		Verb:      "get",
		Group:     gvr.Group,
		Resource:  gvr.Resource,
		Namespace: namespace,
	})
	if evalErr != nil {
		log.Error("checkDispatchRBAC: EvaluateRBAC error",
			slog.String("user", ui.Username),
			slog.String("gvr", gvr.String()),
			slog.String("namespace", namespace),
			slog.Any("err", evalErr),
		)
		return false
	}
	return allowed
}

// dispatchWidgetContentKey builds the identity-free widget content L1
// key and returns the live cache handle (Ship G, 0.30.16x). Returns
// (key, nil, nil) when the layer is disabled — callers MUST treat
// handle==nil as "skip the content lookup, take the existing per-user
// L1 path".
//
// IDENTITY-FREE BY CONSTRUCTION: Username/Groups are left ZERO; the
// ComputeKey branch skips identity for CacheEntryClassWidgetContent
// (resolved.go). The serve-time gateWidgetEnvelope re-derives every
// embedded status.resourcesRefs.items[].allowed flag under the request
// identity before the body is written to the response — see widgets.go.
// A request that lacks UserInfo would not be able to run the gate, but
// keying itself is identity-independent so we do NOT bail on missing
// identity here; the gate's served=false branch handles the no-identity
// case symmetrically with the existing per-user lookup (which DOES
// nil-check UserInfo at dispatchCacheLookupKey).
func dispatchWidgetContentKey(ctx context.Context, group, version, resource, namespace, name string, perPage, page int, extras map[string]any) (string, cacheHandle, *cache.ResolvedKeyInputs) {
	c := cache.ResolvedCache()
	if c == nil {
		return "", nil, nil
	}
	if !cache.WidgetContentL1Enabled() {
		return "", nil, nil
	}
	inputs := cache.ResolvedKeyInputs{
		CacheEntryClass: cache.CacheEntryClassWidgetContent,
		Group:           group,
		Version:         version,
		Resource:        resource,
		Namespace:       namespace,
		Name:            name,
		// Username/Groups intentionally zero — see header.
		PerPage: perPage,
		Page:    page,
		Extras:  extras,
	}
	return cache.ComputeKey(inputs), c, &inputs
}

// dispatchCacheLookupKey builds the L1 resolved-output cache key and
// returns the live cache handle, if the L1 layer is enabled. Returns
// (key, nil, nil) when L1 is disabled — callers MUST treat handle==nil
// as "skip cache lookup, take the 0.30.6 path".
//
// User identity is read from the request context; on error (missing
// or unparseable UserInfo) we treat the request as uncacheable —
// returning a nil handle — so the request still resolves correctly but
// never reads or writes the L1 cache. A keyless request would risk
// cross-user leaks, which is unacceptable.
//
// 0.30.8: the function also returns the canonical ResolvedKeyInputs so
// the caller can stash it on the L1 entry. Refresher reuses Inputs to
// drive a re-resolve on UPDATE/PATCH events.
//
// Ship A.3 / 0.30.179 — identity was folded as a single uint64 via
// cache.BindingSetHash. The pre-A.3 Username + sorted Groups literal
// columns were already GONE from ResolvedKeyInputs at that point.
//
// Ship 0.30.240 — identity REMOVED from ResolvedKeyInputs entirely
// (design 2026-06-02). BindingSetHash, RepresentativeUsername,
// RepresentativeGroups are all gone. The L1 cell is now SA-maximal and
// shared across all customers; per-user RBAC narrowing runs at SERVE
// time via the post-cache filter. The pattern mirrors client-go's
// cache.Indexer: keyed store + per-request view filter at the consumer.
//
// IDENTITY GUARD: we STILL require an identity in the request context.
// The downstream serve-time filter narrows rows against it; an
// anonymous request would have no narrowing target and would risk
// serving SA-maximal bytes to an unauthenticated caller. Same fail-
// closed posture as v3 — only the gating point moved from key
// construction to serve filter precondition.
func dispatchCacheLookupKey(ctx context.Context, handlerKind, group, version, resource, namespace, name string, perPage, page int, extras map[string]any) (string, cacheHandle, *cache.ResolvedKeyInputs) {
	c := cache.ResolvedCache()
	if c == nil {
		return "", nil, nil
	}
	if _, err := xcontext.UserInfo(ctx); err != nil {
		// Defence in depth — without an identity the serve-time
		// filter has nothing to narrow against. Skip the cache.
		return "", nil, nil
	}
	inputs := cache.ResolvedKeyInputs{
		CacheEntryClass: handlerKind,
		Group:           group,
		Version:         version,
		Resource:        resource,
		Namespace:       namespace,
		Name:            name,
		PerPage:         perPage,
		Page:            page,
		Extras:          extras,
	}
	return cache.ComputeKey(inputs), c, &inputs
}

// cacheHandle is the narrow interface the dispatchers depend on. The
// real implementation is *cache.resolvedCache; tests can substitute
// stubs without dragging in the whole singleton. Kept package-private
// so it never leaks beyond dispatchers.
type cacheHandle interface {
	Get(key string) (*cache.ResolvedEntry, bool)
	Put(key string, entry *cache.ResolvedEntry)
}

// emitResolvedCacheLookup writes the per-request falsifier line per
// plan §"Code-path falsifier":
//
//	resolved_cache.lookup hit=true|false key_hash=... resident_bytes=N
//
// We log at INFO so a casual grep on production logs proves whether
// L1 is firing.
//
// Ship OBS-1 (0.30.186): in addition to the log line, bump the
// per-(handlerKind, gvrString) hit/miss counter in
// l1_lookup_metrics.go so /debug/vars exposes the same signal
// without requiring log-line scraping. The bump is one atomic add
// per call — already on the request-path serial budget.
func emitResolvedCacheLookup(log *slog.Logger, handlerKind, gvrString, key string, hit bool, residentBytes int) {
	if log != nil {
		log.Info("resolved_cache.lookup",
			slog.String("subsystem", "cache"),
			slog.String("handler", handlerKind),
			slog.String("gvr", gvrString),
			slog.String("key_hash", key),
			slog.Bool("hit", hit),
			slog.Int("resident_bytes", residentBytes),
		)
	}
	recordL1Lookup(handlerKind, gvrString, hit)
}

// emitDispatchCacheKeyDiag — Ship 0.30.188 — pure-additive diagnostic
// instrumentation for the PIP-seed vs dispatcher-get key-divergence
// investigation. Emits a structured slog line carrying every component
// that folds into `dispatchCacheLookupKey`'s ComputeKey, so the three
// emit sites (PIP seed Put, dispatcher Get, per-user fallback Put) can
// be diff'd line-by-line for ONE widget/restaction to identify which
// field(s) drive the seed→serve miss.
//
// IDENTITY EXTRACTION: pulls Username + Groups from xcontext.UserInfo
// (the cohort ctx for seed sites, the request ctx for dispatcher
// sites). On missing/unparseable identity (defence in depth — should
// never happen at these sites since they all already nil-checked the
// handle) we emit a sentinel "anonymous" / empty groups so the log
// still differentiates the divergent component.
//
// Ship 0.30.240 — `binding_set_hash` field REMOVED from the emission.
// The v4 L1 key is identity-free (design 2026-06-02 §5); the
// `resolvedKeyVersion v3→v4` salt rotation already observably distinct-
// uates v3 keys from v4 keys on the rolling restart, so the per-cohort
// hash field is dead carry-over. Username + Groups stay on the line for
// per-user request correlation; the cache cell itself no longer
// differentiates by identity.
//
// NIL-GUARD: a nil `inputs` (cache disabled or identity missing) is
// permitted; the function still emits so the "why was the lookup
// skipped" question is greppable. The dispatcher callers MUST still
// nil-check the handle separately.
//
// COST: one slog.Info per /call per site. Volume = O(num_widgets +
// num_restactions) at startup (PIP seed) + O(/call rate) at serve time.
// Acceptable for the diagnostic ship.
func emitDispatchCacheKeyDiag(log *slog.Logger, site string, ctx context.Context,
	cacheKey string, inputs *cache.ResolvedKeyInputs,
	handlerKind, group, version, resource, namespace, name string,
	perPage, page int, extras map[string]any,
) {
	if log == nil {
		return
	}
	var (
		username string
		groups   []string
	)
	if ui, err := xcontext.UserInfo(ctx); err == nil {
		username = ui.Username
		groups = ui.Groups
	}
	_ = inputs // Ship 0.30.240 — inputs.BindingSetHash field gone; the
	// signature is preserved so callers don't need to rewire.
	log.Info("dispatch.cache_key.computed",
		slog.String("subsystem", "cache"),
		slog.String("site", site),
		slog.String("key_hash", cacheKey),
		slog.String("username", username),
		slog.Any("groups", groups),
		slog.String("handler_kind", handlerKind),
		slog.String("gvr", fmt.Sprintf("%s/%s, Resource=%s", group, version, resource)),
		slog.String("namespace", namespace),
		slog.String("name", name),
		slog.Int("per_page", perPage),
		slog.Int("page", page),
		slog.Int("extras_len", len(extras)),
	)

	// Ship 0.30.190 Fix B / 0.30.191 carried-forward — additive
	// single-line stderr lane. The slog.Info above is the authoritative
	// emit; the existing call stays ON regardless of this knob. Default
	// OFF; chart values.yaml flips it ON for diagnostic ships and OFF
	// for steady-state.
	//
	// Ship 0.30.240 — `bsh=` field REMOVED from the stderr line too.
	if env.True("DISPATCH_KEY_DIAG_ENABLED") {
		fmt.Fprintf(os.Stderr,
			"DISPATCH_KEY_DIAG site=%s key=%s user=%s groups=%v handler=%s gvr=%s/%s/%s ns=%s name=%s pp=%d p=%d xlen=%d\n",
			site, cacheKey, username, groups,
			handlerKind, group, version, resource, namespace, name, perPage, page, len(extras),
		)
	}
}

// encodeResolvedJSON marshals res with a single canonical encoder shape.
//
// Ship GMC / 0.30.174 — dropped SetIndent("", "  ") for a ~25% wire-
// size reduction at admin scale (the indented LIST envelopes were
// dominated by per-line spaces). The cache-hit + cache-miss paths
// remain byte-equal to each other (the contract here is internal
// consistency); the frontend re-parses JSON regardless of indentation
// so the change is transparent above HTTP.
//
// Centralising the encode here ensures every dispatch site returns
// the same shape; any divergence would break the "cache=on warm
// response equals cache=off response" invariant.
func encodeResolvedJSON(res any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if err := enc.Encode(res); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeResolvedJSON writes the canonical Content-Type + 200 + payload.
// We deliberately do NOT log here on errors writing to the wire — a
// client disconnect mid-write is normal and not actionable.
func writeResolvedJSON(wri http.ResponseWriter, payload []byte) {
	wri.Header().Set("Content-Type", "application/json")
	wri.WriteHeader(http.StatusOK)
	_, _ = wri.Write(payload)
}

// snowplowSACtx — Ship 0.30.166 / #307 AMEND. Returns the snowplow
// ServiceAccount endpoint + *rest.Config the dispatcher attaches to every
// per-request context so the api-stage K8s GET/LIST dispatch can engage
// dispatchViaInternalRESTConfig (client-go transport that correctly
// installs the cluster CA) instead of falling through to plumbing's
// httpcall.Do (whose tlsConfigFor drops the CA for token-auth endpoints
// — the 0.30.103 / 0.30.165 x509 defect).
//
// IDENTICAL MECHANISM to the prior-art sites that already use it:
//   - Phase 1 walker: phase1_walk.go:231 attaches the same SA pair to the
//     SA-credentialed startup walk context (the 0.30.104 fix surface).
//   - L1 refresher: resolve_populate.go:117-131 attaches the same SA pair
//     to the background re-resolve context (the 0.30.113 Part B fix
//     surface, where the SA is transport-only and per-user identity comes
//     from the cached Inputs).
// 0.30.166 extends the SAME attach to the per-request restactions.go +
// widgets.go dispatcher entries — the previously-unpatched surface that
// is the actual cache-off /call request path. See ship-307-tls-x509-cache-
// off-design-amend-2026-05-22.md §2.
//
// GRACEFUL DEGRADATION (AC-307.7): out-of-cluster unit tests have no
// projected SA volume and no KUBERNETES_SERVICE_HOST env — both
// idynamic.ServiceAccountEndpoint() and idynamic.ServiceAccountRESTConfig()
// error. The helper swallows the error and returns (nil, nil). The
// dispatcher caller's nil-guarded attach (`if saEP != nil && saRC != nil`)
// then SKIPS the WithInternalEndpoint / WithInternalRESTConfig calls, so
// the request ctx is byte-identical to pre-0.30.166 and the request flows
// through the unchanged httpcall.Do path. Every unit test in the tree is
// preserved verbatim.
//
// CONCURRENCY: idynamic.ServiceAccountEndpoint memoises a process-wide
// singleton under its own mutex; the helper itself is stateless and safe
// for concurrent callers (every per-request dispatch is a fresh goroutine).
//
// LOG VISIBILITY: by design the helper is silent on the per-request
// happy path — repeated nil-warns would flood the log. The startup-time
// warn already exists at dispatchers.go:58-66 (RegisterRefreshHandlers)
// where the SAME SA pair is fetched once for the refresher; a missing
// SA there is surfaced once and is sufficient for diagnosis.
func snowplowSACtx() (*endpoints.Endpoint, *rest.Config) {
	saEP, err := idynamic.ServiceAccountEndpoint()
	if err != nil {
		return nil, nil
	}
	saRC, err := idynamic.ServiceAccountRESTConfig()
	if err != nil {
		return nil, nil
	}
	return saEP, saRC
}

// rcFromCtx returns the SA *rest.Config that an internal driver
// (Phase 1 walker, L1 refresher, PIP seed, content prewarm) attached
// upstream via cache.WithInternalRESTConfig. Returns nil when no internal
// rc is on ctx (the per-user request path) OR when the attached value is
// of the wrong type.
//
// Ship 0.30.230 fix-at-root: ResolveOptions{RC,SArc} construction sites
// downstream of an internal driver use this helper to thread the SA rc
// at the construction site, so cache.GVRFor / dynamic.NewClient /
// crdschema.ValidateObjectStatus never receive a nil rc. Per-user
// dispatchers (widgets.go, restactions.go) pass r.saRC directly from
// their handler struct field — they do not need this helper.
func rcFromCtx(ctx context.Context) *rest.Config {
	v, ok := cache.InternalRESTConfigFromContext(ctx)
	if !ok {
		return nil
	}
	rc, _ := v.(*rest.Config)
	return rc
}
