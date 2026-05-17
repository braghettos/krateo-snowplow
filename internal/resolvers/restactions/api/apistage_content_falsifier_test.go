// apistage_content_falsifier_test.go — Ship F1 (0.30.119) pre-flight
// falsifier for the content-keyed api-stage L1 + serve-time RBAC gate.
//
// THE MODEL (F1): the api-stage L1 is reshaped from Ship E's per-stage,
// per-user entry to a per-K8s-CALL CONTENT entry — the raw apiserver
// envelope of one dispatch, keyed (gvr, namespace, name-or-empty),
// IDENTITY-FREE (ComputeKey drops Username/Groups for the apistage
// class). The dispatch is un-gated; the per-user RBAC gate runs at a
// single site, on the content-Get-HIT path AND the dispatch-MISS path,
// before the content reaches dict[id].
//
// THE LEAK F1 CLOSES: pre-F1 the RBAC gate ran INSIDE dispatchViaInformer
// — it fired on a miss (fresh dispatch) but was SKIPPED on a Get-hit. A
// shared, identity-free content entry Get-hit by a second user would
// serve that user the FIRST resolver's RBAC-narrowed view. F1 moves the
// gate to a point both paths traverse.
//
// FALSIFIER (drives the REAL api.Resolve stage loop — no resolveOnceFn
// stub):
//
//   TestFalsifierF1_CrossUserNoLeak — user A (broad RBAC) resolves a
//     stage that LISTs widgets cluster-wide → content entries stored
//     un-gated → user B (narrow RBAC — fewer namespaces) resolves the
//     same stage → B's content Get-HITs A's entries → assert B's
//     dict[id] is B-NARROWED, never A's broader view. Pre-fix (gate
//     skipped on the hit path) B sees A's view — the leak.
//
//   TestFalsifierF1_GatedFromStoredByteIdentical — AC-F1.2: the
//     gated-from-stored dict[id] is byte-identical to a fresh
//     inline-gated resolve for the same identity, INCLUDING a stage
//     `filter` that strips metadata.namespace (Reading 1: the gate runs
//     on the raw pre-filter envelope, so a stripping filter cannot
//     defeat it).
//
//   TestFalsifierF1_ContentEntryIdentityFreeShared — AC-F1.3: two users
//     resolving the same stage produce exactly ONE shared content entry
//     per K8s call (identity-free key), not one per user.

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
)

// --- F1 falsifier fixtures ------------------------------------------------

// f1WidgetsGVR is the content-unit GVR the falsifier's stage LISTs. A
// customer-style GVR — not RBAC, not composition (so no carve-out).
var f1WidgetsGVR = schema.GroupVersionResource{
	Group:    "widgets.krateo.io",
	Version:  "v1",
	Resource: "widgets",
}

// f1BroadUser sees widgets in EVERY seeded namespace; f1NarrowUser sees
// them in only f1NarrowNamespaces.
const (
	f1BroadUser  = "broad-user"
	f1NarrowUser = "narrow-user"
)

var (
	f1AllNamespaces    = []string{"team-a", "team-b", "team-c", "team-d"}
	f1NarrowNamespaces = []string{"team-a", "team-b"}
)

// f1WidgetObject builds an unstructured widget in ns. metadata.namespace
// is set — filterListByRBAC reads it for the per-namespace verdict.
func f1WidgetObject(ns, name string) runtime.Object {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "widgets.krateo.io/v1",
		"kind":       "Widget",
		"metadata": map[string]any{
			"namespace": ns,
			"name":      name,
		},
	}}
}

// f1WidgetListerClusterRole grants `list` on widgets.krateo.io widgets.
func f1WidgetListerClusterRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: "f1-widget-lister"},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{f1WidgetsGVR.Group}, Resources: []string{f1WidgetsGVR.Resource}, Verbs: []string{"list"}},
		},
	}
}

// f1RoleBinding binds `user` to the widget-lister ClusterRole in ns —
// granting that user `list widgets` in exactly that namespace.
func f1RoleBinding(ns, user string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "f1-binding-" + user},
		Subjects: []rbacv1.Subject{
			{Kind: rbacv1.UserKind, APIGroup: "rbac.authorization.k8s.io", Name: user},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "f1-widget-lister",
		},
	}
}

