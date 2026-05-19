// streaming_list.go — Ship 0.30.122 R4 Lever 1: a streaming ListWatch
// for the composition GVR's dynamic informer.
//
// THE OOM (0.30.121): with PREWARM_ENABLED=true the dynamic informer's
// initial relist of the 48,999-composition fixture ran at pod startup.
// The standard NewFilteredDynamicInformer ListFunc calls
// dynamic.Interface.List, which inside client-go:
//   (a) io.ReadAll's the whole HTTP response body of a page;
//   (b) unmarshals it into a full []map[string]any (the pre-transform
//       Unstructured set), via a RawMessage intermediate;
//   (c) hands the materialised *UnstructuredList back to the reflector,
//       which only THEN applies the SetTransform strip.
// Every page's full pre-transform objects are alive simultaneously with
// their post-transform copies — the transient duplication that peaked at
// ~17.7 GiB and OOMKilled the pod 4x. Field projection / metadata-only
// is OFF THE TABLE (Diego's hard constraint: full spec+status retained),
// so R4 bounds the *relist transient* instead.
//
// THE FIX (Option A — PM-confirmed): a custom ListFunc that
//   1. issues the paged LIST itself — a `continue`-token walk, identical
//      LIST semantics to client-go's ListPager;
//   2. streams each page's HTTP response body through a json.Decoder —
//      Token() down to the `items` array, then Decode(&obj) ONE element
//      at a time (never io.ReadAll of the whole body, never a full
//      []map[string]any, no RawMessage intermediate);
//   3. applies the strip transform to each item inline as it is decoded;
//   4. drops the reference to the full pre-transform object before
//      decoding the next element — so only already-stripped objects
//      accumulate;
//   5. accumulates only transformed objects into the returned
//      *UnstructuredList.
// The reflector still re-applies SetTransform on the returned items —
// defaultStripUnstructured is idempotent (re-stripping an already-stripped
// object is a no-op), so the store contents are byte-identical to the
// standard path. The WatchFunc is the standard dynamic watch, unchanged.
//
// Gated by RESOLVER_COMPOSITION_STREAMING_LIST (default ON). Flag off, or
// no *rest.Config wired, reverts the composition GVR to the standard
// NewFilteredDynamicInformer (AC-7).

package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamiclister"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/rest"
	clientcache "k8s.io/client-go/tools/cache"
)

// envCompositionStreamingList gates the R4 Lever-1 streaming ListWatch.
// Default ON — the streaming path IS the fix. Set to "false" to revert
// the composition GVR to the standard NewFilteredDynamicInformer (AC-7
// toggle).
const envCompositionStreamingList = "RESOLVER_COMPOSITION_STREAMING_LIST"

// compositionStreamingListEnabled reports whether the R4 streaming
// ListWatch is enabled (default true).
func compositionStreamingListEnabled() bool {
	return boolFromEnv(envCompositionStreamingList, true)
}

// NOTE — the streaming ListWatch reuses the shared listOptionsTweak /
// global listPageLimit (watcher.go); there is NO streaming-only
// page-limit knob, by design. Under the streaming json.Decoder the
// per-page transient is OBJECT-sized (the decoder's token buffer holds
// ~one composition object at a time), NOT page-sized — so a 500-object
// page and a 100-object page produce identical snowplow memory. A
// separate page-limit constant/config would buy zero memory and only
// multiply round-trips, so none is added.

// streamingListGVRs is the declarative set of GVR *groups* whose
// standalone informer is routed through the R4 streaming ListWatch. Same
// shape as resourceOverrides: a write-once init map, lock-free reads.
// Keyed by GROUP (not the full GVR) — the composition group hosts one
// dynamically-named CRD per blueprint, so the resource segment is not
// known at compile time; the group `composition.krateo.io` is the
// stable, declarative discriminator (mirrors matchesAutoDiscoverGroup).
// No per-resource literal — this is an additive routing mechanism, not a
// special case (feedback_no_special_cases.md).
var streamingListGVRs = map[string]struct{}{
	"composition.krateo.io": {},
}

