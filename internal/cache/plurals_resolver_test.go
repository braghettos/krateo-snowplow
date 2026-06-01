// plurals_resolver_test.go — Ship 1 / 0.30.225. -race coverage for
// the plurals permanent store + fast paths.
//
// COVERAGE MAP — design §4.4 / PM AC:
//   - Built-in scheme arm (GVRFor / KindForGVR) hits without
//     touching the discovery client (the test installs a panicking
//     builder to falsify any accidental fall-through).
//   - PluralFor against a CRD-backed gvk fires one discovery hop,
//     populates pluralsStore + pluralsKindReverseStore, returns
//     full Info (Plural + Singular + Shorts) byte-identical to
//     the apiserver APIResource shape.
//   - Concurrent PluralFor calls for the same gvk converge on a
//     single Info via sync.Map.LoadOrStore (-race).
//   - ReasonPluralsDiscoveryHop counter ticks once per
//     discovery hop, NOT per store hit.

package cache

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
)

// resetPluralsForTest is the unified cleanup for tests in this file.
// Restores discoveryClientBuilder, zeros pluralsStore +
// pluralsKindReverseStore, zeros fallthrough counters. The
// FallthroughScope ctx is reset by ResetFallthroughCountersForTest.
func resetPluralsForTest(t *testing.T) {
	t.Helper()
	prev := discoveryClientBuilder
	t.Cleanup(func() { discoveryClientBuilder = prev })
	ResetPluralsStoreForTest()
	t.Cleanup(ResetPluralsStoreForTest)
	ResetFallthroughCountersForTest()
	t.Cleanup(ResetFallthroughCountersForTest)
}

// fakeDiscoveryClient is a hand-rolled discovery.DiscoveryInterface
// stub that returns the supplied APIResourceList for a single
// (group, version). We do NOT use discoveryfake.FakeDiscovery here
// because its ServerResourcesForGroupVersion path has tracker /
// scheme registration prerequisites that are overkill for the
// plurals tests. We only need ServerResourcesForGroupVersion to
// return a fixed list; the discovery.DiscoveryInterface is large
// but most methods are unreachable from PluralFor / KindForGVR.
type fakeDiscoveryClient struct {
	discovery.DiscoveryInterface // embed nil — unreached methods panic per the interface guarantee

	gv        string
	resources []metav1.APIResource
	calls     atomic.Uint64
}

func (f *fakeDiscoveryClient) ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error) {
	f.calls.Add(1)
	if groupVersion != f.gv {
		return &metav1.APIResourceList{}, nil
	}
	return &metav1.APIResourceList{
		GroupVersion: groupVersion,
		APIResources: f.resources,
	}, nil
}

func installFakePluralsBuilder(t *testing.T, fake *fakeDiscoveryClient) {
	t.Helper()
	discoveryClientBuilder = func(rc *rest.Config) (discovery.DiscoveryInterface, error) {
		return fake, nil
	}
}

// fakeCtxWithScope returns a ctx with an active FallthroughScope so
// RecordApiserverFallthrough's per-cell counter ticks (otherwise it
// short-circuits — see fallthrough_meter.go:268-275). Uses an
// arbitrary path name so the counter cell is bounded.
func fakeCtxWithScope() context.Context {
	return WithFallthroughScope(context.Background(), "plurals-test")
}

