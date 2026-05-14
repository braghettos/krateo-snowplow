// refresher.go — Tag 0.30.8: background re-resolve worker pool for L1
// resolved-output cache entries whose dep tuples saw an UPDATE/PATCH
// event.
//
// Per implementation plan §"Tag 0.30.8 What's implemented":
//
//   - Background refresher processes refresh requests from
//     deps.OnUpdate. Cadence: RESOLVED_CACHE_REFRESHER_INTERVAL_MS
//     (default 500 ms), used here as the per-key dedup window.
//   - Bounded parallelism (RESOLVED_CACHE_REFRESHER_PARALLELISM,
//     default 4) prevents storm.
//   - Refresher RE-RESOLVES, never evicts (preserves the
//     feedback_l1_invalidation_delete_only.md rule).
//   - Class-aware-priority-queue HOOK present but NOT activated at this
//     tag — class-blind FIFO until 0.30.11 wires the classifier.
//
// Architecture:
//
//   * `Enqueue(l1Key)` is the deps.OnUpdate hook. It adds l1Key to an
//     in-flight dedup map (sync.Map[l1Key]int64 — value is the unix
//     nano of the last enqueue). If the key was enqueued less than
//     dedupWindow ago, the call is silently dropped (counter
//     `refresh_skipped_dedup` increments). Otherwise the key is pushed
//     into a buffered channel that the worker pool drains.
//   * N workers (parallelism) each Loop: receive l1Key → load entry
//     from L1 store → dispatch resolver via the handler-kind registry
//     → write fresh entry back via Put → counter ++ → clear dedup map
//     bit.
//   * Resolver dispatch lives behind a callback registry
//     (RegisterRefreshFunc) populated by package
//     `internal/handlers/dispatchers` at process start. The cache
//     package cannot import the resolvers (would create a cycle), so
//     the dispatchers register a closure that knows how to drive its
//     resolver.
//   * If the registry has no handler for an entry's kind (e.g., legacy
//     entry pre-0.30.8 with a nil Inputs field), the refresh fires a
//     `refresh_skipped_no_handler` counter and exits without touching
//     the L1 entry — stale-while-revalidate fallback to TTL purge.
//
// Lifecycle: a single goroutine pool launched at process start by
// StartRefresher (called from main.go AFTER ResolvedCache() is built
// and AFTER the dispatchers register their refresh funcs). The pool
// exits cleanly on context cancellation.
//
// Concurrency:
//   * `enqueueCh` is a buffered channel sized at parallelism × 64 —
//     bounded so a storm doesn't blow process memory. Channel-full
//     drops the enqueue and increments `refresh_skipped_full`.
//   * `inFlight` is a sync.Map[l1Key]int64. Reading and writing both go
//     through Load/Store on the same key without holding any other
//     lock. Workers Delete the key from inFlight AFTER completing
//     refresh (or on a terminal error).

package cache

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Refresher env knobs.
const (
	envRefresherIntervalMS  = "RESOLVED_CACHE_REFRESHER_INTERVAL_MS"
	envRefresherParallelism = "RESOLVED_CACHE_REFRESHER_PARALLELISM"
	envRefresherQueueBuffer = "RESOLVED_CACHE_REFRESHER_QUEUE_BUFFER"

	defaultRefresherIntervalMS  = 500
	defaultRefresherParallelism = 4
	// Per-worker queue depth multiplier. queueBuf = parallelism × multiplier.
	// Default 64 → 4 workers × 64 = 256-deep queue. Plenty for steady
	// state; in burst the channel-full counter is the falsifier.
	defaultRefresherQueueBufferMultiplier = 64
)

// RefreshFunc is the callback the cache package invokes on a refresh.
// It MUST re-resolve the entry described by inputs and Put the fresh
// bytes back into the L1 store under the same cache key. The cache
// package supplies the matching key string for convenience.
//
// Implementations live in `internal/handlers/dispatchers` (one per
// handlerKind). The closure has access to the http.Request-equivalent
// context only via inputs — the refresh runs detached from any HTTP
// request and synthesises a context internally.
type RefreshFunc func(ctx context.Context, key string, inputs ResolvedKeyInputs) error

