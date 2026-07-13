// external_ttl_falsifier_test.go — external-widget bounded-TTL cache
// (Option A, 2026-07-10) SAFETY falsifiers at the dispatcher layer.
//
// These are the commit-blocking safety arms (the design's C3 F-key-churn arm
// is a POST-DEPLOY gate on the live obs CR + real Chrome, NOT here):
//
//   - F-toggle-off (C4, §10.2/§11.2): feature OFF → the refresh-key header is
//     stamped byte-identically to today on BOTH the HIT branch and the cold
//     tail, because ExternalTTL/servedExternalTTL are false. The new helper
//     with externalTTL=false is byte-identical to setRefreshKeyHeader.
//   - F-no-arm (C2, §11.5): feature ON (an external-TTL serve) → NO
//     X-Snowplow-Refresh-Key / -Class header is stamped, on BOTH branches, so
//     the browser never arms a /refreshes subscription for a dep-edgeless key.
//   - F-isolation (C1, §11.1) TWO-DIMENSIONAL: (dim-1 KEY) two users under
//     DIFFERENT bindings produce DIFFERENT ComputeKey key hashes for the SAME
//     widget coords (BindingUID fold); (dim-2 OUTPUT) user B never READS user
//     A's cached external bytes out of the shared store. A RED variant routes
//     the external result to the identity-free cell (BindingUID="") and PROVES
//     the harness CATCHES B receiving A's bytes — so the arm discriminates a
//     content-spoof that keeps hashes different while still cross-delivering.
//   - annotation-parse: the general opt-in surface (cap, default-off, malformed
//     → off), no hardcoded widget special-case.
//
// Run: go test -race -count=1 ./internal/handlers/dispatchers/ -run ExternalTTL

package dispatchers

import (
	"net/http/httptest"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ---------------------------------------------------------------------------
// F-toggle-off + F-no-arm — the header choke point (setRefreshKeyHeaderUnlessExternal)
// ---------------------------------------------------------------------------

// TestExternalTTL_HeaderSuppression_BothBranches pins the C2 arming kill and
// the C4 byte-identity together at the single choke point both serve branches
// use (widgets.go:244 HIT / widgets.go cold tail). externalTTL=false MUST be
// byte-identical to the plain setRefreshKeyHeader (C4); externalTTL=true MUST
// stamp NOTHING (F-no-arm) so the browser never arms.
func TestExternalTTL_HeaderSuppression_BothBranches(t *testing.T) {
	cases := []struct {
		name        string
		key         string
		externalTTL bool
		wantKey     string // "" = header MUST be absent
		wantClass   string
	}{
		{
			name: "feature OFF (normal widget, externalTTL=false) — byte-identical to today (C4)",
			key:  "k-widgets", externalTTL: false,
			wantKey: "k-widgets", wantClass: "widgets",
		},
		{
			name: "feature ON (external-TTL serve, externalTTL=true) — NO header, browser cannot arm (C2/F-no-arm)",
			key:  "k-widgets", externalTTL: true,
			wantKey: "", wantClass: "",
		},
		{
			name: "empty key + externalTTL=false — still absent (unchanged additive contract)",
			key:  "", externalTTL: false,
			wantKey: "", wantClass: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			setRefreshKeyHeaderUnlessExternal(rec, tc.key, "widgets", tc.externalTTL)
			if got := rec.Header().Get(refreshKeyHeader); got != tc.wantKey {
				t.Fatalf("%s = %q, want %q", refreshKeyHeader, got, tc.wantKey)
			}
			if got := rec.Header().Get(refreshClassHeader); got != tc.wantClass {
				t.Fatalf("%s = %q, want %q", refreshClassHeader, got, tc.wantClass)
			}
		})
	}
}

// TestExternalTTL_ToggleOff_ByteIdenticalToPlainHelper is the discriminating
// C4 control: for a NON-external serve (externalTTL=false) the new helper must
// produce a header state INDISTINGUISHABLE from the pre-Option-A
// setRefreshKeyHeader — across every (key,class) shape a widget serve uses.
func TestExternalTTL_ToggleOff_ByteIdenticalToPlainHelper(t *testing.T) {
	shapes := []struct{ key, class string }{
		{"k-widgets", "widgets"},
		{"k-ra", "restactions"},
		{"", "widgets"},
	}
	for _, s := range shapes {
		plain := httptest.NewRecorder()
		setRefreshKeyHeader(plain, s.key, s.class)

		guarded := httptest.NewRecorder()
		setRefreshKeyHeaderUnlessExternal(guarded, s.key, s.class, false)

		if plain.Header().Get(refreshKeyHeader) != guarded.Header().Get(refreshKeyHeader) ||
			plain.Header().Get(refreshClassHeader) != guarded.Header().Get(refreshClassHeader) {
			t.Fatalf("C4 FAIL: externalTTL=false diverged from setRefreshKeyHeader for (%q,%q): "+
				"plain{key=%q,class=%q} vs guarded{key=%q,class=%q}",
				s.key, s.class,
				plain.Header().Get(refreshKeyHeader), plain.Header().Get(refreshClassHeader),
				guarded.Header().Get(refreshKeyHeader), guarded.Header().Get(refreshClassHeader))
		}
	}
}

