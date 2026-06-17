// extras_cache_key_test.go — load-bearing cache falsifier for the
// extras→widgets parity feature.
//
// The feature makes a widget's resolved output depend on the per-request
// `extras`. That output is cached in the widget L1 layers. If the cache key
// did NOT already fold `extras`, two requests for the same widget with
// DIFFERENT extras would collide on one cell and the second requester would
// be served the first requester's body — a cross-request correctness defect.
//
// The plan's verify-don't-assume gate asserts both widget L1 keys already
// include extras (dispatchWidgetContentKey → ComputeKey, and
// dispatchCacheLookupKey → ComputeKey). These tests PROVE it through the
// widget dispatcher's own key-builder (not just the raw ResolvedKeyInputs)
// and guard against regression. We target the IDENTITY-FREE widgetContent key
// because that is the SHARED cell where the cross-request collision risk
// actually lives (the per-cohort key additionally folds BindingUID; the
// content key omits identity entirely, so extras is the ONLY per-request
// discriminant beyond the gvr/ns/name/pagination tuple).
package dispatchers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// enableWidgetContentL1 turns the whole resolved-output + widgetContent L1
// stack on for one test and resets the singleton around it. Mirrors the
// setup in widget_content_empty_shell_falsifier_test.go.
func enableWidgetContentL1(t *testing.T) {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	t.Setenv("WIDGET_CONTENT_L1_ENABLED", "true")
	cache.ResetResolvedCacheForTest()
	t.Cleanup(cache.ResetResolvedCacheForTest)
}

// parseExtras mirrors internal/handlers/util.ParseExtras — the production
// extras map is json.Unmarshal'd from the ?extras= query param, so values are
// JSON-native types. Building fixtures this way keeps the key inputs
// byte-identical to the real dispatch path.
func parseExtras(t *testing.T, jsonExtras string) map[string]any {
	t.Helper()
	out := map[string]any{}
	if jsonExtras == "" {
		return out
	}
	if err := json.Unmarshal([]byte(jsonExtras), &out); err != nil {
		t.Fatalf("parseExtras: %v", err)
	}
	return out
}

// TestExtras_WidgetContentKey_DiffersByExtras is THE falsifier: the same
// widget tuple (gvr/ns/name/pagination) with DIFFERENT extras MUST produce
// DIFFERENT identity-free content keys — otherwise the two requests collide
// on one shared cell and leak each other's body cross-request.
func TestExtras_WidgetContentKey_DiffersByExtras(t *testing.T) {
	enableWidgetContentL1(t)

	ctx := context.Background()
	const (
		g, v, r, ns, name = "widgets.templates.krateo.io", "v1beta1", "buttons", "demo-system", "btn-1"
		perPage, page     = 10, 1
	)

	keyA, handleA, _ := dispatchWidgetContentKey(ctx, g, v, r, ns, name, perPage, page,
		parseExtras(t, `{"tenant":"acme"}`))
	keyB, handleB, _ := dispatchWidgetContentKey(ctx, g, v, r, ns, name, perPage, page,
		parseExtras(t, `{"tenant":"globex"}`))

	if handleA == nil || handleB == nil {
		t.Fatalf("widgetContent L1 must be enabled (got handles A=%v B=%v)", handleA, handleB)
	}
	if keyA == "" || keyB == "" {
		t.Fatalf("expected non-empty content keys, got A=%q B=%q", keyA, keyB)
	}
	if keyA == keyB {
		t.Fatalf("FALSIFIER FAILED: same widget + DIFFERENT extras produced the SAME content key %q — "+
			"two requests would collide on one identity-free cell and serve each other's body", keyA)
	}
}

