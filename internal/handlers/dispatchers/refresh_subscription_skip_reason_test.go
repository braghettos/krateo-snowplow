// refresh_subscription_skip_reason_test.go — #101 falsifier for the
// DeriveSubscriptionKeyWithReason skip classification that feeds the handler's
// per-subscription {armed, skipped_informer_miss} INFO summary.
//
// The /refreshes arming path runs under cache.WithInformerOnlyReads (set by the
// handler): an informer GET-miss on a widget/widgetContent coord returns a quiet
// NotFound instead of the endpoint-error storm. This asserts the derivation
// reports WHY a coord skipped so the summary can bucket it:
//   - widget coord whose CR is absent from the informer → SubscriptionSkipInformerMiss
//   - widget coord whose CR IS present → armed (SubscriptionArmed)
//   - unknown class → SubscriptionSkipOther (never mis-attributed to informer-miss)
//
// Reuses the buildWidgetParityWatcher fixture (real fake dynamic client, RBAC
// seeded, panel GVR servable) + ctxUserA. Hermetic, -race, no cluster.

package dispatchers

import (
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// TestDeriveSubscriptionKeyWithReason_InformerMiss — the widget CR is NOT seeded
// (seedWidget=false) so the informer-only objects.Get misses → the coord skips
// with SubscriptionSkipInformerMiss (the #101 storm-coord bucket), NOT armed and
// NOT mis-classified as SubscriptionSkipOther.
func TestDeriveSubscriptionKeyWithReason_InformerMiss(t *testing.T) {
	buildWidgetParityWatcher(t, false, nil) // GVR servable, CR ABSENT
	ctx := cache.WithInformerOnlyReads(ctxUserA())

	for _, class := range []string{classWidgets, cache.CacheEntryClassWidgetContent} {
		t.Run(class, func(t *testing.T) {
			coords := panelCoords(class, nil)
			key, ok, reason := DeriveSubscriptionKeyWithReason(ctx, coords)
			if ok || key != "" {
				t.Fatalf("%s: expected skip (CR absent); got ok=%v key=%q", class, ok, key)
			}
			if reason != SubscriptionSkipInformerMiss {
				t.Fatalf("%s: expected SubscriptionSkipInformerMiss (%d); got %d — the summary would mis-bucket the storm coord",
					class, SubscriptionSkipInformerMiss, reason)
			}
		})
	}
}

// TestDeriveSubscriptionKeyWithReason_ArmedWhenPresent — the SAME fixture WITH
// the widget CR seeded arms the coord (SubscriptionArmed) with a real key. This
// is the GREEN counterpart proving the informer-miss classification keys on CR
// presence, not on the class or the marker.
func TestDeriveSubscriptionKeyWithReason_ArmedWhenPresent(t *testing.T) {
	buildWidgetParityWatcher(t, true, nil) // CR PRESENT
	ctx := cache.WithInformerOnlyReads(ctxUserA())

	coords := panelCoords(classWidgets, nil)
	key, ok, reason := DeriveSubscriptionKeyWithReason(ctx, coords)
	if !ok || key == "" {
		t.Fatalf("expected armed (CR present); got ok=%v key=%q reason=%d", ok, key, reason)
	}
	if reason != SubscriptionArmed {
		t.Fatalf("expected SubscriptionArmed (%d); got %d", SubscriptionArmed, reason)
	}
}

// TestDeriveSubscriptionKeyWithReason_UnknownClassIsOther — an unknown class
// fails closed as SubscriptionSkipOther, never mis-attributed to informer-miss
// (the summary's skipped_informer_miss count must reflect ONLY real GET-misses).
func TestDeriveSubscriptionKeyWithReason_UnknownClassIsOther(t *testing.T) {
	buildWidgetParityWatcher(t, true, nil)
	ctx := cache.WithInformerOnlyReads(ctxUserA())

	coords := panelCoords(classWidgets, nil)
	coords.Class = "not-a-real-class"
	key, ok, reason := DeriveSubscriptionKeyWithReason(ctx, coords)
	if ok || key != "" {
		t.Fatalf("unknown class: expected skip; got ok=%v key=%q", ok, key)
	}
	if reason != SubscriptionSkipOther {
		t.Fatalf("unknown class: expected SubscriptionSkipOther (%d); got %d", SubscriptionSkipOther, reason)
	}
}