// newF1Watcher builds a synced cache=on watcher seeded with one widget
// per namespace in f1AllNamespaces, a widget-lister ClusterRole, and
// RoleBindings granting f1BroadUser EVERY namespace + f1NarrowUser only
// f1NarrowNamespaces. RESOLVED_CACHE_APISTAGE_ENABLED + RESOLVER_USE_INFORMER
// are set so the content-keyed api-stage path is live.
func newF1Watcher(t *testing.T) *cache.ResourceWatcher {
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

	var seed []runtime.Object
	for _, ns := range f1AllNamespaces {
		seed = append(seed, f1WidgetObject(ns, "widget-"+ns))
	}
	seed = append(seed, f1WidgetListerClusterRole())
	for _, ns := range f1AllNamespaces {
		seed = append(seed, f1RoleBinding(ns, f1BroadUser))
	}
	for _, ns := range f1NarrowNamespaces {
		seed = append(seed, f1RoleBinding(ns, f1NarrowUser))
	}

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

// f1ListStage is a one-stage RESTAction api[] entry: a cluster-wide
// widgets LIST. `filter` is `.<id>.items` (jsonHandler wraps the
// response under the stage id before filtering).
func f1ListStage(id string) *templates.API {
	return &templates.API{
		Name:   id,
		Path:   "/apis/widgets.krateo.io/v1/widgets",
		Verb:   ptr.To(http.MethodGet),
		Filter: ptr.To("." + id + ".items"),
	}
}

// f1ResolveAs drives the REAL api.Resolve stage loop for a one-stage
// RESTAction as `username`. No resolveOnceFn stub — the content pipeline
// (apistageContentServe: content Get / un-gated dispatch / gate) runs
// for real. Returns the resolved stage output dict[id].
func f1ResolveAs(t *testing.T, username string, stage *templates.API) map[string]any {
	t.Helper()
	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: username}),
	)
	// The stage has no EndpointRef; resolveOne consults the
	// context-carried internal endpoint first. The widgets LIST is
	// served by the informer pivot (dispatchViaInformer reads the
	// informer store, never the endpoint), so this endpoint is only a
	// placeholder that lets resolveOne succeed without a per-user
	// clientconfig Secret lookup.
	ctx = cache.WithInternalEndpoint(ctx, &endpoints.Endpoint{ServerURL: "http://test.invalid"})
	dict := Resolve(ctx, ResolveOptions{
		// A non-nil RC keeps api.Resolve off its rest.InClusterConfig()
		// early-return (which fails outside a cluster). The stage LIST is
		// served by the informer pivot (dispatchViaInformer reads the
		// informer store, not the endpoint), so RC's contents are never
		// dereferenced — a bare *rest.Config is enough.
		RC:    &rest.Config{},
		Items: []*templates.API{stage},
		// RESTAction identity — F1's content key is per-K8s-call, so
		// these are not folded into the content key, but Resolve still
		// reads them for logging / dep recording.
		RESTActionNamespace: "default",
		RESTActionName:      "f1-widget-restaction",
	})
	return dict
}

// f1WidgetNamesIn extracts the set of widget names from a resolved stage
// output (dict[id] is the post-filter `.items` array of widget objects).
func f1WidgetNamesIn(v any) map[string]bool {
	out := map[string]bool{}
	items, ok := v.([]any)
	if !ok {
		return out
	}
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		meta, _ := m["metadata"].(map[string]any)
		if meta == nil {
			continue
		}
		if name, _ := meta["name"].(string); name != "" {
			out[name] = true
		}
	}
	return out
}

func f1SortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- TestFalsifierF1_CrossUserNoLeak --------------------------------------

