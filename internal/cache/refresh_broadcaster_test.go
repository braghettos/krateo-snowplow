// refresh_broadcaster_test.go — Ship 1 (live-refresh-coherence, option A)
// cache-layer falsifiers. feedback_falsifier_first_before_ship: these are A's
// gate. Pure in-memory (in-process broadcaster); KUBECONFIG unset; NEVER
// touches ./internal/rbac.
//
// Covers:
//   9.5a — PublishRefresh provably unreachable under cache-off (hub nil,
//          counters stay 0; the §2.4 nil-path).
//   9.6  — slow-consumer never stalls the producer (-race; blocked sink, drop
//          counter climbs, OTHER subscribers still receive, publish returns
//          promptly).
//   9.8  — a dropped TERMINAL signal does not strand state: after the last
//          signal is dropped (saturated sink), the cell is still fresh and a
//          later read (modelling the frontend's 5s-throttle refetch) sees it
//          — the SSE signal is an optimisation over the throttle, never a
//          deadlock-on-drop replacement.
//   plus broadcaster mechanics: coalesce-per-key, keyRefs/HasRefreshSubscriber
//   refcount under arm/disarm/unsub, per-key routing (a sub only gets its own
//   armed keys).

package cache

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// withRefreshLayer enables the SSE layer for a test and resets the singleton +
// counters before and after. Mirrors withCleanRefresher's setenv discipline.
func withRefreshLayer(t *testing.T) {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	t.Setenv(envRefreshSSEEnabled, "")        // default ON
	t.Setenv(envRefreshCoalesceWindowMS, "0") // coalescing OFF by default so
	// per-signal tests are deterministic; coalesce test sets it explicitly.
	resetRefreshBroadcasterForTest()
	t.Cleanup(resetRefreshBroadcasterForTest)
}

// drainOne reads one value from ch with a timeout; returns (val, true) or
// ("", false) on timeout.
func drainOne(t *testing.T, ch <-chan string, d time.Duration) (string, bool) {
	t.Helper()
	select {
	case v, ok := <-ch:
		return v, ok
	case <-time.After(d):
		return "", false
	}
}

// --- 9.5a — cache-off: PublishRefresh provably unreachable -------------------

// TestRefreshBroadcaster_CacheOff_PublishUnreachable asserts that with the
// cache subsystem off, refreshHub() returns nil and PublishRefresh is a no-op:
// the broadcaster counters NEVER move. This is the §2.4 / falsifier-9.5a
// nil-path — the belt-and-braces guard complementing resolve_populate's
// pre-Put early return.
func TestRefreshBroadcaster_CacheOff_PublishUnreachable(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false") // master off
	resetRefreshBroadcasterForTest()
	t.Cleanup(resetRefreshBroadcasterForTest)

	if RefreshSSEEnabled() {
		t.Fatalf("RefreshSSEEnabled()=true under CACHE_ENABLED=false — gate broken")
	}
	if h := refreshHub(); h != nil {
		t.Fatalf("refreshHub() != nil under cache-off — want nil (the §2.4 transparent no-op path)")
	}

	// Direct PublishRefresh under cache-off must be a no-op.
	PublishRefresh("any-key")
	PublishRefresh("any-key")
	pub, del, drop, coal := RefreshBroadcasterCounters()
	if pub|del|drop|coal != 0 {
		t.Fatalf("counters moved under cache-off: published=%d delivered=%d dropped=%d coalesced=%d — PublishRefresh is NOT unreachable",
			pub, del, drop, coal)
	}

	// Subscribe under cache-off returns a closed channel (idle stream) + a
	// no-op unsub — the handler degrades to heartbeat-only.
	ch, unsub := SubscribeRefresh(map[string]struct{}{"k": {}})
	defer unsub()
	if _, ok := <-ch; ok {
		t.Fatalf("SubscribeRefresh under cache-off returned an OPEN channel — want a closed/idle channel")
	}
	if HasRefreshSubscriber("k") {
		t.Fatalf("HasRefreshSubscriber true under cache-off — want false")
	}
}

