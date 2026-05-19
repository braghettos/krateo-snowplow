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
