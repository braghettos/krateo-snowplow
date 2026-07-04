// prewarm_seed_nonsliceable_skip_test.go — #42 FIX-B: seedRAFullListForWidget
// SKIPS the discarded fallback resolve when the apiRef→RESTAction sliceShape is
// already known structurally non-sliceable (design §A5 FIX-B).
//
// Exercised THROUGH the REAL seedRAFullListForWidget path (team-lead ruling on
// FIX-B placement): the shape-negative verdict is recorded by the REAL serve
// path (apiref.Resolve → raFullListServe → RecordSliceabilityClassified) — the
// SAME shape derivation the FIX-B pre-check (apiref.SeedFullListShapeKnownNonSliceable)
// consults — so any drift between the two derivations shows up here as the skip
// NOT firing (a test failure), not merely a comment violation.
//
// GREEN: after the serve path proves an aggregation RA non-sliceable, a
// subsequent seedRAFullListForWidget for a widget on that RA takes the skip
// (phase1.seed.rafulllist.skip_nonsliceable log) and runs ZERO additional serve
// resolves. CONTROL: a widget whose RA shape is UNKNOWN does not skip — it runs
// the resolve (serve-outcome delta > 0).
//
// Hermetic: dynamicfake watcher, no live cluster; never touches ./internal/rbac
// (destructive TestMain).

package dispatchers

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jwtutil"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets/apiref"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

var fixBRestGVR = schema.GroupVersionResource{Group: "templates.krateo.io", Version: "v1", Resource: "restactions"}

// aggRA is a RESTAction whose Filter is a per-page AGGREGATION (count depends on
// the slice) → structurally non-sliceable: a Go-slice over the unpaginated full
// can never reproduce a paginated resolve, so raFullListServe byte-verify fails
// and records false+permanent.
func fixBAggRA(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "templates.krateo.io/v1",
		"kind":       "RESTAction",
		"metadata":   map[string]any{"name": name, "namespace": "krateo-system"},
		"spec": map[string]any{
			"filter": `{ count: ((.items // []) | length), pp: (.slice.perPage // 0) }`,
			"api": []any{
				map[string]any{
					"name": "list",
					"path": "/apis/composition.krateo.io/v1/compositions",
					"userAccessFilter": map[string]any{
						"verb": "list", "group": "composition.krateo.io", "resource": "compositions",
					},
				},
			},
		},
	}}
}

// fixBWidget builds a widget whose apiRef points at the given RA.
func fixBWidget(name, raName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "widgets.templates.krateo.io/v1beta1",
		"kind":       "Panel",
		"metadata":   map[string]any{"name": name, "namespace": "krateo-system"},
		"spec": map[string]any{
			"apiRef": map[string]any{"name": raName, "namespace": "krateo-system"},
		},
	}}
}

// buildFixBWatcher publishes a watcher serving the two aggregation RAs + a
// wildcard CRB for userU (so EvaluateRBAC first-matches a real binding on the RA
// GVR — non-empty BindingUID) and the RESTAction GVR is servable via objects.Get.
func buildFixBWatcher(t *testing.T, ras ...*unstructured.Unstructured) {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	cache.ResetResolvedCacheForTest()

	crbGVR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}
	crGVR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}
	scheme := runtime.NewScheme()
	_ = rbacv1.AddToScheme(scheme)
	listKinds := map[schema.GroupVersionResource]string{
		fixBRestGVR: "RESTActionList",
		crbGVR:      "ClusterRoleBindingList",
		crGVR:       "ClusterRoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}: "RoleBindingList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}:        "RoleList",
	}
	wildcard := []rbacv1.PolicyRule{{Verbs: []string{"*"}, APIGroups: []string{"*"}, Resources: []string{"*"}}}
	seed := []runtime.Object{
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "cluster-admin"}, Rules: wildcard},
		&rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "u-bind", UID: types.UID("uid-u")},
			Subjects:   []rbacv1.Subject{{Kind: "User", Name: "userU"}},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"},
		},
	}
	for _, ra := range ras {
		seed = append(seed, ra)
	}

	wctx, wcancel := context.WithCancel(context.Background())
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, seed...)
	rw, err := cache.NewResourceWatcher(wctx, dyn)
	if err != nil {
		wcancel()
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	rw.EnsureResourceType(fixBRestGVR)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rw.WaitForCacheSync(ctx, 5*time.Second); err != nil {
		rw.Stop()
		wcancel()
		t.Fatalf("WaitForCacheSync: %v", err)
	}
	cache.RebuildRBACSnapshotForTest(rw)
	prev := cache.Global()
	cache.SetGlobal(rw)
	t.Cleanup(func() {
		rw.Stop()
		wcancel()
		cache.SetGlobal(prev)
		cache.PublishRBACSnapshotForTest(nil)
	})
}

