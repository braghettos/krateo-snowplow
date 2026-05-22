package dispatchers

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
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
	inputs := cache.ResolvedKeyInputs{
		CacheEntryClass: handlerKind,
		Group:           group,
		Version:         version,
		Resource:        resource,
		Namespace:       namespace,
		Name:            name,
		Username:        ui.Username,
		Groups:          ui.Groups,
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
func emitResolvedCacheLookup(log *slog.Logger, handlerKind, key string, hit bool, residentBytes int) {
	if log == nil {
		return
	}
	log.Info("resolved_cache.lookup",
		slog.String("subsystem", "cache"),
		slog.String("handler", handlerKind),
		slog.String("key_hash", key),
		slog.Bool("hit", hit),
		slog.Int("resident_bytes", residentBytes),
	)
}

// encodeResolvedJSON marshals res with the same json.Encoder settings
// the dispatchers used before 0.30.7 (SetIndent("", "  ")). Centralising
// the encode here ensures the cache-hit path returns byte-identical
// output to the cache-miss path; any divergence would break the
// "cache=on warm response equals cache=off response" contract.
func encodeResolvedJSON(res any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
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
