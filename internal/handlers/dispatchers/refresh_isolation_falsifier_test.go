// refresh_isolation_falsifier_test.go — Ship 1 (live-refresh-coherence,
// option A) cross-user isolation falsifier (9.4a + the 9.4b mechanism
// invariant). feedback_l1_per_user_keyed_never_cohort + feedback_no_special_cases.
//
// Hermetic: an in-process RBAC ResourceWatcher (fake dynamic client; NO
// apiserver, KUBECONFIG unset). NEVER ./internal/rbac (destructive TestMain).
//
//   9.4a — IDENTITY-BOUND classes: the server re-derives the L1 key UNDER the
//          CONNECTION'S authenticated identity (dispatchers.DeriveSubscriptionKey,
//          the exact seam /refreshes' validateSubscription calls). Two users
//          granted compositions by DIFFERENT bindings get DIFFERENT BindingUIDs
//          -> DIFFERENT keys for the SAME coordinates. A connection as A that
//          sends B's coordinates derives A's key, NOT B's -> when B's cell
//          refreshes (publishes B's key), A (armed for A's key only) gets
//          nothing. Forgery-proof by construction: BindingUID comes from the
//          connection's JWT identity, never from the wire.
//
//   9.4b mechanism — IDENTITY-FREE widgetContent: the shared shell key is the
//          SAME for A and B (nobody owns it). So a widgetContent SIGNAL is
//          content-free and CANNOT carry subject-specific information; the
//          per-user ROW gating happens at /call serve time (gateWidgetEnvelope,
//          covered by widget_content_rbac_sensitive_falsifier_test.go). This
//          test proves the SSE key layer is content-free; the per-row CONTENT
//          assertion through a live widget-serve is the cluster falsifier
//          (design §9.4b: runtime falsifiers run against the GKE context).
//
//   fail-closed — a connection with NO identity derives the empty-identity key,
//          distinct from any granted user's key (the dispatchCacheLookupKey
//          FAIL-CLOSED posture).

package dispatchers

import (
	"context"
	"testing"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/snowplow/internal/cache"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// buildTwoUserRBACWatcher seeds a fake RBAC snapshot granting compositions to
// userA via CRB uid-A and to userB via CRB uid-B (distinct bindings ->
// distinct BindingUIDs). Published as cache.Global() for the test duration.
func buildTwoUserRBACWatcher(t *testing.T) {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")

	crbGVR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}
	crGVR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}
	scheme := runtime.NewScheme()
	_ = rbacv1.AddToScheme(scheme)
	compGVR := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1alpha1", Resource: "compositions"}
	panelGVR := schema.GroupVersionResource{Group: "widgets.templates.krateo.io", Version: "v1beta1", Resource: "panels"}
	listKinds := map[schema.GroupVersionResource]string{
		crbGVR: "ClusterRoleBindingList",
		crGVR:  "ClusterRoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}: "RoleBindingList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}:        "RoleList",
		compGVR:  "CompositionList",
		panelGVR: "PanelList",
	}
	// #64: grant compositions AND panels get/list — both classWidgets and
	// widgetContent coords (compositionsCoords + the WidgetContentSharedShell
	// panels coord) now fetch their CR via subscriptionKeyExtras→objects.Get
	// (RBAC-gated under the connection identity). Without the panels grant the
	// widgetContent fetch fail-closes (C64-1) and the shared-shell test can't arm.
	compRule := []rbacv1.PolicyRule{
		{Verbs: []string{"get", "list"}, APIGroups: []string{"composition.krateo.io"}, Resources: []string{"compositions"}},
		{Verbs: []string{"get", "list"}, APIGroups: []string{"widgets.templates.krateo.io"}, Resources: []string{"panels"}},
	}
	mkCR := func(name string, rules []rbacv1.PolicyRule) *rbacv1.ClusterRole {
		return &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Rules: rules}
	}
	mkCRB := func(name, uid, role string, sub rbacv1.Subject) *rbacv1.ClusterRoleBinding {
		return &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)},
			Subjects:   []rbacv1.Subject{sub},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: role},
		}
	}
	seed := []runtime.Object{
		mkCR("comp-reader", compRule),
		// Two distinct bindings to the SAME role for two distinct users ->
		// the per-binding cell identity (BindingUID) differs per user.
		mkCRB("a-bind", "uid-A", "comp-reader", rbacv1.Subject{Kind: "User", Name: "userA"}),
		mkCRB("b-bind", "uid-B", "comp-reader", rbacv1.Subject{Kind: "User", Name: "userB"}),
		// #64: the compositionsCoords() class is classWidgets, which now (post-#64)
		// fetches the widget CR via subscriptionKeyExtras→objects.Get to fold the
		// inline-extras union. Seed the compositions CR (no inline extras → the
		// union == request-only, so the distinct-BindingUID key invariant is
		// unchanged) so the fetch succeeds and the test exercises the real arming
		// path rather than fail-closed-skipping (C64-1).
		&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "composition.krateo.io/v1alpha1",
			"kind":       "Composition",
			"metadata":   map[string]any{"name": "demo-1", "namespace": "team-a"},
			"spec":       map[string]any{},
		}},
		// #64: the WidgetContentSharedShell panels coord (dashboard-piechart/
		// krateo-system) — no inline extras → union==request-only, shared-shell
		// key invariant unchanged.
		&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "widgets.templates.krateo.io/v1beta1",
			"kind":       "Panel",
			"metadata":   map[string]any{"name": "dashboard-piechart", "namespace": "krateo-system"},
			"spec":       map[string]any{},
		}},
	}

	wctx, wcancel := context.WithCancel(context.Background())
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, seed...)
	rw, err := cache.NewResourceWatcher(wctx, dyn)
	if err != nil {
		wcancel()
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rw.WaitForCacheSync(ctx, 5*time.Second); err != nil {
		rw.Stop()
		wcancel()
		t.Fatalf("WaitForCacheSync: %v", err)
	}
	// #64: make the compositions GVR servable so objects.Get serves the seeded
	// CR from the informer (subscriptionKeyExtras' read), not an apiserver
	// fallthrough.
	_, _ = rw.EnsureResourceType(compGVR)
	_, _ = rw.EnsureResourceType(panelGVR)
	_ = rw.WaitForCacheSync(ctx, 5*time.Second)
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

// ctxAs builds a request-shaped ctx carrying ONLY UserInfo (exactly what
// middleware.RefreshAuth places on the /refreshes connection ctx).
func ctxAs(username string) context.Context {
	return xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: username}),
	)
}

