// refresher_apistage_falsifier_test.go — Ship 0.30.117 pre-flight
// falsifier for the AC-E3 defect found validating Ship E (0.30.116).
//
// THE DEFECT: RegisterRefreshHandlers (dispatchers.go) registered the
// refresh handler only for "restactions" and "widgets" — the "apistage"
// handler kind was NEVER registered. An apistage-kind L1 entry pulled
// off the refresher queue therefore hits a nil handler, the refresher
// counts skippedNoHandler, and the entry is silently TTL-only — never
// refreshed.
//
//   FA1 (registry) — after RegisterRefreshHandlers, the handler registry
//        MUST carry an entry for cache.CacheEntryClassApistage. FAILS
//        pre-fix: RefreshFuncForTest(apistage) returns nil — the
//        refresher counts skippedNoHandler for every apistage key.
//
// NOTE (Ship F1, 0.30.119): the original FA2 — an end-to-end refresh of
// a PER-STAGE apistage entry through a stubbed resolveOnceFn — was
// removed when F1 reshaped the apistage L1 from the per-stage model to
// the per-K8s-call CONTENT model. FA2 asserted the Ship E
// apistageHitValue `{value:...}` shape, which F1's raw-apiserver-envelope
// content entry does not produce. The F1 falsifier
// (apistage_content_falsifier_test.go) covers the content-refresh path
// end-to-end against the real stage loop. FA1 — the handler-registry
// assertion — is model-independent and stays.

package dispatchers

import (
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// TestFalsifierFA1_ApistageHandlerRegistered asserts the registry-level
// defect directly: RegisterRefreshHandlers MUST register a RefreshFunc
// for cache.CacheEntryClassApistage alongside "restactions" and "widgets".
//
// FAILS pre-fix: RegisterRefreshHandlers omits the apistage kind, so
// RefreshFuncForTest(apistage) returns nil — the refresher would count
// skippedNoHandler for every apistage key.
func TestFalsifierFA1_ApistageHandlerRegistered(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	cache.ResetDepsForTest()
	cache.ResetResolvedCacheForTest()
	cache.ResetRefresherForTest()
	t.Cleanup(func() {
		cache.ResetRefresherForTest()
		cache.ResetResolvedCacheForTest()
		cache.ResetDepsForTest()
	})

	RegisterRefreshHandlers(nil)

	// The two pre-Ship-E kinds must still be registered (no regression).
	if cache.RefreshFuncForTest("restactions") == nil {
		t.Fatalf("FA1: restactions RefreshFunc unregistered — regression")
	}
	if cache.RefreshFuncForTest("widgets") == nil {
		t.Fatalf("FA1: widgets RefreshFunc unregistered — regression")
	}
	// THE DEFECT: the apistage kind must be registered.
	if cache.RefreshFuncForTest(cache.CacheEntryClassApistage) == nil {
		t.Fatalf("FA1: no RefreshFunc registered for kind=%q — an apistage L1 "+
			"entry off the refresher queue hits a nil handler -> skippedNoHandler "+
			"-> never refreshed (AC-E3 defect)", cache.CacheEntryClassApistage)
	}
}
