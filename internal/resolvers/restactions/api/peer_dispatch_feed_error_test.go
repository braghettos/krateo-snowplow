// peer_dispatch_feed_error_test.go — Build backlog #6 falsifiers.
//
// THE DEFECT (TRACED + PM-confirmed): four served-path branches in
// dispatchOneCall (resolve.go) do a raw `return err` on a feedBytes/feedValue
// error, instead of routing it through recordItemError (Option C-A, #313).
// A feed* error is REACHABLE via the per-item stage FILTER zero-yield
// (handler.go:154 `jq filter %q yielded no value`) and multi-yield
// (handler.go:142 ErrMultiYield) — both converge on jsonHandlerCore where the
// filter runs. On the raw `return err` the worker returns non-nil → g.Wait()
// non-nil → runStage returns stop=true → the whole resolve abandons the stage
// AND all downstream stages, serving partial content (the #313 C-A violation).
//
// The 4 PEER served branches (find-by-code, MAIN line numbers differ from the
// audit's #35-branch refs):
//   1. nested /call          — feedBytes(statusRaw)   after nestedCallResolver
//   2. apistage gated env.    — feedValue(gatedVal)    after apistageContentServe
//   3. informer-served        — feedBytes(raw)         after dispatchViaInformer
//   4. internal-rest-config   — feedBytes(raw)         after dispatchViaInternalRESTConfig
//
// The external-endpoint branch (httpcall.Do / httpFetchAllowingNonJSON) is
// already #313-correct (it routes the feed error through recordItemError) and
// is NOT touched here (#35, parked separately).
//
// ORACLE = the #313 Option C-A contract on HEAD: a per-item iterator error
// (here a per-item FILTER zero-/multi-yield) must NOT skip remaining items or
// downstream stages.  No pre-0.30.128 git-worktree oracle — these served
// branches never went through httpcall.Do, so a pre-0.30.128 baseline would
// FALSELY show no-truncation (architect-confirmed).
//
// PER-SITE DRIVER (mirrors resolve_iter_continue_test.go + the F1 fixtures):
// a 2-stage RA. Stage 1 is an iterator whose served-branch IS the site under
// test; its `filter` ZERO-YIELDS on exactly ONE item (jq `select(...)` that
// drops the failing item's name) and yields the sibling items. Stage 2
// (`downstream`, no iterator) writes dict["downstream"].
//
// CONTENT assertions (NOT HTTP-200 / key-count — feedback_validate_content_not_just_status):
//   RED on the unfixed branch: dict["downstream"] ABSENT (truncated),
//                              dict[errorKey] ABSENT.
//   GREEN after fix:           dict["downstream"] PRESENT,
//                              dict[errorKey] carries the feed error,
//                              the successful sibling item present in dict[id].
//
// json.Unmarshal sub-mode (malformed bytes) is UNREACHABLE — every dispatch
// source emits valid JSON — so it is NOT exercised here
// (feedback_no_fake_production_scenarios).

package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// pdfeNamespaces are the iterator elements every site fans over. The element
// at index pdfeFailIdx is the one whose served response the stage filter
// ZERO-YIELDS on (so its feed* call errors), proving the non-truncation
// contract: the other namespaces (siblings) must still merge AND the
// downstream stage must still run.
var pdfeNamespaces = []string{"ns-a", "ns-b", "ns-c", "ns-d"}

const pdfeFailIdx = 2 // "ns-c" is the failing element

// pdfeVals builds the iterator element array carried in Extras:
// {"vals":[{"ns":"ns-a"},...]}.
func pdfeVals() []any {
	out := make([]any, 0, len(pdfeNamespaces))
	for _, ns := range pdfeNamespaces {
		out = append(out, map[string]any{"ns": ns})
	}
	return out
}

// pdfeResolveCtx builds the base resolve context (UserInfo required by
// Resolve). The username is admin-broad so every site's RBAC gate is a
// transparent pass-through — the only per-item failure under test is the
// FILTER zero-yield, never an RBAC drop.
func pdfeResolveCtx(username string) context.Context {
	return xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: username}),
	)
}