// --- TestPluralFor_DiscoveryArmFullShape ---------------------------------
//
// PluralFor against a CRD-backed gvk fires one discovery hop and
// returns full Info (Plural + Singular + Shorts). Byte-identical
// to the apiserver APIResource shape — this is the gate that
// makes /api-info/names body byte-equal vs the pre-Ship-1 baseline
// possible.
func TestPluralFor_DiscoveryArmFullShape(t *testing.T) {
	resetPluralsForTest(t)
	t.Setenv("CACHE_ENABLED", "true")

	fake := &fakeDiscoveryClient{
		gv: "templates.krateo.io/v1",
		resources: []metav1.APIResource{
			{
				Name:         "restactions",
				SingularName: "restaction",
				Kind:         "RESTAction",
				ShortNames:   []string{"ra"},
				Namespaced:   true,
			},
		},
	}
	installFakePluralsBuilder(t, fake)

	gvk := schema.GroupVersionKind{Group: "templates.krateo.io", Version: "v1", Kind: "RESTAction"}
	ctx := fakeCtxWithScope()
	info, err := PluralFor(ctx, gvk, &rest.Config{})
	if err != nil {
		t.Fatalf("PluralFor: %v", err)
	}
	if info.Plural != "restactions" {
		t.Errorf("Plural: got %q want %q", info.Plural, "restactions")
	}
	if info.Singular != "restaction" {
		t.Errorf("Singular: got %q want %q", info.Singular, "restaction")
	}
	if len(info.Shorts) != 1 || info.Shorts[0] != "ra" {
		t.Errorf("Shorts: got %v want [ra]", info.Shorts)
	}
	if got := fake.calls.Load(); got != 1 {
		t.Errorf("ServerResourcesForGroupVersion calls: got %d want 1", got)
	}
}

// --- TestPluralFor_StoreHITAvoidsDiscovery ------------------------------
//
// Second PluralFor for the same gvk must hit the permanent store
// — discovery client is NOT consulted a second time. This is the
// "store HIT ≤50 ns/op" mechanism (bench gate validates the
// latency; this test validates the count).
func TestPluralFor_StoreHITAvoidsDiscovery(t *testing.T) {
	resetPluralsForTest(t)
	t.Setenv("CACHE_ENABLED", "true")

	fake := &fakeDiscoveryClient{
		gv: "widgets.templates.krateo.io/v1beta1",
		resources: []metav1.APIResource{
			{Name: "panels", SingularName: "panel", Kind: "Panel"},
		},
	}
	installFakePluralsBuilder(t, fake)

	gvk := schema.GroupVersionKind{Group: "widgets.templates.krateo.io", Version: "v1beta1", Kind: "Panel"}
	ctx := fakeCtxWithScope()

	if _, err := PluralFor(ctx, gvk, &rest.Config{}); err != nil {
		t.Fatalf("first PluralFor: %v", err)
	}
	if got := fake.calls.Load(); got != 1 {
		t.Fatalf("after first call: discovery calls = %d, want 1", got)
	}
	// Second call — must hit the store.
	info2, err := PluralFor(ctx, gvk, &rest.Config{})
	if err != nil {
		t.Fatalf("second PluralFor: %v", err)
	}
	if got := fake.calls.Load(); got != 1 {
		t.Errorf("after second call: discovery calls = %d, want 1 (store hit expected)", got)
	}
	if info2.Plural != "panels" {
		t.Errorf("Plural from store: got %q want panels", info2.Plural)
	}
}

// --- TestGVRFor_BuiltinFastPath ------------------------------------------
//
// GVRFor against a built-in GVK (Pod) hits the init() map and
// never touches the discovery client. The test installs a builder
// that panics — any fall-through fails the test loudly.
func TestGVRFor_BuiltinFastPath(t *testing.T) {
	resetPluralsForTest(t)

	discoveryClientBuilder = func(rc *rest.Config) (discovery.DiscoveryInterface, error) {
		t.Fatalf("discoveryClientBuilder must NOT be called for built-in GVK")
		return nil, nil
	}

	cases := []struct {
		gvk  schema.GroupVersionKind
		want schema.GroupVersionResource
	}{
		{schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"},
			schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}},
		{schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
			schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}},
		{schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
			schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}},
		{schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "Role"},
			schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}},
		{schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"},
			schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}},
	}
	for _, c := range cases {
		got, err := GVRFor(context.Background(), c.gvk, nil)
		if err != nil {
			t.Errorf("%s: err=%v", c.gvk, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: GVR got %v want %v", c.gvk, got, c.want)
		}
	}
}