// TestExtras_WidgetContentKey_StableForSameExtras — caching requires the key
// to be DETERMINISTIC for identical inputs (incl. extras), and INSENSITIVE to
// extras-map iteration/JSON key order (ComputeKey canonicalises via
// sorted-key JSON). Same extras (order-shuffled) ⇒ same key ⇒ a real hit.
func TestExtras_WidgetContentKey_StableForSameExtras(t *testing.T) {
	enableWidgetContentL1(t)
	ctx := context.Background()
	const (
		g, v, r, ns, name = "widgets.templates.krateo.io", "v1beta1", "buttons", "demo-system", "btn-1"
		perPage, page     = 10, 1
	)

	key1, _, _ := dispatchWidgetContentKey(ctx, g, v, r, ns, name, perPage, page,
		parseExtras(t, `{"tenant":"acme","region":"eu"}`))
	// Same content, different JSON key order — must canonicalise identically.
	key2, _, _ := dispatchWidgetContentKey(ctx, g, v, r, ns, name, perPage, page,
		parseExtras(t, `{"region":"eu","tenant":"acme"}`))

	if key1 != key2 {
		t.Fatalf("same extras (key order shuffled) must produce the SAME content key, got %q vs %q", key1, key2)
	}
}

// TestExtras_WidgetContentKey_EmptyEqualsNil — backward-compat: a request with
// no extras param (nil) and one with the non-nil empty map ParseExtras returns
// for absent ?extras must key IDENTICALLY, and identically to today's
// no-extras key. ComputeKey folds extras only when len>0, so the empty/nil
// fold writes nothing — proving the no-extras path is byte-identical to
// pre-feature.
func TestExtras_WidgetContentKey_EmptyEqualsNil(t *testing.T) {
	enableWidgetContentL1(t)
	ctx := context.Background()
	const (
		g, v, r, ns, name = "widgets.templates.krateo.io", "v1beta1", "buttons", "demo-system", "btn-1"
		perPage, page     = 10, 1
	)

	keyNil, _, _ := dispatchWidgetContentKey(ctx, g, v, r, ns, name, perPage, page, nil)
	keyEmpty, _, _ := dispatchWidgetContentKey(ctx, g, v, r, ns, name, perPage, page, map[string]any{})

	if keyNil != keyEmpty {
		t.Fatalf("nil extras and empty-map extras must key identically (backward-compat), got %q vs %q", keyNil, keyEmpty)
	}
}

// TestExtras_WidgetContentCell_NoCrossCollision is the end-to-end serve proof:
// store a body under extras-A's key, then look up extras-B's key — it MUST
// miss (so request B never serves A's cached body), and a second lookup of
// extras-A's key MUST hit (so caching still works). This exercises the real
// cache handle Put/Get, not just key equality.
func TestExtras_WidgetContentCell_NoCrossCollision(t *testing.T) {
	enableWidgetContentL1(t)
	ctx := context.Background()
	const (
		g, v, r, ns, name = "widgets.templates.krateo.io", "v1beta1", "buttons", "demo-system", "btn-1"
		perPage, page     = 10, 1
	)

	keyA, handle, _ := dispatchWidgetContentKey(ctx, g, v, r, ns, name, perPage, page,
		parseExtras(t, `{"tenant":"acme"}`))
	keyB, _, _ := dispatchWidgetContentKey(ctx, g, v, r, ns, name, perPage, page,
		parseExtras(t, `{"tenant":"globex"}`))
	if handle == nil {
		t.Fatal("expected a live widgetContent cache handle")
	}

	bodyA := []byte(`{"status":{"widgetData":{"tenant":"acme"}}}`)
	handle.Put(keyA, &cache.ResolvedEntry{RawJSON: bodyA})

	// Request B (different extras) must NOT hit A's cell.
	if _, hit := handle.Get(keyB); hit {
		t.Fatalf("FALSIFIER FAILED: extras-B lookup hit a cell stored under extras-A — cross-request L1 collision")
	}
	// Request A (same extras) must hit, and serve exactly A's body.
	got, hit := handle.Get(keyA)
	if !hit {
		t.Fatal("extras-A lookup must hit the cell it stored")
	}
	if string(got.RawJSON) != string(bodyA) {
		t.Fatalf("extras-A cell served the wrong body: got %q want %q", got.RawJSON, bodyA)
	}
}
