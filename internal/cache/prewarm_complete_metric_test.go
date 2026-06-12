// prewarm_complete_metric_test.go — Task #221 (snowplow half) unit tests.
//
// Asserts:
//   - registerPrewarmCompleteMetric is idempotent (a second call must NOT
//     panic on the duplicate expvar.Publish — the sync.Once guard is what
//     makes it safe to call from init() AND the test helper).
//   - The published snowplow_prewarm_complete expvar flips done 0→1 when
//     the PRODUCTION primitive MarkPhase1Done() fires, and reports a
//     non-negative elapsed_ms after the flip.
//   - Concurrent MarkPhase1Done() flips racing scrape reads are
//     data-race-free (run under -race): MarkPhase1Done is reachable from
//     several startup paths and the expvar.Func is read by the scrape
//     poller, so the observation primitive must be concurrency-safe.
//
// Package cache (internal test) so it can reach registerPrewarmCompleteMetric
// + resetPrewarmCompleteObservedForTest. Mirrors refresher_metrics_test.go.

package cache

import (
	"encoding/json"
	"expvar"
	"sync"
	"testing"
)

// TestRegisterPrewarmCompleteMetric_Idempotent — calling the registration
// helper twice must NOT panic. expvar.Publish panics on a duplicate key;
// the sync.Once guard inside registerPrewarmCompleteMetric is what makes
// the helper safe to call repeatedly (init() in production, the test helper
// here, and back-to-back below).
func TestRegisterPrewarmCompleteMetric_Idempotent(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("registerPrewarmCompleteMetric panicked on second call: %v", r)
		}
	}()
	registerPrewarmCompleteMetric()
	registerPrewarmCompleteMetric() // second call must be a no-op
}

// TestPrewarmCompleteMetric_FlipsViaProductionPrimitive — drives the
// production MarkPhase1Done() and confirms the published expvar reports
// done=1 + a non-negative elapsed_ms. The pre-flip state is asserted first
// so the 0→1 transition is observed end-to-end through the real primitive,
// not a test-only setter.
func TestPrewarmCompleteMetric_FlipsViaProductionPrimitive(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	registerPrewarmCompleteMetric()

	// Start from a clean boundary: clear Phase1Done AND the recorded flip
	// instant so this test owns the 0→1 transition regardless of test
	// ordering within the binary.
	ResetPhase1DoneForTest()
	resetPrewarmCompleteObservedForTest()
	t.Cleanup(func() {
		ResetPhase1DoneForTest()
		resetPrewarmCompleteObservedForTest()
	})

	got := expvar.Get("snowplow_prewarm_complete")
	if got == nil {
		t.Fatalf("snowplow_prewarm_complete not published")
	}

	// Pre-flip: done must be 0, elapsed_ms must be -1 (not yet flipped).
	pre := decodePrewarmComplete(t, got)
	if pre["done"] != 0 {
		t.Fatalf("pre-flip done = %d, want 0", pre["done"])
	}
	if pre["elapsed_ms"] != -1 {
		t.Fatalf("pre-flip elapsed_ms = %d, want -1", pre["elapsed_ms"])
	}

	// Flip via the PRODUCTION primitive — the same call the startup
	// sequence makes. This is the 0→1 boundary the bench gates on.
	MarkPhase1Done()

	post := decodePrewarmComplete(t, got)
	if post["done"] != 1 {
		t.Fatalf("post-flip done = %d, want 1 (MarkPhase1Done did not flip the expvar)", post["done"])
	}
	if post["elapsed_ms"] < 0 {
		t.Fatalf("post-flip elapsed_ms = %d, want >= 0 (process-start→done elapsed)", post["elapsed_ms"])
	}
}

// TestPrewarmCompleteMetric_ConcurrentFlipAndScrape — race detector
// coverage. MarkPhase1Done() is reachable from multiple startup goroutines
// (dispatcher Phase1Warmup, the cache-off else, the readiness safety-net)
// while the expvar.Func is read concurrently by the scrape poller. The
// observation primitive (markPhase1DoneObserved CAS + the atomic load in
// the Func) must be data-race-free. Run with -race.
func TestPrewarmCompleteMetric_ConcurrentFlipAndScrape(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	registerPrewarmCompleteMetric()

	ResetPhase1DoneForTest()
	resetPrewarmCompleteObservedForTest()
	t.Cleanup(func() {
		ResetPhase1DoneForTest()
		resetPrewarmCompleteObservedForTest()
	})

	got := expvar.Get("snowplow_prewarm_complete")
	if got == nil {
		t.Fatalf("snowplow_prewarm_complete not published")
	}

	const writers = 8
	const readers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})

	// Writers: concurrent idempotent flips (mirrors the several startup
	// paths that may each call MarkPhase1Done).
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 200; j++ {
				MarkPhase1Done()
			}
		}()
	}
	// Readers: concurrent scrapes via the published Func (String()
	// triggers the Func body — the production scrape path).
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 200; j++ {
				_ = got.String()
			}
		}()
	}

	close(start)
	wg.Wait()

	// After all flips, the boundary must read done=1 with a single
	// recorded instant (CAS-once → elapsed_ms is stable, not moved by the
	// later flips).
	final := decodePrewarmComplete(t, got)
	if final["done"] != 1 {
		t.Fatalf("final done = %d, want 1", final["done"])
	}
	if final["elapsed_ms"] < 0 {
		t.Fatalf("final elapsed_ms = %d, want >= 0", final["elapsed_ms"])
	}
}

// decodePrewarmComplete renders the published expvar to JSON and decodes
// the map. expvar.Func.String() calls json.Marshal on the returned
// map[string]int64 → a JSON object of decimal numbers.
func decodePrewarmComplete(t *testing.T, v expvar.Var) map[string]int64 {
	t.Helper()
	var m map[string]int64
	if err := json.Unmarshal([]byte(v.String()), &m); err != nil {
		t.Fatalf("decode snowplow_prewarm_complete %q: %v", v.String(), err)
	}
	return m
}