// --- TestKindForGVR_BuiltinFastPath --------------------------------------
//
// KindForGVR against a built-in GVR hits the init() reverse map
// without consulting the discovery client.
func TestKindForGVR_BuiltinFastPath(t *testing.T) {
	resetPluralsForTest(t)

	discoveryClientBuilder = func(rc *rest.Config) (discovery.DiscoveryInterface, error) {
		t.Fatalf("discoveryClientBuilder must NOT be called for built-in GVR")
		return nil, nil
	}

	cases := []struct {
		gvr  schema.GroupVersionResource
		want string
	}{
		{schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}, "Pod"},
		{schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, "Deployment"},
		{schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}, "ConfigMap"},
		{schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}, "CustomResourceDefinition"},
	}
	for _, c := range cases {
		got, err := KindForGVR(context.Background(), c.gvr, nil)
		if err != nil {
			t.Errorf("%s: err=%v", c.gvr, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: Kind got %q want %q", c.gvr, got, c.want)
		}
	}
}

// --- TestKindForGVR_DiscoveryArm -----------------------------------------
//
// KindForGVR against a CRD-backed GVR fires one discovery hop and
// returns the resolved Kind. Subsequent calls hit the reverse
// store — discovery is NOT consulted again.
func TestKindForGVR_DiscoveryArm(t *testing.T) {
	resetPluralsForTest(t)
	t.Setenv("CACHE_ENABLED", "true")

	fake := &fakeDiscoveryClient{
		gv: "composition.krateo.io/v1-2-2",
		resources: []metav1.APIResource{
			{
				Name:         "githubscaffoldingwithcompositionpages",
				SingularName: "githubscaffoldingwithcompositionpage",
				Kind:         "GithubScaffoldingWithCompositionPages",
			},
		},
	}
	installFakePluralsBuilder(t, fake)

	gvr := schema.GroupVersionResource{
		Group:    "composition.krateo.io",
		Version:  "v1-2-2",
		Resource: "githubscaffoldingwithcompositionpages",
	}
	ctx := fakeCtxWithScope()
	kind, err := KindForGVR(ctx, gvr, &rest.Config{})
	if err != nil {
		t.Fatalf("KindForGVR: %v", err)
	}
	if kind != "GithubScaffoldingWithCompositionPages" {
		t.Errorf("Kind: got %q want GithubScaffoldingWithCompositionPages", kind)
	}
	// Second call must hit the reverse store.
	if _, err := KindForGVR(ctx, gvr, &rest.Config{}); err != nil {
		t.Fatalf("second KindForGVR: %v", err)
	}
	if got := fake.calls.Load(); got != 1 {
		t.Errorf("discovery calls: got %d want 1", got)
	}
}

// --- TestPluralFor_RaceSafe ----------------------------------------------
//
// Concurrent PluralFor calls for the SAME gvk converge on a
// single Info via sync.Map.LoadOrStore. -race must report clean.
// We tolerate >1 discovery hop because PluralFor does not
// singleflight (only the store does — losers issue redundant
// discovery hops but drop their fresh Info on the floor); the
// invariant under test is that the RESULT is identical and the
// store ends up with a single, consistent value.
func TestPluralFor_RaceSafe(t *testing.T) {
	resetPluralsForTest(t)
	t.Setenv("CACHE_ENABLED", "true")

	fake := &fakeDiscoveryClient{
		gv: "templates.krateo.io/v1",
		resources: []metav1.APIResource{
			{Name: "restactions", SingularName: "restaction", Kind: "RESTAction", ShortNames: []string{"ra"}},
		},
	}
	installFakePluralsBuilder(t, fake)

	gvk := schema.GroupVersionKind{Group: "templates.krateo.io", Version: "v1", Kind: "RESTAction"}
	ctx := fakeCtxWithScope()
	const N = 64

	var wg sync.WaitGroup
	results := make([]string, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			info, err := PluralFor(ctx, gvk, &rest.Config{})
			if err != nil {
				t.Errorf("goroutine %d: %v", i, err)
				return
			}
			results[i] = info.Plural
		}(i)
	}
	wg.Wait()

	for i, r := range results {
		if r != "restactions" {
			t.Errorf("goroutine %d: got %q want restactions", i, r)
		}
	}
	// Store must contain exactly one entry for the gvk.
	var count int
	pluralsStore.Range(func(_, _ any) bool { count++; return true })
	if count != 1 {
		t.Errorf("pluralsStore size: got %d want 1", count)
	}
}

