// cohort_postjq_metrics.go — Ship Phase B / 0.30.185: per-cohort post-jq
// encoded-bytes cache observability + capacity controls.
//
// EXTENDS the Ship A.2 / 0.30.178 cohort gate memo with a new sub-cache:
// the post-jq value bytes per (cohort × stage-id × jqID). Where Ship A.2's
// permitAll fast-path cached the LIST envelope bytes, Phase B caches the
// JQ-evaluated stage output — eliminating the gojq run, the
// listEnvelopeValue rebuild, and the CopyJSONMap deep-copy from the warm
// hit path.
//
// COUNTERS (per HG-PB.12)
//
//   - snowplow_cohort_postjq_entries_total — int64 aggregate cohort-postjq
//     entries currently held across every live ResolvedEntry.CohortGates
//     memo. Walks the resolved-cache store at scrape time and sums each
//     memo's postJQ size. Bumped on lazy populate.
//
//   - snowplow_cohort_postjq_bytes_total — uint64 cumulative bytes
//     populated into postJQ entries (raw post-jq JSON). Monotonic
//     non-decreasing for the pod lifetime; ops use the rate-of-change as
//     a memory-pressure correlation signal.
//
//   - snowplow_cohort_postjq_hits_total — uint64 cumulative hit count.
//     Bumped on every successful postJQ lookup that bypasses gojq.
//
//   - snowplow_cohort_postjq_misses_total — uint64 cumulative miss count.
//     Bumped on every lookup that finds no entry and triggers compute.
//
//   - snowplow_cohort_postjq_size_histogram_*  — int64 buckets for
//     per-entry size distribution at populate time. Used to derive
//     HG-PB.14 empirical per-entry cost. Bucket boundaries (≤1MiB,
//     ≤4MiB, ≤16MiB, ≤64MiB, >64MiB) chosen to span the avg-24.59-MiB
//     pre-flight observation with at least one bucket per decade.
//
//   - snowplow_cohort_postjq_capacity_skips_total — uint64 count of
//     entries that exceeded the per-entry or aggregate cap and were
//     dropped (not cached). Lets ops correlate cap pressure with hit-
//     rate decay.
//
// CAPACITY CAP (per HG-PB.14)
//
//   COHORT_POSTJQ_CAP_BYTES — per-entry cap in bytes. Default 64 MiB
//   (conservative — avg pre-flight 24.59 MiB; the bucket histogram
//   informs post-deploy tuning). Aggregate cap is per-pod 4 GiB, NOT
//   env-tunable — the pod RSS budget is the load-bearing constraint and
//   this aggregate guard exists to refuse runaway growth on a single
//   identity's exhaustive cohort fan-out.
//
// AGGREGATE GRANULARITY: no per-cohort breakdown — matches the existing
// Ship A.2 / 0.30.178 cohort gate memo metrics. Per-cohort visibility is
// available from the populate slog lines.
//
// CACHE_ENABLED=false GATE: same as cohort_gate_memo_metrics — Disabled()
// returns true => no expvar.Publish call, no counters registered.

package cache

import (
	"expvar"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
)

// Per-entry cap defaults: 64 MiB. The brief documents avg pre-flight
// per-entry size at 24.59 MiB; the 64 MiB conservative default leaves
// headroom for the long-tail and gets refined post-deploy from the
// histogram + populate slog lines.
const cohortPostJQDefaultCapBytes uint64 = 64 * 1024 * 1024

// Aggregate cap: 4 GiB across all postJQ entries. Hard-coded — not env-
// tunable. Aggregate growth above this threshold is treated as a
// runaway-cohort signal and dropped.
const cohortPostJQAggregateCapBytes uint64 = 4 * 1024 * 1024 * 1024

// cohortPostJQEntries is the live total. Bumped on store, decremented on
// memo eviction (which happens implicitly when the parent ResolvedEntry
// is LRU-evicted — see the scrape-time walker for the canonical count).
// Kept here for a fast-read counter that doesn't require walking the
// resolved-cache store.
var cohortPostJQEntries atomic.Int64

// cohortPostJQBytesTotal is the cumulative bytes-populated counter.
// Monotonic non-decreasing.
var cohortPostJQBytesTotal atomic.Uint64

// cohortPostJQBytesLive is the current bytes-held counter. Decremented
// on memo eviction — but with zero-bound storage (Ship A.2) we have no
// eviction hook, so this stays monotonic in practice. Kept here as the
// aggregate-cap predicate's source of truth: the aggregate guard
// inspects this counter and refuses growth above
// cohortPostJQAggregateCapBytes.
var cohortPostJQBytesLive atomic.Uint64

// cohortPostJQHits / cohortPostJQMisses — hit/miss accounting.
var cohortPostJQHits atomic.Uint64
var cohortPostJQMisses atomic.Uint64

// cohortPostJQCapacitySkips counts entries dropped because they exceeded
// the per-entry or aggregate cap.
var cohortPostJQCapacitySkips atomic.Uint64

// Per-entry size histogram counters. Five buckets aligned to the
// pre-flight cohort_memo distribution (avg 24.59 MiB) — see file header.
var (
	cohortPostJQHistLe1MiB   atomic.Int64
	cohortPostJQHistLe4MiB   atomic.Int64
	cohortPostJQHistLe16MiB  atomic.Int64
	cohortPostJQHistLe64MiB  atomic.Int64
	cohortPostJQHistGt64MiB  atomic.Int64
)

