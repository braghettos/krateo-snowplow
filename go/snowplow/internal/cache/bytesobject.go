// bytesobject.go — Ship H1 (0.30.x heap rebuild): the GC-lean,
// bytes-backed resident representation for the composition informer
// store.
//
// THE PROBLEM (TRACED, profiled on the live 0.30.130 pod):
// the dynamic-informer store holds 48,999 compositions as
// *unstructured.Unstructured — each one a deep map[string]interface{}
// tree. map[string]interface{} is the worst-case GC shape: every map
// key is a separately-allocated, separately-scanned pointer. The
// 0.30.130 heap carried ~170M live objects and ~8 GiB of resident
// json.objectInterface decode trees; runtime.scanobject was 62% of
// CPU under navigation load.
//
// THE FIX: replace the stored *unstructured.Unstructured with a
// bytesObject — a small struct whose payload is the object's COMPLETE
// JSON in a single []byte. A []byte backing array contains no
// pointers, so the garbage collector scans the 3-word slice header and
// SKIPS the bytes entirely. 48,999 objects collapse from millions of
// scannable map-key pointers to ~48,999 bounded-pointer structs. Every
// field of every resource is retained verbatim in `raw` — this is a
// change of STORAGE SHAPE, not of stored CONTENT (the H1 hard
// constraint: no field removal).
//
// FINDING 2 (the load-bearing implementation risk): the informer
// indexes objects via clientcache.MetaNamespaceKeyFunc and
// MetaNamespaceIndexFunc, both of which call meta.Accessor(obj).
// meta.Accessor requires the FULL metav1.Object interface — not merely
// GetNamespace()/GetName(). bytesObject satisfies it by EMBEDDING a
// metav1.ObjectMeta (populated at construction from the decoded
// object): *metav1.ObjectMeta implements metav1.Object in full, so the
// embedding bytesObject inherits every accessor and meta.Accessor's
// `case metav1.Object` branch matches directly. The indexer therefore
// keys and namespace-indexes a bytesObject exactly as it did the
// Unstructured.
//
// SB-4 (concurrency — the 0.30.128 lesson): bytesObject is IMMUTABLE
// after construction. `raw` is never mutated; Decode() allocates a
// FRESH map[string]any on every call and never memoizes a shared
// decoded tree on the struct. Two concurrent ListObjects callers
// therefore never alias a map -> no `concurrent map iteration and map
// write` crash class.
package cache

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utiljson "k8s.io/apimachinery/pkg/util/json"
)

// bytesObject is the GC-lean resident representation of one cached
// object. It carries the object's complete JSON in `raw` plus an
// embedded metav1.ObjectMeta so the informer indexer's key-func and
// index-func (which call meta.Accessor) see a full metav1.Object.
//
// GC shape: the struct is an embedded ObjectMeta (itself a bounded set
// of fields — strings, maps, slices that are mostly empty post-strip)
// plus a TypeMeta and a single []byte slice header. The `raw` backing
// array is bytes — NOT pointer-scanned. The dominant cost the old
// representation paid (millions of map-key pointers across 48,999
// deep maps) is gone; what remains scannable is the ObjectMeta's own
// (small, bounded) pointer set per object.
//
// Immutability contract (SB-4): once newBytesObject returns, no field
// of a bytesObject is ever mutated. Decode() reads `raw` and builds a
// brand-new tree; it does not write back. Callers therefore share
// bytesObject pointers freely across goroutines.
type bytesObject struct {
	// TypeMeta carries apiVersion/kind. Embedded so bytesObject also
	// satisfies the parts of runtime.Object that read the GVK, and so
	// json round-trips include it.
	metav1.TypeMeta

	// ObjectMeta is embedded (not just referenced) so bytesObject
	// satisfies the FULL metav1.Object interface required by
	// meta.Accessor — FINDING 2. Populated at construction from the
	// decoded object's metadata; treated as read-only thereafter.
	metav1.ObjectMeta

	// raw is the COMPLETE object JSON — every spec/status/metadata
	// field retained (H1 hard constraint: no field removal). Never
	// mutated after construction. Bytes are not pointer-scanned by the
	// GC: this slice is the whole point of the rebuild.
	raw []byte
}