// pdfeAssertNoTruncation is the shared CONTENT oracle. dict is the full
// Resolve output, id is the iterator stage id, sibling is a substring that
// must appear in the merged dict[id] (a successful sibling item), and errKey
// is the stage's ErrorKey.
//
//	GREEN (fixed): downstream present, dict[errKey] carries the feed error,
//	               sibling present in dict[id].
//	RED (unfixed): downstream absent (truncated) AND dict[errKey] absent.
func pdfeAssertNoTruncation(t *testing.T, dict map[string]any, id, errKey, sibling string) {
	t.Helper()

	_, downstreamPresent := dict["downstream"]
	_, errPresent := dict[errKey]

	if !downstreamPresent {
		t.Errorf("TRUNCATION: dict[\"downstream\"] absent — the per-item feed error truncated "+
			"the whole resolve (raw `return err` instead of recordItemError). dict keys=%v", keysOf(dict))
	}
	if !errPresent {
		t.Errorf("dict[%q] absent — the feed error was not recorded via recordItemError. "+
			"dict keys=%v", errKey, keysOf(dict))
	}

	// The recorded error must be an accumulating slice (W-A) carrying the
	// feed error (the zero-yield message).
	if errPresent {
		if es, ok := dict[errKey].([]any); !ok {
			t.Errorf("dict[%q] is %T, want []any (accumulating slice)", errKey, dict[errKey])
		} else if len(es) == 0 {
			t.Errorf("dict[%q] empty slice; want >=1 feed error", errKey)
		} else {
			joined := fmt.Sprintf("%v", es)
			// The two reachable feed (filter) errors: zero-yield
			// (handler.go:154 "jq filter %q yielded no value") and
			// multi-yield (ErrMultiYield — "jq query yielded more than one
			// value, expected exactly one").
			if !containsAny(joined, "yielded no value", "more than one value") {
				t.Errorf("dict[%q] does not carry the feed (filter) error: %v", errKey, es)
			}
		}
	}

	// A successful sibling must be present in the merged stage output.
	if sibling != "" {
		if !containsAny(fmt.Sprintf("%v", dict[id]), sibling) {
			t.Errorf("successful sibling %q absent from dict[%q]: %v", sibling, id, dict[id])
		}
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// SITE 1 — nested /call (feedBytes(statusRaw))
// ---------------------------------------------------------------------------
//
// SEAM: RegisterNestedCallResolver returns each item's referenced RESTAction
// Status.Raw as valid JSON {"ns":"<ns>"}. The stage Path is a /call loopback
// (objects.ParseCallPathToObjectRef must parse it) carrying the per-item ns.
// The filter `.site1 | select(.ns != "ns-c")` ZERO-YIELDS for ns-c → that
// item's feedBytes(statusRaw) returns the zero-yield error.
//
// RED on HEAD: site 1's `if err := feedBytes(statusRaw); err != nil { return err }`
// truncates → downstream absent.
func TestPeerFeedError_Site1_NestedCall_ZeroYield_NoTruncate(t *testing.T) {
	t.Setenv("RESOLVER_INPROCESS_NESTED_CALL", "true")
	t.Setenv("RESOLVER_ITER_PARALLELISM", "1")

	// Wire the nested-call resolver seam: return each ref's name as JSON.
	prev := nestedCallResolver
	nestedCallResolver = func(_ context.Context, ref templates.ObjectReference, _, _ int, _ map[string]any) ([]byte, error) {
		// ref.Name carries the per-item ns (set via the /call path template).
		return []byte(fmt.Sprintf(`{"ns":%q}`, ref.Name)), nil
	}
	t.Cleanup(func() { nestedCallResolver = prev })

	id := "site1"
	itemsStage := &templates.API{
		Name: id,
		// /call loopback path; the ns becomes the referenced object name so
		// the nested resolver echoes it back. objects.ParseCallPathToObjectRef
		// parses resource+apiVersion+name from the query.
		Path: `${ "/call?resource=widgets&apiVersion=widgets.krateo.io%2Fv1&name=" + .ns }`,
		Verb: ptr.To(http.MethodGet),
		DependsOn: &templates.Dependency{
			Iterator: ptr.To(".vals"),
		},
		Filter:   ptr.To(fmt.Sprintf(`.%s | select(.ns != "%s")`, id, pdfeNamespaces[pdfeFailIdx])),
		ErrorKey: ptr.To("error"),
	}
	downstream := &templates.API{
		Name:      "downstream",
		Path:      `${ "/call?resource=widgets&apiVersion=widgets.krateo.io%2Fv1&name=down" }`,
		Verb:      ptr.To(http.MethodGet),
		DependsOn: &templates.Dependency{Name: id},
		Filter:    ptr.To(".downstream"),
	}

	ctx := pdfeResolveCtx("site1-user")
	ctx = cache.WithInternalEndpoint(ctx, &endpoints.Endpoint{ServerURL: "http://test.invalid"})
	dict := Resolve(ctx, ResolveOptions{
		RC:                  &rest.Config{},
		Items:               []*templates.API{itemsStage, downstream},
		Extras:              map[string]any{"vals": pdfeVals()},
		RESTActionNamespace: "default",
		RESTActionName:      "peer-feed-site1",
	})

	pdfeAssertNoTruncation(t, dict, id, "error", "ns-a")
}

// ---------------------------------------------------------------------------
// SITE 1 multi-yield variant (ErrMultiYield, same return seam)
// ---------------------------------------------------------------------------
//
// The filter `.site1.list[]` over a multi-element array MULTI-YIELDS (>=2
// values) → EvalValue returns ErrMultiYield (handler.go:142). The nested
// resolver returns a 2-element list for the failing ns so feedBytes errors
// on that item only.
func TestPeerFeedError_Site1_NestedCall_MultiYield_NoTruncate(t *testing.T) {
	t.Setenv("RESOLVER_INPROCESS_NESTED_CALL", "true")
	t.Setenv("RESOLVER_ITER_PARALLELISM", "1")

	failNS := pdfeNamespaces[pdfeFailIdx]
	prev := nestedCallResolver
	nestedCallResolver = func(_ context.Context, ref templates.ObjectReference, _, _ int, _ map[string]any) ([]byte, error) {
		if ref.Name == failNS {
			// 2-element list → `.list[]` multi-yields → ErrMultiYield.
			return []byte(`{"list":[{"ns":"x"},{"ns":"y"}]}`), nil
		}
		// single-element list → `.list[]` single-yields → merges fine.
		return []byte(fmt.Sprintf(`{"list":[{"ns":%q}]}`, ref.Name)), nil
	}
	t.Cleanup(func() { nestedCallResolver = prev })

	id := "site1"
	itemsStage := &templates.API{
		Name:      id,
		Path:      `${ "/call?resource=widgets&apiVersion=widgets.krateo.io%2Fv1&name=" + .ns }`,
		Verb:      ptr.To(http.MethodGet),
		DependsOn: &templates.Dependency{Iterator: ptr.To(".vals")},
		Filter:    ptr.To(fmt.Sprintf(`.%s.list[]`, id)),
		ErrorKey:  ptr.To("error"),
	}
	downstream := &templates.API{
		Name:      "downstream",
		Path:      `${ "/call?resource=widgets&apiVersion=widgets.krateo.io%2Fv1&name=down" }`,
		Verb:      ptr.To(http.MethodGet),
		DependsOn: &templates.Dependency{Name: id},
		Filter:    ptr.To(".downstream"),
	}

	ctx := pdfeResolveCtx("site1-user")
	ctx = cache.WithInternalEndpoint(ctx, &endpoints.Endpoint{ServerURL: "http://test.invalid"})
	dict := Resolve(ctx, ResolveOptions{
		RC:                  &rest.Config{},
		Items:               []*templates.API{itemsStage, downstream},
		Extras:              map[string]any{"vals": pdfeVals()},
		RESTActionNamespace: "default",
		RESTActionName:      "peer-feed-site1-multi",
	})

	pdfeAssertNoTruncation(t, dict, id, "error", "ns-a")
}

// ---------------------------------------------------------------------------
// shared informer/apistage fixture (sites 2 & 3)
// ---------------------------------------------------------------------------

var pdfeGVR = schema.GroupVersionResource{
	Group:    "widgets.krateo.io",
	Version:  "v1",
	Resource: "widgets",
}

// pdfeWidget builds an unstructured widget named after its namespace so the
// per-item filter can target exactly one item by content.
func pdfeWidget(ns string) runtime.Object {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "widgets.krateo.io/v1",
		"kind":       "Widget",
		"metadata": map[string]any{
			"namespace": ns,
			"name":      "widget-" + ns,
		},
	}}
}

