// refresh_subscription.go — Ship 1 (live-refresh-coherence, option A).
//
// The forgery-proof per-subject isolation seam for GET /refreshes (design
// §5). A /refreshes connection asks to be armed for a widget by sending the
// SAME resource coordinates it used for /call (group/version/resource/ns/
// name/page/perPage/extras/class) — NOT an opaque key. The server then
// re-derives the expected L1 key UNDER THE CONNECTION'S authenticated
// identity (the UserInfo placed on ctx by middleware.RefreshAuth) using the
// IDENTICAL key-derivation the /call dispatcher uses:
//
//   - identity-bound classes (restactions/widgets/apistage/raFullList) ->
//     dispatchCacheLookupKey: rbac.EvaluateRBAC(ctx-identity, get, gvr, ns,
//     name) -> BindingUID -> cache.ComputeKey (helpers.go:200-243).
//   - widgetContent (identity-free) -> dispatchWidgetContentKey
//     (helpers.go:147-168, identity-free ComputeKey).
//
// WHY THIS IS FORGERY-PROOF (design §5.2): the BindingUID is computed from
// the CONNECTION'S JWT-derived UserInfo on ctx, never from anything the
// client supplies. A malicious client that sends user-B's coordinates over
// user-A's connection makes the server derive the key under A's identity ->
// A's BindingUID -> a DIFFERENT key than B's cell -> the connection is armed
// for a key that B's refreshes never publish to. A client can only ever arm
// keys that ITS OWN identity legitimately produces. This is exactly the
// dispatchCacheLookupKey FAIL-CLOSED posture (missing/foreign identity ->
// empty-identity key -> no match).
//
// This REUSES the existing key-derivation seam verbatim — no new identity
// logic, no per-resource/path/user special-case (feedback_no_special_cases).
// The functions are exported so internal/handlers/refreshes.go (package
// handlers) can call them; the underlying dispatchCacheLookupKey /
// dispatchWidgetContentKey stay package-private.

package dispatchers

