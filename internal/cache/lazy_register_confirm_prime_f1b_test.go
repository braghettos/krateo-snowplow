// lazy_register_confirm_prime_f1b_test.go — #130 F1b falsifiers.
//
// FIX under test: the LAZY-REGISTER production seam — EnsureResourceType /
// EnsureResourceTypeMetadataOnly (watcher.go) — now primes ONE scoped
// conjunct-4 confirm (ConfirmResourceType, async, off the register lock) for
// every GVR it lazily registers, so the GVR becomes IsServable (all four
// conjuncts true) within one discovery round-trip instead of latching
// typeConfirmed:false and falling through to a live LIST.
//
// # Why F1b (the REACHABILITY close — the twice-learned lesson)
//
// F1 (1.7.4) wired the approved confirm-prime onto the discovery-walk register
// path (PrewarmRegisterFromNavigation), gated behind PREWARM_REGISTER_ENABLED
// (default OFF). Under the DEPLOYED default config that walk never runs, so
// F1's on-deploy deltas were all zero — the mechanism was runtime-INERT. The
// registration path the default binary ACTUALLY runs is lazy registration via
// EnsureResourceType, reached from the dispatch seams with NO env flag on the
// chain. F1b primes confirm THERE.
//
// # THE headline arm: F-reachable
//
// TestF1b_LazyRegister_PrimesConfirm_NoFlagNoDirectCall drives the
// default-config entry path end-to-end: EnsureResourceType (the exact call the
// dispatch seams make) with NO env flags set and NO direct call to any Confirm*
// function → asserts typeConfirmed flips true and the GVR becomes servable. The
// RED control (unwire the two primeConfirmAsyncLocked call sites in
// watcher.go's EnsureResourceType success paths) makes this SAME flow show the
// conjunct-4 snapshot {Registered:true HasSynced:true WatchHealthy:true
// TypeConfirmed:FALSE} — reproduced-and-caught by the assertion below. The RED
// was captured before ship (see docs falsifier artifact).
//
// # Other arms
//
//   - F-skip-guard: same-GV sibling registrations after a confirmed GV do not
//     re-storm discovery beyond one call per distinct GVR-first-registration;
//     a re-EnsureResourceType of an already-confirmed GVR issues ZERO calls.
//   - F-no-stall (-race, K>1): concurrent EnsureResourceType + dispatch-style
//     reads + the ticker over a shared GVR set stay -race clean and never
//     deadlock/serialise on the discovery call (the prime is async, off-lock).
//   - F-offline-degrade: with no discovery client, lazy register issues ZERO
//     discovery calls and IsServable stays degraded-true — offline preserved.
//
// Per feedback_no_special_cases.md: every GVR is a generic customer-style CRD
// GVR; the prime is uniform, no per-GVR carve-out.
//
// All -race -count=1, 3×.
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

// f1b GVRs reuse the walk.example / other.example scheme kinds registered by
// f1WalkListKinds (walk_confirm_prime_f1_test.go, same package) so the fake
// dynamic client can decode the informer initial LISTs.
var (
	f1bGVRa = f1WalkGVRa // walk.example/v1 alphas
	f1bGVRb = f1WalkGVRb // walk.example/v1 betas  (SAME GV as a — sibling)
	f1bGVRc = f1WalkGVRc // other.example/v1 gammas (distinct GV)
)

func newF1bWatcher(t *testing.T) *cache.ResourceWatcher {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		f1WalkScheme(), f1WalkListKinds())
	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil {
		t.Fatalf("expected non-nil watcher under CACHE_ENABLED=true")
	}
	t.Cleanup(func() {
		rw.Stop()
		time.Sleep(50 * time.Millisecond)
	})
	return rw
}

// ensureAndSync registers gvr via EnsureResourceType (the EXACT call the
// dispatch seams make — no flag, no direct Confirm* call) and blocks until its
// informer's initial LIST reconciles (HasSynced).
func ensureAndSync(t *testing.T, rw *cache.ResourceWatcher, gvr schema.GroupVersionResource) {
	t.Helper()
	added, syncCh := rw.EnsureResourceType(gvr)
	if !added {
		t.Fatalf("EnsureResourceType(%s): want added=true (first lazy register)", gvr)
	}
	select {
	case <-syncCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("informer for %s did not sync within 5s", gvr)
	}
}

// waitServable polls IsServable until true or timeout — the async confirm prime
// lands on a watcher goroutine after EnsureResourceType returns, so servability
// is eventually-consistent (bounded by one discovery round-trip). A timeout
// here IS the RED signal: the GVR latched typeConfirmed:false.
func waitServable(t *testing.T, rw *cache.ResourceWatcher, gvr schema.GroupVersionResource, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if rw.IsServable(gvr) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return rw.IsServable(gvr)
}

