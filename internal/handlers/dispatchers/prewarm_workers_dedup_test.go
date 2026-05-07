// Q-COLD-1 Option F (snowplow 0.25.309) — unit tests for the
// bind-identity dedup at PrewarmWorkerPool job-intake.
//
// What this validates:
//
//   - Two users sharing one binding-identity cohort produce ONE prewarm
//     walk (the second is skipped as a cohort-dup, accounted in
//     `usersSkippedAsCohortDup`).
//   - When `RBACWatcher` is nil (pre-Option-F fallback) or returns "",
//     the dedup degenerates to per-username — distinct users still
//     prewarm independently, no false sharing.
//   - A NOVEL binding-identity arriving AFTER an unrelated cohort has
//     finished still fires its own prewarm — the `seenCohorts` skip is
//     keyed by bid, not "any prior cohort".
//   - Race-clean under `-race` with mixed-cohort concurrent enqueues.
//
// Test strategy: as in `prewarm_workers_test.go`, configure
// `EntryPoints=nil` so `runPerUser` is a microsecond-fast no-op. We
// then observe the cohort counters published by `processOne`.
//
// Out-of-scope corner-case integration assertions (Q-COLD-1 PM gate
// brief, Corner A and Corner B) are documented at the end of this file
// — they require live RBAC informers and HTTP-time refilter plumbing
// that is exercised separately by the e2e/bench harness.
package dispatchers

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// stubRBACWatcherFor returns an RBACWatcher with its identityCache
// pre-populated so `CachedBindingIdentity(username, groups)` returns
// `identityByUser[username]` without needing live informers. Users not
// pre-populated will fall through to ComputeBindingIdentity which (with
// synced=false) returns "" — so callers must populate every user they
// expect to dedup.
func stubRBACWatcherFor(t *testing.T, identityByUser map[string]string) *cache.RBACWatcher {
	t.Helper()
	rw := cache.NewRBACWatcher(nil, nil)
	for u, id := range identityByUser {
		rw.InjectBindingIdentityForTest(u, nil, id)
	}
	return rw
}

// newDedupTestPool is `newTestPool` plus an injected `RBACWatcher` so
// `processOne` can compute a real bid at job-intake. EntryPoints is
// still nil so the walk itself remains a no-op.
func newDedupTestPool(workers, queueCap int, rw *cache.RBACWatcher) *PrewarmWorkerPool {
	return &PrewarmWorkerPool{
		Workers:     workers,
		QueueCap:    queueCap,
		Cache:       cache.NewMem(time.Hour),
		AuthnNS:     "krateo-system",
		JobTimeout:  500 * time.Millisecond,
		RBACWatcher: rw,
	}
}

