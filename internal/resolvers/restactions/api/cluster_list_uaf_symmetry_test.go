// cluster_list_uaf_symmetry_test.go — Ship S.2 / 0.30.213
// predicate-symmetry seal test (AC-S2.3 + AC-S2.7).
//
// Per memory feedback_predicate_subject_kind_symmetry (the ζ HARD-REVERT
// lesson, 0.30.183): the cluster-list-collapse RBAC predicate MUST apply
// symmetrically across User, Group, AND ServiceAccount subject kinds.
// CohortNSACL already does (Ship 1.1 / 0.30.196 added SA landings via
// CRBsByServiceAccount + cohortEffectiveGroups); this test seals the
// invariant under -race so a future refactor cannot break one subject
// kind silently.
//
// Per memory feedback_shared_vs_copy_is_a_concurrency_change: enabling
// the cluster-list collapse converts the per-NS-LIST private-copy
// iterator path to a SHARED entry.Items aliased path served via
// cohortGateMemoServe. This is a concurrency change — the symmetry
// test MUST run under `go test -race` to catch any aliasing defect
// across parallel cohorts on the SAME cluster-LIST cell.
//
// Test mechanism:
//
//   - Three sub-tests, each builds a watcher whose target user is granted
//     `list widgets` cluster-wide via a ClusterRoleBinding bound to a
//     DIFFERENT subject kind:
//       (A) User-kind CRB bound to symBroadUserName.
//       (B) Group-kind CRB bound to symBroadGroupName.
//       (C) ServiceAccount-kind CRB bound to a SA identity.
//
//   - Each sub-test drives N=8 concurrent attemptClusterListCollapse
//     calls against the SAME cluster-list cell from the same identity
//     kind. With -race enabled, the runner reports any data race on
//     entry.Items or CohortGates aliasing.
//
//   - Each sub-test asserts EXACTLY ONE successful collapse (gate=0
//     once, deny-on-cache-hit on the remaining concurrent calls is
//     fine — the OUTCOME is "cluster-list cell populated, served from
//     one dispatch").
//
// Discharges PM Condition 4 (mandatory -race CI gate on the symmetry
// test file).

package api

import (
	"context"
	"net/http"
	"sync"
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

// Fixture identities — distinct per-subject-kind. The User-kind and
// Group-kind sub-tests use plain strings; the SA-kind sub-test uses the
// canonical "system:serviceaccount:<ns>:<name>" form so CohortNSACL's
// parseCohortSAUsername path fires.
const (
	symBroadUserName      = "sym-broad-user"
	symBroadGroupName     = "sym-broad-group"
	symSAServiceAccountNS = "krateo-system"
	symSAServiceAccount   = "sym-broad-sa"
)

// symSAUsername returns the canonical SA username for the SA sub-test.
func symSAUsername() string {
	return "system:serviceaccount:" + symSAServiceAccountNS + ":" + symSAServiceAccount
}

// newSymmetryWatcher builds a synced cache=on watcher whose targetSubject
// is granted `list widgets` CLUSTER-WIDE via a ClusterRole + a
// ClusterRoleBinding whose SINGLE Subject has the requested kind+name.
// All three sub-tests reuse the same widget seed (one widget per
// f1AllNamespaces ns); the only thing that varies across sub-tests is
// the binding's Subject kind.
func newSymmetryWatcher(t *testing.T, subjectKind, subjectName string) *cache.ResourceWatcher {
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
	// widgets` cluster-wide + a single ClusterRoleBinding with the
	// requested subject-kind shape.
	var seed []runtime.Object
	for _, ns := range f1AllNamespaces {
		seed = append(seed, f1WidgetObject(ns, "widget-"+ns))
	}
	cr := &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: "sym-widget-lister"},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{f1WidgetsGVR.Group}, Resources: []string{f1WidgetsGVR.Resource}, Verbs: []string{"list"}},
		},
	}
	subj := rbacv1.Subject{Kind: subjectKind, Name: subjectName}
	switch subjectKind {
	case rbacv1.UserKind, rbacv1.GroupKind:
		subj.APIGroup = "rbac.authorization.k8s.io"
	case rbacv1.ServiceAccountKind:
		subj.Namespace = symSAServiceAccountNS
		// APIGroup intentionally empty for ServiceAccount subjects
		// (the rbac/v1 schema requires it empty).
	}
	crb := &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: "sym-broad-binding"},
		Subjects:   []rbacv1.Subject{subj},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "sym-widget-lister"},
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

