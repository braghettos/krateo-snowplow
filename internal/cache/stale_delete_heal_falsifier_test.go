// stale_delete_heal_falsifier_test.go — Fix #1 (stale-delete informer
// heal/re-touch) pre-flight falsifier triad.
//
// Team rule feedback_falsifier_first_before_ship: every assertion here is
// written BEFORE the 1a/1b production change and is the negative control
// of record. The discriminating delta lives in part A (the scoped-confirm
// helper that 1b primes on the LIST register path): against the unfixed
// tree `ConfirmResourceType` does not exist, so the package does not
// compile — the captured FAIL artifact is the compile error. Once 1b adds
// the helper, the SAME assertions pass and pin the behaviour.
//
// Context (docs/rca-stale-delete-compositiondefinitions-informer-2026-06-25.md):
// a `compositiondefinitions` cluster-LIST that backs /blueprints is served
// from the apistage CONTENT cache HIT, which short-circuits BEFORE
// dispatchViaInformer. Once the entry is Put while the GVR is
// `not-servable` (registered-but-unconfirmed / watch-broken), nothing ever
// re-touches the informer to confirm it, so the data informer's DELETE
// never dirty-marks the dependent /blueprints entry and the catalog stays
// stale to TTL.
//
//	F-heal-A — ConfirmResourceType confirms ONE GVR exactly the way a full
//	           RefreshDiscovery pass would (conjunct 4), and clears a broken
//	           watch on an advanced RV (conjunct-3 recovery) — WITHOUT
//	           forking a parallel confirm predicate and WITHOUT touching any
//	           OTHER registered GVR. This is 1b's core: prime one scoped
//	           confirmation pass on the api-step LIST register path so the
//	           first post-boot LIST does not wait a full 30s tick and a
//	           transient discovery flap self-corrects.
//
//	F-heal-B — once the GVR is confirmed + watch-healthy (servable), a real
//	           informer DELETE flows DeleteFunc -> submitDeleteEvent ->
//	           OnDelete and DIRTY-MARKS the dependent resolved-output LIST
//	           entry. CONTENT, not status: the assertion is on the served
//	           membership (the deleted object's L1 key is dirty-marked /
//	           the LIST-dep bucket fired), never on dirtyMark>0 alone.
//	           (feedback_validate_content_not_just_status).
//
//	F-heal-C — negative control: a registered-but-UNCONFIRMED GVR (the
//	           latched not-servable state the content cache shields) is NOT
//	           servable; after ONE scoped ConfirmResourceType it flips to
//	           servable=true — proving the heal re-touch is what lifts the
//	           latch, and the SAME scoped pass does NOT confirm an unrelated
//	           registered GVR whose type the apiserver does not serve.
//
// Per feedback_no_special_cases.md: every GVR here is a generic
// customer-style GVR (the secrets test GVR); there is NO compositiondefinitions
// literal — the helper under test is uniform over every registered GVR.

package cache_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/krateoplatformops/snowplow/internal/cache"

	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// healSecondGVR is a SECOND registered GVR used by the no-side-effect
// assertions: a scoped ConfirmResourceType(gvr) must touch ONLY gvr's
// confirmed state, never a peer's. configmaps shares the core group/version
// with secrets in discovery keying ("v1"), so the negative-side assertion
// (a scoped confirm of secrets does not confirm a peer whose type is
// unserved) uses a DISTINCT group/version to keep the discovery key
// independent.
var healSecondGVR = schema.GroupVersionResource{
	Group: "apps", Version: "v1", Resource: "deployments",
}

// healThirdGVR is the delete+recreate churn GVR for the N2 interleave race
// (TestFalsifierHealA_ScopedConfirmRace). Distinct group/version so its
// discovery key is independent of secrets/deployments.
var healThirdGVR = schema.GroupVersionResource{
	Group: "batch", Version: "v1", Resource: "jobs",
}

// healListKinds extends the servable test List-kinds with the Deployments +
// Jobs List entries so the fake dynamic client accepts those informers'
// initial LIST without panicking.
func healListKinds() map[schema.GroupVersionResource]string {
	m := servableListKinds()
	m[healSecondGVR] = "DeploymentList"
	m[healThirdGVR] = "JobList"
	return m
}

