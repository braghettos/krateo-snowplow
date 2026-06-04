// prewarm_engine_test.go — Ship 1 unit tests for the prewarm engine's
// customer-priority yield + the bounded dedup queue. Package dispatchers
// so it can reach the unexported engine internals. Non-destructive.
//
// Ship 2 Stage 2 / 0.30.247 — adds scope-key dedup tests for the new
// scopeKindGVRDiscovered + its gvr payload.

package dispatchers

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
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

// ─────────────────────────────────────────────────────────────────────
// Ship 2 Stage 2 / 0.30.247 — scopeKindGVRDiscovered scope-key tests.
// ─────────────────────────────────────────────────────────────────────

// TestScopeKindGVRDiscovered_KeyDeterminism (RC-2 falsifier) asserts
// that enqueueing two scopes with the SAME gvr coalesces to ONE
// pending entry. The bounded dedup queue suppresses redundant
// re-prewarm work for the same discovered GVR (a discovery-storm of
// the same CRD must not amplify into N rePrewarm calls).
func TestScopeKindGVRDiscovered_KeyDeterminism(t *testing.T) {
	e := &prewarmEngine{
		pending: map[string]prewarmScope{},
		signal:  make(chan struct{}, 1),
	}
	gvr := schema.GroupVersionResource{
		Group:    "composition.krateo.io",
		Version:  "v1-2-2",
		Resource: "githubscaffoldingwithcompositionpages",
	}
	e.enqueueScope(prewarmScope{kind: scopeKindGVRDiscovered, gvr: gvr})
	e.enqueueScope(prewarmScope{kind: scopeKindGVRDiscovered, gvr: gvr})
	e.enqueueScope(prewarmScope{kind: scopeKindGVRDiscovered, gvr: gvr})

	if len(e.pending) != 1 {
		t.Fatalf("expected 1 pending entry after 3 enqueues of the same GVR, got %d", len(e.pending))
	}
	if e.enqueuedTotal.Load() != 3 {
		t.Fatalf("expected enqueuedTotal=3 (all attempts counted), got %d", e.enqueuedTotal.Load())
	}
	s, ok := e.dequeueScope()
	if !ok {
		t.Fatal("expected one scope dequeueable, got empty queue")
	}
	if s.kind != scopeKindGVRDiscovered {
		t.Fatalf("expected scopeKindGVRDiscovered, got %q", s.kind)
	}
	if s.gvr != gvr {
		t.Fatalf("expected gvr payload %+v, got %+v", gvr, s.gvr)
	}
	if _, ok := e.dequeueScope(); ok {
		t.Fatal("expected queue empty after drain")
	}
}

// TestScopeKindGVRDiscovered_KeyDistinct (RC-2 falsifier) asserts that
// enqueueing two scopes with DIFFERENT gvrs produces TWO pending
// entries (no false coalesce). Each unique discovered GVR must get
// its own re-prewarm — coalescing distinct GVRs would silently lose
// re-prewarm work for one of them.
func TestScopeKindGVRDiscovered_KeyDistinct(t *testing.T) {
	e := &prewarmEngine{
		pending: map[string]prewarmScope{},
		signal:  make(chan struct{}, 1),
	}
	gvrA := schema.GroupVersionResource{
		Group:    "composition.krateo.io",
		Version:  "v1-2-2",
		Resource: "githubscaffoldingwithcompositionpages",
	}
	gvrB := schema.GroupVersionResource{
		Group:    "composition.krateo.io",
		Version:  "v1-2-2",
		Resource: "anothercompositionkind",
	}
	e.enqueueScope(prewarmScope{kind: scopeKindGVRDiscovered, gvr: gvrA})
	e.enqueueScope(prewarmScope{kind: scopeKindGVRDiscovered, gvr: gvrB})

	if len(e.pending) != 2 {
		t.Fatalf("expected 2 pending entries for distinct GVRs, got %d (false coalesce)", len(e.pending))
	}

	// Both scopes must be dequeueable; collect them.
	got := map[schema.GroupVersionResource]struct{}{}
	for i := 0; i < 2; i++ {
		s, ok := e.dequeueScope()
		if !ok {
			t.Fatalf("expected 2 dequeueable scopes, got only %d", i)
		}
		if s.kind != scopeKindGVRDiscovered {
			t.Fatalf("expected scopeKindGVRDiscovered, got %q", s.kind)
		}
		got[s.gvr] = struct{}{}
	}
	if _, ok := got[gvrA]; !ok {
		t.Fatalf("missing gvrA from dequeued set: %+v", got)
	}
	if _, ok := got[gvrB]; !ok {
		t.Fatalf("missing gvrB from dequeued set: %+v", got)
	}
}

// TestScopeKindGVRDiscovered_KeyDistinctFromBoot asserts that
// scopeKindGVRDiscovered with a zero GVR is STILL distinct from
// scopeKindBoot. This protects against a future refactor that might
// collapse the two by accident.
func TestScopeKindGVRDiscovered_KeyDistinctFromBoot(t *testing.T) {
	bootKey := prewarmScope{kind: scopeKindBoot}.key()
	discKey := prewarmScope{kind: scopeKindGVRDiscovered}.key()
	if bootKey == discKey {
		t.Fatalf("boot key %q collides with gvr-discovered key %q", bootKey, discKey)
	}
}

// TestScopeKindGVRDiscovered_KeyFormat pins the key format so callers
// reading the engine's pending map by key string have a stable
// contract. Documented in the prewarmScope.key() comment.
func TestScopeKindGVRDiscovered_KeyFormat(t *testing.T) {
	gvr := schema.GroupVersionResource{
		Group:    "composition.krateo.io",
		Version:  "v1",
		Resource: "compositions",
	}
	want := "gvr-discovered|composition.krateo.io/v1, Resource=compositions"
	got := prewarmScope{kind: scopeKindGVRDiscovered, gvr: gvr}.key()
	if got != want {
		t.Fatalf("key format drift:\n  want: %q\n  got:  %q", want, got)
	}
}