// TestFalsifierF1_CrossUserNoLeak is the headline F1 falsifier. The broad
// user resolves first — the stage's widgets LIST is dispatched un-gated
// and the raw 4-namespace envelope is stored in the identity-free content
// entry. The narrow user then resolves the SAME stage: their content Get
// HITs the broad user's stored entry. Their dict[id] MUST be narrowed to
// their 2 authorized namespaces — they must NOT see the broad user's
// 4-namespace view.
//
// PRE-FIX (gate skipped on the hit path) the narrow user sees all 4
// widgets — the cross-user leak. The fix runs the serve-time RBAC gate on
// the Get-hit path too.
func TestFalsifierF1_CrossUserNoLeak(t *testing.T) {
	newF1Watcher(t)
	stage := f1ListStage("widgets")

	// Broad user resolves first — populates the content entry un-gated.
	broadDict := f1ResolveAs(t, f1BroadUser, stage)
	broadNames := f1WidgetNamesIn(broadDict["widgets"])
	if len(broadNames) != len(f1AllNamespaces) {
		t.Fatalf("setup: broad user resolved %d widgets, want %d (all namespaces): %v",
			len(broadNames), len(f1AllNamespaces), f1SortedKeys(broadNames))
	}

	// Narrow user resolves the SAME stage — content Get HITs the broad
	// user's stored entry. The serve-time gate must narrow it to the
	// narrow user's 2 authorized namespaces.
	narrowDict := f1ResolveAs(t, f1NarrowUser, stage)
	narrowNames := f1WidgetNamesIn(narrowDict["widgets"])

	if len(narrowNames) != len(f1NarrowNamespaces) {
		t.Fatalf("F1 CROSS-USER LEAK: narrow user saw %d widgets, want %d "+
			"(only their %v namespaces). The narrow user Get-hit the broad "+
			"user's identity-free content entry and the serve-time RBAC gate "+
			"did NOT narrow it — they see the broad user's view.\n want: %v\n got:  %v",
			len(narrowNames), len(f1NarrowNamespaces), f1NarrowNamespaces,
			[]string{"widget-team-a", "widget-team-b"}, f1SortedKeys(narrowNames))
	}
	for _, ns := range f1NarrowNamespaces {
		if !narrowNames["widget-"+ns] {
			t.Fatalf("F1: narrow user missing authorized widget widget-%s; got %v",
				ns, f1SortedKeys(narrowNames))
		}
	}
	// And NONE of the namespaces the narrow user is NOT authorized for.
	for _, ns := range []string{"team-c", "team-d"} {
		if narrowNames["widget-"+ns] {
			t.Fatalf("F1 CROSS-USER LEAK: narrow user saw widget-%s — a namespace "+
				"they have NO RBAC grant for; the content entry leaked the broad "+
				"user's view", ns)
		}
	}
}

// --- TestFalsifierF1_GatedFromStoredByteIdentical -------------------------

