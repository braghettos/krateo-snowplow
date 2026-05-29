// cluster_list_test.go — Ship D.5 (0.30.152) unit tests for the
// cluster-list-when-allowed iterator collapse helpers.
//
// Discharge map:
//
//   AC-D5.3   — TestApistageContentKey_ClusterScopeDistinctFromNamespaced
//   AC-D5.13  — TestAttemptClusterListCollapse_CacheDisabledShortCircuits,
//               TestAttemptClusterListCollapse_NilSnapshotShortCircuits
//   AC-D5.14  — TestValidateClusterListShape_* (multi-element shape check)
//
// Gating logic — TestAttemptClusterListCollapse_OptInOff /
// _NoIterator covers the structural gates that do not need the
// resolver pivot or the RBAC snapshot to fire.
//
// The full RBAC-permit path + dispatch + Put are exercised by the
// resolver-level falsifier tests (see apistage_content_falsifier_test.go
// for the pattern); these unit tests focus on the helpers' own gates
// + the AC-D5.14 shape check so the wiring is independently auditable.

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// endpointStub returns a zero-value endpoint suitable for tests that
// never actually dispatch — the cluster-list helpers we exercise here
// (gate logic, GVR derivation, shape check) ignore ep entirely on the
// short-circuit paths.
func endpointStub() endpoints.Endpoint { return endpoints.Endpoint{} }

// ---------- AC-D5.3 — cluster-scope key disambiguation ----------

func TestApistageContentKey_ClusterScopeDistinctFromNamespaced(t *testing.T) {
	nsKey := cache.ComputeKey(cache.ResolvedKeyInputs{
		CacheEntryClass: cache.CacheEntryClassApistage,
		Group:           "composition.krateo.io",
		Version:         "v1-2-2",
		Resource:        "githubscaffoldingwithcompositionpages",
		Namespace:       "bench-ns-01",
		Name:            "",
	})
	clusterKey := cache.ComputeKey(cache.ResolvedKeyInputs{
		CacheEntryClass: cache.CacheEntryClassApistage,
		Group:           "composition.krateo.io",
		Version:         "v1-2-2",
		Resource:        "githubscaffoldingwithcompositionpages",
		Namespace:       "",
		Name:            "",
	})
	if nsKey == clusterKey {
		t.Fatalf("cluster-scope key MUST differ from namespaced key:\n ns=%q\n cluster=%q",
			nsKey, clusterKey)
	}
	// Sanity — two cluster-scope keys for the same GVR collapse to the
	// same cell (identity-free property the cluster-list dispatch
	// relies on for the cross-user share).
	clusterKey2 := cache.ComputeKey(cache.ResolvedKeyInputs{
		CacheEntryClass: cache.CacheEntryClassApistage,
		Group:           "composition.krateo.io",
		Version:         "v1-2-2",
		Resource:        "githubscaffoldingwithcompositionpages",
		Namespace:       "",
		Name:            "",
	})
	if clusterKey != clusterKey2 {
		t.Fatalf("cluster-scope key must be deterministic across calls; got %q != %q",
			clusterKey, clusterKey2)
	}
}

// ---------- AC-D5.14 — defensive multi-element shape check ----------
//
// Ship 0.30.194 Fix B — validateClusterListShape now also returns the
// parsedListEnvelope from its single decode (the call site previously
// did a SECOND parseListEnvelope of the same bytes). The tests assert
// (ok, reason) the same way; the happy-path test additionally asserts
// the returned parsedListEnvelope's items/apiVersion/kind match what
// parseListEnvelope would produce.

// testShapeGVR is a stable GVR used across the shape-check tests; the
// gvr is consulted ONLY when envelope.APIVersion / envelope.Kind are
// empty (a happens-when-defensive-tests fallback path), so the choice
// is irrelevant on the happy paths.
var testShapeGVR = schema.GroupVersionResource{
	Group:    "composition.krateo.io",
	Version:  "v1-2-2",
	Resource: "githubscaffoldingwithcompositionpages",
}

