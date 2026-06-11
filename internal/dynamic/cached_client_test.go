// cached_client_test.go — Task #322 (#318-R2) Commit 1 falsifier F-2:
// the boot-smoke unit test whose ABSENCE let the nil-mapper ship 4×
// (the 0.30.226->0.30.231 WithSkipMapper 6-revert class). The existing
// client_test.go is //go:build integration — that is precisely why no
// unit test caught the nil-mapper deref. THIS test is a PLAIN unit test
// (no build tag) so it runs in the default gate.
//
// It mirrors the NON-INTEGRATION process-singleton test shape of
// sa_client_memoisation_test.go (C7): plant/build a singleton, assert
// pointer-identity reuse, assert reset semantics — but here against a
// FAKE discovery client so we can count the downloadAPIs-equivalent
// call.
//
// COVERAGE:
//   - resourceInterfaceFor(Options{GVR:...}) does NOT panic at the
//     mapper deref sites (client.go:139 KindFor / :146 RESTMapping) —
//     the EXACT crash site of the 4 reverts.
//   - a ResourceInterface is returned (non-nil) for a GVR-only Get.
//   - REUSE: the second resolve does NOT re-download discovery — the
//     fake's ServerGroups() (the downloadAPIs-equivalent the non-
//     aggregated memCacheClient.refreshLocked calls, memcache.go:262)
//     is called EXACTLY ONCE across many resolves.
//   - INVALIDATE: after InvalidateSADiscovery() the next resolve DOES
//     re-download (download count increments by exactly 1) — proving
//     mapper.Reset() -> memCacheClient.Invalidate() works.

package dynamic

import (
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	cacheddiscovery "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

// fakeCachedDiscovery is a discovery.DiscoveryInterface stub returning a
// single group (default: the CRD-meta group) with a CRD-backed resource.
// It counts ServerGroups() — the call the (non-aggregated) memCacheClient
// makes on its delegate inside refreshLocked when it (re)downloads the
// API surface (memcache.go:262). Embeds the interface (nil) so any
// unreached method panics loudly rather than silently returning a zero
// value. Mirrors fakeDiscoveryForCRD in internal/cache.
type fakeCachedDiscovery struct {
	discovery.DiscoveryInterface // embed nil — unreached methods panic

	mu            sync.Mutex
	groupsCalls   int
	resourceCalls int
}

func (f *fakeCachedDiscovery) ServerGroups() (*metav1.APIGroupList, error) {
	f.mu.Lock()
	f.groupsCalls++
	f.mu.Unlock()
	return &metav1.APIGroupList{
		Groups: []metav1.APIGroup{
			{
				Name: "apiextensions.k8s.io",
				Versions: []metav1.GroupVersionForDiscovery{
					{Version: "v1", GroupVersion: "apiextensions.k8s.io/v1"},
				},
				PreferredVersion: metav1.GroupVersionForDiscovery{
					Version: "v1", GroupVersion: "apiextensions.k8s.io/v1",
				},
			},
		},
	}, nil
}

func (f *fakeCachedDiscovery) ServerResourcesForGroupVersion(gv string) (*metav1.APIResourceList, error) {
	f.mu.Lock()
	f.resourceCalls++
	f.mu.Unlock()
	if gv != "apiextensions.k8s.io/v1" {
		return &metav1.APIResourceList{}, nil
	}
	return &metav1.APIResourceList{
		GroupVersion: gv,
		APIResources: []metav1.APIResource{
			{
				Name:         "customresourcedefinitions",
				SingularName: "customresourcedefinition",
				Namespaced:   false,
				Kind:         "CustomResourceDefinition",
			},
		},
	}, nil
}

func (f *fakeCachedDiscovery) groupsCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.groupsCalls
}

// installFakeCachedDiscovery swaps the discovery seam to return the given
// fake, restoring the original on cleanup, and clears the singleton both
// before and after so tests do not bleed state.
func installFakeCachedDiscovery(t *testing.T, fake discovery.DiscoveryInterface) {
	t.Helper()
	resetSADiscoveryForTest()
	orig := discoveryClientForConfigFn
	discoveryClientForConfigFn = func(rc *rest.Config) (discovery.DiscoveryInterface, error) {
		return fake, nil
	}
	t.Cleanup(func() {
		discoveryClientForConfigFn = orig
		resetSADiscoveryForTest()
	})
}

// crdMetaGVR is the GVR ValidateObjectStatus passes to Get (schema.go:87)
// — apiextensions.k8s.io/v1/customresourcedefinitions, a fixed built-in.
var crdMetaGVR = schema.GroupVersionResource{
	Group:    "apiextensions.k8s.io",
	Version:  "v1",
	Resource: "customresourcedefinitions",
}

