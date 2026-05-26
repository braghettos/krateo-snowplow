// cohort_gate_memo_store.go — Ship GMC / 0.30.174, A.2 / 0.30.178.
//
// The per-ResolvedEntry storage primitive for the cohort gate memo.
// Lookup is lock-free (sync.Map.Load); Store is also lock-free
// (sync.Map.LoadOrStore) — no LRU machinery, zero-bound storage per
// Diego's 2026-05-26 ratification (Ship A.2 / 0.30.178).
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
//
// ZERO-BOUND STORAGE (Ship A.2 / 0.30.178)
//   The 0.30.174 GMC store used a `container/list`-backed LRU at 256
//   entries per ResolvedEntry. Diego ratified zero-bound storage on
//   2026-05-26: cohort cardinality is empirically tiny (admin +
//   cyberjoker + a handful of per-namespace cohorts; ~tens, not
//   hundreds), so the bookkeeping cost outweighs the bound. The store
//   is now just a sync.Map; per-entry GC reclaims everything when the
//   ResolvedEntry itself is LRU-evicted by the resolved-cache store
//   (resolved.go:778, removeElementLocked).
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
	"sync"
	"sync/atomic"
)

// CohortGateMemoStore is the per-ResolvedEntry container of cohort
// memos. The stored value type is `any` because the memo shape
// (keptNames + rbacGen + encoded bytes) lives in api/ — keeping cache/
// free of that import. Callers Lookup/Store via type assertion on the
// receiving end.
//
// Ship A.2 / 0.30.178 — the store is unbounded: cohort cardinality is
// empirically tiny and eviction rides the parent ResolvedEntry's LRU
// in the resolved-cache store. No per-store LRU machinery.
type CohortGateMemoStore struct {
	memos sync.Map // string (cohortKey) -> any (memo payload)

	// size is the count of distinct cohort keys currently in memos.
	// Bumped on first-time Store (new key); decremented on no path
	// (zero-bound — there is no per-store eviction). Exposed via Size()
	// for observability (snowplow_cohort_memo_entries_total expvar).
	size atomic.Int64
}

// NewCohortGateMemoStore constructs an empty store.
func NewCohortGateMemoStore() *CohortGateMemoStore {
	return &CohortGateMemoStore{}
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

// Store records memo under cohortKey. Lock-free — sync.Map.LoadOrStore.
// Replacement (same cohortKey stored twice) does NOT bump size. Ship A.2:
// no eviction, no LRU touch — zero-bound storage.
//
// Returns false unconditionally (no eviction path). Kept the boolean
// return so callers compiled against the 0.30.174 signature don't break.
func (s *CohortGateMemoStore) Store(cohortKey string, memo any) bool {
	if s == nil || cohortKey == "" || memo == nil {
		return false
	}
	if _, loaded := s.memos.LoadOrStore(cohortKey, memo); !loaded {
		s.size.Add(1)
		return false
	}
	// Replacement (key already present) — overwrite atomically. The
	// memo is immutable after Store per the file header contract, so a
	// concurrent reader observing the old vs new value both produce
	// correct served bytes (the rbacGen check on hit guards staleness).
	s.memos.Store(cohortKey, memo)
	return false
}

// Size returns the current number of memo entries. Safe under
// concurrent traffic.
func (s *CohortGateMemoStore) Size() int64 {
	if s == nil {
		return 0
	}
	return s.size.Load()
}
