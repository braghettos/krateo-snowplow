// deps_refresh_delivery_falsifier_test.go — #61 (1.5.8) the /refreshes
// zero-delivery falsifier set.
//
// RCA: docs/rca-refreshes-zero-delivery-2026-06-26.md. A composition-DETAIL
// widget resolves its displayed target into status.resourcesRefs.items[] with
// an EMPTY spec.resourcesRefs. extractResourcesRefs USED to read only `spec`
// → recordWidgetDeps recorded NO dep edge on the displayed resource → when
// that resource reconciled, OnUpdate dirty-marked only the intermediate
// apistage cells, NEVER the top-level armed L1 key → PublishRefresh never
// fired for it → the armed /refreshes subscriber received zero `event:
// refresh`. Broken 1.5.5–1.5.7. The fix (deps_extract.go) reads the UNION of
// status.resourcesRefs ∪ spec.resourcesRefs.
//
// THREE arms (RCA §5):
//   - ARM A (key-equality golden) — REGRESSION GUARD, green pre+post. Lives in
//     handlers/refreshes_test.go + dispatchers/refresh_isolation_falsifier_test.go
//     (DeriveSubscriptionKey == the serve key, byte-identical per class). The
//     keys were never the bug; that existing golden pins the equality contract
//     that was previously untested. NOT re-duplicated here.
//   - ARM B (TestFalsifier61_StatusRefDepCoverage) — the discriminating
//     dep-coverage falsifier: status ref R, EMPTY spec → recordWidgetDeps →
//     OnUpdate(R) dirty-marks the widget key. RED pre-fix (spec-only read →
//     no dep on R → key NOT marked), GREEN post-fix.
//   - C-1 (TestFalsifier61_EndToEndDelivery) — the REQUIRED sufficiency arm:
//     seed the widget L1 entry, recordWidgetDeps from a status-bearing res,
//     arm SubscribeRefresh for the key, OnUpdate(R) through the REAL refresh
//     hook, assert refreshDeliveredTotal +1 AND the subscriber channel
//     receives the key. Proves dirty-mark → enqueue → (refresher terminal
//     PublishRefresh) → delivery END-TO-END.
//
// Hermetic: no apiserver, no kubeconfig. The one MODELED link in C-1 is the
// refresher's re-resolve→Put→PublishRefresh terminal step: rather than spin
// the async refresher worker + a real resolve closure (non-hermetic, pulls the
// whole resolver), the test wires the dep tracker's REAL refresh hook
// (Deps().SetRefreshHook, the exact seam refresher.go:425 wires) to call
// cache.PublishRefresh(l1Key) — which is verbatim what resolve_populate.go:328
// does after the refresher's Put. So the chain under test is the real
// OnUpdate→dirty-mark→enqueueFn→PublishRefresh→subscriber path; only the
// re-resolve compute between enqueue and publish is elided (it cannot change
// WHICH key publishes).

package dispatchers

import (
	"testing"
	"time"

	"log/slog"

	"github.com/krateoplatformops/snowplow/internal/cache"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// detailWidgetForTest builds a composition-DETAIL widget in the post-resolve
// shape: the displayed target (awsvpcstacks/demo-vpc) lives ONLY in
// status.resourcesRefs.items[] (the resolver writes it there from the request
// extras); spec.resourcesRefs is EMPTY (the templated-detail shape). This is
// the exact shape that recorded zero dep edges pre-fix.
func detailWidgetForTest() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "widgets.templates.krateo.io/v1beta1",
		"kind":       "Panel",
		"metadata": map[string]any{
			"name":      "detail-panel-1",
			"namespace": "demo-ns",
		},
		// EMPTY spec.resourcesRefs — the displayed target is request-driven,
		// not author-declared. (No apiRef either: isolate edge type 1.)
		"spec": map[string]any{},
		// The resolved displayed ref lives in STATUS as a ResourceRefResult:
		// {id, path, verb, allowed}. The gvr/ns/name are encoded ONLY in the
		// /call `path` query string — there are NO inline apiVersion/resource/
		// namespace/name fields. This is the trap: a naive status FIELD read
		// finds empty strings; only objects.ParseCallPathToObjectRef on `path`
		// recovers the displayed target.
		"status": map[string]any{
			"resourcesRefs": map[string]any{
				"items": []any{
					map[string]any{
						"id":      "displayed-1",
						"path":    "/call?resource=awsvpcstacks&apiVersion=composition.krateo.io%2Fv1&namespace=demo-system&name=demo-vpc",
						"verb":    "GET",
						"allowed": true,
					},
				},
			},
		},
	}}
}

// displayedResource is the (gvr, ns, name) the detail widget DISPLAYS — the
// resource whose reconcile must dirty-mark the widget key.
var (
	displayedGVR  = schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1", Resource: "awsvpcstacks"}
	displayedNS   = "demo-system"
	displayedName = "demo-vpc"

	detailWidgetGVR = schema.GroupVersionResource{Group: "widgets.templates.krateo.io", Version: "v1beta1", Resource: "panels"}
)

