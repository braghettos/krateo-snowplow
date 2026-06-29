// refresh_subscription_pagination_test.go — #64 REAL root cause: the
// subscription key must fold the SAME (perPage, page) the EMIT key folds, so a
// non-paginated widget (emit default -1,-1) matches a subscription that sends
// 0,0 (json zero / the frontend's ?sub= omits the fields).
//
// THE BUG (shipped 1.5.5–1.5.11): the EMIT /call runs page/perPage through
// paginationInfo → normalizePagination → -1,-1 for a non-paginated widget;
// DeriveSubscriptionKey folded the raw coords (0,0). "-1" != "0" → key mismatch
// → published + subscribers>=1 + delivered:0, for EVERY class. The fix extracts
// normalizePagination and applies it to the subscription coords too.
//
// DISCRIMINATOR DISCIPLINE (what masked it 3×): the EMIT key here is computed
// from the REAL paginationInfo default — a *http.Request with NO page/perPage
// query — NOT a hand-passed -1,-1 tuple. The SUB coords have PerPage/Page = 0,0
// (the json-decoded ?sub= zero), NOT hand-set to -1. So the test fails iff the
// two sides normalize differently, which is exactly the production gap.
package dispatchers

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// emitKeyRealDefault computes the EMIT cache key the /call dispatcher would
// stamp for a non-paginated widget: it runs the REAL paginationInfo over a
// request with no page/perPage query (→ -1,-1, exactly as widgets.go does at
// the genuine-Put), then folds that into dispatchCacheLookupKey. No tuple is
// hand-passed — the -1,-1 comes from the production normalization.
func emitKeyRealDefault(t *testing.T, ctx context.Context, coords SubscriptionCoordinates) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/call?resource=panels&apiVersion=widgets.templates.krateo.io/v1beta1", nil)
	perPage, page := paginationInfo(slog.Default(), req) // NO page/perPage query → the real -1,-1 default
	key, handle, _ := dispatchCacheLookupKey(ctx, coords.Class,
		coords.Group, coords.Version, coords.Resource, coords.Namespace, coords.Name,
		perPage, page, coords.Extras)
	if handle == nil || key == "" {
		t.Fatalf("emit key derivation failed (handle=%v key=%q)", handle, key)
	}
	return key
}

// PAGINATION ARM 1 — the real-default discriminator. Non-paginated widget:
// emit folds -1,-1 (real paginationInfo default); sub coords omit page/perPage
// (0,0). DeriveSubscriptionKey must normalize 0,0 → -1,-1 and match the emit
// key byte-for-byte. RED today (0,0 vs -1,-1), GREEN after the fix.
func TestFalsifier64Pagination_NonPaginatedRealDefault(t *testing.T) {
	buildWidgetParityWatcher(t, true, nil) // panel CR, no inline extras
	ctx := ctxUserA()

	// SUB coords: page/perPage OMITTED → Go zero values 0,0 (what ?sub= sends).
	coords := panelCoords(classWidgets, nil)
	if coords.PerPage != 0 || coords.Page != 0 {
		t.Fatalf("setup: sub coords must be the json-zero 0,0 (got perPage=%d page=%d)", coords.PerPage, coords.Page)
	}

	subKey, ok := DeriveSubscriptionKey(ctx, coords)
	if !ok || subKey == "" {
		t.Fatalf("DeriveSubscriptionKey failed (ok=%v key=%q)", ok, subKey)
	}

	emitKey := emitKeyRealDefault(t, ctx, coords)

	// Sanity: the emit default really is the non-paginated sentinel, not 0,0 —
	// else the test can't discriminate. (A request with page/perPage=0,0 query
	// would also normalize to -1,-1, but here we send NO query at all.)
	emit0Key, _, _ := dispatchCacheLookupKey(ctx, classWidgets,
		coords.Group, coords.Version, coords.Resource, coords.Namespace, coords.Name,
		0, 0, nil)
	if emitKey == emit0Key {
		t.Fatalf("pagination setup: emit -1,-1 default equals the raw 0,0 key — ComputeKey is not "+
			"distinguishing the pagination fold, so this test can't discriminate the bug")
	}

	if subKey != emitKey {
		t.Fatalf("PAGINATION ARM1 RED: sub key %q != emit key %q. The subscription coords (0,0) must be "+
			"normalized through the SAME normalizePagination the emit path uses (→ -1,-1) — else a "+
			"non-paginated widget's armed key never matches its published cell (the 1.5.5–1.5.11 delivered:0).",
			subKey, emitKey)
	}
}

