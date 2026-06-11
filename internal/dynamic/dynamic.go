package dynamic

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// Ship 2 (production-aim cleanup 2026-06-01) — ResourceFor and KindFor
// removed. Their construction sites (one in schema.ValidateObjectStatus,
// one in resourcesrefs.resolveOne) now use cache.GVRFor / cache.KindForGVR
// which serve via the in-process built-in scheme + permanent plurals
// store, eliminating a per-/call cold restmapper + DiscoveryClient
// build.

func GroupVersion(obj map[string]any) schema.GroupVersion {
	av := getNestedString(obj, "apiVersion")

	if (len(av) == 0) || (av == "/") {
		return schema.GroupVersion{}
	}

	switch strings.Count(av, "/") {
	case 0:
		return schema.GroupVersion{Group: "", Version: av}
	case 1:
		i := strings.Index(av, "/")
		return schema.GroupVersion{Group: av[:i], Version: av[i+1:]}
	default:
		return schema.GroupVersion{}
	}
}

func GetAPIVersion(obj map[string]any) string {
	return getNestedString(obj, "apiVersion")
}

func GetKind(obj map[string]any) string {
	return getNestedString(obj, "kind")
}

func GetNamespace(obj map[string]any) string {
	return getNestedString(obj, "metadata", "namespace")
}

func GetName(obj map[string]any) string {
	return getNestedString(obj, "metadata", "name")
}

func GetUID(obj map[string]any) types.UID {
	return types.UID(getNestedString(obj, "metadata", "uid"))
}

func getNestedString(obj map[string]any, fields ...string) string {
	val, found, err := unstructured.NestedString(obj, fields...)
	if !found || err != nil {
		return ""
	}
	return val
}
