// discovery_storm_a2_test.go — Fix A2 (discovery storm) falsifier suite.
//
// A2 swaps the per-call RAW discovery client for a process-singleton
// cache-local CachedDiscoveryInterface (memcache.NewMemCacheClient) and
// splits DiscoverGroupResources into:
//
//   - DiscoverGroupResources (forceFresh:false) — the hot /call walker
//     path; version-complete short-circuit when the cache is Fresh() AND
//     every served version's GVR is already registered.
//   - DiscoverGroupResourcesFresh (forceFresh:true) — the CRD-event path;
//     Invalidate()s this group then re-reads the apiserver BEFORE the
//     registration walk (no stale read on CREATE/UPDATE).
//
// Falsifiers:
//   F-A2a (HARD GATE) — CRD UPDATE that newly-serves G@v2 REGISTERS the
//     v2 GVR (not just ticks DiscoveryInvoked). Fails a blind-permanent
//     cache, passes forceFresh.
//   F-A2b — a 2nd hot-path call on a fully-registered group does ZERO
//     apiserver round-trips (ServerResourcesForGroupVersion call-count
//     unchanged).
//   F-A2c — modePassthrough (cache off) never builds the cached client;
//     raw path identical (no discovery hop).
//
// The -race falsifier F-A2d lives in discovery_storm_a2_race_test.go.

package cache

import (
	"context"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
)

// flipDiscovery is a discovery.DiscoveryInterface stub for one group that
// can be flipped from serving one version-set to another at runtime, and
// counts ServerGroups + ServerResourcesForGroupVersion calls (the
// apiserver round-trip proxies). It is wrapped by NewMemCacheClient inside
// getCachedDiscovery, so a cache HIT serves from memory and these counters
// DO NOT advance — that is exactly what F-A2b asserts.
type flipDiscovery struct {
	discovery.DiscoveryInterface // embed nil — unreached methods panic

	group string

	mu            sync.Mutex
	versions      []string                        // served versions (flippable)
	resByVersion  map[string][]metav1.APIResource // GVRs per version
	groupsCalls   int
	resourceCalls int
}

func (f *flipDiscovery) ServerGroups() (*metav1.APIGroupList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groupsCalls++
	gvfd := make([]metav1.GroupVersionForDiscovery, 0, len(f.versions))
	for _, v := range f.versions {
		gvfd = append(gvfd, metav1.GroupVersionForDiscovery{Version: v, GroupVersion: f.group + "/" + v})
	}
	return &metav1.APIGroupList{Groups: []metav1.APIGroup{{Name: f.group, Versions: gvfd}}}, nil
}

func (f *flipDiscovery) ServerResourcesForGroupVersion(gv string) (*metav1.APIResourceList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resourceCalls++
	for _, v := range f.versions {
		if gv == f.group+"/"+v {
			return &metav1.APIResourceList{GroupVersion: gv, APIResources: f.resByVersion[v]}, nil
		}
	}
	return &metav1.APIResourceList{GroupVersion: gv}, nil
}

func (f *flipDiscovery) flipTo(versions []string) {
	f.mu.Lock()
	f.versions = versions
	f.mu.Unlock()
}

func (f *flipDiscovery) resourceCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resourceCalls
}

// installFlipDiscovery swaps the cache discoveryClientBuilder seam to
// return fake AND resets the cached-discovery singleton so getCachedDiscovery
// rebuilds around it.
func installFlipDiscovery(t *testing.T, fake *flipDiscovery) {
	t.Helper()
	prev := discoveryClientBuilder
	discoveryClientBuilder = func(rc *rest.Config) (discovery.DiscoveryInterface, error) {
		return fake, nil
	}
	resetCachedDiscoveryForTest()
	t.Cleanup(func() {
		discoveryClientBuilder = prev
		resetCachedDiscoveryForTest()
	})
}

// ghscpGVR / gvr helpers — the synthetic composition resource the fake serves.
const (
	a2Group  = "composition.krateo.io"
	a2Plural = "githubscaffoldingwithcompositionpages"
	a2Kind   = "GHSCP"
)

func a2GVR(version string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: a2Group, Version: version, Resource: a2Plural}
}

func a2Resources() []metav1.APIResource {
	return []metav1.APIResource{{Name: a2Plural, Kind: a2Kind, Namespaced: true}}
}

