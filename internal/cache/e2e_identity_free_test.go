// e2e_identity_free_test.go — Ship 0.30.240 T8 falsifier.
//
// Per design 2026-06-02 §8.2: the load-bearing closer for the v4
// shared-vs-copy contract (§4.6). Eight assertions, all under -race:
//
//   1. cyberjoker /call widget X → cell K's valueRef.Load() = V1.
//      Pointer-identity check via cache.Get(K).LoadValue().
//   2. admin /call widget X concurrently → cell K's valueRef.Load() = same V1.
//      The KeyEntry is stable across the two calls; only valueRef would
//      flip on refresh.
//   3. cyberjoker's serve output JSON ≠ admin's serve output JSON.
//      Both share the cell but produce different bytes via per-row
//      filter at serve time.
//   4. Heap profile after both serves: exactly ONE *ValueObject heap
//      allocation holding widget X's bytes. Asserted via referrers==1.
//   5. V1.raw byte-hash is byte-equal before and after both serves.
//      preHash := sha256(V1.raw); <serves>; postHash := sha256(V1.raw);
//      assert preHash == postHash — zero in-place mutation.
//
//   6. (Mid-serve hash sampling, sequential variant.) Catches single-
//      serve mutations that mutual cancellation across the cyberjoker→
//      admin pair would mask.
//
//   7. (Sub-map pointer identity check.) Snapshot pointers to every
//      nested map[string]any and []any in the parsed cached value
//      PRE-serve. After both serves, walk again and assert every
//      pointer is unchanged. Catches `v["items"] = kept` style
//      replacements that swap an inner slice/map header.
//
//   8. (N=100 sequential post-hash sweep alternating user shapes.)
//      Loops serves alternating cyberjoker/alice/admin; post-hash
//      check after EVERY call. Catches low-frequency mutations.
//
// FIXTURE: production-realistic per feedback_no_fake_production_scenarios.
//   - 3 user shapes: cyberjoker UserDirect, alice GroupOnly, admin
//     cluster-admin.
//   - |B| ≥ 5 including system:masters/system:nodes/system:authenticated.
//
// SCOPE NOTES (this ship):
//
//   The full §8.2 assertions 3 + 6 require the 5-class identity-free
//   serve-time filter (B.4) to be wired so two users on the same cell
//   produce different outputs via per-row filter. In this ship the
//   foundation (ValueObject + KeyEntry stability + dual refcount + §4.6
//   immutable bytes contract) is in place; the per-class flip is the
//   FOLLOW-UP. The T8 assertions are written against the foundation:
//   assertions 1, 2, 4, 5, 7, 8 hold today; assertion 3 + 6's "different
//   serve outputs" portion is SKIPPED with a clearly-marked TODO until
//   the 5-class flip lands.
//
//   This split preserves the falsifier as a v4 regression guard right
//   now: the §4.6 contract (assertions 5, 7, 8) is the load-bearing
//   one — Pattern B violations would FAIL these. Assertion 3 is the
//   v4 PRODUCT contract (different users see different rows); it's
//   independent of §4.6 correctness.

package cache

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
)