// TestFalsifierF1_GatedFromStoredByteIdentical asserts AC-F1.2: the
// gated-from-stored-content dict[id] is byte-identical to a fresh
// inline-gated resolve for the same identity — INCLUDING a stage `filter`
// that strips metadata.namespace.
//
// Reading 1's whole point: the content entry stores the RAW pre-filter
// apiserver envelope; the RBAC gate runs on THAT (metadata.namespace
// present); the stage `filter` runs AFTER the gate. So a filter that
// projects away metadata.namespace cannot defeat the gate.
//
// Method: first resolve (broad user — cold, populates the content
// entry). Then the narrow user resolves the SAME stage TWICE: once
// served from the stored content entry (hit), and the assertion is that
// the narrow user's output is correctly narrowed even though the stage
// filter strips namespace. A degenerate gate (gating post-filter) would
// see no namespace and fail-closed-drop-all or leak.
func TestFalsifierF1_GatedFromStoredByteIdentical(t *testing.T) {
	newF1Watcher(t)

	// Stage filter PROJECTS each widget to just its name — stripping
	// metadata.namespace. Under Reading 1 the gate already ran on the
	// raw envelope, so the projection is harmless.
	stage := &templates.API{
		Name:   "widgets",
		Path:   "/apis/widgets.krateo.io/v1/widgets",
		Verb:   ptr.To(http.MethodGet),
		Filter: ptr.To("[.widgets.items[] | {name: .metadata.name}]"),
	}

	// Broad user first — cold-resolves, populates the content entry.
	_ = f1ResolveAs(t, f1BroadUser, stage)

	// Narrow user — served from the stored content entry (Get-hit).
	narrowDict := f1ResolveAs(t, f1NarrowUser, stage)

	// The projected output: a list of {name: ...}. The narrow user must
	// see exactly their 2 authorized widgets — the gate ran on the raw
	// envelope BEFORE the namespace-stripping projection.
	got := map[string]bool{}
	if items, ok := narrowDict["widgets"].([]any); ok {
		for _, it := range items {
			if m, ok := it.(map[string]any); ok {
				if name, _ := m["name"].(string); name != "" {
					got[name] = true
				}
			}
		}
	}
	want := map[string]bool{"widget-team-a": true, "widget-team-b": true}
	if len(got) != len(want) {
		t.Fatalf("AC-F1.2: namespace-stripping filter — narrow user got %d projected "+
			"widgets, want %d. The gate must run on the RAW pre-filter envelope "+
			"(Reading 1); a post-filter gate cannot see metadata.namespace.\n"+
			" want: %v\n got:  %v", len(got), len(want),
			f1SortedKeys(want), f1SortedKeys(got))
	}
	for name := range want {
		if !got[name] {
			t.Fatalf("AC-F1.2: narrow user missing authorized %q; got %v",
				name, f1SortedKeys(got))
		}
	}

	// Byte-identical check: a SECOND narrow-user resolve (also a content
	// Get-hit) yields the identical dict[id] — the gate is a pure
	// function of (stored raw envelope, identity, RBAC store).
	narrowDict2 := f1ResolveAs(t, f1NarrowUser, stage)
	b1, _ := json.Marshal(narrowDict["widgets"])
	b2, _ := json.Marshal(narrowDict2["widgets"])
	if string(b1) != string(b2) {
		t.Fatalf("AC-F1.2: two narrow-user resolves of the same stage produced "+
			"different output — the serve-time gate is not deterministic\n"+
			" first:  %s\n second: %s", b1, b2)
	}
}

// --- TestFalsifierF1_ContentEntryIdentityFreeShared -----------------------

// TestFalsifierF1_ContentEntryIdentityFreeShared asserts AC-F1.3: two
// distinct users resolving the same stage produce exactly ONE shared
// content entry per K8s call — the content key is identity-free.
//
// The stage LISTs widgets cluster-wide = one K8s call. After both users
// resolve, the resolved-output store must hold exactly ONE apistage
// content entry (apistage_store_total advanced by exactly 1 — the broad
// user's cold dispatch Put; the narrow user's resolve is a content
// Get-hit, no second Put).
func TestFalsifierF1_ContentEntryIdentityFreeShared(t *testing.T) {
	newF1Watcher(t)
	stage := f1ListStage("widgets")

	store := cache.ResolvedCache()
	if store == nil {
		t.Fatalf("resolved cache nil under RESOLVED_CACHE_ENABLED=true")
	}
	before := store.Stats().ApistageStoreTotal

	// Broad user — cold resolve, ONE content Put for the cluster-wide LIST.
	_ = f1ResolveAs(t, f1BroadUser, stage)
	afterBroad := store.Stats().ApistageStoreTotal
	if afterBroad != before+1 {
		t.Fatalf("AC-F1.3: broad-user resolve Put %d apistage entries, want exactly 1 "+
			"(one cluster-wide widgets LIST = one content entry)", afterBroad-before)
	}

	// Narrow user resolves the SAME stage — the content key is
	// identity-free, so this is a Get-HIT: NO new content entry.
	_ = f1ResolveAs(t, f1NarrowUser, stage)
	afterNarrow := store.Stats().ApistageStoreTotal
	if afterNarrow != afterBroad {
		t.Fatalf("AC-F1.3: narrow-user resolve Put %d MORE apistage entries, want 0 "+
			"— the content entry is identity-free and must be SHARED; a second "+
			"Put means the key still carries identity", afterNarrow-afterBroad)
	}

	// And the store holds exactly one apistage entry total.
	if total := afterNarrow - before; total != 1 {
		t.Fatalf("AC-F1.3: %d apistage content entries after two users resolved the "+
			"same stage, want exactly 1 (identity-free, shared)", total)
	}
}
