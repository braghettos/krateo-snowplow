// readiness_backstop_metrics_test.go — F5 (#131) falsifiers: backstop-Ready is a
// LOUD, counted failure signal, and the happy path is SILENT.
//
//	F5-1 (the alert fires): recordReadinessBackstop bumps snowplow_readyz_backstop_fired.
//	F5-2 (latch discriminates): a FIRED firstNav latch reports fired()==true (happy
//	     path → no alert), an unfired latch reports false (backstop arm → alert). This
//	     is the exact predicate engineSeed's backstop arms gate the alert on, so it
//	     pins the "silent on happy path, loud on backstop" contract.
//	F5-3 (expvar published + reads back): the counter is registered and reflects the
//	     process count operators/alerts scrape at /debug/vars.
//	F5-4 (panic path, arch item 5): a pipSeed that PANICS drives the REAL
//	     phase1WarmupWith recover block → recordReadinessBackstop("seed_panic") →
//	     counter bumps AND readiness still flips (MarkPhase1Done fires). The worst
//	     failure mode is counted, not silent. Exercises the real closure, not a shadow.
package dispatchers

import (
	"context"
	"expvar"
	"testing"
	"time"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// TestF5_1_RecordBackstop_BumpsCounter — the alert-fires arm. Each
// recordReadinessBackstop call increments the process expvar by exactly one.
func TestF5_1_RecordBackstop_BumpsCounter(t *testing.T) {
	before := readinessBackstopFired.Value()
	recordReadinessBackstop("phase1_timeout", 200*time.Millisecond, -1)
	if got := readinessBackstopFired.Value(); got != before+1 {
		t.Fatalf("F5-1: snowplow_readyz_backstop_fired = %d, want %d (one bump per backstop-Ready)", got, before+1)
	}
	// A second, different reason still bumps (the counter is reason-agnostic —
	// ANY backstop-Ready is a failed boot).
	recordReadinessBackstop("seed_incomplete", time.Second, 3)
	if got := readinessBackstopFired.Value(); got != before+2 {
		t.Fatalf("F5-1: second backstop must also bump; got %d want %d", got, before+2)
	}
}

// TestF5_2_LatchFired_DiscriminatesHappyVsBackstop — the discrimination arm. The
// engineSeed backstop arms gate the alert on !firstNav.fired(). A latch that has
// FIRED (happy path — all nav widgets seeded) must report fired()==true so NO
// alert is emitted; an unfired latch (backstop — nav widgets unseeded) reports
// false so the alert fires. This is the "silent on the happy path" guarantee.
func TestF5_2_LatchFired_DiscriminatesHappyVsBackstop(t *testing.T) {
	l := newFirstNavLatch()
	if l.fired() {
		t.Fatal("F5-2: a fresh latch must report fired()==false (nothing seeded yet → backstop arm would alert)")
	}
	// Fire it (the happy path: all cohorts' nav widgets seeded).
	l.fire("segment_complete", 3, 3, "", -1, 50*time.Millisecond)
	if !l.fired() {
		t.Fatal("F5-2: a FIRED latch must report fired()==true → engineSeed's backstop arms must NOT alert on the happy path")
	}
}

// TestF5_3_ExpvarPublished — the observability arm. The counter is published under
// the canonical key so /debug/vars (and any alert scraping it) sees it.
func TestF5_3_ExpvarPublished(t *testing.T) {
	registerReadinessBackstopMetrics() // idempotent
	v := expvar.Get("snowplow_readyz_backstop_fired")
	if v == nil {
		t.Fatal("F5-3: snowplow_readyz_backstop_fired must be published at /debug/vars")
	}
	before := readinessBackstopFired.Value()
	recordReadinessBackstop("boot_error", 10*time.Millisecond, -1)
	// The published var and the backing counter are the SAME object → the scrape
	// reflects the bump.
	if v != expvar.Get("snowplow_readyz_backstop_fired") {
		t.Fatal("F5-3: the published var must be stable across scrapes")
	}
	if readinessBackstopFired.Value() != before+1 {
		t.Fatalf("F5-3: the published counter must reflect the bump; got %d want %d", readinessBackstopFired.Value(), before+1)
	}
}

// TestF5_4_PanicSeed_CountsBackstop — arch item 5. A pipSeed that PANICS must
// drive the REAL phase1WarmupWith recover block: recover logs phase1.seed.panic,
// then recordReadinessBackstop("seed_panic") bumps the counter, then the
// MarkPhase1Done defer still flips readiness (Ready-DEGRADED, never
// not-Ready-forever). Drives the actual closure via a panicking pipSeedFn — no
// shadow of the recover logic.
func TestF5_4_PanicSeed_CountsBackstop(t *testing.T) {
	rw := phase1TestWatcher(t)
	cache.ResetPhase1DoneForTest()
	t.Cleanup(cache.ResetPhase1DoneForTest)

	// No roots — the walk completes its sync barrier fast; the seed is the only
	// interesting step. A nil-root lister keeps the arm focused on the panic path.
	lister := func(ctx context.Context) ([]navigationRoot, error) { return nil, nil }
	resolver := func(ctx context.Context, root navigationRoot) error { return nil }

	// The pipSeed that PANICS — the worst failure mode the recover block guards.
	panicSeed := pipSeedFn(func(ctx context.Context) error {
		panic("F5-4: simulated seed panic")
	})

	before := readinessBackstopFired.Value()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// phase1WarmupWith must NOT propagate the panic (the recover swallows it) and
	// must still return nil / flip Ready-degraded.
	if err := phase1WarmupWith(ctx, rw, lister, resolver, nil, nil, panicSeed, nil); err != nil {
		t.Fatalf("F5-4: phase1WarmupWith must survive a panicking seed (recover), got err=%v", err)
	}

	if got := readinessBackstopFired.Value(); got != before+1 {
		t.Fatalf("F5-4: a panicking seed must count ONE backstop-Ready; snowplow_readyz_backstop_fired = %d, want %d — the worst failure mode went silent", got, before+1)
	}
	// C2 backstop preserved: readiness still flipped despite the panic.
	if !cache.IsPhase1Done() {
		t.Fatal("F5-4: MarkPhase1Done must still fire after a seed panic (Ready-DEGRADED, never not-Ready-forever)")
	}
}
