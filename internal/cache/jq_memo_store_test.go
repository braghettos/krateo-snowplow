// jq_memo_store_test.go — Ship 0.30.240 JQ memo primitive coverage.
//
// Race + correctness coverage for the singleflight-guarded
// LookupOrPopulate path, the cap+evict path, and the global counter.

package cache

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// TestJQMemoStore_LookupOrPopulate_SingleflightDedupes — N=64
// concurrent LookupOrPopulate for the SAME key must invoke populate
// exactly ONCE. The remaining 63 calls wait for the singleflight
// winner and receive the same result.
func TestJQMemoStore_LookupOrPopulate_SingleflightDedupes(t *testing.T) {
	s := NewJQMemoStore()

	var populateCount atomic.Int32
	populate := func() (any, error) {
		populateCount.Add(1)
		return map[string]any{"totalCount": float64(42)}, nil
	}

	const N = 64
	var wg sync.WaitGroup
	wg.Add(N)
	results := make([]any, N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			v, err := s.LookupOrPopulate("k", populate)
			if err != nil {
				t.Errorf("LookupOrPopulate[%d] err: %v", i, err)
				return
			}
			results[i] = v
		}(i)
	}
	wg.Wait()

	if got := populateCount.Load(); got != 1 {
		t.Errorf("singleflight FAIL: populate ran %d times; want 1", got)
	}
	// Every result must be the same map (semantically; identity not
	// required because LookupOrPopulate may return either the
	// singleflight winner's value or a Lookup-hit on a parallel
	// caller's Store).
	for i, r := range results {
		m, ok := r.(map[string]any)
		if !ok {
			t.Errorf("results[%d] is %T; want map[string]any", i, r)
			continue
		}
		if v, _ := m["totalCount"].(float64); v != 42 {
			t.Errorf("results[%d] totalCount=%v; want 42", i, v)
		}
	}
}

// TestJQMemoStore_LookupOrPopulate_ErrorNotMemoized — a populate
// error must NOT store anything; the next LookupOrPopulate retries.
func TestJQMemoStore_LookupOrPopulate_ErrorNotMemoized(t *testing.T) {
	s := NewJQMemoStore()

	sentinelErr := errors.New("populate failure")
	var populateCount atomic.Int32
	populate := func() (any, error) {
		populateCount.Add(1)
		return nil, sentinelErr
	}

	if _, err := s.LookupOrPopulate("k", populate); !errors.Is(err, sentinelErr) {
		t.Fatalf("first LookupOrPopulate err = %v; want sentinelErr", err)
	}
	if _, err := s.LookupOrPopulate("k", populate); !errors.Is(err, sentinelErr) {
		t.Fatalf("second LookupOrPopulate err = %v; want sentinelErr (memo "+
			"should NOT have cached the error)", err)
	}
	if got := populateCount.Load(); got != 2 {
		t.Errorf("populate ran %d times; want 2 (error should not memoize)", got)
	}
}

// TestJQMemoStore_StoreLookup_RoundTrip — basic shape: Store then
// Lookup returns the same value.
func TestJQMemoStore_StoreLookup_RoundTrip(t *testing.T) {
	s := NewJQMemoStore()
	memo := map[string]any{"a": float64(1)}
	if evicted := s.Store("k", memo); evicted {
		t.Errorf("first Store evicted; want false")
	}
	got, ok := s.Lookup("k")
	if !ok {
		t.Fatalf("Lookup after Store miss")
	}
	if m := got.(map[string]any); m["a"] != float64(1) {
		t.Errorf("Lookup returned wrong value: %+v", m)
	}
}

// TestJQMemoStore_CapEvictsOldestInsertion — capacity-bound store
// evicts oldest-INSERTED entry on overflow.
func TestJQMemoStore_CapEvictsOldestInsertion(t *testing.T) {
	t.Setenv("CACHE_JQ_MEMO_CAP", "2")
	s := NewJQMemoStore()

	s.Store("k1", map[string]any{"v": 1})
	s.Store("k2", map[string]any{"v": 2})
	if got := s.Size(); got != 2 {
		t.Fatalf("size after 2 stores = %d; want 2", got)
	}
	// Third store overflows — k1 (oldest) must evict.
	if !s.Store("k3", map[string]any{"v": 3}) {
		t.Fatalf("third Store did NOT report eviction")
	}
	if _, ok := s.Lookup("k1"); ok {
		t.Errorf("k1 should have been evicted")
	}
	if _, ok := s.Lookup("k2"); !ok {
		t.Errorf("k2 should survive")
	}
	if _, ok := s.Lookup("k3"); !ok {
		t.Errorf("k3 should be present (just inserted)")
	}
	if got := s.OverflowTotal(); got != 1 {
		t.Errorf("overflowTotal = %d; want 1", got)
	}
}

// TestJQMemoStore_NilSafe — nil store / empty key are no-ops.
func TestJQMemoStore_NilSafe(t *testing.T) {
	var s *JQMemoStore
	if v, ok := s.Lookup("k"); v != nil || ok {
		t.Errorf("nil Lookup: %v %v; want nil false", v, ok)
	}
	if s.Store("k", "v") {
		t.Errorf("nil Store returned true")
	}
	if v, err := s.LookupOrPopulate("k", func() (any, error) { return "X", nil }); v != "X" || err != nil {
		t.Errorf("nil LookupOrPopulate should still invoke populate: v=%v err=%v", v, err)
	}
}

// TestJQMemoComposeKey_Deterministic — same inputs yield same output;
// any input change yields a different key.
func TestJQMemoComposeKey_Deterministic(t *testing.T) {
	k1 := JQMemoComposeKey("widget-abc", "cohort-xyz", 7)
	k2 := JQMemoComposeKey("widget-abc", "cohort-xyz", 7)
	if k1 != k2 {
		t.Errorf("non-deterministic: %s vs %s", k1, k2)
	}
	cases := []struct {
		name string
		w, c string
		g    uint64
	}{
		{"different widget", "widget-DIFF", "cohort-xyz", 7},
		{"different cohort", "widget-abc", "cohort-DIFF", 7},
		{"different gen", "widget-abc", "cohort-xyz", 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := JQMemoComposeKey(tc.w, tc.c, tc.g)
			if k == k1 {
				t.Errorf("expected different key for %s; got identical", tc.name)
			}
		})
	}
}

// TestJQMemoStoreLoadOrInit_AtomicFirstWriterWins — N concurrent
// goroutines calling LoadOrInit on the same entry receive the SAME
// *JQMemoStore (atomic CAS).
func TestJQMemoStoreLoadOrInit_AtomicFirstWriterWins(t *testing.T) {
	entry := &ResolvedEntry{}
	const N = 64
	stores := make([]*JQMemoStore, N)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			stores[i] = JQMemoStoreLoadOrInit(entry)
		}(i)
	}
	wg.Wait()
	first := stores[0]
	for i, s := range stores {
		if s != first {
			t.Errorf("stores[%d]=%p ≠ stores[0]=%p (atomic LoadOrInit raced)", i, s, first)
		}
	}
	if first == nil {
		t.Errorf("LoadOrInit returned nil for non-nil entry")
	}
}
