// discovery_storm_a2_race_test.go — Fix A2 falsifier F-A2d (the mandatory
// concurrent -race test for the private->shared conversion, the 0.30.128
// hazard class — feedback_shared_vs_copy_is_a_concurrency_change).
//
// A2 converts the per-call PRIVATE raw discovery client into ONE
// process-singleton cache-local CachedDiscoveryInterface read by every hot
// /call walker goroutine (DiscoverGroupResources, forceFresh:false) AND
// mutated by the CRD-event path (DiscoverGroupResourcesFresh →
// cached.Invalidate()). This test races concurrent hot-path readers
// (distinct groups) against a SEPARATE goroutine looping the forceFresh
// Invalidate()+re-read path.
//
// Run with: go test -race -count=1 ./internal/cache/...
// PASS criterion: zero data races AND no panic.

package cache

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
)

// multiGroupFlipDiscovery serves several groups, each with one CRD-backed
// resource. Goroutine-safe (the embedded counters are mutex-guarded). It
// is wrapped by NewMemCacheClient inside getCachedDiscovery.
type multiGroupFlipDiscovery struct {
	discovery.DiscoveryInterface // embed nil — unreached methods panic

	mu     sync.Mutex
	groups map[string][]string                        // group -> versions
	res    map[string]map[string][]metav1.APIResource // group -> version -> resources
	gCalls int64
	rCalls int64
}

func (f *multiGroupFlipDiscovery) ServerGroups() (*metav1.APIGroupList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gCalls++
	out := make([]metav1.APIGroup, 0, len(f.groups))
	for g, vs := range f.groups {
		gvfd := make([]metav1.GroupVersionForDiscovery, 0, len(vs))
		for _, v := range vs {
			gvfd = append(gvfd, metav1.GroupVersionForDiscovery{Version: v, GroupVersion: g + "/" + v})
		}
		out = append(out, metav1.APIGroup{Name: g, Versions: gvfd})
	}
	return &metav1.APIGroupList{Groups: out}, nil
}

func (f *multiGroupFlipDiscovery) ServerResourcesForGroupVersion(gv string) (*metav1.APIResourceList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rCalls++
	for g, byV := range f.res {
		for v, rs := range byV {
			if gv == g+"/"+v {
				return &metav1.APIResourceList{GroupVersion: gv, APIResources: rs}, nil
			}
		}
	}
	return &metav1.APIResourceList{GroupVersion: gv}, nil
}

// TestA2_Race_ConcurrentHotReadersVsForceFreshInvalidator is F-A2d.
func TestA2_Race_ConcurrentHotReadersVsForceFreshInvalidator(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	ResetDepsForTest()
	t.Cleanup(ResetDepsForTest)
	ResetNavigationDiscoveredGroupsForTest()
	t.Cleanup(ResetNavigationDiscoveredGroupsForTest)
	ResetDiscoverySingleflightForTest()
	t.Cleanup(ResetDiscoverySingleflightForTest)

	groups := []string{"g0.example.io", "g1.example.io", "g2.example.io", "g3.example.io"}
	res := func(plural, kind string) []metav1.APIResource {
		return []metav1.APIResource{{Name: plural, Kind: kind, Namespaced: true}}
	}
	grpVersions := map[string][]string{}
	grpRes := map[string]map[string][]metav1.APIResource{}
	gvrs := make([]schema.GroupVersionResource, 0, len(groups))
	for i, g := range groups {
		grpVersions[g] = []string{"v1"}
		plural := "kind" + string(rune('a'+i)) + "s"
		grpRes[g] = map[string][]metav1.APIResource{"v1": res(plural, "Kind"+string(rune('A'+i)))}
		gvrs = append(gvrs, schema.GroupVersionResource{Group: g, Version: "v1", Resource: plural})
	}

	rw := newSyntheticRemoveWatcher(t, gvrs...)
	rw.SetRESTConfig(&rest.Config{Host: "https://stub.invalid"})
	SetGlobal(rw)
	t.Cleanup(func() { SetGlobal(nil); rw.Stop() })

	fake := &multiGroupFlipDiscovery{groups: grpVersions, res: grpRes}
	prev := discoveryClientBuilder
	discoveryClientBuilder = func(rc *rest.Config) (discovery.DiscoveryInterface, error) { return fake, nil }
	resetCachedDiscoveryForTest()
	t.Cleanup(func() { discoveryClientBuilder = prev; resetCachedDiscoveryForTest() })

	rc := &rest.Config{Host: "https://fake.test"}

	// Build the singleton up front so all goroutines share it.
	if _, err := getCachedDiscovery(rc); err != nil {
		t.Fatalf("initial cached client build: %v", err)
	}

	const (
		readers       = 12
		readsPerRdr   = 200
		invalidations = 150
	)
	var (
		wg   sync.WaitGroup
		stop atomic.Bool
	)

	// READER goroutines — concurrent forceFresh:false hot-path discovery
	// over distinct groups (the shared cached client's read side).
	wg.Add(readers)
	for r := 0; r < readers; r++ {
		go func(r int) {
			defer wg.Done()
			g := groups[r%len(groups)]
			for i := 0; i < readsPerRdr && !stop.Load(); i++ {
				_, _ = DiscoverGroupResources(context.Background(), rc, g)
			}
		}(r)
	}

	// INVALIDATOR goroutine — SEPARATE from the readers (the new shared-
	// mutation surface). Loops the forceFresh path which Invalidate()s the
	// shared cached client then re-reads.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < invalidations && !stop.Load(); i++ {
			g := groups[i%len(groups)]
			_, _ = DiscoverGroupResourcesFresh(context.Background(), rc, g)
			time.Sleep(time.Microsecond)
		}
	}()

	wg.Wait()
	stop.Store(true)
	// Reaching here without a -race report or a panic is the PASS gate.
}
