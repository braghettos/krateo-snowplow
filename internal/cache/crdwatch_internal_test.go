// crdwatch_internal_test.go — 0.30.102 Tag B internal tests for the
// CRD-watch's unexported GVR-derivation logic. package cache so it can
// reach compositionGVRFromCRDObject + toUnstructuredMap.

package cache

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientcache "k8s.io/client-go/tools/cache"
)

// crdUnstructured builds an unstructured CustomResourceDefinition with
// the given group/plural and a single served+storage version.
func crdUnstructured(group, plural string, versions []map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": plural + "." + group},
		"spec": map[string]any{
			"group":    group,
			"names":    map[string]any{"plural": plural},
			"versions": toAnySlice(versions),
		},
	}}
}

func toAnySlice(in []map[string]any) []any {
	out := make([]any, len(in))
	for i := range in {
		out[i] = in[i]
	}
	return out
}

func TestCompositionGVRFromCRDObject_StorageVersionPreferred(t *testing.T) {
	crd := crdUnstructured("composition.krateo.io", "githubscaffoldings", []map[string]any{
		{"name": "v1alpha1", "served": true, "storage": false},
		{"name": "v1", "served": true, "storage": true},
	})
	gvr, ok := compositionGVRFromCRDObject(crd)
	if !ok {
		t.Fatalf("compositionGVRFromCRDObject must derive a GVR from a valid CRD")
	}
	want := schema.GroupVersionResource{
		Group: "composition.krateo.io", Version: "v1", Resource: "githubscaffoldings",
	}
	if gvr != want {
		t.Fatalf("gvr = %v, want %v (storage version must win)", gvr, want)
	}
}

func TestCompositionGVRFromCRDObject_FirstServedWhenNoStorage(t *testing.T) {
	crd := crdUnstructured("composition.krateo.io", "things", []map[string]any{
		{"name": "v1beta1", "served": true, "storage": false},
	})
	gvr, ok := compositionGVRFromCRDObject(crd)
	if !ok {
		t.Fatalf("expected a GVR from a served-only CRD")
	}
	if gvr.Version != "v1beta1" {
		t.Fatalf("version = %q, want v1beta1 (first served)", gvr.Version)
	}
}

func TestCompositionGVRFromCRDObject_RejectsNoServedVersion(t *testing.T) {
	crd := crdUnstructured("composition.krateo.io", "things", []map[string]any{
		{"name": "v1", "served": false, "storage": true},
	})
	if _, ok := compositionGVRFromCRDObject(crd); ok {
		t.Fatalf("a CRD with no SERVED version must not yield a GVR")
	}
}

func TestCompositionGVRFromCRDObject_RejectsNonCRD(t *testing.T) {
	notCRD := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "x"},
	}}
	if _, ok := compositionGVRFromCRDObject(notCRD); ok {
		t.Fatalf("a non-CRD object must not yield a GVR")
	}
}

func TestCompositionGVRFromCRDObject_UnwrapsTombstone(t *testing.T) {
	crd := crdUnstructured("composition.krateo.io", "things", []map[string]any{
		{"name": "v1", "served": true, "storage": true},
	})
	tomb := clientcache.DeletedFinalStateUnknown{Key: "things.composition.krateo.io", Obj: crd}
	gvr, ok := compositionGVRFromCRDObject(tomb)
	if !ok {
		t.Fatalf("compositionGVRFromCRDObject must unwrap a DeletedFinalStateUnknown tombstone")
	}
	if gvr.Resource != "things" {
		t.Fatalf("resource = %q, want things", gvr.Resource)
	}
}

func TestToUnstructuredMap(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{"k": "v"}}
	m, ok := toUnstructuredMap(u)
	if !ok || m["k"] != "v" {
		t.Fatalf("toUnstructuredMap(*Unstructured) failed: ok=%v m=%v", ok, m)
	}
	raw := map[string]any{"a": 1}
	m2, ok2 := toUnstructuredMap(raw)
	if !ok2 || m2["a"] != 1 {
		t.Fatalf("toUnstructuredMap(map) failed: ok=%v m=%v", ok2, m2)
	}
	if _, ok3 := toUnstructuredMap("not-an-object"); ok3 {
		t.Fatalf("toUnstructuredMap must reject a non-object")
	}
}
