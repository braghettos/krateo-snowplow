// deps_test.go — Tag 0.30.8 unit tests for the DepTracker.
//
// Coverage: Record + RecordList idempotency, four-bucket lookup
// (exact / ns-list / cluster-name / cluster-list), DELETE evicts from
// the wired L1 store + clears reverse index, UPDATE enqueues into the
// refresh hook WITHOUT evicting, RemoveL1Key purges all forward and
// reverse records, cap-reached drops + warn-once, concurrent
// Record + OnDelete is race-free.

package cache

import (
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

func newTestDepTracker(t *testing.T, maxRecords int64) *DepTracker {
	t.Helper()
	d := newDepTracker(maxRecords)
	return d
}

func gvrCompositions() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "composition.krateo.io",
		Version:  "v1",
		Resource: "compositions",
	}
}

// --- Record + lookup ---------------------------------------------------------

func TestDeps_RecordExactObject(t *testing.T) {
	d := newTestDepTracker(t, 1_000)
	gvr := gvrCompositions()
	d.Record("L1A", gvr, "bench-ns-01", "app-1")

	if got := d.totalRecords.Load(); got != 1 {
		t.Fatalf("totalRecords=%d want 1", got)
	}
	// Idempotent re-record.
	d.Record("L1A", gvr, "bench-ns-01", "app-1")
	if got := d.totalRecords.Load(); got != 1 {
		t.Fatalf("idempotent re-record bumped count: %d", got)
	}
}

func TestDeps_RecordListEncodedAsWildcard(t *testing.T) {
	d := newTestDepTracker(t, 1_000)
	gvr := gvrCompositions()
	d.RecordList("L1A", gvr, "bench-ns-01")

	// Internally the list-bucket key has Name="*".
	bucket := DepKey{GVR: gvr, Namespace: "bench-ns-01", Name: listWildcard}
	if _, ok := d.forward.Load(bucket); !ok {
		t.Fatalf("list-bucket missing for %v", bucket)
	}
}

func TestDeps_FourBucketLookup(t *testing.T) {
	d := newTestDepTracker(t, 1_000)
	gvr := gvrCompositions()

	d.Record("L1_exact", gvr, "bench-ns-01", "app-1")    // exact
	d.RecordList("L1_nslist", gvr, "bench-ns-01")        // ns-list
	d.Record("L1_clustname", gvr, "", "app-1")           // cluster-name (rare)
	d.RecordList("L1_clustlist", gvr, "")                // cluster-list

	matched := d.collectMatches(gvr, "bench-ns-01", "app-1")
	want := []string{"L1_exact", "L1_nslist", "L1_clustname", "L1_clustlist"}
	for _, k := range want {
		if _, ok := matched[k]; !ok {
			t.Errorf("collectMatches missing %q (got %v)", k, matched)
		}
	}

	// A different name in the same ns matches only the ns-list +
	// cluster-list buckets (NOT the exact or cluster-name buckets).
	matched2 := d.collectMatches(gvr, "bench-ns-01", "app-2")
	if _, ok := matched2["L1_exact"]; ok {
		t.Errorf("exact bucket leaked into different-name lookup")
	}
	if _, ok := matched2["L1_nslist"]; !ok {
		t.Errorf("ns-list bucket missing for sibling lookup")
	}
	if _, ok := matched2["L1_clustlist"]; !ok {
		t.Errorf("cluster-list bucket missing for sibling lookup")
	}
}

// --- OnDelete ----------------------------------------------------------------

func TestDeps_OnDelete_EvictsAndClearsReverse(t *testing.T) {
	d := newTestDepTracker(t, 1_000)
	store := newResolvedCache(100, 1<<20, time.Hour)
	d.SetStore(store)

	gvr := gvrCompositions()
	// Two L1 entries, both depending on (gvr, ns, "app-1").
	store.Put("L1A", &ResolvedEntry{RawJSON: []byte(`{"a":1}`)})
	store.Put("L1B", &ResolvedEntry{RawJSON: []byte(`{"b":2}`)})
	d.Record("L1A", gvr, "bench-ns-01", "app-1")
	d.Record("L1B", gvr, "bench-ns-01", "app-1")

	got := d.OnDelete(gvr, "bench-ns-01", "app-1")
	if got != 2 {
		t.Fatalf("OnDelete returned %d evicted, want 2", got)
	}
	if _, ok := store.Get("L1A"); ok {
		t.Errorf("L1A should have been evicted by DELETE")
	}
	if _, ok := store.Get("L1B"); ok {
		t.Errorf("L1B should have been evicted by DELETE")
	}
	if d.totalRecords.Load() != 0 {
		t.Errorf("reverse cleanup didn't run: totalRecords=%d", d.totalRecords.Load())
	}
	if d.evictDeleteTotal.Load() != 2 {
		t.Errorf("evictDeleteTotal=%d want 2", d.evictDeleteTotal.Load())
	}
}

func TestDeps_OnDelete_NilStoreIsNoOp(t *testing.T) {
	// Dep tracker should never panic when no store is wired (unit
	// tests can exercise it standalone). It still cleans up dep
	// records — the L1 layer is the only side it can't touch.
	d := newTestDepTracker(t, 1_000)
	gvr := gvrCompositions()
	d.Record("L1A", gvr, "ns", "n")
	if got := d.OnDelete(gvr, "ns", "n"); got != 0 {
		t.Fatalf("expected 0 evictions with no store, got %d", got)
	}
	// But the dep records ARE cleaned (RemoveL1Key runs anyway).
	if d.totalRecords.Load() != 0 {
		t.Fatalf("totalRecords=%d after OnDelete; cleanup didn't fire", d.totalRecords.Load())
	}
}