// a2RealWatcher builds a CACHE_ENABLED watcher seeded for the composition
// GVRs at v1 + v2, sets it as Global, and registers cleanup.
func a2RealWatcher(t *testing.T) *ResourceWatcher {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")
	rw := newSyntheticRemoveWatcher(t, a2GVR("v1"), a2GVR("v2"))
	rw.SetRESTConfig(&rest.Config{Host: "https://stub.invalid"})
	SetGlobal(rw)
	t.Cleanup(func() { SetGlobal(nil); rw.Stop() })
	return rw
}

// --- F-A2a (HARD GATE) — CRD UPDATE newly-serves v2 → v2 GVR REGISTERED ---

// TestA2_ForceFresh_RegistersNewlyServedVersion is F-A2a.
//
// Primes the group at v1 (DiscoverGroupResources → cache populated, v1 GVR
// registered). Then flips the fake to serve v1+v2 and fires a CRD UPDATE
// through the real handlers.UpdateFunc. Post-fix, the forceFresh CRD-event
// path Invalidate()s the cache, re-reads the apiserver, and REGISTERS the
// v2 GVR.
//
// RED against a blind-permanent cache: a memoized A2 with NO forceFresh
// would serve the stale v1-only discovery on the UPDATE → v2 GVR never
// registered → IsRegistered(v2)==false. The HARD GATE is the
// IsRegistered(v2) assertion, NOT merely DiscoveryInvoked ticking.
func TestA2_ForceFresh_RegistersNewlyServedVersion(t *testing.T) {
	withCleanCRDDiscovery(t)
	ResetDepsForTest()
	t.Cleanup(ResetDepsForTest)

	rw := a2RealWatcher(t)

	fake := &flipDiscovery{
		group:    a2Group,
		versions: []string{"v1"},
		resByVersion: map[string][]metav1.APIResource{
			"v1": a2Resources(),
			"v2": a2Resources(),
		},
	}
	installFlipDiscovery(t, fake)
	SetProcessSARestConfig(&rest.Config{Host: "https://fake.test"})

	// PRIME: hot-path discovery at v1 → v1 GVR registered, cache populated.
	if _, err := DiscoverGroupResources(context.Background(), ProcessSARestConfig(), a2Group); err != nil {
		t.Fatalf("prime DiscoverGroupResources(v1): %v", err)
	}
	if !rw.IsRegistered(a2GVR("v1")) {
		t.Fatalf("prime FAIL: v1 GVR not registered after initial discovery")
	}
	if rw.IsRegistered(a2GVR("v2")) {
		t.Fatalf("prime FAIL: v2 GVR registered before it was served (test setup bug)")
	}
	spawnedBefore := DiscoveryGVRsSpawned(a2Group)

	// FLIP: apiserver now serves v1+v2.
	fake.flipTo([]string{"v1", "v2"})

	// Fire a CRD UPDATE through the REAL handler closure (the production
	// path: handlers.UpdateFunc → submitCRDLifecycleEvent → worker →
	// triggerCRDDiscovery → DiscoverGroupResourcesFresh).
	crdGVR := CRDGVRForTest()
	gw := newGateWatcher()
	ch := make(chan struct{})
	close(ch)
	gw.syncCh[crdGVR] = ch
	handlers := gw.depEventHandlers(crdGVR)

	oldBO := crdBytesObjMultiVersion(t, "ghscp."+a2Group, a2Group, a2Plural,
		[]versionSpec{{Name: "v1", Served: true}})
	newBO := crdBytesObjMultiVersion(t, "ghscp."+a2Group, a2Group, a2Plural,
		[]versionSpec{{Name: "v1", Served: true}, {Name: "v2", Served: true}})

	handlers.UpdateFunc(oldBO, newBO)

	if !WaitCRDDiscoveryProcessedForTest(1, 3000) {
		t.Fatalf("F-A2a FAIL: worker did not process the CRD UPDATE in 3s; %s",
			crdDiscoveryStatsString())
	}

	// HARD GATE: the v2 GVR was REGISTERED (not just DiscoveryInvoked).
	if !rw.IsRegistered(a2GVR("v2")) {
		t.Fatalf("F-A2a FAIL (HARD GATE): CRD UPDATE serving the new v2 version did NOT "+
			"register the v2 GVR. A blind-permanent A2 cache served stale v1-only "+
			"discovery. forceFresh must Invalidate + re-read BEFORE the walk. %s",
			crdDiscoveryStatsString())
	}
	if got := DiscoveryGVRsSpawned(a2Group); got <= spawnedBefore {
		t.Fatalf("F-A2a FAIL: DiscoveryGVRsSpawned(%s)=%d did not increase past %d "+
			"(the v2 GVR registration must bump the spawned counter)", a2Group, got, spawnedBefore)
	}
}