// waitForCondition polls `cond` until it returns true or `timeout`
// elapses. Used in place of bare time.Sleep to keep the tests fast on
// fast hardware while still tolerant on slow CI.
func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// Test_R5_DedupByBindingIdentity — three users sharing one binding
// identity produce ONE prewarm walk; the other two are skipped as
// cohort dups. Pinned counters:
//
//	cohortCount                 == 1
//	representativeUsersProcessed == 1
//	usersSkippedAsCohortDup     == 2
//	processed                   == 1  (skip path returns BEFORE Add)
func Test_R5_DedupByBindingIdentity(t *testing.T) {
	rw := stubRBACWatcherFor(t, map[string]string{
		"alice":   "cohort-A",
		"bob":     "cohort-A",
		"charlie": "cohort-A",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := newDedupTestPool(1, 16, rw) // 1 worker → strictly serial, no race
	p.Start(ctx)

	for _, u := range []string{"alice", "bob", "charlie"} {
		if !p.Enqueue(PrewarmJob{Username: u}) {
			t.Fatalf("enqueue %q dropped", u)
		}
	}

	// Wait for all three to either process (1) or skip (2).
	ok := waitForCondition(t, drainTimeout, func() bool {
		return p.cohortCount.Load()+p.usersSkippedAsCohortDup.Load() == 3
	})
	if !ok {
		t.Fatalf("dedup path did not see all 3 inputs: cohort=%d skipped=%d processed=%d",
			p.cohortCount.Load(), p.usersSkippedAsCohortDup.Load(), p.processed.Load())
	}

	if got := p.cohortCount.Load(); got != 1 {
		t.Errorf("cohortCount: got %d, want 1", got)
	}
	if got := p.representativeUsersProcessed.Load(); got != 1 {
		t.Errorf("representativeUsersProcessed: got %d, want 1", got)
	}
	if got := p.usersSkippedAsCohortDup.Load(); got != 2 {
		t.Errorf("usersSkippedAsCohortDup: got %d, want 2", got)
	}
	if got := p.processed.Load(); got != 1 {
		t.Errorf("processed: got %d, want 1 (skip path must NOT increment processed)", got)
	}
}

// Test_R5_FallbackToUsername — when RBACWatcher is nil, bid degenerates
// to job.Username. Three distinct users each fire their own prewarm
// (no false sharing). Three users with the SAME username dedup
// (mirroring pre-Option-F behaviour).
func Test_R5_FallbackToUsername_DistinctUsers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := newDedupTestPool(1, 16, nil) // RBACWatcher = nil → fallback path
	p.Start(ctx)

	for _, u := range []string{"alice", "bob", "charlie"} {
		if !p.Enqueue(PrewarmJob{Username: u}) {
			t.Fatalf("enqueue %q dropped", u)
		}
	}
	if !waitForCondition(t, drainTimeout, func() bool {
		return p.cohortCount.Load() == 3
	}) {
		t.Fatalf("expected 3 distinct fallback cohorts, got %d", p.cohortCount.Load())
	}
	if got := p.usersSkippedAsCohortDup.Load(); got != 0 {
		t.Errorf("usersSkippedAsCohortDup: got %d, want 0 (distinct usernames must not dedup)", got)
	}
	if got := p.processed.Load(); got != 3 {
		t.Errorf("processed: got %d, want 3", got)
	}
}

// Test_R5_FallbackToUsername_SameUsername — duplicate enqueues for the
// same username under nil RBACWatcher MUST dedup (the fallback bid is
// the username itself, so seenCohorts/inflightUsers semantics apply).
func Test_R5_FallbackToUsername_SameUsername(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := newDedupTestPool(1, 16, nil)
	p.Start(ctx)

	for i := 0; i < 5; i++ {
		_ = p.Enqueue(PrewarmJob{Username: "alice"})
	}
	if !waitForCondition(t, drainTimeout, func() bool {
		return p.cohortCount.Load()+p.usersSkippedAsCohortDup.Load() == 5
	}) {
		t.Fatalf("did not see all 5 inputs settle: cohort=%d skipped=%d",
			p.cohortCount.Load(), p.usersSkippedAsCohortDup.Load())
	}
	if got := p.cohortCount.Load(); got != 1 {
		t.Errorf("cohortCount: got %d, want 1 (same-username fallback must dedup)", got)
	}
	if got := p.usersSkippedAsCohortDup.Load(); got != 4 {
		t.Errorf("usersSkippedAsCohortDup: got %d, want 4", got)
	}
}

// Test_R5_NewCohortMember — start with 1 cohort prewarmed; enqueue a
// novel-bid user. The new bid must NOT be skipped as cohort-dup; it
// fires its own walk, advancing cohortCount from 1 to 2.
func Test_R5_NewCohortMember(t *testing.T) {
	rw := stubRBACWatcherFor(t, map[string]string{
		"alice": "cohort-A",
		"bob":   "cohort-A",
		"dora":  "cohort-B", // novel bid
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := newDedupTestPool(1, 16, rw)
	p.Start(ctx)

	// Phase 1: prewarm cohort A via alice + bob.
	_ = p.Enqueue(PrewarmJob{Username: "alice"})
	_ = p.Enqueue(PrewarmJob{Username: "bob"})
	if !waitForCondition(t, drainTimeout, func() bool {
		return p.cohortCount.Load() == 1 && p.usersSkippedAsCohortDup.Load() == 1
	}) {
		t.Fatalf("phase 1 did not settle: cohort=%d skipped=%d",
			p.cohortCount.Load(), p.usersSkippedAsCohortDup.Load())
	}

	// Phase 2: novel bid arrives. Must NOT be skipped.
	_ = p.Enqueue(PrewarmJob{Username: "dora"})
	if !waitForCondition(t, drainTimeout, func() bool {
		return p.cohortCount.Load() == 2
	}) {
		t.Fatalf("novel cohort did not fire: cohort=%d skipped=%d",
			p.cohortCount.Load(), p.usersSkippedAsCohortDup.Load())
	}

	// Skipped MUST stay at 1 (only bob); dora was a fresh cohort.
	if got := p.usersSkippedAsCohortDup.Load(); got != 1 {
		t.Errorf("usersSkippedAsCohortDup: got %d, want 1 (novel cohort must not skip)", got)
	}
	if got := p.processed.Load(); got != 2 {
		t.Errorf("processed: got %d, want 2 (alice + dora)", got)
	}
}

// Test_R5_RaceClean fires concurrent enqueues for many cohorts under
// `-race`. The accounting invariant:
//
//	cohortCount + usersSkippedAsCohortDup == enqueued
//
// must hold strictly (no double-count, no lost update). With 4 cohorts
// of 25 users each (100 total), cohortCount must equal 4 and skipped
// must equal 96. Order of arrival does not change the totals — every
// user lands in exactly one of the two buckets.
func Test_R5_RaceClean(t *testing.T) {
	const cohorts = 4
	const usersPerCohort = 25
	identityByUser := make(map[string]string, cohorts*usersPerCohort)
	users := make([]string, 0, cohorts*usersPerCohort)
	for c := 0; c < cohorts; c++ {
		bid := fmt.Sprintf("cohort-%d", c)
		for u := 0; u < usersPerCohort; u++ {
			name := fmt.Sprintf("user-%d-%d", c, u)
			identityByUser[name] = bid
			users = append(users, name)
		}
	}

	rw := stubRBACWatcherFor(t, identityByUser)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := newDedupTestPool(8, 256, rw)
	p.Start(ctx)

	var wg sync.WaitGroup
	for _, u := range users {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			_ = p.Enqueue(PrewarmJob{Username: u})
		}(u)
	}
	wg.Wait()

	total := int64(cohorts * usersPerCohort)
	if !waitForCondition(t, 4*drainTimeout, func() bool {
		return p.cohortCount.Load()+p.usersSkippedAsCohortDup.Load() == total
	}) {
		t.Fatalf("race accounting did not settle: cohort=%d skipped=%d (want sum=%d)",
			p.cohortCount.Load(), p.usersSkippedAsCohortDup.Load(), total)
	}

	if got := p.cohortCount.Load(); got != cohorts {
		t.Errorf("cohortCount: got %d, want %d", got, cohorts)
	}
	if got := p.usersSkippedAsCohortDup.Load(); got != total-cohorts {
		t.Errorf("usersSkippedAsCohortDup: got %d, want %d", got, total-cohorts)
	}
	if got := p.processed.Load(); got != cohorts {
		t.Errorf("processed: got %d, want %d (one per cohort)", got, cohorts)
	}
}

// Test_R5_HeapStatsCarriesCohortCounters — after drain, the snapshot
// published via LoadPrewarmHeapStats() carries the cohort counters so
// /metrics/runtime can expose them. Pins the wire shape that the canary
// observer reads.
func Test_R5_HeapStatsCarriesCohortCounters(t *testing.T) {
	prewarmHeapStatsTestMu.Lock()
	defer prewarmHeapStatsTestMu.Unlock()
	resetPrewarmHeapStats()

	rw := stubRBACWatcherFor(t, map[string]string{
		"alice": "cohort-A",
		"bob":   "cohort-A",
		"dora":  "cohort-B",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := newDedupTestPool(2, 32, rw)
	p.Start(ctx)

	for _, u := range []string{"alice", "bob", "dora"} {
		_ = p.Enqueue(PrewarmJob{Username: u})
	}
	if !waitForCondition(t, drainTimeout, func() bool {
		return p.cohortCount.Load() == 2 && p.usersSkippedAsCohortDup.Load() == 1
	}) {
		t.Fatalf("walk did not settle before drain check")
	}

	// Wait for the drain quiet window so publishHeapStats fires.
	deadline := time.Now().Add(poolDrainQuietWindow + 2*time.Second)
	var stats *PrewarmHeapStats
	for time.Now().Before(deadline) {
		stats = LoadPrewarmHeapStats()
		if stats != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if stats == nil {
		t.Fatalf("snapshot never published — drain detector did not fire")
	}
	if stats.CohortCount != 2 {
		t.Errorf("snapshot.CohortCount: got %d, want 2", stats.CohortCount)
	}
	if stats.RepresentativeUsersProcessed != 2 {
		t.Errorf("snapshot.RepresentativeUsersProcessed: got %d, want 2",
			stats.RepresentativeUsersProcessed)
	}
	if stats.UsersSkippedAsCohortDup != 1 {
		t.Errorf("snapshot.UsersSkippedAsCohortDup: got %d, want 1",
			stats.UsersSkippedAsCohortDup)
	}
}

// ---------------------------------------------------------------------------
// Corner-case integration assertions (NOT exercised here, scoped per PM
// brief §1.2; documented as plumbing-required so the next harness pass
// covers them).
//
// Corner A — UAF-touching widgets MUST NOT pollute shared L1
//
//	The widget L1 write at internal/handlers/dispatchers/widgets.go:263
//	already gates `_ = c.SetResolvedRaw(...)` behind `!tracker.UAFTouching()`,
//	so UAF-touching widgets SKIP the L1 write entirely (per Q-RBAC-DECOUPLE
//	C(d) v4 §2.3 Fix-W). Option F changes which user's walk fills L1, but
//	does NOT change WHICH widgets get written — UAF-touching widgets still
//	fall through to the per-user HTTP-time path (Path D). Therefore Option F
//	is safe by construction: no widget that depends on per-user data lives
//	in the cohort-shared L1 in the first place.
//
//	The canary verification (PM gate "byte-equal content_match across 3
//	cohort members") is the integration-level proof: pick 3 users in one
//	cohort, fetch the same widget, assert bodies equal post-RBAC-refilter.
//	That path requires a real cluster + JWT mint and is owned by the
//	deploy step in the run plan, NOT by this test file.
//
// Corner B — RBAC drift inside the cohort window
//
//	The dedup key is the binding-identity at TIME OF ENQUEUE
//	(processOne computes it once, before LoadOrStore). If a CRB delta
//	moves user 50 to a new identity AFTER user 1 was already enqueued
//	with the old identity, two outcomes:
//
//	  (a) The watcher's `identityCache` is invalidated on the CRB
//	      change (`scheduleInvalidateFromBinding`), so user 50's NEXT
//	      enqueue computes the NEW bid → distinct cohort → fresh walk.
//	  (b) The audit `binding_identity_transition` fires inside
//	      `cache.CacheIdentity` when the new bid is observed at
//	      HTTP-time, providing the operator a paper trail.
//
//	Reproducing (a)+(b) in a unit test requires wiring rbac informer
//	state changes mid-flight, which lives in cache_test.go and
//	rbac_watcher_*_test.go integration suites, not here. The unit-level
//	invariant — bid is captured at intake and not re-evaluated mid-walk —
//	is structurally covered by the call-sequence in `processOne` and
//	pinned by Test_R5_NewCohortMember (a re-cohorted user appearing
//	with a NEW bid is treated as novel).
// ---------------------------------------------------------------------------