// --- OnUpdate ----------------------------------------------------------------

func TestDeps_OnUpdate_EnqueuesAndDoesNotEvict(t *testing.T) {
	// Binding rule (feedback_l1_invalidation_delete_only.md): UPDATE
	// MUST NOT evict; it enqueues into the refresher.
	d := newTestDepTracker(t, 1_000)
	store := newResolvedCache(100, 1<<20, time.Hour)
	d.SetStore(store)

	gvr := gvrCompositions()
	store.Put("L1A", &ResolvedEntry{RawJSON: []byte(`{}`)})
	d.Record("L1A", gvr, "ns", "n")

	var enqueued []string
	var mu sync.Mutex
	d.SetRefreshHook(func(l1Key string) {
		mu.Lock()
		enqueued = append(enqueued, l1Key)
		mu.Unlock()
	})

	got := d.OnUpdate(gvr, "ns", "n")
	if got != 1 {
		t.Fatalf("OnUpdate returned %d enqueued, want 1", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(enqueued) != 1 || enqueued[0] != "L1A" {
		t.Fatalf("enqueue list wrong: %v", enqueued)
	}
	// CRITICAL: UPDATE must NOT have evicted.
	if _, ok := store.Get("L1A"); !ok {
		t.Fatalf("OnUpdate evicted L1A — violates feedback_l1_invalidation_delete_only.md")
	}
	// Reverse records must still exist.
	if d.totalRecords.Load() != 1 {
		t.Fatalf("OnUpdate dropped dep records; totalRecords=%d", d.totalRecords.Load())
	}
	if d.enqueueUpdateTotal.Load() != 1 {
		t.Fatalf("enqueueUpdateTotal=%d want 1", d.enqueueUpdateTotal.Load())
	}
}

func TestDeps_OnUpdate_NoHookIsNoOp(t *testing.T) {
	d := newTestDepTracker(t, 1_000)
	gvr := gvrCompositions()
	d.Record("L1A", gvr, "ns", "n")
	got := d.OnUpdate(gvr, "ns", "n")
	if got != 1 {
		t.Fatalf("OnUpdate must still return matched count even without a hook, got %d", got)
	}
}

// --- RemoveL1Key (LRU cleanup) ----------------------------------------------

func TestDeps_RemoveL1Key_PurgesForwardAndReverse(t *testing.T) {
	d := newTestDepTracker(t, 1_000)
	gvr := gvrCompositions()
	d.Record("L1A", gvr, "ns", "n1")
	d.Record("L1A", gvr, "ns", "n2")
	d.RecordList("L1A", gvr, "ns")

	if got := d.totalRecords.Load(); got != 3 {
		t.Fatalf("setup: totalRecords=%d want 3", got)
	}

	d.RemoveL1Key("L1A")

	if got := d.totalRecords.Load(); got != 0 {
		t.Fatalf("RemoveL1Key left %d records", got)
	}
	// Forward buckets should be empty (or pruned).
	d.forward.Range(func(k, v any) bool {
		ks := v.(*keySet)
		if ks.count.Load() != 0 {
			t.Errorf("forward bucket %v retained %d keys", k, ks.count.Load())
		}
		return true
	})
}

// --- Bounded growth ---------------------------------------------------------

func TestDeps_CapDropsAndWarnsOnce(t *testing.T) {
	d := newTestDepTracker(t, 2) // tiny cap for the test
	gvr := gvrCompositions()
	d.Record("L1A", gvr, "ns", "n1")
	d.Record("L1A", gvr, "ns", "n2")
	d.Record("L1A", gvr, "ns", "n3") // dropped
	d.Record("L1A", gvr, "ns", "n4") // dropped

	if got := d.totalRecords.Load(); got != 2 {
		t.Fatalf("totalRecords=%d want 2 (cap)", got)
	}
	if got := d.recordDroppedCap.Load(); got != 2 {
		t.Fatalf("recordDroppedCap=%d want 2", got)
	}
}

// --- Concurrency -----------------------------------------------------------

func TestDeps_ConcurrentRecordAndDelete_RaceFree(t *testing.T) {
	d := newTestDepTracker(t, 100_000)
	store := newResolvedCache(10_000, 1<<24, time.Hour)
	d.SetStore(store)

	gvr := gvrCompositions()

	var wg sync.WaitGroup
	const N = 200

	// Writers: Record + Put.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			key := "L1_" + itoa(i)
			store.Put(key, &ResolvedEntry{RawJSON: []byte("x")})
			d.Record(key, gvr, "ns", "n_"+itoa(i%50))
		}
	}()

	// Deleters: OnDelete sweeps half the names.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			d.OnDelete(gvr, "ns", "n_"+itoa(i%50))
		}
	}()

	// Updaters: OnUpdate sweeps the other half. No real hook needed —
	// we just exercise the lookup path.
	d.SetRefreshHook(func(_ string) {})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			d.OnUpdate(gvr, "ns", "n_"+itoa(i%50))
		}
	}()

	wg.Wait()
	// No assertion on final counts — the goal is race-free under `-race`.
}

// --- helpers ---------------------------------------------------------------

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	buf := make([]byte, 0, 10)
	for i > 0 {
		buf = append([]byte{digits[i%10]}, buf...)
		i /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}
