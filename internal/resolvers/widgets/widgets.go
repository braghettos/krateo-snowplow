package widgets

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	xcontext "github.com/krateoplatformops/plumbing/context"
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

	// identityContextKey — the A2 author-declared identity-dependence field
	// (definitive-cache-identity-architecture §1.1): spec.identityContext is an
	// OPTIONAL []string enumerating which authenticated-principal keys the
	// widget's rendered output depends on. Absent/empty = identity-free (the
	// default; per-binding-shareable + seedable). Read off the unstructured spec
	// (same absence-tolerant pattern as the extras accessors).
	identityContextKey = "identityContext"

	// keyExtrasKey — the F6 author-declared request-extras-dependence field
	// (docs/f6-chrome-route-key-design-2026-07-12.md §4 Option A-declare):
	// spec.keyExtras is an OPTIONAL []string enumerating which REQUEST-extras
	// keys (the ?extras= JSON the frontend folds route params / query into) this
	// widget's rendered output depends on. Only the declared keys PARTITION the
	// cache key; undeclared request extras still reach the RESOLVE input (the jq
	// dict) but do NOT vary the key. Absent/empty = fold NOTHING from request
	// extras (the identity-free chrome-widget default: one seeded cell serves all
	// routes). This is the exact identityContext shape one dimension over —
	// author declaration of key-dependence — so it is read the same absence-
	// tolerant way and filters at the SINGLE shared effectiveKeyExtras site.
	keyExtrasKey = "keyExtras"
)

// identityContextEnumUsername / identityContextEnumGroups are the ONLY enum
// values the Phase A contract honors — exactly jwtutil.UserInfo (username,
// groups). D1 (Diego 2026-07-07): displayName is FORECLOSED from the enum (it is
// not a JWT claim for any strategy, §1.3), so the accessor FILTERS to these two
// IN CODE — an out-of-enum value (a stale/typo'd/displayName declaration) is
// dropped, never injected. This is the code-side twin of the CRD enum bound: the
// server never injects a key the principal does not carry.
const (
	identityContextEnumUsername = "username"
	identityContextEnumGroups   = "groups"
)