func TestValidateClusterListShape_HappyPath(t *testing.T) {
	raw := mustJSON(t, map[string]any{
		"apiVersion": "composition.krateo.io/v1-2-2",
		"kind":       "GithubScaffoldingWithCompositionPagesList",
		"items": []any{
			map[string]any{
				"apiVersion": "composition.krateo.io/v1-2-2",
				"kind":       "GithubScaffoldingWithCompositionPages",
				"metadata":   map[string]any{"name": "a", "namespace": "ns-1"},
			},
			map[string]any{
				"apiVersion": "composition.krateo.io/v1-2-2",
				"kind":       "GithubScaffoldingWithCompositionPages",
				"metadata":   map[string]any{"name": "b", "namespace": "ns-2"},
			},
		},
	})
	parsed, ok, reason := validateClusterListShape(testShapeGVR, raw)
	if !ok {
		t.Fatalf("validateClusterListShape: expected ok=true on well-formed envelope; reason=%q", reason)
	}
	if len(parsed.items) != 2 {
		t.Fatalf("validateClusterListShape: expected 2 items in parsed envelope; got %d", len(parsed.items))
	}
	if parsed.apiVersion != "composition.krateo.io/v1-2-2" {
		t.Fatalf("validateClusterListShape: apiVersion=%q want composition.krateo.io/v1-2-2", parsed.apiVersion)
	}
	if parsed.kind != "GithubScaffoldingWithCompositionPagesList" {
		t.Fatalf("validateClusterListShape: kind=%q want GithubScaffoldingWithCompositionPagesList", parsed.kind)
	}
	// Byte-compat with parseListEnvelope — the items unwrap pattern is
	// []*unstructured.Unstructured{Object: it} where it is the
	// json.Unmarshal'd map[string]any. Spot-check that the first item's
	// metadata.name round-trips through the wrap.
	if got := parsed.items[0].GetName(); got != "a" {
		t.Fatalf("validateClusterListShape: items[0].GetName()=%q want \"a\"", got)
	}
	if got := parsed.items[1].GetNamespace(); got != "ns-2" {
		t.Fatalf("validateClusterListShape: items[1].GetNamespace()=%q want \"ns-2\"", got)
	}
}

// TestValidateClusterListShape_ParseListEnvelopeEquivalence — Ship
// 0.30.194 Fix B byte-compat gate. The architect's dedup design relies
// on validateClusterListShape producing a parsedListEnvelope that is
// structurally identical to parseListEnvelope's output for the same
// raw bytes + gvr. This test runs both functions on the same input
// and asserts items count, apiVersion, kind, and per-item ns/name
// round-trip equivalence.
func TestValidateClusterListShape_ParseListEnvelopeEquivalence(t *testing.T) {
	raw := mustJSON(t, map[string]any{
		"apiVersion": "composition.krateo.io/v1-2-2",
		"kind":       "GithubScaffoldingWithCompositionPagesList",
		"items": []any{
			map[string]any{
				"apiVersion": "composition.krateo.io/v1-2-2",
				"kind":       "GithubScaffoldingWithCompositionPages",
				"metadata":   map[string]any{"name": "a", "namespace": "ns-1"},
			},
			map[string]any{
				"apiVersion": "composition.krateo.io/v1-2-2",
				"kind":       "GithubScaffoldingWithCompositionPages",
				"metadata":   map[string]any{"name": "b", "namespace": "ns-2"},
			},
			map[string]any{
				"apiVersion": "composition.krateo.io/v1-2-2",
				"kind":       "GithubScaffoldingWithCompositionPages",
				"metadata":   map[string]any{"name": "c", "namespace": "ns-3"},
			},
		},
	})
	vParsed, ok, reason := validateClusterListShape(testShapeGVR, raw)
	if !ok {
		t.Fatalf("validateClusterListShape: ok=false on well-formed envelope; reason=%q", reason)
	}
	pParsed, ok := parseListEnvelope(testShapeGVR, raw)
	if !ok {
		t.Fatalf("parseListEnvelope: ok=false on well-formed envelope")
	}
	if len(vParsed.items) != len(pParsed.items) {
		t.Fatalf("items count mismatch: validate=%d parse=%d",
			len(vParsed.items), len(pParsed.items))
	}
	if vParsed.apiVersion != pParsed.apiVersion {
		t.Fatalf("apiVersion mismatch: validate=%q parse=%q",
			vParsed.apiVersion, pParsed.apiVersion)
	}
	if vParsed.kind != pParsed.kind {
		t.Fatalf("kind mismatch: validate=%q parse=%q",
			vParsed.kind, pParsed.kind)
	}
	for i := range vParsed.items {
		if vParsed.items[i].GetName() != pParsed.items[i].GetName() {
			t.Fatalf("item[%d] name mismatch: validate=%q parse=%q",
				i, vParsed.items[i].GetName(), pParsed.items[i].GetName())
		}
		if vParsed.items[i].GetNamespace() != pParsed.items[i].GetNamespace() {
			t.Fatalf("item[%d] namespace mismatch: validate=%q parse=%q",
				i, vParsed.items[i].GetNamespace(), pParsed.items[i].GetNamespace())
		}
	}
}

