package api

import (
	"context"
	"fmt"
	"log/slog"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	httpcall "github.com/krateoplatformops/plumbing/http/request"
	"github.com/krateoplatformops/plumbing/http/response"
	"github.com/krateoplatformops/plumbing/maps"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/dynamic"
	"k8s.io/client-go/rest"
)

const (
	//annotationKeyVerboseAPI = "krateo.io/verbose"
	headerAcceptJSON = "Accept: application/json"
)

type ResolveOptions struct {
	RC      *rest.Config
	AuthnNS string
	Verbose bool
	Items   []*templates.API
	PerPage int
	Page    int
	Extras  map[string]any

	// Watcher is the cluster-wide informer cache. When nil (the
	// default at 0.30.1, since CACHE_ENABLED defaults to false),
	// every API call takes the apiserver branch via httpcall.Do.
	// Routing for K8s-served endpoints flips on at 0.30.2.
	Watcher *cache.ResourceWatcher
}

func Resolve(ctx context.Context, opts ResolveOptions) map[string]any {
	if len(opts.Items) == 0 {
		return map[string]any{}
	}

	if opts.RC == nil {
		var err error
		opts.RC, err = rest.InClusterConfig()
		if err != nil {
			return map[string]any{}
		}
	}

	log := xcontext.Logger(ctx)
	log.Info("pagination options", slog.Int("page", opts.Page), slog.Int("perPage", opts.PerPage))

	// Cache routing gate. At 0.30.1 cache.Disabled() defaults to true
	// and Watcher is nil — every API call takes the apiserver branch.
	// The 0.30.2 ship lands the cache-served branch keyed off Watcher.
	if cache.Disabled() || opts.Watcher == nil {
		log.Debug("api.Resolve: cache disabled or watcher unset; using apiserver branch",
			slog.Bool("cache_disabled", cache.Disabled()),
			slog.Bool("watcher_nil", opts.Watcher == nil))
	}

	user, err := xcontext.UserInfo(ctx)
	if err != nil {
		log.Error("unable to fetch user info from context", slog.Any("err", err))
		return map[string]any{}
	}

	// Sort API by Depends
	names, err := topologicalSort(opts.Items)
	if err != nil {
		log.Error("unable to sorted api by deps", slog.Any("error", err))
		return map[string]any{}
	}
	log.Debug("sorted api by deps", slog.Any("names", names))

	apiMap := make(map[string]*templates.API, len(opts.Items))
	for _, id := range names {
		for _, el := range opts.Items {
			if el.Name == id {
				apiMap[id] = el
				break
			}
		}
	}
	log.Debug("created api map", slog.Int("total", len(apiMap)))

	// Endpoints reference mapper
	mapper := endpointReferenceMapper{
		authnNS:  opts.AuthnNS,
		username: user.Username,
		rc:       opts.RC,
	}

	dict := map[string]any{}
	if opts.Extras != nil {
		dict = maps.DeepCopyJSON(opts.Extras)
	}

	if opts.PerPage > 0 && opts.Page > 0 {
		dict["slice"] = map[string]any{
			"page":    opts.Page,
			"perPage": opts.PerPage,
			"offset":  (opts.Page - 1) * opts.PerPage,
		}
	}

	log.Info("base dict for api resolver", slog.Any("dict", dict))

	for _, id := range names {
		// Get the api with this identifier
		apiCall, ok := apiMap[id]
		if !ok {
			log.Warn("api not found in apiMap", slog.Any("name", id))
			continue
		}
		if apiCall.Headers == nil {
			apiCall.Headers = []string{headerAcceptJSON}
		}

		// Tag 0.30.9 Sub-scope A: detect userAccessFilter.
		// When set, the dispatch uses snowplow's ServiceAccount
		// endpoint (cluster-wide read) — NOT the per-user
		// clientconfig — and the response is in-process-refiltered
		// per object through EvaluateRBAC. When unset, the dispatch
		// path is unchanged from 0.30.8 (per-user-token via the
		// endpointReferenceMapper). Per Revision 5 (binding): atomic
		// ship — no gate flag. Portal RestActions opt in by adding
		// the userAccessFilter stanza; the resolver branches
		// per-stage.
		uafActive := apiCall.UserAccessFilter != nil

		// User-bearer-token append: only for non-UAF stages. When
		// UAF is active the SA endpoint carries the SA token (no
		// user-bearer override needed); appending the user token
		// here would route the call through the user's credentials
		// instead of the SA's — breaking the entire UAF mechanism.
		if !uafActive {
			if accessToken, _ := xcontext.AccessToken(ctx); accessToken != "" {
				if apiCall.EndpointRef == nil || ptr.Deref(apiCall.ExportJWT, false) {
					apiCall.Headers = append(apiCall.Headers,
						fmt.Sprintf("Authorization: Bearer %s", accessToken))
				}
			}
		}

		// Resolve the endpoint. UAF stages use the snowplow-SA
		// endpoint; non-UAF stages go through the per-user
		// clientconfig (or the named EndpointRef) as before.
		var ep endpoints.Endpoint
		if uafActive {
			saEP, saErr := dynamic.ServiceAccountEndpoint()
			if saErr != nil {
				log.Error("userAccessFilter: cannot acquire ServiceAccount endpoint; falling through to per-user dispatch (degraded mode)",
					slog.String("name", id), slog.Any("err", saErr))
				// Fail-closed-but-respond: per Revision 5 atomic
				// ship there is no toggle to fall back to the
				// per-user path correctly (we'd leak the user
				// bearer token to a SA-marked stage). Returning
				// an empty result for this stage and continuing.
				dict[id] = map[string]any{"items": []any{}}
				continue
			}
			ep = *saEP
		} else {
			resolved, err := mapper.resolveOne(ctx, apiCall.EndpointRef)
			if err != nil {
				log.Error("unable to resolve api endpoint reference",
					slog.String("name", id), slog.Any("ref", apiCall.EndpointRef), slog.Any("error", err))
				return dict
			}
			ep = resolved
		}
		if opts.Verbose {
			ep.Debug = opts.Verbose
		}
		log.Debug("resolved endpoint for api call",
			slog.String("name", id), slog.String("host", ep.ServerURL),
			slog.Bool("uaf", uafActive))

		tmp := createRequestOptions(log, apiCall, dict)
		if len(tmp) == 0 {
			log.Warn("empty request options for http call", slog.Any("name", id))
			continue
		}

		for _, call := range tmp {
			call.Endpoint = &ep
			call.ResponseHandler = jsonHandler(ctx, jsonHandlerOptions{
				key: id, out: dict, filter: apiCall.Filter,
			})

			log.Debug("calling api", slog.String("name", id),
				slog.String("host", call.Endpoint.ServerURL), slog.String("path", call.Path),
				slog.Any("out", dict),
			)

			res := httpcall.Do(ctx, call)
			if res.Status == response.StatusFailure {
				log.Error("api call response failure", slog.String("name", id),
					slog.String("host", call.Endpoint.ServerURL), slog.String("path", call.Path),
					slog.String("error", res.Message))

				tmp, err := response.AsMap(res)
				if err != nil {
					log.Warn("unable to encode status as dict", slog.Any("err", err))
				}

				if len(tmp) > 0 {
					dict[call.ErrorKey] = tmp
				} else {
					dict[call.ErrorKey] = res.Message
				}

				if !call.ContinueOnError {
					return dict
				}
			}

			log.Info("api successfully resolved",
				slog.String("name", id),
				slog.String("host", call.Endpoint.ServerURL), slog.String("path", call.Path),
				slog.Any("depth", mapDepth(dict)),
			)
		}

		// Tag 0.30.9 Sub-scope A: refilter the SA-dispatched result
		// in-process. Runs AFTER all dispatched calls for this API
		// stage complete + their jsonHandler has populated dict[id].
		// Per Revision 2 binding: EvaluateRBAC fires per object —
		// this is the load-bearing security gate that turns cluster-
		// wide-read into user-scoped result sets.
		if uafActive {
			rf := applyUserAccessFilter(ctx, dict, apiCall)
			emitRefilterFalsifier(log, apiCall, user.Username, rf)
		}
	}

	removeManagedFields(dict)
	//delete(dict, "slice")

	return dict
}

func removeManagedFields(data any) {
	switch v := data.(type) {
	case map[string]any:
		delete(v, "managedFields")
		// scansiona tutte le altre chiavi
		for _, val := range v {
			removeManagedFields(val)
		}
	case []any:
		for _, elem := range v {
			removeManagedFields(elem)
		}
	// other types (string, int, ecc.) -> do nothing
	default:
		return
	}
}