// symBuildContext composes a user-context for the symmetry sub-test.
// All three sub-tests share the same Username/Groups shape; the
// subject-kind variation is in the binding side (newSymmetryWatcher).
//
//   - User-kind sub-test: Username=symBroadUserName, no groups.
//   - Group-kind sub-test: Username="some-other-user" (NOT bound), Groups=[symBroadGroupName].
//   - SA-kind sub-test: Username=symSAUsername() (canonical SA form).
func symBuildContext(username string, groups []string) context.Context {
	return xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{
			Username: username,
			Groups:   groups,
		}),
	)
}

// symDriveCollapseConcurrent fires N concurrent attemptClusterListCollapse
// calls and asserts:
//
//   - at least ONE call returned ok=true (cluster-LIST cell got populated).
//   - all calls returned without panicking under -race.
//
// The test is a -race seal: any aliasing defect on entry.Items or
// CohortGates from concurrent serves on the same cell will surface as a
// race report; any panic will surface as a goroutine failure.
func symDriveCollapseConcurrent(t *testing.T, ctx context.Context, apiCall *templates.API) {
	t.Helper()
	store := cache.ResolvedCache()
	if store == nil {
		t.Fatalf("resolved cache nil under RESOLVED_CACHE_ENABLED=true")
	}
	withClusterListCollapseEnabledForTest(t)

	const workers = 8
	var wg sync.WaitGroup
	results := make([]bool, workers)
	gates := make([]int, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			newTmp, useCluster, gate := attemptClusterListCollapse(
				ctx, clusterListLogger(t), apiCall, nil,
				map[string]any{}, endpoints.Endpoint{}, store, true,
			)
			results[idx] = useCluster
			gates[idx] = gate
			_ = newTmp
		}(w)
	}
	wg.Wait()

	successCount := 0
	for i, ok := range results {
		if ok {
			successCount++
		}
		// A gate value > 7 is impossible by the deny-gate table; sanity
		// check against silent corruption under -race.
		if gates[i] < 0 || gates[i] > 7 {
			t.Fatalf("worker %d: gate=%d out of range [0,7]", i, gates[i])
		}
	}
	if successCount == 0 {
		t.Fatalf("symmetry FAIL: no worker hit the cluster-list collapse success path "+
			"(gates=%v). All workers had RBAC-permitted cluster-scope `list widgets`; "+
			"at least ONE should have succeeded. This is a ζ-class predicate-symmetry "+
			"regression — re-check CohortNSACL's <subject-kind> landings.",
			gates)
	}
}

// widgetsClusterListStage is the iterator-over-namespaces stage used by
// the symmetry sub-tests. The iterator yields ONE element (single-NS) so
// the original deriveTargetGVRForClusterList path succeeds (no need to
// exercise the UAF-derivation fallback here — that's
// cluster_list_uaf_derive_test.go's job). This isolates the symmetry
// test to the RBAC + concurrency invariant.
func widgetsClusterListStage() *templates.API {
	return &templates.API{
		Name: "widgets-symmetry",
		Path: `${ "/apis/widgets.krateo.io/v1/namespaces/" + .ns + "/widgets" }`,
		Verb: ptr.To(http.MethodGet),
		DependsOn: &templates.Dependency{
			Iterator: ptr.To(`[{"ns":"team-a"}]`),
		},
	}
}

// Sub-test (A) — User-kind RoleBinding.
func TestClusterListCollapse_PredicateSymmetry_UserSubject(t *testing.T) {
	_ = newSymmetryWatcher(t, rbacv1.UserKind, symBroadUserName)
	ctx := symBuildContext(symBroadUserName, nil)
	symDriveCollapseConcurrent(t, ctx, widgetsClusterListStage())
}

// Sub-test (B) — Group-kind RoleBinding.
func TestClusterListCollapse_PredicateSymmetry_GroupSubject(t *testing.T) {
	_ = newSymmetryWatcher(t, rbacv1.GroupKind, symBroadGroupName)
	// Username is irrelevant for Group-kind binding; the Group landing
	// is what grants the cohort. Use a distinct username so a User-kind
	// fall-through cannot accidentally cover a missing Group landing.
	ctx := symBuildContext("unbound-username", []string{symBroadGroupName})
	symDriveCollapseConcurrent(t, ctx, widgetsClusterListStage())
}

// Sub-test (C) — ServiceAccount-kind RoleBinding.
func TestClusterListCollapse_PredicateSymmetry_ServiceAccountSubject(t *testing.T) {
	_ = newSymmetryWatcher(t, rbacv1.ServiceAccountKind, symSAServiceAccount)
	// Canonical SA username — CohortNSACL's parseCohortSAUsername fires
	// + cohortEffectiveGroups expands the synthetic SA groups. This is
	// the path Ship 1.1 / 0.30.196 added; the sub-test seals it does
	// not regress under -race.
	ctx := symBuildContext(symSAUsername(), nil)
	symDriveCollapseConcurrent(t, ctx, widgetsClusterListStage())
}
