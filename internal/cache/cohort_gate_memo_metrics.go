// cohort_gate_memo_metrics.go — Ship A.2 / 0.30.178: cohort gate memo
// observability counters.
//
// Three aggregate-only counters, exposed via expvar. Mirrors the existing
// pattern in fallthrough_meter_expvar.go (prior art).
//
// REGISTERED VIA init(). Same gate as the fallthrough meter: under
// CACHE_ENABLED=false the cache subsystem does not exist and these
// counters MUST NOT be registered (cache-off transparent-fallback
// contract — project_cache_off_is_transparent_fallback).
//
// COUNTERS
//
//   - snowplow_cohort_memo_entries_total — int64 aggregate across all
//     live ResolvedEntry.CohortGates stores. Walks the resolved-cache
//     store at scrape time and sums Size() per entry. Lock-free read
//     (atomic.Int64.Load per store + the cache's internal mutex held
//     only for the index walk).
//
//   - snowplow_cohort_memo_total_bytes — uint64 cumulative byte count
//     of memo payloads currently held. Bumped on Store() by the memo
//     populate site; NOT decremented (zero-bound: no per-store
//     eviction; releases ride the parent ResolvedEntry's LRU). Stays
//     monotonically non-decreasing for a given pod lifetime; ops use
//     the rate-of-change for memory pressure correlation.
//
//   - snowplow_cohort_memo_encoded_bytes_cached_total — uint64
//     cumulative count of bytes ENCODED into a memo's encodedJSON +
//     encodedGzip fields (permitAll path only). Same monotonic semantics
//     as total_bytes; lets ops distinguish encoded-cache vs keptNames-
//     only memo growth.
//
//   - snowplow_cohort_memo_overflow_total — uint64 aggregate count of
//     insertion-order evictions fired by the per-entry cap
//     (CACHE_COHORT_MEMO_CAP, Ship 3 / 0.30.197). Walks the resolved-
//     cache store at scrape time and sums OverflowTotal() per entry.
//     A non-zero, growing value signals a cohort-cardinality-pressured
//     content cell (per-user-binding RBAC topology); 0 at today's
//     ~34-cohort scale. Lock-free per-store read (atomic.Uint64.Load).
//
// AGGREGATE GRANULARITY (per Diego 2026-05-26 ratification): no
// per-cohort breakdown — the cohort space is bounded and operator-
// readable from the gate-memo log lines already emitted by
// apistage_cohort_memo.go.

package cache

import (
	"expvar"
	"sync"
	"sync/atomic"
)

// CohortGateMemoTotalBytes is the cumulative byte count of memo
// payloads ever stored. Bumped by the memo populate site (callers in
// api/apistage_cohort_memo.go) on each Store() with the encoded byte
// size of the memo's keptNames or encoded fields.
//
// Monotonic non-decreasing. Exposed via the
// snowplow_cohort_memo_total_bytes expvar.
var cohortGateMemoTotalBytes atomic.Uint64

// CohortGateMemoEncodedBytesCachedTotal counts bytes encoded into the
// permitAll fast-path's encodedJSON + encodedGzip fields. Lets ops
// distinguish encoded-cache vs keptNames-only memo growth.
var cohortGateMemoEncodedBytesCachedTotal atomic.Uint64

// RecordCohortMemoBytes bumps the cumulative total-bytes counter by n.
// Called by the memo populate site for EVERY memo regardless of
// permitAll/!permitAll branch.
//
// n must be the in-memory size of the memo payload (keptNames map
// header + encoded bytes). Callers may use unsafe.Sizeof + len(map) *
// per-entry estimate, or just len(encoded) on the permitAll path. The
// counter is not load-bearing for correctness; over- or under-counting
// by a fixed factor is acceptable so long as the rate-of-change tracks
// the populate rate.
func RecordCohortMemoBytes(n int) {
	if n <= 0 {
		return
	}
	cohortGateMemoTotalBytes.Add(uint64(n))
}

