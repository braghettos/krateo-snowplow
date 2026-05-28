// cohort_gate_memo_store.go — Ship GMC / 0.30.174, A.2 / 0.30.178,
// Ship 3 / 0.30.197.
//
// The per-ResolvedEntry storage primitive for the cohort gate memo.
// Lookup is lock-free (sync.Map.Load); Store takes a single small
// mutex ONLY on the memo-miss/stale path (never on a hit) to keep the
// insertion-order eviction bookkeeping consistent with the sync.Map
// publication. At steady state — when callers hit the memo — Store
// never fires, so the hot path is pure lock-free Lookup.
//
// SAFETY (binding)
//   - feedback_l1_per_user_keyed_never_cohort.md — this store holds NO
//     resolved-output payloads; only an opaque per-cohort memo whose
//     semantics (kept-name set + rbac generation + zero-bound encoded
//     bytes) live in api/apistage_cohort_memo.go. The L1 RESOLVED
//     cache itself stays per-user-keyed.
//   - feedback_shared_vs_copy_is_a_concurrency_change.md — memos are
//     opaque interface values populated by callers; THIS file makes no
//     assumption about their mutability. Callers MUST treat stored
//     memos as immutable after Store returns.
//   - feedback_capacity_caps_empirical_per_entry_cost.md — the cap is
//     env-tunable (CACHE_COHORT_MEMO_CAP, default 128); per-memo size
//     is dominated by the kept-name map (api-side), not by this store's
//     own bookkeeping.
//
// COHORT-COUNT-INDEPENDENCE (Ship 3 / 0.30.197)
//   The 0.30.178 store was a zero-bound sync.Map: a hot shared content
//   cell accumulated O(cohorts) memos, the last O(cohorts) memory
//   structure under a per-user-binding RBAC topology. Ship 3 re-bounds
//   it via an insertion-order cap (oldest-inserted evicted on overflow,
//   NOT strict-LRU — there is no recency tracking on Lookup, which is
//   why Lookup stays lock-free). Eviction is benign: a dropped memo →
//   next Lookup miss → caller re-derives via filterListByRBAC. The
//   per-request RBAC content gate runs regardless; the memo is a pure
//   cache over a deterministic function of (cohort, rbacGen, items), so
//   eviction can only cause recomputation, never a different result.
//
//   CACHE_COHORT_MEMO_CAP <= 0 means UNBOUNDED — the escape hatch back
//   to the 0.30.178 zero-bound behavior, honoring the provisional/
//   removable cache contract (project_caching_is_provisional).
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

// defaultCohortGateMemoCap is the per-ResolvedEntry insertion-order cap
// on the number of distinct cohorts memoized when CACHE_COHORT_MEMO_CAP
// is unset. 128 is well above the expected cohort cardinality (admin +
// cyberjoker + a handful of per-namespace cohorts; ~tens at today's
// scale) and keeps per-entry overhead bounded under a pathological
// per-user-binding RBAC topology where cohort count grows with users.
const defaultCohortGateMemoCap = 128

// CohortGateMemoStore is the per-ResolvedEntry container of cohort
// memos. The stored value type is `any` because the memo shape
// (keptNames + rbacGen + encoded bytes) lives in api/ — keeping cache/
// free of that import. Callers Lookup/Store via type assertion on the
// receiving end; the cap+eviction machinery here is value-shape-agnostic.
type CohortGateMemoStore struct {
	memos sync.Map // string (cohortKey) -> any (memo payload)

	// cap is the insertion-order eviction bound, read once in
	// NewCohortGateMemoStore from CACHE_COHORT_MEMO_CAP. cap <= 0 means
	// UNBOUNDED (zero-bound escape hatch). Immutable after construction.
	cap int

	mu    sync.Mutex
	order *list.List // front = newest-inserted. Element.Value is the cohortKey string.
	index map[string]*list.Element

	// size mirrors order.Len() but is readable without mu for the
	// quick "are we over cap?" check and the observability accessor.
	// Reconciled INSIDE mu on every mutation.
	size atomic.Int64

	// overflowTotal counts how often the cap+evict path fired.
	overflowTotal atomic.Uint64
}

// NewCohortGateMemoStore constructs an empty store. The eviction cap is
// resolved here once from CACHE_COHORT_MEMO_CAP (default 128; <= 0 =>
// unbounded), so tests can override it via t.Setenv before construction.
func NewCohortGateMemoStore() *CohortGateMemoStore {
	return &CohortGateMemoStore{
		cap:   intFromEnv("CACHE_COHORT_MEMO_CAP", defaultCohortGateMemoCap),
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
// fast path — sync.Map.Load. This is the load-bearing property of the
// store: there is NO lock and NO recency touch on Lookup, so a steady-
// state hit costs exactly one sync.Map.Load.
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

// Store records memo under cohortKey, evicting the oldest-INSERTED
// entry when the per-entry cap is exceeded (insertion-order eviction,
// NOT strict-LRU — there is no recency tracking, so Lookup stays
// lock-free). Returns true if an eviction fired.
//
// cap <= 0 disables eviction entirely (unbounded escape hatch).
//
// Store fires ONLY on a memo miss/stale (the caller Looks up first and
// only Stores on miss), so this mutex is never taken on a hit. The
// publication ordering is intentional:
//  1. LoadOrStore into sync.Map — readers can hit the new memo now.
//  2. PushFront + cap check under mu — eviction bookkeeping learns the
//     new entry.
//
// A concurrent Lookup between (1) and (2) returns the populated memo,
// which is correct (memos are immutable after Store). The window is
// bounded by the few atomics in step 2.
func (s *CohortGateMemoStore) Store(cohortKey string, memo any) bool {
	if s == nil || cohortKey == "" || memo == nil {
		return false
	}
	if _, loaded := s.memos.LoadOrStore(cohortKey, memo); loaded {
		// Replacement of an existing cohort key — overwrite atomically.
		// NO recency touch (insertion-order, not LRU). The memo is
		// immutable after Store per the file header contract, so a
		// concurrent reader observing the old vs new value both produce
		// correct served bytes (the rbacGen check on hit guards
		// staleness). No eviction can happen on a replacement.
		s.memos.Store(cohortKey, memo)
		return false
	}

	s.mu.Lock()
	el := s.order.PushFront(cohortKey)
	s.index[cohortKey] = el
	s.size.Add(1)

	evicted := false
	// cap <= 0 => unbounded; skip eviction entirely.
	for s.cap > 0 && s.size.Load() > int64(s.cap) {
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
// concurrent traffic. Decremented on eviction (Ship 3) so the
// bounded-growth invariant holds.
func (s *CohortGateMemoStore) Size() int64 {
	if s == nil {
		return 0
	}
	return s.size.Load()
}

// OverflowTotal returns the cumulative count of insertion-order
// evictions fired by the cap. Useful for ops correlation; published
// via snowplow_cohort_memo_overflow_total.
func (s *CohortGateMemoStore) OverflowTotal() uint64 {
	if s == nil {
		return 0
	}
	return s.overflowTotal.Load()
}
