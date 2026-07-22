// refresh_subscription_invariant_test.go — #67 (frontend's TOP ask): the
// standing per-class subscription-key invariant. For coords as the frontend
// would send them from a /call response, DeriveSubscriptionKey(coords) MUST
// equal the EMIT cache key ComputeKey(call) — PER CLASS (widgets /
// widgetContent / restactions), over REAL widget shapes (the
// descriptions-composition-detail-metadata shape: apiRef-based, NO inline
// extras) and the REAL emit-default pagination path (paginationInfo, NEVER a
// hand-set tuple).
//
// This codifies the forgery-proof design's load-bearing equality — the one
// that broke SILENTLY across 1.5.5–1.5.11 (the whole /refreshes saga). It is
// the guard that would have caught EVERY broken build pre-deploy, hermetically.
//
// Built on the #66 shared-seam refactor: it exercises the REAL
// deriveSubscription body (via DeriveSubscriptionKey + the pre-hash accessor),
// not a mirror — so it cannot pass against a stale shadow.
//
// THE SAGA LESSON, codified: the EMIT side is computed from the REAL paths —
// paginationInfo's default (no page/perPage query) for pagination, and the
// SAME unionForKey(GetApiRefExtras, GetResourcesRefsExtras, request) the
// widgets.go genuine-Put folds for extras. The SUB side is the production
// DeriveSubscriptionKey over coords shaped as the frontend's ?sub= sends them
// (page/perPage OMITTED → 0,0). They must be byte-identical for every class.
package dispatchers

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"strconv"
	"testing"

	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets"
)

// emitInputsForClass computes the EMIT-side ResolvedKeyInputs the /call
// dispatcher would Put for the given class + coords, using ONLY production
// paths: paginationInfo's real default (no page/perPage query) and, for the
// widget classes, the SAME inline-extras union widgets.go folds. No hand-set
// tuple, no constructed extras — the real emit shape.
func emitInputsForClass(t *testing.T, ctx context.Context, class string, coords SubscriptionCoordinates) *cache.ResolvedKeyInputs {
	t.Helper()
	// Drive the REAL paginationInfo over a /call shaped like the frontend's:
	// NO page/perPage query for the non-paginated case (→ -1,-1 default), or
	// the explicit query for the paginated case (→ the frontend's 20,2). NEVER
	// a hand-passed tuple — the emit pagination comes from paginationInfo.
	url := "/call?resource=" + coords.Resource + "&apiVersion=" + coords.Group + "/" + coords.Version
	if coords.PerPage > 0 {
		url += "&perPage=" + strconv.Itoa(coords.PerPage) + "&page=" + strconv.Itoa(coords.Page)
	}
	req := httptest.NewRequest("GET", url, nil)
	pp, pg := paginationInfo(slog.Default(), req)

	switch class {
	case classWidgets, cache.CacheEntryClassWidgetContent:
		// Fold the SAME union the widgets.go genuine-Put folds, from the REAL CR.
		got := objects.Get(ctx, templatesv1.ObjectReference{
			Reference:  templatesv1.Reference{Name: coords.Name, Namespace: coords.Namespace},
			APIVersion: coords.Group + "/" + coords.Version,
			Resource:   coords.Resource,
		})
		if got.Err != nil || got.Unstructured == nil {
			t.Fatalf("emit setup: objects.Get(%s/%s) failed: %v", coords.Namespace, coords.Name, got.Err)
		}
		emitExtras := unionForKey(
			widgets.GetApiRefExtras(got.Unstructured.Object),
			widgets.GetResourcesRefsExtras(got.Unstructured.Object),
			coords.Extras)
		var inputs *cache.ResolvedKeyInputs
		if class == cache.CacheEntryClassWidgetContent {
			_, _, inputs = dispatchWidgetContentKey(ctx,
				coords.Group, coords.Version, coords.Resource, coords.Namespace, coords.Name, pp, pg, emitExtras)
		} else {
			_, _, inputs = dispatchCacheLookupKey(ctx, class,
				coords.Group, coords.Version, coords.Resource, coords.Namespace, coords.Name, pp, pg, emitExtras)
		}
		return inputs

	default: // restactions / apistage / raFullList — request-only extras.
		_, _, inputs := dispatchCacheLookupKey(ctx, class,
			coords.Group, coords.Version, coords.Resource, coords.Namespace, coords.Name, pp, pg, coords.Extras)
		return inputs
	}
}

