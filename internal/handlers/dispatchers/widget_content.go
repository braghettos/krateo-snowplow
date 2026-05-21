// widget_content.go — Ship G (0.30.16x): identity-free widget content L1
// layer. This is "F1 one tier up": the F1 apistage class caches per-K8s-
// call envelopes identity-free; Ship G caches the resolved WIDGET envelope
// identity-free, one tier higher.
//
// TWO sites in this file:
//
//   1. populateWidgetContentL1 — the F2 walker's free side-effect of
//      widgets.Resolve. After the walker resolves a navigation widget
//      under the SA identity (withPhase1SAContext, phase1_walk.go:514),
//      this helper Put()s the encoded envelope under the identity-free
//      key (gvr, ns, name, perPage, page, extras=nil). The SA identity
//      sets every status.resourcesRefs.items[].allowed flag to true
//      (the snowplow SA holds */* get/list/watch); the stored body
//      carries those SA-evaluated flags un-stripped. They are NEVER
//      served verbatim — the gate runs on every Get-hit.
//
//   2. gateWidgetEnvelope — the serve-time per-user RBAC gate. On a
//      content-Get-hit (widgets.go), this helper unmarshals the cached
//      envelope, walks status.resourcesRefs.items[], OVERWRITES each
//      `allowed` flag via rbac.UserCan under the REQUEST identity
//      (xcontext.UserInfo), and re-encodes via the SAME helper a cold
//      resolve uses (encodeResolvedJSON — SetIndent("", "  ")). The
//      served body is byte-identical to a cold per-user resolve up to
//      `status.traceId` (which the cache content does NOT carry — the
//      walker never sets it; widgets.go injects traceId only on the
//      cold-resolve path at :128-132).
//
// AC-G.3 cross-user share: admin and cyberjoker hit the SAME L1 key for
// the same (gvr, ns, name, perPage, page). One Put on first request;
// the second request Gets the shell + runs the gate independently. The
// cache content is shared; the body that leaves the pod is per-user.
//
// AC-G.4 byte-equivalence: the gate is a re-run of the SAME function
// (rbac.UserCan -> EvaluateRBAC) over the SAME typed-RBAC snapshot the
// cold-resolve uses at resourcesrefs/resolve.go:88-92. By construction
// the gated body == a cold per-user resolve, modulo `status.traceId`
// (cache content has none; the dispatcher writes the gated body
// directly so no per-request traceId is injected on the hit path
// either — see widgets.go).
//
// Per feedback_l1_per_user_keyed_never_cohort: this layer appears to
// violate the per-user-keyed invariant on first read, but it does NOT
// because the gate runs on EVERY Get-hit. The cache content is the
// SHELL (with SA-evaluated `allowed=true` flags); the body that leaves
// the pod is per-user-narrowed. Same architectural property F1 used
// for the apistage class — the feedback file's invariant prohibits
// serving cached content VERBATIM, not the existence of an identity-
// free shell layer behind a per-user gate.

package dispatchers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/maps"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/handlers/util"
	"github.com/krateoplatformops/snowplow/internal/rbac"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// widgetContentL1Key returns the identity-free widget content L1 key for
// (gvr, ns, name, perPage, page) and the canonical inputs that hashed
// into it. Returns ("", nil) when the layer or the cache subsystem are
// disabled — callers MUST treat key=="" as "skip the content layer".
//
// Shared by populateWidgetContentL1 (post-Resolve Put) and the F2 walker
// at phase1_walk.go (pre-Resolve L1KeyContext install). Both call sites
// MUST hash to the same cell so the inner-call dep edges the resolver
// records via L1KeyFromContext attach to the SAME L1 entry the walker
// subsequently Put()s.
func widgetContentL1Key(gvr schema.GroupVersionResource, namespace, name string, perPage, page int) (string, *cache.ResolvedKeyInputs) {
	if cache.ResolvedCache() == nil {
		return "", nil
	}
	if !cache.WidgetContentL1Enabled() {
		return "", nil
	}
	inputs := cache.ResolvedKeyInputs{
		CacheEntryClass: cache.CacheEntryClassWidgetContent,
		Group:           gvr.Group,
		Version:         gvr.Version,
		Resource:        gvr.Resource,
		Namespace:       namespace,
		Name:            name,
		// Username/Groups intentionally zero — identity-free key.
		PerPage: perPage,
		Page:    page,
		// Extras nil at prewarm — the walker does not receive
		// user-supplied extras; serve-time requests with non-nil
		// extras hash to a different cell and miss the prewarmed
		// entry. Acceptable: extras-bearing requests are the rare
		// per-request shape, the bulk navigation never carries them.
	}
	return cache.ComputeKey(inputs), &inputs
}