// refresher is the singleton worker pool. Constructed lazily by
// StartRefresher; one per process.
type refresher struct {
	parallelism  int
	dedupWindow  time.Duration
	enqueueCh    chan string
	inFlight     sync.Map // l1Key -> int64 (unix nano of last enqueue)
	handlersMu   sync.RWMutex
	handlers     map[string]RefreshFunc

	// Falsifier counters (atomic).
	enqueueTotal       atomic.Uint64
	skippedDedupTotal  atomic.Uint64
	skippedFullTotal   atomic.Uint64
	skippedNoEntryTotal atomic.Uint64
	skippedNoHandler   atomic.Uint64
	completedTotal     atomic.Uint64
	failedTotal        atomic.Uint64

	startedOnce sync.Once
	stopCh      chan struct{}
	// workersDone is closed by the workers as they exit so test
	// cleanup can wait for full shutdown before tearing the singleton
	// down. Bounded by sync.WaitGroup; production never reads it.
	workersWG sync.WaitGroup
}

var (
	refresherInstance *refresher
	refresherInit     sync.Once
)

// refresherSingleton returns the process-wide refresher, constructing
// it lazily.
func refresherSingleton() *refresher {
	refresherInit.Do(func() {
		parallelism := intFromEnv(envRefresherParallelism, defaultRefresherParallelism)
		if parallelism <= 0 {
			parallelism = defaultRefresherParallelism
		}
		intervalMS := intFromEnv(envRefresherIntervalMS, defaultRefresherIntervalMS)
		if intervalMS <= 0 {
			intervalMS = defaultRefresherIntervalMS
		}
		bufMul := intFromEnv(envRefresherQueueBuffer, defaultRefresherQueueBufferMultiplier)
		if bufMul <= 0 {
			bufMul = defaultRefresherQueueBufferMultiplier
		}
		refresherInstance = &refresher{
			parallelism: parallelism,
			dedupWindow: time.Duration(intervalMS) * time.Millisecond,
			enqueueCh:   make(chan string, parallelism*bufMul),
			handlers:    map[string]RefreshFunc{},
			stopCh:      make(chan struct{}),
		}
	})
	return refresherInstance
}

// RegisterRefreshFunc wires a refresh handler for handlerKind ("restactions",
// "widgets"). Safe to call multiple times; later calls replace the
// earlier wiring (used by tests + by hot-reload scenarios).
//
// MUST be called BEFORE StartRefresher so the worker pool sees a fully
// populated handler map on its first dequeue.
func RegisterRefreshFunc(handlerKind string, fn RefreshFunc) {
	r := refresherSingleton()
	r.handlersMu.Lock()
	r.handlers[handlerKind] = fn
	r.handlersMu.Unlock()
}

// StartRefresher launches the worker pool. Idempotent — repeated calls
// are no-ops (the second StartRefresher does NOT spawn more workers).
// The pool exits cleanly when ctx is canceled OR when StopRefresher is
// called.
func StartRefresher(ctx context.Context) {
	if !ResolvedCacheEnabled() {
		return
	}
	r := refresherSingleton()
	r.startedOnce.Do(func() {
		// Wire the dep tracker's update hook to point at our enqueue.
		Deps().SetRefreshHook(func(l1Key string) {
			r.enqueue(l1Key)
		})

		for i := 0; i < r.parallelism; i++ {
			r.workersWG.Add(1)
			go func(id int) {
				defer r.workersWG.Done()
				r.workerLoop(ctx, id)
			}(i)
		}

		slog.Info("refresher.started",
			slog.String("subsystem", "cache"),
			slog.Int("parallelism", r.parallelism),
			slog.Duration("dedup_window", r.dedupWindow),
			slog.Int("queue_buffer", cap(r.enqueueCh)),
		)
	})
}

// StopRefresher closes the stop channel. Safe to call multiple times.
// Used by tests; production lets the context-cancel path drive shutdown.
func StopRefresher() {
	r := refresherSingleton()
	select {
	case <-r.stopCh:
		// already stopped
	default:
		close(r.stopCh)
	}
}

// enqueue tries to add l1Key to the work channel. Returns true if the
// key was accepted, false if it was deduped or the channel was full.
//
// The dedup check is a sync.Map.LoadOrStore on (l1Key -> nowNanos). If
// a prior record exists AND it is younger than dedupWindow, the call
// is dropped. Otherwise we Store fresh and push to the channel.
func (r *refresher) enqueue(l1Key string) bool {
	if l1Key == "" {
		return false
	}
	now := time.Now().UnixNano()
	prevI, loaded := r.inFlight.LoadOrStore(l1Key, now)
	if loaded {
		prevNanos := prevI.(int64)
		if now-prevNanos < int64(r.dedupWindow) {
			r.skippedDedupTotal.Add(1)
			return false
		}
		// Old entry — refresh the timestamp so we don't dedup again
		// within the window starting now.
		r.inFlight.Store(l1Key, now)
	}
	select {
	case r.enqueueCh <- l1Key:
		r.enqueueTotal.Add(1)
		return true
	default:
		// Channel full — drop AND remove the inFlight stamp so a
		// later enqueue isn't blocked by a stale dedup record.
		r.inFlight.Delete(l1Key)
		r.skippedFullTotal.Add(1)
		return false
	}
}