// GetIdentityContext reads spec.identityContext off the unstructured widget CR
// and returns the DECLARED identity keys FILTERED to the Phase A enum
// ({username, groups}) — the A2 declaration accessor
// (definitive-cache-identity-architecture §1.1). Absence-tolerant: absent /
// wrong-type / empty ⇒ nil (identity-free default, byte-identical to pre-A2).
// Out-of-enum entries (e.g. displayName, a typo) are DROPPED in code (D1
// foreclosure) — never a silent inject of a key the principal lacks. Order is
// preserved and duplicates are collapsed so the derived identity map is
// deterministic. Reads the raw slice via maps.NestedSlice (deep copy → no CR
// aliasing), mirroring GetResourcesRefsExtras.
func GetIdentityContext(obj map[string]any) []string {
	raw, ok, err := maps.NestedSlice(obj, "spec", identityContextKey)
	if !ok || err != nil {
		return nil
	}
	var out []string
	seen := map[string]struct{}{}
	for _, v := range raw {
		s, isStr := v.(string)
		if !isStr {
			continue
		}
		// D1 enum filter IN CODE: honor only username/groups.
		if s != identityContextEnumUsername && s != identityContextEnumGroups {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// GetKeyExtras reads spec.keyExtras off the unstructured widget CR and returns
// the DECLARED request-extras key names — the F6 declaration accessor
// (docs/f6-chrome-route-key-design-2026-07-12.md §4 Option A-declare). It is the
// request-extras twin of GetIdentityContext, one dimension over: instead of
// enumerating which authenticated-principal keys the output depends on, it
// enumerates which REQUEST-extras keys (route params / query the frontend folds
// into ?extras=) the widget's resolution depends on. Only these keys are allowed
// to PARTITION the cache key (filterDeclaredKeyExtras applies the allowlist);
// undeclared request extras are still passed to the RESOLVE input untouched.
//
// UNLIKE GetIdentityContext there is NO enum filter: request-extras key names are
// author-defined and open-ended (namespace, name, and any custom route/query
// param), so the accessor honors every declared string. Absence-tolerant: absent
// / wrong-type / empty ⇒ nil (the fold-nothing chrome-widget default,
// byte-identical to pre-F6 for the identity-free corpus). Order is preserved and
// duplicates collapsed for determinism. Reads via maps.NestedSlice (deep copy →
// no CR aliasing), mirroring GetIdentityContext / GetResourcesRefsExtras.
func GetKeyExtras(obj map[string]any) []string {
	raw, ok, err := maps.NestedSlice(obj, "spec", keyExtrasKey)
	if !ok || err != nil {
		return nil
	}
	var out []string
	seen := map[string]struct{}{}
	for _, v := range raw {
		s, isStr := v.(string)
		if !isStr {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// DeclaredIdentity is the SINGLE derivation of a declared widget's
// server-trusted identity map (definitive-cache-identity-architecture §1.3 +
// §2.2). It is the ONE place identity ever meets the resolve input / key fold,
// called from BOTH the key path (dispatchers effectiveKeyExtras via
// declaredIdentityForKey) and the resolve-input path (widgets Resolve) — so the
// key and the body cannot desync (the #64 anti-drift principle at the identity
// dimension).
//
// It reads the widget's declared identityContext (GetIdentityContext, already
// enum-filtered) and materialises ONLY those keys from the authenticated
// principal on ctx (xcontext.UserInfo — the JWT the authn middleware minted).
//
// GATE AUTHN-1 (Diego binding constraint): the ONLY source is
// xcontext.UserInfo(ctx). ZERO user-store reads — no User CR, no IdP call, no
// LDAP query, no apiserver fetch — so it is STRATEGY-AGNOSTIC by construction
// (basic / OIDC / LDAP / serviceaccount all mint the same JWT tuple). displayName
// is not derivable here (not in the JWT); the enum forecloses declaring it.
//
// Returns nil when: no declaration (identity-free widget — the ~99% corpus
// path), or no/invalid UserInfo on ctx (fail-safe: absent identity yields no
// injection, never a spurious key). A nil return makes the caller's fold a
// no-op → byte-identical to pre-A2 for every undeclared widget (the prod-inert
// acceptance). Values: username → ui.Username; groups → a fresh copy of
// ui.Groups (never aliases the ctx slice). Only NON-EMPTY values are injected —
// an empty username is not a key (jq `// empty` semantics apply downstream).
func DeclaredIdentity(ctx context.Context, obj map[string]any) map[string]any {
	declared := GetIdentityContext(obj)
	if len(declared) == 0 {
		return nil // identity-free widget — no injection (prod-inert default)
	}
	ui, err := xcontext.UserInfo(ctx)
	if err != nil {
		return nil // no authenticated principal → inject nothing (fail-safe)
	}
	out := map[string]any{}
	for _, key := range declared {
		switch key {
		case identityContextEnumUsername:
			if ui.Username != "" {
				out[identityContextEnumUsername] = ui.Username
			}
		case identityContextEnumGroups:
			if len(ui.Groups) > 0 {
				// JSON-native []any (NOT []string): this identity map is folded
				// into opts.Extras (resolve.go:64-73) and threaded through
				// resolveApiRef → mergeRequestWins → plumbing/maps.DeepCopyJSON,
				// which PANICS on a Go []string ("cannot deep copy []string" — it
				// only handles the json.Unmarshal-produced []any). A fresh []any
				// keeps the value deep-copy-safe AND non-aliasing
				// (feedback_shared_vs_copy_is_a_concurrency_change), and is
				// key-parity byte-identical: json.Marshal([]string{"devs"}) ==
				// json.Marshal([]any{"devs"}) == ["devs"], so canonicaliseExtras
				// hashes it identically (the A1 prod-inert goldens stay green).
				g := make([]any, len(ui.Groups))
				for i, v := range ui.Groups {
					g[i] = v
				}
				out[identityContextEnumGroups] = g
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

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
