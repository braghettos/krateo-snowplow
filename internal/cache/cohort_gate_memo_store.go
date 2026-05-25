// cohort_gate_memo_store.go — Ship GMC / 0.30.174.
//
// The per-ResolvedEntry storage primitive for the cohort gate memo.
// Lookup is lock-free (sync.Map.Load); Store takes a single small
// mutex to keep the bounded-LRU eviction order consistent with the
// sync.Map publication.
//
// SAFETY (binding)
//   - feedback_l1_per_user_keyed_never_cohort.md — this store holds NO
//     resolved-output payloads; only an opaque per-cohort memo whose
//     semantics (kept-name set + rbac generation) live in api/
//     apistage_cohort_memo.go. The L1 RESOLVED cache itself stays
//     per-user-keyed.
//   - feedback_capacity_caps_empirical_per_entry_cost.md — bounded
//     LRU at 256 entries; per-memo size is dominated by the kept-name
//     map (api-side), not by this store's own bookkeeping.
//   - feedback_shared_vs_copy_is_a_concurrency_change.md — memos are
//     opaque interface values populated by callers; THIS file makes no
//     assumption about their mutability. Callers MUST treat stored
//     memos as immutable after Store returns.
//
// LIFETIME
//   - The store is lazily attached to a ResolvedEntry via
//     CohortGateMemoStoreLoadOrInit. It survives the entry's LRU
//     lifetime; eviction of the entry from the resolved cache drops
//     the store along with the entry. No separate sweep.
//   - The store NEVER holds a back-pointer to the entry; the entry's
//     atomic.Pointer is the only edge.

package cache

import (
	"container/list"
	"sync"
	"sync/atomic"
)

// CohortGateMemoMaxEntries is the per-ResolvedEntry LRU cap on the
// number of distinct cohorts memoized. Per the Ship GMC brief: simple
// counter + insertion-order list; on overflow log + evict oldest.
//
// 256 is well above the expected cohort cardinality (admin +
// cyberjoker + a handful of per-namespace cohorts) and keeps per-entry
// overhead bounded under pathological cohort churn.
const CohortGateMemoMaxEntries = 256

// CohortGateMemoStore is the per-ResolvedEntry container of cohort
// memos. The stored value type is `any` because the memo shape
// (keptNames + rbacGen) lives in api/ — keeping cache/ free of that
// import. Callers Lookup/Store via type assertion on the receiving
// end; the cap+LRU machinery here is value-shape-agnostic.
type CohortGateMemoStore struct {
	memos sync.Map // string (cohortKey) -> any (memo payload)

	mu    sync.Mutex
	order *list.List // front = MRU. Element.Value is the cohortKey string.
	index map[string]*list.Element

	// size mirrors order.Len() but is readable without mu for the
	// quick "are we over cap?" check before taking the writer lock.
	// Reconciled INSIDE mu on every mutation.
	size atomic.Int64

	// OverflowTotal counts how often the cap+evict path fired.
	overflowTotal atomic.Uint64
}

// NewCohortGateMemoStore constructs an empty store.
func NewCohortGateMemoStore() *CohortGateMemoStore {
	return &CohortGateMemoStore{
		order: list.New(),
		index: map[string]*list.Element{},
	}
}

// CohortGateMemoStoreLoadOrInit atomically returns the *CohortGateMemoStore
// attached to entry, creating one on the first call. Concurrent first
// callers race via atomic.Pointer.CompareAndSwap; losers discard their
// fresh allocation and reuse the winner's store. Nil-safe on entry.
func CohortGateMemoStoreLoadOrInit(entry *ResolvedEntry) *CohortGateMemoStore {
	if entry == nil {
		return nil
	}
	if s := entry.CohortGates.Load(); s != nil {
		return s
	}
	fresh := NewCohortGateMemoStore()
	if entry.CohortGates.CompareAndSwap(nil, fresh) {
		return fresh
	}
	// Lost the race — drop our allocation; return the winner.
	return entry.CohortGates.Load()
}

// Lookup returns the memo for cohortKey, or (nil, false). Lock-free
// fast path — sync.Map.Load.
func (s *CohortGateMemoStore) Lookup(cohortKey string) (any, bool) {
	if s == nil || cohortKey == "" {
		return nil, false
	}
	v, ok := s.memos.Load(cohortKey)
	if !ok {
		return nil, false
	}
	return v, true
}

// Store records memo under cohortKey, evicting the oldest entry when
// the per-entry cap (CohortGateMemoMaxEntries) is exceeded. Returns
// true if an LRU eviction fired.
//
// The publication ordering is intentional:
//  1. LoadOrStore into sync.Map — readers can hit the new memo now.
//  2. PushFront under mu — eviction order learns about the new entry.
//
// A concurrent Lookup between (1) and (2) returns the populated memo,
// which is correct (memos are immutable after Store). The window is
// bounded by the few atomics in step 2.
func (s *CohortGateMemoStore) Store(cohortKey string, memo any) bool {
	if s == nil || cohortKey == "" || memo == nil {
		return false
	}
	if _, loaded := s.memos.LoadOrStore(cohortKey, memo); loaded {
		// Replacement of an existing cohort key — refresh the LRU
		// touch under mu (no eviction needed) and return.
		s.mu.Lock()
		if el, ok := s.index[cohortKey]; ok {
			s.order.MoveToFront(el)
		}
		s.mu.Unlock()
		return false
	}

	s.mu.Lock()
	el := s.order.PushFront(cohortKey)
	s.index[cohortKey] = el
	s.size.Add(1)

	evicted := false
	for s.size.Load() > CohortGateMemoMaxEntries {
		tail := s.order.Back()
		if tail == nil {
			break
		}
		victimKey, _ := tail.Value.(string)
		s.order.Remove(tail)
		delete(s.index, victimKey)
		s.size.Add(-1)
		s.memos.Delete(victimKey)
		evicted = true
		s.overflowTotal.Add(1)
	}
	s.mu.Unlock()
	return evicted
}

// Size returns the current number of memo entries. Safe under
// concurrent traffic.
func (s *CohortGateMemoStore) Size() int64 {
	if s == nil {
		return 0
	}
	return s.size.Load()
}

// OverflowTotal returns the cumulative count of LRU evictions fired
// by the cap. Useful for ops correlation.
func (s *CohortGateMemoStore) OverflowTotal() uint64 {
	if s == nil {
		return 0
	}
	return s.overflowTotal.Load()
}