// TestFalsifier67_SubscriptionKeyInvariant_PerClass — the standing invariant,
// over REPRESENTATIVE REAL widget shapes × the three subscribable classes (the
// arch's HARD constraint against re-creating the masking): a no-inline-extras
// detail widget (the descriptions-composition-detail-metadata shape that
// diverged silently for 6 builds), an inline-extras widget, and a paginated
// request. For each, the SUBSCRIPTION key (production DeriveSubscriptionKey
// over frontend-shaped coords) MUST equal the EMIT key (real paginationInfo
// default + real fetched-CR extras union), with field-by-field pre-hash
// equality as the stronger BindingUID-independent proof.
func TestFalsifier67_SubscriptionKeyInvariant_PerClass(t *testing.T) {
	shapes := []struct {
		name              string
		inlineApiRefExtra map[string]any // nil → no-inline detail shape
		perPage, page     int            // 0,0 → frontend non-paginated; >0 → paginated
	}{
		{name: "no-inline-detail", inlineApiRefExtra: nil, perPage: 0, page: 0},
		{name: "inline-extras", inlineApiRefExtra: map[string]any{"region": "eu"}, perPage: 0, page: 0},
		{name: "paginated", inlineApiRefExtra: nil, perPage: 20, page: 2},
	}
	classes := []string{classWidgets, cache.CacheEntryClassWidgetContent, classRestActions}

	for _, sh := range shapes {
		sh := sh
		t.Run(sh.name, func(t *testing.T) {
			// Seed the REAL widget CR for this shape (apiRef-based; inline extras
			// only when the shape declares them). The emit side reads this exact
			// CR — never a hand-constructed parallel.
			buildWidgetParityWatcher(t, true, sh.inlineApiRefExtra)
			ctx := ctxUserA()

			for _, class := range classes {
				t.Run(class, func(t *testing.T) {
					coords := panelCoords(class, nil)
					coords.PerPage = sh.perPage // frontend sends these verbatim; 0,0 for non-paginated
					coords.Page = sh.page

					subKey, ok := DeriveSubscriptionKey(ctx, coords)
					if !ok || subKey == "" {
						t.Fatalf("%s/%s: DeriveSubscriptionKey failed (ok=%v)", sh.name, class, ok)
					}
					subIn, okIn := deriveSubscriptionKeyInputsForTest(ctx, coords)
					if !okIn || subIn == nil {
						t.Fatalf("%s/%s: sub inputs failed", sh.name, class)
					}

					emitIn := emitInputsForClass(t, ctx, class, coords)
					if emitIn == nil {
						t.Fatalf("%s/%s: emit inputs nil", sh.name, class)
					}

					// THE INVARIANT: digest equality (what the broadcaster matches on).
					if got := cache.ComputeKey(*emitIn); subKey != got {
						t.Fatalf("%s/%s INVARIANT BROKEN: DeriveSubscriptionKey %q != emit ComputeKey %q — the "+
							"armed key would never match the published key (the 1.5.5–1.5.11 zero-delivery class).",
							sh.name, class, subKey, got)
					}
					// Field-by-field pre-hash equality (the stronger proof; names
					// the diverging field if it ever regresses).
					assertKeyInputsFieldEqual(t, sh.name+"/"+class, subIn, emitIn)
				})
			}
		})
	}
}

// assertKeyInputsFieldEqual checks every ComputeKey-folded field of two
// ResolvedKeyInputs is equal (RepresentativeUsername/Groups are excluded from
// the hash, so excluded here). On mismatch it names the diverging field — the
// pre-hash input-string diff the saga prescribes as prevention.
func assertKeyInputsFieldEqual(t *testing.T, class string, a, b *cache.ResolvedKeyInputs) {
	t.Helper()
	if a.CacheEntryClass != b.CacheEntryClass {
		t.Fatalf("%s field CacheEntryClass: %q != %q", class, a.CacheEntryClass, b.CacheEntryClass)
	}
	if a.Group != b.Group || a.Version != b.Version || a.Resource != b.Resource {
		t.Fatalf("%s field GVR: %s/%s/%s != %s/%s/%s", class, a.Group, a.Version, a.Resource, b.Group, b.Version, b.Resource)
	}
	if a.Namespace != b.Namespace || a.Name != b.Name {
		t.Fatalf("%s field ns/name: %s/%s != %s/%s", class, a.Namespace, a.Name, b.Namespace, b.Name)
	}
	if a.BindingUID != b.BindingUID {
		t.Fatalf("%s field BindingUID: %q != %q", class, a.BindingUID, b.BindingUID)
	}
	if a.PerPage != b.PerPage || a.Page != b.Page {
		t.Fatalf("%s field pagination: %d/%d != %d/%d (the #64 page/perPage divergence class)", class, a.PerPage, a.Page, b.PerPage, b.Page)
	}
	if a.Stage != b.Stage {
		t.Fatalf("%s field Stage: %q != %q", class, a.Stage, b.Stage)
	}
	// #118 (c) C-118-4 — the per-user RBAC sub-generation must be pre-hash
	// field-equal across arming↔serve (both derive it via the SAME
	// dispatchCacheLookupKey → RBACSubGenForSubject). A divergence here means a
	// subscription armed a different sub-gen than the emit stamped → the armed
	// key would never match after a grant/revoke rotated one side only (the #64
	// desync class at the RBAC-freshness dimension). Extends the #67 invariant.
	if a.RBACSubGen != b.RBACSubGen {
		t.Fatalf("%s field RBACSubGen: %d != %d — arming↔serve RBAC sub-gen desync (a grant/revoke would rotate one side's key only)", class, a.RBACSubGen, b.RBACSubGen)
	}
	if cache.ComputeKey(*a) != cache.ComputeKey(*b) {
		t.Fatalf("%s: digests differ despite field-equal scalars — Extras canonicalisation divergence?", class)
	}
}