// TestRefreshBroadcaster_ToggleOff_PublishUnreachable asserts the per-feature
// REFRESH_SSE_ENABLED=false back-out knob also makes the layer a no-op while
// the cache itself stays on (provisional/removable, project_caching_is_provisional).
func TestRefreshBroadcaster_ToggleOff_PublishUnreachable(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	t.Setenv(envRefreshSSEEnabled, "false") // feature off, cache on
	resetRefreshBroadcasterForTest()
	t.Cleanup(resetRefreshBroadcasterForTest)

	if RefreshSSEEnabled() {
		t.Fatalf("RefreshSSEEnabled()=true under REFRESH_SSE_ENABLED=false")
	}
	if refreshHub() != nil {
		t.Fatalf("refreshHub() != nil under REFRESH_SSE_ENABLED=false")
	}
	PublishRefresh("k")
	if pub, del, drop, coal := RefreshBroadcasterCounters(); pub|del|drop|coal != 0 {
		t.Fatalf("counters moved with feature toggled off: %d/%d/%d/%d", pub, del, drop, coal)
	}
}

// --- per-key routing + delivery ---------------------------------------------

// TestRefreshBroadcaster_PerKeyRouting asserts a subscriber receives ONLY the
// keys it armed, and that an unarmed key is not delivered to it.
func TestRefreshBroadcaster_PerKeyRouting(t *testing.T) {
	withRefreshLayer(t)

	chA, unsubA := SubscribeRefresh(map[string]struct{}{"key-A": {}})
	defer unsubA()
	chB, unsubB := SubscribeRefresh(map[string]struct{}{"key-B": {}})
	defer unsubB()

	PublishRefresh("key-A")
	if v, ok := drainOne(t, chA, time.Second); !ok || v != "key-A" {
		t.Fatalf("A did not receive its armed key-A: got %q ok=%v", v, ok)
	}
	// B must NOT receive key-A (per-key routing).
	if v, ok := drainOne(t, chB, 150*time.Millisecond); ok {
		t.Fatalf("B received %q for key-A — per-key routing leaked", v)
	}

	PublishRefresh("key-B")
	if v, ok := drainOne(t, chB, time.Second); !ok || v != "key-B" {
		t.Fatalf("B did not receive its armed key-B: got %q ok=%v", v, ok)
	}
	// An entirely unsubscribed key delivers to nobody.
	PublishRefresh("key-UNSUB")
	if v, ok := drainOne(t, chA, 150*time.Millisecond); ok {
		t.Fatalf("A received unsubscribed key: %q", v)
	}
}

// --- keyRefs / HasRefreshSubscriber refcount --------------------------------

// TestRefreshBroadcaster_KeyRefsRefcount asserts the per-key subscriber
// reverse-index refcounts correctly across Subscribe / ArmKey / DisarmKey /
// unsub, and that HasRefreshSubscriber reflects it (the §12.3 O(1) presence
// check that A maintains for a later ship to consume).
func TestRefreshBroadcaster_KeyRefsRefcount(t *testing.T) {
	withRefreshLayer(t)

	if HasRefreshSubscriber("shared") {
		t.Fatalf("HasRefreshSubscriber(shared) true before any subscribe")
	}

	_, unsub1 := SubscribeRefresh(map[string]struct{}{"shared": {}})
	if !HasRefreshSubscriber("shared") {
		t.Fatalf("HasRefreshSubscriber(shared) false after 1 subscriber")
	}
	if n := RefreshSubscriberCount(); n != 1 {
		t.Fatalf("subscriber count=%d want 1", n)
	}

	// Second subscriber arms the same key -> refcount 2.
	_, unsub2 := SubscribeRefresh(map[string]struct{}{"shared": {}})
	if n := RefreshSubscriberCount(); n != 2 {
		t.Fatalf("subscriber count=%d want 2", n)
	}

	// Drop one — still armed by the other.
	unsub1()
	if !HasRefreshSubscriber("shared") {
		t.Fatalf("HasRefreshSubscriber(shared) false after 1 of 2 unsub — refcount underflow")
	}
	// Drop the other — now nobody is armed.
	unsub2()
	if HasRefreshSubscriber("shared") {
		t.Fatalf("HasRefreshSubscriber(shared) true after all unsub — keyRefs leak")
	}
	if n := RefreshSubscriberCount(); n != 0 {
		t.Fatalf("subscriber count=%d want 0 after all unsub", n)
	}

	// Idempotent unsub must not underflow.
	unsub1()
	unsub2()
	if HasRefreshSubscriber("shared") {
		t.Fatalf("HasRefreshSubscriber(shared) true after idempotent re-unsub")
	}
}

