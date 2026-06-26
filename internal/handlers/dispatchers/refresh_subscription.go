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

	"github.com/krateoplatformops/snowplow/internal/cache"
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
func DeriveSubscriptionKey(ctx context.Context, coords SubscriptionCoordinates) (string, bool) {
	switch coords.Class {
	case cache.CacheEntryClassWidgetContent:
		// Identity-free shared shell. The key is the same for every subject
		// (nobody owns it); the per-user gating is re-applied at serve time
		// (gateWidgetEnvelope). Arming it reveals only "the shared envelope
		// changed" — not subject-specific information (design §5.2 caveat).
		key, handle, _ := dispatchWidgetContentKey(ctx,
			coords.Group, coords.Version, coords.Resource,
			coords.Namespace, coords.Name, coords.PerPage, coords.Page, coords.Extras)
		if handle == nil || key == "" {
			return "", false
		}
		return key, true

	case classRestActions,
		classWidgets,
		cache.CacheEntryClassApistage,
		cache.CacheEntryClassRAFullList:
		// Identity-bound: dispatchCacheLookupKey folds the BindingUID derived
		// from ctx's identity. A foreign coordinate set yields the caller's
		// own BindingUID -> a key the foreign cell never publishes to.
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