// compositionsCoords is the identity-bound coordinate set for a compositions
// widget (the headline live-refresh case: a composition reconcile fans to its
// cards).
func compositionsCoords() SubscriptionCoordinates {
	return SubscriptionCoordinates{
		Class:     classWidgets,
		Group:     "composition.krateo.io",
		Version:   "v1alpha1",
		Resource:  "compositions",
		Namespace: "team-a",
		Name:      "demo-1",
	}
}

// TestRefreshIsolation_IdentityBound_DistinctKeysPerSubject is falsifier 9.4a.
// The SAME coordinates derive DIFFERENT keys under A's vs B's identity, and the
// forgery attempt (A sending B's coordinates) derives A's key, not B's.
func TestRefreshIsolation_IdentityBound_DistinctKeysPerSubject(t *testing.T) {
	buildTwoUserRBACWatcher(t)

	coords := compositionsCoords()

	keyA, okA := DeriveSubscriptionKey(ctxAs("userA"), coords)
	keyB, okB := DeriveSubscriptionKey(ctxAs("userB"), coords)
	if !okA || !okB {
		t.Fatalf("derivation failed: okA=%v okB=%v (RBAC snapshot not granting compositions?)", okA, okB)
	}
	if keyA == "" || keyB == "" {
		t.Fatalf("empty key(s): keyA=%q keyB=%q — BindingUID not derived (RBAC snapshot wrong)", keyA, keyB)
	}
	// THE ISOLATION INVARIANT: distinct subjects -> distinct per-binding keys
	// for the SAME identity-bound coordinates.
	if keyA == keyB {
		t.Fatalf("9.4a FAIL: userA and userB derived the SAME key %q for the same coordinates — "+
			"the per-binding cell identity did not separate them (cross-user signal leak)", keyA)
	}

	// FORGERY: A's connection sends B's coordinates. Since B's coordinates are
	// identical to A's here (same composition), the point is sharper: the key
	// is a function of the CONNECTION identity, never the wire. A connection
	// authenticated as A ALWAYS derives keyA regardless of what coordinates it
	// claims — it can never derive keyB. Re-derive under A and assert it equals
	// keyA (A's own key), NOT keyB.
	forged, _ := DeriveSubscriptionKey(ctxAs("userA"), coords)
	if forged == keyB {
		t.Fatalf("9.4a FORGERY FAIL: connection-as-A derived B's key %q — a client forged another subject's subscription", keyB)
	}
	if forged != keyA {
		t.Fatalf("9.4a: connection-as-A derived %q, expected its own key %q", forged, keyA)
	}
	t.Logf("9.4a OK: keyA=%s keyB=%s (distinct per-binding keys; connection-as-A can only ever derive keyA)", keyA[:12], keyB[:12])
}

