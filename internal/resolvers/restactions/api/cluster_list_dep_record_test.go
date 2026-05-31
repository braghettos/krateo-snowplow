// cluster_list_dep_record_test.go — Ship 0.30.212 F-4 freshness falsifier
// for the cluster-list-collapse apistage Put path.
//
// THE DEFECT (#72, traced 2026-05-29): attemptClusterListCollapse's
// success path Puts the validated cluster-scope envelope into the
// apistage L1 (cluster_list.go:320) with NO matching
// cache.Deps().RecordList call. Consequence: an informer ADD/UPDATE/
// DELETE on the underlying GVR never dirty-marks the cluster-scope
// cell — the collapsed entry freezes at boot-time content until the
// 3600s TTL. PM caught this third site that the architect's initial
// 2-site RCA missed.
//
// THE FIX (Ship 0.30.212, Site 3): the Put at cluster_list.go:320 is
// followed by cache.Deps().RecordList(contentKey, gvr, "") — always
// LIST with empty namespace by construction (contentKey is built two
// lines above with ns="" + name=""). Mirrors the dispatcher LIST
// record at resolve.go:550.
//
// TESTABILITY NOTES:
//
//  1. In production attemptClusterListCollapse is held INERT behind
//     clusterListCollapseEnabled (false) — Ship S.1-re sequencing gate,
//     held off until S.2 lands refresher-decoupling. The test flips the
//     package var locally so the full collapse path runs; production
//     behaviour is byte-identical.
//
//  2. Gate 5 (RBAC permit) requires cluster-scope `list` on the target
//     GVR. The shared newF1Watcher harness grants only per-namespace
//     RoleBindings, so this test builds a dedicated watcher whose user
//     is bound via a ClusterRoleBinding (cluster-scope grant).

package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// withClusterListCollapseEnabledForTest flips clusterListCollapseEnabled
// to true and restores the prior value on Cleanup. Used ONLY by this
// test file. Production never assigns the var; default stays false.
func withClusterListCollapseEnabledForTest(t *testing.T) {
	t.Helper()
	prev := clusterListCollapseEnabled
	clusterListCollapseEnabled = true
	t.Cleanup(func() { clusterListCollapseEnabled = prev })
}

const clusterListBroadUser = "cluster-list-broad-user"

// newClusterListWatcher builds a synced cache=on watcher whose
// clusterListBroadUser is granted `list widgets` CLUSTER-WIDE via a
// ClusterRole + ClusterRoleBinding (NOT per-namespace RoleBindings).
// Required for attemptClusterListCollapse's gate 5 — rbac.EvaluateRBAC
// with namespace="" returns permit only on a cluster-scope grant.
func newClusterListWatcher(t *testing.T) *cache.ResourceWatcher {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_APISTAGE_ENABLED", "true")
	t.Setenv("RESOLVER_USE_INFORMER", "true")
	cache.ResetResolvedCacheForTest()
	cache.ResetDepsForTest()
	t.Cleanup(func() {
		cache.ResetResolvedCacheForTest()
		cache.ResetDepsForTest()
	})

	// Seed: one widget per namespace + ClusterRole granting `list
	// widgets` cluster-wide + ClusterRoleBinding for the test user.
	var seed []runtime.Object
	for _, ns := range f1AllNamespaces {
		seed = append(seed, f1WidgetObject(ns, "widget-"+ns))
	}
	cr := &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-list-widget-lister"},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{f1WidgetsGVR.Group}, Resources: []string{f1WidgetsGVR.Resource}, Verbs: []string{"list"}},
		},
	}
	crb := &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-list-broad-binding"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: "rbac.authorization.k8s.io", Name: clusterListBroadUser}},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-list-widget-lister"},
	}
	seed = append(seed, cr, crb)

	scheme := runtime.NewScheme()
	_ = rbacv1.AddToScheme(scheme)
	listKinds := map[schema.GroupVersionResource]string{
		f1WidgetsGVR: "WidgetList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}:                "RoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}:         "RoleBindingList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}:         "ClusterRoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}: "ClusterRoleBindingList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, seed...)

	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil {
		t.Fatalf("expected non-nil watcher under CACHE_ENABLED=true")
	}
	t.Cleanup(func() { rw.Stop(); time.Sleep(50 * time.Millisecond) })

	added, syncCh := rw.EnsureResourceType(f1WidgetsGVR)
	if !added {
		t.Fatalf("EnsureResourceType(widgets): want added=true")
	}
	select {
	case <-syncCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("widgets informer did not sync within 5s")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rw.WaitForCacheSync(ctx, 5*time.Second); err != nil {
		t.Fatalf("WaitForCacheSync (RBAC informers): %v", err)
	}
	cache.SetGlobal(rw)
	t.Cleanup(func() { cache.SetGlobal(nil) })
	return rw
}