// TestFalsifier61_StatusRefDepCoverage — ARM B. The discriminating
// dep-coverage falsifier. RED before the deps_extract union fix (spec-only
// read finds no displayed ref → no dep edge → OnUpdate marks 0), GREEN after
// (union reads status → dep edge on demo-vpc → OnUpdate marks the widget key).
func TestFalsifier61_StatusRefDepCoverage(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	cache.ResetDepsForTest()

	l1Key := "L1_detail_demo_vpc"
	recordWidgetDeps(slog.Default(), l1Key, detailWidgetGVR, detailWidgetForTest())

	// The widget key MUST depend on the DISPLAYED resource (status ref).
	matched := cache.Deps().CollectMatchesForTest(displayedGVR, displayedNS, displayedName)
	if _, ok := matched[l1Key]; !ok {
		t.Fatalf("ARM B RED: widget key %q records NO dep edge on the DISPLAYED resource %s/%s/%s "+
			"(status.resourcesRefs ref). extractResourcesRefs must read status ∪ spec, not spec-only. matched=%v",
			l1Key, displayedGVR, displayedNS, displayedName, matched)
	}

	// And a real OnUpdate on that resource must dirty-mark (return marked>=1
	// including our key).
	marked := cache.Deps().OnUpdate(displayedGVR, displayedNS, displayedName)
	if marked < 1 {
		t.Fatalf("ARM B RED: OnUpdate(%s/%s/%s) dirty-marked %d keys — the widget key was never enqueued; "+
			"PublishRefresh can never fire for it.", displayedGVR, displayedNS, displayedName, marked)
	}
}

// TestFalsifier61_StaticSpecRefStillRecorded — UNION guard (C-0): a
// genuinely-static spec.resourcesRefs ref MUST still be recorded after the fix
// (the union must not be a status-only flip that drops static refs).
func TestFalsifier61_StaticSpecRefStillRecorded(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	cache.ResetDepsForTest()

	staticGVR := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1", Resource: "compositions"}
	w := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "widgets.templates.krateo.io/v1beta1",
		"kind":       "Panel",
		"metadata":   map[string]any{"name": "static-panel", "namespace": "demo-ns"},
		"spec": map[string]any{
			"resourcesRefs": map[string]any{
				"items": []any{
					map[string]any{
						"id":         "static-1",
						"apiVersion": "composition.krateo.io/v1",
						"resource":   "compositions",
						"namespace":  "bench-ns-01",
						"name":       "static-app-01",
					},
				},
			},
		},
		// Empty status — the static-only widget shape.
		"status": map[string]any{},
	}}

	l1Key := "L1_static_panel"
	recordWidgetDeps(slog.Default(), l1Key, detailWidgetGVR, w)

	matched := cache.Deps().CollectMatchesForTest(staticGVR, "bench-ns-01", "static-app-01")
	if _, ok := matched[l1Key]; !ok {
		t.Fatalf("UNION GUARD: the union DROPPED a static spec.resourcesRefs ref %s/bench-ns-01/static-app-01 "+
			"(the fix must be status∪spec, NOT a flip to status-only). matched=%v", staticGVR, matched)
	}
}

// TestFalsifier61_EndToEndDelivery — C-1 (HARD, REQUIRED). Proves the full
// sufficiency chain: recordWidgetDeps(status ref) → SubscribeRefresh(armed
// key) → OnUpdate(R) → [real refresh hook = the resolve_populate.go:328
// PublishRefresh terminal] → the armed subscriber RECEIVES the key AND
// refreshDeliveredTotal increments. RED pre-fix (no dep edge → OnUpdate marks
// 0 → hook never called → no publish → subscriber gets nothing → delivered
// stays flat).
func TestFalsifier61_EndToEndDelivery(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	// HERMETIC TO THE ENV: do NOT rely on the REFRESH_SSE_ENABLED default —
	// RefreshSSEEnabled() is `env != "false"`, so a runner whose shell has
	// REFRESH_SSE_ENABLED set to anything-but-true makes ResetRefreshBroadcasterForTest
	// re-read it disabled -> the hub is nil -> SubscribeRefresh returns a CLOSED
	// channel -> zero delivery -> a green-here/red-on-another-env split on a HARD
	// gate. Hard-set it BEFORE the reset so the reset picks up the enabled hub.
	t.Setenv("REFRESH_SSE_ENABLED", "true")
	cache.ResetDepsForTest()
	cache.ResetRefreshBroadcasterForTest()
	t.Cleanup(cache.ResetRefreshBroadcasterForTest)

	l1Key := "L1_detail_demo_vpc_e2e"

	// Wire the REAL dep-tracker refresh hook to the REAL terminal publish
	// (verbatim resolve_populate.go:328: cache.PublishRefresh(key) after a
	// successful refresher re-resolve+Put). The re-resolve compute between
	// enqueue and publish is elided — it cannot change WHICH key publishes.
	cache.Deps().SetRefreshHook(func(key string, _ schema.GroupVersionResource) {
		cache.PublishRefresh(key)
	})

	// Record the dep edges from the resolved detail widget (status-bearing).
	recordWidgetDeps(slog.Default(), l1Key, detailWidgetGVR, detailWidgetForTest())

	// Arm a /refreshes subscriber for the widget key (the seam
	// handlers.Refreshes uses after re-deriving the key under the connection
	// identity).
	ch, unsub := cache.SubscribeRefresh(map[string]struct{}{l1Key: {}})
	defer unsub()

	_, deliveredBefore, _, _ := cache.RefreshBroadcasterCounters()

	// The displayed resource reconciles (18×/min in production).
	cache.Deps().OnUpdate(displayedGVR, displayedNS, displayedName)

	// The armed subscriber must receive the key.
	select {
	case got := <-ch:
		if got != l1Key {
			t.Fatalf("C-1: subscriber received key %q, want the armed widget key %q", got, l1Key)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("C-1 RED: /refreshes subscriber received NOTHING within 2s after the displayed resource "+
			"reconciled — the dep-change never reached PublishRefresh for the armed key (zero-delivery).")
	}

	_, deliveredAfter, _, _ := cache.RefreshBroadcasterCounters()
	if deliveredAfter != deliveredBefore+1 {
		t.Fatalf("C-1: refreshDeliveredTotal = %d, want %d (exactly one (key→subscriber) delivery)",
			deliveredAfter, deliveredBefore+1)
	}
}
