// lazy_register_test.go — Tag 0.30.92 falsifier: every inner-call path
// the resolver dispatches MUST register an informer via
// cache.Global().EnsureResourceType BEFORE httpcall.Do fires. Without
// this wire-up, downstream GVRs (compositions, sidebar widgets, etc.)
// have no DeleteFunc handler, and DepTracker.OnDelete never fires —
// the 0.30.91 probe failure (`evict_delete=0` after deliberate DELETE).
//
// We exercise lazyRegisterInnerCallPaths directly with a fake dynamic
// client + a real ResourceWatcher, mirroring the dispatchers-package
// test pattern (TestRecordWidgetDeps_TriggersEnsureResourceType).

package api

import (
	"context"
	"log/slog"
	"net/http"
	"testing"

	httpcall "github.com/krateoplatformops/plumbing/http/request"
	"github.com/krateoplatformops/plumbing/ptr"
	"github.com/krateoplatformops/snowplow/internal/cache"
	rbacv1 "k8s.io/api/rbac/v1"
	corev1 "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func newSchemeForResolverLazyRegister() *k8sruntime.Scheme {
	sch := k8sruntime.NewScheme()
	_ = rbacv1.AddToScheme(sch)
	_ = corev1.AddToScheme(sch)
	return sch
}

// listKindsForResolverLazyRegister covers (a) the eager-registered RBAC
// GVRs and (b) the downstream GVRs the test below feeds into
// lazyRegisterInnerCallPaths.
func listKindsForResolverLazyRegister() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		// Eager-registered RBAC set.
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}:                "RoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}:         "RoleBindingList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}:         "ClusterRoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}: "ClusterRoleBindingList",
		// Inner-call targets the resolver paths point at — these MUST
		// be lazy-registered by the new hook.
		{Group: "composition.krateo.io", Version: "v1", Resource: "compositions"}: "CompositionList",
		{Group: "", Version: "v1", Resource: "pods"}:                              "PodList",
		{Group: "apps", Version: "v1", Resource: "deployments"}:                   "DeploymentList",
		{Group: "templates.krateo.io", Version: "v1", Resource: "widgets"}:        "WidgetList",
	}
}

// TestLazyRegisterInnerCallPaths_TriggersEnsureForEveryGVR is the
// 0.30.92 falsifier: every distinct apiserver-derived GVR in the
// per-stage RequestOptions slice MUST be registered on the global
// watcher after the helper returns.
//
// Verification probe (same as the dispatcher-side test): a registered
// GVR's ListObjects returns a non-nil slice; an unregistered GVR
// returns nil.
func TestLazyRegisterInnerCallPaths_TriggersEnsureForEveryGVR(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		newSchemeForResolverLazyRegister(), listKindsForResolverLazyRegister())

	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil {
		t.Fatalf("expected non-nil watcher under CACHE_ENABLED=true")
	}
	defer rw.Stop()

	cache.SetGlobal(rw)
	t.Cleanup(func() { cache.SetGlobal(nil) })

	// Three distinct downstream GVRs across a mixed iterator stage:
	// compositions (named group / namespaced), pods (core group /
	// namespaced), deployments (apps group / namespaced). Plus one
	// repeated compositions entry to assert the dedup branch in the
	// helper (same GVR seen twice in tmp is a no-op after the first).
	//
	// `Path` + `Verb` live on the embedded RequestInfo struct, so the
	// composite literal must initialize RequestInfo explicitly.
	opts := []httpcall.RequestOptions{
		{
			RequestInfo: httpcall.RequestInfo{
				Verb: ptr.To(http.MethodGet),
				Path: "/apis/composition.krateo.io/v1/namespaces/bench-ns-01/compositions",
			},
		},
		{
			RequestInfo: httpcall.RequestInfo{
				Verb: ptr.To(http.MethodGet),
				Path: "/api/v1/namespaces/bench-ns-02/pods",
			},
		},
		{
			RequestInfo: httpcall.RequestInfo{
				Verb: ptr.To(http.MethodGet),
				Path: "/apis/apps/v1/namespaces/bench-ns-03/deployments",
			},
		},
		{
			RequestInfo: httpcall.RequestInfo{
				Verb: ptr.To(http.MethodGet),
				Path: "/apis/composition.krateo.io/v1/namespaces/bench-ns-04/compositions",
			},
		},
	}

	lazyRegisterInnerCallPaths(slog.Default(), opts)

	mustBeRegistered := []schema.GroupVersionResource{
		{Group: "composition.krateo.io", Version: "v1", Resource: "compositions"},
		{Group: "", Version: "v1", Resource: "pods"},
		{Group: "apps", Version: "v1", Resource: "deployments"},
	}
	for _, gvr := range mustBeRegistered {
		got := rw.ListObjects(gvr, "")
		if got == nil {
			t.Fatalf("0.30.92 WIRE-UP BROKEN: GVR %s was not registered on watcher; lazyRegisterInnerCallPaths did not call EnsureResourceType for it",
				gvr.String())
		}
	}

	// An untouched GVR (widgets) must NOT have been registered — the
	// helper only registers what the request options enumerate.
	widgetsGVR := schema.GroupVersionResource{Group: "templates.krateo.io", Version: "v1", Resource: "widgets"}
	if got := rw.ListObjects(widgetsGVR, ""); got != nil {
		t.Fatalf("over-registration regression: widgets GVR was registered, but the request options never targeted it")
	}
}

