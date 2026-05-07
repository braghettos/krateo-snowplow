// Q-COHORT-PREWARM (v0.25.312) — unit tests for the EnqueueForUser /
// EnqueueForCohort lifts on PrewarmWorkerPool. Exercises the per-user
// resolver indirection (SetEnqueueByName / SetEnqueueByNameFunc) and
// the cohort fan-out counter without touching the K8s API.
//
// Test strategy: install a fake `enqueueByName` that records call
// arguments and decides whether to forward to the real Enqueue. This
// isolates the lift refactor (Q-COHORT-PREWARM §2.1) from secret
// reads, JWT minting, and the resolver pipeline.
package dispatchers

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// TestPool_EnqueueForUser_NoFnReturnsFalse confirms that EnqueueForUser
// is a safe no-op when SetEnqueueByName has not been called yet.
func TestPool_EnqueueForUser_NoFnReturnsFalse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newTestPool(1, 16)
	p.Start(ctx)
	if ok := p.EnqueueForUser(ctx, "alice"); ok {
		t.Errorf("EnqueueForUser before SetEnqueueByName should return false")
	}
}

// TestPool_EnqueueForUser_RejectsEmptyAndStopped covers the defensive
// guards: empty username and pre-Start / post-Stop pools.
func TestPool_EnqueueForUser_RejectsEmptyAndStopped(t *testing.T) {
	p := newTestPool(1, 16)
	p.SetEnqueueByNameFunc(func(ctx context.Context, username string) bool {
		t.Fatalf("fn must not be called when pool is not started")
		return false
	})
	if ok := p.EnqueueForUser(context.Background(), "alice"); ok {
		t.Errorf("EnqueueForUser before Start should return false")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	if ok := p.EnqueueForUser(ctx, ""); ok {
		t.Errorf("EnqueueForUser with empty username should return false")
	}
}

// TestPool_EnqueueForUser_PostsToQueue asserts the lift refactor: when
// the closure forwards to p.Enqueue, the job appears on the queue and
// the processed counter advances.
func TestPool_EnqueueForUser_PostsToQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := newTestPool(2, 32)
	p.Start(ctx)

	var calls atomic.Int64
	p.SetEnqueueByNameFunc(func(ctx context.Context, username string) bool {
		calls.Add(1)
		// Mirror the production closure: synthesise a minimal job and
		// forward to p.Enqueue. EntryPoints is empty so runPerUser is
		// a no-op (≈ µs per job).
		return p.Enqueue(PrewarmJob{Username: username})
	})

	if ok := p.EnqueueForUser(ctx, "alice"); !ok {
		t.Fatalf("EnqueueForUser returned false unexpectedly")
	}
	waitForProcessed(t, p, 1, drainTimeout)

	enq, proc, drop := p.Stats()
	if calls.Load() != 1 {
		t.Errorf("fn calls: got %d, want 1", calls.Load())
	}
	if enq != 1 {
		t.Errorf("enqueued: got %d, want 1", enq)
	}
	if proc != 1 {
		t.Errorf("processed: got %d, want 1", proc)
	}
	if drop != 0 {
		t.Errorf("dropped: got %d, want 0", drop)
	}
}

// TestPool_EnqueueForCohort_FanoutAndCounter exercises EnqueueForCohort
// with N usernames and confirms (a) all N forward to the closure,
// (b) CohortFanouts == N, (c) Enqueue's enqueued counter == N.
func TestPool_EnqueueForCohort_FanoutAndCounter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := newTestPool(4, 256)
	p.Start(ctx)

	var calls sync.Map
	p.SetEnqueueByNameFunc(func(ctx context.Context, username string) bool {
		calls.Store(username, struct{}{})
		return p.Enqueue(PrewarmJob{Username: username})
	})

	const N = 50
	usernames := make([]string, 0, N)
	for i := 0; i < N; i++ {
		usernames = append(usernames, usernameFor(i))
	}
	accepted, dropped := p.EnqueueForCohort(ctx, usernames)
	if accepted != N {
		t.Errorf("accepted: got %d, want %d", accepted, N)
	}
	if dropped != 0 {
		t.Errorf("dropped: got %d, want 0", dropped)
	}
	if got := p.CohortFanouts(); got != int64(N) {
		t.Errorf("CohortFanouts: got %d, want %d", got, N)
	}
	waitForProcessed(t, p, N, drainTimeout)
}