// resolveOnce builds (or reuses) the singleton, type-asserts to the
// concrete *unstructuredClient (in-package), and exercises
// resourceInterfaceFor(Options{GVR:...}) — the path that hits
// uc.mapper.KindFor (client.go:139) then uc.mapper.RESTMapping
// (client.go:146), i.e. the exact deref site the WithSkipMapper reverts
// crashed on. Returns the ResourceInterface so the caller can assert it
// is non-nil. We stop at resourceInterfaceFor (no real Get) so no
// apiserver hop is needed against the dummy rc.
func resolveOnce(t *testing.T, rc *rest.Config) {
	t.Helper()
	cli, err := SharedSADiscoveryClient(rc)
	if err != nil {
		t.Fatalf("SharedSADiscoveryClient errored: %v", err)
	}
	if cli == nil {
		t.Fatalf("SharedSADiscoveryClient returned nil Client on success")
	}
	uc, ok := cli.(*unstructuredClient)
	if !ok {
		t.Fatalf("SharedSADiscoveryClient returned %T, want *unstructuredClient", cli)
	}

	// THE LOAD-BEARING NO-PANIC ASSERTION: resourceInterfaceFor derefs
	// uc.mapper unconditionally (KindFor at client.go:139 for GVR-only
	// opts, RESTMapping at :146). With a nil mapper (the WithSkipMapper
	// regression) this panics. With the cached mapper it returns a
	// ResourceInterface.
	ri, err := uc.resourceInterfaceFor(Options{GVR: crdMetaGVR})
	if err != nil {
		t.Fatalf("resourceInterfaceFor(Options{GVR: crdGVR}) errored: %v "+
			"(expected a resolved ResourceInterface — the cached mapper must "+
			"map the CRD GVR from the fake discovery)", err)
	}
	if ri == nil {
		t.Fatalf("resourceInterfaceFor returned a nil ResourceInterface without error")
	}
}

// TestSharedSADiscoveryClient_BootSmoke_NoPanic_ReuseAndInvalidate is the
// F-2 boot-smoke falsifier. RED on a hypothetical nil-mapper build (the
// resourceInterfaceFor deref panics); GREEN on the cached client.
func TestSharedSADiscoveryClient_BootSmoke_NoPanic_ReuseAndInvalidate(t *testing.T) {
	fake := &fakeCachedDiscovery{}
	installFakeCachedDiscovery(t, fake)

	rc := &rest.Config{Host: "https://boot-smoke.invalid"}

	// First resolve — builds the singleton, first KindFor downloads
	// discovery exactly once.
	resolveOnce(t, rc)
	if got := fake.groupsCallCount(); got != 1 {
		t.Fatalf("after first resolve: ServerGroups() called %d times, want 1 "+
			"(the boot download)", got)
	}

	// REUSE: 9 more resolves must NOT re-download — the singleton's
	// mapper delegate is warm. ServerGroups() stays at 1. This is the
	// N-per-call -> 1 win: the baseline probe shows N fresh clients call
	// ServerGroups() N times; the singleton calls it ONCE.
	for i := 0; i < 9; i++ {
		resolveOnce(t, rc)
	}
	if got := fake.groupsCallCount(); got != 1 {
		t.Fatalf("after 10 resolves on the warm singleton: ServerGroups() called %d times, "+
			"want 1 — discovery is being re-downloaded per call (reuse broken)", got)
	}

	// INVALIDATE: InvalidateSADiscovery() -> mapper.Reset() ->
	// memCacheClient.Invalidate() + nil delegate. The NEXT resolve must
	// re-download exactly once (count 1 -> 2).
	InvalidateSADiscovery()
	resolveOnce(t, rc)
	if got := fake.groupsCallCount(); got != 2 {
		t.Fatalf("after InvalidateSADiscovery() + one resolve: ServerGroups() called %d times, "+
			"want 2 — Reset() did not force a re-download (invalidation broken)", got)
	}

	// And subsequent resolves are warm again (stays at 2).
	for i := 0; i < 5; i++ {
		resolveOnce(t, rc)
	}
	if got := fake.groupsCallCount(); got != 2 {
		t.Fatalf("after re-warm: ServerGroups() called %d times, want 2 "+
			"(the post-invalidate rebuild should be reused)", got)
	}
}

// TestSharedSADiscoveryClient_Memoised pins the pointer-identity reuse
// contract (mirrors sa_client_memoisation_test.go): repeated calls with
// the same rc return the SAME Client pointer.
func TestSharedSADiscoveryClient_Memoised(t *testing.T) {
	fake := &fakeCachedDiscovery{}
	installFakeCachedDiscovery(t, fake)

	rc := &rest.Config{Host: "https://memoised.invalid"}

	cli1, err := SharedSADiscoveryClient(rc)
	if err != nil {
		t.Fatalf("call 1 errored: %v", err)
	}
	for i := 0; i < 10; i++ {
		cliN, err := SharedSADiscoveryClient(rc)
		if err != nil {
			t.Fatalf("call %d errored: %v", i+2, err)
		}
		if cliN != cli1 {
			t.Fatalf("call %d returned a DIFFERENT Client pointer than call 1 — "+
				"the singleton is not shared (got %p want %p)", i+2, cliN, cli1)
		}
	}
}

