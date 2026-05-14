// deps_extract.go — Tag 0.30.8: widget dep-edge extraction for the L1
// resolved-output cache dep tracker.
//
// Per implementation plan §"Tag 0.30.8 Three-edge dependency recording
// specification":
//
//   Edge type 1 (Widget → resourcesRefs RENDER-ONLY, STATIC declarative).
//   Walks `spec.resourcesRefs.items[]` and emits one Record per item
//   whose `id` is NOT in the action-ref skip set derived from
//   `status.widgetData.actions[*].resourceRefId`.
//
//   Edge type 2 (Widget → apiRef → RestAction, STATIC declarative).
//   Reads `spec.apiRef` and emits one Record on
//   (restactions.templates.krateo.io, apiRef.namespace, apiRef.name).
//
//   Edge type 3 (RestAction → inner K8s call, DYNAMIC) is OUT OF SCOPE
//   at this tag — it would require a *RecordingDeps context threaded
//   through internal/resolvers/restactions/api/resolve.go. Deferred to
//   a future sub-ship.
//
// Action-ref filter (Revision 14, CORRECTNESS-CRITICAL): an item whose
// `id` matches any `resourceRefId` value inside
// `status.widgetData.actions.<actionType>[*].resourceRefId` is an
// action-only ref (e.g., a "View Logs" button target). Tracking those
// would cause spurious L1 invalidations. The filter is conservative:
// when actions cannot be parsed (e.g., the resolver short-circuited
// before populating widgetData), the skip set is empty and every ref
// is treated as render-eligible. That over-records (a small refresher
// cost) rather than under-records (which would risk stale data).
//
// Concurrency: helpers read fields from a *unstructured.Unstructured
// the caller owns; no mutation; safe to call from the dispatcher's
// goroutine.

package dispatchers

