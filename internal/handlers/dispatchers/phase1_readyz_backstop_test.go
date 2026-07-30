// phase1_readyz_backstop_test.go — M7 (coverage-audit): the Phase-1 readiness
// C2 BACKSTOP is unconditional. phase1WarmupWith flips /readyz (MarkPhase1Done)
// on EVERY exit of the Step-7.6 seed block — normal return, seed error, ctx /
// pipGlobalTimeout expiry, OR a panic unwinding through the recover — so a
// stuck/panicking/failing seed yields Ready-DEGRADED, NEVER not-Ready-forever.
//
//	(a) panicking seed  → IsPhase1Done()==true after return + backstop counter++
//	                      AND the panic does NOT escape phase1WarmupWith.
//	(b) blocking seed past a tiny ctx timeout → the flip STILL happens (the
//	                      MarkPhase1Done defer fires when the seed ctx cancels).
//	(c) lister listErr  → NO early flip / early return; the walk falls through to
//	                      the SINGLE Step-7.6 seed flip (seed invoked exactly once).
//
// RED arms:
//   - "early-return on roots_list_failed": a regression that early-returned (or
//     early-flipped + returned) on the lister error would NOT invoke the seed —
//     arm (c) asserts the seed runs exactly once, so that regression reds it.
//   - "panic escapes past MarkPhase1Done": arm (a) asserts phase1WarmupWith
//     returns without propagating the panic AND readiness flipped; a missing /
//     misordered recover reds both.
//
// Complements F5-4 (readiness_backstop_metrics_test.go, panic→counter) with the
// blocking-timeout and lister-error backstop arms it does not cover. Drives the
// REAL phase1WarmupWith over the phase1TestWatcher — no shadow of the flip logic.

package dispatchers

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// TestM7_PanicSeed_FlipsAndCounts_NoEscape — arm (a). A panicking seed must NOT
// crash phase1WarmupWith (recover swallows), readiness still flips, and the
// backstop counter records the worst failure mode.
func TestM7_PanicSeed_FlipsAndCounts_NoEscape(t *testing.T) {
	rw := phase1TestWatcher(t)
	cache.ResetPhase1DoneForTest()
	t.Cleanup(cache.ResetPhase1DoneForTest)

	lister := func(ctx context.Context) ([]navigationRoot, error) { return nil, nil }
	resolver := func(ctx context.Context, root navigationRoot) error { return nil }
	panicSeed := pipSeedFn(func(ctx context.Context) error { panic("M7: seed panic") })

	before := readinessBackstopFired.Value()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The call must return (recover ran) — a panic escaping here fails the test
	// process, which is itself the "panic escapes past MarkPhase1Done" RED.
	err := phase1WarmupWith(ctx, rw, lister, resolver, nil, nil, panicSeed, nil)
	if err != nil {
		t.Fatalf("arm (a): phase1WarmupWith must survive a panicking seed; got err=%v", err)
	}
	if !cache.IsPhase1Done() {
		t.Fatal("arm (a) RED (panic escapes past MarkPhase1Done): readiness did NOT flip after a seed panic — the MarkPhase1Done defer must run AFTER the recover, unconditionally")
	}
	if got := readinessBackstopFired.Value(); got != before+1 {
		t.Fatalf("arm (a): a panicking seed must count exactly ONE backstop-Ready; got %d want %d", got, before+1)
	}
}

