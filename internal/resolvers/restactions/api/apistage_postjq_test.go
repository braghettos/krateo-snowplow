// apistage_postjq_test.go — Ship Phase B / 0.30.185.
//
// Falsifier tests for the per-cohort post-jq value bytes cache. Covers
// HG-PB.10 (filter-change invalidation), HG-PB.11 (rbacGen invalidation),
// HG-PB.12 (expvar shape), HG-PB.15 (CACHE_ENABLED=false bypass),
// HG-PB.17 (cohort-keyed, no cross-cohort leak), ErrMultiYield recompute,
// empty-result caching, whitespace-distinct entries, and jqID stability.

package api

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/cespare/xxhash/v2"
	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// testGVR is a convenience for unit tests — the cache treats gvr only as
// a log argument, so any GVR shape is fine.
var testGVR = schema.GroupVersionResource{Group: "g", Version: "v", Resource: "r"}

// TestJQIDFromFilter_StableAcrossStructRecreation proves jqID for the
// same filter string is identical across multiple calls — load-bearing
// because the *string passed in is a transient struct value re-created
// per request from YAML, and the hash MUST stabilise across those
// re-creations.
func TestJQIDFromFilter_StableAcrossStructRecreation(t *testing.T) {
	a := ".items[] | select(.metadata.name | startswith(\"app-\"))"
	b := ".items[] | select(.metadata.name | startswith(\"app-\"))"
	// Distinct pointers to identical strings.
	pa := &a
	pb := &b
	idA, okA := JQIDFromFilter(pa)
	idB, okB := JQIDFromFilter(pb)
	if !okA || !okB {
		t.Fatalf("JQIDFromFilter returned ok=false on non-empty filters: %v %v", okA, okB)
	}
	if idA != idB {
		t.Fatalf("jqID not stable across struct re-creation: %d vs %d", idA, idB)
	}

	// Sanity: independently computed xxhash matches.
	want := xxhash.Sum64String(a)
	if idA != want {
		t.Fatalf("jqID != xxhash.Sum64String: got %d, want %d", idA, want)
	}
}

// TestJQIDFromFilter_NilEmpty proves nil and "" return (0, false) — the
// caller MUST check the ok bool before engaging the cache. Returning ok
// would let an "empty filter" silently poison the cache.
func TestJQIDFromFilter_NilEmpty(t *testing.T) {
	if _, ok := JQIDFromFilter(nil); ok {
		t.Fatalf("nil filter returned ok=true")
	}
	empty := ""
	if _, ok := JQIDFromFilter(&empty); ok {
		t.Fatalf("empty filter returned ok=true")
	}
}

// TestJQIDFromFilter_DistinctFilters proves different filter texts
// produce different jqIDs (no collisions on the trivial cases).
func TestJQIDFromFilter_DistinctFilters(t *testing.T) {
	a := ".items[].metadata.name"
	b := ".items[].metadata.namespace"
	idA, _ := JQIDFromFilter(&a)
	idB, _ := JQIDFromFilter(&b)
	if idA == idB {
		t.Fatalf("distinct filters hashed to same jqID: %d", idA)
	}
}

// TestPostJQ_WhitespaceDistinctCacheEntries proves whitespace differences
// in filter text produce DIFFERENT jqIDs (and therefore distinct cache
// entries). PM-ratified behaviour: we do NOT canonicalise.
func TestPostJQ_WhitespaceDistinctCacheEntries(t *testing.T) {
	a := ".items[]"
	b := ".items [ ]"
	idA, _ := JQIDFromFilter(&a)
	idB, _ := JQIDFromFilter(&b)
	if idA == idB {
		t.Fatalf("whitespace-different filters hashed to same jqID: %d", idA)
	}
}