// Compile-time assertions: bytesObject must satisfy both interfaces the
// informer machinery exercises.
//
//   - metav1.Object  — required by meta.Accessor, hence by
//     MetaNamespaceKeyFunc / MetaNamespaceIndexFunc (FINDING 2).
//   - runtime.Object — required by the indexer-read cast sites
//     watcher.go:1705 / :1758 (GetTypedObject / ListTypedObjects),
//     and the general contract that informer store values are
//     runtime.Objects.
//
// A pointer receiver is used because metav1.ObjectMeta's accessor set
// is defined on *ObjectMeta.
var (
	_ metav1.Object  = (*bytesObject)(nil)
	_ runtime.Object = (*bytesObject)(nil)
)

// newBytesObject builds a bytesObject from an already-decoded (and
// already-stripped, per the existing defaultStripUnstructured policy)
// *unstructured.Unstructured. It re-marshals the object to JSON for
// `raw` and copies the indexer-relevant metadata into the embedded
// ObjectMeta.
//
// H1 deliberately accepts this re-marshal (the "ingestion double
// decode"): the reflector already decoded the LIST body into the
// Unstructured map; H1 marshals it back to bytes. Removing that second
// pass is H2's job (a custom bytes-ListWatch), explicitly out of H1
// scope.
//
// Returns (nil, error) when the object cannot be marshalled — the
// caller (the transform) then falls through to storing the plain
// Unstructured, so a malformed object never stalls the informer.
func newBytesObject(uns *unstructured.Unstructured) (*bytesObject, error) {
	raw, err := json.Marshal(uns.Object)
	if err != nil {
		return nil, err
	}

	gvk := uns.GroupVersionKind()

	bo := &bytesObject{
		TypeMeta: metav1.TypeMeta{
			Kind:       gvk.Kind,
			APIVersion: gvk.GroupVersion().String(),
		},
		raw: raw,
	}

	// Copy the metadata the indexer + event handlers read. unstructured
	// already exposes typed accessors; we mirror them into the embedded
	// ObjectMeta so meta.Accessor sees a complete metav1.Object.
	//
	// Namespace + Name are load-bearing for MetaNamespaceKeyFunc;
	// the rest are copied for fidelity so any caller reading metadata
	// off the stored object (rather than the decoded `raw`) sees the
	// same values. The authoritative full object is always `raw` —
	// Decode() is the field-fidelity path.
	bo.SetName(uns.GetName())
	bo.SetNamespace(uns.GetNamespace())
	bo.SetResourceVersion(uns.GetResourceVersion())
	bo.SetUID(uns.GetUID())
	bo.SetGeneration(uns.GetGeneration())
	bo.SetLabels(uns.GetLabels())
	bo.SetAnnotations(uns.GetAnnotations())
	bo.SetCreationTimestamp(uns.GetCreationTimestamp())
	if dt := uns.GetDeletionTimestamp(); dt != nil {
		bo.SetDeletionTimestamp(dt)
	}
	bo.SetOwnerReferences(uns.GetOwnerReferences())
	bo.SetFinalizers(uns.GetFinalizers())

	return bo, nil
}