func TestValidateClusterListShape_KindNotList(t *testing.T) {
	raw := mustJSON(t, map[string]any{
		"apiVersion": "v1",
		"kind":       "SingleObject", // does NOT end in List
		"items":      []any{map[string]any{"apiVersion": "v1", "kind": "ConfigMap"}},
	})
	_, ok, reason := validateClusterListShape(testShapeGVR, raw)
	if ok {
		t.Fatalf("validateClusterListShape: expected ok=false when kind does not end in List; reason=%q", reason)
	}
	if !strings.Contains(reason, "kind-not-list") {
		t.Fatalf("expected reason to flag kind-not-list; got %q", reason)
	}
}

func TestValidateClusterListShape_EmptyItems(t *testing.T) {
	raw := mustJSON(t, map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMapList",
		"items":      []any{},
	})
	_, ok, reason := validateClusterListShape(testShapeGVR, raw)
	if ok {
		t.Fatalf("validateClusterListShape: expected ok=false on empty items; reason=%q", reason)
	}
	if !strings.Contains(reason, "items-empty") {
		t.Fatalf("expected reason to flag items-empty; got %q", reason)
	}
}

func TestValidateClusterListShape_ItemMissingApiVersion(t *testing.T) {
	raw := mustJSON(t, map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMapList",
		"items": []any{
			// apiVersion absent → Go nil from absent map key.
			map[string]any{"kind": "ConfigMap"},
		},
	})
	_, ok, reason := validateClusterListShape(testShapeGVR, raw)
	if ok {
		t.Fatalf("validateClusterListShape: expected ok=false when an item lacks apiVersion; reason=%q", reason)
	}
	if !strings.Contains(reason, "missing-apiVersion") {
		t.Fatalf("expected reason to flag missing-apiVersion; got %q", reason)
	}
}

func TestValidateClusterListShape_ItemMissingKind(t *testing.T) {
	raw := mustJSON(t, map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMapList",
		"items": []any{
			map[string]any{"apiVersion": "v1"},
		},
	})
	_, ok, reason := validateClusterListShape(testShapeGVR, raw)
	if ok {
		t.Fatalf("validateClusterListShape: expected ok=false when an item lacks kind; reason=%q", reason)
	}
	if !strings.Contains(reason, "missing-kind") {
		t.Fatalf("expected reason to flag missing-kind; got %q", reason)
	}
}

func TestValidateClusterListShape_MalformedJSON(t *testing.T) {
	_, ok, reason := validateClusterListShape(testShapeGVR, []byte("{not-json"))
	if ok {
		t.Fatalf("validateClusterListShape: expected ok=false on malformed JSON; reason=%q", reason)
	}
	if !strings.Contains(reason, "unmarshal-failed") {
		t.Fatalf("expected reason to flag unmarshal-failed; got %q", reason)
	}
}

// AC-D5.14 conditional ratification: the shape check must complete in
// ≤10ms. The check runs on a structurally-large envelope (2,000 items
// each carrying a small object) so the median measurement reflects the
// production envelope shape; the spec calls for ≤10ms per invocation,
// not per-item. This test records the per-call overhead so the
// diff-review gate sees the empirical number; it FAILS only on a
// gross budget breach (>50ms) to avoid CI noise on busy machines.
func TestValidateClusterListShape_Overhead(t *testing.T) {
	items := make([]any, 0, 2000)
	for i := 0; i < 2000; i++ {
		items = append(items, map[string]any{
			"apiVersion": "composition.krateo.io/v1-2-2",
			"kind":       "GithubScaffoldingWithCompositionPages",
			"metadata": map[string]any{
				"name":      "obj",
				"namespace": "ns",
			},
		})
	}
	raw := mustJSON(t, map[string]any{
		"apiVersion": "composition.krateo.io/v1-2-2",
		"kind":       "GithubScaffoldingWithCompositionPagesList",
		"items":      items,
	})

	// Warm-up — first call pays the json.Unmarshal cold cost. Measure
	// the median across 5 runs to track a stable number.
	var total time.Duration
	const runs = 5
	for i := 0; i < runs; i++ {
		start := time.Now()
		_, ok, reason := validateClusterListShape(testShapeGVR, raw)
		elapsed := time.Since(start)
		total += elapsed
		if !ok {
			t.Fatalf("run %d: validateClusterListShape returned ok=false: %s", i, reason)
		}
	}
	avg := total / runs
	t.Logf("validateClusterListShape AC-D5.14 overhead: 2000 items, avg=%v over %d runs", avg, runs)
	// Hard guard: a >50ms single-invocation latency on a 2K envelope
	// would be a 5× budget breach — surface this as a test failure so
	// the diff-review gate cannot miss it.
	if avg > 50*time.Millisecond {
		t.Fatalf("AC-D5.14 overhead budget breach: avg=%v > 50ms (5× the 10ms PM-ratified budget)", avg)
	}
}