// workerLoop is one of N worker goroutines. Each blocks on enqueueCh
// until a key arrives, then dispatches refresh. On ctx.Done() or
// stopCh close the loop exits.
func (r *refresher) workerLoop(ctx context.Context, workerID int) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("refresher.worker.panic",
				slog.String("subsystem", "cache"),
				slog.Int("worker_id", workerID),
				slog.Any("panic", rec),
			)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopCh:
			return
		case key := <-r.enqueueCh:
			r.processOne(ctx, key)
		}
	}
}

// processOne handles a single refresh: load the entry from L1, dispatch
// the registered handler for its kind, clear the in-flight stamp on
// completion. Errors are counted but never propagated — the next
// UPDATE will trigger a fresh enqueue.
func (r *refresher) processOne(ctx context.Context, key string) {
	defer r.inFlight.Delete(key)

	c := ResolvedCache()
	if c == nil {
		r.skippedNoEntryTotal.Add(1)
		return
	}
	entry, ok := c.Get(key)
	if !ok || entry == nil {
		// L1 may have evicted between OnUpdate and us picking up the
		// key. Stale-while-revalidate degrades to next-cold-miss for
		// this key; not an error.
		r.skippedNoEntryTotal.Add(1)
		return
	}
	if entry.Inputs == nil {
		// Legacy 0.30.7 entry — no Inputs to drive a re-resolve.
		// Skip silently; TTL will purge eventually.
		r.skippedNoHandler.Add(1)
		return
	}
	r.handlersMu.RLock()
	fn := r.handlers[entry.Inputs.HandlerKind]
	r.handlersMu.RUnlock()
	if fn == nil {
		r.skippedNoHandler.Add(1)
		return
	}
	if err := fn(ctx, key, *entry.Inputs); err != nil {
		r.failedTotal.Add(1)
		slog.Warn("refresher.refresh_failed",
			slog.String("subsystem", "cache"),
			slog.String("handler_kind", entry.Inputs.HandlerKind),
			slog.String("key_hash", key),
			slog.Any("err", err),
		)
		return
	}
	r.completedTotal.Add(1)
}

// refresherStatsSnapshot is the read-only snapshot the summary log
// consumes. Lives here rather than as a method on refresher because the
// summary log might fire before any RegisterRefreshFunc / StartRefresher
// call (so the singleton may legitimately be nil-init); resolved.go
// pulls a copy via this helper.
type refresherStats struct {
	enqueued     uint64
	completed    uint64
	failed       uint64
	skippedDedup uint64
	skippedFull  uint64
	skippedNoEntry uint64
	skippedNoHandler uint64
}

func refresherStatsSnapshot() refresherStats {
	r := refresherSingleton()
	if r == nil {
		return refresherStats{}
	}
	return refresherStats{
		enqueued:         r.enqueueTotal.Load(),
		completed:        r.completedTotal.Load(),
		failed:           r.failedTotal.Load(),
		skippedDedup:     r.skippedDedupTotal.Load(),
		skippedFull:      r.skippedFullTotal.Load(),
		skippedNoEntry:   r.skippedNoEntryTotal.Load(),
		skippedNoHandler: r.skippedNoHandler.Load(),
	}
}

// resetRefresherForTest tears the singleton down so each test sees a
// clean refresher. Exported only via the *_test.go shim — production
// code MUST NOT call this.
//
// CRITICAL: blocks until every worker goroutine has actually exited.
// Without this barrier, a worker mid-processOne can race with the
// next test's resetResolvedCacheForTest (data-race detected 0.30.8
// dev cycle).
func resetRefresherForTest() {
	if refresherInstance != nil {
		select {
		case <-refresherInstance.stopCh:
		default:
			close(refresherInstance.stopCh)
		}
		// Wait for workers to finish their current processOne call.
		// In practice this is <50ms (the slowest test handler sleeps
		// 50ms); we cap at 5s as a defensive deadline that should
		// never fire.
		done := make(chan struct{})
		go func() {
			refresherInstance.workersWG.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			// Worker stuck — defensive log; test will likely still
			// fail downstream because of corruption.
		}
	}
	refresherInstance = nil
	refresherInit = sync.Once{}
}