// --- TestPluralsStore_Accessor -------------------------------------------
//
// PluralsStore() returns the live *sync.Map (not a copy). Used by
// tests / observability — Ship 1 brief calls this out explicitly.
func TestPluralsStore_Accessor(t *testing.T) {
	resetPluralsForTest(t)
	if PluralsStore() == nil {
		t.Fatal("PluralsStore() returned nil")
	}
	if PluralsStore() != &pluralsStore {
		t.Errorf("PluralsStore() did not return the live store")
	}
}

// --- TestPluralFor_DiscoveryCounter --------------------------------------
//
// ReasonPluralsDiscoveryHop ticks once per discovery hop, NOT per
// store hit. The bench gate's monotonic-to-ceiling invariant
// depends on this — store hits MUST NOT increment the counter.
func TestPluralFor_DiscoveryCounter(t *testing.T) {
	resetPluralsForTest(t)
	t.Setenv("CACHE_ENABLED", "true")

	fake := &fakeDiscoveryClient{
		gv: "templates.krateo.io/v1",
		resources: []metav1.APIResource{
			{Name: "restactions", SingularName: "restaction", Kind: "RESTAction"},
		},
	}
	installFakePluralsBuilder(t, fake)

	gvk := schema.GroupVersionKind{Group: "templates.krateo.io", Version: "v1", Kind: "RESTAction"}
	ctx := fakeCtxWithScope()
	for i := 0; i < 5; i++ {
		if _, err := PluralFor(ctx, gvk, &rest.Config{}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	// Counter must tick exactly once (the first miss). The remaining
	// 4 calls hit the store and bypass discovery + the
	// RecordApiserverFallthrough call.
	got := FallthroughCount("plurals-test", gvk.String(), ReasonPluralsDiscoveryHop)
	if got != 1 {
		t.Errorf("ReasonPluralsDiscoveryHop count: got %d want 1", got)
	}
}

// --- TestPluralFor_KindAbsent --------------------------------------------
//
// When the apiserver has no APIResource matching gvk.Kind, PluralFor
// returns a zero Info + nil error (matching the plumbing/cache
// plurals.Get semantics — the /api-info/names handler will then
// surface 404 to the client). The store is NOT poisoned with the
// zero entry — we never want a subsequent successful CRD CREATE
// to be masked by a stale negative.
func TestPluralFor_KindAbsent(t *testing.T) {
	resetPluralsForTest(t)
	t.Setenv("CACHE_ENABLED", "true")

	fake := &fakeDiscoveryClient{
		gv:        "composition.krateo.io/v1-2-2",
		resources: []metav1.APIResource{},
	}
	installFakePluralsBuilder(t, fake)

	gvk := schema.GroupVersionKind{Group: "composition.krateo.io", Version: "v1-2-2", Kind: "GithubScaffoldingWithCompositionPages"}
	ctx := fakeCtxWithScope()
	info, err := PluralFor(ctx, gvk, &rest.Config{})
	if err != nil {
		t.Fatalf("PluralFor (kind absent): err=%v", err)
	}
	if info.Plural != "" {
		t.Errorf("expected zero Plural for absent kind, got %q", info.Plural)
	}
}