// populateWidgetContentL1 is the F2 walker's free side-effect of
// widgets.Resolve — Ship G (0.30.16x) §2.3. After the walker resolves a
// navigation widget under the SA identity, this helper Puts the encoded
// envelope under the identity-free key AND records dep edges into the
// DepTracker so an informer event on any K8s object the widget transitively
// depends on dirty-marks the entry and the refresher re-resolves it.
//
// Args:
//   - ctx — the SA-credentialed walker context (withPhase1SAContext).
//     The Put itself is identity-independent; the ctx is unused here
//     beyond a defensive nil-cache guard.
//   - gvr — the widget CR's GVR. Threaded from the caller — either
//     got.GVR at the recursive call site or schema.ParseGroupVersion of
//     the resolved ObjectReference at the root site.
//   - in — the widget CR (the resolver's input). namespace/name read
//     from its metadata via in.GetNamespace/GetName.
//   - perPage, page — the pagination the walker resolved THIS widget
//     under. Part of the content key so prewarm and serve-time agree
//     on the same key when perPage matches.
//   - res — the resolved widget envelope (resolver output). Encoded
//     verbatim — no strip of `allowed` flags; the gate overwrites
//     them per-request.
//
// Dep edges recorded (mirrors widgets.go:195 per-user path exactly):
//   - Self-dep: (gvr, ns, name) -> key. DELETE on the widget CR evicts
//     the content entry via DepTracker.RemoveL1.
//   - apiRef -> RestAction (when spec.apiRef present).
//   - Each render-eligible resourcesRefs item -> key (action-only refs
//     filtered out per Revision 14).
//   - Inner-call deps (edge type 3, recorded inside the resolver via
//     L1KeyFromContext) are wired by the walker installing
//     cache.WithL1KeyContext(ctx, key) BEFORE widgets.Resolve — see
//     phase1_walk.go around the widgets.Resolve call.
//
// No return value: a Put failure (cache nil, encode failure, etc.) is
// logged at debug. The walker's primary purpose (informer discovery)
// continues unaffected.
//
// Concurrency: ResolvedCache().Put is mutex-guarded; concurrent walker
// invocations on different widgets are safe. Same-widget concurrent
// Puts (a no-op in production — the walker is single-threaded per root)
// would replace-in-place via the LRU's index lookup.
func populateWidgetContentL1(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	in *unstructured.Unstructured,
	perPage, page int,
	res *unstructured.Unstructured,
) {
	if in == nil || res == nil {
		return
	}
	key, inputs := widgetContentL1Key(gvr, in.GetNamespace(), in.GetName(), perPage, page)
	if key == "" {
		return
	}
	c := cache.ResolvedCache()
	if c == nil {
		return
	}
	log := xcontext.Logger(ctx)
	if log == nil {
		log = slog.Default()
	}
	encoded, err := encodeResolvedJSON(res)
	if err != nil {
		// A failed encode at prewarm is non-fatal — the serve-time
		// resolve still works. Log at debug; the walker continues.
		log.Debug("widget_content.populate.encode_failed",
			slog.String("subsystem", "cache"),
			slog.String("gvr", gvr.String()),
			slog.String("ns", in.GetNamespace()),
			slog.String("name", in.GetName()),
			slog.Any("err", err),
		)
		return
	}
	c.Put(key, &cache.ResolvedEntry{
		RawJSON: encoded,
		Inputs:  inputs,
	})

	// Record dep edges so K8s informer events dirty-mark this entry and
	// the refresher re-resolves it. Without this, the entry is TTL-only
	// stale-forever — the AC-G.5 defect the architect's diff-review caught.
	// Mirrors the per-user widgets.go:195 path exactly: same arguments,
	// same call site (after Put). Use `res` (the resolved envelope) — it
	// carries spec.apiRef and spec/status.resourcesRefs.items[] that
	// recordWidgetDeps reads (the walker's `in` and `res` are the same CR
	// shape; recordWidgetDeps is robust to either, but we mirror widgets.go).
	recordWidgetDeps(log, key, gvr, res)
}

