package cache

import (
	"testing"
	"time"
)

// TestAPIResultCacheTTL_Is5Minutes pins the api-result L1 TTL to 5 minutes.
//
// Rationale (snowplow 0.25.311, 2026-05-07):
//   - 60s TTL caused 75% miss rate on iterator-fanned-out RESTAction calls
//     (compositions-list at 49K compositions), driving Resolve.func1.3 to
//     43.91% of CPU (architect-resolve-func13-attribution-2026-05-07.md §3 R1).
//   - PM gate (pm-ttl-fix-2026-05-07.md §1.4) lifts the constant to 5 minutes,
//     NOT to 1h, to preserve the SWR UPDATE-freshness bound mandated by
//     feedback_l1_invalidation_delete_only.md (UPDATE/PATCH events do NOT
//     evict api-result keys; natural TTL is the only mechanism that ages
//     in-place mutations).
//
// This test is intentionally brittle: any future change to APIResultCacheTTL
// must deliberately update this assertion AND go through the PM gate review.
// Do NOT relax it without re-reading the two analyses cited above.
func TestAPIResultCacheTTL_Is5Minutes(t *testing.T) {
	if APIResultCacheTTL != 5*time.Minute {
		t.Fatalf("APIResultCacheTTL = %v, want %v (5 minutes)", APIResultCacheTTL, 5*time.Minute)
	}
}
