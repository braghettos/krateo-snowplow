package objects

import (
	"context"
	"log/slog"
	"net/http"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/http/response"
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
	// R5 (0.30.110): resolver-side dep recording. When this Get runs
	// under a cache.WithL1KeyContext ctx, the fetched object becomes a
	// dependency of the L1 entry being populated — record the edge so a
	// later DELETE / ADD / UPDATE of the object invalidates that entry.
	//
	// Gated hard on !cache.Disabled(): with the cache subsystem off
	// there is no DepTracker store to invalidate and no L1 entry to key
	// against; recording would be dead weight. The deferred call reads
	// the FINAL res (after whichever path produced the object) so it
	// covers both the informer-served and apiserver-served branches.
	if !cache.Disabled() {
		defer recordGetDep(ctx, &res)
	}

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
	// from the in-process informer cache. #57: implicit-on-cache —
	// useInformer() now folds to !cache.Disabled() (the standalone
	// RESOLVER_USE_INFORMER flag was retired). With cache off the
	// cache.Disabled() short-circuit above already returned, so this
	// block runs only on the cache-on path; the binary is byte-identical
	// to the apiserver path under cache-off (R-FALSE-1 invariant
	// preserved via the single CACHE_ENABLED master gate).
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
				// 0.30.97: gate the served path on IsServable
				// (registered AND HasSynced) — the uniform servability
				// predicate also used by the resolver pivot. A
				// not-yet-fully-synced widget GVR must never serve a
				// stale/partial object: its indexer partition can still
				// be draining even after HasSynced has flipped at the
				// start of the processor drain, and a pre-sync miss is
				// indistinguishable from a real NotFound. Anything
				// non-servable falls through to the apiserver.
				if !rw.IsServable(gvr) {
					// Not registered / not yet synced. Fire best-effort
					// lazy registration so a SUBSEQUENT call can serve;
					// EnsureResourceType is idempotent + singleflighted
					// under rw.mu. This call still falls through to the
					// apiserver — pre-sync reads would look identical to
					// a real NotFound.
					_, _ = rw.EnsureResourceType(gvr)
					log.Debug("objects.Get: informer not servable; apiserver fallthrough",
						slog.String("gvr", gvr.String()),
						slog.String("ns", ref.Namespace),
						slog.String("name", ref.Name))
				} else if obj, hit := rw.GetObject(gvr, ref.Namespace, ref.Name); hit && filterGetByRBAC(ctx, gvr, obj) {
					// Cache hit AND the context's user is RBAC-authorized
					// to `get` this object.
					//
					// Tag 0.30.101: filterGetByRBAC is the GET-verb RBAC
					// check — the GET-path sibling of Tag A (0.30.100,
					// the resolver pivot's LIST filter). The informer
					// branch bypasses the per-user `<username>-
					// clientconfig` bearer token that getFromAPIServer
					// reads, so without this check a narrow-RBAC user
					// GETting a known object name in a namespace they
					// have no `get` grant for would receive it.
					// FAIL-CLOSED: a denied GET, a missing identity, or
					// an evaluator error all return false → this branch
					// is skipped → the call falls through to
					// getFromAPIServer, which issues the GET under the
					// user's own token (the apiserver's authoritative
					// 403). See filterGetByRBAC (informer_serve.go).
					//
					// DeepCopy so the strip never mutates the shared
					// informer-store object, then apply the EXACT same
					// field strips getFromAPIServer performs (see
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
				// Fall through to the apiserver for either:
				//   - GET-miss (informer synced, object absent) — the
				//     caller sees the apiserver NotFound envelope shape;
				//   - GET-hit but RBAC-denied / no-identity / evaluator
				//     error (Tag 0.30.101 filterGetByRBAC) — the
				//     apiserver issues the GET under the user's own
				//     token and returns the authoritative 403.
			}
		}
		// Any fall-through inside the routed branch — parse failure,
		// nil/passthrough/metadata-only watcher, not-synced, GET-miss,
		// RBAC-denied GET — is an apiserver-served call under the active
		// pivot.
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

	// 0.30.103: ClientConfigFor returns the context-injected
	// internal-dispatch *rest.Config when an internal/startup driver
	// (Phase 1's SA-credentialed walk) is in flight, else delegates to
	// the unchanged kubeconfig.NewClientConfig per-user path. This is
	// what makes Phase 1's SA fetch work — the SA's raw-PEM CA + bearer
	// token cannot survive kubeconfig.NewClientConfig (see
	// cache.WithInternalRESTConfig).
	rc, err := cache.ClientConfigFor(ctx, ep)
	if err != nil {
		log.Error("unable to create kubernetes client config", slog.Any("err", err))
		res.Err = response.New(http.StatusInternalServerError, err)
		return
	}

	// Ship D (0.30.141) — F-6: objects.getFromAPIServer is the routed
	// pivot's apiserver fall-through arm. Record BEFORE the upstream
	// client build so a panicking construction still increments the
	// counter (AC-D.3 ordering).
	cache.RecordApiserverFallthrough(ctx, cache.ReasonClientBuild, res.GVR.String())
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

// recordGetDep registers an exact-object dependency edge for a
// successful objects.Get, keyed by the L1 key carried in ctx.
//
// R5 (0.30.110): invoked deferred from Get, only when the cache
// subsystem is enabled (the caller gates on !cache.Disabled()). It is a
// no-op when:
//
//   - ctx carries no L1 key (cache.L1KeyFromContext == "") — the Get is
//     not populating a cacheable L1 entry; recording would be a stray
//     edge. This is the AC-R5 negative case.
//   - the Get did not produce an object (res.Err set or res.Unstructured
//     nil) — there is nothing to depend on.
//
// The recorded tuple is (res.GVR, namespace, name): a DELETE of that
// object self-evicts the L1 entry; an ADD/UPDATE dirty-marks it.
func recordGetDep(ctx context.Context, res *Result) {
	l1Key := cache.L1KeyFromContext(ctx)
	if l1Key == "" {
		return
	}
	if res == nil || res.Err != nil || res.Unstructured == nil {
		return
	}
	cache.Deps().Record(l1Key, res.GVR, res.Unstructured.GetNamespace(), res.Unstructured.GetName())
}