// RecordCohortPostJQHit bumps the hit counter. Called by the lookup site
// when a postJQ entry is found and the gojq run is bypassed.
func RecordCohortPostJQHit() {
	cohortPostJQHits.Add(1)
}

// RecordCohortPostJQMiss bumps the miss counter. Called by the lookup
// site when no entry exists for the (cohort, stage-id, jqID) tuple.
func RecordCohortPostJQMiss() {
	cohortPostJQMisses.Add(1)
}

// RecordCohortPostJQStore bumps the entries + bytes counters AND the
// per-entry size histogram. Called by the populate site after a
// successful Store.
func RecordCohortPostJQStore(bytes int) {
	if bytes <= 0 {
		// Empty result is a valid cache entry — count it as one entry,
		// zero bytes. Per the brief's "Empty result" rule.
		cohortPostJQEntries.Add(1)
		cohortPostJQHistLe1MiB.Add(1)
		return
	}
	cohortPostJQEntries.Add(1)
	cohortPostJQBytesTotal.Add(uint64(bytes))
	cohortPostJQBytesLive.Add(uint64(bytes))

	switch {
	case bytes <= 1*1024*1024:
		cohortPostJQHistLe1MiB.Add(1)
	case bytes <= 4*1024*1024:
		cohortPostJQHistLe4MiB.Add(1)
	case bytes <= 16*1024*1024:
		cohortPostJQHistLe16MiB.Add(1)
	case bytes <= 64*1024*1024:
		cohortPostJQHistLe64MiB.Add(1)
	default:
		cohortPostJQHistGt64MiB.Add(1)
	}
}

// RecordCohortPostJQCapacitySkip bumps the capacity-skip counter. The
// per-entry size that triggered the skip is logged at the populate site
// (slog), not aggregated here — the counter is the rate-of-change
// signal; the slog has the per-event detail.
func RecordCohortPostJQCapacitySkip() {
	cohortPostJQCapacitySkips.Add(1)
}

// CohortPostJQBytesLive returns the current bytes-held aggregate.
// Exposed for the populate site's aggregate-cap predicate. Lock-free.
func CohortPostJQBytesLive() uint64 {
	return cohortPostJQBytesLive.Load()
}

// CohortPostJQAggregateCapBytes returns the hard-coded 4 GiB aggregate
// cap. Exposed for the populate site + tests.
func CohortPostJQAggregateCapBytes() uint64 {
	return cohortPostJQAggregateCapBytes
}

// CohortPostJQCapBytes returns the per-entry cap, read once per call
// from COHORT_POSTJQ_CAP_BYTES env. Default 64 MiB on absent / unparse-
// able env. Re-reading the env per call is fine — this is on the
// populate path, not the hit path, and env reads are cheap.
//
// The cap is in BYTES (decimal int). A zero or negative value reverts
// to the default; we never disable the per-entry guard.
func CohortPostJQCapBytes() uint64 {
	v := os.Getenv("COHORT_POSTJQ_CAP_BYTES")
	if v == "" {
		return cohortPostJQDefaultCapBytes
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil || n == 0 {
		return cohortPostJQDefaultCapBytes
	}
	return n
}

// cohortPostJQMetricsOnce guards expvar.Publish calls — duplicate keys
// panic, sync.Once prevents that across init() and test helpers.
var cohortPostJQMetricsOnce sync.Once

func init() {
	if Disabled() {
		return
	}
	registerCohortPostJQMetrics()
}

func registerCohortPostJQMetrics() {
	cohortPostJQMetricsOnce.Do(func() {
		expvar.Publish("snowplow_cohort_postjq_entries_total", expvar.Func(func() any {
			return cohortPostJQEntries.Load()
		}))
		expvar.Publish("snowplow_cohort_postjq_bytes_total", expvar.Func(func() any {
			return cohortPostJQBytesTotal.Load()
		}))
		expvar.Publish("snowplow_cohort_postjq_bytes_live", expvar.Func(func() any {
			return cohortPostJQBytesLive.Load()
		}))
		expvar.Publish("snowplow_cohort_postjq_hits_total", expvar.Func(func() any {
			return cohortPostJQHits.Load()
		}))
		expvar.Publish("snowplow_cohort_postjq_misses_total", expvar.Func(func() any {
			return cohortPostJQMisses.Load()
		}))
		expvar.Publish("snowplow_cohort_postjq_capacity_skips_total", expvar.Func(func() any {
			return cohortPostJQCapacitySkips.Load()
		}))
		expvar.Publish("snowplow_cohort_postjq_size_hist_le_1mib", expvar.Func(func() any {
			return cohortPostJQHistLe1MiB.Load()
		}))
		expvar.Publish("snowplow_cohort_postjq_size_hist_le_4mib", expvar.Func(func() any {
			return cohortPostJQHistLe4MiB.Load()
		}))
		expvar.Publish("snowplow_cohort_postjq_size_hist_le_16mib", expvar.Func(func() any {
			return cohortPostJQHistLe16MiB.Load()
		}))
		expvar.Publish("snowplow_cohort_postjq_size_hist_le_64mib", expvar.Func(func() any {
			return cohortPostJQHistLe64MiB.Load()
		}))
		expvar.Publish("snowplow_cohort_postjq_size_hist_gt_64mib", expvar.Func(func() any {
			return cohortPostJQHistGt64MiB.Load()
		}))
	})
}
