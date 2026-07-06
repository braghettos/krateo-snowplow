// prewarm_keepwarm_sweep_test.go — #102 c1 keep-warm sweep falsifiers
// (docs/g-ttl-quiet-page-keepwarm-design-2026-07-04.md §5 + PM GTTL-1..6).
//
// Hermetic, -race, seams only (no cluster / apiserver). Serializes on
// engineLatchTestMu (shares the process engine singleton queue + seed counters
// + customerInFlight atomic with the sibling engine tests). Does NOT touch
// ./internal/rbac/... (destructive TestMain).
//
// ARMS:
//   GTTL-2  TestKeepwarmSweep_CadenceIsTTLTimesThreeQuarters — the cadence is
//           the TTL×3/4 design ratio derived from RESOLVED_CACHE_TTL_SECONDS,
//           not a magic duration.
//   GTTL-6  TestKeepwarmSweep_EnqueuesScopeNotStoreScan — a tick enqueues a
//           scopeKindKeepwarm on the engine (seed-side re-enqueue; the store is
//           never scanned). Coalesces on key()=="keepwarm".
//   ARM-KEEPWARM (cadence loop) TestKeepwarmSweep_TickerEnqueuesThenStopsOnCancel
//           — runKeepwarmSweepLoop enqueues on tick and exits on ctx cancel
//           (no goroutine leak).
//   GTTL-3 / ARM-SCOPE TestKeepwarmSweep_RANK1Only_DoesNotSweepRank2 (+ mutation)
//           — the rank1Only seed touches ONLY rank-1 targets; the sweep-all-ranks
//           mutation (rank1Only=false under the same fixture) DOES touch rank-2
//           = RED, proving the bound discriminates.
//   GTTL-1 / ARM-BACKSTOP TestSeedPutGate_DeclinesOnStageError (+ mutation +
//           empty-result control) lives in phase1_seed_put_gate_test.go.

package dispatchers

