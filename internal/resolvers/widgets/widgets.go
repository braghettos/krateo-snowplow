package widgets

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/krateoplatformops/plumbing/maps"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
)

const (
	widgetDataKey            = "widgetData"
	widgetDataTemplateKey    = "widgetDataTemplate"
	apiRefKey                = "apiRef"
	extrasKey                = "extras"
	resourcesRefsKey         = "resourcesRefs"
	resourcesRefsTemplateKey = "resourcesRefsTemplate"
	// resourcesRefsTemplateExtrasKey is the SIBLING block that carries the
	// author-declarable inline extras scoped to the resourcesRefsTemplate jq
	// (inline-extras design P §3). It is a sibling — NOT a sub-key of
	// resourcesRefsTemplate — so GetResourcesRefsTemplate's bare-slice read
	// (resourcesRefsTemplateKey, below) stays byte-identical and existing
	// blueprints are untouched (the PM-endorsed non-breaking shape).
	resourcesRefsTemplateExtrasKey = "resourcesRefsTemplateExtras"
)

func GetAPIVersion(obj map[string]any) string {
	val, err := maps.NestedString(obj, "apiVersion")
	if err != nil {
		return ""
	}
	return val
}

func GetKind(obj map[string]any) string {
	val, err := maps.NestedString(obj, "kind")
	if err != nil {
		return ""
	}
	return val
}

func GetNamespace(obj map[string]any) string {
	val, err := maps.NestedString(obj, "metadata", "namespace")
	if err != nil {
		return ""
	}
	return val
}

func GetName(obj map[string]any) string {
	val, err := maps.NestedString(obj, "metadata", "name")
	if err != nil {
		return ""
	}
	return val
}

func GetUID(obj map[string]any) string {
	val, err := maps.NestedString(obj, "metadata", "uid")
	if err != nil {
		return ""
	}
	return val
}

func GetWidgetData(obj map[string]any) map[string]any {
	data, ok, err := maps.NestedMap(obj, "spec", widgetDataKey)
	if !ok || err != nil {
		return map[string]any{}
	}
	return data
}

// GetApiRefExtras reads the author-declarable inline extras scoped to the
// apiRef RA fetch (inline-extras design P §3, surface 1: spec.apiRef.extras).
//
// It reads the `extras` SUB-KEY off the raw unstructured spec.apiRef map
// DIRECTLY — it deliberately does NOT route through GetApiRef's
// ObjectReference unmarshal, and there is NO Extras field on
// templatesv1.ObjectReference (core.go:168). That struct is shared by 7
// non-widget consumers (the generic /call object ref, fetchObject, and the
// seed/prewarm/refresher paths) that would silently inherit the field, and
// GetApiRef's json.Unmarshal would absorb spec.apiRef.extras into the typed
// ref — coupling the apiRef-fetch identity to extras the seed-literal sites
// cannot see. Reading the sub-key off the unstructured map keeps
// ObjectReference + GetApiRef untouched (the load-bearing no-pollution
// constraint, design §3).
//
// maps.NestedMap returns a DEEP COPY (maps.go: NestedMap → DeepCopyJSON), so
// the returned map never aliases the shared widget CR — the merge helpers can
// fold it without a shared-vs-copy concurrency hazard
// (feedback_shared_vs_copy_is_a_concurrency_change). Returns {} on
// absent/typed-miss → backward-compat no-op (mirrors GetWidgetData).
func GetApiRefExtras(obj map[string]any) map[string]any {
	data, ok, err := maps.NestedMap(obj, "spec", apiRefKey, extrasKey)
	if !ok || err != nil {
		return map[string]any{}
	}
	return data
}

// GetResourcesRefsExtras reads the author-declarable inline extras scoped to
// the resourcesRefsTemplate jq ONLY (inline-extras design P §3, surface 2).
//
// It reads the SIBLING block spec.resourcesRefsTemplateExtras — the
// PM-endorsed non-breaking shape — so the existing GetResourcesRefsTemplate
// bare-slice read (widgets.go: maps.NestedSlice(obj,"spec","resourcesRefsTemplate"))
// stays byte-identical. (A bare slice has no sibling field to hang a block-
// level extras map on, so reading from a sibling block avoids restructuring
// the shipped slice; the widget CRD schema declaration of this field is a
// portal-chart follow-up — snowplow tolerates its absence by returning {}.)
//
// As with GetApiRefExtras, maps.NestedMap returns a deep copy → no aliasing of
// the shared CR. Returns {} on absent/typed-miss → backward-compat no-op.
func GetResourcesRefsExtras(obj map[string]any) map[string]any {
	data, ok, err := maps.NestedMap(obj, "spec", resourcesRefsTemplateExtrasKey)
	if !ok || err != nil {
		return map[string]any{}
	}
	return data
}

func GetWidgetDataTemplate(obj map[string]any) ([]templatesv1.WidgetDataTemplate, error) {
	data, ok, err := maps.NestedSliceNoCopy(obj, "spec", widgetDataTemplateKey)
	if !ok || err != nil {
		return nil, err
	}

	items, err := maps.ToMapSlice(data)
	if err != nil {
		return nil, err
	}

	return maps.MapSliceToStructSlice[templatesv1.WidgetDataTemplate](items)
}

func GetApiRef(obj map[string]any) (templatesv1.ObjectReference, error) {
	src, ok, err := maps.NestedMapNoCopy(obj, "spec", apiRefKey)
	if !ok || err != nil {
		return templatesv1.ObjectReference{}, err
	}

	dat, err := json.Marshal(src)
	if err != nil {
		return templatesv1.ObjectReference{}, err
	}

	ref := templatesv1.ObjectReference{
		Resource:   "restactions",
		APIVersion: fmt.Sprintf("%s/%s", templatesv1.Group, templatesv1.Version),
	}
	err = json.Unmarshal(dat, &ref)

	return ref, err
}

func GetResourcesRefs(obj map[string]any) ([]templatesv1.ResourceRef, error) {
	arr, ok, err := maps.NestedSlice(obj, "spec", resourcesRefsKey, "items")
	if !ok || err != nil {
		return []templatesv1.ResourceRef{}, err
	}

	mapSlice, err := maps.ToMapSlice(arr)
	if err != nil {
		return []templatesv1.ResourceRef{}, err
	}

	return maps.MapSliceToStructSlice[templatesv1.ResourceRef](mapSlice)
}

func GetResourcesRefsTemplate(obj map[string]any) ([]templatesv1.ResourceRefTemplate, error) {
	arr, ok, err := maps.NestedSlice(obj, "spec", resourcesRefsTemplateKey)
	if !ok || err != nil {
		return []templatesv1.ResourceRefTemplate{}, err
	}

	mapSlice, err := maps.ToMapSlice(arr)
	if err != nil {
		return []templatesv1.ResourceRefTemplate{}, err
	}

	return maps.MapSliceToStructSlice[templatesv1.ResourceRefTemplate](mapSlice)
}

func loggerAttr(obj map[string]any) slog.Attr {
	return slog.Group("widget",
		slog.String("name", GetName(obj)),
		slog.String("namespace", GetNamespace(obj)),
		slog.String("apiVersion", GetAPIVersion(obj)),
		slog.String("kind", GetKind(obj)),
	)
}