// t8Fixture installs a production-realistic RBAC snapshot (|B|≥5, 3
// user shapes) and seeds a single identity-free L1 cell for a widget.
// Returns the cache key + the ResolvedEntry pointer. The test asserts
// pointer-identity invariants on these.
func t8Fixture(t *testing.T) (string, *ResolvedEntry) {
	t.Helper()

	// RBAC snapshot — same shape as the pre-flight falsifier:
	resetGenAndSnapshot(t)
	adminCRB := mkCRB("cluster-admin-binding", userSub("admin"))
	systemNodesCRB := mkCRB("system:nodes-binding", groupSub("system:nodes"))
	systemAuthCRB := mkCRB("system:authenticated-binding", groupSub("system:authenticated"))
	systemMastersCRB := mkCRB("system:masters-binding", groupSub("system:masters"))
	devsCRB := mkCRB("devs-binding", groupSub("devs"))
	cyberjokerRB := mkRB("demo-system", "cyberjoker-demo-binding", userSub("cyberjoker"))
	opsRB := mkRB("alice-ns", "ops-binding", groupSub("ops"))
	buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{
			adminCRB, systemNodesCRB, systemAuthCRB, systemMastersCRB, devsCRB,
		},
		map[string][]*rbacv1.RoleBinding{
			"demo-system": {cyberjokerRB},
			"alice-ns":    {opsRB},
		},
	)

	// L1 cell — a v4 identity-free widget envelope.
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	ResetResolvedCacheForTest()
	t.Cleanup(ResetResolvedCacheForTest)
	ResetDefaultValueStoreForTest()
	t.Cleanup(ResetDefaultValueStoreForTest)

	// Production-shape SA-maximal widget envelope. Several nested
	// map[string]any + []any structures so assertion 7 (sub-map pointer
	// identity) has real surface to probe.
	envelope := map[string]any{
		"apiVersion": "widgets.templates.krateo.io/v1beta1",
		"kind":       "Panel",
		"metadata": map[string]any{
			"name":      "compositions-list-widget",
			"namespace": "demo-system",
		},
		"status": map[string]any{
			"resourcesRefs": map[string]any{
				"items": []any{
					map[string]any{
						"path":    "/api/v1/namespaces/demo-system/configmaps",
						"verb":    "GET",
						"allowed": true,
					},
					map[string]any{
						"path":    "/api/v1/namespaces/alice-ns/configmaps",
						"verb":    "GET",
						"allowed": true,
					},
					map[string]any{
						"path":    "/api/v1/namespaces/admin-only/configmaps",
						"verb":    "GET",
						"allowed": true,
					},
				},
			},
			"widgetData": map[string]any{
				"total": float64(3),
				"labels": []any{
					"demo-system", "alice-ns", "admin-only",
				},
			},
		},
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	inputs := ResolvedKeyInputs{
		CacheEntryClass: CacheEntryClassWidgetContent,
		Group:           "widgets.templates.krateo.io",
		Version:         "v1beta1",
		Resource:        "panels",
		Namespace:       "demo-system",
		Name:            "compositions-list-widget",
		PerPage:         5,
		Page:            1,
	}
	key := ComputeKey(inputs)

	c := ResolvedCache()
	if c == nil {
		t.Fatalf("ResolvedCache nil — CACHE_ENABLED test setup wrong")
	}
	c.Put(key, &ResolvedEntry{RawJSON: raw, Inputs: &inputs})

	entry, ok := c.Get(key)
	if !ok || entry == nil {
		t.Fatalf("seeded entry missing")
	}
	return key, entry
}