import (
	"context"

	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// classRestActions / classWidgets are the two entry-class values that have
// no named constant in internal/cache (they are bare literals across the
// dispatcher — dispatchers.go:88-89, restactions.go:123, widgets.go:170).
// Stated once here so DeriveSubscriptionKey's accepted-class allowlist is a
// uniform per-class discriminant, not a scattered literal (the three other
// classes use the cache.CacheEntryClass* constants). This is the class
// allowlist, not a per-resource/path/user special-case (feedback_no_special_cases).
const (
	classRestActions = "restactions"
	classWidgets     = "widgets"
)

// SubscriptionCoordinates is the resource tuple a /refreshes connection
// supplies to arm one widget. It is the SAME coordinate set the /call
// dispatcher keys a widget by. Identity is NOT part of this struct — it is
// taken from the connection's ctx, never the wire (the forgery-proof
// property, see file header).
type SubscriptionCoordinates struct {
	// Class is the CacheEntryClass: "widgets" | "widgetContent" |
	// "restactions" | "apistage" | "raFullList". The frontend learns the
	// right value from the X-Snowplow-Refresh-Class response header the /call
	// dispatcher stamps (helpers.go setRefreshKeyHeader) — it echoes that
	// verbatim rather than guessing widgets-vs-widgetContent (frontend guide
	// §2.5). An unknown class fails closed in DeriveSubscriptionKey's default.
	Class     string
	Group     string
	Version   string
	Resource  string
	Namespace string
	Name      string
	PerPage   int
	Page      int
	Extras    map[string]any
}

// DeriveSubscriptionKey re-derives the L1 key for coords UNDER ctx's
// authenticated identity, using the same derivation the /call dispatcher
// uses. Returns (key, true) when a key was derived; (\"\", false) when the
// cache layer is off / identity is missing / the class is unknown — in which
// case the caller MUST reject that subscription entry (fail-closed).
//
// No apiserver read is issued: dispatchCacheLookupKey's only external touch
// is rbac.EvaluateRBAC, which reads the in-process RBAC snapshot (the same
// snapshot /call consults) — never the apiserver. So /refreshes stays off
// the apiserver path end to end.
// subscriptionKeyExtras returns the SAME extras union the EMIT path folds into
// a widget's cache key — the inline author-declared blocks UNIONed with the
// request extras — so the subscription key the server arms is byte-identical to
// the key the resolver Puts + PublishRefreshes (#64).
//
// THE BUG IT FIXES: the emit key folds unionForKey(spec.apiRef.extras,
// spec.resourcesRefsTemplateExtras, requestExtras) (helpers.go:275 +
// widgets.GetApiRefExtras/GetResourcesRefsExtras), but DeriveSubscriptionKey
// historically folded ONLY coords.Extras (the request half). A
// composition-DETAIL widget with inline extras therefore derived a DIFFERENT
// key on the subscription side than the cell publishes → zero delivery. The
// frontend CANNOT supply the inline blocks (they are server-side CR fields it
// never sees), so the union MUST be reconstructed server-side from the widget
// CR — exactly as the emit path does.
//
// MECHANISM REUSE (C64-2, no re-impl): the inline getters
// widgets.GetApiRefExtras / GetResourcesRefsExtras + the in-package unionForKey
// are the EXACT functions the emit path uses, so the fold is identical by
// construction.
//
// FAIL-CLOSED (C64-1): objects.Get failing (NotFound / RBAC-denied / nil) →
// (nil, false). The caller SKIPS the coordinate — it does NOT arm a
// request-only key (which would never match the emit key for an inline-extras
// widget, re-introducing the bug) and does NOT fall back. A connection can
// only arm a key whose widget CR its own identity can GET — the same
// forgery-proof posture as the BindingUID derivation (the objects.Get carries
// the connection ctx's identity + RBAC).
//
// COST (C64-6): objects.Get is informer-served under cache-on (get.go
// useInformer→IsServable) — an in-memory read at SUBSCRIBE time (≤ the armed
// coord count, bounded ≤512), never per-event, never the apiserver.
func subscriptionKeyExtras(ctx context.Context, c SubscriptionCoordinates) (map[string]any, bool) {
	got := objects.Get(ctx, templatesv1.ObjectReference{
		Reference: templatesv1.Reference{
			Name:      c.Name,
			Namespace: c.Namespace,
		},
		APIVersion: schema.GroupVersion{Group: c.Group, Version: c.Version}.String(),
		Resource:   c.Resource,
	})
	if got.Err != nil || got.Unstructured == nil {
		// Fail-closed: skip the coord (no request-only fallback).
		return nil, false
	}
	return unionForKey(
		widgets.GetApiRefExtras(got.Unstructured.Object),
		widgets.GetResourcesRefsExtras(got.Unstructured.Object),
		c.Extras,
	), true
}

func DeriveSubscriptionKey(ctx context.Context, coords SubscriptionCoordinates) (string, bool) {
	// (#64 real root cause — page/perPage divergence) The EMIT /call path runs
	// coords through paginationInfo → normalizePagination, so a non-paginated
	// widget folds "-1","-1" into ComputeKey. The subscription receives
	// coords.PerPage/Page from ?sub= as 0,0 (the frontend sends 0, or omits the
	// fields → json zero). Without normalizing here, the sub key folds "0","0"
	// ≠ the emit "-1","-1" → the armed key never matches the published one →
	// delivered:0, for EVERY class (the fold is class-independent). Apply the
	// SHARED normalization core (the same one paginationInfo calls — extracted,
	// not re-implemented, so the two sides cannot drift) to ALL classes BEFORE
	// the per-class derivation below.
	coords.PerPage, coords.Page = normalizePagination(coords.PerPage, coords.Page)

	switch coords.Class {
	case cache.CacheEntryClassWidgetContent:
		// Identity-free shared shell. The key is the same for every subject
		// (nobody owns it); the per-user gating is re-applied at serve time
		// (gateWidgetEnvelope). Arming it reveals only "the shared envelope
		// changed" — not subject-specific information (design §5.2 caveat).
		//
		// #64: a widgetContent cell is a widget — its emit key folds the inline
		// extras union, so the subscription key MUST too. Reconstruct the union
		// from the widget CR (fail-closed-skip if it can't be GET) and pass it
		// in place of the request-only coords.Extras.
		keyExtras, ok := subscriptionKeyExtras(ctx, coords)
		if !ok {
			return "", false
		}
		key, handle, _ := dispatchWidgetContentKey(ctx,
			coords.Group, coords.Version, coords.Resource,
			coords.Namespace, coords.Name, coords.PerPage, coords.Page, keyExtras)
		if handle == nil || key == "" {
			return "", false
		}
		return key, true

	case classWidgets:
		// Identity-bound widget. #64: same inline-extras union as widgetContent
		// — the emit key (widgets.go genuine-Put) folds unionForKey(inline,
		// request); the subscription key must fold the identical union or the
		// armed key never matches the published one for an inline-extras widget.
		keyExtras, ok := subscriptionKeyExtras(ctx, coords)
		if !ok {
			return "", false
		}
		key, handle, _ := dispatchCacheLookupKey(ctx, coords.Class,
			coords.Group, coords.Version, coords.Resource,
			coords.Namespace, coords.Name, coords.PerPage, coords.Page, keyExtras)
		if handle == nil || key == "" {
			return "", false
		}
		return key, true

	case classRestActions,
		cache.CacheEntryClassApistage,
		cache.CacheEntryClassRAFullList:
		// Identity-bound: dispatchCacheLookupKey folds the BindingUID derived
		// from ctx's identity. A foreign coordinate set yields the caller's
		// own BindingUID -> a key the foreign cell never publishes to.
		//
		// #64: these classes carry NO inline-extras blocks (they are not
		// widgets — a RESTAction/apistage/raFullList cell's emit key folds only
		// the request extras), so raw coords.Extras is request-only parity on
		// BOTH sides. UNCHANGED.
		key, handle, _ := dispatchCacheLookupKey(ctx, coords.Class,
			coords.Group, coords.Version, coords.Resource,
			coords.Namespace, coords.Name, coords.PerPage, coords.Page, coords.Extras)
		if handle == nil || key == "" {
			return "", false
		}
		return key, true

	default:
		// Unknown class — fail closed.
		return "", false
	}
}

// deriveSubscriptionKeyInputsForTest mirrors DeriveSubscriptionKey but returns
// the PRE-HASH ResolvedKeyInputs (the 3rd value DeriveSubscriptionKey discards)
// so the #64 golden can assert pre-hash-STRING equality against the emit-side
// inputs field-by-field — STRONGER than digest equality and independent of the
// (on-cluster-only) admin BindingUID. Test-only; production uses
// DeriveSubscriptionKey. Mirrors the exact same normalization + per-class
// branching so the inputs it returns are what DeriveSubscriptionKey hashes.
func deriveSubscriptionKeyInputsForTest(ctx context.Context, coords SubscriptionCoordinates) (*cache.ResolvedKeyInputs, bool) {
	coords.PerPage, coords.Page = normalizePagination(coords.PerPage, coords.Page)

	switch coords.Class {
	case cache.CacheEntryClassWidgetContent:
		keyExtras, ok := subscriptionKeyExtras(ctx, coords)
		if !ok {
			return nil, false
		}
		_, handle, inputs := dispatchWidgetContentKey(ctx,
			coords.Group, coords.Version, coords.Resource,
			coords.Namespace, coords.Name, coords.PerPage, coords.Page, keyExtras)
		if handle == nil || inputs == nil {
			return nil, false
		}
		return inputs, true

	case classWidgets:
		keyExtras, ok := subscriptionKeyExtras(ctx, coords)
		if !ok {
			return nil, false
		}
		_, handle, inputs := dispatchCacheLookupKey(ctx, coords.Class,
			coords.Group, coords.Version, coords.Resource,
			coords.Namespace, coords.Name, coords.PerPage, coords.Page, keyExtras)
		if handle == nil || inputs == nil {
			return nil, false
		}
		return inputs, true

	case classRestActions,
		cache.CacheEntryClassApistage,
		cache.CacheEntryClassRAFullList:
		_, handle, inputs := dispatchCacheLookupKey(ctx, coords.Class,
			coords.Group, coords.Version, coords.Resource,
			coords.Namespace, coords.Name, coords.PerPage, coords.Page, coords.Extras)
		if handle == nil || inputs == nil {
			return nil, false
		}
		return inputs, true

	default:
		return nil, false
	}
}