import (
	"context"
	"testing"
	"time"

	"github.com/krateoplatformops/plumbing/endpoints"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// GTTL-2 — cadence == TTL×3/4 derived from RESOLVED_CACHE_TTL_SECONDS.
func TestKeepwarmSweep_CadenceIsTTLTimesThreeQuarters(t *testing.T) {
	t.Setenv("RESOLVED_CACHE_TTL_SECONDS", "3600")
	ttl := cache.ResolvedCacheTTL()
	if ttl != 3600*time.Second {
		t.Fatalf("ResolvedCacheTTL: want 3600s; got %v", ttl)
	}
	got := keepwarmSweepInterval()
	want := ttl * keepwarmCadenceNumerator / keepwarmCadenceDenominator // 3/4
	if got != want {
		t.Fatalf("keepwarmSweepInterval: want TTL×3/4 = %v; got %v", want, got)
	}
	// Concretely: 3600s × 3/4 = 2700s.
	if got != 2700*time.Second {
		t.Fatalf("keepwarmSweepInterval @3600s TTL: want 2700s; got %v", got)
	}

	// A different TTL re-derives (no hardcoded interval): 800s × 3/4 = 600s.
	t.Setenv("RESOLVED_CACHE_TTL_SECONDS", "800")
	if got := keepwarmSweepInterval(); got != 600*time.Second {
		t.Fatalf("keepwarmSweepInterval @800s TTL: want 600s; got %v", got)
	}
}

// GTTL-6 — a tick ENQUEUES a scopeKindKeepwarm on the engine singleton (the
// sweep is seed-side re-enqueue, not a store scan) and coalesces on "keepwarm".
func TestKeepwarmSweep_EnqueuesScopeNotStoreScan(t *testing.T) {
	engineLatchTestMu.Lock()
	defer engineLatchTestMu.Unlock()
	drainSingletonPending() // clean slate

	enqueueKeepwarmSweep()
	enqueueKeepwarmSweep() // second tick coalesces (dedup on key()=="keepwarm")

	e := prewarmEngineSingleton()
	e.mu.Lock()
	n := len(e.pending)
	_, hasKeepwarm := e.pending[string(scopeKindKeepwarm)]
	e.mu.Unlock()

	if !hasKeepwarm {
		t.Fatalf("expected a pending scopeKindKeepwarm after enqueue; pending has no \"keepwarm\" key")
	}
	if n != 1 {
		t.Fatalf("expected the two ticks to COALESCE to 1 pending scope (dedup on key); got %d pending", n)
	}
	drainSingletonPending()
}

// ARM-KEEPWARM (cadence loop) — the ticker enqueues on each tick and the loop
// exits promptly on ctx cancel (no leak). Uses a short interval directly (not
// the production TTL×3/4) so the test is fast; the cadence VALUE is asserted by
// GTTL-2 above.
func TestKeepwarmSweep_TickerEnqueuesThenStopsOnCancel(t *testing.T) {
	engineLatchTestMu.Lock()
	defer engineLatchTestMu.Unlock()
	drainSingletonPending()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runKeepwarmSweepLoop(ctx, 5*time.Millisecond)
		close(done)
	}()

	// Wait until at least one tick enqueued a keepwarm scope.
	deadline := time.Now().Add(2 * time.Second)
	enqueued := false
	for time.Now().Before(deadline) {
		e := prewarmEngineSingleton()
		e.mu.Lock()
		_, ok := e.pending[string(scopeKindKeepwarm)]
		e.mu.Unlock()
		if ok {
			enqueued = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !enqueued {
		cancel()
		<-done
		t.Fatal("ARM-KEEPWARM: ticker did not enqueue a keepwarm scope within 2s")
	}

	cancel()
	select {
	case <-done:
		// loop exited on ctx cancel — no leak.
	case <-time.After(2 * time.Second):
		t.Fatal("ARM-KEEPWARM: runKeepwarmSweepLoop did not exit within 2s of ctx cancel (goroutine leak)")
	}
	drainSingletonPending()
}

// GTTL-3 / ARM-SCOPE — the rank1Only seed touches ONLY rank-1 targets; the
// sweep-all-ranks mutation (rank1Only=false) DOES touch rank-2 = RED.
//
// Fixture: one widget with targets under TWO ranks — devs (collapsed=442, rank
// 1) and ops (collapsed=5, rank 2). rank1Only=true must seed devs and skip ops;
// rank1Only=false (boot / the mutation) must seed BOTH.
func TestKeepwarmSweep_RANK1Only_DoesNotSweepRank2(t *testing.T) {
	engineLatchTestMu.Lock()
	defer engineLatchTestMu.Unlock()
	zeroCustomerInFlight()
	quietLoggingE(t)

	devs := eID{name: "devs", group: true, collapsed: 442}
	ops := eID{name: "ops", group: true, collapsed: 5}

	h := newNavWidgetHarvester()
	gvr := eWidgetGVR("flexes")
	eHarvestWidget(h, "dashboard-flex", gvr)
	widgets := h.snapshot()

	prevEnum := enumeratePrewarmTargetsForGVRFn
	enumeratePrewarmTargetsForGVRFn = func(g schema.GroupVersionResource, _ string) []cache.PrewarmTarget {
		if g == gvr {
			return eIdentityTargets(gvr, devs, ops)
		}
		return nil
	}
	t.Cleanup(func() { enumeratePrewarmTargetsForGVRFn = prevEnum })

	run := func(rank1Only bool) map[string]bool {
		seen := map[string]bool{}
		prevW := seedOneWidgetFn
		seedOneWidgetFn = func(ctx context.Context, _ navWidgetEntry, _ string) error {
			seen[eIdentityLabel(ctx)] = true
			return nil
		}
		defer func() { seedOneWidgetFn = prevW }()
		if err := seedScopeYielding(context.Background(), nil, widgets, endpoints.Endpoint{}, nil, "authn-ns", rank1Only); err != nil {
			t.Fatalf("seedScopeYielding(rank1Only=%v) returned %v; want nil", rank1Only, err)
		}
		return seen
	}

	// GREEN: rank1Only=true seeds ONLY rank-1 (devs), NOT rank-2 (ops).
	sweep := run(true)
	if !sweep["group:devs"] {
		t.Fatalf("ARM-SCOPE: rank-1 keepwarm sweep did NOT seed the rank-1 identity (devs); seen=%v", sweep)
	}
	if sweep["group:ops"] {
		t.Fatalf("ARM-SCOPE VIOLATED: the rank-1 keepwarm sweep seeded the rank-2 identity (ops) — the c1 bound is not holding; seen=%v", sweep)
	}

	// MUTATION (sweep-all-ranks): rank1Only=false under the SAME fixture DOES
	// seed rank-2 → proves the ARM-SCOPE assert discriminates the bound (a
	// no-op bound would seed ops in both arms).
	full := run(false)
	if !full["group:devs"] || !full["group:ops"] {
		t.Fatalf("mutation (rank1Only=false) NOT RED: the all-ranks seed must touch BOTH devs and ops "+
			"(else the ARM-SCOPE assert cannot discriminate the bound); seen=%v", full)
	}
}