// newBytesObjectFromRaw builds a bytesObject DIRECTLY from an item's
// raw JSON bytes — Ship H2a, the LIST-decode fix.
//
// Unlike newBytesObject (which takes an already-built *Unstructured map
// tree and re-marshals it — H1's ingestion path, 845 MB of
// encoding/json.Marshal), this constructor NEVER builds the full
// map[string]interface{} tree for spec/status. The streaming LIST
// decoder hands it the item's raw JSON frame as a json.RawMessage;
// newBytesObjectFromRaw decodes ONLY the `metadata` sub-object (and the
// top-level apiVersion/kind scalars) to populate the embedded
// ObjectMeta/TypeMeta — the indexer-relevant fields. The spec/status
// body is carried verbatim inside `raw` and is never decoded at
// ingestion. This eliminates the UnstructuredList.UnmarshalJSON 5.28
// GiB driver AND H1's added ingestion marshal.
//
// CONTRACT — `raw` MUST already be stripped (SB-1 / AC-H2a.2): the
// caller (decodeItemsArrayStreaming) strips managedFields +
// last-applied-config at the JSON-bytes level BEFORE calling this, so
// `raw` matches the H1-stored shape exactly (stripped of those two
// keys, everything else retained). newBytesObjectFromRaw does not strip
// — it takes `raw` as the authoritative, already-policy-applied frame.
// `raw` is retained by reference and never mutated (SB-4) — the caller
// MUST pass a slice the streaming decoder will not reuse.
//
// NUMERIC FIDELITY: the metadata sub-decode uses
// k8s.io/apimachinery/pkg/util/json (integral numbers -> int64),
// matching the dynamic informer's UnstructuredJSONScheme convention —
// the same discipline Decode() applies (see its doc). metadata fields
// like `generation` are integers; a plain encoding/json would make
// them float64 and break the embedded ObjectMeta's typed accessors.
//
// Returns (nil, error) when `raw` is not a decodable JSON object — the
// caller falls through (logs + skips the item) so a single malformed
// item never stalls the streaming LIST.
func newBytesObjectFromRaw(raw []byte) (*bytesObject, error) {
	// Decode ONLY the indexer-relevant envelope: apiVersion, kind, and
	// the metadata sub-object. spec/status are deliberately captured by
	// json.RawMessage and NEVER decoded into a map tree — that is the
	// whole point of H2a.
	var envelope struct {
		APIVersion string          `json:"apiVersion"`
		Kind       string          `json:"kind"`
		Metadata   json.RawMessage `json:"metadata"`
	}
	if err := utiljson.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}

	bo := &bytesObject{
		TypeMeta: metav1.TypeMeta{
			Kind:       envelope.Kind,
			APIVersion: envelope.APIVersion,
		},
		raw: raw,
	}

	// Decode the metadata sub-object directly into the embedded
	// ObjectMeta. metav1.ObjectMeta has json tags for every field, so a
	// single util/json.Unmarshal populates name/namespace/
	// resourceVersion/uid/generation/labels/annotations/
	// creationTimestamp/ownerReferences/finalizers/deletionTimestamp in
	// one pass — no per-field SetX calls. This is the only map-shaped
	// decode H2a does per item, and it is bounded to metadata (small),
	// NOT the full spec/status tree.
	//
	// An item with no metadata (malformed) leaves ObjectMeta zero —
	// MetaNamespaceKeyFunc would then key it as "" and the indexer Add
	// would surface a KeyError upstream rather than silently dropping
	// it; that is the correct loud failure for a malformed item.
	if len(envelope.Metadata) > 0 {
		if err := utiljson.Unmarshal(envelope.Metadata, &bo.ObjectMeta); err != nil {
			return nil, err
		}
	}

	return bo, nil
}

// stripItemJSON applies the defaultStripUnstructured policy to a LIST
// item's raw JSON bytes — Ship H2a, AC-H2a.2.
//
// It removes exactly the two fields the existing strip policy removes:
//
//   - metadata.managedFields
//   - metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"]
//
// CRITICAL — it does this WITHOUT building the spec/status map tree.
// The item is decoded into an ordered-key-agnostic shape where ONLY
// `metadata` is a map (metadata is small — decoding it is the same
// bounded cost newBytesObjectFromRaw already pays) and EVERY OTHER
// top-level key (spec, status, and any others) is a json.RawMessage
// carried verbatim. The two strip keys are deleted from the metadata
// map; the item is re-marshalled. spec/status are spliced back as raw
// bytes — never decoded into a map[string]interface{}.
//
// The result `raw` matches the H1-stored shape exactly: stripped of the
// two policy fields, every other field (spec, status, all metadata
// except the two) retained verbatim.
//
// If the input has no managedFields and no last-applied annotation the
// re-marshal still runs (cheap — metadata is small) and produces an
// equivalent object; correctness does not depend on detecting the
// no-op case.
//
// Returns (nil, error) on undecodable input — the caller skips the item.
func stripItemJSON(itemRaw []byte) ([]byte, error) {
	// Decode the item: metadata as a map (small, bounded), every other
	// key as raw bytes carried verbatim. util/json keeps integral
	// numbers as int64 for the metadata map (numeric fidelity).
	var fields map[string]json.RawMessage
	if err := utiljson.Unmarshal(itemRaw, &fields); err != nil {
		return nil, err
	}

	metaRaw, hasMeta := fields["metadata"]
	if !hasMeta || len(metaRaw) == 0 {
		// No metadata sub-object — nothing to strip. Return the input
		// unchanged (a defensive copy so the caller's decoder buffer
		// is not aliased).
		out := make([]byte, len(itemRaw))
		copy(out, itemRaw)
		return out, nil
	}

	var metaMap map[string]json.RawMessage
	if err := utiljson.Unmarshal(metaRaw, &metaMap); err != nil {
		return nil, err
	}

	changed := false

	// Drop metadata.managedFields.
	if _, ok := metaMap["managedFields"]; ok {
		delete(metaMap, "managedFields")
		changed = true
	}

	// Drop the last-applied annotation from metadata.annotations.
	if annoRaw, ok := metaMap["annotations"]; ok && len(annoRaw) > 0 {
		var annos map[string]json.RawMessage
		if err := utiljson.Unmarshal(annoRaw, &annos); err != nil {
			return nil, err
		}
		if _, present := annos[lastAppliedAnnotation]; present {
			delete(annos, lastAppliedAnnotation)
			changed = true
			if len(annos) == 0 {
				// Mirror defaultStripUnstructured: an emptied
				// annotations map is dropped entirely (SetAnnotations(nil)).
				delete(metaMap, "annotations")
			} else {
				reAnno, err := json.Marshal(annos)
				if err != nil {
					return nil, err
				}
				metaMap["annotations"] = reAnno
			}
		}
	}

	if !changed {
		// Nothing stripped — return a defensive copy of the original
		// (avoids aliasing the streaming decoder's buffer; SB-4).
		out := make([]byte, len(itemRaw))
		copy(out, itemRaw)
		return out, nil
	}

	// Re-marshal metadata, splice it back into the item. spec/status
	// (and any other top-level keys) are still json.RawMessage —
	// re-marshalling fields just re-emits those bytes verbatim; the
	// spec/status map tree is NEVER built.
	reMeta, err := json.Marshal(metaMap)
	if err != nil {
		return nil, err
	}
	fields["metadata"] = reMeta

	return json.Marshal(fields)
}