import (
	"log/slog"
	"strings"

	"github.com/krateoplatformops/plumbing/maps"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// restActionGVR is the canonical GVR for the RestAction CR (edge type 2).
var restActionGVR = schema.GroupVersionResource{
	Group:    "templates.krateo.io",
	Version:  "v1",
	Resource: "restactions",
}

// recordWidgetDeps walks the resolved widget object and records dep
// edges into the cache.DepTracker. l1Key is the L1 entry's cache key
// the dependent edges are recorded against.
//
// Recorded edges (this tag):
//   * Self-dep: widget CR (gvr, ns, name) → l1Key. Ensures DELETE on
//     the widget itself evicts its own cache entry.
//   * apiRef → restactions(GVR=restactions.templates.krateo.io,
//     ns=apiRef.namespace, name=apiRef.name).
//   * Each render-eligible resourcesRefs item → l1Key, filtered by
//     status.widgetData.actions.<actionType>[*].resourceRefId.
//
// Per feedback_restaction_no_widget_logic.md: this code lives in the
// dispatchers package (HTTP wiring), not the resolver, so widget
// equivalence remains the widget canonicalization layer's job. The
// dispatcher only RECORDS deps; it does not interpret widget output.
//
// Returns nil to allow chaining; counters track failures via the dep
// tracker's Stats.
func recordWidgetDeps(log *slog.Logger, l1Key string, gvr schema.GroupVersionResource, w *unstructured.Unstructured) {
	if l1Key == "" || w == nil {
		return
	}
	deps := cache.Deps()

	// Self-dep: widget CR → its own L1 entry. DELETE on the widget
	// evicts the cache entry.
	deps.Record(l1Key, gvr, w.GetNamespace(), w.GetName())

	// Edge type 2: spec.apiRef → RestAction.
	if apiRefName, apiRefNS, ok := readApiRef(w); ok {
		deps.Record(l1Key, restActionGVR, apiRefNS, apiRefName)
	}

	// Edge type 1: status.resourcesRefs.items[], filtered by
	// status.widgetData.actions.
	skipIDs := extractActionRefIDs(w)
	for _, ref := range extractResourcesRefs(w) {
		if ref.ID != "" && skipIDs[ref.ID] {
			// Action-only ref — skip per Revision 14 filter.
			continue
		}
		refGVR, ok := parseGVR(ref.APIVersion, ref.Resource)
		if !ok {
			continue
		}
		if ref.Name == "" {
			// List-scope dep (e.g., a ref that targets "all of kind X
			// in namespace Y"). Record with name="*".
			deps.RecordList(l1Key, refGVR, ref.Namespace)
			continue
		}
		deps.Record(l1Key, refGVR, ref.Namespace, ref.Name)
	}
}

// recordRestActionSelfDep records the self-dep edge for a RestAction
// dispatch. Kept as a separate exported helper so the restactions.go
// dispatcher doesn't reach into deps directly.
func recordRestActionSelfDep(l1Key string, gvr schema.GroupVersionResource, ns, name string) {
	if l1Key == "" {
		return
	}
	cache.Deps().Record(l1Key, gvr, ns, name)
}

// readApiRef returns (name, namespace, ok) from a widget's spec.apiRef.
func readApiRef(w *unstructured.Unstructured) (string, string, bool) {
	if w == nil {
		return "", "", false
	}
	name, _, _ := unstructured.NestedString(w.Object, "spec", "apiRef", "name")
	ns, _, _ := unstructured.NestedString(w.Object, "spec", "apiRef", "namespace")
	if name == "" {
		return "", "", false
	}
	if ns == "" {
		// apiRef in same namespace as the widget by Krateo convention.
		ns = w.GetNamespace()
	}
	return name, ns, true
}

// resourceRefLite is the minimal subset of ResourceRef we need.
type resourceRefLite struct {
	ID         string
	APIVersion string
	Resource   string
	Namespace  string
	Name       string
}

// extractResourcesRefs reads spec.resourcesRefs.items[] off the widget
// object and returns the lite triples.
func extractResourcesRefs(w *unstructured.Unstructured) []resourceRefLite {
	if w == nil {
		return nil
	}
	arr, ok, err := maps.NestedSlice(w.Object, "spec", "resourcesRefs", "items")
	if !ok || err != nil {
		return nil
	}
	out := make([]resourceRefLite, 0, len(arr))
	for _, raw := range arr {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		r := resourceRefLite{}
		if v, ok := m["id"].(string); ok {
			r.ID = v
		}
		if v, ok := m["apiVersion"].(string); ok {
			r.APIVersion = v
		}
		if v, ok := m["resource"].(string); ok {
			r.Resource = v
		}
		if v, ok := m["namespace"].(string); ok {
			r.Namespace = v
		}
		if v, ok := m["name"].(string); ok {
			r.Name = v
		}
		out = append(out, r)
	}
	return out
}

// extractActionRefIDs walks status.widgetData.actions and returns the
// set of resourceRefId values found across all action entries. Per
// Revision 14, the resolver populates this status during Resolve; if
// status is empty (resolver failed before reaching action wiring), the
// set is empty and every spec.resourcesRefs item is treated as a
// render dep.
//
// The walker tolerates two shapes:
//   1. actions.<actionType> = []{resourceRefId: "..."} (a slice).
//   2. actions.<actionType> = {resourceRefId: "..."}   (a map).
// The Krateo widget contract has historically used both; the prewarm
// helper this lifts from also handled both shapes.
func extractActionRefIDs(w *unstructured.Unstructured) map[string]bool {
	out := map[string]bool{}
	if w == nil {
		return out
	}
	actions, ok, err := maps.NestedMap(w.Object, "status", "widgetData", "actions")
	if !ok || err != nil {
		return out
	}
	for _, raw := range actions {
		switch v := raw.(type) {
		case []any:
			for _, item := range v {
				if m, ok := item.(map[string]any); ok {
					if id, ok := m["resourceRefId"].(string); ok && id != "" {
						out[id] = true
					}
				}
			}
		case map[string]any:
			if id, ok := v["resourceRefId"].(string); ok && id != "" {
				out[id] = true
			}
		}
	}
	return out
}

// parseGVR maps (apiVersion, resource) to a GroupVersionResource.
// Returns ok=false when either input is malformed.
//   - "v1" → group="", version="v1"
//   - "apps/v1" → group="apps", version="v1"
//   - "templates.krateo.io/v1" → group="templates.krateo.io", version="v1"
func parseGVR(apiVersion, resource string) (schema.GroupVersionResource, bool) {
	if apiVersion == "" || resource == "" {
		return schema.GroupVersionResource{}, false
	}
	gvr := schema.GroupVersionResource{Resource: resource}
	parts := strings.SplitN(apiVersion, "/", 2)
	if len(parts) == 1 {
		// Core group ("v1").
		gvr.Version = parts[0]
	} else {
		gvr.Group = parts[0]
		gvr.Version = parts[1]
	}
	if gvr.Version == "" {
		return schema.GroupVersionResource{}, false
	}
	return gvr, true
}