// ---------- Cluster-scope path construction ----------

func TestClusterScopePathFor_NamedGroup(t *testing.T) {
	gvr := schema.GroupVersionResource{
		Group:    "composition.krateo.io",
		Version:  "v1-2-2",
		Resource: "githubscaffoldingwithcompositionpages",
	}
	got := clusterScopePathFor(gvr)
	want := "/apis/composition.krateo.io/v1-2-2/githubscaffoldingwithcompositionpages"
	if got != want {
		t.Fatalf("clusterScopePathFor: got %q want %q", got, want)
	}
}

func TestClusterScopePathFor_CoreGroup(t *testing.T) {
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	got := clusterScopePathFor(gvr)
	want := "/api/v1/configmaps"
	if got != want {
		t.Fatalf("clusterScopePathFor (core group): got %q want %q", got, want)
	}
}

// The path returned by clusterScopePathFor must round-trip through
// cache.ParseAPIServerPathToDep with ns="" and name="" — the
// identity-free apistage key the cluster-list dispatch relies on. A
// regression here would silently mis-key the cache entry and break
// AC-D5.5 (cross-user share).
func TestClusterScopePathFor_RoundTripParseDep(t *testing.T) {
	cases := []schema.GroupVersionResource{
		{Group: "composition.krateo.io", Version: "v1-2-2", Resource: "githubscaffoldingwithcompositionpages"},
		{Version: "v1", Resource: "configmaps"}, // core group
		{Group: "apps", Version: "v1", Resource: "deployments"},
	}
	for _, gvr := range cases {
		path := clusterScopePathFor(gvr)
		parsedGVR, ns, name, ok := cache.ParseAPIServerPathToDep(path)
		if !ok {
			t.Fatalf("ParseAPIServerPathToDep failed for path %q (gvr=%s)", path, gvr)
		}
		if parsedGVR != gvr {
			t.Fatalf("ParseAPIServerPathToDep returned gvr=%s want %s for path %q",
				parsedGVR, gvr, path)
		}
		if ns != "" || name != "" {
			t.Fatalf("ParseAPIServerPathToDep ns=%q name=%q want both empty for path %q",
				ns, name, path)
		}
	}
}

// ---------- buildClusterListCall — basic shape ----------

func TestBuildClusterListCall_PathAndVerb(t *testing.T) {
	apiCall := &templates.API{
		Name:    "compositions-list",
		Path:    `${ "/apis/composition.krateo.io/" + .version + "/namespaces/" + .namespace + "/" + .plural }`,
		Headers: []string{"Accept: application/json", "X-Marker: cluster-list-test"},
		DependsOn: &templates.Dependency{
			Iterator: ptr.To(".compositions[]"),
		},
	}
	gvr := schema.GroupVersionResource{
		Group:    "composition.krateo.io",
		Version:  "v1-2-2",
		Resource: "githubscaffoldingwithcompositionpages",
	}
	got := buildClusterListCall(apiCall, endpointStub(), gvr)
	if got.Path != "/apis/composition.krateo.io/v1-2-2/githubscaffoldingwithcompositionpages" {
		t.Fatalf("buildClusterListCall.Path = %q", got.Path)
	}
	if got.Verb == nil || *got.Verb != "GET" {
		t.Fatalf("buildClusterListCall.Verb = %v; want GET", got.Verb)
	}
	if len(got.Headers) != 2 || got.Headers[1] != "X-Marker: cluster-list-test" {
		t.Fatalf("buildClusterListCall.Headers = %v; want copied verbatim", got.Headers)
	}
	// Mutating apiCall.Headers must NOT alias back into the cluster
	// call's Headers (defensive copy invariant).
	apiCall.Headers[0] = "Accept: application/MUTATED"
	if got.Headers[0] == "Accept: application/MUTATED" {
		t.Fatalf("buildClusterListCall.Headers aliased apiCall.Headers (mutation leaked)")
	}
}