// RecordCohortMemoEncodedBytes bumps the cumulative encoded-bytes
// counter by n. Called by the memo populate site ONLY on the permitAll
// fast-path, with n = len(encodedJSON) + len(encodedGzip).
func RecordCohortMemoEncodedBytes(n int) {
	if n <= 0 {
		return
	}
	cohortGateMemoEncodedBytesCachedTotal.Add(uint64(n))
}

// cohortMemoEntriesTotal walks the live resolved-cache store and sums
// the Size() of every entry's attached CohortGateMemoStore. Used by the
// snowplow_cohort_memo_entries_total expvar Func — evaluated at scrape
// time.
//
// Cost: O(N entries) per scrape. The resolved cache caps at maxEntries
// (default 50K); the walk takes the store's mutex but each entry's
// Size() is a lock-free atomic.Int64.Load. Scrape rate is low (every
// 10-60s) so this is fine.
//
// Returns 0 when the resolved cache is not active (Disabled() / not
// yet initialised).
func cohortMemoEntriesTotal() int64 {
	c := resolvedCacheInstance
	if c == nil {
		return 0
	}
	var sum int64
	c.mu.Lock()
	for _, el := range c.index {
		item, ok := el.Value.(*lruItem)
		if !ok || item == nil || item.entry == nil {
			continue
		}
		if store := item.entry.CohortGates.Load(); store != nil {
			sum += store.Size()
		}
	}
	c.mu.Unlock()
	return sum
}

// cohortMemoOverflowTotal walks the live resolved-cache store and sums
// the OverflowTotal() of every entry's attached CohortGateMemoStore.
// Used by the snowplow_cohort_memo_overflow_total expvar Func —
// evaluated at scrape time. Same cost profile as cohortMemoEntriesTotal
// (O(N entries) under the store mutex; per-store read is a lock-free
// atomic.Uint64.Load). Returns 0 when the resolved cache is inactive.
func cohortMemoOverflowTotal() uint64 {
	c := resolvedCacheInstance
	if c == nil {
		return 0
	}
	var sum uint64
	c.mu.Lock()
	for _, el := range c.index {
		item, ok := el.Value.(*lruItem)
		if !ok || item == nil || item.entry == nil {
			continue
		}
		if store := item.entry.CohortGates.Load(); store != nil {
			sum += store.OverflowTotal()
		}
	}
	c.mu.Unlock()
	return sum
}

// cohortMemoMetricsOnce guards the expvar.Publish calls so they run at
// most once per process even if invoked from both init() and a test
// helper. expvar.Publish panics on duplicate key; sync.Once prevents
// that.
var cohortMemoMetricsOnce sync.Once

func init() {
	// CFG-1 mirror: under CACHE_ENABLED=false the cache subsystem does
	// not exist and these counters MUST NOT be registered.
	if Disabled() {
		return
	}
	registerCohortMemoMetrics()
}

// registerCohortMemoMetrics performs the expvar.Publish calls for the
// three counters. Guarded by cohortMemoMetricsOnce so it is safe to
// call from both init() and any future test helper.
func registerCohortMemoMetrics() {
	cohortMemoMetricsOnce.Do(func() {
		expvar.Publish("snowplow_cohort_memo_entries_total", expvar.Func(func() any {
			return cohortMemoEntriesTotal()
		}))
		expvar.Publish("snowplow_cohort_memo_total_bytes", expvar.Func(func() any {
			return cohortGateMemoTotalBytes.Load()
		}))
		expvar.Publish("snowplow_cohort_memo_encoded_bytes_cached_total", expvar.Func(func() any {
			return cohortGateMemoEncodedBytesCachedTotal.Load()
		}))
		expvar.Publish("snowplow_cohort_memo_overflow_total", expvar.Func(func() any {
			return cohortMemoOverflowTotal()
		}))
	})
}
