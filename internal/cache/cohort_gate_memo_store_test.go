// cohort_gate_memo_store_test.go — Ship GMC / 0.30.174, A.2 / 0.30.178,
// Ship 3 / 0.30.197.
//
// Concurrency falsifier per feedback_shared_vs_copy_is_a_concurrency_change:
// the per-ResolvedEntry cohort store is a sync.Map for lock-free Lookup +
// an insertion-order eviction cap touched only on the Store miss path
// (Ship 3). The tests drive N goroutines into the same store with
// overlapping cohort keys and assert:
//
//   - No data race (-race must report clean).
//   - Every published cohort key (within the cap window) is observable.
//   - Size stays bounded by CACHE_COHORT_MEMO_CAP (eviction decrements).
//   - The lock-free Lookup hot path has no recency machinery (benchmark).

package cache

import (
	"math/rand"
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

// TestCohortGateMemoStore_BoundedGrowth — Ship 3 / 0.30.197: the store
// re-bounds the last O(cohorts) memory structure via an insertion-order
// cap (CACHE_COHORT_MEMO_CAP). Inserting N >> cap distinct cohort keys
// must leave Size() <= cap (eviction decrements). This FAILS against the
// pre-Ship-3 zero-bound code (which leaves Size() == N), proving it is a
// real falsifier, not a tautology.
func TestCohortGateMemoStore_BoundedGrowth(t *testing.T) {
	t.Setenv("CACHE_COHORT_MEMO_CAP", "64")
	s := NewCohortGateMemoStore()

	const (
		n   = 1024
		cap = 64
	)
	for i := 0; i < n; i++ {
		key := "c-" + strconv.Itoa(i)
		s.Store(key, &struct{ k string }{k: key})
	}
	if got := s.Size(); got > cap {
		t.Fatalf("Size after %d distinct insertions = %d, want <= %d (bounded)", n, got, cap)
	}
	// Insertion-order eviction: the newest-inserted keys survive; the
	// last key MUST still be observable.
	last := "c-" + strconv.Itoa(n-1)
	if _, ok := s.Lookup(last); !ok {
		t.Fatalf("Lookup(%q) missed; newest-inserted key must survive", last)
	}
	// The oldest-inserted key MUST have been evicted.
	if _, ok := s.Lookup("c-0"); ok {
		t.Fatalf("Lookup(c-0) hit; oldest-inserted key must be evicted under cap %d", cap)
	}
	if got := s.OverflowTotal(); got == 0 {
		t.Fatalf("OverflowTotal = 0; expected eviction to have fired with %d insertions over cap %d", n, cap)
	}
}

// TestCohortGateMemoStore_UnboundedEscapeHatch — Ship 3 / 0.30.197:
// CACHE_COHORT_MEMO_CAP <= 0 restores the 0.30.178 zero-bound behavior
// (the provisional/removable cache contract). Inserting N distinct keys
// leaves Size() == N with no eviction.
func TestCohortGateMemoStore_UnboundedEscapeHatch(t *testing.T) {
	t.Setenv("CACHE_COHORT_MEMO_CAP", "0")
	s := NewCohortGateMemoStore()

	const n = 1024
	for i := 0; i < n; i++ {
		key := "c-" + strconv.Itoa(i)
		s.Store(key, &struct{ k string }{k: key})
	}
	if got := s.Size(); got != int64(n) {
		t.Fatalf("Size after %d insertions with cap<=0 = %d, want %d (unbounded)", n, got, n)
	}
	for i := 0; i < n; i++ {
		key := "c-" + strconv.Itoa(i)
		if _, ok := s.Lookup(key); !ok {
			t.Fatalf("Lookup(%q) missed; cap<=0 must not evict", key)
		}
	}
	if got := s.OverflowTotal(); got != 0 {
		t.Fatalf("OverflowTotal = %d with cap<=0; want 0 (no eviction)", got)
	}
}

// TestCohortGateMemoStore_ConcurrentStoreEvictRace — Ship 3 / 0.30.197.
// MANDATORY -race: this is a lock-free->bounded concurrency change. cap=8,
// 64 goroutines each Store distinct keys while concurrently Looking up
// random keys. After wg.Wait(): Size() <= cap and a recently-inserted key
// (never a candidate for eviction) still Looks up.
func TestCohortGateMemoStore_ConcurrentStoreEvictRace(t *testing.T) {
	t.Setenv("CACHE_COHORT_MEMO_CAP", "8")
	s := NewCohortGateMemoStore()

	const (
		workers  = 64
		perWkr   = 32
		capLimit = 8
	)
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(w)))
			for j := 0; j < perWkr; j++ {
				key := "w" + strconv.Itoa(w) + "-k" + strconv.Itoa(j)
				s.Store(key, &struct{ k string }{k: key})
				// Concurrent lock-free Lookup of an arbitrary key.
				probe := "w" + strconv.Itoa(rng.Intn(workers)) + "-k" + strconv.Itoa(rng.Intn(perWkr))
				_, _ = s.Lookup(probe)
			}
		}(w)
	}
	wg.Wait()

	if got := s.Size(); got > capLimit {
		t.Fatalf("Size after concurrent stores = %d, want <= %d (bounded under race)", got, capLimit)
	}
	// Re-Store a fresh key now (no contention) — it must be present and
	// Size still bounded.
	fresh := "fresh-after-wait"
	s.Store(fresh, &struct{ k string }{k: fresh})
	if _, ok := s.Lookup(fresh); !ok {
		t.Fatalf("Lookup(%q) missed; a just-inserted key must survive", fresh)
	}
	if got := s.Size(); got > capLimit {
		t.Fatalf("Size after fresh insert = %d, want <= %d", got, capLimit)
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

// BenchmarkCohortGateMemoStore_LookupHit — Ship 3 / 0.30.197 head-on
// proof that the read path is unchanged. Pre-populate 34 keys (today's
// cohort scale), then b.RunParallel pure Lookup hits. The Lookup path is
// a single sync.Map.Load with NO lock and NO recency touch, so ns/op
// must be within noise of the pre-Ship-3 zero-bound build.
func BenchmarkCohortGateMemoStore_LookupHit(b *testing.B) {
	s := NewCohortGateMemoStore()
	const keys = 34
	for i := 0; i < keys; i++ {
		key := "c-" + strconv.Itoa(i)
		s.Store(key, &struct{ k string }{k: key})
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := "c-" + strconv.Itoa(i%keys)
			if _, ok := s.Lookup(key); !ok {
				b.Fatalf("Lookup(%q) missed during benchmark", key)
			}
			i++
		}
	})
}