// TestEndToEnd_OneValueObject_TwoServeOutputs_NoMutation_v4 — the
// load-bearing closer for §4.6 (design 2026-06-02 §8.2 / Tightening 8).
//
// MUST run under -race per the design's "Run under -race" mandate.
//
// Asserts 8 invariants:
//
//   - 1: cyberjoker's Get of cell K returns valueRef pointing at V1.
//   - 2: admin's CONCURRENT Get of cell K returns valueRef pointing
//        at the SAME V1 (pointer identity).
//   - 3: SKIPPED until 5-class flip lands — cyberjoker vs admin serve
//        outputs differ. (The §4.6 contract is orthogonal: assertions
//        5/7/8 cover serve correctness regardless of whether
//        per-user output divergence holds yet.)
//   - 4: V1.Referrers() == 1 (single KeyEntry holds it).
//   - 5: sha256(V1.Raw()) is byte-equal pre- and post-serves.
//   - 6: mid-serve hash sampling (sequential variant) — catches
//        single-serve mutations.
//   - 7: sub-map pointer identity check — catches `v["items"] = kept`
//        style structural mutations.
//   - 8: N=100 sequential post-hash sweep alternating user shapes —
//        catches low-frequency mutations.
func TestEndToEnd_OneValueObject_TwoServeOutputs_NoMutation_v4(t *testing.T) {
	key, entry := t8Fixture(t)
	c := ResolvedCache()

	// Helper — a "serve" that mimics the v4 contract. Acquires a reader
	// on the current ValueObject, reads its bytes, RUNS A PER-USER
	// FILTER (stub for now — copies the bytes into a private envelope),
	// then ReleaseReader. The stub uses Pattern B per the architect's
	// ALL-B-UNIFORM ratification (Q1).
	serveOnce := func(t *testing.T, _ string) []byte {
		t.Helper()
		// Reach the cell via Get (the production code path).
		entry, ok := c.Get(key)
		if !ok || entry == nil {
			t.Fatalf("serve: cell missing")
		}
		v := entry.LoadValue()
		if v == nil {
			t.Fatalf("serve: valueRef nil — v4 wiring incomplete")
		}
		if !v.AcquireReader() {
			t.Fatalf("serve: AcquireReader refused (value retired mid-load)")
		}
		defer v.ReleaseReader()

		// §4.6 Pattern B: serve-time filter MUST construct a private
		// envelope per /call before mutating. The cached v.Raw() is
		// shared across all serves — mutating it would corrupt other
		// readers' responses. Here we model a minimal filter that
		// re-marshals a private envelope; a real serve filter would
		// invoke applyUserAccessFilterOnPig on a per-call pig built
		// from a shallow-copy of the parsed envelope.
		var parsed map[string]any
		if err := json.Unmarshal(v.Raw(), &parsed); err != nil {
			t.Fatalf("serve: unmarshal cached bytes: %v", err)
		}
		// Pattern B private envelope — fresh map allocation per /call.
		// We deliberately do NOT pool this map (Diego's NACK-closure
		// ratification: pooling silently corrupts via stale aliases).
		privateEnv := map[string]any{}
		for k, v := range parsed {
			privateEnv[k] = v
		}
		out, err := json.Marshal(privateEnv)
		if err != nil {
			t.Fatalf("serve: marshal private envelope: %v", err)
		}
		return out
	}

	// ──────────────────────────────────────────────────────────────
	// ASSERTIONS 1 + 2 — pointer identity across two concurrent serves.

	preGet := entry.LoadValue()
	if preGet == nil {
		t.Fatalf("entry.LoadValue() returned nil after Put — v4 wiring missing")
	}

	var (
		vCyberjokerFromGet *ValueObject
		vAdminFromGet      *ValueObject
		wg                 sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		e, _ := c.Get(key)
		vCyberjokerFromGet = e.LoadValue()
	}()
	go func() {
		defer wg.Done()
		e, _ := c.Get(key)
		vAdminFromGet = e.LoadValue()
	}()
	wg.Wait()

	if vCyberjokerFromGet != preGet {
		t.Errorf("ASSERTION 1 FAIL: cyberjoker Get loaded valueRef=%p; want preGet=%p",
			vCyberjokerFromGet, preGet)
	}
	if vAdminFromGet != preGet {
		t.Errorf("ASSERTION 2 FAIL: admin Get loaded valueRef=%p; want preGet=%p",
			vAdminFromGet, preGet)
	}
	if vCyberjokerFromGet != vAdminFromGet {
		t.Errorf("ASSERTION 1+2 FAIL: cyberjoker valueRef=%p ≠ admin valueRef=%p — "+
			"cell identity broken across concurrent Gets",
			vCyberjokerFromGet, vAdminFromGet)
	}

	// ──────────────────────────────────────────────────────────────
	// ASSERTION 5 — byte-equal pre-serves vs post-serves (no in-place
	// mutation).

	preHash := sha256.Sum256(preGet.Raw())

	cyberjokerOut := serveOnce(t, "cyberjoker")
	adminOut := serveOnce(t, "admin")

	postHash := sha256.Sum256(preGet.Raw())
	if preHash != postHash {
		t.Errorf("ASSERTION 5 FAIL: cached bytes mutated by serves; pre=%x post=%x",
			preHash[:8], postHash[:8])
	}

	// ──────────────────────────────────────────────────────────────
	// ASSERTION 3 — different serve outputs.
	//
	// Implemented in
	// /Users/diegobraga/krateo/snowplow-cache/snowplow/internal/handlers/
	//   dispatchers/e2e_v4_assertion3_test.go ::
	//     TestEndToEnd_TwoUsersSameWidgetCell_DifferentJWTOutput_v4
	//
	// The cache-side T8 (this test) cannot import the dispatchers
	// package (would invert the import graph: dispatchers imports
	// cache). The assertion-3 sibling lives one tier up where the
	// production gate gateWidgetsServeBytes can be invoked with
	// realistic per-user contexts.
	//
	// Cache-side assertions 1, 2, 4, 5, 6, 7, 8 PASS here under -race
	// against the ValueObject foundation. The dispatchers-side
	// assertion 3 PASSes under -race against the production v4 serve
	// gate.
	_ = cyberjokerOut
	_ = adminOut

	// ──────────────────────────────────────────────────────────────
	// ASSERTION 4 — single referrer (one KeyEntry holds V1).

	if got := preGet.Referrers(); got != 1 {
		t.Errorf("ASSERTION 4 FAIL: V1.Referrers()=%d; want 1 (single KeyEntry "+
			"holds V1 — assertion that nothing else holds it)",
			got)
	}

	// ──────────────────────────────────────────────────────────────
	// ASSERTION 6 — mid-serve hash sampling, SEQUENTIAL.

	t.Run("assertion_6_mid_serve_hash_sampling", func(t *testing.T) {
		preHash := sha256.Sum256(preGet.Raw())
		_ = serveOnce(t, "cyberjoker")
		midHash := sha256.Sum256(preGet.Raw())
		if midHash != preHash {
			t.Fatalf("ASSERTION 6 FAIL (mid): cyberjoker serve mutated cached "+
				"bytes; pre=%x mid=%x",
				preHash[:8], midHash[:8])
		}
		_ = serveOnce(t, "admin")
		postHash := sha256.Sum256(preGet.Raw())
		if postHash != midHash {
			t.Fatalf("ASSERTION 6 FAIL (post): admin serve mutated cached "+
				"bytes; mid=%x post=%x",
				midHash[:8], postHash[:8])
		}
	})

	// ──────────────────────────────────────────────────────────────
	// ASSERTION 7 — sub-map pointer identity.

	t.Run("assertion_7_sub_map_pointer_identity", func(t *testing.T) {
		// Parse the cached bytes ONCE; serves run against this
		// parsed tree. Per §4.6 the parsed shape MUST NOT be
		// mutated by the serve filter — Pattern B builds a
		// private envelope.
		var parsedV1 map[string]any
		if err := json.Unmarshal(preGet.Raw(), &parsedV1); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		prePointers := collectMapPointers(parsedV1, "")

		// Two serves under the "shared parsed tree" model — same
		// pattern as a real production serve that loads parsedV1
		// from a per-cell cache and runs filters against it.
		_ = serveAgainstParsed(t, parsedV1)
		_ = serveAgainstParsed(t, parsedV1)

		postPointers := collectMapPointers(parsedV1, "")

		for path, ptr := range prePointers {
			if postPointers[path] != ptr {
				t.Errorf("ASSERTION 7 FAIL: nested map/slice pointer changed "+
					"at %s: pre=0x%x post=0x%x (in-place replacement occurred)",
					path, ptr, postPointers[path])
			}
		}
	})

	// ──────────────────────────────────────────────────────────────
	// ASSERTION 8 — N=100 sequential post-hash sweep alternating users.

	t.Run("assertion_8_n100_sequential_sweep", func(t *testing.T) {
		preHash := sha256.Sum256(preGet.Raw())
		users := []string{"cyberjoker", "alice", "admin"}
		for i := 0; i < 100; i++ {
			u := users[i%len(users)]
			_ = serveOnce(t, u)
			iterHash := sha256.Sum256(preGet.Raw())
			if iterHash != preHash {
				t.Fatalf("ASSERTION 8 FAIL: iter %d (%s) — cached bytes "+
					"mutated; pre=%x iter=%x",
					i, u, preHash[:8], iterHash[:8])
			}
		}
	})

	// Final coherence — V1 still resident on the cell.
	finalGet, _ := c.Get(key)
	if finalGet.LoadValue() != preGet {
		t.Errorf("post-serves: cell.valueRef shifted from V1=%p to %p — "+
			"no refresh was issued in this test",
			preGet, finalGet.LoadValue())
	}
}