// Decode reconstructs a fresh *unstructured.Unstructured from `raw`.
//
// SB-4 — concurrency: a brand-new map[string]any is allocated on EVERY
// call. The returned tree is private to the caller; bytesObject never
// memoizes it. Two concurrent ListObjects callers each get their own
// independent tree, so neither the 0.30.128 shared-map crash class nor
// any aliasing across goroutines is possible.
//
// Decode is the field-fidelity path (AC-H1.1): `raw` is the complete
// object JSON, so the decoded Unstructured is deep-equal to what the
// dynamic informer would have stored pre-rebuild, modulo only the
// pre-existing defaultStripUnstructured policy applied before
// construction.
//
// NUMERIC FIDELITY — load-bearing: the decode uses
// k8s.io/apimachinery/pkg/util/json, NOT encoding/json. The dynamic
// informer's reflector decodes objects through apimachinery's
// UnstructuredJSONScheme, which converts integral JSON numbers to
// int64 (encoding/json would yield float64 for every number). Using
// util/json here makes the decoded tree deep-equal to what the plain
// dynamic informer would have stored — a plain encoding/json.Unmarshal
// would silently change every int field to a float64 and break
// reflect.DeepEqual against a control informer (AC-H1.1) as well as
// any downstream jq expression that depends on integer-vs-float typing
// (feedback_cache_must_not_constrain_jq.md).
//
// Returns (nil, error) on a malformed `raw`; in practice `raw` was
// produced by json.Marshal in newBytesObject so this cannot fail for a
// well-formed bytesObject, but callers handle the error defensively.
func (b *bytesObject) Decode() (*unstructured.Unstructured, error) {
	m := make(map[string]any)
	if err := utiljson.Unmarshal(b.raw, &m); err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: m}, nil
}

// decodeBytesObject is the decode-on-access helper used by the five
// indexer-read cast sites in watcher.go. It accepts ANY informer store
// value:
//
//   - *bytesObject              -> Decode() to a fresh Unstructured.
//   - *unstructured.Unstructured -> returned as-is (the CACHE_ENABLED=
//     false / non-bytes-routed path, and the pre-rebuild shape).
//
// Returning (nil, false) only for a genuinely unusable value means a
// bytesObject is NEVER silently dropped at a cast site — that silent-
// drop defect (FINDING 1 / AC-H1.2) is exactly what this helper
// closes. The boolean is the caller's existing `ok` guard.
func decodeBytesObject(obj interface{}) (*unstructured.Unstructured, bool) {
	switch v := obj.(type) {
	case *bytesObject:
		uns, err := v.Decode()
		if err != nil || uns == nil {
			return nil, false
		}
		return uns, true
	case *unstructured.Unstructured:
		return v, true
	default:
		return nil, false
	}
}

