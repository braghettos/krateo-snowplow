package objects

import (
	"context"
	"log/slog"
	"net/http"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/http/response"
	"github.com/krateoplatformops/plumbing/kubeconfig"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/dynamic"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	lastAppliedConfigAnnotation = "kubectl.kubernetes.io/last-applied-configuration"
)

type Result struct {
	GVR          schema.GroupVersionResource
	Unstructured *unstructured.Unstructured
	Err          *response.Status
}

func Get(ctx context.Context, ref templatesv1.ObjectReference) (res Result) {
	log := xcontext.Logger(ctx)

	// Cache routing gate. When the cache subsystem is disabled
	// (CACHE_ENABLED unset/false) there is no informer to serve from;
	// every read goes straight to the apiserver. We do NOT increment the
	// fallthrough counter here — the counters measure the 0.30.96 routed
	// pivot's serve rate, and cache-disabled is "pivot inactive", not a
	// pivot fallthrough.
	if cache.Disabled() {
		return getFromAPIServer(ctx, ref)
	}

	// 0.30.96 Gap A — routed branch. Serve widget / entry-CR object GETs
	// from the in-process informer cache. Gated by the SAME
	// RESOLVER_USE_INFORMER flag as the 0.30.95 resolver pivot: with the
	// flag unset this whole block is skipped and the binary is
	// byte-identical to 0.30.95 (R-FALSE-1 invariant preserved).
	//
	// Per feedback_no_special_cases.md: the branch is uniform across
	// every GVR — the gate is cache-mode + informer-state predicates,
	// never a per-resource switch.
	if useInformer() {
		// Lazy-start the objects_get.summary goroutine the first time
		// the pivot is exercised (sync.Once-bounded; never started when
		// the flag stays off for the process lifetime).
		startObjectsGetSummary()

		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err == nil {
			gvr := gv.WithResource(ref.Resource)
			rw := cache.Global()
			// Passthrough mode has no informers (cache=off diagnostic
			// mode); metadata-only GVRs carry only ObjectMeta — neither
			// can satisfy a full-object resolver read.
			if rw != nil && !rw.IsPassthrough() && !rw.IsMetadataOnly(gvr) {
				if !rw.IsSynced(gvr) {
					// Not registered / not yet synced. Fire best-effort
					// lazy registration so a SUBSEQUENT call can serve;
					// EnsureResourceType is idempotent + singleflighted
					// under rw.mu. This call still falls through to the
					// apiserver — pre-sync reads would look identical to
					// a real NotFound.
					_, _ = rw.EnsureResourceType(gvr)
					log.Debug("objects.Get: informer not synced; apiserver fallthrough",
						slog.String("gvr", gvr.String()),
						slog.String("ns", ref.Namespace),
						slog.String("name", ref.Name))
				} else if obj, hit := rw.GetObject(gvr, ref.Namespace, ref.Name); hit {
					// Cache hit. DeepCopy so the strip never mutates the
					// shared informer-store object, then apply the EXACT
					// same field strips getFromAPIServer performs (see
					// stripForServe — byte-equivalence is mandatory per
					// feedback_cache_must_not_constrain_jq.md).
					out := obj.DeepCopy()
					stripForServe(out)
					objectsGetInformerServed.Add(1)
					log.Debug("objects.Get: served from informer",
						slog.String("gvr", gvr.String()),
						slog.String("ns", ref.Namespace),
						slog.String("name", ref.Name))
					res.GVR = gvr
					res.Unstructured = out
					return res
				}
				// GET-miss (informer synced, object absent): fall
				// through to the apiserver so the caller sees the
				// apiserver NotFound envelope shape.
			}
		}
		// Any fall-through inside the routed branch — parse failure,
		// nil/passthrough/metadata-only watcher, not-synced, GET-miss —
		// is an apiserver-served call under the active pivot.
		objectsGetApiserverFallthrough.Add(1)
		return getFromAPIServer(ctx, ref)
	}

	// Flag off: pivot inactive. Take the apiserver branch unchanged from
	// pre-0.30.96. No counter increment — see the cache.Disabled() note.
	return getFromAPIServer(ctx, ref)
}

func getFromAPIServer(ctx context.Context, ref templatesv1.ObjectReference) (res Result) {
	log := xcontext.Logger(ctx)

	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		log.Error("unable to parse group version", slog.Any("reference", ref), slog.Any("err", err))
		res.Err = response.New(http.StatusBadRequest, err)
		return
	}
	res.GVR = gv.WithResource(ref.Resource)

	ep, err := xcontext.UserConfig(ctx)
	if err != nil {
		log.Error("unable to get user endpoint", slog.Any("err", err))
		res.Err = response.New(http.StatusUnauthorized, err)
		return
	}

	rc, err := kubeconfig.NewClientConfig(ctx, ep)
	if err != nil {
		log.Error("unable to create kubernetes client config", slog.Any("err", err))
		res.Err = response.New(http.StatusInternalServerError, err)
		return
	}

	cli, err := dynamic.NewClient(rc)
	if err != nil {
		log.Error("unable to create kubernetes dynamic client", slog.Any("err", err))
		res.Err = response.New(http.StatusInternalServerError, err)
		return
	}

	uns, err := cli.Get(context.Background(), ref.Name, dynamic.Options{
		Namespace: ref.Namespace,
		GVR:       res.GVR,
	})
	if err != nil {
		log.Error("unable to get resource",
			slog.String("name", ref.Name), slog.String("namespace", ref.Namespace),
			slog.String("gvr", res.GVR.String()), slog.Any("err", err))

		res.Err = response.New(http.StatusInternalServerError, err)
		if apierrors.IsForbidden(err) {
			res.Err = response.New(http.StatusForbidden, err)
		} else if apierrors.IsNotFound(err) {
			res.Err = response.New(http.StatusNotFound, err)
		}

		return
	}

	annotations := uns.GetAnnotations()
	if annotations != nil {
		delete(annotations, lastAppliedConfigAnnotation)
		uns.SetAnnotations(annotations)
	}
	uns.SetManagedFields(nil)

	res.Unstructured = uns
	res.Err = nil
	return
}
