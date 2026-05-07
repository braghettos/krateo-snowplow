// Q-COLD-1 PM gate G3 followup (snowplow 0.25.308) — unit tests for
// PrewarmWorkerPool heap-alloc instrumentation. The legacy sampler in
// WarmL1FromEntryPoints does not fire under PREWARM_MODE=event-driven;
// these tests cover the R5 worker-pool path that actually runs in
// production.
//
// Test strategy: drive the pool with EntryPoints=nil so runPerUser is a
// no-op (microsecond-fast) and assert the snapshot lifecycle:
//
//   1. LoadPrewarmHeapStats() returns nil before any pool starts.
//   2. After enqueue+drain+quiet-window, LoadPrewarmHeapStats() returns
//      non-nil with monotonic ordering: HeapStartBytes >= 0,
//      HeapPeakBytes >= HeapStartBytes, DurationMs > 0.
//   3. Race-clean under -race (single instrumentation goroutine, atomic
//      pointer publish, no shared mutable state with the workers).
//
// We do NOT assert specific MB numbers — the goal is presence + sanity
// of the snapshot, not a regression bound on heap (canary measurement
// owns the latter).
package dispatchers

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// withShortDrainWindow temporarily lowers the drain quiet window so the
// instrumentation fires within the test budget.
//
// Note: this is fragile against parallel tests that also touch the
// instrumentation. We serialise them with prewarmHeapStatsTestMu below.
var prewarmHeapStatsTestMu sync.Mutex

// resetPrewarmHeapStats is a test-only helper to clear the package
// global so each test starts from the documented "nil before first
// drain" state. Paired with the helper used by prewarm_l2_pagination_test.go.
func resetPrewarmHeapStats() {
	prewarmHeapStats.Store(nil)
}

// TestPool_HeapStats_NilBeforeStart asserts the prewarm.* block is
// absent from /metrics/runtime until the first drain completes.
//
// /metrics/runtime omits the "prewarm" key via omitempty when
// LoadPrewarmHeapStats() returns nil; this test pins that contract for
// the R5 path.
func TestPool_HeapStats_NilBeforeStart(t *testing.T) {
	prewarmHeapStatsTestMu.Lock()
	defer prewarmHeapStatsTestMu.Unlock()
	resetPrewarmHeapStats()

	if got := LoadPrewarmHeapStats(); got != nil {
		t.Fatalf("LoadPrewarmHeapStats before pool start must return nil; got %+v", got)
	}
}

// TestPool_HeapStats_PublishedAfterDrain enqueues N jobs against a
// no-op runPerUser, waits for the drain quiet-window to elapse, and
// asserts the snapshot is populated with monotonic ordering.
//
// drain quiet-window is 5s in production. We don't override it (the
// const is package-private) — instead we accept a longer test runtime
// and use a generous timeout. CI race-builds tolerate this.
func TestPool_HeapStats_PublishedAfterDrain(t *testing.T) {
	prewarmHeapStatsTestMu.Lock()
	defer prewarmHeapStatsTestMu.Unlock()
	resetPrewarmHeapStats()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := newTestPool(2, 32)
	p.Start(ctx)

	// Drive a small burst so processed > 0 by the time the drain
	// detector samples.
	const N = 8
	for i := 0; i < N; i++ {
		_ = p.Enqueue(PrewarmJob{Username: usernameFor(i)})
	}
	if got := waitForProcessed(t, p, N, 2*time.Second); got != N {
		t.Fatalf("processed: got %d, want %d (queue did not drain)", got, N)
	}

	// Wait for the drain quiet window + sampler granularity.
	// poolDrainQuietWindow=5s + 500ms drain-check tick + slack.
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
		t.Fatalf("LoadPrewarmHeapStats stayed nil after drain+quiet-window — instrumentation did not publish")
	}

	if stats.HeapStartBytes == 0 {
		t.Errorf("HeapStartBytes=0 — runtime.ReadMemStats should never return 0 in a running process")
	}
	if stats.HeapPeakBytes < stats.HeapStartBytes {
		t.Errorf("HeapPeakBytes (%d) < HeapStartBytes (%d) — peak must be monotonic over start",
			stats.HeapPeakBytes, stats.HeapStartBytes)
	}
	if stats.DurationMs <= 0 {
		t.Errorf("DurationMs=%d — must be positive after at least one sample tick", stats.DurationMs)
	}
}

// TestPool_HeapStats_NoPublishWhenZeroProcessed asserts the drain
// detector does not publish a snapshot when no users were ever
// enqueued. This matters because the canary observer reads "no prewarm
// block" as "pool not yet drained"; publishing an all-zero processed
// snapshot would wrongly look like a successful prewarm with 0 users.
//
// The instrumentation requires processed > 0 before declaring drain
// (see runHeapInstrumentation: drain criterion).
func TestPool_HeapStats_NoPublishWhenZeroProcessed(t *testing.T) {
	prewarmHeapStatsTestMu.Lock()
	defer prewarmHeapStatsTestMu.Unlock()
	resetPrewarmHeapStats()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := newTestPool(1, 16)
	p.Start(ctx)

	// Wait longer than the quiet window without enqueueing anything.
	// The detector must NOT publish.
	time.Sleep(poolDrainQuietWindow + 1*time.Second)

	if got := LoadPrewarmHeapStats(); got != nil {
		t.Errorf("LoadPrewarmHeapStats published a snapshot with zero processed jobs; got %+v", got)
	}
}

// TestPool_HeapStats_RaceClean fires concurrent enqueue + read of
// LoadPrewarmHeapStats from many goroutines while the instrumentation
// goroutine is sampling. -race must not flag any access to
// prewarmHeapStats or the heapPeak atomic.
func TestPool_HeapStats_RaceClean(t *testing.T) {
	prewarmHeapStatsTestMu.Lock()
	defer prewarmHeapStatsTestMu.Unlock()
	resetPrewarmHeapStats()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := &PrewarmWorkerPool{
		Workers:    4,
		QueueCap:   256,
		Cache:      cache.NewMem(time.Hour),
		AuthnNS:    "krateo-system",
		JobTimeout: 100 * time.Millisecond,
	}
	p.Start(ctx)

	var wg sync.WaitGroup
	// Producer: spam enqueues.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = p.Enqueue(PrewarmJob{Username: usernameFor(i)})
		}
	}()
	// Reader: spam LoadPrewarmHeapStats.
	wg.Add(1)
	go func() {
		defer wg.Done()
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			_ = LoadPrewarmHeapStats()
		}
	}()
	wg.Wait()
	// Drain so context cancel does not orphan the publisher.
	waitForProcessed(t, p, 200, drainTimeout)
}