// TestClusterListCollapsePut_RecordsDepEdges drives the cluster-list
// collapse success path to completion (gate-permit → un-gated dispatch
// via dispatchViaInformer → shape check → apistageStore.Put) and
// asserts the new Ship 0.30.212 Site 3 dep-record block at
// cluster_list.go:321's neighbourhood fired with the LIST bucket
// (gvr, ns="").
//
// Pre-fix this test FAILS because the collapse Put has no dep-record
// companion → CollectMatchesForTest finds NO entry under the
// cluster-scope LIST bucket → the refresh hook is never called on
// subsequent OnAdd events for the GVR. Post-fix both invariants PASS.
func TestClusterListCollapsePut_RecordsDepEdges(t *testing.T) {
	rw := newClusterListWatcher(t)
	_ = rw
	withClusterListCollapseEnabledForTest(t)

	// Wire the refresh-hook capture before driving the collapse so the
	// post-Put dep-record's OnAdd propagation is observable.
	hook, snapshot := depRecordCapturedHook()
	cache.Deps().SetRefreshHook(hook)

	store := cache.ResolvedCache()
	if store == nil {
		t.Fatalf("resolved cache nil under RESOLVED_CACHE_ENABLED=true")
	}

	// Stage shape: iterator yields ONE element with ns="team-a" (a
	// seeded namespace from newClusterListWatcher) so the Path
	// template resolves to a namespace-scoped widgets path.
	// deriveTargetGVRForClusterList then derives (widgets.krateo.io/v1/
	// widgets) and (ns="team-a"), returns ok=true (namespace-scoped).
	// The RBAC gate then evaluates cluster-scope `list widgets` for
	// clusterListBroadUser — admitted because the ClusterRoleBinding
	// grants cluster-wide list.
	apiCall := &templates.API{
		Name: "widgets-cluster-collapse",
		Path: `${ "/apis/widgets.krateo.io/v1/namespaces/" + .ns + "/widgets" }`,
		Verb: ptr.To(http.MethodGet),
		DependsOn: &templates.Dependency{
			Iterator: ptr.To(`[{"ns":"team-a"}]`),
		},
	}

	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: clusterListBroadUser}),
	)

	newTmp, useCluster, gate := attemptClusterListCollapse(
		ctx, clusterListLogger(t), apiCall, nil,
		map[string]any{}, endpoints.Endpoint{}, store, true,
	)
	if !useCluster {
		t.Fatalf("attemptClusterListCollapse: expected success path (useClusterList=true); "+
			"got useCluster=%v gate=%d tmp=%v. Setup broken — newClusterListWatcher's "+
			"ClusterRoleBinding grants cluster-scope `list widgets`; "+
			"clusterListCollapseEnabled is flipped.",
			useCluster, gate, newTmp)
	}
	if len(newTmp) != 1 {
		t.Fatalf("collapse: expected ONE cluster-scope call; got %d", len(newTmp))
	}

	// The contentKey under which Site 3's Put landed: identity-free,
	// cluster-scope LIST (ns="" + name="") for the widgets GVR.
	expectedKey := cache.ComputeKey(contentKeyInputs(f1WidgetsGVR, "", ""))

	// PRIMARY — Site 3's RecordList fired. CollectMatchesForTest with
	// any (gvr, ns="", new-name) must find expectedKey in the LIST
	// bucket.
	matched := cache.Deps().CollectMatchesForTest(f1WidgetsGVR, "team-a", "new-widget-post-collapse")
	if _, present := matched[expectedKey]; !present {
		t.Fatalf("Ship 0.30.212 Site 3 FAIL: cluster-list collapse Put did NOT record a "+
			"LIST dep edge for the cluster-scope content cell.\n"+
			"  contentKey  = %q\n"+
			"  GVR         = %s\n"+
			"  matched set = %v\n"+
			"Without this edge an informer event on %s/* never dirty-marks the "+
			"collapsed cluster-scope cell — the cache freezes at boot-time content "+
			"until TTL=3600s (F-4 defect, #72, PM-caught third site). The fix is the "+
			"cache.Deps().RecordList(contentKey, gvr, \"\") call after apistageStore.Put.",
			expectedKey, f1WidgetsGVR.String(), matched, f1WidgetsGVR.String())
	}

	// MECHANISM — an OnAdd for ANY widget object in ANY namespace must
	// propagate to the refresh hook with expectedKey. The cluster-scope
	// LIST bucket has ns="" + name=listWildcard, so OnAdd's exact-name
	// + list-scope matchers both fire (collectMatches union).
	beforeAdd := snapshot()
	matchCount := cache.Deps().OnAdd(f1WidgetsGVR, "team-c", "new-widget-team-c")
	if matchCount < 1 {
		t.Fatalf("Ship 0.30.212 Site 3 MECHANISM FAIL: Deps().OnAdd returned matchCount=%d, "+
			"want >=1 — the cluster-scope LIST edge exists but did not match a new ADD "+
			"event on the same GVR.", matchCount)
	}
	afterAdd := snapshot()
	if len(afterAdd) <= len(beforeAdd) {
		t.Fatalf("Ship 0.30.212 Site 3 MECHANISM FAIL: refresh hook was NOT called after "+
			"OnAdd matched %d edge(s); enqueued-keys before=%v after=%v",
			matchCount, beforeAdd, afterAdd)
	}
	found := false
	prev := map[string]int{}
	for _, k := range beforeAdd {
		prev[k]++
	}
	for _, k := range afterAdd {
		if k == expectedKey {
			if prev[k] > 0 {
				prev[k]--
				continue
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Ship 0.30.212 Site 3 MECHANISM FAIL: refresh hook fired but the captured "+
			"L1 keys do NOT include the cluster-scope contentKey %q. captured=%v",
			expectedKey, afterAdd)
	}

	// NEGATIVE CONTROL — an unrelated GVR must NOT match expectedKey.
	unrelatedGVR := schema.GroupVersionResource{
		Group: "totally-unrelated.krateo.io", Version: "v1", Resource: "frobs",
	}
	if neg := cache.Deps().CollectMatchesForTest(unrelatedGVR, "", "anything"); neg != nil {
		if _, ok := neg[expectedKey]; ok {
			t.Fatalf("Site 3 NEGATIVE CONTROL FAIL: the cluster-scope contentKey %q was "+
				"recorded under an unrelated GVR %s — dep edge is over-broad",
				expectedKey, unrelatedGVR.String())
		}
	}
}
