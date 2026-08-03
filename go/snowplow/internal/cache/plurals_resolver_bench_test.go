// plurals_resolver_bench_test.go — Ship 1 / 0.30.225 bench gates.
//
// Bench gates per the dev brief:
//
//   - Built-in arm ≤100 ns/op, zero allocs/op  → BenchmarkGVRFor_Builtin
//                                                 + BenchmarkKindForGVR_Builtin
//   - Store HIT  ≤50 ns/op                     → BenchmarkPluralFor_StoreHIT
//
// The bench file is suffixed _bench_test.go so it lives alongside
// the unit test file but can be invoked separately via the
// dev-precommit Gate 3 command:
//
//   go test -bench=. -benchmem -benchtime=3x ./internal/cache/ -run='^$'
//
// (the -run='^$' filter excludes the unit tests so bench output is
// not noisy with regular test logs).

package cache

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
)

// BenchmarkGVRFor_Builtin measures the built-in scheme fast path
// for GVRFor. Pod is the canonical built-in; the lookup is a map
// load against the init()-populated builtinGVKToGVR.
//
// GATE: ≤100 ns/op, 0 allocs/op.
func BenchmarkGVRFor_Builtin(b *testing.B) {
	ctx := context.Background()
	gvk := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = GVRFor(ctx, gvk, nil)
	}
}

// BenchmarkKindForGVR_Builtin measures the built-in scheme fast
// path for KindForGVR. Same gate as GVRFor: ≤100 ns/op, 0
// allocs/op.
func BenchmarkKindForGVR_Builtin(b *testing.B) {
	ctx := context.Background()
	gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = KindForGVR(ctx, gvr, nil)
	}
}

// BenchmarkPluralFor_StoreHIT measures the permanent-store fast
// path for PluralFor. We pre-populate the store via one discovery
// hop, then time subsequent calls.
//
// GATE: ≤50 ns/op.
func BenchmarkPluralFor_StoreHIT(b *testing.B) {
	// Pre-populate via fake discovery so the gvk lands in the
	// permanent store, then swap to a panicking builder to catch
	// any accidental discovery hop during the timed loop.
	ResetPluralsStoreForTest()
	prev := discoveryClientBuilder
	defer func() {
		discoveryClientBuilder = prev
		ResetPluralsStoreForTest()
	}()
	b.Setenv("CACHE_ENABLED", "true")

	fake := &fakeDiscoveryClient{
		gv: "templates.krateo.io/v1",
		resources: []metav1.APIResource{
			{Name: "restactions", SingularName: "restaction", Kind: "RESTAction", ShortNames: []string{"ra"}},
		},
	}
	discoveryClientBuilder = func(rc *rest.Config) (discovery.DiscoveryInterface, error) {
		return fake, nil
	}

	gvk := schema.GroupVersionKind{Group: "templates.krateo.io", Version: "v1", Kind: "RESTAction"}
	ctx := WithFallthroughScope(context.Background(), "plurals-bench")
	// Hoist the *rest.Config out of the hot loop — `&rest.Config{}`
	// per iteration heap-allocates and dominates the per-op cost.
	// The fast path never dereferences rc (store HIT short-circuits
	// before the discovery branch); we keep a single pointer alive
	// for the entire benchmark so allocs/op == 0.
	rc := &rest.Config{}
	if _, err := PluralFor(ctx, gvk, rc); err != nil {
		b.Fatalf("pre-populate: %v", err)
	}
	// Now swap to panic-builder; the store-hit path must never
	// invoke the builder.
	discoveryClientBuilder = func(rc *rest.Config) (discovery.DiscoveryInterface, error) {
		b.Fatalf("discoveryClientBuilder invoked during store-hit benchmark")
		return nil, nil
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = PluralFor(ctx, gvk, rc)
	}
}