// TestRefreshIsolation_ForgeryRejectedViaBroadcaster wires the derivation into
// the broadcaster: A arms ONLY the key it legitimately derives; when B's cell
// publishes (B's key), A receives NOTHING. This is the end-to-end forgery-proof
// assertion at the signal layer.
func TestRefreshIsolation_ForgeryRejectedViaBroadcaster(t *testing.T) {
	buildTwoUserRBACWatcher(t)
	t.Setenv("REFRESH_SSE_ENABLED", "")
	t.Setenv("REFRESH_COALESCE_WINDOW_MS", "0")
	cache.ResetRefreshBroadcasterForTest()
	t.Cleanup(cache.ResetRefreshBroadcasterForTest)

	coords := compositionsCoords()
	keyA, okA := DeriveSubscriptionKey(ctxAs("userA"), coords)
	keyB, okB := DeriveSubscriptionKey(ctxAs("userB"), coords)
	if !okA || !okB || keyA == keyB {
		t.Fatalf("setup: keys not distinct (keyA=%q keyB=%q okA=%v okB=%v)", keyA, keyB, okA, okB)
	}

	// A's connection arms ONLY the key A's identity produced (what
	// validateSubscription does). It does NOT and cannot arm keyB.
	chA, unsubA := cache.SubscribeRefresh(map[string]struct{}{keyA: {}})
	defer unsubA()

	// B's cell refreshes — the refresher publishes keyB.
	cache.PublishRefresh(keyB)

	// A must receive NOTHING (it is not armed for keyB).
	select {
	case leaked := <-chA:
		t.Fatalf("9.4a LEAK: A received a signal %q for B's refreshing cell — cross-user isolation broken", leaked)
	case <-time.After(200 * time.Millisecond):
		// correct — no leak
	}

	// Sanity: A DOES receive its OWN cell's refresh.
	cache.PublishRefresh(keyA)
	select {
	case got := <-chA:
		if got != keyA {
			t.Fatalf("A received %q, want its own keyA", got)
		}
	case <-time.After(time.Second):
		t.Fatalf("A did not receive its own cell's refresh — derivation/arming broken")
	}
}

// TestRefreshIsolation_WidgetContentSharedShell proves the 9.4b MECHANISM
// invariant at the key layer: the identity-free widgetContent key is the SAME
// for A and B (nobody owns it), so a widgetContent SIGNAL is content-free and
// cannot carry subject-specific data. The per-user ROW gating is applied at
// /call serve time (gateWidgetEnvelope — see
// widget_content_rbac_sensitive_falsifier_test.go); the per-row CONTENT
// assertion through a live serve is the cluster falsifier.
func TestRefreshIsolation_WidgetContentSharedShell(t *testing.T) {
	buildTwoUserRBACWatcher(t)
	t.Setenv("WIDGET_CONTENT_L1_ENABLED", "true")

	wc := SubscriptionCoordinates{
		Class:     cache.CacheEntryClassWidgetContent,
		Group:     "widgets.templates.krateo.io",
		Version:   "v1beta1",
		Resource:  "panels",
		Namespace: "krateo-system",
		Name:      "dashboard-piechart",
		PerPage:   5,
		Page:      1,
	}
	keyA, okA := DeriveSubscriptionKey(ctxAs("userA"), wc)
	keyB, okB := DeriveSubscriptionKey(ctxAs("userB"), wc)
	if !okA || !okB {
		t.Fatalf("widgetContent derivation failed: okA=%v okB=%v", okA, okB)
	}
	// Identity-free: SAME key for both subjects (the shared shell).
	if keyA != keyB {
		t.Fatalf("widgetContent key differs across subjects (keyA=%q keyB=%q) — it must be identity-FREE "+
			"(shared shell); the per-user narrowing is the serve-time gate, NOT the key", keyA, keyB)
	}
	t.Logf("9.4b mechanism OK: widgetContent key shared across A and B (%s) — signal is content-free; "+
		"per-row gating is at /call serve (gateWidgetEnvelope)", keyA[:12])
}

// TestRefreshIsolation_NoIdentityFailClosed asserts a connection with NO
// identity (defence in depth — RefreshAuth would 401 first) derives the
// empty-identity key for an identity-bound class, distinct from any granted
// user's key. This is the dispatchCacheLookupKey FAIL-CLOSED posture.
func TestRefreshIsolation_NoIdentityFailClosed(t *testing.T) {
	buildTwoUserRBACWatcher(t)

	coords := compositionsCoords()
	keyA, _ := DeriveSubscriptionKey(ctxAs("userA"), coords)

	// Bare ctx (no UserInfo) -> dispatchCacheLookupKey returns (\"\", nil, nil)
	// because xcontext.UserInfo errors -> DeriveSubscriptionKey returns ok=false.
	key, ok := DeriveSubscriptionKey(context.Background(), coords)
	if ok {
		// If a key WAS derived it must be the empty-identity key, never a
		// granted user's key.
		if key == keyA {
			t.Fatalf("FAIL-CLOSED VIOLATION: a no-identity connection derived granted user A's key %q", keyA)
		}
	}
	// ok=false (skip the entry) is the expected fail-closed outcome; either
	// way, the no-identity connection can never arm A's key.
}
