package dynamic

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// NOTE Ship 2 / 0.30.226 — ResourceFor and KindFor were deleted as
// part of the plurals-resolver hot-path swap. Their per-/call cold
// restmapper construction was the dominant CPU + GC alloc source on
// cyberjoker traffic (bgMarkWorker 34.49% pre-ship). Callers now use
// cache.GVRFor / cache.KindForGVR (permanent sync.Map store seeded
// by the boot walker, zero-alloc fast path for built-in scheme +
// CRD-backed kinds in the corpus). See:
//   - internal/cache/plurals_resolver.go GVRFor + KindForGVR
//   - internal/resolvers/widgets/resourcesrefs/resolve.go (caller A)
//   - internal/resolvers/crds/schema/schema.go (caller B)

func GroupVersion(obj map[string]any) schema.GroupVersion {
	av := getNestedString(obj, "apiVersion")

	if (len(av) == 0) || (av == "/") {
		return schema.GroupVersion{}
	}

	switch strings.Count(av, "/") {
	case 0:
		return schema.GroupVersion{"", av}
	case 1:
		i := strings.Index(av, "/")
		return schema.GroupVersion{av[:i], av[i+1:]}
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