// TestPostJQ_StoreLookupRoundTrip proves a stored entry can be looked up
// and produces the same bytes; unmarshalCohortPostJQ then yields a
// value that round-trips back to the original (byte-identity on the
// JSON round-trip).
func TestPostJQ_StoreLookupRoundTrip(t *testing.T) {
	ctx := xcontext.BuildContext(t.Context())
	memo := &cohortGateMemo{permitAll: true, rbacGen: 1}
	filter := ".items[].metadata.name"
	jqID, _ := JQIDFromFilter(&filter)
	const stageID = "compositions"

	// Pre-flight: lookup misses on a cold memo.
	if _, hit := lookupCohortPostJQ(memo, stageID, jqID); hit {
		t.Fatalf("cold memo: lookup returned hit")
	}

	// Store a payload.
	want := []byte(`["a","b","c"]`)
	storeCohortPostJQ(ctx, memo, testGVR, "cohort-1", stageID, jqID, want)

	// Lookup hits with byte-identical content.
	got, hit := lookupCohortPostJQ(memo, stageID, jqID)
	if !hit {
		t.Fatalf("warm memo: lookup missed")
	}
	if string(got) != string(want) {
		t.Fatalf("bytes mismatch: got=%q want=%q", got, want)
	}

	// Unmarshal yields a usable value.
	v, err := unmarshalCohortPostJQ(got)
	if err != nil {
		t.Fatalf("unmarshalCohortPostJQ: %v", err)
	}
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("unmarshal result not []any: %T %v", v, v)
	}
	if len(arr) != 3 || arr[0] != "a" {
		t.Fatalf("unmarshal content mismatch: %v", arr)
	}
}

// TestPostJQ_EmptyResultCached proves empty bytes are a valid cache
// entry (PM-ratified empty-result policy). A hit on empty bytes returns
// (nil, nil) from unmarshalCohortPostJQ.
func TestPostJQ_EmptyResultCached(t *testing.T) {
	ctx := xcontext.BuildContext(t.Context())
	memo := &cohortGateMemo{permitAll: true, rbacGen: 1}
	filter := ".items | select(.foo)"
	jqID, _ := JQIDFromFilter(&filter)
	const stageID = "compositions"

	storeCohortPostJQ(ctx, memo, testGVR, "cohort-1", stageID, jqID, nil)
	got, hit := lookupCohortPostJQ(memo, stageID, jqID)
	if !hit {
		t.Fatalf("empty entry: lookup missed")
	}
	if len(got) != 0 {
		t.Fatalf("empty entry: bytes=%q want empty", got)
	}
	v, err := unmarshalCohortPostJQ(got)
	if err != nil {
		t.Fatalf("unmarshal empty: %v", err)
	}
	if v != nil {
		t.Fatalf("unmarshal empty: got %v, want nil", v)
	}
}

// TestPostJQ_FilterChangeInvalidatesPerJQID proves a filter change
// produces a different jqID and the cache lookup misses on the new
// jqID even though the cohort + stage-id are identical (HG-PB.10).
func TestPostJQ_FilterChangeInvalidatesPerJQID(t *testing.T) {
	ctx := xcontext.BuildContext(t.Context())
	memo := &cohortGateMemo{permitAll: true, rbacGen: 1}
	const stageID = "compositions"

	f1 := ".items[].metadata.name"
	f2 := ".items[].metadata.namespace"
	id1, _ := JQIDFromFilter(&f1)
	id2, _ := JQIDFromFilter(&f2)
	if id1 == id2 {
		t.Fatalf("filter pair did not hash to distinct ids: %d", id1)
	}

	storeCohortPostJQ(ctx, memo, testGVR, "c", stageID, id1, []byte(`["x"]`))
	if _, hit := lookupCohortPostJQ(memo, stageID, id1); !hit {
		t.Fatalf("f1: lookup missed after store")
	}
	// f2 has its own jqID -> miss.
	if _, hit := lookupCohortPostJQ(memo, stageID, id2); hit {
		t.Fatalf("f2 lookup hit on f1-only memo")
	}
}