// pdfeAdminUser is granted a cluster-wide wildcard so the dispatch RBAC gate
// is transparent — the only per-item failure under test is the filter
// zero-yield.
const pdfeAdminUser = "pdfe-admin"

func pdfeAdminRBACSeed() []runtime.Object {
	return []runtime.Object{
		&rbacv1.ClusterRole{
			TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
			ObjectMeta: metav1.ObjectMeta{Name: "pdfe-admin-role"},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}}},
		},
		&rbacv1.ClusterRoleBinding{
			TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
			ObjectMeta: metav1.ObjectMeta{Name: "pdfe-admin-binding"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: "rbac.authorization.k8s.io", Name: pdfeAdminUser}},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "pdfe-admin-role"},
		},
	}
}

// pdfeListKinds is the dynamicfake LIST-kind map (widgets + RBAC kinds the
// watcher constructor eagerly registers).
func pdfeListKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		pdfeGVR: "WidgetList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}:               "RoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}:        "RoleBindingList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}:        "ClusterRoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}: "ClusterRoleBindingList",
	}
}

// pdfeNewWatcher stands up a synced cache=on watcher seeded with one widget
// per namespace plus the admin RBAC. apistageOn flips
// RESOLVED_CACHE_APISTAGE_ENABLED (site 2 vs site 3).
func pdfeNewWatcher(t *testing.T, apistageOn bool) *cache.ResourceWatcher {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	if apistageOn {
		t.Setenv("RESOLVED_CACHE_APISTAGE_ENABLED", "true")
	} else {
		t.Setenv("RESOLVED_CACHE_APISTAGE_ENABLED", "false")
	}
	cache.ResetResolvedCacheForTest()
	cache.ResetDepsForTest()
	t.Cleanup(func() {
		cache.ResetResolvedCacheForTest()
		cache.ResetDepsForTest()
	})

	var seed []runtime.Object
	for _, ns := range pdfeNamespaces {
		seed = append(seed, pdfeWidget(ns))
	}
	seed = append(seed, pdfeAdminRBACSeed()...)

	scheme := runtime.NewScheme()
	_ = rbacv1.AddToScheme(scheme)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, pdfeListKinds(), seed...)

	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil {
		t.Fatalf("expected non-nil watcher under CACHE_ENABLED=true")
	}
	t.Cleanup(rw.Stop)

	added, syncCh := rw.EnsureResourceType(pdfeGVR)
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

