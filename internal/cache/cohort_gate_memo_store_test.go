// cohort_gate_memo_store_test.go — Ship GMC / 0.30.174, A.2 / 0.30.178.
//
// Concurrency falsifier per feedback_shared_vs_copy_is_a_concurrency_change:
// the per-ResolvedEntry cohort store is a sync.Map (zero-bound, Ship A.2).
// The test drives N goroutines into the same store with overlapping cohort
// keys and asserts:
//
//   - No data race (-race must report clean).
//   - Every published cohort key is observable by a Lookup.
//   - Size grows linearly with distinct cohort keys (no cap, no eviction).

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

// TestCohortGateMemoStore_ZeroBoundGrowth — Ship A.2 / 0.30.178: the
// store has no LRU and no eviction. Inserting N distinct cohort keys
// must leave Size() at N and every key observable by Lookup.
func TestCohortGateMemoStore_ZeroBoundGrowth(t *testing.T) {
	s := NewCohortGateMemoStore()

	const n = 1024 // well above the pre-A.2 cap of 256 — verifies no eviction.
	for i := 0; i < n; i++ {
		key := "c-" + strconv.Itoa(i)
		s.Store(key, &struct{ k string }{k: key})
	}
	if got := s.Size(); got != int64(n) {
		t.Fatalf("Size after %d distinct insertions = %d, want %d", n, got, n)
	}
	// Every inserted key MUST still be present — no LRU eviction.
	for i := 0; i < n; i++ {
		key := "c-" + strconv.Itoa(i)
		if _, ok := s.Lookup(key); !ok {
			t.Fatalf("Lookup(%q) missed; expected present (zero-bound store)", key)
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