// asRuntimeObject is the decode-on-access helper for the two
// runtime.Object cast sites (GetTypedObject :1705, ListTypedObjects
// :1758). A *bytesObject is decoded to an *unstructured.Unstructured
// (which IS a runtime.Object); anything that is already a
// runtime.Object (the typed-RBAC pointers, a plain Unstructured) is
// returned unchanged.
//
// In production the composition GVR — the only GVR routed through the
// bytes representation by bytesResourceOverrides — is never read via
// GetTypedObject/ListTypedObjects (those serve the four RBAC GVRs,
// which use the typed-RBAC override, not the bytes override). This
// helper exists so that IF a bytesObject ever reaches those sites it
// still yields a usable object rather than being silently dropped —
// AC-H1.2 demands all five sites be decode-safe.
func asRuntimeObject(obj interface{}) (runtime.Object, bool) {
	if bo, ok := obj.(*bytesObject); ok {
		uns, err := bo.Decode()
		if err != nil || uns == nil {
			return nil, false
		}
		return uns, true
	}
	if robj, ok := obj.(runtime.Object); ok {
		return robj, true
	}
	return nil, false
}

// --- runtime.Object implementation -----------------------------------
//
// metav1.TypeMeta already provides GetObjectKind() (it returns itself
// as a schema.ObjectKind). bytesObject only needs DeepCopyObject().

// DeepCopyObject satisfies runtime.Object. bytesObject is immutable, so
// a "copy" need not clone `raw` for safety — but runtime.Object's
// contract is a genuinely independent object, so we copy the slice
// header's backing array too. Cheap relative to the map-tree deep copy
// the old representation paid.
func (b *bytesObject) DeepCopyObject() runtime.Object {
	if b == nil {
		return nil
	}
	rawCopy := make([]byte, len(b.raw))
	copy(rawCopy, b.raw)
	return &bytesObject{
		TypeMeta:   b.TypeMeta,
		ObjectMeta: *b.ObjectMeta.DeepCopy(),
		raw:        rawCopy,
	}
}

// GroupVersionKind returns the object's GVK, decoded once at
// construction into the embedded TypeMeta.
func (b *bytesObject) GroupVersionKind() schema.GroupVersionKind {
	return b.TypeMeta.GroupVersionKind()
}

// --- bytesObjectList — the LIST return type for the H2a streaming path ---
//
// The streaming ListFunc must return a runtime.Object that client-go's
// reflector can treat as a list: meta.ListAccessor reads its
// resourceVersion, meta.ExtractListWithAlloc reads its Items. H1's
// bytesObject cannot live in *unstructured.UnstructuredList (whose
// Items is []unstructured.Unstructured — a concrete struct type), so
// H2a introduces bytesObjectList: a list whose Items is
// []runtime.Object, each element a *bytesObject.
//
// reflector compatibility:
//   - meta.ListAccessor: bytesObjectList embeds metav1.ListMeta, which
//     provides GetListMeta() -> metav1.ListInterface — so it satisfies
//     the metav1.ListMetaAccessor branch of meta.ListAccessor.
//   - meta.ExtractListWithAlloc: it reflects over the `Items` field; a
//     []runtime.Object slice whose element type implements
//     runtime.Object is handled by the `implementsObject` branch — each
//     *bytesObject IS a runtime.Object (asserted above).
//   - runtime.Object: GetObjectKind() comes from the embedded TypeMeta;
//     DeepCopyObject() is implemented below.
//
// After ExtractListWithAlloc the reflector calls store.Replace, which
// re-applies the SetTransform (StripBulkyFieldsForResourceType) to each
// item. A *bytesObject fails that transform's
// `obj.(*unstructured.Unstructured)` cast and is returned UNCHANGED — a
// clean idempotent passthrough (AC-H2a.4): no re-marshal, no drop.
type bytesObjectList struct {
	metav1.TypeMeta
	metav1.ListMeta

	// Items holds *bytesObject values typed as runtime.Object so the
	// reflector's reflection-based ExtractList accepts the slice.
	Items []runtime.Object
}

var _ runtime.Object = (*bytesObjectList)(nil)

// GetObjectKind is provided by the embedded TypeMeta; declared here
// only for documentation symmetry with bytesObject. (No explicit method
// needed — TypeMeta.GetObjectKind is promoted.)

// DeepCopyObject satisfies runtime.Object for the list. The reflector
// does not deep-copy the list itself on the hot path, but the contract
// requires the method. Items are shallow-copied (each *bytesObject is
// immutable — SB-4 — so sharing the element pointers is safe; only the
// slice header is fresh).
func (l *bytesObjectList) DeepCopyObject() runtime.Object {
	if l == nil {
		return nil
	}
	cp := &bytesObjectList{
		TypeMeta: l.TypeMeta,
		ListMeta: *l.ListMeta.DeepCopy(),
	}
	if l.Items != nil {
		cp.Items = make([]runtime.Object, len(l.Items))
		copy(cp.Items, l.Items)
	}
	return cp
}
