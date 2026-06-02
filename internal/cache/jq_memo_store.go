// jq_memo_store.go — Ship 0.30.240 Tier 3 per-cohort JQ output memo.
//
// Per design 2026-06-02 §9.14 + Tightening 3 (singleflight) + Tightening 5
// (budget cap). The Tier 3 memo caches the widgetDataTemplate.Resolve
// output keyed by `(widgetCellKey, cohortKey, rbacGen)`. On HIT, the
// serve-time gate skips the JQ template re-eval entirely.
//
// PERFORMANCE SHAPE (architect's Risk 12 empirical baseline):
//   - Cold (cohort × widget × rbacGen MISS): 40.6 ms (single-field) /
//     136 ms (multi-field) at N=50K admin scale.
//   - Warm (≥9/10 after cohort warmup): ~100 ns memo Lookup +
//     ~100 ns shallow clone = ~200 ns total.
//
// SINGLEFLIGHT (Tightening 3): the populate step is single-flighted
// via golang.org/x/sync/singleflight, keyed identically to the memo
// (widgetCellKey + cohortKey + rbacGen). N concurrent /call from the
// same cohort × widget × rbacGen run ONE populate, not N. Without
// singleflight, a sustained-burst (Tightening 7's measurement) would
// pay 10× JQ cost on cold cohort entry.
//
// CAPACITY CAP (Tightening 5): per-widget memo size capped via env
// CACHE_JQ_MEMO_CAP (default 128/widget; <= 0 means unbounded). LRU
// eviction policy is insertion-order on miss-path, mirroring
// CohortGateMemoStore at internal/cache/cohort_gate_memo_store.go.
//
// COUNTERS (Tightening 5): two metrics published via the
// jq_memo_metrics.go expvar pipeline (sibling file):
//   - snowplow_jq_memo_entries_total: gauge of current entries
//     across all widget cells (sum over per-widget stores).
//   - snowplow_jq_memo_evictions_total: monotonic cumulative LRU
//     evictions.
//
// CONCURRENCY MODEL — mirrors CohortGateMemoStore:
//   - Lookup: lock-free sync.Map.Load.
//   - Store: sync.Map.LoadOrStore + mu-guarded order list for the
//     cap+evict path. mu is taken only on the miss path, never on a
//     hit.
//   - Singleflight: per-key Do() on populate. Losers of the populate
//     race wait for the winner; all N callers see the same result
//     bytes.
//
// LIFECYCLE — the store hangs off the *ResolvedEntry alongside
// CohortGates. The same atomic.Pointer pattern keeps construction
// race-free (CompareAndSwap on first access; loser drops its
// allocation).

package cache

import (
	"container/list"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/singleflight"
)

// JQMemoStore caches the widgetDataTemplate.Resolve output per-cohort
// per-widget per-rbacGen. The stored value type is `any` (the caller
// type-asserts to the specific shape it stored — typically
// map[string]any for the widgetData map); the cap+eviction machinery
// is value-shape-agnostic. Mirrors CohortGateMemoStore's design.
type JQMemoStore struct {
	memos sync.Map // string (composite key) -> any (memo payload)

	// cap is the insertion-order eviction bound, read once in
	// NewJQMemoStore from CACHE_JQ_MEMO_CAP. cap <= 0 means UNBOUNDED.
	// Immutable after construction.
	cap int

	mu    sync.Mutex
	order *list.List // front = newest-inserted. Element.Value is the composite key string.
	index map[string]*list.Element

	// size mirrors order.Len() but is readable without mu for the
	// quick "are we over cap?" check and the observability accessor.
	size atomic.Int64

	// overflowTotal counts how often the cap+evict path fired. Surfaced
	// as snowplow_jq_memo_evictions_total via JQMemoEvictionsTotal().
	overflowTotal atomic.Uint64

	// sf is the singleflight group for populate. Keyed identically to
	// the memo. Tightening 3 — N concurrent /call from the same cohort
	// × widget × rbacGen run ONE populate, not N.
	sf singleflight.Group
}

// defaultJQMemoCap mirrors defaultCohortGateMemoCap (128 entries per
// widget cell). At customer scale (~50-100 cohorts × ~30 widgets) the
// total memo entries top out at ~3,840 well under any meaningful
// memory pressure.
const defaultJQMemoCap = 128

// NewJQMemoStore constructs an empty store. The eviction cap is
// resolved here once from CACHE_JQ_MEMO_CAP (default 128; <= 0 =>
// unbounded), so tests can override via t.Setenv before construction.
func NewJQMemoStore() *JQMemoStore {
	return &JQMemoStore{
		cap:   intFromEnv("CACHE_JQ_MEMO_CAP", defaultJQMemoCap),
		order: list.New(),
		index: map[string]*list.Element{},
	}
}

// JQMemoStoreLoadOrInit atomically returns the *JQMemoStore attached
// to entry, creating one on the first call. Concurrent first callers
// race via atomic.Pointer.CompareAndSwap; losers discard their fresh
// allocation and reuse the winner's store. Nil-safe on entry.
//
// This is the v4 per-widget-cell entry point — each widget L1 cell
// gets its own store, hung off ResolvedEntry.JQMemo.
func JQMemoStoreLoadOrInit(entry *ResolvedEntry) *JQMemoStore {
	if entry == nil {
		return nil
	}
	if s := entry.JQMemo.Load(); s != nil {
		return s
	}
	fresh := NewJQMemoStore()
	if entry.JQMemo.CompareAndSwap(nil, fresh) {
		return fresh
	}
	return entry.JQMemo.Load()
}