// TestF1b_LazyRegister_PrimesConfirm_NoFlagNoDirectCall is THE reachability
// falsifier. It drives ONLY the default-config lazy-register call —
// EnsureResourceType — with NO env flags and NO direct Confirm* call, and
// asserts the GVR becomes servable (conjunct 4 typeConfirmed flips true).
//
// RED control (documented + captured pre-ship): comment out the two
// rw.primeConfirmAsyncLocked(gvr) call sites in EnsureResourceType's success
// paths → this test fails with the exact latched snapshot
// {Registered:true HasSynced:true WatchHealthy:true TypeConfirmed:false} and a
// servability timeout — the 1.7.3/1.7.4 boot readout reproduced.
func TestF1b_LazyRegister_PrimesConfirm_NoFlagNoDirectCall(t *testing.T) {
	rw := newF1bWatcher(t)
	disco := newCountingDiscovery(map[string]bool{"walk.example/v1": true})
	rw.SetDiscoveryClient(disco)

	// The ONLY registration action — the default-config seam. No flag, no
	// PrewarmRegisterFromNavigation, no direct ConfirmResourceType(s) call.
	ensureAndSync(t, rw, f1bGVRa)

	// The async prime lands within one discovery round-trip. Poll for it.
	if !waitServable(t, rw, f1bGVRa, 3*time.Second) {
		snap := rw.ServabilitySnapshotFor(f1bGVRa)
		t.Fatalf("F-reachable: a lazily-registered synced GVR MUST become servable via the "+
			"auto-prime with NO flag and NO direct Confirm* call; it did not — this is the "+
			"1.7.3/1.7.4 latched-false readout reproduced. snap=%+v "+
			"(RED control: this is exactly what unwiring primeConfirmAsyncLocked yields)", snap)
	}

	// Prove the conjunct that moved is conjunct 4 (typeConfirmed), and the
	// other three held all along (so the fix addressed the SOLE blocker).
	snap := rw.ServabilitySnapshotFor(f1bGVRa)
	if !snap.Registered || !snap.HasSynced || !snap.WatchHealthy || !snap.TypeConfirmed {
		t.Fatalf("F-reachable: want all four conjuncts true after auto-prime; got %+v", snap)
	}

	// Exactly one discovery call for the GV — the prime is a single round-trip,
	// not a storm.
	if got := disco.callCount("walk.example/v1"); got != 1 {
		t.Fatalf("F-reachable cost: want exactly 1 discovery round-trip for the auto-prime; got %d", got)
	}
}

// TestF1b_LazyRegister_MetadataOnlyPath_PrimesConfirm asserts the SAME
// auto-prime fires on the metadata-only registration success path
// (EnsureResourceTypeMetadataOnly). Reachability must hold on BOTH lazy-register
// success branches, not just the full-unstructured one.
func TestF1b_LazyRegister_MetadataOnlyPath_PrimesConfirm(t *testing.T) {
	rw := newF1bWatcher(t)
	disco := newCountingDiscovery(map[string]bool{"walk.example/v1": true})
	rw.SetDiscoveryClient(disco)

	added, syncCh := rw.EnsureResourceTypeMetadataOnly(f1bGVRa)
	if !added {
		// No metadata client wired in this harness → the metadata-only entry
		// returns (false,nil) by contract; skip rather than assert a false
		// negative. The full-unstructured arm above is the load-bearing one;
		// this arm documents the wiring exists on the metadata branch too and
		// runs meaningfully once a metaClient is present.
		t.Skip("metadata-only entry returned added=false (no metaClient wired in this harness); " +
			"full-unstructured reachability arm covers the prime")
	}
	select {
	case <-syncCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("metadata-only informer for %s did not sync within 5s", f1bGVRa)
	}
	if !waitServable(t, rw, f1bGVRa, 3*time.Second) {
		t.Fatalf("F-reachable (metadata-only): lazily-registered synced GVR must become servable "+
			"via the auto-prime; snap=%+v", rw.ServabilitySnapshotFor(f1bGVRa))
	}
}