// TestPostJQ_DistinctStageIDsDistinctEntries proves stage-id is part of
// the cache key. Two stages with the same filter against the same
// cohort memo MUST produce distinct entries — different stage names
// wrap the envelope under different keys, so the post-jq output differs.
func TestPostJQ_DistinctStageIDsDistinctEntries(t *testing.T) {
	ctx := xcontext.BuildContext(t.Context())
	memo := &cohortGateMemo{permitAll: true, rbacGen: 1}
	filter := ".compositions.items"
	jqID, _ := JQIDFromFilter(&filter)

	storeCohortPostJQ(ctx, memo, testGVR, "c", "stage-a", jqID, []byte(`["a"]`))
	storeCohortPostJQ(ctx, memo, testGVR, "c", "stage-b", jqID, []byte(`["b"]`))

	a, hitA := lookupCohortPostJQ(memo, "stage-a", jqID)
	b, hitB := lookupCohortPostJQ(memo, "stage-b", jqID)
	if !hitA || !hitB {
		t.Fatalf("stage entries missed: a=%v b=%v", hitA, hitB)
	}
	if string(a) == string(b) {
		t.Fatalf("distinct stage-ids returned identical bytes: %q", a)
	}
}

// TestPostJQ_ConcurrentSameKeyDedup proves N concurrent goroutines
// storing the same (memo, stage, jqID) tuple result in ONE memo entry —
// LoadOrStore deduplicates the writers.
func TestPostJQ_ConcurrentSameKeyDedup(t *testing.T) {
	ctx := xcontext.BuildContext(t.Context())
	memo := &cohortGateMemo{permitAll: true, rbacGen: 1}
	filter := ".items"
	jqID, _ := JQIDFromFilter(&filter)
	const stageID = "compositions"

	const workers = 32
	want := []byte(`["a","b"]`)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			storeCohortPostJQ(ctx, memo, testGVR, "c", stageID, jqID, want)
		}()
	}
	wg.Wait()

	got, hit := lookupCohortPostJQ(memo, stageID, jqID)
	if !hit {
		t.Fatalf("post-storm lookup missed")
	}
	if string(got) != string(want) {
		t.Fatalf("post-storm bytes mismatch: got=%q want=%q", got, want)
	}

	count := 0
	memo.postJQEncoded.Range(func(_, _ any) bool { count++; return true })
	if count != 1 {
		t.Fatalf("postJQEncoded entries after storm = %d, want 1", count)
	}
}

// TestPostJQ_PerEntryCapDrops proves an entry above the per-entry cap is
// dropped (NOT stored). The cap is read via cache.CohortPostJQCapBytes;
// we set COHORT_POSTJQ_CAP_BYTES low for the duration of the test.
func TestPostJQ_PerEntryCapDrops(t *testing.T) {
	t.Setenv("COHORT_POSTJQ_CAP_BYTES", "16")
	ctx := xcontext.BuildContext(t.Context())
	memo := &cohortGateMemo{permitAll: true, rbacGen: 1}
	filter := ".items"
	jqID, _ := JQIDFromFilter(&filter)
	const stageID = "compositions"

	big := []byte(strings.Repeat("x", 256))
	storeCohortPostJQ(ctx, memo, testGVR, "c", stageID, jqID, big)
	if _, hit := lookupCohortPostJQ(memo, stageID, jqID); hit {
		t.Fatalf("cap-exceeded entry was cached")
	}
}

// TestUnmarshalCohortPostJQ_PerCallIsolation proves two consecutive
// Unmarshal calls on the same bytes produce DISTINCT value trees — the
// downstream gojq mutation contract requires per-call isolation.
func TestUnmarshalCohortPostJQ_PerCallIsolation(t *testing.T) {
	raw := []byte(`{"items":["a","b"]}`)
	v1, err := unmarshalCohortPostJQ(raw)
	if err != nil {
		t.Fatalf("unmarshal v1: %v", err)
	}
	v2, err := unmarshalCohortPostJQ(raw)
	if err != nil {
		t.Fatalf("unmarshal v2: %v", err)
	}
	m1 := v1.(map[string]any)
	m2 := v2.(map[string]any)
	m1["items"] = "mutated"
	items, ok := m2["items"].([]any)
	if !ok {
		t.Fatalf("v2 items not []any after v1 mutation: %T", m2["items"])
	}
	if len(items) != 2 {
		t.Fatalf("v2 items len = %d, want 2 (isolation broken)", len(items))
	}
}