// NOTE: F-isolation (C1) TWO-DIMENSIONAL — the KEY-hash + resolved-OUTPUT arms
// and the RED identity-free-cell cross-delivery variant — lives in the cache
// package (external_ttl_entry_falsifier_test.go), where BOTH the real
// cache.ComputeKey derivation and the real store (newResolvedCache) are in
// scope. It drives the SHIPPED per-binding key end-to-end; keeping it beside
// ComputeKey avoids a cross-package unexported-store shim.

// ---------------------------------------------------------------------------
// annotation-parse — the general opt-in surface (no hardcoded special-case).
// ---------------------------------------------------------------------------

func objWithAnnotation(kv map[string]string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{}}
	if kv != nil {
		o.SetAnnotations(kv)
	}
	return o
}

func TestExternalTTL_AnnotationParse(t *testing.T) {
	cases := []struct {
		name string
		ann  map[string]string
		want time.Duration
	}{
		{"absent → OFF (default)", nil, 0},
		{"empty map → OFF", map[string]string{}, 0},
		{"unrelated annotation only → OFF", map[string]string{"other": "5"}, 0},
		{"empty value → OFF", map[string]string{externalCacheTTLAnnotation: ""}, 0},
		{"zero → OFF", map[string]string{externalCacheTTLAnnotation: "0"}, 0},
		{"negative → OFF", map[string]string{externalCacheTTLAnnotation: "-10"}, 0},
		{"unparseable → OFF (never errors the serve)", map[string]string{externalCacheTTLAnnotation: "abc"}, 0},
		{"float string unparseable → OFF", map[string]string{externalCacheTTLAnnotation: "20.5"}, 0},
		{"valid 20s → ON", map[string]string{externalCacheTTLAnnotation: "20"}, 20 * time.Second},
		{"at cap 120s → ON", map[string]string{externalCacheTTLAnnotation: "120"}, 120 * time.Second},
		{"over cap 3600s → clamped to 120s (defensive, §5)", map[string]string{externalCacheTTLAnnotation: "3600"}, 120 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := externalCacheTTLFromAnnotations(objWithAnnotation(tc.ann))
			if got != tc.want {
				t.Fatalf("externalCacheTTLFromAnnotations = %v, want %v", got, tc.want)
			}
		})
	}
	// nil object → OFF (nil-safe).
	if got := externalCacheTTLFromAnnotations(nil); got != 0 {
		t.Fatalf("nil obj: got %v, want 0 (off)", got)
	}
}

// TestExternalTTL_AnnotationIsGeneral_NoWidgetSpecialCase guards
// feedback_no_special_cases: the opt-in must be the ANNOTATION, never a
// hardcoded widget name. A widget named "obs-*" with NO annotation is OFF; a
// widget with ANY other name WITH the annotation is ON. Enablement is
// name-independent.
func TestExternalTTL_AnnotationIsGeneral_NoWidgetSpecialCase(t *testing.T) {
	// obs-named widget, NO annotation → OFF (no name-based special-case).
	obsNoAnn := &unstructured.Unstructured{Object: map[string]any{}}
	obsNoAnn.SetName("obs-log-stream")
	if got := externalCacheTTLFromAnnotations(obsNoAnn); got != 0 {
		t.Fatalf("no-special-case FAIL: an obs-* widget with NO annotation must be OFF; got %v — "+
			"a hardcoded name special-case would turn it on", got)
	}
	// Arbitrary-named widget, WITH annotation → ON (opt-in is general).
	otherWithAnn := &unstructured.Unstructured{Object: map[string]any{}}
	otherWithAnn.SetName("some-random-widget")
	otherWithAnn.SetAnnotations(map[string]string{externalCacheTTLAnnotation: "15"})
	if got := externalCacheTTLFromAnnotations(otherWithAnn); got != 15*time.Second {
		t.Fatalf("no-special-case FAIL: a non-obs widget WITH the annotation must be ON; got %v", got)
	}
}