// TestRefreshBroadcaster_ArmDisarmLive asserts a live connection can add/remove
// keys without reconnecting, and delivery follows the armed set.
func TestRefreshBroadcaster_ArmDisarmLive(t *testing.T) {
	withRefreshLayer(t)

	// Subscribe with no keys, then arm one live.
	ch, unsub := SubscribeRefresh(map[string]struct{}{})
	defer unsub()

	// Reach the live *refreshSub to call Arm/Disarm (the handler holds it;
	// the test reaches it through the hub for the unit assertion).
	h := refreshHub()
	h.mu.RLock()
	var s *refreshSub
	for _, sub := range h.subs {
		s = sub
	}
	h.mu.RUnlock()
	if s == nil {
		t.Fatalf("no live sub found")
	}

	s.ArmKey("late-key")
	if !HasRefreshSubscriber("late-key") {
		t.Fatalf("ArmKey did not register late-key in keyRefs")
	}
	PublishRefresh("late-key")
	if v, ok := drainOne(t, ch, time.Second); !ok || v != "late-key" {
		t.Fatalf("did not receive late-armed key: got %q ok=%v", v, ok)
	}

	s.DisarmKey("late-key")
	if HasRefreshSubscriber("late-key") {
		t.Fatalf("DisarmKey did not clear late-key from keyRefs")
	}
	PublishRefresh("late-key")
	if v, ok := drainOne(t, ch, 150*time.Millisecond); ok {
		t.Fatalf("received %q after DisarmKey", v)
	}
}

// --- coalesce ----------------------------------------------------------------

// TestRefreshBroadcaster_CoalescePerKey asserts that N publishes for the SAME
// key within the coalesce window collapse to ONE fan-out (design §2.3), while
// a publish for a DIFFERENT key in the same window is NOT coalesced.
func TestRefreshBroadcaster_CoalescePerKey(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	t.Setenv(envRefreshSSEEnabled, "")
	t.Setenv(envRefreshCoalesceWindowMS, "500") // 500ms window
	resetRefreshBroadcasterForTest()
	t.Cleanup(resetRefreshBroadcasterForTest)

	ch, unsub := SubscribeRefresh(map[string]struct{}{"k1": {}, "k2": {}})
	defer unsub()

	// Burst of 5 publishes for k1 inside the window -> 1 delivered, 4 coalesced.
	for i := 0; i < 5; i++ {
		PublishRefresh("k1")
	}
	// k2 once -> delivered (different key, not coalesced against k1).
	PublishRefresh("k2")

	// Collect what arrives within a short drain.
	got := map[string]int{}
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		v, ok := drainOne(t, ch, 50*time.Millisecond)
		if !ok {
			continue
		}
		got[v]++
	}
	if got["k1"] != 1 {
		t.Fatalf("k1 delivered %d times in coalesce window — want exactly 1 (burst not collapsed)", got["k1"])
	}
	if got["k2"] != 1 {
		t.Fatalf("k2 delivered %d times — want 1 (different key wrongly coalesced)", got["k2"])
	}
	_, _, _, coalesced := RefreshBroadcasterCounters()
	if coalesced != 4 {
		t.Fatalf("coalesced counter=%d want 4 (the 4 suppressed k1 re-emits)", coalesced)
	}
}

// --- 9.6 — slow consumer never stalls the producer (-race) ------------------

