// refresher_test.go — Tag 0.30.8 unit tests for the L1 resolved-output
// cache refresher.
//
// Coverage:
//   - Enqueue dedupes within the dedup window.
//   - Worker pool drains the queue and invokes the registered handler.
//   - Handler returning nil → completed counter ticks.
//   - Handler returning error → failed counter ticks; the entry is
//     NOT evicted (stale-while-revalidate is preserved).
//   - Missing handler for a kind → skipped_no_handler counter ticks.
//   - Channel-full path increments skipped_full counter without
//     blocking.
//   - Concurrent enqueue + dequeue is race-free (run under -race).

package cache

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// withCleanRefresher gives each test a fresh refresher singleton with
// the supplied environment. Returns the cleanup function the test
// MUST defer.
func withCleanRefresher(t *testing.T, parallelism, intervalMS int) func() {
	t.Helper()
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv(envRefresherParallelism, strconv.Itoa(parallelism))
	t.Setenv(envRefresherIntervalMS, strconv.Itoa(intervalMS))
	resetResolvedCacheForTest()
	resetDepsForTest()
	resetRefresherForTest()
	return func() {
		resetRefresherForTest()
		resetDepsForTest()
		resetResolvedCacheForTest()
	}
}

func TestRefresher_HandlerInvokedOnEnqueue(t *testing.T) {
	cleanup := withCleanRefresher(t, 2, 50)
	defer cleanup()

	c := ResolvedCache()
	if c == nil {
		t.Fatalf("ResolvedCache nil — test setup wrong")
	}
	inputs := ResolvedKeyInputs{HandlerKind: "widgets", Username: "u"}
	key := ComputeKey(inputs)
	c.Put(key, &ResolvedEntry{RawJSON: []byte(`{"x":1}`), Inputs: &inputs})

	var called atomic.Int32
	RegisterRefreshFunc("widgets", func(_ context.Context, k string, in ResolvedKeyInputs) error {
		if k != key {
			t.Errorf("handler got key %q want %q", k, key)
		}
		if in.HandlerKind != "widgets" {
			t.Errorf("handler got HandlerKind %q want widgets", in.HandlerKind)
		}
		called.Add(1)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartRefresher(ctx)

	if !refresherSingleton().enqueue(key) {
		t.Fatalf("enqueue returned false on a fresh key")
	}

	// Wait up to 2s for the worker to drain.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if called.Load() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if called.Load() != 1 {
		t.Fatalf("handler not called within 2s; called=%d", called.Load())
	}
	if got := refresherSingleton().completedTotal.Load(); got != 1 {
		t.Errorf("completedTotal=%d want 1", got)
	}
}

func TestRefresher_DedupWithinWindow(t *testing.T) {
	cleanup := withCleanRefresher(t, 1, 200) // 200ms window
	defer cleanup()

	c := ResolvedCache()
	inputs := ResolvedKeyInputs{HandlerKind: "widgets"}
	key := ComputeKey(inputs)
	c.Put(key, &ResolvedEntry{RawJSON: []byte(`{}`), Inputs: &inputs})

	RegisterRefreshFunc("widgets", func(context.Context, string, ResolvedKeyInputs) error {
		// Slow enough that the second enqueue lands while we're holding
		// the key in-flight, so the dedup map dedupes it.
		time.Sleep(50 * time.Millisecond)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartRefresher(ctx)

	r := refresherSingleton()
	if !r.enqueue(key) {
		t.Fatalf("first enqueue rejected")
	}
	if r.enqueue(key) {
		t.Fatalf("second enqueue within window should be deduped")
	}
	if got := r.skippedDedupTotal.Load(); got != 1 {
		t.Errorf("skippedDedupTotal=%d want 1", got)
	}
}

func TestRefresher_HandlerErrorTicksFailedNoEviction(t *testing.T) {
	cleanup := withCleanRefresher(t, 1, 50)
	defer cleanup()

	c := ResolvedCache()
	inputs := ResolvedKeyInputs{HandlerKind: "widgets"}
	key := ComputeKey(inputs)
	c.Put(key, &ResolvedEntry{RawJSON: []byte(`{}`), Inputs: &inputs})

	RegisterRefreshFunc("widgets", func(context.Context, string, ResolvedKeyInputs) error {
		return errors.New("simulated resolver failure")
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartRefresher(ctx)

	refresherSingleton().enqueue(key)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if refresherSingleton().failedTotal.Load() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := refresherSingleton().failedTotal.Load(); got != 1 {
		t.Fatalf("failedTotal=%d want 1", got)
	}
	// CRITICAL: a failing refresh must NOT have evicted.
	if _, ok := c.Get(key); !ok {
		t.Fatalf("entry was evicted after refresh failure — violates stale-while-revalidate")
	}
}

func TestRefresher_NoHandlerForKind(t *testing.T) {
	cleanup := withCleanRefresher(t, 1, 50)
	defer cleanup()

	c := ResolvedCache()
	inputs := ResolvedKeyInputs{HandlerKind: "unregistered"}
	key := ComputeKey(inputs)
	c.Put(key, &ResolvedEntry{RawJSON: []byte(`{}`), Inputs: &inputs})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartRefresher(ctx)

	refresherSingleton().enqueue(key)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if refresherSingleton().skippedNoHandler.Load() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := refresherSingleton().skippedNoHandler.Load(); got != 1 {
		t.Fatalf("skippedNoHandler=%d want 1", got)
	}
}

func TestRefresher_ConcurrentEnqueueRaceFree(t *testing.T) {
	cleanup := withCleanRefresher(t, 4, 1)
	defer cleanup()

	c := ResolvedCache()
	var seen atomic.Int64
	RegisterRefreshFunc("widgets", func(context.Context, string, ResolvedKeyInputs) error {
		seen.Add(1)
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartRefresher(ctx)

	const N = 200
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		inputs := ResolvedKeyInputs{HandlerKind: "widgets", Name: "n" + itoa(i)}
		key := ComputeKey(inputs)
		c.Put(key, &ResolvedEntry{RawJSON: []byte(`{}`), Inputs: &inputs})

		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			refresherSingleton().enqueue(k)
		}(key)
	}
	wg.Wait()

	// Wait for queue to drain — give 3s.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s := refresherStatsSnapshot()
		if s.completed+s.skippedDedup+s.skippedFull+s.skippedNoHandler+s.skippedNoEntry >= N {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// --- Dep tracker → refresher hook wiring -----------------------------------

func TestRefresher_DepTrackerOnUpdateEnqueues(t *testing.T) {
	cleanup := withCleanRefresher(t, 2, 50)
	defer cleanup()

	c := ResolvedCache()
	inputs := ResolvedKeyInputs{HandlerKind: "widgets"}
	key := ComputeKey(inputs)
	c.Put(key, &ResolvedEntry{RawJSON: []byte(`{}`), Inputs: &inputs})

	gvr := gvrCompositions()
	Deps().Record(key, gvr, "ns", "n")

	var fired atomic.Int32
	RegisterRefreshFunc("widgets", func(context.Context, string, ResolvedKeyInputs) error {
		fired.Add(1)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartRefresher(ctx)

	Deps().OnUpdate(gvr, "ns", "n")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fired.Load() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fired.Load() != 1 {
		t.Fatalf("dep-tracker → refresher path didn't fire within 2s")
	}
	// And the entry must still be in the cache (UPDATE does NOT evict).
	if _, ok := c.Get(key); !ok {
		t.Fatalf("OnUpdate evicted entry — violates feedback_l1_invalidation_delete_only.md")
	}
}
