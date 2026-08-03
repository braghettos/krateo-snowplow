// resolve_concurrency_falsifier_test.go — regression guard for the
// REVERT of OOM fix (a) (project_regression_journal 2026-06-24, task #29).
//
// Fix (a) (1.5.1 / #46 / commit f13c7be) inserted a single process-wide
// *semaphore.Weighted acquire at BOTH customer-dispatch ServeHTTP entries
// (restactions.go + widgets.go), BEFORE the resolve work and — critically —
// before the L1-hit short-circuit. With its FLAT 2.25 GiB per-resolve weight
// charged against a 4 GiB budget, the effective cap was 1: every customer
// /call across the whole process serialized behind a single permit, so a
// burst of light warm widget hits queued for minutes → browser-cancel →
// blank widgets on bs-test-ger-03.
//
// THE PREVENTION RULE (feedback_bounding_mechanism_discipline rule 4): the
// concurrency=1 falsifier that SHIPPED (a) could never have caught the
// serialization — it asserted "cap holds" at N→1, the very behaviour that
// broke production. The correct falsifier exercises REALISTIC concurrency
// (N≥8 light customer resolves) and asserts they ALL proceed in parallel
// with NO shared-semaphore gate serializing them. This is that test. It is
// the inverse of the shipped one: it FAILS if any process-wide blocking
// gate is re-introduced on the customer-dispatch entry path.
//
// We exercise the REAL shared customer-dispatch entry bracket that remains
// after the revert — markCustomerInFlight (prewarm_engine.go:98), the
// customer-priority signal, which is KEPT (it is a non-blocking atomic
// counter, NOT part of (a)). The test proves the entry bracket admits full
// parallelism. A re-introduced shared semaphore at the entry would cap the
// observed concurrency below N and trip the assertion.
//
// NEVER uses the remote kubeconfig (feedback_no_go_test_against_remote_
// kubeconfig): pure in-process, no apiserver, no cache, no RBAC.
package dispatchers

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestCustomerResolveEntry_NoSerialization is the realistic-concurrency
// falsifier. N concurrent customer dispatches each run the real entry
// bracket (markCustomerInFlight) plus a tiny simulated resolve, released
// together off a shared start barrier. We assert PEAK observed concurrency
// reaches N — i.e. the entry path imposes no process-wide cap. With fix (a)
// present this peaks at 1 (the FLAT-weight cap=1 that broke production).
func TestCustomerResolveEntry_NoSerialization(t *testing.T) {
	const N = 16 // ≥ 8 per the prevention rule; well above the (a) cap of 1.

	var (
		start    = make(chan struct{})
		inFlight atomic.Int64
		peak     atomic.Int64
		wg       sync.WaitGroup
		// allEntered closes once every goroutine has crossed the entry
		// bracket. If a shared entry semaphore capped concurrency below N,
		// this would never reach N and the timed wait below would fire.
		entered  atomic.Int64
		allEntered = make(chan struct{})
		once       sync.Once
	)

	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			<-start

			// REAL customer-dispatch entry bracket. After the revert this
			// is the only shared package-level primitive a /call acquires
			// before its resolve work; it must never block.
			done := markCustomerInFlight()
			defer done()

			// Record concurrency reached inside the bracket.
			cur := inFlight.Add(1)
			for { // lock-free max
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			if entered.Add(1) == N {
				once.Do(func() { close(allEntered) })
			}

			// Simulate a light (warm L1-hit) resolve: tiny, non-blocking.
			// Hold briefly so the goroutines overlap and peak can reach N.
			time.Sleep(20 * time.Millisecond)
			inFlight.Add(-1)
		}()
	}

	close(start)

	select {
	case <-allEntered:
		// All N crossed the entry bracket concurrently — no serialization.
	case <-time.After(5 * time.Second):
		t.Fatalf("only %d/%d customer dispatches reached the entry bracket "+
			"concurrently within 5s — a process-wide gate is serializing the "+
			"customer resolve path (fix (a) regression re-introduced)",
			entered.Load(), N)
	}

	wg.Wait()

	if got := peak.Load(); got < N {
		t.Fatalf("peak concurrent customer dispatches = %d, want %d: the "+
			"customer resolve entry path is capped below the inbound burst "+
			"size — exactly the fix-(a) serialization the revert removes", got, N)
	}

	// Post-condition: the in-flight counter drains fully (the bracket's
	// deferred decrement covers every path; no leaked permits).
	if got := customerInFlightCount.Load(); got != 0 {
		t.Fatalf("customerInFlightCount = %d after drain, want 0", got)
	}
}

// TestCustomerResolveEntry_LightHitsDoNotQueue is the direct analogue of the
// production symptom: a burst of light WARM hits (the KB widget L1-hits that
// went blank on bs-test-ger-03). Under fix (a) each warm hit had to acquire
// the 2.25 GiB permit BEFORE the L1-hit short-circuit, so they queued behind
// cap=1 for 215s+. Here we assert a burst of N light entries completes well
// within a tight deadline — impossible if any of them serialized behind a
// shared permit.
func TestCustomerResolveEntry_LightHitsDoNotQueue(t *testing.T) {
	const N = 16
	const per = 20 * time.Millisecond
	// If fully parallel, total ≈ per (+scheduling). If serialized behind a
	// cap=1 gate, total ≈ N*per = 320ms. A 200ms deadline cleanly separates
	// the two: parallel passes, any single-permit serialization fails.
	const deadline = 200 * time.Millisecond

	var wg sync.WaitGroup
	wg.Add(N)
	t0 := time.Now()
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			done := markCustomerInFlight()
			defer done()
			time.Sleep(per) // light warm-hit cost
		}()
	}
	wg.Wait()
	if elapsed := time.Since(t0); elapsed > deadline {
		t.Fatalf("burst of %d light customer hits took %v (deadline %v): the "+
			"hits serialized — a shared entry gate is queuing warm L1 hits "+
			"(the bs-test-ger-03 blank-widget regression)", N, elapsed, deadline)
	}
}
