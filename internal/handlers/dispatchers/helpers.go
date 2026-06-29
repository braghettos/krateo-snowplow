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

	return normalizePagination(perPage, page)
}

// normalizePagination is the SINGLE normalization core for the cache-key
// pagination fold, shared by the EMIT path (paginationInfo, from the /call
// query) and the SUBSCRIPTION path (DeriveSubscriptionKey, from ?sub= coords)
// so both stamp byte-identical (perPage, page) into ComputeKey.
//
// (#64 real root cause) A non-paginated /call sends no perPage/page query →
// paginationInfo's -1,-1 default → the emit key folds "-1","-1". The frontend
// subscription sends perPage:0/page:0 (or omits them → json zero) → 0,0. "-1"
// != "0" → key mismatch → published + subscribers>=1 + delivered:0, for EVERY
// class (the fold is class-independent). EXTRACTING the normalization (rather
// than re-implementing it on the subscription side) is the fix: the drift
// between two hand-written copies is exactly what caused the bug.
//
// Rules (mirrors the historical paginationInfo, the page=1 rule INCLUDED):
//   - perPage <= 0  → -1 (non-paginated sentinel)
//   - page    <= 0  → -1 (non-paginated sentinel)
//   - perPage > 0 && page <= 0 → page = 1 (a paginated request with no/zero
//     page means the first page; this rule MUST survive the extraction)
func normalizePagination(perPage, page int) (int, int) {
	if perPage <= 0 {
		perPage = -1
	}
	if page <= 0 {
		page = -1
	}
	if perPage > 0 && page <= 0 {
		page = 1
	}
	return perPage, page
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

	// Ship 0.30.242 H.c-layered (Phase 2 step 2a) — EvaluateRBAC returns
	// (allowed, matchedBindingUID, err). This per-item caller ignores
	// matchedBindingUID; cache-key callers consume it (helpers.go:202 +
	// :306 via the per-request memo, Phase 2b).
	allowed, _, evalErr := rbac.EvaluateRBAC(ctx, rbac.EvaluateOptions{
		Username:  ui.Username,
		Groups:    ui.Groups,
		Verb:      "get",
		Group:     gvr.Group,
		Resource:  gvr.Resource,
		Namespace: namespace,
		// Ship L1 (0.30.252): per-item caller discards
		// matchedBindingUID — skip the CRB/RB stable-sort.
		SkipBindingUID: true,
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
// Ship 0.30.242 H.c-layered Phase 2b — identity is folded as the
// per-layer BindingUID (the first-match binding's UID that authorised
// the layer's GET, returned by rbac.EvaluateRBAC). Replaces the v3
// BindingSetHash uint64 cohort hash. Per-binding sharing (design §1.2):
// two users granted by the SAME binding share the L1 cell.
//
// PATH B (Diego ratified 2026-06-03 R3 deferral): the BindingUID is
// derived via a DIRECT rbac.EvaluateRBAC call per request, not via the
// per-request memo. The memo type ships scaffolding-only in 2b; F3
// falsifier validates this deferral. If F3 reveals per-request memo
// consistency is load-bearing, plumbing is pulled forward as Phase 2d.
//
// FAIL-CLOSED: allowed=false / err yields bindingUID="" (the cell key
// collapses to the empty-identity row — same shape as cache=off's
// transparent fallback, project_cache_off_is_transparent_fallback).
func dispatchCacheLookupKey(ctx context.Context, handlerKind, group, version, resource, namespace, name string, perPage, page int, extras map[string]any) (string, cacheHandle, *cache.ResolvedKeyInputs) {
	c := cache.ResolvedCache()
	if c == nil {
		return "", nil, nil
	}
	ui, err := xcontext.UserInfo(ctx)
	if err != nil {
		// Defence in depth — without an identity we cannot key
		// safely. Skip the cache for this request.
		return "", nil, nil
	}
	// Path B direct call — derive BindingUID for the layer's GET-permit.
	_, bindingUID, _ := rbac.EvaluateRBAC(ctx, rbac.EvaluateOptions{
		Username:  ui.Username,
		Groups:    ui.Groups,
		Verb:      "get",
		Group:     group,
		Resource:  resource,
		Namespace: namespace,
		Name:      name,
	})
	inputs := cache.ResolvedKeyInputs{
		CacheEntryClass: handlerKind,
		Group:           group,
		Version:         version,
		Resource:        resource,
		Namespace:       namespace,
		Name:            name,
		BindingUID:      bindingUID,
		// Representative identity for the refresher's re-resolve.
		// Carried on Inputs but NOT folded into ComputeKey (the cell
		// is keyed by BindingUID, not by the literal name). The first
		// writer's identity is the cell's representative; every member
		// of the equivalence class authorised by this BindingUID
		// produces byte-identical output (per-binding sharing — design
		// §1.2).
		RepresentativeUsername: ui.Username,
		RepresentativeGroups:   ui.Groups,
		PerPage:                perPage,
		Page:                   page,
		Extras:                 extras,
	}
	return cache.ComputeKey(inputs), c, &inputs
}

// unionForKey builds the body-dependency fingerprint the widget L1 key must
// fold under inline-extras design P (§1, §4.3): the UNION of both
// author-declared inline maps (apiRef.extras + resourcesRefsTemplateExtras)
// PLUS the per-request extras, with REQUEST WINNING on every collision
// (mirroring the per-surface request-wins precedence in widgets.Resolve).
//
// WHY a flat union is the correct key material: a change to EITHER inline map
// changes the resolved body (apiRef.extras feeds the apiRef fetch → ds; the
// rrt block feeds the resourcesRefsTemplate jq), so the widget content +
// per-cohort keys MUST vary on either. The union is collision-sound because
// the widget key ALSO folds Group/Version/Resource/Namespace/Name (and, on
// the per-cohort cell, BindingUID) — so two DIFFERENT widgets never share a
// cell regardless of extras, and for the SAME widget the inline maps are fixed
// by its CR (cannot vary request-to-request). The union therefore only needs
// to discriminate the per-request variable (request extras) and the
// per-widget-fixed inline contribution — which a flat superset does
// faithfully (design §1 collision-soundness note).
//
// SCOPING: this union keys the WIDGET cell only. The apiRef sub-cell
// (RAFullList) is keyed separately by the apiRef-EFFECTIVE map
// (merge(apiRefInline, request)) threaded through apiref.Resolve's Extras —
// the rrt-inline map MUST NOT enter the apiRef sub-cell (it does not affect
// the apiRef fetch). See widgets.go's resolveApiRef + ra_full_list.go.
//
// All inputs are read off the widget CR via maps.NestedMap (deep copies) or
// are the per-request extras; the result is a fresh map and never aliases any
// of them. nil/empty all-round ⇒ a fresh empty map ⇒ ComputeKey's len>0 guard
// skips the extras fold ⇒ the key is byte-identical to a no-extras request
// (backward-compat, falsifier #8). Mechanism-uniform (feedback_no_special_cases):
// every key folds the same way; no widget-name table, no key allowlist.
func unionForKey(apiRefInline, rrtInline, request map[string]any) map[string]any {
	out := make(map[string]any, len(apiRefInline)+len(rrtInline)+len(request))
	// Inline maps first; request overwrites last so the request value wins on
	// collision — matching the resolved body precedence. (Order between the two
	// inline maps is irrelevant for a key fingerprint: both are CR-fixed, so
	// the body is a deterministic function of the CR regardless.)
	for k, v := range apiRefInline {
		out[k] = v
	}
	for k, v := range rrtInline {
		out[k] = v
	}
	for k, v := range request {
		out[k] = v
	}
	return out
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
// investigation (architect's TRACED diff on `BindingSetHash`). Emits a
// structured slog line carrying ALL the components that fold into
// `dispatchCacheLookupKey`'s ComputeKey, so the three emit sites (PIP
// seed Put, dispatcher Get, per-user fallback Put) can be diff'd line-
// by-line for ONE widget/restaction to identify which field(s) drive
// the seed→serve miss.
//
// IDENTITY EXTRACTION: pulls Username + Groups from xcontext.UserInfo
// (the cohort ctx for seed sites, the request ctx for dispatcher
// sites). On missing/unparseable identity (defence in depth — should
// never happen at these sites since they all already nil-checked the
// handle) we emit a sentinel "anonymous" / empty groups so the log
// still differentiates the divergent component.
//
// BindingSetHash is read from `inputs.BindingSetHash` — the SAME value
// that ComputeKey folded into the returned cacheKey — rather than re-
// computing it. This guarantees the logged hash and the cache key are
// derived from the same snapshot read.
//
// NIL-GUARD: a nil `inputs` (cache disabled or identity missing) is
// permitted; the function still emits with binding_uid="" so the
// "why was the lookup skipped" question is greppable. The dispatcher
// callers MUST still nil-check the handle separately.
//
// COST: one slog.Info per /call per site.
//
// Ship 0.30.242 H.c-layered Phase 2b — the diagnostic field renamed
// from `binding_set_hash uint64` to `binding_uid string` (the new
// cell-key identity dimension). Same diagnostic intent (find divergent
// key components between PIP-seed and dispatcher-get sites); new field
// shape carries the per-layer first-match binding identity instead of
// the v3 cohort hash.
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
	var bindingUID string
	if inputs != nil {
		bindingUID = inputs.BindingUID
	} else {
		// Compute directly so the field still differentiates — the
		// cache-disabled / no-identity branch returns nil inputs, but
		// the diagnostic is interested in the per-layer BindingUID
		// value itself. Path B direct call (Phase 2b R3 deferral).
		_, bindingUID, _ = rbac.EvaluateRBAC(ctx, rbac.EvaluateOptions{
			Username:  username,
			Groups:    groups,
			Verb:      "get",
			Group:     group,
			Resource:  resource,
			Namespace: namespace,
			Name:      name,
		})
	}
	log.Info("dispatch.cache_key.computed",
		slog.String("subsystem", "cache"),
		slog.String("site", site),
		slog.String("key_hash", cacheKey),
		slog.String("binding_uid", bindingUID),
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
	// single-line stderr lane. Same intent as the pre-ship lane; field
	// renamed from bsh (binding-set-hash uint64) to buid (binding-UID
	// string).
	if env.True("DISPATCH_KEY_DIAG_ENABLED") {
		fmt.Fprintf(os.Stderr,
			"DISPATCH_KEY_DIAG site=%s key=%s buid=%s user=%s groups=%v handler=%s gvr=%s/%s/%s ns=%s name=%s pp=%d p=%d xlen=%d\n",
			site, cacheKey, bindingUID, username, groups,
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

// refreshKeyHeader is the response header carrying the L1 subscription key
// for the live-refresh signal (Ship 1). The frontend reads it to arm a
// /refreshes subscription for this widget (design §6). Additive and invisible
// to the JSON body contract; it MUST be in CORS ExposedHeaders (main.go) so a
// cross-origin fetch/react-query layer can read it.
const refreshKeyHeader = "X-Snowplow-Refresh-Key"

// refreshClassHeader carries the CacheEntryClass the response was keyed
// under. Paired with refreshKeyHeader so the frontend arms the EXACT
// /refreshes subscription class instead of guessing widgets-vs-widgetContent
// (frontend guide §2.5/§8). The value matches SubscriptionCoordinates.Class
// exactly: "widgets" | "widgetContent" | "restactions" | "apistage" |
// "raFullList". Additive; MUST be in CORS ExposedHeaders (main.go).
const refreshClassHeader = "X-Snowplow-Refresh-Class"

// setRefreshKeyHeader stamps the live-refresh subscription key + the class it
// was keyed under, before WriteHeader. No-op on an empty key (L1 disabled /
// RBAC-skipped / cache-off) — the headers are purely additive and must never
// appear empty. class is the CacheEntryClass the dispatcher keyed this
// response by (the SAME class the caller passes to emitResolvedCacheLookup);
// it is stamped only alongside a non-empty key so the two headers are always
// consistent. MUST be called before writeResolvedJSON (which calls
// WriteHeader).
func setRefreshKeyHeader(wri http.ResponseWriter, key, class string) {
	if key == "" {
		return
	}
	wri.Header().Set(refreshKeyHeader, key)
	if class != "" {
		wri.Header().Set(refreshClassHeader, class)
	}
}

// writeResolvedJSON writes the canonical Content-Type + 200 + payload.
// We deliberately do NOT log here on errors writing to the wire — a
// client disconnect mid-write is normal and not actionable.
//
// MEMORY CEILING (#99, ruled CLOSE-DOCUMENTED 2026-06-12): payload is
// held fully in heap before this single Write. The largest payload today
// is the admin compositions-list at ~12.9 MB (49K compositions;
// transient, 0.05 mix-weight). Bounded and harmless at current traffic.
// Do NOT convert to chunked/http.ResponseController streaming: naive
// streaming on large payloads caused a 22-38x warm-latency regression
// (0.30.176, feedback_no_naive_compression_middleware) and there is no
// streaming-write prior art in this package. If a payload ever exceeds a
// few tens of MB, cache PRE-ENCODED bytes at the value layer rather than
// streaming the write.
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
//
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