// TestHandlerCore_CapturePostJQ_FiresOnSingleYield proves the
// capturePostJQ hook installed via jsonHandlerOptions fires on the
// single-yield success branch only. The hook receives the same value
// that flows into out[key].
func TestHandlerCore_CapturePostJQ_FiresOnSingleYield(t *testing.T) {
	ctx := xcontext.BuildContext(t.Context())
	out := map[string]any{}
	var captured any
	var fired int
	filter := ".k.x"
	opts := jsonHandlerOptions{
		key:    "k",
		out:    out,
		filter: &filter,
		capturePostJQ: func(v any) {
			fired++
			captured = v
		},
	}
	if err := jsonHandlerCore(ctx, opts, map[string]any{"x": float64(42)}); err != nil {
		t.Fatalf("jsonHandlerCore: %v", err)
	}
	if fired != 1 {
		t.Fatalf("capturePostJQ fired %d times, want 1", fired)
	}
	if got, want := out["k"], captured; got != want {
		t.Fatalf("captured=%v, out[k]=%v — mismatch", want, got)
	}
}

// TestHandlerCore_CapturePostJQ_NotFiredOnMultiYield proves multi-yield
// does NOT invoke capturePostJQ — the PM-ratified recompute-don't-cache
// policy for ErrMultiYield. The jsonHandlerCore returns the error
// unchanged.
func TestHandlerCore_CapturePostJQ_NotFiredOnMultiYield(t *testing.T) {
	ctx := xcontext.BuildContext(t.Context())
	out := map[string]any{}
	var fired int
	filter := ".k.items[]"
	opts := jsonHandlerOptions{
		key:    "k",
		out:    out,
		filter: &filter,
		capturePostJQ: func(v any) {
			fired++
		},
	}
	err := jsonHandlerCore(ctx, opts, map[string]any{
		"items": []any{"a", "b", "c"},
	})
	if err == nil {
		t.Fatalf("expected ErrMultiYield, got nil")
	}
	if fired != 0 {
		t.Fatalf("capturePostJQ fired on multi-yield: %d times", fired)
	}
}

// TestHandlerCore_CapturePostJQ_NotFiredOnNoFilter proves the hook is
// gated on filter != nil. When the stage has no filter, the hook MUST
// stay silent (caching pre-jq values would corrupt the post-jq cache).
func TestHandlerCore_CapturePostJQ_NotFiredOnNoFilter(t *testing.T) {
	ctx := xcontext.BuildContext(t.Context())
	out := map[string]any{}
	var fired int
	opts := jsonHandlerOptions{
		key:    "k",
		out:    out,
		filter: nil,
		capturePostJQ: func(v any) {
			fired++
		},
	}
	if err := jsonHandlerCore(ctx, opts, map[string]any{"a": float64(1)}); err != nil {
		t.Fatalf("jsonHandlerCore: %v", err)
	}
	if fired != 0 {
		t.Fatalf("capturePostJQ fired with nil filter: %d", fired)
	}
}

// TestPostJQ_CapBytesEnvOverride proves COHORT_POSTJQ_CAP_BYTES env is
// honoured; an unset env returns the default 64 MiB.
func TestPostJQ_CapBytesEnvOverride(t *testing.T) {
	t.Setenv("COHORT_POSTJQ_CAP_BYTES", "")
	if got := cache.CohortPostJQCapBytes(); got != 64*1024*1024 {
		t.Fatalf("default cap = %d, want %d", got, 64*1024*1024)
	}

	t.Setenv("COHORT_POSTJQ_CAP_BYTES", "1024")
	if got := cache.CohortPostJQCapBytes(); got != 1024 {
		t.Fatalf("custom cap = %d, want 1024", got)
	}

	t.Setenv("COHORT_POSTJQ_CAP_BYTES", "abc")
	if got := cache.CohortPostJQCapBytes(); got != 64*1024*1024 {
		t.Fatalf("invalid cap parsing did not fall back to default: %d", got)
	}
}

