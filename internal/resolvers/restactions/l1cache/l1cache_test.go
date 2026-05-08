// Q-OOM-FIX (v0.25.313 RCA, 2026-05-08) — unit test for Patch F: skip
// identical L2 writes inside resolveAndCacheInner.
//
// The architect's spec scoped Patch F as a small hygiene improvement:
// when the existing L2 entry is byte-equal to the new refiltered slice,
// skip cache.L2Put. This test pins the contract using the
// L2WritesSkippedIdentical / L2Writes counters as the signal — both
// are produced by the only L2 write site in the codebase, so they
// cleanly distinguish "wrote" from "skipped".

package l1cache

import (
	"context"
	"log/slog"
	"testing"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jwtutil"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestResolveAndCache_SkipsIdenticalL2Write — Patch F primary contract.
// Two back-to-back resolves of the same RESTAction produce byte-identical
// refiltered output; the first call writes L2, the second SHOULD skip.
//
// Signal:
//   - L2Writes increments by exactly 1 across both calls.
//   - L2WritesSkippedIdentical increments by exactly 1 on the second call.
func TestResolveAndCache_SkipsIdenticalL2Write(t *testing.T) {
	prevEnabled := cache.L2Enabled()
	cache.SetL2EnabledForTest(true)
	defer cache.SetL2EnabledForTest(prevEnabled)
	cache.FlushL2ForTest()

	mc := cache.NewMem(time.Hour)

	// Same minimal CR shape as prewarm_l2_coverage_test — empty Spec.API
	// and empty Spec.Filter so restactions.Resolve completes without a
	// kube apiserver. Output is deterministic across calls.
	cr := &templates.RESTAction{}
	cr.SetName("ra-patchF-test")
	cr.SetNamespace("ns-patchF-test")

	uns, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cr)
	if err != nil {
		t.Fatalf("ToUnstructured: %v", err)
	}

	identity := "bid-patchF-test"
	username := "test-patchF-user"
	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithLogger(slog.Default()),
		xcontext.WithUserInfo(jwtutil.UserInfo{
			Username: username,
			Groups:   []string{"devs"},
		}),
	)
	ctx = cache.WithCache(ctx, mc)
	ctx = cache.WithBindingIdentity(ctx, identity)

	gvr := schema.GroupVersionResource{
		Group: "templates.krateo.io", Version: "v1", Resource: "restactions",
	}
	l1Key := cache.ResolvedKey(identity, gvr, "ns-patchF-test", "ra-patchF-test", -1, -1)

	in := Input{
		Cache:       mc,
		Obj:         uns,
		ResolvedKey: l1Key,
		AuthnNS:     "krateo-system",
		PerPage:     -1,
		Page:        -1,
	}

	writesBefore := cache.GlobalMetrics.L2Writes.Load()
	skippedBefore := cache.GlobalMetrics.L2WritesSkippedIdentical.Load()

	// First call — must write to L2.
	if _, err := ResolveAndCache(ctx, in); err != nil {
		t.Fatalf("first ResolveAndCache: %v", err)
	}

	writesAfter1 := cache.GlobalMetrics.L2Writes.Load()
	skippedAfter1 := cache.GlobalMetrics.L2WritesSkippedIdentical.Load()

	// First-call assertions: at least one write, no skip.
	//
	// We assert ">= 1" on writes (not "== 1") because the L2 reduction
	// gate may fire on tiny outputs in a future tweak; the contract
	// under test is "wasted second write skipped", not "first write
	// always lands". A skip on the first call would invalidate the
	// experiment so we require zero skips here.
	if writesAfter1-writesBefore < 1 {
		t.Fatalf("first call: L2Writes delta = %d, want >= 1 (precondition for the skip test)",
			writesAfter1-writesBefore)
	}
	if skippedAfter1-skippedBefore != 0 {
		t.Fatalf("first call: L2WritesSkippedIdentical delta = %d, want 0",
			skippedAfter1-skippedBefore)
	}

	// Second call — same Input, same context. resolveAndCacheInner is
	// deterministic at this CR shape, so refiltered output is byte-
	// equal to the cached entry. L2Writes MUST stay flat;
	// L2WritesSkippedIdentical MUST tick exactly once.
	if _, err := ResolveAndCache(ctx, in); err != nil {
		t.Fatalf("second ResolveAndCache: %v", err)
	}

	writesAfter2 := cache.GlobalMetrics.L2Writes.Load()
	skippedAfter2 := cache.GlobalMetrics.L2WritesSkippedIdentical.Load()

	if got := writesAfter2 - writesAfter1; got != 0 {
		t.Fatalf("second call: L2Writes delta = %d, want 0 (the bug Patch F fixes is exactly this re-write)",
			got)
	}
	if got := skippedAfter2 - skippedAfter1; got != 1 {
		t.Fatalf("second call: L2WritesSkippedIdentical delta = %d, want 1", got)
	}

	t.Logf("Patch F verified: writes total=%d, skipped total=%d across two identical calls",
		writesAfter2-writesBefore, skippedAfter2-skippedBefore)
}