// TestM7_BlockingSeedPastCtxTimeout_StillFlips — arm (b). A seed that blocks
// until its ctx cancels (here bounded by a tiny OUTER ctx timeout that the seed
// ctx inherits) must NOT wedge readiness: the MarkPhase1Done defer fires when
// the seed returns after cancellation. Proves the flip is min(seed-complete,
// timeout), never not-Ready-forever.
func TestM7_BlockingSeedPastCtxTimeout_StillFlips(t *testing.T) {
	rw := phase1TestWatcher(t)
	cache.ResetPhase1DoneForTest()
	t.Cleanup(cache.ResetPhase1DoneForTest)

	lister := func(ctx context.Context) ([]navigationRoot, error) { return nil, nil }
	resolver := func(ctx context.Context, root navigationRoot) error { return nil }

	seedObservedCancel := make(chan struct{}, 1)
	blockingSeed := pipSeedFn(func(ctx context.Context) error {
		// Block until the seed ctx (a child of the warmup ctx) cancels.
		<-ctx.Done()
		seedObservedCancel <- struct{}{}
		return ctx.Err()
	})

	// Tiny OUTER timeout → the seed ctx (WithTimeout(ctx, pipGlobalTimeout)) is a
	// child, so it cancels when the outer ctx expires well before 8 minutes.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- phase1WarmupWith(ctx, rw, lister, resolver, nil, nil, blockingSeed, nil) }()

	select {
	case <-done:
		// phase1WarmupWith returned — the seed unblocked on cancellation.
	case <-time.After(10 * time.Second):
		t.Fatal("arm (b) RED: phase1WarmupWith did not return within 10s — a blocking seed WEDGED readiness (not-Ready-forever). The seed ctx must be bounded so the MarkPhase1Done defer still fires.")
	}

	select {
	case <-seedObservedCancel:
		// good — the seed observed its ctx cancel (bounded), not a leak.
	default:
		t.Fatal("arm (b): the seed never observed ctx cancellation — the seed ctx must be a bounded child of the warmup ctx")
	}

	if !cache.IsPhase1Done() {
		t.Fatal("arm (b) RED: readiness did NOT flip after the seed ctx cancelled — MarkPhase1Done must fire regardless (C2 backstop)")
	}
}

// TestM7_ListerError_NoEarlyReturn_SingleSeedFlip — arm (c). A lister error
// (config-vars ConfigMap absent at boot) must NOT early-return / early-flip. The
// walk falls through Steps 4-7 over the empty roots set to the SINGLE Step-7.6
// seed, so the seed is invoked EXACTLY ONCE and readiness flips there (not on a
// deleted early-flip path). RED (early-return on roots_list_failed): the old
// give-up branch would skip the seed entirely — seedCalls==0.
func TestM7_ListerError_NoEarlyReturn_SingleSeedFlip(t *testing.T) {
	rw := phase1TestWatcher(t)
	cache.ResetPhase1DoneForTest()
	t.Cleanup(cache.ResetPhase1DoneForTest)

	lister := func(ctx context.Context) ([]navigationRoot, error) {
		return nil, context.DeadlineExceeded // model "config-vars ConfigMap not present yet"
	}
	resolver := func(ctx context.Context, root navigationRoot) error {
		t.Fatal("arm (c): the resolver must not run when the lister errored (empty roots set)")
		return nil
	}

	var seedCalls atomic.Int64
	seed := pipSeedFn(func(ctx context.Context) error {
		seedCalls.Add(1)
		// Assert the walk reached the seed WITHOUT having flipped early: the
		// only flip is the Step-7.6 MarkPhase1Done defer that fires on this
		// block's exit — so at the moment the seed body runs, readiness is
		// still pre-flip.
		if cache.IsPhase1Done() {
			t.Error("arm (c) RED (early-flip on roots_list_failed): readiness flipped BEFORE the Step-7.6 seed — the roots-error branch must NOT flip early; the single flip is the seed-block defer")
		}
		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := phase1WarmupWith(ctx, rw, lister, resolver, nil, nil, seed, nil)
	// walkErr==nil (the lister error is logged + tolerated, not returned);
	// syncErr==nil over the meta-query seeds. So the return is nil.
	if err != nil {
		t.Fatalf("arm (c): a lister error must be TOLERATED (logged, roots=nil), not returned; got %v", err)
	}
	if n := seedCalls.Load(); n != 1 {
		t.Fatalf("arm (c) RED (early-return on roots_list_failed): the Step-7.6 seed must run EXACTLY ONCE despite the lister error; got %d invocations", n)
	}
	if !cache.IsPhase1Done() {
		t.Fatal("arm (c): readiness must flip via the SINGLE Step-7.6 seed-block defer")
	}
}