// pdfeGetByNameStage builds an iterator stage that GET-by-names one widget per
// namespace from the informer/apistage cache. The filter zero-yields on the
// failing namespace's widget (by name) and yields the siblings.
func pdfeGetByNameStage(id string, multiYield bool) *templates.API {
	var filter string
	if multiYield {
		// `.id | .x[]` over a 2-element array → ErrMultiYield. The widget has
		// no `.x`; we instead build a synthetic 2-element via the stage path
		// returning the bare object — so for multi-yield we filter `.id |
		// (.metadata, .metadata)` which yields twice. Simpler: wrap object in
		// a 2-element array via `[.id.metadata, .id.metadata] | .[]`.
		filter = fmt.Sprintf(`[.%s.metadata, .%s.metadata] | .[]`, id, id)
	} else {
		failName := "widget-" + pdfeNamespaces[pdfeFailIdx]
		filter = fmt.Sprintf(`.%s | select(.metadata.name != "%s")`, id, failName)
	}
	return &templates.API{
		Name: id,
		// GET-by-name per namespace; name=widget-<ns> exists in the seed.
		Path:      `${ "/apis/widgets.krateo.io/v1/namespaces/" + .ns + "/widgets/widget-" + .ns }`,
		Verb:      ptr.To(http.MethodGet),
		DependsOn: &templates.Dependency{Iterator: ptr.To(".vals")},
		Filter:    ptr.To(filter),
		ErrorKey:  ptr.To("error"),
	}
}