// matchesStreamingListGroup reports whether gvr's group is in the
// streamingListGVRs set.
func matchesStreamingListGroup(gvr schema.GroupVersionResource) bool {
	_, ok := streamingListGVRs[gvr.Group]
	return ok
}

// streamingDynamicInformer is the R4 GenericInformer wrapper — the exact
// shape of client-go's unexported dynamicInformer, but built around a
// ListWatch whose ListFunc streams. Satisfies informers.GenericInformer
// so addResourceTypeLocked can store it in rw.informers transparently.
type streamingDynamicInformer struct {
	informer clientcache.SharedIndexInformer
	gvr      schema.GroupVersionResource
}

func (d *streamingDynamicInformer) Informer() clientcache.SharedIndexInformer {
	return d.informer
}

// Lister returns a GenericLister over this informer's indexer.
//
// Ship H1 — B2 SILENT-DROP GUARD: dynamiclister.New produces a lister
// whose List/Get type-assert every indexer value to
// *unstructured.Unstructured. The composition group is routed through
// BOTH the streaming informer (this type) AND the H1 bytes-override
// (the SetTransform installed in addResourceTypeLocked stores
// *bytesObject for this group). A dynamiclister would therefore
// SILENTLY DROP every bytesObject — an empty/short list, no crash, no
// log: exactly the FINDING 1 silent-drop defect class, but on a path
// the five watcher.go cast sites do not cover.
//
// Verified at H1 ship time: there are ZERO production callers of
// Lister() — every cache read goes through GetObject / ListObjects /
// GetTypedObject / ListTypedObjects (which read GetIndexer() directly
// and decode-on-access). So this panic is unreachable today.
//
// It is retained as a LOUD trap: if a future caller invokes Lister()
// on a bytes-routed GVR, it fails immediately with a diagnostic
// message pointing at the fix — rather than silently returning a
// truncated list that would be misdiagnosed for ships. A future caller
// that genuinely needs a lister here must add a bytesObject-aware
// lister (decode-on-access), the same way the five cast sites were
// converted. Per feedback_no_park_broken_behind_flag: a known
// silent-drop trap is made loud, not left latent.
func (d *streamingDynamicInformer) Lister() clientcache.GenericLister {
	if matchesBytesOverrideGroup(d.gvr) {
		panic("cache: streamingDynamicInformer.Lister() called for bytes-override GVR " +
			d.gvr.String() + " — dynamiclister would silently drop *bytesObject values " +
			"(H1 bytes-backed store). Read via ResourceWatcher.ListObjects / GetObject " +
			"(decode-on-access) or add a bytesObject-aware lister; do NOT use dynamiclister here.")
	}
	return dynamiclister.NewRuntimeObjectShim(dynamiclister.New(d.informer.GetIndexer(), d.gvr))
}

var _ informers.GenericInformer = &streamingDynamicInformer{}