// TestPool_EnqueueForCohort_DedupsViaInflight confirms that firing the
// same cohort twice in quick succession does not double-process: the
// inflightUsers tryLock + the L1 Exists short-circuit (in production)
// dedups; the closure is still called both times but the queue work
// is idempotent.
//
// Here we don't exercise inflight directly (jobs are too fast); instead
// we assert the contract that EnqueueForCohort is safe to call with
// duplicate usernames within one slice — accepted counts each slot.
func TestPool_EnqueueForCohort_DuplicatesAreAccepted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newTestPool(2, 64)
	p.Start(ctx)
	p.SetEnqueueByNameFunc(func(ctx context.Context, username string) bool {
		return p.Enqueue(PrewarmJob{Username: username})
	})

	usernames := []string{"alice", "alice", "bob", "alice", "bob"}
	accepted, dropped := p.EnqueueForCohort(ctx, usernames)
	if accepted != len(usernames) {
		t.Errorf("accepted: got %d, want %d", accepted, len(usernames))
	}
	if dropped != 0 {
		t.Errorf("dropped: got %d, want 0", dropped)
	}
	if got := p.CohortFanouts(); got != int64(len(usernames)) {
		t.Errorf("CohortFanouts: got %d, want %d", got, len(usernames))
	}
}

// TestPool_EnqueueForCohort_StormDoesNotSaturate is the PM-mandated
// G-QUEUE-DRAIN gate (storm test). Fires 1004 cohort enqueues (matching
// the bench cluster's active-user count) and asserts:
//   - EnqueueForCohort returns within a reasonable bound (non-blocking).
//   - The pool drains the queue to 0 within the storm-test wall.
//
// With Workers=4, QueueCap=2048, EntryPoints=nil (so runPerUser is a
// no-op), this should drain in well under 1s. In production Workers=4
// drains real prewarm walks at ~5s each — the PM gate (5 min) maps to
// ~250 jobs/worker × 5s ≈ 21 min worst-case (architect §3.2). The unit
// test only proves the pool plumbing is non-blocking and finite under
// burst — the empirical 5-min bound is verified on the cluster (Step
// 9-10 of the run brief).
func TestPool_EnqueueForCohort_StormDoesNotSaturate(t *testing.T) {
	if testing.Short() {
		t.Skip("storm test (1004 fan-outs); skipping in -short")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := &PrewarmWorkerPool{
		Workers:    4,
		QueueCap:   2048,
		Cache:      cache.NewMem(time.Hour),
		AuthnNS:    "krateo-system",
		JobTimeout: 200 * time.Millisecond,
		// EntryPoints intentionally empty — runPerUser is a no-op.
	}
	p.Start(ctx)
	p.SetEnqueueByNameFunc(func(ctx context.Context, username string) bool {
		return p.Enqueue(PrewarmJob{Username: username})
	})

	const N = 1004
	usernames := make([]string, 0, N)
	for i := 0; i < N; i++ {
		usernames = append(usernames, usernameFor(i))
	}

	start := time.Now()
	accepted, dropped := p.EnqueueForCohort(ctx, usernames)
	enqueueElapsed := time.Since(start)

	// EnqueueForCohort must return promptly (non-blocking). On a typical
	// laptop this should be well under 100 ms even at 1004 entries.
	if enqueueElapsed > 5*time.Second {
		t.Errorf("EnqueueForCohort took too long: %v (must be non-blocking)", enqueueElapsed)
	}

	// All 1004 slots must be accepted (queue cap 2048 absorbs the burst)
	// and CohortFanouts must increment to exactly N for the accepted set.
	if accepted+dropped != N {
		t.Errorf("accepted+dropped: got %d, want %d", accepted+dropped, N)
	}
	// In this configuration we expect zero drops (queue is sized for it).
	if dropped != 0 {
		t.Errorf("dropped: got %d, want 0 (QueueCap=%d, N=%d)", dropped, p.QueueCap, N)
	}
	if got := p.CohortFanouts(); got != int64(accepted) {
		t.Errorf("CohortFanouts: got %d, want %d", got, accepted)
	}

	// Queue must drain fully within the storm timeout. With no-op
	// runPerUser this is sub-second.
	const stormDrainTimeout = 10 * time.Second
	waitForProcessed(t, p, int64(accepted), stormDrainTimeout)

	if depth := p.QueueDepth(); depth != 0 {
		t.Errorf("queue did not drain: depth=%d after %v", depth, stormDrainTimeout)
	}
}