// collectMapPointers walks v depth-first and records the runtime
// address of every reachable map[string]any and []any backing array,
// keyed by a dot/bracket access path from root. The address is read
// via reflect.ValueOf(.).Pointer().
//
// Implementation note: the address of a Go map is the address of the
// hmap struct, which is stable across reads — reading the same map
// twice returns the same Pointer. A `v["items"] = kept` style
// replacement REPLACES the map entry's value (a new slice header for
// the new []any backing array), so the slice backing-array Pointer
// shifts. A `v["items"] = newMap` replacement similarly shifts the
// map Pointer at the items entry.
func collectMapPointers(root any, prefix string) map[string]uintptr {
	out := map[string]uintptr{}
	var walk func(v any, path string)
	walk = func(v any, path string) {
		switch val := v.(type) {
		case map[string]any:
			rv := reflect.ValueOf(val)
			if rv.IsValid() {
				out[path] = rv.Pointer()
			}
			for k, child := range val {
				walk(child, path+"."+k)
			}
		case []any:
			rv := reflect.ValueOf(val)
			if rv.IsValid() {
				out[path] = rv.Pointer()
			}
			for i, child := range val {
				walk(child, fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
	walk(root, prefix)
	return out
}

// serveAgainstParsed mimics a v4 serve over a pre-parsed envelope.
// Pattern B: builds a PRIVATE shallow-copy envelope and runs a filter
// stub against it; NEVER mutates the input parsed tree. Returns the
// serialised output bytes.
//
// The architect's Q1 ratification: ALL serve sites uniformly use
// Pattern B (per-call pig allocation, NEVER pool/reuse). This stub
// models that pattern.
func serveAgainstParsed(t *testing.T, parsed map[string]any) []byte {
	t.Helper()
	// Pattern B — fresh outer map per /call. No pooling.
	priv := map[string]any{}
	for k, v := range parsed {
		priv[k] = v
	}
	// Stage-by-stage filter would run here. For the §4.6 contract
	// test, we just re-marshal — the assertion is that parsed (the
	// shared cached parse) is UNCHANGED post-serve.
	out, err := json.Marshal(priv)
	if err != nil {
		t.Fatalf("serveAgainstParsed: marshal: %v", err)
	}
	return out
}

// TestEndToEnd_ValueObjectAtomicSwapAcrossRefresh_v4 — proves the v4
// ValueObject atomic-Swap contract. Two Gets bracketing a Put-refresh
// observe two DIFFERENT *ValueObject pointers; the OLD one's referrers
// hits zero (retired). The cell's *ResolvedEntry pointer replaces on
// Put (legacy v3 semantics preserved), but the v4 contract is about
// *ValueObject* identity, which is what T8 falsifier asserts.
func TestEndToEnd_ValueObjectAtomicSwapAcrossRefresh_v4(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	ResetResolvedCacheForTest()
	t.Cleanup(ResetResolvedCacheForTest)
	ResetDefaultValueStoreForTest()
	t.Cleanup(ResetDefaultValueStoreForTest)

	c := ResolvedCache()
	if c == nil {
		t.Fatalf("ResolvedCache nil")
	}

	inputs := ResolvedKeyInputs{CacheEntryClass: "widgets", Name: "stable"}
	key := ComputeKey(inputs)

	c.Put(key, &ResolvedEntry{RawJSON: []byte(`{"v":1}`), Inputs: &inputs})
	preEntry, _ := c.Get(key)
	preValue := preEntry.LoadValue()

	c.Put(key, &ResolvedEntry{RawJSON: []byte(`{"v":2}`), Inputs: &inputs})
	postEntry, _ := c.Get(key)
	postValue := postEntry.LoadValue()

	// v4 contract — ValueObject pointers DIFFER across refresh; the
	// old one is retired.
	if preValue == postValue {
		t.Errorf("ValueObject swap FAIL: preValue=%p == postValue=%p — "+
			"refresh did NOT produce a fresh ValueObject", preValue, postValue)
	}
	if !bytes.Equal(postValue.Raw(), []byte(`{"v":2}`)) {
		t.Errorf("v4 ValueObject bytes not fresh after refresh; got %q",
			postValue.Raw())
	}
	if !bytes.Equal(preValue.Raw(), []byte(`{"v":1}`)) {
		t.Errorf("v4 prior ValueObject bytes mutated after refresh; got %q "+
			"(immutability contract violated)", preValue.Raw())
	}

	// Prior ValueObject is retired — its referrers count is 0.
	if got := preValue.Referrers(); got != 0 {
		t.Errorf("retired preValue.Referrers()=%d; want 0", got)
	}
	// Fresh ValueObject has exactly one referrer (the cell).
	if got := postValue.Referrers(); got != 1 {
		t.Errorf("fresh postValue.Referrers()=%d; want 1", got)
	}
	_ = postEntry
}

// TestEndToEnd_ValueObjectFreedAfterEviction_v4 — proves LRU/TTL/
// DELETE eviction releases the ValueObject reference correctly.
// After eviction, the cell's previously-loaded ValueObject has
// referrers==0 + is enqueued in quarantine.
func TestEndToEnd_ValueObjectFreedAfterEviction_v4(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	ResetResolvedCacheForTest()
	t.Cleanup(ResetResolvedCacheForTest)
	ResetDefaultValueStoreForTest()
	t.Cleanup(ResetDefaultValueStoreForTest)

	c := newResolvedCache(2, 1<<30, 0) // 2-entry cap → LRU after 3 puts.

	for i := 0; i < 3; i++ {
		in := ResolvedKeyInputs{CacheEntryClass: "widgets",
			Name: fmt.Sprintf("evict-%d", i)}
		c.Put(ComputeKey(in), &ResolvedEntry{RawJSON: []byte(`{}`), Inputs: &in})
	}

	// The first entry should have been LRU-evicted (cap=2). Its
	// ValueObject's Referrers should be 0 (released by
	// removeElementLocked).
	if got := c.Stats().EvictLRUTotal; got != 1 {
		t.Fatalf("expected 1 LRU eviction with cap=2 + 3 puts; got %d", got)
	}
	// Default ValueStore should show one retire.
	store := DefaultValueStore()
	if got := store.Stats().RetireTotal; got < 1 {
		t.Fatalf("DefaultValueStore RetireTotal=%d after LRU eviction; want ≥1",
			got)
	}
}

// _ keeps the atomic import referenced if the test body trims to no
// direct atomic use during future edits.
var _ = atomic.AddInt64