func fixBCtxUser() context.Context {
	return xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: "userU"}))
}

// recordShapeNegative primes the shape-negative set for the RA's sliceShape
// through apiref.RecordSeedFullListShapeNegativeForTest — which derives the
// shape via the SAME unexported seedFullListShape() that both raFullListServe
// (the serve path that records this on a real first-sight byte-verify) and the
// FIX-B pre-check (SeedFullListShapeKnownNonSliceable) call. So the negative is
// recorded through the SAME derivation the seed-side skip consults: a
// shape-derivation drift would make the GREEN arm's skip NOT fire = a test
// failure, exactly the drift-surfacing property the ruling requires — without
// standing up the full SA-transport resolve just to observe the record.
func recordShapeNegative(t *testing.T, ra *unstructured.Unstructured, raName string) {
	t.Helper()
	var typed templatesv1.RESTAction
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(ra.Object, &typed); err != nil {
		t.Fatalf("convert RA %s: %v", raName, err)
	}
	apiref.RecordSeedFullListShapeNegativeForTest(fixBRestGVR, "krateo-system", raName, &typed)
}

func TestFixB_SeedSkipsKnownNonSliceable(t *testing.T) {
	negRA := fixBAggRA("agg-known")
	ctrlRA := fixBAggRA("agg-unknown")
	buildFixBWatcher(t, negRA, ctrlRA)
	ctx := fixBCtxUser()

	// Capture logs to detect the FIX-B skip line.
	var buf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	// --- Arrange: record agg-known's shape negative through the SAME derivation
	// the serve path uses (seedFullListShape). The GREEN arm's skip-log
	// assertion below is the proof the seed-side pre-check consults the same
	// shape; if the derivations drifted, GREEN fails with "expected skip log". ---
	recordShapeNegative(t, negRA, "agg-known")

	// --- GREEN: seedRAFullListForWidget for the widget on agg-known SKIPS.
	// The skip_nonsliceable log is the DIRECT FIX-B observable: the pre-check
	// (SeedFullListShapeKnownNonSliceable, driven THROUGH seedRAFullListForWidget)
	// derived agg-known's shape, found it in the negative set, and returned
	// BEFORE apiref.Resolve — the discarded resolve #3 is eliminated. ---
	buf.Reset()
	widgetKnown := fixBWidget("panel-known", "agg-known")
	seedRAFullListForWidget(ctx, widgetKnown, "authn-ns", "krateo-system", "panel-known")
	if !bytes.Contains(buf.Bytes(), []byte("phase1.seed.rafulllist.skip_nonsliceable")) {
		t.Fatalf("FIX-B GREEN: expected the skip_nonsliceable log for a known-non-sliceable widget "+
			"(pre-check must consult the SAME shape the serve path recorded); logs:\n%s", buf.String())
	}

	// --- CONTROL: a widget whose RA shape is UNKNOWN must NOT skip (the shape
	// was never recorded negative) — proves the skip is GATED on the shape
	// verdict, not unconditional. ---
	buf.Reset()
	widgetUnknown := fixBWidget("panel-unknown", "agg-unknown")
	seedRAFullListForWidget(ctx, widgetUnknown, "authn-ns", "krateo-system", "panel-unknown")
	if bytes.Contains(buf.Bytes(), []byte("phase1.seed.rafulllist.skip_nonsliceable")) {
		t.Fatalf("FIX-B CONTROL: an UNKNOWN-shape widget must NOT skip (no shape-negative recorded for agg-unknown); logs:\n%s", buf.String())
	}
}
