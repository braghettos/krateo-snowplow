// deps_falsifier_test.go — Ship A (0.30.110) pre-flight falsifier F3.
//
// Team rule feedback_falsifier_first_before_ship: this test is written
// BEFORE the production fix and MUST fail against the unfixed 0.30.109
// objects.Get path.
//
//   F3 — objects.Get must record a dep edge. When objects.Get is invoked
//        under a cache.WithL1KeyContext ctx and serves an object from the
//        informer cache, it must call cache.Deps().Record(l1Key, gvr, ns,
//        name) so a later DELETE of that object invalidates the L1 entry.
//        FAILS today: get.go has zero Deps().Record calls.
//
// The negative leg (AC-R5 negative case) asserts that an objects.Get
// WITHOUT an L1 key in context records nothing — the no-record invariant.

package objects

import (
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// TestFalsifierF3_GetRecordsDepEdge invokes objects.Get under a
// WithL1KeyContext ctx for an object held in a synced informer. The
// served read MUST register an exact-object dep edge keyed by the
// context's L1 key.
//
// FAILS on 0.30.109: objects/get.go performs zero Deps().Record calls,
// so collectMatches finds nothing for the fetched object.
func TestFalsifierF3_GetRecordsDepEdge(t *testing.T) {
	resetServeCounters()
	// #57: pivot implicit under CACHE_ENABLED (set by newServeWatcher below).
	cache.ResetDepsForTest()

	newServeWatcher(t, newServeTestObject("default", "alpha", "marker-alpha"))

	const l1Key = "L1_widget_under_test"
	ctx := cache.WithL1KeyContext(serveAdminCtx(), l1Key)

	res := Get(ctx, serveTestRef("default", "alpha"))
	if res.Err != nil {
		t.Fatalf("F3: objects.Get unexpected Err: %v", res.Err)
	}
	if res.Unstructured == nil {
		t.Fatalf("F3: objects.Get returned nil object — informer serve did not fire")
	}

	// A dep edge for the fetched (gvr, ns, name) must now exist, keyed by
	// the context's L1 key.
	matched := cache.Deps().CollectMatchesForTest(serveTestGVR, "default", "alpha")
	if _, ok := matched[l1Key]; !ok {
		t.Fatalf("F3: objects.Get under WithL1KeyContext did not record a dep "+
			"edge for the fetched object; collectMatches=%v — get.go has no "+
			"Deps().Record call", matched)
	}
}

// TestFalsifierF3_GetNoKeyRecordsNothing is the AC-R5 negative leg: an
// objects.Get with NO L1 key in context must record nothing.
func TestFalsifierF3_GetNoKeyRecordsNothing(t *testing.T) {
	resetServeCounters()
	// #57: pivot implicit under CACHE_ENABLED (set by newServeWatcher below).
	cache.ResetDepsForTest()

	newServeWatcher(t, newServeTestObject("default", "beta", "marker-beta"))

	// Bare context — no WithL1KeyContext.
	res := Get(serveAdminCtx(), serveTestRef("default", "beta"))
	if res.Err != nil {
		t.Fatalf("F3-neg: objects.Get unexpected Err: %v", res.Err)
	}

	if got := cache.Deps().Stats().RecordTotal; got != 0 {
		t.Fatalf("F3-neg: objects.Get without an L1 key recorded %d edges, want 0", got)
	}
}

// TestACR5_CacheDisabledRecordsNothing — with CACHE_ENABLED=false the
// objects.Get dep-recording path must be a clean no-op even when the
// context carries an L1 key. The cache layer is provisional bridge code
// and CACHE_ENABLED=false must remain a fully-functional plain path.
func TestACR5_CacheDisabledRecordsNothing(t *testing.T) {
	resetServeCounters()
	t.Setenv("CACHE_ENABLED", "false")
	t.Setenv("RESOLVER_USE_INFORMER", "true") // #57: stale/retired flag — ignored under cache-off
	cache.ResetDepsForTest()

	// Even with an L1 key in context, cache-off must record nothing.
	ctx := cache.WithL1KeyContext(serveAdminCtx(), "L1_should_not_record")
	// The apiserver path fails at UserConfig (no endpoint) — that is the
	// expected cache-off behaviour; we only assert zero dep recording.
	_ = Get(ctx, serveTestRef("default", "alpha"))

	if got := cache.Deps().Stats().RecordTotal; got != 0 {
		t.Fatalf("AC-R5: CACHE_ENABLED=false recorded %d dep edges; want 0 "+
			"(cache-off must stay a clean no-op)", got)
	}
}