// --- F-A2b — hot-path warm short-circuit: ZERO apiserver round-trips ------

// TestA2_HotPath_WarmShortCircuit_NoRoundTrips is F-A2b.
//
// Primes the group fully (v1 registered, cache Fresh). A 2nd hot-path
// DiscoverGroupResources call — with every served version already
// registered — must short-circuit with ZERO new apiserver round-trips
// (ServerResourcesForGroupVersion call-count unchanged), because the
// version-complete predicate is satisfied and reads come from the memcache.
func TestA2_HotPath_WarmShortCircuit_NoRoundTrips(t *testing.T) {
	withCleanCRDDiscovery(t)
	ResetDepsForTest()
	t.Cleanup(ResetDepsForTest)

	rw := a2RealWatcher(t)

	fake := &flipDiscovery{
		group:        a2Group,
		versions:     []string{"v1"},
		resByVersion: map[string][]metav1.APIResource{"v1": a2Resources()},
	}
	installFlipDiscovery(t, fake)
	rc := &rest.Config{Host: "https://fake.test"}

	// PRIME — first call does the full walk + populates the cache.
	if _, err := DiscoverGroupResources(context.Background(), rc, a2Group); err != nil {
		t.Fatalf("prime: %v", err)
	}
	if !rw.IsRegistered(a2GVR("v1")) {
		t.Fatalf("prime FAIL: v1 GVR not registered")
	}
	callsAfterPrime := fake.resourceCallCount()

	// 2nd + 3rd hot-path calls — warm cache + every served version
	// registered → version-complete short-circuit → ZERO new round-trips.
	for i := 0; i < 2; i++ {
		if _, err := DiscoverGroupResources(context.Background(), rc, a2Group); err != nil {
			t.Fatalf("warm call %d: %v", i, err)
		}
	}

	if got := fake.resourceCallCount(); got != callsAfterPrime {
		t.Fatalf("F-A2b FAIL: ServerResourcesForGroupVersion call-count moved from %d to %d "+
			"on warm hot-path calls; want UNCHANGED (the version-complete short-circuit "+
			"must serve from the memcache with ZERO apiserver round-trips)", callsAfterPrime, got)
	}
}

// --- F-A2c — cache off (modePassthrough) never builds the cached client ----

// TestA2_CacheOff_NoCachedClientBuilt is F-A2c.
//
// Under modePassthrough the cached discovery client is NEVER built (the
// build site is AFTER the passthrough guard), so DiscoverGroupResources is
// a (0,nil) no-op and the raw path stays byte-identical. We assert the
// fake discovery client received ZERO calls AND the singleton stays nil.
func TestA2_CacheOff_NoCachedClientBuilt(t *testing.T) {
	withCleanCRDDiscovery(t)

	// A passthrough-mode watcher as Global.
	rw := &ResourceWatcher{mode: modePassthrough}
	SetGlobal(rw)
	t.Cleanup(func() { SetGlobal(nil) })

	fake := &flipDiscovery{
		group:        a2Group,
		versions:     []string{"v1"},
		resByVersion: map[string][]metav1.APIResource{"v1": a2Resources()},
	}
	installFlipDiscovery(t, fake)
	rc := &rest.Config{Host: "https://fake.test"}

	n, err := DiscoverGroupResources(context.Background(), rc, a2Group)
	if err != nil || n != 0 {
		t.Fatalf("F-A2c FAIL: passthrough DiscoverGroupResources returned (%d,%v); want (0,nil)", n, err)
	}
	if fake.groupsCalls != 0 || fake.resourceCalls != 0 {
		t.Fatalf("F-A2c FAIL: cache-off path made discovery calls (groups=%d res=%d); "+
			"want 0/0 (raw path must be byte-identical, no cached client built)",
			fake.groupsCalls, fake.resourceCalls)
	}
	cachedDiscoveryMu.RLock()
	built := cachedDiscoveryClient != nil
	cachedDiscoveryMu.RUnlock()
	if built {
		t.Fatalf("F-A2c FAIL: cached discovery client was built under modePassthrough; " +
			"the build site must be AFTER the passthrough guard")
	}
}