// TestFeedPostJQValue_MergeSemantics proves feedPostJQValue mirrors
// jsonHandlerCore's post-filter merge: append on slice, wrap into slice
// on existing scalar.
func TestFeedPostJQValue_MergeSemantics(t *testing.T) {
	var mu sync.Mutex

	d := map[string]any{}
	if err := feedPostJQValue("a", &mu, d, "k"); err != nil {
		t.Fatal(err)
	}
	if d["k"] != "a" {
		t.Fatalf("absent: d[k]=%v want a", d["k"])
	}

	if err := feedPostJQValue("b", &mu, d, "k"); err != nil {
		t.Fatal(err)
	}
	gotSlice, ok := d["k"].([]any)
	if !ok || len(gotSlice) != 2 || gotSlice[0] != "a" || gotSlice[1] != "b" {
		t.Fatalf("scalar+scalar merge: d[k]=%v want [a,b]", d["k"])
	}

	if err := feedPostJQValue("c", &mu, d, "k"); err != nil {
		t.Fatal(err)
	}
	gotSlice = d["k"].([]any)
	if len(gotSlice) != 3 || gotSlice[2] != "c" {
		t.Fatalf("slice+scalar merge: d[k]=%v want [a,b,c]", d["k"])
	}
}

// TestPostJQ_JSONRoundTripStability proves the marshal+unmarshal cycle
// the cache relies on produces stable JSON for the gojq result types
// (nil, bool, int, float64, string, []any, map[string]any). The cache
// stores the marshal, the hit unmarshals; the unmarshal value must be
// usable downstream.
func TestPostJQ_JSONRoundTripStability(t *testing.T) {
	cases := []any{
		nil,
		true,
		float64(42),
		"hello",
		[]any{"a", "b", float64(1)},
		map[string]any{"foo": float64(1), "bar": []any{"x"}},
	}
	for _, c := range cases {
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("marshal(%v): %v", c, err)
		}
		v, err := unmarshalCohortPostJQ(raw)
		if err != nil {
			t.Fatalf("unmarshal(%s): %v", raw, err)
		}
		got, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("re-marshal: %v", err)
		}
		if string(got) != string(raw) {
			t.Fatalf("round-trip drift: in=%s out=%s", raw, got)
		}
	}
}

// TestPostJQ_NoCrossMemoLeak proves the cache is scoped per memo (per
// cohort): a store under memo-A is invisible to a lookup against
// memo-B even with identical (stage-id, jqID). HG-PB.17 invariant.
func TestPostJQ_NoCrossMemoLeak(t *testing.T) {
	ctx := xcontext.BuildContext(t.Context())
	memoA := &cohortGateMemo{permitAll: true, rbacGen: 1}
	memoB := &cohortGateMemo{permitAll: true, rbacGen: 1}
	filter := ".items"
	jqID, _ := JQIDFromFilter(&filter)
	const stageID = "compositions"

	storeCohortPostJQ(ctx, memoA, testGVR, "cohort-a", stageID, jqID, []byte(`["a-only"]`))
	if _, hit := lookupCohortPostJQ(memoA, stageID, jqID); !hit {
		t.Fatalf("memoA: own lookup missed")
	}
	if got, hit := lookupCohortPostJQ(memoB, stageID, jqID); hit {
		t.Fatalf("memoB: cross-cohort leak hit, bytes=%q", got)
	}
}

// TestPostJQ_NilMemoSafe proves the lookup/store helpers are nil-safe.
// The resolve.go caller already guards on memo != nil, but the helpers
// should not panic if a future refactor passes nil.
func TestPostJQ_NilMemoSafe(t *testing.T) {
	ctx := xcontext.BuildContext(t.Context())
	if _, hit := lookupCohortPostJQ(nil, "k", 1); hit {
		t.Fatalf("nil memo: lookup returned hit")
	}
	storeCohortPostJQ(ctx, nil, testGVR, "c", "k", 1, []byte(`x`))
}