// TestRefreshBroadcaster_SlowConsumerNeverStalls is falsifier 9.6. Run under
// -race. A subscriber whose sink is never drained must NOT block PublishRefresh:
// once its buffer (cap refreshSubChanCap) fills, further sends DROP (counter
// climbs) and the producer returns promptly. A second, healthy subscriber must
// keep receiving throughout. Concurrent publishers + a concurrent (blocked)
// reader exercise the RWMutex fan-out path for the race detector.
func TestRefreshBroadcaster_SlowConsumerNeverStalls(t *testing.T) {
	withRefreshLayer(t)

	// Slow sink: subscribe, never read from chSlow.
	chSlow, unsubSlow := SubscribeRefresh(map[string]struct{}{"hot": {}})
	defer unsubSlow()
	_ = chSlow // deliberately never drained

	// Healthy sink on the same key.
	chFast, unsubFast := SubscribeRefresh(map[string]struct{}{"hot": {}})
	defer unsubFast()

	var fastReceived atomic.Int64
	fastDone := make(chan struct{})
	go func() {
		defer close(fastDone)
		for {
			select {
			case _, ok := <-chFast:
				if !ok {
					return
				}
				fastReceived.Add(1)
			case <-time.After(2 * time.Second):
				return // no more signals
			}
		}
	}()

	// Fire far more than the slow sink's buffer can hold, from several
	// goroutines (race coverage on the fan-out RLock).
	const publishers = 4
	const perPublisher = 200 // 800 >> refreshSubChanCap (64)
	publishStart := time.Now()
	var wg sync.WaitGroup
	for p := 0; p < publishers; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perPublisher; i++ {
				PublishRefresh("hot")
			}
		}()
	}
	wg.Wait()
	publishElapsed := time.Since(publishStart)

	// The producer must never have blocked on the full slow sink. 800
	// non-blocking publishes are sub-ms work; a generous ceiling that still
	// catches a real stall (a blocked send would hang indefinitely).
	if publishElapsed > 2*time.Second {
		t.Fatalf("PublishRefresh took %s for %d publishes — a slow consumer STALLED the producer (the default: drop arm failed)",
			publishElapsed, publishers*perPublisher)
	}

	// The slow sink must have caused drops (its buffer filled).
	_, delivered, dropped, _ := RefreshBroadcasterCounters()
	if dropped == 0 {
		t.Fatalf("dropped counter=0 — the slow sink never overflowed; the drop arm was not exercised")
	}

	// The healthy sink must have received signals throughout (it was not
	// starved by the slow one).
	<-fastDone
	if fastReceived.Load() == 0 {
		t.Fatalf("healthy subscriber received 0 signals — the slow consumer starved it")
	}
	t.Logf("9.6 slow-consumer: %d publishes in %s, delivered=%d dropped=%d, healthy_received=%d (producer never stalled)",
		publishers*perPublisher, publishElapsed.Round(time.Millisecond), delivered, dropped, fastReceived.Load())
}

// --- 9.8 — dropped TERMINAL signal degrades to the throttle, never stale ----

// TestRefreshBroadcaster_DroppedTerminalSignalDegrades is falsifier 9.8 at the
// cache layer. A saturated sink drops the FINAL signal for a key (no further
// commit re-signals). The invariant: a dropped signal must NOT strand the
// widget — because the dropped signal carried no payload, the L1 entry is
// ALREADY fresh, so the frontend's blind 5s-throttle refetch (modelled here as
// a plain Get after the drop) reads the fresh value. The SSE signal is an
// optimisation over the throttle, never a deadlock-on-drop replacement.
func TestRefreshBroadcaster_DroppedTerminalSignalDegrades(t *testing.T) {
	withRefreshLayer(t)
	t.Setenv("RESOLVED_CACHE_TTL_SECONDS", "3600")

	c := ResolvedCache()
	if c == nil {
		t.Fatalf("ResolvedCache nil")
	}
	in := ResolvedKeyInputs{CacheEntryClass: "widgets", Namespace: "team-a", Name: "w"}
	key := ComputeKey(in)

	// A saturated subscriber on the key: never drained, buffer will fill.
	chSlow, unsub := SubscribeRefresh(map[string]struct{}{key: {}})
	defer unsub()
	_ = chSlow

	// Refresher commits the FRESH value to L1 (this is what makes the signal
	// coherent: the data is in L1 before the signal fires).
	const fresh = `{"phase":"v2-FRESH"}`
	c.Put(key, &ResolvedEntry{RawJSON: []byte(fresh), Inputs: &in})

	// Now flood signals so the slow sink's buffer fills and the TERMINAL
	// signal for this key is dropped.
	for i := 0; i < refreshSubChanCap+50; i++ {
		PublishRefresh(key)
	}
	if _, _, dropped, _ := RefreshBroadcasterCounters(); dropped == 0 {
		t.Fatalf("setup: no drop occurred; cannot test the dropped-terminal case")
	}

	// The frontend's 5s throttle eventually refetches REGARDLESS of the
	// dropped signal — modelled as a Get. The entry must be FRESH (the drop
	// did not strand it stale), proving degrade-to-throttle convergence.
	entry, ok := c.Get(key)
	if !ok || entry == nil {
		t.Fatalf("entry absent after dropped terminal signal — widget stranded")
	}
	if string(entry.RawJSON) != fresh {
		t.Fatalf("throttle refetch read stale body %q want %q — dropped signal stranded the widget stale (9.8 FAIL)",
			entry.RawJSON, fresh)
	}
}