// TestF1b_SkipGuard_ReEnsureConfirmedGVR_ZeroDiscovery asserts the
// skip-if-already-confirmed guard: once a GVR is confirmed, a repeat
// EnsureResourceType (dispatch singleflight hit) issues ZERO additional
// discovery calls. It also shows a same-GV SIBLING (betas, same walk.example/v1
// as alphas) costs its own single first-register discovery call — the guard is
// per-GVR-lifetime, bounding lazy-register confirm cost.
func TestF1b_SkipGuard_ReEnsureConfirmedGVR_ZeroDiscovery(t *testing.T) {
	rw := newF1bWatcher(t)
	disco := newCountingDiscovery(map[string]bool{"walk.example/v1": true})
	rw.SetDiscoveryClient(disco)

	// First register + confirm alphas.
	ensureAndSync(t, rw, f1bGVRa)
	if !waitServable(t, rw, f1bGVRa, 3*time.Second) {
		t.Fatalf("setup: alphas must be servable after first register auto-prime; snap=%+v",
			rw.ServabilitySnapshotFor(f1bGVRa))
	}
	afterA := disco.callCount("walk.example/v1")
	if afterA != 1 {
		t.Fatalf("setup: want exactly 1 discovery call after alphas first register; got %d", afterA)
	}

	// Re-EnsureResourceType(alphas) — a dispatch singleflight HIT (added=false).
	// The prime is on the added=true success path only; a hit never reaches
	// primeConfirmAsyncLocked. Belt-and-suspenders: even if it did, the
	// skip-if-already-confirmed guard would short-circuit before any discovery
	// call. Assert ZERO additional calls either way.
	for i := 0; i < 5; i++ {
		added, _ := rw.EnsureResourceType(f1bGVRa)
		if added {
			t.Fatalf("re-EnsureResourceType(alphas): want added=false (singleflight hit); got true")
		}
	}
	// Give any (erroneous) async prime a chance to fire, then assert no growth.
	time.Sleep(200 * time.Millisecond)
	if got := disco.callCount("walk.example/v1"); got != afterA {
		t.Fatalf("F-skip-guard: re-registering an already-confirmed GVR must issue ZERO additional "+
			"discovery calls; grew %d -> %d", afterA, got)
	}

	// A same-GV SIBLING first-registration (betas) is a distinct GVR-lifetime:
	// it pays its own single discovery round-trip (the guard is per-GVR, and the
	// bound is one round-trip per distinct first-register).
	ensureAndSync(t, rw, f1bGVRb)
	if !waitServable(t, rw, f1bGVRb, 3*time.Second) {
		t.Fatalf("betas must be servable after its first register auto-prime; snap=%+v",
			rw.ServabilitySnapshotFor(f1bGVRb))
	}
	if got := disco.callCount("walk.example/v1"); got != afterA+1 {
		t.Fatalf("F-skip-guard sibling: betas first-register should add exactly 1 discovery call; "+
			"got %d (was %d)", got, afterA)
	}
}

// TestF1b_LazyRegister_Race is the -race / no-stall / no-deadlock falsifier.
// It drives K>1 concurrent lazy registrations (distinct GVRs) racing the ticker
// (RefreshDiscovery) and dispatch-style servability reads over the shared
// confirm maps. The prime is ASYNC + off the register lock, so a concurrent
// register must never block on another register's discovery call, and the maps
// (rw.confirmed / rw.watchBroken / rw.lastSyncRV) must stay -race clean.
//
// K>1 and M>1 (feedback_falsifier_shape_must_discriminate): 4 concurrent
// registrants × repeated ticker/reads over a multi-GVR, multi-GV set. A
// degenerate K=1 shape would not exercise the register-vs-register + register-
// vs-ticker map contention this fix introduces.
func TestF1b_LazyRegister_Race(t *testing.T) {
	rw := newF1bWatcher(t)
	disco := newCountingDiscovery(map[string]bool{
		"walk.example/v1":  true,
		"other.example/v1": true,
	})
	rw.SetDiscoveryClient(disco)

	gvrs := []schema.GroupVersionResource{f1bGVRa, f1bGVRb, f1bGVRc}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	// K>1 concurrent registrants each hammering EnsureResourceType over the
	// shared set — the first per-GVR wins added=true and spawns the async prime;
	// the rest are singleflight hits. No registrant may stall on another's prime.
	for k := 0; k < 4; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				for _, gvr := range gvrs {
					rw.EnsureResourceType(gvr)
				}
			}
		}()
	}
	// Ticker path racing the same confirm maps.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 25; i++ {
			rw.RefreshDiscovery(ctx)
		}
	}()
	// Dispatch-style servability reads racing the writers.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			for _, gvr := range gvrs {
				_ = rw.IsServable(gvr)
			}
		}
	}()
	wg.Wait()

	// Convergent verdict: every GVR ends up servable (registered+synced+confirmed).
	for _, gvr := range gvrs {
		if !waitServable(t, rw, gvr, 3*time.Second) {
			t.Fatalf("post-race: %s must be servable; snap=%+v", gvr, rw.ServabilitySnapshotFor(gvr))
		}
	}
}

// TestF1b_LazyRegister_OfflineDegrade asserts the degraded path: with NO
// discovery client wired, lazy register issues ZERO discovery calls (the prime
// short-circuits on disco==nil) and conjunct 4 degrades to true, so the GVR is
// servable — the offline behaviour is preserved, not broken.
func TestF1b_LazyRegister_OfflineDegrade(t *testing.T) {
	rw := newF1bWatcher(t)
	// NO SetDiscoveryClient — disco stays nil (offline).

	ensureAndSync(t, rw, f1bGVRa)

	// Degraded-true: conjunct 4 returns true when disco==nil, so a
	// registered+synced GVR is servable with no prime work at all.
	if !waitServable(t, rw, f1bGVRa, 2*time.Second) {
		t.Fatalf("offline degrade: registered+synced GVR must be servable with no discovery client "+
			"(conjunct 4 degraded-true); snap=%+v", rw.ServabilitySnapshotFor(f1bGVRa))
	}
}
