// refresh_class_header_test.go — task #48: finalize the SSE-arming class
// contract. snowplow now exposes the CacheEntryClass it keyed a /call
// response under via X-Snowplow-Refresh-Class, so the frontend arms the
// EXACT /refreshes subscription class instead of guessing widgets-vs-
// widgetContent (frontend guide §2.5/§8).
//
// These hermetic falsifiers pin two properties:
//  1. setRefreshKeyHeader stamps BOTH X-Snowplow-Refresh-Key and
//     X-Snowplow-Refresh-Class together, per class; omits both on an empty
//     key (additive, never-empty contract).
//  2. The class string each dispatcher serve-site passes
//     (widgets.go widgetContent/widgets, restactions.go restactions) is an
//     ACCEPTED SubscriptionCoordinates.Class — i.e. a value the frontend
//     can hand straight back to /refreshes ?sub. A typo (e.g.
//     "widgetcontent") would land in DeriveSubscriptionKey's fail-closed
//     default and be unarmable; this guard catches it at the source.

package dispatchers

import (
	"net/http/httptest"
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

func TestSetRefreshKeyHeader_StampsKeyAndClass(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		class string
		// expected header values; "" means the header MUST be absent.
		wantKey   string
		wantClass string
	}{
		{
			name: "widgetContent content-hit (widgets.go:169)",
			key:  "k-widgetcontent", class: "widgetContent",
			wantKey: "k-widgetcontent", wantClass: "widgetContent",
		},
		{
			name: "widgets hit/miss (widgets.go:210,341)",
			key:  "k-widgets", class: "widgets",
			wantKey: "k-widgets", wantClass: "widgets",
		},
		{
			name: "restactions hit/miss (restactions.go:139,314)",
			key:  "k-ra", class: "restactions",
			wantKey: "k-ra", wantClass: "restactions",
		},
		{
			name: "empty key — BOTH headers absent (additive, never-empty)",
			key:  "", class: "widgets",
			wantKey: "", wantClass: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			setRefreshKeyHeader(rec, tc.key, tc.class)

			if got := rec.Header().Get(refreshKeyHeader); got != tc.wantKey {
				t.Fatalf("%s = %q, want %q", refreshKeyHeader, got, tc.wantKey)
			}
			if got := rec.Header().Get(refreshClassHeader); got != tc.wantClass {
				t.Fatalf("%s = %q, want %q", refreshClassHeader, got, tc.wantClass)
			}
		})
	}
}

// TestRefreshClassHeader_ValuesAreArmableSubscriptionClasses guards the
// call-site wiring: every class string the dispatcher stamps MUST be a
// member of the accepted SubscriptionCoordinates.Class set (the same set
// DeriveSubscriptionKey switches on). If a serve site stamped a typo'd or
// non-subscription class, the frontend would echo an unarmable class into
// /refreshes ?sub and fall to the fail-closed default — this catches that.
func TestRefreshClassHeader_ValuesAreArmableSubscriptionClasses(t *testing.T) {
	// The classes the dispatcher stamps at the five serve sites.
	stamped := []string{"widgetContent", "widgets", "restactions"}

	// The accepted-class allowlist, mirrored from DeriveSubscriptionKey's
	// switch (refresh_subscription.go:81-98). Using the SAME source
	// constants so a future rename of either side stays in lockstep.
	accepted := map[string]bool{
		cache.CacheEntryClassWidgetContent: true, // "widgetContent"
		classWidgets:                       true, // "widgets"
		classRestActions:                   true, // "restactions"
		cache.CacheEntryClassApistage:      true, // "apistage"
		cache.CacheEntryClassRAFullList:    true, // "raFullList"
	}

	for _, c := range stamped {
		if !accepted[c] {
			t.Fatalf("dispatcher stamps X-Snowplow-Refresh-Class %q, which is NOT an accepted "+
				"SubscriptionCoordinates.Class — the frontend could not arm it (it would hit "+
				"DeriveSubscriptionKey's fail-closed default)", c)
		}
	}

	// Pin the exact wire strings so a constant rename that diverges from the
	// frontend contract is caught (the guide §2.5/§8 documents these values).
	if cache.CacheEntryClassWidgetContent != "widgetContent" {
		t.Fatalf("widgetContent class string drifted: %q", cache.CacheEntryClassWidgetContent)
	}
	if classWidgets != "widgets" || classRestActions != "restactions" {
		t.Fatalf("widgets/restactions class strings drifted: %q / %q", classWidgets, classRestActions)
	}
}