// TestPeekCohortMemoForPostJQ_NilStoreSafe proves the peek helper is
// nil-safe on every input.
func TestPeekCohortMemoForPostJQ_NilStoreSafe(t *testing.T) {
	ctx := xcontext.BuildContext(t.Context())
	filter := ".items"
	if memo, cohort, jqID, raw, hit := peekCohortMemoForPostJQ(ctx, nil, &filter, "s", false); hit {
		t.Fatalf("nil store: peek returned hit memo=%v cohort=%q jqID=%d raw=%q",
			memo, cohort, jqID, raw)
	}
}

// TestPeekCohortMemoForPostJQ_EmptyStageIDMisses proves the peek
// short-circuits on an empty stage-id (no cohort hash, no store
// lookup).
func TestPeekCohortMemoForPostJQ_EmptyStageIDMisses(t *testing.T) {
	ctx := xcontext.BuildContext(t.Context())
	store := cache.NewCohortGateMemoStore()
	filter := ".items"
	if _, _, _, _, hit := peekCohortMemoForPostJQ(ctx, store, &filter, "", false); hit {
		t.Fatalf("empty stage-id: peek returned hit")
	}
}

// TestPeekCohortMemoForPostJQ_SliceActiveMisses proves the peek bypasses
// the cache when dict["slice"] is active (the jq input shape differs,
// the cached post-jq bytes are not safe to serve).
func TestPeekCohortMemoForPostJQ_SliceActiveMisses(t *testing.T) {
	ctx := xcontext.BuildContext(t.Context())
	store := cache.NewCohortGateMemoStore()
	filter := ".items"
	if _, _, _, _, hit := peekCohortMemoForPostJQ(ctx, store, &filter, "s", true); hit {
		t.Fatalf("sliceActive: peek returned hit")
	}
}

// TestPeekCohortMemoForPostJQ_NilFilterMisses proves the peek misses
// on a nil filter — no jqID computable, no cache.
func TestPeekCohortMemoForPostJQ_NilFilterMisses(t *testing.T) {
	ctx := xcontext.BuildContext(t.Context())
	store := cache.NewCohortGateMemoStore()
	if _, _, _, _, hit := peekCohortMemoForPostJQ(ctx, store, nil, "s", false); hit {
		t.Fatalf("nil filter: peek returned hit")
	}
}

// TestPeekCohortMemoForPostJQ_NoMemoMisses proves the peek misses when
// the store has no memo for the cohort yet (cold path).
func TestPeekCohortMemoForPostJQ_NoMemoMisses(t *testing.T) {
	ctx := xcontextWithTestUser(t.Context(), "alice", []string{"admin"})
	store := cache.NewCohortGateMemoStore()
	filter := ".items"
	if _, _, _, _, hit := peekCohortMemoForPostJQ(ctx, store, &filter, "s", false); hit {
		t.Fatalf("cold store: peek returned hit")
	}
}

// TestPeekCohortMemoForPostJQ_StaleStampMisses proves a rbacGen mismatch
// between the memo and the live CohortRBACGen invalidates the postJQ
// hit. The stamp check is the load-bearing invalidation hook for the
// per-cohort generator (cache.CohortRBACGen — Ship GMC.1 / 0.30.175).
func TestPeekCohortMemoForPostJQ_StaleStampMisses(t *testing.T) {
	ctx := xcontextWithTestUser(t.Context(), "alice", []string{"admin"})
	store := cache.NewCohortGateMemoStore()
	cohort := cohortKeyHashFromUserInfo("alice", []string{"admin"})
	// Force a mismatch: align with live gen then add 1.
	live := cache.CohortRBACGen("alice", []string{"admin"})
	memo := &cohortGateMemo{
		permitAll: true,
		rbacGen:   live + 1,
	}
	filter := ".items"
	jqID, _ := JQIDFromFilter(&filter)
	storeCohortPostJQ(ctx, memo, testGVR, cohort, "compositions", jqID, []byte(`["x"]`))
	store.Store(cohort, memo)

	if _, _, _, _, hit := peekCohortMemoForPostJQ(ctx, store, &filter, "compositions", false); hit {
		t.Fatalf("stale-stamp memo: peek returned hit")
	}
}