// TestLazyRegisterInnerCallPaths_SkipsNonApiserverPaths covers the
// branches that must NOT trigger registration: external endpoints,
// JQ-templated fragments that escaped evaluation, and empty paths.
// Any of these triggering EnsureResourceType would attempt to register
// an informer for a non-existent GVR, which would either nil-panic in
// factory.ForResource OR spin up an unbounded retry loop in the
// informer goroutine — neither acceptable in a hot resolver path.
func TestLazyRegisterInnerCallPaths_SkipsNonApiserverPaths(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		newSchemeForResolverLazyRegister(), listKindsForResolverLazyRegister())

	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	defer rw.Stop()
	cache.SetGlobal(rw)
	t.Cleanup(func() { cache.SetGlobal(nil) })

	// Pre-snapshot the four eager-registered RBAC GVRs so we can
	// assert nothing else was added.
	preRegistered := map[schema.GroupVersionResource]bool{
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}:                true,
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}:         true,
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}:         true,
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}: true,
	}

	opts := []httpcall.RequestOptions{
		// External endpoint — GitHub, no apiserver GVR.
		{RequestInfo: httpcall.RequestInfo{Path: "https://api.github.com/repos/foo/bar/contents"}},
		// JQ-templated leak — parser MUST return ok=false.
		{RequestInfo: httpcall.RequestInfo{Path: `${ "/api/v1/namespaces/" + (.) + "/pods" }`}},
		// Empty path — defensive.
		{RequestInfo: httpcall.RequestInfo{Path: ""}},
		// Root path — no GVR.
		{RequestInfo: httpcall.RequestInfo{Path: "/"}},
	}
	lazyRegisterInnerCallPaths(slog.Default(), opts)

	// Probe a few inner-call GVRs the helper SHOULD NOT have touched.
	candidates := []schema.GroupVersionResource{
		{Group: "composition.krateo.io", Version: "v1", Resource: "compositions"},
		{Group: "", Version: "v1", Resource: "pods"},
		{Group: "apps", Version: "v1", Resource: "deployments"},
		{Group: "templates.krateo.io", Version: "v1", Resource: "widgets"},
	}
	for _, gvr := range candidates {
		if preRegistered[gvr] {
			continue
		}
		if got := rw.ListObjects(gvr, ""); got != nil {
			t.Fatalf("non-apiserver path triggered EnsureResourceType for %s — parser leak", gvr.String())
		}
	}
}

// TestLazyRegisterInnerCallPaths_NilWatcherIsSilent covers the
// production cache=off branch: cache.Global() returns nil and the
// helper must early-return without panicking.
func TestLazyRegisterInnerCallPaths_NilWatcherIsSilent(t *testing.T) {
	cache.SetGlobal(nil)
	t.Cleanup(func() { cache.SetGlobal(nil) })

	opts := []httpcall.RequestOptions{
		{RequestInfo: httpcall.RequestInfo{Path: "/apis/composition.krateo.io/v1/namespaces/x/compositions"}},
	}
	// Must not panic.
	lazyRegisterInnerCallPaths(slog.Default(), opts)
}