// pdfeResolveCached drives the REAL api.Resolve over the informer/apistage
// cache as pdfeAdminUser, with the downstream stage GET-by-name'ing an extra
// widget (ns-a) so its presence proves the loop continued.
func pdfeResolveCached(t *testing.T, watcher *cache.ResourceWatcher, stage *templates.API) map[string]any {
	t.Helper()
	id := stage.Name
	downstream := &templates.API{
		Name:      "downstream",
		Path:      `/apis/widgets.krateo.io/v1/namespaces/ns-a/widgets/widget-ns-a`,
		Verb:      ptr.To(http.MethodGet),
		DependsOn: &templates.Dependency{Name: id},
		Filter:    ptr.To(".downstream"),
	}
	ctx := pdfeResolveCtx(pdfeAdminUser)
	ctx = cache.WithInternalEndpoint(ctx, &endpoints.Endpoint{ServerURL: "http://test.invalid"})
	return Resolve(ctx, ResolveOptions{
		RC:                  &rest.Config{},
		Items:               []*templates.API{stage, downstream},
		Extras:              map[string]any{"vals": pdfeVals()},
		Watcher:             watcher,
		RESTActionNamespace: "default",
		RESTActionName:      "peer-feed-cached",
	})
}

// ---------------------------------------------------------------------------
// SITE 2 — apistage gated envelope (feedValue(gatedVal))
// ---------------------------------------------------------------------------
//
// RESOLVED_CACHE_APISTAGE_ENABLED=true → the apistageContentServe path feeds a
// decoded gated value via feedValue. The failing item's filter zero-yields →
// feedValue returns the error → site 2's raw `return err` truncates.
func TestPeerFeedError_Site2_ApistageContent_ZeroYield_NoTruncate(t *testing.T) {
	t.Setenv("RESOLVER_ITER_PARALLELISM", "1")
	rw := pdfeNewWatcher(t, true) // apistage ON
	dict := pdfeResolveCached(t, rw, pdfeGetByNameStage("site2", false))
	pdfeAssertNoTruncation(t, dict, "site2", "error", "widget-ns-a")
}

func TestPeerFeedError_Site2_ApistageContent_MultiYield_NoTruncate(t *testing.T) {
	t.Setenv("RESOLVER_ITER_PARALLELISM", "1")
	rw := pdfeNewWatcher(t, true)
	dict := pdfeResolveCached(t, rw, pdfeGetByNameStage("site2", true))
	pdfeAssertNoTruncation(t, dict, "site2", "error", "")
}

// ---------------------------------------------------------------------------
// SITE 3 — informer-served (feedBytes(raw))
// ---------------------------------------------------------------------------
//
// apistage OFF → the dispatchViaInformer branch feeds raw bytes via feedBytes.
// The failing item's filter zero-yields → feedBytes returns the error → site
// 3's raw `return err` truncates.
func TestPeerFeedError_Site3_Informer_ZeroYield_NoTruncate(t *testing.T) {
	t.Setenv("RESOLVER_ITER_PARALLELISM", "1")
	rw := pdfeNewWatcher(t, false) // apistage OFF → informer branch
	dict := pdfeResolveCached(t, rw, pdfeGetByNameStage("site3", false))
	pdfeAssertNoTruncation(t, dict, "site3", "error", "widget-ns-a")
}

func TestPeerFeedError_Site3_Informer_MultiYield_NoTruncate(t *testing.T) {
	t.Setenv("RESOLVER_ITER_PARALLELISM", "1")
	rw := pdfeNewWatcher(t, false)
	dict := pdfeResolveCached(t, rw, pdfeGetByNameStage("site3", true))
	pdfeAssertNoTruncation(t, dict, "site3", "error", "")
}