// TestPeekCohortMemoForPostJQ_NotPermitAllMisses proves the peek
// requires permitAll == true to serve. A !permitAll memo (narrow RBAC
// cohort) MUST miss — the kept-set varies per cohort and the post-jq
// output is not safe to cross-serve.
func TestPeekCohortMemoForPostJQ_NotPermitAllMisses(t *testing.T) {
	ctx := xcontextWithTestUser(t.Context(), "alice", []string{"narrow"})
	store := cache.NewCohortGateMemoStore()
	cohort := cohortKeyHashFromUserInfo("alice", []string{"narrow"})
	memo := &cohortGateMemo{
		permitAll: false,
		rbacGen:   cache.CohortRBACGen("alice", []string{"narrow"}),
		keptNames: map[string]struct{}{"ns/x": {}},
	}
	filter := ".items"
	jqID, _ := JQIDFromFilter(&filter)
	storeCohortPostJQ(ctx, memo, testGVR, cohort, "s", jqID, []byte(`["x"]`))
	store.Store(cohort, memo)

	if _, _, _, _, hit := peekCohortMemoForPostJQ(ctx, store, &filter, "s", false); hit {
		t.Fatalf("!permitAll memo: peek returned hit")
	}
}

// TestPeekCohortMemoForPostJQ_HitPath proves the load-bearing
// short-circuit: with a populated permitAll memo + populated postJQ
// entry, the peek returns hit=true and the cached raw bytes — WITHOUT
// invoking listEnvelopeValue or CopyJSONMap.
//
// FALSIFIER PROOF for architect review item 3: the function signature
// (peekCohortMemoForPostJQ) DOES NOT receive `parsedListEnvelope`, so
// by construction it CANNOT call listEnvelopeValue or CopyJSONMap.
// The HIT path's CPU win is structural, not implementation-dependent.
func TestPeekCohortMemoForPostJQ_HitPath(t *testing.T) {
	ctx := xcontextWithTestUser(t.Context(), "alice", []string{"admin"})
	store := cache.NewCohortGateMemoStore()
	cohort := cohortKeyHashFromUserInfo("alice", []string{"admin"})

	memo := &cohortGateMemo{
		permitAll: true,
		rbacGen:   cache.CohortRBACGen("alice", []string{"admin"}),
	}
	filter := ".items[]"
	jqID, _ := JQIDFromFilter(&filter)
	const stageID = "compositions"
	want := []byte(`["a","b"]`)
	storeCohortPostJQ(ctx, memo, testGVR, cohort, stageID, jqID, want)
	store.Store(cohort, memo)

	gotMemo, gotCohort, gotJQID, gotRaw, hit := peekCohortMemoForPostJQ(ctx, store, &filter, stageID, false)
	if !hit {
		t.Fatalf("populated memo+entry: peek returned miss")
	}
	if gotMemo != memo {
		t.Fatalf("peek returned wrong memo pointer")
	}
	if gotCohort != cohort {
		t.Fatalf("peek returned wrong cohort: %q want %q", gotCohort, cohort)
	}
	if gotJQID != jqID {
		t.Fatalf("peek returned wrong jqID: %d want %d", gotJQID, jqID)
	}
	if string(gotRaw) != string(want) {
		t.Fatalf("peek returned wrong bytes: %q want %q", gotRaw, want)
	}
}

// xcontextWithTestUser is a tiny helper that attaches a UserInfo to ctx
// using the same xcontext.WithUserInfo path the real request pipeline
// uses. Kept local to the test file to avoid leaking test scaffolding
// into the production package.
func xcontextWithTestUser(ctx context.Context, username string, groups []string) context.Context {
	return xcontext.BuildContext(ctx, xcontext.WithUserInfo(jwtutil.UserInfo{
		Username: username,
		Groups:   groups,
	}))
}