// TestSharedSADiscoveryClient_NilRC_Errors_NoCache pins the transparent-
// fallback contract: a nil rc returns an error WITHOUT caching (so
// ValidateObjectStatus falls back to dynamic.NewClient), and a later
// valid call still builds the singleton.
func TestSharedSADiscoveryClient_NilRC_Errors_NoCache(t *testing.T) {
	fake := &fakeCachedDiscovery{}
	installFakeCachedDiscovery(t, fake)

	if _, err := SharedSADiscoveryClient(nil); err == nil {
		t.Fatalf("SharedSADiscoveryClient(nil) returned no error — the fallback "+
			"contract requires an error on nil rc so ValidateObjectStatus can "+
			"fall back to dynamic.NewClient")
	}

	// Singleton must still be unbuilt (nil-rc error did not cache).
	saDiscoveryMu.RLock()
	inst := saDiscoveryInstance
	saDiscoveryMu.RUnlock()
	if inst != nil {
		t.Fatalf("nil-rc error path cached a singleton (%+v) — error paths must NOT cache", inst)
	}

	// A later valid call still works.
	rc := &rest.Config{Host: "https://post-nil.invalid"}
	if _, err := SharedSADiscoveryClient(rc); err != nil {
		t.Fatalf("valid call after nil-rc error failed: %v", err)
	}
}

// TestInvalidateSADiscovery_BeforeBuild_NoPanic pins that invalidating
// before the singleton is built is a soft no-op (the bridge may fire on a
// CRD event before any ValidateObjectStatus has built the singleton).
func TestInvalidateSADiscovery_BeforeBuild_NoPanic(t *testing.T) {
	resetSADiscoveryForTest()
	t.Cleanup(resetSADiscoveryForTest)
	// Must not panic with no singleton built.
	InvalidateSADiscovery()
}

// --- RED-side baseline: the N-per-call cost the singleton eliminates ----------

// newPerCallClient reproduces the EXACT pre-fix construction at
// internal/dynamic/client.go:18-39 (NewClient) but with the fake
// discovery substituted, so each call builds a FRESH memCacheClient +
// DeferredDiscoveryRESTMapper — exactly what dynamic.NewClient(rc) did
// per child GET inside ValidateObjectStatus before this ship. A real
// *dynamic.DynamicClient is built from a dummy rc (no network — we stop
// at resourceInterfaceFor, the KindFor deref site).
func newPerCallClient(t *testing.T, disco discovery.DiscoveryInterface) *unstructuredClient {
	t.Helper()
	dynClient, err := dynamic.NewForConfig(&rest.Config{Host: "https://percall.invalid"})
	if err != nil {
		t.Fatalf("dynamic.NewForConfig: %v", err)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(
		cacheddiscovery.NewMemCacheClient(disco),
	)
	return &unstructuredClient{
		dynamicClient:   dynClient,
		discoveryClient: disco,
		mapper:          mapper,
		converter:       runtime.DefaultUnstructuredConverter,
	}
}

// TestPerCallClient_DownloadsEachCall is the RED-side baseline of the
// headline discrimination "fake-discovery download call-count goes from
// N-per-call to 1 when the singleton is swapped in". It pins the
// N-per-call cost the PRE-FIX per-call dynamic.NewClient pattern paid:
// K fresh clients each re-download discovery on their first GVR-only
// resolve, so the fake's ServerGroups() (the downloadAPIs-equivalent the
// non-aggregated memCacheClient.refreshLocked calls) fires K times.
//
// The GREEN side is TestSharedSADiscoveryClient_BootSmoke_NoPanic_Reuse
// AndInvalidate above: 10 resolves on the singleton call ServerGroups()
// exactly ONCE. The two together are the falsifier-first RED/GREEN
// evidence (N=5 -> 1).
func TestPerCallClient_DownloadsEachCall(t *testing.T) {
	const K = 5
	fake := &fakeCachedDiscovery{}

	for i := 0; i < K; i++ {
		// One fresh client per iteration == one fresh memCacheClient +
		// DeferredDiscoveryRESTMapper, exactly like the pre-fix
		// dynamic.NewClient(rc) inside ValidateObjectStatus.
		cli := newPerCallClient(t, fake)
		if _, err := cli.resourceInterfaceFor(Options{GVR: crdMetaGVR}); err != nil {
			t.Fatalf("iteration %d resourceInterfaceFor errored: %v", i, err)
		}
	}

	got := fake.groupsCallCount()
	t.Logf("RED baseline (per-call NewClient): ServerGroups() called %d times across %d "+
		"fresh clients (N-per-call discovery download). GREEN (singleton) = 1.", got, K)
	if got != K {
		t.Fatalf("expected per-call download N=%d (one ServerGroups per fresh client); got %d",
			K, got)
	}
}