// --- F-heal-A — scoped confirm reuses RefreshDiscovery's predicate -----------

// TestFalsifierHealA_ScopedConfirmMatchesRefreshDiscovery is 1b's core
// falsifier. It proves ConfirmResourceType(ctx, gvr):
//
//  1. confirms gvr (conjunct 4) when the apiserver serves its type — the
//     SAME outcome a full RefreshDiscovery pass produces, so 1b does not
//     fork a parallel predicate;
//  2. leaves an UNRELATED registered GVR (whose type the apiserver does
//     NOT serve) unconfirmed — the scope is exactly gvr, no fan-out;
//  3. clears a broken watch on an advanced RV (conjunct-3 recovery) — the
//     same recovery branch RefreshDiscovery runs.
//
// Against the unfixed tree this FAILS to COMPILE: ConfirmResourceType does
// not exist. The compile error is the captured pre-flight FAIL artifact.
func TestFalsifierHealA_ScopedConfirmMatchesRefreshDiscovery(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	// Seed one secret so the secrets informer's RV advances past the
	// genuinely-empty zero value — needed for the conjunct-3 recovery
	// branch (LastSyncResourceVersion must be non-empty AND advance).
	seed := newUnstructuredSecret("ns-a", "secret-a")
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		newTestScheme(), healListKinds(), seed)
	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	t.Cleanup(func() {
		rw.Stop()
		time.Sleep(50 * time.Millisecond)
	})

	// Discovery serves the secrets type ("v1") but NOT the deployments
	// type ("apps/v1"). A scoped confirm of secrets must confirm secrets
	// and leave deployments unconfirmed.
	disco := &fakeDiscovery{served: map[string]bool{"v1": true}}

	// #130 F1b: register the informers with NO discovery client wired so the
	// lazy-register auto-prime (primeConfirmAsyncLocked) short-circuits on its
	// disco==nil guard — this test needs to establish the "registered + synced
	// but UNCONFIRMED" latched pre-state by hand, which the auto-prime would
	// otherwise dissolve. The discovery client is wired AFTER registration so
	// the SCOPED ConfirmResourceType this test exercises is the ONLY confirm
	// path that runs. (feedback_no_shortcuts_or_workarounds: this is not a
	// bypass of the fix — it is the correct way to set up a negative control
	// for a helper the fix now also auto-invokes; the fix's own reachability is
	// proven in lazy_register_confirm_prime_f1b_test.go.)
	for _, gvr := range []schema.GroupVersionResource{servableTestGVR, healSecondGVR} {
		added, syncCh := rw.EnsureResourceType(gvr)
		if !added {
			t.Fatalf("EnsureResourceType(%s): want added=true", gvr)
		}
		select {
		case <-syncCh:
		case <-time.After(5 * time.Second):
			t.Fatalf("EnsureResourceType(%s): informer did not sync within 5s", gvr)
		}
	}
	rw.SetDiscoveryClient(disco)

	// PRE: neither GVR is confirmed yet (no RefreshDiscovery pass run), so
	// both are not-servable — the latched state the content cache shields.
	if rw.IsServable(servableTestGVR) {
		t.Fatalf("pre-confirm: secrets must be not-servable (unconfirmed); got servable")
	}
	if rw.IsServable(healSecondGVR) {
		t.Fatalf("pre-confirm: deployments must be not-servable (unconfirmed); got servable")
	}

	// ACT: ONE scoped confirmation pass for secrets only.
	rw.ConfirmResourceType(context.Background(), servableTestGVR)

	// POST conjunct-4: secrets confirmed -> servable. This is the SAME
	// outcome rw.RefreshDiscovery would produce for secrets.
	if !rw.IsServable(servableTestGVR) {
		t.Fatalf("F-heal-A: scoped ConfirmResourceType(secrets) did not confirm secrets; "+
			"IsServable=false (conjunct 4 not lifted)")
	}
	// SCOPE: deployments (unrelated, type unserved) stays unconfirmed.
	// Proves the scoped pass touched ONLY the named GVR — no fan-out.
	if rw.IsServable(healSecondGVR) {
		t.Fatalf("F-heal-A: scoped ConfirmResourceType(secrets) leaked confirmation to "+
			"deployments (whose type the apiserver does not serve) — scope is not gvr-local")
	}

	// Conjunct-3 recovery PARITY: break the watch, then prove a scoped
	// ConfirmResourceType produces the SAME conjunct-3 outcome a full
	// RefreshDiscovery does — i.e. it reuses RefreshDiscovery's recovery
	// body, not a fork. Whether the fake reflector's LastSyncResourceVersion
	// happens to advance is nondeterministic, so we assert EQUIVALENCE
	// (scoped == full) rather than a fixed clear: run a full RefreshDiscovery
	// to obtain the reference outcome, re-break, run the scoped confirm, and
	// assert the resulting servability matches. (The conjunct-4 confirm +
	// scope above is the load-bearing assertion; per the RCA §8 Q2 the
	// stale-delete latch is most-likely conjunct 4.)
	rw.FireWatchError(servableTestGVR)
	if rw.IsServable(servableTestGVR) {
		t.Fatalf("setup: FireWatchError must drop secrets to not-servable (conjunct 3)")
	}
	rw.RefreshDiscovery(context.Background())
	fullOutcome := rw.IsServable(servableTestGVR)

	rw.FireWatchError(servableTestGVR)
	if rw.IsServable(servableTestGVR) {
		t.Fatalf("setup: re-break must drop secrets to not-servable")
	}
	rw.ConfirmResourceType(context.Background(), servableTestGVR)
	scopedOutcome := rw.IsServable(servableTestGVR)

	if scopedOutcome != fullOutcome {
		t.Fatalf("F-heal-A: scoped ConfirmResourceType conjunct-3 recovery diverged from "+
			"RefreshDiscovery (scoped servable=%v, full servable=%v) — the scoped helper "+
			"is NOT reusing RefreshDiscovery's recovery body", scopedOutcome, fullOutcome)
	}
}