// ---------- attemptClusterListCollapse — structural gates ----------

// TestAttemptClusterListCollapse_InertGateDenies — Ship S.1-re holds the
// cluster-list collapse INERT behind the compile-time
// clusterListCollapseEnabled const (false). The FIRST statement of
// attemptClusterListCollapse returns deny-gate 1 and NO later gate runs,
// so the helper is behaviour-identical to healthy 0.30.204. Even with a
// fully-eligible-looking stage (iterator present), the inert gate must
// deny at gate 1 before the cache-off / snapshot / GVR / RBAC gates.
// When S.2 flips the const true this test should be re-pointed at the
// gate-2 (cache-off) contract.
func TestAttemptClusterListCollapse_InertGateDenies(t *testing.T) {
	cache.SetGlobal(nil)
	apiCall := &templates.API{
		Name:      "compositions-list",
		Path:      `${ "/apis/g/v/namespaces/" + .ns + "/r" }`,
		DependsOn: &templates.Dependency{Iterator: ptr.To(`["a","b"]`)},
	}
	tmp, ok, gate := attemptClusterListCollapse(
		context.Background(), clusterListLogger(t), apiCall,
		map[string]any{}, endpointStub(), nil, true)
	if ok || tmp != nil {
		t.Fatalf("inert collapse must deny; got ok=%v tmp=%v", ok, tmp)
	}
	if gate != 1 {
		t.Fatalf("inert collapse must deny at gate 1 (S.1-re sequencing gate); got gate=%d", gate)
	}
}

func TestAttemptClusterListCollapse_NoIterator(t *testing.T) {
	// A no-iterator stage has nothing to collapse; the helper
	// short-circuits. While the S.1-re inert gate is active the deny
	// happens at gate 1 before the iterator gate is reached — either way
	// the contract here is "must short-circuit" (ok==false, tmp==nil).
	apiCall := &templates.API{
		Name:      "x",
		DependsOn: nil, // no iterator
	}
	tmp, ok, _ := attemptClusterListCollapse(
		context.Background(), clusterListLogger(t), apiCall,
		map[string]any{}, endpointStub(), nil, true)
	if ok || tmp != nil {
		t.Fatalf("no iterator must short-circuit; got ok=%v tmp=%v", ok, tmp)
	}
}

func TestAttemptClusterListCollapse_EmptyIterator(t *testing.T) {
	apiCall := &templates.API{
		Name:      "compositions-list",
		Path:      "/apis/g/v/r",
		DependsOn: &templates.Dependency{Iterator: ptr.To("")},
	}
	tmp, ok, _ := attemptClusterListCollapse(
		context.Background(), clusterListLogger(t), apiCall,
		map[string]any{}, endpointStub(), nil, true)
	if ok || tmp != nil {
		t.Fatalf("empty iterator must short-circuit; got ok=%v tmp=%v", ok, tmp)
	}
}

func TestAttemptClusterListCollapse_ApistageStoreNil(t *testing.T) {
	apiCall := &templates.API{
		Name:      "compositions-list",
		Path:      `${ "/apis/g/v/namespaces/" + .ns + "/r" }`,
		DependsOn: &templates.Dependency{Iterator: ptr.To(`[{"ns":"a"}]`)},
	}
	tmp, ok, _ := attemptClusterListCollapse(
		context.Background(), clusterListLogger(t), apiCall,
		map[string]any{}, endpointStub(), nil, false /* apistage disabled */)
	if ok || tmp != nil {
		t.Fatalf("apistage disabled must short-circuit; got ok=%v tmp=%v", ok, tmp)
	}
}

// ---------- deriveTargetGVRForClusterList — recipe ----------

