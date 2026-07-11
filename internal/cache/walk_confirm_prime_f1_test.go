// walk_confirm_prime_f1_test.go — #130 F1 falsifiers.
//
// FIX under test: the discovery-walk registration path
// (PrewarmRegisterFromNavigation, prewarm.go) now primes ONE scoped
// conjunct-4 confirm (ConfirmResourceTypes) over exactly the GVRs the walk
// newly registers, so a walk-registered GVR becomes IsServable (all four
// conjuncts true) within one discovery round-trip instead of waiting a full
// discoveryRefreshInterval (30s) ticker tick.
//
// Root cause (1.7.3 serve_miss readout): every walk-registered GVR latched
// registered:true hasSynced:true watchHealthy:true typeConfirmed:FALSE —
// conjunct 4 was the sole universal blocker until the ticker.
//
// Falsifiers here (run -race -count=1, 3×):
//
//   - TestF1Walk_ConfirmPrimed_ServableWithoutTicker (F-confirm-primed):
//     after ConfirmResourceTypes, a walk-registered synced GVR is IsServable.
//     RED control: WITHOUT the confirm prime the same GVR is NOT servable
//     (typeConfirmed false) even though registered+synced hold — that is the
//     bug reproduced. A single RefreshDiscovery pass (the ticker) also flips
//     it true, proving the prime just brings that flip forward, not fakes it.
//
//   - TestF1Walk_ConfirmResourceTypes_DedupsPerGV (cost bound): two GVRs
//     sharing one group/version cost exactly ONE
//     ServerResourcesForGroupVersion call, not two.
//
//   - TestF1Walk_ConfirmResourceTypes_Idempotent (F-idempotent/no-regress):
//     a second ConfirmResourceTypes over the same set does not re-storm and
//     leaves the GVR confirmed; an unregistered GVR in the set is a no-op.
//
//   - TestF1Walk_ConfirmResourceTypes_OfflineDegrade (F-offline-degrade):
//     with no discovery client wired, ConfirmResourceTypes issues ZERO
//     discovery calls and IsServable stays degraded-true (conjunct 4 falls
//     back to HasSynced-only) — the degraded path is not broken.
//
// Per feedback_no_special_cases.md: every GVR is a generic customer-style
// CRD GVR; the confirm is uniform, no per-GVR carve-out.
package cache_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/krateoplatformops/snowplow/internal/cache"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// Two customer-style CRD GVRs that SHARE one group/version — the dedup
// dimension. A third GVR in a distinct group/version proves multi-GV.
var (
	f1WalkGVRa = schema.GroupVersionResource{Group: "walk.example", Version: "v1", Resource: "alphas"}
	f1WalkGVRb = schema.GroupVersionResource{Group: "walk.example", Version: "v1", Resource: "betas"}
	f1WalkGVRc = schema.GroupVersionResource{Group: "other.example", Version: "v1", Resource: "gammas"}
)

func f1WalkScheme() *k8sruntime.Scheme {
	// Reuse the RBAC-registered scheme so the bootstrap RBAC informers
	// (registered by NewResourceWatcher) can decode their initial LISTs.
	return newTestScheme()
}

func f1WalkListKinds() map[schema.GroupVersionResource]string {
	// Merge the RBAC bootstrap List kinds — NewResourceWatcher registers the
	// four RBAC GVRs, whose informers LIST at startup and panic without them.
	m := rbacListKinds()
	m[f1WalkGVRa] = "AlphaList"
	m[f1WalkGVRb] = "BetaList"
	m[f1WalkGVRc] = "GammaList"
	return m
}

// countingDiscovery is a discovery double that COUNTS
// ServerResourcesForGroupVersion calls per group/version so the dedup
// falsifier can assert one-call-per-GV. `served` toggles which
// group/versions the fake apiserver reports as serving their type.
type countingDiscovery struct {
	mu     sync.Mutex
	served map[string]bool // groupVersion -> served
	calls  map[string]int  // groupVersion -> call count
}

