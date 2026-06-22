// lazy_register_storm_test.go — Fix A1 (discovery storm) falsifier F-A1.
//
// The composition-detail discovery storm (docs/troubleshoot-composition-
// detail-discovery-storm-2026-06-22.md, RC-1): a composition-detail stage
// emits ONE RequestOptions entry per iterator dispatch (28 composition
// kinds for composition.krateo.io), every entry carrying the SAME static
// group. Pre-fix, the AddNavigationDiscoveredGroup+DiscoverGroupResources
// block fired once PER entry → 28× the same synchronous discovery hop on
// the hot resolve path.
//
// F-A1 asserts the per-group dedup: with N entries in group G plus M in a
// distinct group H, DiscoverGroupResources fires EXACTLY ONCE per distinct
// group (2 total here, NOT N+M), AND every distinct GVR is still
// EnsureResourceType'd (the GVR `seen` dedup is unchanged — N+M GVRs
// registered, GVR-deduped).
//
// FAILS pre-fix: discoverGroupResourcesFn would be invoked N+M times (one
// per opts entry sharing a group). PASSES post-fix: 1 per distinct group.

package api

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"testing"

	httpcall "github.com/krateoplatformops/plumbing/http/request"
	"github.com/krateoplatformops/plumbing/ptr"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
)

// TestLazyRegister_PerGroupDedup_KillsStorm is F-A1.
func TestLazyRegister_PerGroupDedup_KillsStorm(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	t.Setenv("PREWARM_ENABLED", "true")

	if !cache.PrewarmEnabled() {
		t.Fatalf("test precondition: PrewarmEnabled() must be true for the discovery block to run")
	}

	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(
		newSchemeForResolverLazyRegister(), listKindsForResolverLazyRegister())
	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	defer rw.Stop()
	cache.SetGlobal(rw)
	t.Cleanup(func() { cache.SetGlobal(nil) })

	// Count discoverGroupResourcesFn invocations per group. Swap the
	// package seam; restore on cleanup.
	var (
		mu        sync.Mutex
		callsByGr = map[string]int{}
	)
	prevFn := discoverGroupResourcesFn
	discoverGroupResourcesFn = func(ctx context.Context, rc *rest.Config, group string) (int, error) {
		mu.Lock()
		callsByGr[group]++
		mu.Unlock()
		return 0, nil
	}
	t.Cleanup(func() { discoverGroupResourcesFn = prevFn })

	// Internal rest config on the context — the SA-credentialed walker
	// pattern that gates the discovery hop (resolve.go).
	ctx := cache.WithInternalRESTConfig(context.Background(), &rest.Config{Host: "https://fake.test"})

	// N=4 entries in composition.krateo.io (distinct namespaces → distinct
	// paths, but the SAME static group + same GVR), M=2 entries in a
	// second distinct group "apps".
	const (
		groupG = "composition.krateo.io"
		groupH = "apps"
	)
	mkPath := func(group, ns, plural string) string {
		return "/apis/" + group + "/v1/namespaces/" + ns + "/" + plural
	}
	opts := []httpcall.RequestOptions{
		{RequestInfo: httpcall.RequestInfo{Verb: ptr.To(http.MethodGet), Path: mkPath(groupG, "ns-01", "compositions")}},
		{RequestInfo: httpcall.RequestInfo{Verb: ptr.To(http.MethodGet), Path: mkPath(groupG, "ns-02", "compositions")}},
		{RequestInfo: httpcall.RequestInfo{Verb: ptr.To(http.MethodGet), Path: mkPath(groupG, "ns-03", "compositions")}},
		{RequestInfo: httpcall.RequestInfo{Verb: ptr.To(http.MethodGet), Path: mkPath(groupG, "ns-04", "compositions")}},
		{RequestInfo: httpcall.RequestInfo{Verb: ptr.To(http.MethodGet), Path: mkPath(groupH, "ns-05", "deployments")}},
		{RequestInfo: httpcall.RequestInfo{Verb: ptr.To(http.MethodGet), Path: mkPath(groupH, "ns-06", "deployments")}},
	}

	lazyRegisterInnerCallPaths(ctx, slog.Default(), opts)

	// PRIMARY F-A1 ASSERTION: exactly 1 discovery call per DISTINCT group.
	mu.Lock()
	defer mu.Unlock()
	if got := callsByGr[groupG]; got != 1 {
		t.Fatalf("F-A1 FAIL: DiscoverGroupResources fired %d× for %s; want exactly 1 "+
			"(per-group dedup must collapse the 4 same-group entries into one hop — the storm)",
			got, groupG)
	}
	if got := callsByGr[groupH]; got != 1 {
		t.Fatalf("F-A1 FAIL: DiscoverGroupResources fired %d× for %s; want exactly 1 "+
			"(a 2nd DISTINCT group must STILL register+discover, not be collapsed by a bare once)",
			got, groupH)
	}
	if len(callsByGr) != 2 {
		t.Fatalf("F-A1 FAIL: discovery fired for %d distinct groups; want 2 (%v)", len(callsByGr), callsByGr)
	}

	// AC — AddNavigationDiscoveredGroup still fires for EVERY distinct
	// group (not collapsed to a bare sync.Once).
	if !cache.IsNavigationDiscoveredGroup(groupG) {
		t.Errorf("F-A1 FAIL: %s not in navDiscoveredGroups", groupG)
	}
	if !cache.IsNavigationDiscoveredGroup(groupH) {
		t.Errorf("F-A1 FAIL: 2nd distinct group %s not in navDiscoveredGroups (collapse bug)", groupH)
	}

	// AC — the GVR `seen` dedup + EnsureResourceType touch count is
	// UNCHANGED: both distinct GVRs are registered (compositions, widgets).
	for _, gvr := range []schema.GroupVersionResource{
		{Group: groupG, Version: "v1", Resource: "compositions"},
		{Group: groupH, Version: "v1", Resource: "deployments"},
	} {
		if rw.ListObjects(gvr, "") == nil {
			t.Errorf("F-A1 FAIL: GVR %s not registered — A1 must not change EnsureResourceType behavior", gvr)
		}
	}
}
