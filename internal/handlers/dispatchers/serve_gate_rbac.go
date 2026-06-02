// serve_gate_rbac.go — Ship 0.30.240 v4 serve gate RBAC helpers.
//
// Isolated from serve_gate.go so the rbac package import lives in one
// place (keeps the import graph audit clean). The single helper
// itemPermittedByRBAC is the per-item RBAC check used by
// stripDictItemsByRBAC (serve_gate.go) when narrowing the cached
// apiRef RA dict's items slice per-user before re-running the JQ
// template.

package dispatchers

import (
	"context"

	"github.com/krateoplatformops/snowplow/internal/rbac"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// itemPermittedByRBAC reports whether the request's identity is
// permitted to `get` the resource carried in `item` (a K8s object's
// metadata map). Returns false (deny) on any parse failure —
// fail-closed posture per the v4 serve gate contract.
//
// Reads the GVR from the item's apiVersion + kind fields. The GR
// resolution is approximate (uses lowercased plural-of-kind as the
// resource); the rbac.UserCan check is on (Group, Resource), so
// matching is structural — wildcard ClusterRoles permit-all
// regardless, narrow Roles permit-specific. The conservative
// posture: if apiVersion or kind is missing, deny.
func itemPermittedByRBAC(ctx context.Context, item map[string]any) bool {
	apiVersion, _ := item["apiVersion"].(string)
	kind, _ := item["kind"].(string)
	if apiVersion == "" || kind == "" {
		// Item shape doesn't carry the GVR — fail-closed.
		// Production RA outputs the K8s object envelope which
		// always carries apiVersion + kind; an empty here means
		// the cached dict's items aren't K8s LIST envelopes (a
		// non-RBAC-sensitive data shape).
		return false
	}
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return false
	}
	resource := kindToResourcePlural(kind)
	gvr := gv.WithResource(resource)

	metadata, _ := item["metadata"].(map[string]any)
	namespace, _ := metadata["namespace"].(string)

	return rbac.UserCan(ctx, rbac.UserCanOptions{
		Verb:          "get",
		GroupResource: gvr.GroupResource(),
		Namespace:     namespace,
	})
}

// kindToResourcePlural — minimal Kind → Resource plural mapping for
// the RBAC check. The mapping is structural (lowercase + add 's')
// for the vast majority of K8s kinds (Pod → pods, Composition →
// compositions, ConfigMap → configmaps, etc.). The cluster's RBAC
// rules match on resource STRINGS, so this needs to align with how
// Roles/ClusterRoles spell the resource — which is always lowercase
// plural by K8s convention.
//
// Edge cases (Endpoints, Status — already plural; or irregular like
// NetworkPolicy → networkpolicies) are NOT handled here. For the v4
// minimum, the cluster's compositions/widgets/panels/restactions
// follow the regular pattern. If a future RA outputs a Kind with
// irregular pluralisation, this needs RESTMapper integration — but
// that adds a dep cycle.
//
// FAIL-CLOSED edge case: an unknown kind that lowercases+pluralises
// to a name NOT in the cluster's RBAC rules returns deny — same
// posture as the explicit fail-closed paths above.
func kindToResourcePlural(kind string) string {
	if kind == "" {
		return ""
	}
	// Lowercase. Append 's' if not already plural-ending. Kept
	// minimal — production kinds in the apiRef RA universe are
	// regular.
	lower := lowercase(kind)
	if endsWith(lower, "s") {
		return lower
	}
	return lower + "s"
}

// lowercase — ASCII-only lowercase. K8s Kinds are PascalCase ASCII.
func lowercase(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}

// endsWith — strings.HasSuffix without the strings import.
func endsWith(s, suffix string) bool {
	if len(s) < len(suffix) {
		return false
	}
	return s[len(s)-len(suffix):] == suffix
}