// gateWidgetEnvelope applies the serve-time per-user RBAC gate to a raw
// widget envelope retrieved from the identity-free content layer — the
// Ship G analogue of F1's gateContentEnvelope. It walks the embedded
// status.resourcesRefs.items[] slice and OVERWRITES each `allowed` flag
// via rbac.UserCan under the request identity.
//
// Returns (gatedEnvelope, served):
//   - served==false — fail-closed: no identity on ctx, or a malformed
//     stored envelope. The caller falls through to the existing per-
//     user widget L1 lookup, which ALSO nil-checks UserInfo at
//     dispatchCacheLookupKey and bails on the same condition.
//   - served==true  — gatedEnvelope is the RBAC-narrowed bytes ready
//     to write to the response wire. encodeResolvedJSON uses the SAME
//     encoder settings the cold-resolve path uses (SetIndent("", "  "))
//     so the body is byte-identical to a cold-resolve response for the
//     same request identity, modulo status.traceId (the cache content
//     has none; the dispatcher emits the gated body directly).
//
// AC-G.4 binding: per-item re-derivation is byte-equivalent to a fresh
// resolveOne loop because both call sites invoke rbac.UserCan with the
// SAME (Verb, GroupResource, Namespace) signature over the SAME typed-
// RBAC snapshot (resourcesrefs/resolve.go:88-92 — see verbs.go for the
// REST→kube verb mapping).
//
// Sub-microsecond per item (typed-RBAC snapshot lookup, no apiserver
// round-trip). N items per widget ≈ tens; gate budget per hit ≈ <50µs
// CPU, dominated by the json.Unmarshal of the cached body.
func gateWidgetEnvelope(
	ctx context.Context,
	raw []byte,
) ([]byte, bool) {
	if _, err := xcontext.UserInfo(ctx); err != nil {
		// FAIL-CLOSED: no identity to gate against. The caller falls
		// through to the existing per-user L1, which dispatch also
		// nil-checks UserInfo and bails — same contract.
		return nil, false
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false
	}
	// Walk status.resourcesRefs.items[] (a []any of map[string]any per
	// the marshalled templatesv1.ResourceRefResult shape). For each
	// item, re-derive `allowed` under THIS user's identity. The other
	// fields (id, path, verb, payload) are identity-invariant by §2.2
	// of the design doc — preserved untouched.
	if items, ok, err := maps.NestedSlice(obj, "status", "resourcesRefs", "items"); ok && err == nil {
		for i, raw := range items {
			it, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			it["allowed"] = recomputeAllowedFromRefItem(ctx, it)
			items[i] = it
		}
		// SetNestedField replaces the slice in place — the items slice
		// is the same value we just mutated. The Set is defensive
		// against any internal copy semantics the maps package may
		// introduce in the future.
		_ = maps.SetNestedField(obj, items, "status", "resourcesRefs", "items")
	}
	encoded, err := encodeResolvedJSON(obj)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

// recomputeAllowedFromRefItem replays resourcesrefs/resolve.go:88-92 for
// ONE item under the request identity. The item carries:
//   - "path" — the /call?... URL (built via resourcesrefs/resolve.go's
//     buildPath; carries resource/apiVersion/namespace/name).
//   - "verb" — the REST verb (GET/POST/PUT/PATCH/DELETE per the
//     kubeToREST map in resourcesrefs/verbs.go).
//
// Mapping back: the GVR is reconstructed from the item's path via
// util.ParseCallPathToObjectRef (callpath.go:36) + inline
// schema.ParseGroupVersion + GroupVersion.WithResource (the same
// pattern util.ParseGVR at gvr.go:23 uses for HTTP requests). The
// REST verb is mapped back to its kube equivalent (POST→create,
// GET→get, etc. — see verbs.go's restToKube). The (verb, gvr.GroupResource,
// namespace) tuple is then handed to rbac.UserCan — the EXACT same
// signature resourcesrefs/resolveOne uses at resolve.go:88-92.
//
// Returns false (fail-closed) on any parse failure or missing
// path/verb — defensive against a malformed cached item. The caller
// in gateWidgetEnvelope writes the result back to `allowed`, so a
// degraded item shows allowed=false rather than the SA-evaluated
// allowed=true the cache shell may carry.
func recomputeAllowedFromRefItem(ctx context.Context, item map[string]any) bool {
	path, _ := item["path"].(string)
	verb, _ := item["verb"].(string)
	if path == "" || verb == "" {
		return false
	}
	ref, ok := util.ParseCallPathToObjectRef(path)
	if !ok {
		// Not a /call endpoint — external URL or malformed. The cold-
		// resolve path would not have set allowed=true for this item
		// either (the path would not parse via resourcesrefs's
		// buildPath shape), so false is the correct fail-closed verdict.
		return false
	}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return false
	}
	gvr := gv.WithResource(ref.Resource)
	kubeVerb, ok := restVerbToKube(verb)
	if !ok {
		return false
	}
	return rbac.UserCan(ctx, rbac.UserCanOptions{
		Verb:          kubeVerb,
		GroupResource: gvr.GroupResource(),
		Namespace:     ref.Namespace,
	})
}

// restVerbToKube maps an HTTP method back to its kube verb — the inverse
// of resourcesrefs/verbs.go's kubeToREST map. Kept here (not lifted into
// internal/handlers/util) because it is the gate's sole consumer and is
// trivially small; lifting would invert the verbs.go authoring direction
// for no reuse gain.
//
// The map mirrors resourcesrefs/verbs.go's restToKube exactly. Returns
// ok=false for an unknown verb so the gate fails closed rather than
// guessing.
func restVerbToKube(restVerb string) (string, bool) {
	switch strings.ToUpper(restVerb) {
	case http.MethodGet:
		return "get", true
	case http.MethodPost:
		return "create", true
	case http.MethodPut:
		return "update", true
	case http.MethodPatch:
		return "patch", true
	case http.MethodDelete:
		return "delete", true
	default:
		return "", false
	}
}