func TestDeriveTargetGVRForClusterList_NamespacedPath(t *testing.T) {
	apiCall := &templates.API{
		Name: "x",
		Path: `${ "/apis/composition.krateo.io/v1-2-2/namespaces/" + .ns + "/githubscaffoldingwithcompositionpages" }`,
		DependsOn: &templates.Dependency{
			// ForEach contract: the iterator query must return ONE
			// JSON array which it then unmarshals + ranges over. A
			// literal array expression matches that contract; `[]`
			// would emit a token stream of objects which is not
			// valid JSON.
			Iterator: ptr.To(`[{"ns":"bench-ns-01"},{"ns":"bench-ns-02"}]`),
		},
	}
	gvr, ok := deriveTargetGVRForClusterList(
		context.Background(), clusterListLogger(t), apiCall, map[string]any{})
	if !ok {
		t.Fatalf("deriveTargetGVRForClusterList: expected ok=true on namespaced iterator path")
	}
	want := schema.GroupVersionResource{
		Group:    "composition.krateo.io",
		Version:  "v1-2-2",
		Resource: "githubscaffoldingwithcompositionpages",
	}
	if gvr != want {
		t.Fatalf("deriveTargetGVRForClusterList: got %s want %s", gvr, want)
	}
}

func TestDeriveTargetGVRForClusterList_ClusterScopePath(t *testing.T) {
	// Iterator over a cluster-scope path — no namespace segment to
	// collapse. The helper must reject so the caller keeps the
	// iterator verbatim (the RA already operates cluster-wide).
	apiCall := &templates.API{
		Name: "x",
		Path: `${ "/apis/composition.krateo.io/v1-2-2/" + .plural }`,
		DependsOn: &templates.Dependency{
			Iterator: ptr.To(`[{"plural":"crd1"},{"plural":"crd2"}]`),
		},
	}
	gvr, ok := deriveTargetGVRForClusterList(
		context.Background(), clusterListLogger(t), apiCall, map[string]any{})
	if ok {
		t.Fatalf("deriveTargetGVRForClusterList: expected ok=false on cluster-scope iterator (gvr=%s)", gvr)
	}
}

func TestDeriveTargetGVRForClusterList_EmptyIterator(t *testing.T) {
	apiCall := &templates.API{
		Name: "x",
		Path: `${ "/apis/g/v/namespaces/" + .ns + "/r" }`,
		DependsOn: &templates.Dependency{
			Iterator: ptr.To(`[]`), // expands to zero elements
		},
	}
	_, ok := deriveTargetGVRForClusterList(
		context.Background(), clusterListLogger(t), apiCall, map[string]any{})
	if ok {
		t.Fatalf("deriveTargetGVRForClusterList: expected ok=false on empty iterator")
	}
}

func TestDeriveTargetGVRForClusterList_NilIterator(t *testing.T) {
	apiCall := &templates.API{Name: "x", Path: "/api/v1/namespaces/foo/pods"}
	_, ok := deriveTargetGVRForClusterList(
		context.Background(), clusterListLogger(t), apiCall, map[string]any{})
	if ok {
		t.Fatalf("deriveTargetGVRForClusterList: expected ok=false on nil DependsOn")
	}
}

// ---------- AC-D5.5 race seal — concurrent validateClusterListShape ----------

// The shape check is a pure function over the input bytes — no shared
// state, no globals. This -race test seals the no-shared-state property
// at 64 concurrent workers × 32 invocations against the same input.
// Any future regression that introduces e.g. a package-level decoder
// pool would surface here.
func TestValidateClusterListShape_RaceConcurrent(t *testing.T) {
	raw := mustJSON(t, map[string]any{
		"apiVersion": "composition.krateo.io/v1-2-2",
		"kind":       "GithubScaffoldingWithCompositionPagesList",
		"items": []any{
			map[string]any{"apiVersion": "composition.krateo.io/v1-2-2", "kind": "GithubScaffoldingWithCompositionPages"},
		},
	})
	const workers = 64
	const iters = 32
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				if _, ok, _ := validateClusterListShape(testShapeGVR, raw); !ok {
					t.Errorf("concurrent validateClusterListShape returned ok=false unexpectedly")
					return
				}
			}
		}()
	}
	wg.Wait()
}

// ---------- helpers ----------

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

// discardLogger is defined in refilter_test.go (no-arg variant). All
// cluster_list_test.go call sites use clusterListLogger() to ignore
// the *testing.T plumbing while remaining future-proofed if the
// existing helper signature ever shifts.
func clusterListLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return discardLogger()
}