// PAGINATION ARM 1b — PRE-HASH STRING EQUALITY (the team-lead hard gate,
// stronger than digest equality + independent of the on-cluster admin
// BindingUID). Capture the EMIT-side ResolvedKeyInputs (from the real
// paginationInfo default) and the SUB-side ResolvedKeyInputs (from the
// json-zero coords) and assert EVERY hashed field is byte-identical — class,
// GVR, ns, name, BindingUID, PerPage, Page, Stage, and canonicalised Extras.
// This is the empirical proof the prior 4 cycles lacked: the two ComputeKey
// INPUTS are equal field-for-field, so the digests must be equal regardless of
// the (matched-both-sides) BindingUID.
func TestFalsifier64Pagination_PreHashInputEquality(t *testing.T) {
	buildWidgetParityWatcher(t, true, nil)
	ctx := ctxUserA()
	coords := panelCoords(classWidgets, nil) // json-zero 0,0 sub

	subIn, ok := deriveSubscriptionKeyInputsForTest(ctx, coords)
	if !ok || subIn == nil {
		t.Fatalf("sub inputs derivation failed (ok=%v)", ok)
	}

	// EMIT inputs via the REAL paginationInfo default (no page/perPage query).
	req := httptest.NewRequest("GET", "/call?resource=panels&apiVersion=widgets.templates.krateo.io/v1beta1", nil)
	pp, pg := paginationInfo(slog.Default(), req)
	_, _, emitIn := dispatchCacheLookupKey(ctx, classWidgets,
		coords.Group, coords.Version, coords.Resource, coords.Namespace, coords.Name,
		pp, pg, coords.Extras)
	if emitIn == nil {
		t.Fatalf("emit inputs nil")
	}

	// Field-by-field pre-hash equality (every field ComputeKey folds).
	if subIn.CacheEntryClass != emitIn.CacheEntryClass {
		t.Fatalf("class: sub %q != emit %q", subIn.CacheEntryClass, emitIn.CacheEntryClass)
	}
	if subIn.Group != emitIn.Group || subIn.Version != emitIn.Version || subIn.Resource != emitIn.Resource {
		t.Fatalf("GVR: sub %s/%s/%s != emit %s/%s/%s", subIn.Group, subIn.Version, subIn.Resource, emitIn.Group, emitIn.Version, emitIn.Resource)
	}
	if subIn.Namespace != emitIn.Namespace || subIn.Name != emitIn.Name {
		t.Fatalf("ns/name: sub %s/%s != emit %s/%s", subIn.Namespace, subIn.Name, emitIn.Namespace, emitIn.Name)
	}
	if subIn.BindingUID != emitIn.BindingUID {
		t.Fatalf("BindingUID: sub %q != emit %q", subIn.BindingUID, emitIn.BindingUID)
	}
	if subIn.PerPage != emitIn.PerPage || subIn.Page != emitIn.Page {
		t.Fatalf("PRE-HASH PAGINATION DIVERGENCE: sub perPage/page %d/%d != emit %d/%d — the #64 bug. The "+
			"subscription coords must normalize through the SAME normalizePagination the emit path uses.",
			subIn.PerPage, subIn.Page, emitIn.PerPage, emitIn.Page)
	}
	if subIn.Stage != emitIn.Stage {
		t.Fatalf("Stage: sub %q != emit %q", subIn.Stage, emitIn.Stage)
	}
	// And the digests must therefore be equal (the absolute on-cluster digest
	// vs the frontend header needs the real admin BindingUID; here both sides
	// derive the SAME test BindingUID, so digest-equality follows from
	// field-equality and is the hermetic proof).
	if cache.ComputeKey(*subIn) != cache.ComputeKey(*emitIn) {
		t.Fatalf("digests differ despite field-equal inputs — ComputeKey non-determinism?")
	}
}

// PAGINATION ARM 2 — paginated, no regression. A real paginated /call
// (perPage=20&page=2) and a sub sending the same 20,2 must still match.
func TestFalsifier64Pagination_PaginatedNoRegression(t *testing.T) {
	buildWidgetParityWatcher(t, true, nil)
	ctx := ctxUserA()

	coords := panelCoords(classWidgets, nil)
	coords.PerPage = 20
	coords.Page = 2

	subKey, ok := DeriveSubscriptionKey(ctx, coords)
	if !ok {
		t.Fatalf("DeriveSubscriptionKey(paginated) failed")
	}

	// Emit from the REAL paginationInfo over a 20/2 query.
	req := httptest.NewRequest("GET", "/call?resource=panels&apiVersion=widgets.templates.krateo.io/v1beta1&perPage=20&page=2", nil)
	pp, pg := paginationInfo(slog.Default(), req)
	if pp != 20 || pg != 2 {
		t.Fatalf("setup: paginationInfo(20,2) returned %d,%d", pp, pg)
	}
	emitKey, _, _ := dispatchCacheLookupKey(ctx, classWidgets,
		coords.Group, coords.Version, coords.Resource, coords.Namespace, coords.Name,
		pp, pg, nil)

	if subKey != emitKey {
		t.Fatalf("PAGINATION ARM2: paginated sub key %q != emit key %q — pagination regression", subKey, emitKey)
	}
}

// PAGINATION ARM 3 — per-class: widgetContent + restactions also normalize
// (the fold is class-independent). Both must match the emit -1,-1 default from
// a 0,0 sub. RED today / GREEN after.
func TestFalsifier64Pagination_PerClassNormalize(t *testing.T) {
	buildWidgetParityWatcher(t, true, nil)
	ctx := ctxUserA()

	for _, class := range []string{cache.CacheEntryClassWidgetContent, classRestActions} {
		coords := panelCoords(class, nil) // 0,0 sub
		subKey, ok := DeriveSubscriptionKey(ctx, coords)
		if !ok || subKey == "" {
			t.Fatalf("class %s: DeriveSubscriptionKey failed (ok=%v)", class, ok)
		}
		// Emit -1,-1 default for the same class.
		req := httptest.NewRequest("GET", "/call?resource=panels&apiVersion=widgets.templates.krateo.io/v1beta1", nil)
		pp, pg := paginationInfo(slog.Default(), req)
		var emitKey string
		if class == cache.CacheEntryClassWidgetContent {
			emitKey, _, _ = dispatchWidgetContentKey(ctx,
				coords.Group, coords.Version, coords.Resource, coords.Namespace, coords.Name, pp, pg, coords.Extras)
		} else {
			emitKey, _, _ = dispatchCacheLookupKey(ctx, class,
				coords.Group, coords.Version, coords.Resource, coords.Namespace, coords.Name, pp, pg, coords.Extras)
		}
		if subKey != emitKey {
			t.Fatalf("PAGINATION ARM3 class %s RED: sub key %q != emit -1,-1 key %q (per-class normalization missing)",
				class, subKey, emitKey)
		}
	}
}