// newStreamingDynamicInformer builds a standalone dynamic informer for
// gvr whose ListFunc streams (R4 Lever 1). Returns (nil, false) when the
// streaming path cannot be constructed (no *rest.Config, REST client
// build failure) so the caller can fall back to the standard informer.
//
// The WatchFunc is byte-identical to NewFilteredDynamicInformer's — only
// the ListFunc is replaced. tweak is the shared listOptionsTweak so the
// streaming LIST carries the SAME paging policy as every other informer
// (no streaming-only page-limit constant — see the NOTE above).
func newStreamingDynamicInformer(
	rc *rest.Config,
	dyn dynamic.Interface,
	gvr schema.GroupVersionResource,
	indexers clientcache.Indexers,
	tweak func(*metav1.ListOptions),
) (informers.GenericInformer, bool) {
	if rc == nil || dyn == nil {
		return nil, false
	}
	restClient, err := streamingRESTClient(rc)
	if err != nil {
		slog.Warn("cache.streaming_list.rest_client_failed",
			slog.String("subsystem", "cache"),
			slog.String("gvr", gvr.String()),
			slog.String("error", err.Error()),
			slog.String("effect", "falling back to the standard dynamic informer for this GVR"),
		)
		return nil, false
	}

	lw := &clientcache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			if tweak != nil {
				tweak(&options)
			}
			return streamingList(context.TODO(), restClient, gvr, options)
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			// Standard dynamic watch — unchanged from
			// NewFilteredDynamicInformer. A WATCH stream is already
			// incremental (one event at a time); only the initial LIST
			// needed the streaming treatment.
			if tweak != nil {
				tweak(&options)
			}
			return dyn.Resource(gvr).Namespace(metav1.NamespaceAll).Watch(context.TODO(), options)
		},
	}

	informer := clientcache.NewSharedIndexInformerWithOptions(
		lw,
		&unstructured.Unstructured{},
		clientcache.SharedIndexInformerOptions{
			ResyncPeriod:      0, // pure event-driven — matches the factory
			Indexers:          indexers,
			ObjectDescription: gvr.String(),
		},
	)
	return &streamingDynamicInformer{informer: informer, gvr: gvr}, true
}

// streamingRESTClient builds a rest.RESTClient for raw apiserver GETs
// from rc. It mirrors how dynamic.NewForConfig configures its client —
// an unversioned REST client with a no-op codec, since streamingList
// reads the body itself rather than letting client-go decode it.
func streamingRESTClient(rc *rest.Config) (*rest.RESTClient, error) {
	cfg := rest.CopyConfig(rc)
	cfg.GroupVersion = &schema.GroupVersion{}
	cfg.APIPath = "/apis"
	cfg.NegotiatedSerializer = serializer.NewCodecFactory(runtime.NewScheme()).WithoutConversion()
	if cfg.UserAgent == "" {
		cfg.UserAgent = rest.DefaultKubernetesUserAgent()
	}
	return rest.RESTClientFor(cfg)
}

// streamingList issues the paged LIST for gvr and streams every page's
// response body through a json.Decoder, decoding + stripping one item at
// a time. The returned *UnstructuredList holds only post-strip objects;
// no full pre-transform copy of the 48,999-object set is ever alive.
//
// Paging: a `continue`-token walk, identical semantics to client-go's
// ListPager — each page carries opts.Limit (listPageLimit) and the
// previous page's metadata.continue token until the apiserver returns an
// empty continue.
func streamingList(
	ctx context.Context,
	rc *rest.RESTClient,
	gvr schema.GroupVersionResource,
	opts metav1.ListOptions,
) (*unstructured.UnstructuredList, error) {
	out := &unstructured.UnstructuredList{}
	pages := 0
	totalItems := 0

	for {
		pageOpts := opts // value copy — only the continue token changes per page
		body, err := streamingListPageBody(ctx, rc, gvr, pageOpts)
		if err != nil {
			return nil, err
		}

		cont, rv, apiVersion, kind, perr := decodeListPageStreaming(body, out, &totalItems)
		// Always close the body before evaluating the parse error so a
		// failed page cannot leak the HTTP connection.
		_ = body.Close()
		if perr != nil {
			return nil, fmt.Errorf("streaming list %s page %d: %w", gvr.String(), pages, perr)
		}

		// BLOCKER 2 (re-review) — never serve an error envelope as a
		// (truncated) list. rest.Request.Stream already returns an error
		// for a non-2xx response, but a 200 response can still carry a
		// metav1 `kind: Status` object (a proxy, a watch-cache edge, a
		// misbehaving apiserver). That envelope has no `items`, so the
		// streaming decoder would silently produce a zero/partial list
		// with continueToken=="" — the loop would break and streamingList
		// would return (out, nil). A Status on page 50 of ~490 → the
		// informer relists to a TRUNCATED composition set and serves it
		// as authoritative (the S4-class "partial looks complete" trap).
		// Fail the WHOLE list instead — never break-and-return-partial.
		if kind == "Status" {
			return nil, fmt.Errorf("streaming list %s page %d: apiserver returned a Status "+
				"envelope, not a List — failing the whole list rather than serving a "+
				"truncated set", gvr.String(), pages)
		}
		pages++

		// Carry envelope identity onto the result list ONCE, from the
		// FIRST page only (the same one-shot pattern for all three).
		if out.GetAPIVersion() == "" && apiVersion != "" {
			out.SetAPIVersion(apiVersion)
		}
		if out.GetKind() == "" && kind != "" {
			out.SetKind(kind)
		}
		// BLOCKER 1 (re-review) — the list's resourceVersion MUST be the
		// FIRST page's, not last-write-wins. For a paged collection LIST
		// the apiserver pins a snapshot RV at the first page (embedded in
		// the continue token); every page reports that same snapshot RV.
		// Last-write-wins is fragile: if any page returns a differing RV
		// (a watch-cache edge, a 410-retry) the list would end with the
		// wrong RV and the informer's subsequent WATCH would start from
		// it → missed/duplicated events, silent cache drift. Capture it
		// once, from page 1, with the same `== ""` guard as apiVersion/kind.
		if out.GetResourceVersion() == "" && rv != "" {
			out.SetResourceVersion(rv)
		}

		if cont == "" {
			break
		}
		opts.Continue = cont
		// Subsequent pages must NOT re-send a resourceVersion — the
		// continue token fully determines the page (apiserver contract).
		opts.ResourceVersion = ""
	}

	slog.Info("cache.streaming_list.completed",
		slog.String("subsystem", "cache"),
		slog.String("gvr", gvr.String()),
		slog.Int("pages", pages),
		slog.Int("items", totalItems),
		slog.Int64("page_limit", opts.Limit),
		slog.String("note", "paged LIST streamed item-by-item — no full pre-transform materialisation"),
	)
	return out, nil
}