func newCountingDiscovery(served map[string]bool) *countingDiscovery {
	return &countingDiscovery{served: served, calls: map[string]int{}}
}

func (d *countingDiscovery) ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error) {
	d.mu.Lock()
	d.calls[groupVersion]++
	served := d.served[groupVersion]
	d.mu.Unlock()
	if !served {
		return &metav1.APIResourceList{GroupVersion: groupVersion}, nil
	}
	// Report every resource this GV could serve — the confirm only checks
	// the Resource name appears, so list all three plurals; harmless.
	return &metav1.APIResourceList{
		GroupVersion: groupVersion,
		APIResources: []metav1.APIResource{
			{Name: "alphas", Kind: "Alpha", Namespaced: true},
			{Name: "betas", Kind: "Beta", Namespaced: true},
			{Name: "gammas", Kind: "Gamma", Namespaced: true},
		},
	}, nil
}

func (d *countingDiscovery) callCount(gv string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls[gv]
}

func newF1WalkWatcher(t *testing.T) *cache.ResourceWatcher {
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

// registerAndSync registers gvr via the same EnsureResourceType the walk
// uses and blocks until its informer's initial LIST reconciles (HasSynced).
func registerAndSync(t *testing.T, rw *cache.ResourceWatcher, gvr schema.GroupVersionResource) {
	t.Helper()
	added, syncCh := rw.EnsureResourceType(gvr)
	if !added {
		t.Fatalf("EnsureResourceType(%s): want added=true", gvr)
	}
	select {
	case <-syncCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("informer for %s did not sync within 5s", gvr)
	}
}

// TestF1Walk_ConfirmPrimed_ServableWithoutTicker is the primary F1
// falsifier + its RED control. A walk-registered, synced GVR:
//
//	RED (no confirm prime): IsServable == false — conjunct 4 (typeConfirmed)
//	  is the blocker even though registered+synced hold. This is the bug.
//	GREEN (ConfirmResourceTypes): IsServable == true within one discovery
//	  round-trip — NO ticker wait.
//
// A subsequent RefreshDiscovery (the ticker) also yields true, proving the
// prime brings the SAME flip forward rather than fabricating servability.
func TestF1Walk_ConfirmPrimed_ServableWithoutTicker(t *testing.T) {
	rw := newF1WalkWatcher(t)
	disco := newCountingDiscovery(map[string]bool{"walk.example/v1": true})

	// #130 F1b: register with NO discovery client wired so the lazy-register
	// auto-prime short-circuits (disco==nil guard) — this test's negative
	// control is "registered+synced but UNCONFIRMED ⇒ not-servable", which the
	// auto-prime would otherwise dissolve. Wire discovery AFTER register so the
	// walk-path ConfirmResourceTypes below is the ONLY confirm that runs. (The
	// F1b auto-prime's own reachability is proven in
	// lazy_register_confirm_prime_f1b_test.go; this arm still guards F1's
	// walk-path batch confirm.)
	registerAndSync(t, rw, f1WalkGVRa)
	rw.SetDiscoveryClient(disco)

	// RED control: BEFORE any confirm, the GVR is registered + synced but
	// NOT confirmed → not servable. This is exactly the 1.7.3 readout state.
	if rw.IsServable(f1WalkGVRa) {
		t.Fatalf("RED control invalid: a walk-registered synced GVR must NOT be servable "+
			"before the confirm prime (conjunct 4 typeConfirmed=false); got servable=true")
	}
	snap := rw.ServabilitySnapshotFor(f1WalkGVRa)
	if !snap.Registered || !snap.HasSynced || !snap.WatchHealthy {
		t.Fatalf("RED control setup wrong: want registered+synced+watchHealthy; got %+v", snap)
	}
	if snap.TypeConfirmed {
		t.Fatalf("RED control invalid: typeConfirmed must be false pre-prime; got %+v", snap)
	}

	// GREEN: the F1 fix — prime the scoped confirm over the walk set.
	rw.ConfirmResourceTypes(context.Background(), []schema.GroupVersionResource{f1WalkGVRa})

	if !rw.IsServable(f1WalkGVRa) {
		t.Fatalf("F1: after ConfirmResourceTypes the walk-registered GVR MUST be servable "+
			"without a ticker wait; got servable=false (snap=%+v)", rw.ServabilitySnapshotFor(f1WalkGVRa))
	}

	// Sanity: the ticker path reaches the same verdict — the prime is not a
	// shortcut around real discovery, it is the same confirm brought forward.
	rw.RefreshDiscovery(context.Background())
	if !rw.IsServable(f1WalkGVRa) {
		t.Fatalf("F1: RefreshDiscovery (ticker path) disagrees with the prime; got servable=false")
	}
}

// TestF1Walk_ConfirmResourceTypes_DedupsPerGV asserts the cost bound: two
// GVRs sharing one group/version cost exactly ONE discovery round-trip for
// that GV, and a GVR in a second GV costs exactly one more.
func TestF1Walk_ConfirmResourceTypes_DedupsPerGV(t *testing.T) {
	rw := newF1WalkWatcher(t)
	disco := newCountingDiscovery(map[string]bool{
		"walk.example/v1":  true,
		"other.example/v1": true,
	})

	// #130 F1b: register the three GVRs with NO discovery client wired so the
	// lazy-register auto-prime short-circuits (disco==nil guard) and issues
	// ZERO discovery calls. This isolates the per-GV dedup contract of the
	// walk-path ConfirmResourceTypes BATCH — the property this test exists to
	// prove — from the per-GVR auto-prime (which has no per-GV dedup and would
	// otherwise add same-GV calls, the got==3 the pre-rework test saw). Wire
	// discovery AFTER register, then the batch below is the ONLY discovery
	// caller, so its per-GV dedup is asserted cleanly: a==b share
	// walk.example/v1 ⇒ exactly ONE call for that GV; c is other.example/v1 ⇒
	// exactly one more. (NOT a papering of the count to 3 — the batch really
	// does dedup to 1/GV; the auto-prime's per-GVR cost is asserted separately
	// in TestF1b_SkipGuard_ReEnsureConfirmedGVR_ZeroDiscovery.)
	registerAndSync(t, rw, f1WalkGVRa)
	registerAndSync(t, rw, f1WalkGVRb)
	registerAndSync(t, rw, f1WalkGVRc)
	rw.SetDiscoveryClient(disco)

	rw.ConfirmResourceTypes(context.Background(), []schema.GroupVersionResource{
		f1WalkGVRa, f1WalkGVRb, f1WalkGVRc,
	})

	if got := disco.callCount("walk.example/v1"); got != 1 {
		t.Fatalf("cost bound: want exactly 1 discovery call for shared GV walk.example/v1, got %d", got)
	}
	if got := disco.callCount("other.example/v1"); got != 1 {
		t.Fatalf("cost bound: want exactly 1 discovery call for other.example/v1, got %d", got)
	}
	// All three confirmed → all three servable.
	for _, gvr := range []schema.GroupVersionResource{f1WalkGVRa, f1WalkGVRb, f1WalkGVRc} {
		if !rw.IsServable(gvr) {
			t.Fatalf("want %s servable after per-GV-deduped confirm; got false", gvr)
		}
	}
}

// TestF1Walk_ConfirmResourceTypes_Idempotent asserts no re-storm on a second
// pass and that an UNregistered GVR in the set is a cheap no-op (no panic, no
// spurious confirm).
func TestF1Walk_ConfirmResourceTypes_Idempotent(t *testing.T) {
	rw := newF1WalkWatcher(t)
	disco := newCountingDiscovery(map[string]bool{"walk.example/v1": true})
	rw.SetDiscoveryClient(disco)

	registerAndSync(t, rw, f1WalkGVRa)

	set := []schema.GroupVersionResource{f1WalkGVRa, f1WalkGVRb /* NOT registered */}
	rw.ConfirmResourceTypes(context.Background(), set)
	afterFirst := disco.callCount("walk.example/v1")

	rw.ConfirmResourceTypes(context.Background(), set)
	afterSecond := disco.callCount("walk.example/v1")

	// A second pass DOES query discovery again (it must re-verify a possibly-
	// retracted type) but it is ONE call per GV per pass — not a storm.
	if afterSecond-afterFirst != 1 {
		t.Fatalf("idempotent: second pass should add exactly 1 discovery call for the GV, "+
			"got delta=%d (first=%d second=%d)", afterSecond-afterFirst, afterFirst, afterSecond)
	}
	// The registered GVR stays servable; the unregistered one never becomes
	// servable (no informer to serve from) — no spurious confirm resurrected.
	if !rw.IsServable(f1WalkGVRa) {
		t.Fatalf("idempotent: registered GVR must stay servable across double-confirm")
	}
	if rw.IsServable(f1WalkGVRb) {
		t.Fatalf("idempotent: unregistered GVR must NOT be servable (no informer); got true")
	}
}

// TestF1Walk_ConfirmResourceTypes_Race is the concurrency falsifier. The
// walk's ConfirmResourceTypes writes rw.confirmed / rw.watchBroken /
// rw.lastSyncRV under rw.mu — the SAME maps the discovery-refresh ticker
// (RefreshDiscovery) and the scoped ConfirmResourceType write. This drives
// all three concurrently against a shared GVR set (K>1 confirmers × the
// ticker × concurrent registration) so -race flags any unsynchronised map
// access. Degenerate single-goroutine shapes would not exercise the
// map-under-lock contention this fix introduces.
func TestF1Walk_ConfirmResourceTypes_Race(t *testing.T) {
	rw := newF1WalkWatcher(t)
	disco := newCountingDiscovery(map[string]bool{
		"walk.example/v1":  true,
		"other.example/v1": true,
	})
	rw.SetDiscoveryClient(disco)

	gvrs := []schema.GroupVersionResource{f1WalkGVRa, f1WalkGVRb, f1WalkGVRc}
	for _, gvr := range gvrs {
		registerAndSync(t, rw, gvr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	// K>1 concurrent walk-confirmers over the shared set.
	for k := 0; k < 4; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				rw.ConfirmResourceTypes(ctx, gvrs)
			}
		}()
	}
	// The ticker path, racing the same maps.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 25; i++ {
			rw.RefreshDiscovery(ctx)
		}
	}()
	// The scoped single-GVR confirm, racing the same maps.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 25; i++ {
			rw.ConfirmResourceType(ctx, f1WalkGVRa)
		}
	}()
	wg.Wait()

	// Convergent verdict: all three GVRs confirmed → servable.
	for _, gvr := range gvrs {
		if !rw.IsServable(gvr) {
			t.Fatalf("post-race: %s must be servable; got false (snap=%+v)", gvr, rw.ServabilitySnapshotFor(gvr))
		}
	}
}

// TestF1Walk_ConfirmResourceTypes_OfflineDegrade asserts the degraded path:
// with NO discovery client wired, ConfirmResourceTypes issues zero discovery
// calls and conjunct 4 degrades to true (HasSynced-only), so the GVR is
// servable — the pre-0.30.98 behaviour is preserved, not broken.
func TestF1Walk_ConfirmResourceTypes_OfflineDegrade(t *testing.T) {
	rw := newF1WalkWatcher(t)
	// NO SetDiscoveryClient — disco stays nil (offline).

	registerAndSync(t, rw, f1WalkGVRa)

	// Should not panic and should be a no-op w.r.t. discovery.
	rw.ConfirmResourceTypes(context.Background(), []schema.GroupVersionResource{f1WalkGVRa})

	// Degraded-true: conjunct 4 returns true when disco==nil, so a
	// registered+synced GVR is servable.
	if !rw.IsServable(f1WalkGVRa) {
		t.Fatalf("offline degrade: registered+synced GVR must be servable with no discovery client "+
			"(conjunct 4 degraded-true); got false (snap=%+v)", rw.ServabilitySnapshotFor(f1WalkGVRa))
	}
}