// ---------------------------------------------------------------------------
// SITE 4 — internal-rest-config (feedBytes(raw))
// ---------------------------------------------------------------------------
//
// cache.WithInternalRESTConfig + a TLS httptest apiserver (the SAME real
// client-go transport path internal_dispatch_paged_test.go drives, via
// internalClientFor — no new fake-client seam) → the
// dispatchViaInternalRESTConfig branch serves GET-by-name and feeds raw bytes
// via feedBytes. The failing item's filter zero-yields → feedBytes errors →
// site 4's raw `return err` truncates.
//
// CACHE_ENABLED stays unset → resolverUseInformer() false → the informer block
// is skipped; the internal-rest-config block (gated on the context
// *rest.Config) owns the GET.
func TestPeerFeedError_Site4_InternalRESTConfig_ZeroYield_NoTruncate(t *testing.T) {
	t.Setenv("RESOLVER_ITER_PARALLELISM", "1")
	dict := pdfeResolveInternalRC(t, false)
	pdfeAssertNoTruncation(t, dict, "site4", "error", "widget-ns-a")
}

func TestPeerFeedError_Site4_InternalRESTConfig_MultiYield_NoTruncate(t *testing.T) {
	t.Setenv("RESOLVER_ITER_PARALLELISM", "1")
	dict := pdfeResolveInternalRC(t, true)
	pdfeAssertNoTruncation(t, dict, "site4", "error", "")
}

// pdfeInternalRCFixture is a TLS apiserver answering GET-by-name for
// /apis/widgets.krateo.io/v1/namespaces/<ns>/widgets/widget-<ns> with the bare
// widget object (the exact wire shape client-go's dynamic Get reads).
func pdfeInternalRCFixture(t *testing.T) (*rest.Config, func()) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Path: /apis/widgets.krateo.io/v1/namespaces/<ns>/widgets/widget-<ns>
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		name := parts[len(parts)-1]
		ns := ""
		if len(parts) >= 2 {
			ns = parts[len(parts)-3]
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w,
			`{"apiVersion":"widgets.krateo.io/v1","kind":"Widget",`+
				`"metadata":{"namespace":%q,"name":%q}}`, ns, name)
	}))
	caPEM := pemEncodeCert(srv.Certificate())
	rc := &rest.Config{
		Host:            srv.URL,
		BearerToken:     "fake-sa-jwt",
		TLSClientConfig: rest.TLSClientConfig{CAData: caPEM},
	}
	return rc, srv.Close
}

// pdfeResolveInternalRC drives api.Resolve with a TLS apiserver attached via
// cache.WithInternalRESTConfig, so the internal-rest-config dispatch branch
// owns the per-item GET-by-name.
func pdfeResolveInternalRC(t *testing.T, multiYield bool) map[string]any {
	t.Helper()
	resetInternalClientCacheForTest()
	t.Cleanup(resetInternalClientCacheForTest)

	rc, closeSrv := pdfeInternalRCFixture(t)
	t.Cleanup(closeSrv)

	id := "site4"
	stage := pdfeGetByNameStage(id, multiYield)

	downstream := &templates.API{
		Name:      "downstream",
		Path:      `/apis/widgets.krateo.io/v1/namespaces/ns-a/widgets/widget-ns-a`,
		Verb:      ptr.To(http.MethodGet),
		DependsOn: &templates.Dependency{Name: id},
		Filter:    ptr.To(".downstream"),
	}

	ctx := pdfeResolveCtx(pdfeAdminUser)
	ctx = cache.WithInternalEndpoint(ctx, &endpoints.Endpoint{ServerURL: "http://test.invalid"})
	ctx = cache.WithInternalRESTConfig(ctx, rc)
	return Resolve(ctx, ResolveOptions{
		RC:                  &rest.Config{},
		Items:               []*templates.API{stage, downstream},
		Extras:              map[string]any{"vals": pdfeVals()},
		RESTActionNamespace: "default",
		RESTActionName:      "peer-feed-site4",
	})
}
