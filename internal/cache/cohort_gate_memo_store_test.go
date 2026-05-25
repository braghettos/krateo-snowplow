// cohort_gate_memo_store_test.go — Ship GMC / 0.30.174.
//
// Concurrency falsifier per feedback_shared_vs_copy_is_a_concurrency_change:
// the per-ResolvedEntry cohort store is a sync.Map + bounded LRU. The test
// drives N goroutines into the same store with overlapping cohort keys and
// asserts:
//
//   - No data race (-race must report clean).
//   - Size never exceeds the cap.
//   - Every published cohort key is observable by a Lookup.
//   - LRU eviction kicks in when the cap is exceeded.

package cache

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCohortGateMemoStore_ConcurrentSameCohortPopulate(t *testing.T) {
	// Two goroutines racing to populate the SAME cohort key MUST end
	// up with EXACTLY ONE memo in the store; both Lookups return a
	// non-nil memo afterwards.
	s := NewCohortGateMemoStore()
	cohort := "same-cohort"

	const workers = 32
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			memo := &struct{ idx int }{idx: i}
			s.Store(cohort, memo)
			v, ok := s.Lookup(cohort)
			if !ok || v == nil {
				t.Errorf("goroutine %d: Lookup after Store missed", i)
			}
		}(i)
	}
	wg.Wait()

	if got := s.Size(); got != 1 {
		t.Fatalf("Size after %d concurrent Store of same cohort = %d, want 1", workers, got)
	}
	if _, ok := s.Lookup(cohort); !ok {
		t.Fatalf("Lookup(%q) missed after concurrent Store", cohort)
	}
}

func TestCohortGateMemoStore_LRUCapEvicts(t *testing.T) {
	s := NewCohortGateMemoStore()

	// Insert exactly cap+10 distinct cohorts; size MUST settle at cap;
	// overflow counter MUST be 10.
	for i := 0; i < CohortGateMemoMaxEntries+10; i++ {
		key := "c-" + strconv.Itoa(i)
		s.Store(key, &struct{}{})
	}
	if got := s.Size(); got != int64(CohortGateMemoMaxEntries) {
		t.Fatalf("Size after over-cap insertions = %d, want %d", got, CohortGateMemoMaxEntries)
	}
	if got := s.OverflowTotal(); got != 10 {
		t.Fatalf("OverflowTotal = %d, want 10", got)
	}

	// Oldest 10 keys ("c-0".."c-9") MUST be evicted; newest 10 MUST be
	// present.
	for i := 0; i < 10; i++ {
		if _, ok := s.Lookup("c-" + strconv.Itoa(i)); ok {
			t.Fatalf("Lookup(c-%d) hit; expected evicted", i)
		}
	}
	for i := CohortGateMemoMaxEntries + 5; i < CohortGateMemoMaxEntries+10; i++ {
		if _, ok := s.Lookup("c-" + strconv.Itoa(i)); !ok {
			t.Fatalf("Lookup(c-%d) missed; expected present", i)
		}
	}
}

func TestCohortGateMemoStore_LoadOrInit_ConcurrentSameEntry(t *testing.T) {
	// Two goroutines racing on the same *ResolvedEntry MUST see the
	// SAME *CohortGateMemoStore — the atomic.Pointer.CompareAndSwap
	// publishes the winner; losers discard their allocation.
	entry := &ResolvedEntry{}

	const workers = 64
	var (
		wg    sync.WaitGroup
		first atomic.Pointer[CohortGateMemoStore]
	)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			s := CohortGateMemoStoreLoadOrInit(entry)
			if s == nil {
				t.Errorf("LoadOrInit returned nil")
				return
			}
			// Publish the first store seen; assert every subsequent
			// observation matches it.
			if first.CompareAndSwap(nil, s) {
				return
			}
			if want := first.Load(); want != s {
				t.Errorf("LoadOrInit returned divergent stores: %p vs %p", want, s)
			}
		}()
	}
	wg.Wait()

	if got := entry.CohortGates.Load(); got == nil {
		t.Fatalf("CohortGates still nil after LoadOrInit")
	}
}

func TestCohortGateMemoStore_NilSafe(t *testing.T) {
	var s *CohortGateMemoStore // nil

	// All methods MUST be nil-safe.
	if _, ok := s.Lookup("k"); ok {
		t.Fatalf("nil store Lookup returned ok=true")
	}
	if s.Store("k", &struct{}{}) {
		t.Fatalf("nil store Store returned evicted=true")
	}
	if got := s.Size(); got != 0 {
		t.Fatalf("nil store Size = %d, want 0", got)
	}
	if got := s.OverflowTotal(); got != 0 {
		t.Fatalf("nil store OverflowTotal = %d, want 0", got)
	}

	// Nil entry MUST yield nil store.
	if got := CohortGateMemoStoreLoadOrInit(nil); got != nil {
		t.Fatalf("LoadOrInit(nil) = %p, want nil", got)
	}
}

func TestCohortGateMemoStore_LookupEmptyKey(t *testing.T) {
	s := NewCohortGateMemoStore()
	if _, ok := s.Lookup(""); ok {
		t.Fatalf("Lookup with empty key returned ok=true")
	}
	if s.Store("", &struct{}{}) {
		t.Fatalf("Store with empty key returned evicted=true")
	}
	if got := s.Size(); got != 0 {
		t.Fatalf("Size after empty-key Store = %d, want 0", got)
	}
}