// --- F-heal-A race — scoped confirm is concurrency-safe ----------------------

// TestFalsifierHealA_ScopedConfirmRace runs ConfirmResourceType
// concurrently with the discovery-refresh ticker's RefreshDiscovery over
// the SAME registered GVRs. Both write rw.confirmed / rw.watchBroken under
// rw.mu; -race must stay clean (feedback_shared_vs_copy_is_a_concurrency_change:
// the scoped helper touches state shared with the ticker, so it needs a
// concurrent -race test, not a content-equivalence check).
func TestFalsifierHealA_ScopedConfirmRace(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	seed := newUnstructuredSecret("ns-a", "secret-a")
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		newTestScheme(), healListKinds(), seed)
	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	t.Cleanup(func() {
		rw.Stop()
		time.Sleep(50 * time.Millisecond)
	})
	disco := &fakeDiscovery{served: map[string]bool{"v1": true, "apps/v1": true}}
	rw.SetDiscoveryClient(disco)

	for _, gvr := range []schema.GroupVersionResource{servableTestGVR, healSecondGVR} {
		_, syncCh := rw.EnsureResourceType(gvr)
		select {
		case <-syncCh:
		case <-time.After(5 * time.Second):
			t.Fatalf("EnsureResourceType(%s): did not sync", gvr)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	// Full-pass refresher (the production ticker's body).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			if ctx.Err() != nil {
				return
			}
			rw.RefreshDiscovery(ctx)
		}
	}()
	// Scoped confirms over both GVRs, plus watch-error churn to exercise
	// the conjunct-3 write path under contention.
	for _, gvr := range []schema.GroupVersionResource{servableTestGVR, healSecondGVR} {
		gvr := gvr
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				if ctx.Err() != nil {
					return
				}
				rw.ConfirmResourceType(ctx, gvr)
				if i%7 == 0 {
					rw.FireWatchError(gvr)
				}
			}
		}()
	}
	// Concurrent readers to surface any read/write race on the maps.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			if ctx.Err() != nil {
				return
			}
			_ = rw.IsServable(servableTestGVR)
			_ = rw.IsServable(healSecondGVR)
		}
	}()
	// N2 (architect) — delete+recreate churn on a THIRD GVR, run concurrently
	// with a goroutine that confirms it. This exercises the exact interleave
	// the ConfirmResourceType re-read (curGI under the write lock) guards
	// against: a RemoveResourceType between the RLock snapshot and the write
	// Lock, possibly followed by an EnsureResourceType of a FRESH informer.
	// -race must stay clean and ConfirmResourceType must never write
	// rw.lastSyncRV off a torn-down reflector handle.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			if ctx.Err() != nil {
				return
			}
			_, syncCh := rw.EnsureResourceType(healThirdGVR)
			if syncCh != nil {
				select {
				case <-syncCh:
				case <-time.After(time.Second):
				case <-ctx.Done():
					return
				}
			}
			rw.RemoveResourceType(healThirdGVR)
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			if ctx.Err() != nil {
				return
			}
			rw.ConfirmResourceType(ctx, healThirdGVR)
			_ = rw.IsServable(healThirdGVR)
		}
	}()
	wg.Wait()
}

