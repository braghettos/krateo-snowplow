// prewarm_engine_test.go — Ship 1 unit tests for the prewarm engine's
// customer-priority yield + the bounded dedup queue. Package dispatchers
// so it can reach the unexported engine internals. Non-destructive.

package dispatchers

import (
	"context"
	"testing"
	"time"
)

// TestEngineYieldsWhileCustomerInFlight asserts the engine's
// yieldToCustomer parks while a customer /call is marked in flight and
// returns promptly once the call completes.
func TestEngineYieldsWhileCustomerInFlight(t *testing.T) {
	// Reset the counter (other tests may have left it nonzero on failure).
	for customerInFlight() {
		customerInFlightCount.Add(-1)
	}

	e := &prewarmEngine{
		pending:   map[string]prewarmScope{},
		signal:    make(chan struct{}, 1),
		yieldPoll: 2 * time.Millisecond,
	}

	done := markCustomerInFlight()
	if !customerInFlight() {
		t.Fatal("expected customerInFlight true after mark")
	}

	yielded := make(chan struct{})
	go func() {
		e.yieldToCustomer(context.Background())
		close(yielded)
	}()

	// The yield must NOT return while the customer call is in flight.
	select {
	case <-yielded:
		t.Fatal("yieldToCustomer returned while a customer call was in flight")
	case <-time.After(30 * time.Millisecond):
	}

	// Clear the in-flight mark — the yield must return promptly.
	done()
	select {
	case <-yielded:
		// good
	case <-time.After(500 * time.Millisecond):
		t.Fatal("yieldToCustomer did not return after the customer call completed")
	}

	if e.yieldTotal.Load() == 0 {
		t.Fatal("expected yieldTotal > 0 (the engine parked at least once)")
	}
}

// TestEngineYieldNoOpWhenIdle asserts the yield is a fast no-op when no
// customer call is in flight (the steady-state path).
func TestEngineYieldNoOpWhenIdle(t *testing.T) {
	for customerInFlight() {
		customerInFlightCount.Add(-1)
	}
	e := &prewarmEngine{
		pending:   map[string]prewarmScope{},
		signal:    make(chan struct{}, 1),
		yieldPoll: time.Second,
	}
	start := time.Now()
	e.yieldToCustomer(context.Background())
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("idle yield took %v — should be a fast no-op", d)
	}
}

// TestEngineQueueDedup asserts enqueueing the same scope key twice
// coalesces to one pending entry, and dequeue drains it.
func TestEngineQueueDedup(t *testing.T) {
	e := &prewarmEngine{
		pending: map[string]prewarmScope{},
		signal:  make(chan struct{}, 1),
	}
	e.enqueueScope(prewarmScope{kind: scopeKindBoot})
	e.enqueueScope(prewarmScope{kind: scopeKindBoot})
	if len(e.pending) != 1 {
		t.Fatalf("expected 1 pending after dedup, got %d", len(e.pending))
	}
	if e.enqueuedTotal.Load() != 2 {
		t.Fatalf("expected enqueuedTotal=2 (both calls counted), got %d", e.enqueuedTotal.Load())
	}
	s, ok := e.dequeueScope()
	if !ok || s.kind != scopeKindBoot {
		t.Fatalf("expected boot scope dequeued, got %+v ok=%v", s, ok)
	}
	if _, ok := e.dequeueScope(); ok {
		t.Fatal("expected empty queue after drain")
	}
}

// TestEngineWorkerRunsScopeAfterYield asserts the worker runs the handler
// for an enqueued scope and that the customer-priority gate does not
// permanently block it (idle path).
func TestEngineWorkerRunsScopeAfterYield(t *testing.T) {
	for customerInFlight() {
		customerInFlightCount.Add(-1)
	}
	e := &prewarmEngine{
		pending:   map[string]prewarmScope{},
		signal:    make(chan struct{}, 1),
		yieldPoll: 2 * time.Millisecond,
	}
	ran := make(chan prewarmScope, 1)
	e.scopeHandler = func(_ context.Context, s prewarmScope) error {
		ran <- s
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.runWorker(ctx)

	e.enqueueScope(prewarmScope{kind: scopeKindBoot})
	select {
	case s := <-ran:
		if s.kind != scopeKindBoot {
			t.Fatalf("worker ran wrong scope: %+v", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not run the enqueued scope")
	}
}

// TestEngineScopeDoneFiresOnCompletion (S2) asserts the scopeDone callback
// fires the instant a scope completes — the mechanism that releases the
// boot background goroutine at actual completion instead of after the full
// pipGlobalTimeout.
func TestEngineScopeDoneFiresOnCompletion(t *testing.T) {
	for customerInFlight() {
		customerInFlightCount.Add(-1)
	}
	e := &prewarmEngine{
		pending:   map[string]prewarmScope{},
		signal:    make(chan struct{}, 1),
		yieldPoll: 2 * time.Millisecond,
	}
	e.scopeHandler = func(_ context.Context, _ prewarmScope) error { return nil }
	done := make(chan prewarmScope, 1)
	e.scopeDone = func(s prewarmScope, _ error) { done <- s }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.runWorker(ctx)

	e.enqueueScope(prewarmScope{kind: scopeKindBoot})
	select {
	case s := <-done:
		if s.kind != scopeKindBoot {
			t.Fatalf("scopeDone fired for wrong scope: %+v", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scopeDone did not fire on scope completion (S2 regression)")
	}
}
