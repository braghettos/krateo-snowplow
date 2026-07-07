// prewarm_engine_queue_testhelpers_test.go — F.4 / R1 test shims.
//
// The engine's queue moved from a hand-rolled pending-map+signal-channel to a
// client-go workqueue.TypedRateLimitingInterface[prewarmScope] (F.4 §3.1 R1).
// The old white-box tests inspected e.pending / e.dequeueScope directly; these
// shims re-express that intent against the workqueue so the regression fences
// stay meaningful without every test hand-driving Get/Done/Forget.
//
// F4-C7: existing FIX-F/#99b/shape-A falsifiers keep their behavioural
// assertions; only the queue-internal plumbing they touch is migrated here.

package dispatchers

import "k8s.io/client-go/util/workqueue"

// newTestEngine constructs a prewarmEngine with a fresh rate-limiting workqueue
// (same construction as prewarmEngineSingleton, but not the process singleton so
// each test is isolated). Replaces the old `&prewarmEngine{pending:..., signal:...}`
// literal.
func newTestEngine() *prewarmEngine {
	return &prewarmEngine{
		queue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[prewarmScope](),
		),
		yieldPoll: defaultEngineYieldPoll,
	}
}

// pendingLenForTest reports the queue's current depth (the migrated
// len(e.pending)). Rate-limited (AddRateLimited) items in backoff are NOT yet
// in the queue proper, so this counts only ready items — same as the old map,
// which only held immediately-ready scopes.
func (e *prewarmEngine) pendingLenForTest() int { return e.queue.Len() }

// drainScopeForTest pops one ready scope FIFO, marking it Done (so the queue
// permits its re-add later). Returns ok=false when the queue is empty. Replaces
// the old dequeueScope() pop. NOTE: unlike the old map, the workqueue blocks on
// Get with no ready item, so callers must ensure at least one item is queued
// (or use pendingLenForTest first) — every migrated call site already does.
func (e *prewarmEngine) drainScopeForTest() (prewarmScope, bool) {
	if e.queue.Len() == 0 {
		return prewarmScope{}, false
	}
	s, shutdown := e.queue.Get()
	if shutdown {
		return prewarmScope{}, false
	}
	e.queue.Done(s)
	return s, true
}

// pendingHasBootForTest reports whether a boot scope is currently queued. The
// workqueue has no key-peek, so this drains-and-reinstates: it pops every ready
// item, checks for the boot key, then re-Adds them all (order-preserving for
// the assertion's purpose — the tests that use this only check presence, not
// order). Done is called per item so the re-Add is accepted.
func (e *prewarmEngine) pendingHasBootForTest() bool {
	bootKey := prewarmScope{kind: scopeKindBoot}.key()
	n := e.queue.Len()
	found := false
	popped := make([]prewarmScope, 0, n)
	for i := 0; i < n; i++ {
		s, shutdown := e.queue.Get()
		if shutdown {
			break
		}
		e.queue.Done(s)
		if s.key() == bootKey {
			found = true
		}
		popped = append(popped, s)
	}
	for _, s := range popped {
		e.queue.Add(s)
	}
	return found
}