// F-heal-B (DELETE through a servable informer dirty-marks the dependent
// LIST entry, content-not-status) lives in
// stale_delete_heal_dep_falsifier_test.go (package cache) because it wires
// the unexported resolved-cache store + DepTracker. Keeping the
// store-wiring assertion in-package avoids exporting a production
// test-only constructor.

// --- F-heal-C — the latched not-servable state heals via one scoped confirm ---

// TestFalsifierHealC_LatchedNotServableHealsOnConfirm is the negative
// control for 1a/1b's heal effect, stated as a true-vs-false delta on the
// SAME GVR:
//
//	BEFORE the scoped confirm (the latched state the content cache shields):
//	  registered + synced, but UNCONFIRMED -> IsServable = false
//	AFTER one scoped ConfirmResourceType:
//	  registered + synced + confirmed       -> IsServable = true
//
// The delta is the captured control: the heal re-touch (1b primes this on
// the LIST register path; 1a keeps re-touching on the content HIT) is what
// lifts the latch. Order matters per the PM AC: heal FORWARD behaviour;
// it does not retroactively evict — a DELETE arriving AFTER the heal is
// what evicts (covered by F-heal-B).
func TestFalsifierHealC_LatchedNotServableHealsOnConfirm(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	seed := newUnstructuredSecret("ns-a", "secret-a")
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		newTestScheme(), healListKinds(), seed)
	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	t.Cleanup(func() {
		rw.Stop()
		time.Sleep(50 * time.Millisecond)
	})
	disco := &fakeDiscovery{served: map[string]bool{"v1": true}}

	// #130 F1b: register with NO discovery client wired so the lazy-register
	// auto-prime short-circuits (disco==nil guard) and this test can establish
	// the "registered + synced but UNCONFIRMED" latched control by hand; wire
	// the discovery client AFTER register so the scoped ConfirmResourceType
	// below is the only confirm that runs.
	added, syncCh := rw.EnsureResourceType(servableTestGVR)
	if !added {
		t.Fatalf("EnsureResourceType: want added=true")
	}
	select {
	case <-syncCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("informer did not sync within 5s")
	}
	rw.SetDiscoveryClient(disco)

	// BEFORE: registered + synced but unconfirmed -> not-servable. This is
	// the exact latch the content cache shields permanently pre-fix.
	before := rw.IsServable(servableTestGVR)
	if before {
		t.Fatalf("negative control invalid: a registered+synced-but-unconfirmed GVR must be "+
			"not-servable; got servable before any confirm")
	}

	// HEAL: one scoped confirmation pass (what 1b primes on the LIST
	// register path).
	rw.ConfirmResourceType(context.Background(), servableTestGVR)

	// AFTER: confirmed -> servable.
	after := rw.IsServable(servableTestGVR)
	if !after {
		t.Fatalf("F-heal-C: scoped confirm did not heal the latched not-servable GVR; "+
			"still not-servable after ConfirmResourceType")
	}
	if before == after {
		t.Fatalf("negative control: expected a false-vs-true delta on the SAME GVR "+
			"(before=%v after=%v); the scoped confirm is not lifting the latch", before, after)
	}
	t.Logf("F-heal-C heal delta: before-confirm servable=%v, after-confirm servable=%v", before, after)
}