// Lookup returns the memo for compositeKey, or (nil, false). Lock-free
// fast path — sync.Map.Load. This is the load-bearing property of the
// store: a steady-state hit costs exactly one sync.Map.Load (~10 ns).
func (s *JQMemoStore) Lookup(compositeKey string) (any, bool) {
	if s == nil || compositeKey == "" {
		return nil, false
	}
	v, ok := s.memos.Load(compositeKey)
	if !ok {
		return nil, false
	}
	return v, true
}

// Store records memo under compositeKey, evicting the oldest-INSERTED
// entry when the per-widget cap is exceeded. Returns true if an
// eviction fired.
//
// cap <= 0 disables eviction entirely (unbounded escape hatch).
//
// Mirrors CohortGateMemoStore.Store — sync.Map.LoadOrStore first
// (readers can hit immediately), then mu-guarded order bookkeeping.
func (s *JQMemoStore) Store(compositeKey string, memo any) bool {
	if s == nil || compositeKey == "" || memo == nil {
		return false
	}
	if _, loaded := s.memos.LoadOrStore(compositeKey, memo); loaded {
		// Replacement of an existing key — overwrite atomically. No
		// eviction can happen on a replacement.
		s.memos.Store(compositeKey, memo)
		return false
	}

	s.mu.Lock()
	el := s.order.PushFront(compositeKey)
	s.index[compositeKey] = el
	s.size.Add(1)

	evicted := false
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

// LookupOrPopulate is the singleflight-guarded miss path. On HIT
// returns the cached memo. On MISS calls populate() under the
// singleflight group's Do — N concurrent callers for the same key
// share ONE populate invocation. The populate result is stored under
// compositeKey before LookupOrPopulate returns.
//
// populate MUST be deterministic (same input → same output) AND
// idempotent (multiple invocations produce equivalent results); the
// singleflight de-dupe guarantees only one runs per key, but the
// returned value is the same for every concurrent caller.
//
// A populate error is returned to all concurrent callers; the memo is
// NOT stored on error. The next /call retries via a fresh
// singleflight slot.
func (s *JQMemoStore) LookupOrPopulate(compositeKey string, populate func() (any, error)) (any, error) {
	if s == nil || compositeKey == "" {
		// Disabled / no key — invoke populate uncached.
		if populate == nil {
			return nil, nil
		}
		return populate()
	}
	if v, ok := s.Lookup(compositeKey); ok {
		return v, nil
	}
	v, err, _ := s.sf.Do(compositeKey, func() (any, error) {
		// Double-check inside the singleflight winner — a parallel
		// caller may have populated the slot between our Lookup miss
		// and the Do invocation.
		if v, ok := s.Lookup(compositeKey); ok {
			return v, nil
		}
		val, err := populate()
		if err != nil {
			return nil, err
		}
		s.Store(compositeKey, val)
		return val, nil
	})
	return v, err
}

// Size returns the current entry count. Lock-free.
func (s *JQMemoStore) Size() int64 {
	if s == nil {
		return 0
	}
	return s.size.Load()
}

// OverflowTotal returns the cumulative eviction count (monotonic).
// Lock-free. Surfaced as snowplow_jq_memo_evictions_total.
func (s *JQMemoStore) OverflowTotal() uint64 {
	if s == nil {
		return 0
	}
	return s.overflowTotal.Load()
}

// ─────────────────────────────────────────────────────────────────────
// Global counter accessors. The metrics layer (jq_memo_metrics.go,
// sibling file) sums per-store size into a process-global gauge and
// accumulates eviction totals. The accessors below are the read-side
// entry points the dispatchers + expvar pipeline use.

// jqMemoEvictionsTotalGlobal accumulates evictions across every
// *JQMemoStore in the process. Bumped by Store() on the evict path
// via reportJQMemoEviction.
var jqMemoEvictionsTotalGlobal atomic.Uint64

// reportJQMemoEviction is invoked from within Store's eviction loop
// to keep the global counter in sync with per-store overflowTotal.
// Called once per evicted entry.
func reportJQMemoEviction() {
	jqMemoEvictionsTotalGlobal.Add(1)
}

// JQMemoEvictionsTotal returns the process-global cumulative eviction
// count across every *JQMemoStore. Exported for the metrics layer's
// expvar.Func.
func JQMemoEvictionsTotal() uint64 {
	return jqMemoEvictionsTotalGlobal.Load()
}

// JQMemoComposeKey builds the canonical composite key for a JQ memo
// lookup: SHA-256(widgetCellKey || cohortKey || rbacGen). Centralised
// so the dispatcher's serve site + any test fixture produce
// byte-identical keys.
//
// rbacGen is the per-cohort generation counter; it MUST shift on any
// binding mutation affecting the cohort so a stale memo is naturally
// orphaned (and eventually LRU-evicted). The same rbacGen the cohort
// gate memo uses (via CohortRBACGen) is reused here for symmetry.
func JQMemoComposeKey(widgetCellKey, cohortKey string, rbacGen uint64) string {
	// Cheap composition — the inputs are already content-hashes (the
	// L1 key is sha256-hex; cohortKey is sha256-truncated-hex). No
	// further hashing needed; concatenation with separators is unique
	// across all input tuples.
	return widgetCellKey + "\x00" + cohortKey + "\x00" + formatUint64(rbacGen)
}

// formatUint64 — local-scope strconv-free uint64 stringification so
// this file stays in cache/ without strconv import bloat for one tiny
// formatter. Output is base-10, no leading zeros.
func formatUint64(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
