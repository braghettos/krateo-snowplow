// prewarm_complete_metric.go — Task #221 (snowplow half). Exposes the
// Phase1Done / prewarm-completion boundary via the expvar surface so the
// bench can poll /debug/vars for readiness instead of slog-scanning for
// the `phase1.warmup.completed` log line.
//
// EXPVAR NAME (load-bearing): `snowplow_prewarm_complete`. The bench dev
// codes against this exact key in parallel — do NOT rename.
//
// VALUE SHAPE: a map (expvar.Func → map[string]int64) under that single
// name, mirroring the established map-valued expvars in this codebase
// (snowplow_crd_discovery in crd_discovery_expvar.go;
// snowplow_phase1_cohort_seed_status in phase1_pip_metrics.go):
//
//	"done":       0 before the Phase1Done flip, 1 after. THE boundary the
//	             bench gates on (== 1 means /readyz has flipped to 200).
//	"elapsed_ms": process-start → Phase1Done wall-clock in ms. -1 until
//	             the flip. This is the cold-start-to-ready elapsed — the
//	             closest cheaply-available elapsed at the MarkPhase1Done
//	             site (the dispatcher's phase1.warmup.completed elapsed is
//	             local to phase1WarmupWith in another package and is NOT
//	             reachable here without new coupling; process-start→done
//	             is one time.Now() at package init + one atomic store in
//	             MarkPhase1Done — see markPhase1DoneObserved).
//
// WIRED AT: the canonical Phase1Done flip — cache.MarkPhase1Done()
// (phase1.go). That primitive (NOT the dispatcher's phase1.warmup.completed
// slog) is the single point every flip path funnels through:
//
//   - the dispatcher happy path (phase1_walk.go:717, Step 8),
//   - the cache-off / prewarm-off else branch (main.go:676),
//   - the readiness safety-net (main.go:753, ShouldFlipPhase1DoneOnStartup).
//
// Wiring the observation at MarkPhase1Done therefore captures the boundary
// on ALL paths; wiring it at phase1.warmup.completed would leave the
// expvar stuck at done=0 forever under cache-off (that slog never fires on
// the cache-off path), violating the boundary contract.
//
// PRIOR ART: refresher_metrics.go / schema_cache_metrics.go — the
// established sync.Once-guarded, Disabled()-gated, expvar.Func-lazy idiom.
// init() registers at process start; the Disabled() gate keeps the key off
// /debug/vars under CACHE_ENABLED=false (transparent-fallback contract,
// project_cache_off_is_transparent_fallback).

package cache

import (
	"expvar"
	"sync"
	"sync/atomic"
	"time"
)

// prewarmProcessStartNanos anchors the elapsed_ms denominator at process
// start (one time.Now() at package init). Captured unconditionally — it is
// a plain timestamp, not a registered surface, so the Disabled() gate does
// not apply to it (only to the expvar.Publish).
var prewarmProcessStartNanos = time.Now().UnixNano()

// phase1DoneNanos is the monotonic instant MarkPhase1Done first flipped the
// boundary. 0 until the first flip. Set once via CompareAndSwap so a
// second MarkPhase1Done call (the primitive is idempotent) does not move
// the recorded boundary timestamp.
var phase1DoneNanos atomic.Int64

// markPhase1DoneObserved records the Phase1Done flip instant for the
// elapsed_ms readout. Called from MarkPhase1Done (phase1.go) on every
// invocation; the CompareAndSwap makes only the FIRST flip stick, matching
// MarkPhase1Done's set-once production lifecycle. Safe for concurrent use.
func markPhase1DoneObserved() {
	phase1DoneNanos.CompareAndSwap(0, time.Now().UnixNano())
}

// resetPrewarmCompleteObservedForTest clears the recorded flip instant so a
// test driving MarkPhase1Done can re-observe the 0→1 elapsed transition.
// TEST-ONLY — production's lifecycle is set-once. Pairs with
// ResetPhase1DoneForTest (phase1.go).
func resetPrewarmCompleteObservedForTest() {
	phase1DoneNanos.Store(0)
}

// prewarmCompleteMetricOnce guards expvar.Publish so registration runs at
// most once per process even if both init() and the test helper invoke it.
// expvar.Publish panics on a duplicate key; sync.Once prevents that.
var prewarmCompleteMetricOnce sync.Once

func init() {
	// CFG-1 mirror (same gate as the other cache-side expvar publishers):
	// under CACHE_ENABLED=false the prewarm boundary is a no-op flip and
	// this key MUST NOT appear at /debug/vars.
	if Disabled() {
		return
	}
	registerPrewarmCompleteMetric()
}

// registerPrewarmCompleteMetric publishes the snowplow_prewarm_complete
// expvar. Guarded by prewarmCompleteMetricOnce so it is safe to call from
// init() and from RegisterPrewarmCompleteMetricForTest.
//
// The value is an expvar.Func evaluated lazily at scrape time — it reads
// IsPhase1Done() (the same atomic the /readyz handler consults) plus the
// recorded flip instant, so there is no per-/call cost and a pre-flip
// scrape simply reports done=0, elapsed_ms=-1.
func registerPrewarmCompleteMetric() {
	prewarmCompleteMetricOnce.Do(func() {
		expvar.Publish("snowplow_prewarm_complete", expvar.Func(func() any {
			out := map[string]int64{
				"done":       0,
				"elapsed_ms": -1,
			}
			if IsPhase1Done() {
				out["done"] = 1
				if done := phase1DoneNanos.Load(); done > 0 {
					out["elapsed_ms"] = (done - prewarmProcessStartNanos) / int64(time.Millisecond)
				}
			}
			return out
		}))
	})
}

// RegisterPrewarmCompleteMetricForTest forces the snowplow_prewarm_complete
// expvar registration under tests that flip CACHE_ENABLED=true via
// t.Setenv after init() already ran with CACHE_ENABLED unset. Idempotent
// (sync.Once-guarded). Production callers MUST NOT use this function.
func RegisterPrewarmCompleteMetricForTest() {
	registerPrewarmCompleteMetric()
}