// streamingListPageBody issues ONE page's raw HTTP GET and returns the
// response body as a stream. The caller MUST Close it.
func streamingListPageBody(
	ctx context.Context,
	rc *rest.RESTClient,
	gvr schema.GroupVersionResource,
	opts metav1.ListOptions,
) (io.ReadCloser, error) {
	req := rc.Get().
		AbsPath(streamingListAbsPath(gvr)...).
		SpecificallyVersionedParams(&opts, paramCodec, metav1.SchemeGroupVersion)
	return req.Stream(ctx)
}

// streamingListAbsPath builds the apiserver collection path segments for
// a cluster-wide LIST of gvr. Core group ("") uses /api/<version>; a
// named group uses /apis/<group>/<version>. The composition GVR is
// always a named group, but the core branch keeps the helper general.
func streamingListAbsPath(gvr schema.GroupVersionResource) []string {
	if gvr.Group == "" {
		return []string{"api", gvr.Version, gvr.Resource}
	}
	return []string{"apis", gvr.Group, gvr.Version, gvr.Resource}
}

// decodeListPageStreaming consumes one LIST page's response body with a
// streaming json.Decoder. It walks the top-level object, and when it
// reaches the `items` array it Decode()s ONE element at a time into a
// fresh unstructured.Unstructured, applies the strip transform, appends
// the stripped object to out.Items, and drops the per-element reference
// before decoding the next — so the page's full pre-transform set is
// never simultaneously alive.
//
// Returns (continueToken, resourceVersion, apiVersion, kind, err).
func decodeListPageStreaming(
	body io.Reader,
	out *unstructured.UnstructuredList,
	totalItems *int,
) (continueToken, resourceVersion, apiVersion, kind string, err error) {
	dec := json.NewDecoder(body)

	// Expect the opening '{' of the LIST envelope.
	tok, err := dec.Token()
	if err != nil {
		return "", "", "", "", fmt.Errorf("read list envelope open: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return "", "", "", "", fmt.Errorf("list envelope: expected object, got %v", tok)
	}

	for dec.More() {
		// Each top-level key.
		keyTok, kerr := dec.Token()
		if kerr != nil {
			return "", "", "", "", fmt.Errorf("read list envelope key: %w", kerr)
		}
		key, _ := keyTok.(string)

		switch key {
		case "apiVersion":
			apiVersion, err = decodeStringValue(dec)
			if err != nil {
				return "", "", "", "", err
			}
		case "kind":
			kind, err = decodeStringValue(dec)
			if err != nil {
				return "", "", "", "", err
			}
		case "metadata":
			// The list metadata carries continue + resourceVersion.
			var meta struct {
				Continue        string `json:"continue"`
				ResourceVersion string `json:"resourceVersion"`
			}
			if derr := dec.Decode(&meta); derr != nil {
				return "", "", "", "", fmt.Errorf("decode list metadata: %w", derr)
			}
			continueToken = meta.Continue
			resourceVersion = meta.ResourceVersion
		case "items":
			// The streaming heart: walk the items array element by element.
			if derr := decodeItemsArrayStreaming(dec, out, totalItems); derr != nil {
				return "", "", "", "", derr
			}
		default:
			// Unknown top-level key — decode-and-discard into a throwaway
			// so the decoder stays positioned correctly.
			var discard json.RawMessage
			if derr := dec.Decode(&discard); derr != nil {
				return "", "", "", "", fmt.Errorf("skip list key %q: %w", key, derr)
			}
		}
	}

	// Consume the closing '}'.
	if _, cerr := dec.Token(); cerr != nil && cerr != io.EOF {
		return "", "", "", "", fmt.Errorf("read list envelope close: %w", cerr)
	}
	return continueToken, resourceVersion, apiVersion, kind, nil
}

// decodeItemsArrayStreaming consumes the `items` array. The decoder is
// positioned just before the array. It reads the '[', then Decode()s one
// element per iteration into a FRESH unstructured.Unstructured, strips
// it, appends the stripped object, and lets the pre-transform map go out
// of scope before the next Decode — bounding the live set to one item
// plus the accumulated stripped slice.
func decodeItemsArrayStreaming(
	dec *json.Decoder,
	out *unstructured.UnstructuredList,
	totalItems *int,
) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("read items array open: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return fmt.Errorf("items: expected array, got %v", tok)
	}

	for dec.More() {
		// Decode ONE element into a fresh object map. The map is the
		// only pre-transform copy alive; after the strip + append it is
		// dropped on the next loop iteration.
		obj := map[string]any{}
		if derr := dec.Decode(&obj); derr != nil {
			return fmt.Errorf("decode list item %d: %w", *totalItems, derr)
		}
		item := unstructured.Unstructured{Object: obj}
		// R4 step 3 — strip inline. defaultStripUnstructured drops
		// managedFields + the last-applied annotation; spec + status are
		// untouched (Diego's hard constraint). The reflector re-applies
		// SetTransform on the returned list — idempotent on an already-
		// stripped object, so store contents are byte-identical to the
		// standard path.
		_, _ = defaultStripUnstructured(&item)
		out.Items = append(out.Items, item)
		*totalItems++
		// `obj` / `item` go out of scope here — the next Decode allocates
		// a fresh map; only out.Items (stripped objects) accumulates.
	}

	// Consume the closing ']'.
	if _, cerr := dec.Token(); cerr != nil {
		return fmt.Errorf("read items array close: %w", cerr)
	}
	return nil
}

// decodeStringValue reads a single JSON string value the decoder is
// positioned on.
func decodeStringValue(dec *json.Decoder) (string, error) {
	tok, err := dec.Token()
	if err != nil {
		return "", fmt.Errorf("read string value: %w", err)
	}
	s, ok := tok.(string)
	if !ok {
		return "", fmt.Errorf("expected string value, got %T", tok)
	}
	return s, nil
}

// paramCodec encodes metav1.ListOptions into URL query parameters for
// the raw REST request. metav1.ParameterCodec is the standard codec
// client-go uses for the same purpose.
var paramCodec = metav1.ParameterCodec
