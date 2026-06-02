// value_store.go — Ship 0.30.240 (design 2026-06-02 §6).
//
// The v4 identity-free L1 splits the cache structure into two layers:
//
//   1. KeyEntry — per L1-key metadata cell. Stable across refreshes;
//      carries Inputs, CreatedAt, Pinned, CohortGates, and a single
//      atomic.Pointer[ValueObject] that flips on refresh.
//   2. ValueObject — content-addressed immutable bytes + per-cohort JQ
//      memo + dual-atomic reference counts. Quarantined for 10s after
//      reaching zero referrers + zero readers before final GC.
//
// In this codebase the existing `ResolvedEntry` already plays the
// KeyEntry role (it carries Inputs / CreatedAt / Pinned / CohortGates
// + the cached RawJSON/Items). The v4 wiring is INCREMENTAL:
//
//   - ResolvedEntry gains a `valueRef atomic.Pointer[ValueObject]`
//     populated alongside the existing RawJSON / Items fields on
//     Put. Existing readers that access entry.RawJSON / entry.Items
//     directly continue to work — Put still produces an entry whose
//     fields and ValueObject are byte-equal by construction.
//   - The refresher's atomic.Swap path goes through valueRef so the
//     T8 falsifier's pointer-identity contract (design §8.2 assertions
//     1-8) is satisfiable.
//   - Future ships migrate read sites from entry.RawJSON to
//     entry.LoadValue() incrementally.
//
// CONCURRENCY CONTRACT (load-bearing):
//
//   - ValueObject.raw is IMMUTABLE. Once putValue produces a *ValueObject,
//     its bytes are frozen. Any v4 serve path that mutates the cached
//     bytes violates §4.6 — the falsifier (T8 assertion 5/6/7/8) FAILs.
//   - Refresh path: putValue(newBytes) → store.atomic.Swap on KeyEntry.
//     The OLD *ValueObject's referrers count decrements; quarantine
//     timer starts when both counters hit 0.
//   - Serve path: load valueRef under no lock; immediately Add(1) to
//     readers; defer Add(-1) before return. The dual-counter design
//     prevents the quarantine sweeper from freeing a ValueObject whose
//     bytes a goroutine is still serving over the wire.
//
// REFCOUNT INVARIANTS (design §6):
//
//   - referrers ≥ 0 always. Bumped to 1 by putValue; decremented to 0
//     by atomic.Swap on the KeyEntry. Never goes negative.
//   - readers ≥ 0 always. Bumped to N by N concurrent serves; decremented
//     by N as those serves return. Never goes negative.
//   - GC eligibility: referrers == 0 AND readers == 0. Then a 10-second
//     quarantine timer must elapse before final free. The quarantine
//     guards the reader-pin race (Risk 9): a reader that loaded
//     valueRef just before refresh's Swap had its Add(1) race with the
//     sweeper's read-of-readers; the 10s window is generous enough that
//     any in-flight serve completes before its ValueObject is freed.
//
// LRU INTEGRATION (this ship):
//
//   - ValueObject is independent of LRU eviction. The KeyEntry is what
//     the LRU evicts; on eviction we call entry.releaseValue() which
//     decrements the referrers count and enqueues the ValueObject for
//     quarantine GC.
//   - The store-level byte budget still counts entry.RawJSON length
//     (entryBytes in resolved.go). Future ship may shift accounting to
//     ValueObject.raw; not required for this ship.

package cache

import (
	"sync"
	"sync/atomic"
	"time"
)

// ValueObject is the immutable content cell — the v4 second layer of
// the L1 cache. Constructed exclusively by ValueStore.Put.
//
// FIELDS:
//
//   - raw: the encoded JSON output. Immutable. Readers may safely
//     reference this slice's backing array across goroutines without
//     mutex.
//   - createdAt: wall time at first putValue, for diagnostics.
//   - referrers / readers: dual-atomic reference counts. See file
//     header for invariants.
//   - quarantineUntil: zero when not in quarantine; otherwise the
//     wall-time deadline at which the sweeper may free this object.
//     Set atomically when both counters reach zero.
//
// THREAD SAFETY: all reads/writes go through atomics or the methods
// below. No external locking required.
type ValueObject struct {
	raw       []byte
	createdAt time.Time

	referrers atomic.Int64
	readers   atomic.Int64

	// quarantineUntil — UnixNano of the GC eligibility deadline.
	// Zero means not in quarantine. Bumped by retire(); read by the
	// sweeper.
	quarantineUntil atomic.Int64
}

// Raw returns the immutable encoded bytes. Callers MUST NOT mutate
// the returned slice — doing so violates §4.6 and trips T8 falsifier
// assertions 5/6/7/8.
func (v *ValueObject) Raw() []byte {
	if v == nil {
		return nil
	}
	return v.raw
}

// CreatedAt returns the wall time the ValueObject was constructed.
func (v *ValueObject) CreatedAt() time.Time {
	if v == nil {
		return time.Time{}
	}
	return v.createdAt
}

// Referrers returns the current referrer count (cache-cell pointers
// holding this ValueObject). For diagnostics + T8 falsifier assertion 4.
func (v *ValueObject) Referrers() int64 {
	if v == nil {
		return 0
	}
	return v.referrers.Load()
}

// Readers returns the current reader count (in-flight serve goroutines
// holding this ValueObject). For diagnostics + the quarantine GC gate.
func (v *ValueObject) Readers() int64 {
	if v == nil {
		return 0
	}
	return v.readers.Load()
}

// AcquireReader bumps the reader count. MUST be paired with a deferred
// ReleaseReader in the same goroutine that holds the bytes.
//
// Returns false if the ValueObject is in quarantine (already retired);
// the caller MUST re-read valueRef from the KeyEntry in that case —
// the cell was refreshed mid-load and the prior bytes are no longer
// canonical. (Practically: such a race is rare and the resulting
// re-load is a single atomic.Pointer.Load.)
func (v *ValueObject) AcquireReader() bool {
	if v == nil {
		return false
	}
	// CAS-style fast path: the quarantine flag is the LSB of
	// quarantineUntil. If non-zero, the object is retired.
	if v.quarantineUntil.Load() != 0 {
		return false
	}
	v.readers.Add(1)
	// Re-check AFTER bump (the sweeper may have retired between our
	// initial check and the Add). If retired, undo and tell caller to
	// retry.
	if v.quarantineUntil.Load() != 0 {
		v.readers.Add(-1)
		return false
	}
	return true
}

// ReleaseReader decrements the reader count. Paired with AcquireReader.
func (v *ValueObject) ReleaseReader() {
	if v == nil {
		return
	}
	v.readers.Add(-1)
}

// retire marks the ValueObject for quarantine GC. Called by the
// KeyEntry's Swap path when the referrer count hits zero. Idempotent —
// re-calling retire on an already-retired object is a no-op.
//
// The quarantine deadline is now+ValueQuarantineDuration; the sweeper
// runs every ValueQuarantineSweepInterval and frees retired objects
// whose deadline has elapsed AND whose reader count is zero.
func (v *ValueObject) retire() {
	if v == nil {
		return
	}
	deadline := time.Now().Add(ValueQuarantineDuration).UnixNano()
	v.quarantineUntil.CompareAndSwap(0, deadline)
}

// ValueQuarantineDuration is the minimum wall time between a
// ValueObject's retirement (zero referrers) and its eligibility for
// final GC. Per design §6: 10s. Generous against the reader-pin race
// (Risk 9): an in-flight serve goroutine that loaded valueRef just
// before Swap will complete its work-and-Release well within 10s.
//
// Adjustable via env CACHE_VALUE_QUARANTINE_SECONDS for diagnostic
// runs; default 10. A 0/negative value collapses the quarantine to a
// single sweep cycle (test-only; production MUST run with the default).
var ValueQuarantineDuration = 10 * time.Second

// ValueQuarantineSweepInterval is the period of the background sweeper
// goroutine. Picked at 2s — long enough to keep CPU overhead negligible,
// short enough that GC pressure from retired ValueObjects unwinds
// promptly under churn. The sweeper is started ONCE per process via
// startValueQuarantineSweeperOnce when the first ValueStore is created.
var ValueQuarantineSweepInterval = 2 * time.Second

// ─────────────────────────────────────────────────────────────────────
// ValueStore — content-addressed ValueObject registry + quarantine
// sweeper.
//
// The store doesn't dedupe by content hash in this ship (design §6 left
// dedup as a future optimisation; the LOC budget here is bounded by
// what's strictly load-bearing for the §4.6 contract). Each putValue
// produces a fresh *ValueObject; refcounts and quarantine still apply
// so the T8 falsifier's identity-pointer invariant + Risk 9 mitigation
// both hold.
//
// A future ship can layer dedup on top: key the store by sha256(raw),
// LoadOrStore via sync.Map, increment referrers on dedupe hit. The
// public API (putValue/retire) stays the same.

// ValueStore manages ValueObject lifecycle: construction, quarantine,
// GC. Singleton-per-process — accessed via DefaultValueStore.
//
// LOCK ORDERING: ValueStore.mu MUST NOT be held when calling any
// ResolvedCacheStore method (would risk lock inversion). The store's
// quarantine list is the only state — pure-additive sweeper loop.
type ValueStore struct {
	mu sync.Mutex

	// quarantine — singly-linked list of retired ValueObjects awaiting
	// final GC. The sweeper goroutine walks this list every
	// ValueQuarantineSweepInterval and drops entries whose deadline is
	// past AND whose reader count is zero.
	quarantine []*ValueObject

	// startedSweeper guards the once-only sweeper goroutine start.
	startedSweeper atomic.Bool

	// stopCh closes when the store is shut down (test cleanup only).
	stopCh chan struct{}

	// Falsifier counters (atomic; surfaced via /debug/vars).
	putTotal       atomic.Uint64
	retireTotal    atomic.Uint64
	gcTotal        atomic.Uint64
	quarantineLive atomic.Int64 // current quarantine list length
}

// NewValueStore constructs a fresh ValueStore. Production callers go
// through DefaultValueStore.
func NewValueStore() *ValueStore {
	return &ValueStore{
		stopCh: make(chan struct{}),
	}
}

// putValue constructs a new *ValueObject for raw and bumps its
// referrers to 1 (the KeyEntry pointing at it). Idempotent semantics
// per call — there's no content-dedup in this ship.
func (s *ValueStore) putValue(raw []byte) *ValueObject {
	if s == nil {
		return nil
	}
	// Defensive copy — the v4 contract requires the cached bytes be
	// immutable, and a caller passing a buffer they later mutate would
	// violate §4.6 silently. Allocating once at putValue is cheap (the
	// bytes are encoded JSON, typically <1 MiB; the alloc happens on
	// refresh, well off the hot serve path).
	frozen := make([]byte, len(raw))
	copy(frozen, raw)

	v := &ValueObject{
		raw:       frozen,
		createdAt: time.Now(),
	}
	v.referrers.Store(1)

	s.putTotal.Add(1)
	s.startSweeperOnce()
	return v
}

// retire marks v for quarantine GC. Decrements referrers; if it hits
// zero, retires the object and enqueues it on the quarantine list.
// Called by KeyEntry.swapValue when the prior valueRef is overwritten,
// and by KeyEntry.releaseValue when the cell itself is evicted.
//
// Safe to call with a nil v (no-op) or a nil store (no-op).
func (s *ValueStore) retire(v *ValueObject) {
	if s == nil || v == nil {
		return
	}
	remaining := v.referrers.Add(-1)
	if remaining > 0 {
		return
	}
	if remaining < 0 {
		// Defensive — should never happen. A negative referrers count
		// means double-release. Clamp at 0 and skip retirement.
		v.referrers.Store(0)
		return
	}
	v.retire()
	s.retireTotal.Add(1)

	s.mu.Lock()
	s.quarantine = append(s.quarantine, v)
	s.quarantineLive.Store(int64(len(s.quarantine)))
	s.mu.Unlock()
}

// startSweeperOnce starts the background quarantine sweeper goroutine
// at most once per ValueStore. The sweeper runs until Stop is called
// (test cleanup) or the process exits.
func (s *ValueStore) startSweeperOnce() {
	if s == nil {
		return
	}
	if !s.startedSweeper.CompareAndSwap(false, true) {
		return
	}
	go s.sweepLoop()
}

// sweepLoop runs the quarantine sweeper. Every
// ValueQuarantineSweepInterval, walks the quarantine list, drops
// every entry whose quarantineUntil has elapsed AND whose readers
// count is zero. Entries whose readers are non-zero stay in the list
// for the next pass (the in-flight serve will eventually release and
// the next sweep will free them).
func (s *ValueStore) sweepLoop() {
	t := time.NewTicker(ValueQuarantineSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-t.C:
			s.sweepOnce(time.Now())
		}
	}
}

// sweepOnce performs a single GC pass over the quarantine list at
// wall-time `now`. Test code can drive this directly with a synthetic
// `now` to bypass the timer.
func (s *ValueStore) sweepOnce(now time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.quarantine) == 0 {
		return
	}
	nowNS := now.UnixNano()
	kept := s.quarantine[:0]
	for _, v := range s.quarantine {
		deadline := v.quarantineUntil.Load()
		if deadline == 0 {
			// Resurrected? Should not happen under the v4 contract,
			// but if it did we drop from quarantine without freeing.
			continue
		}
		if deadline > nowNS {
			kept = append(kept, v)
			continue
		}
		if v.readers.Load() > 0 {
			// Still in-flight. Hold for the next sweep.
			kept = append(kept, v)
			continue
		}
		// Eligible. Drop the slice reference; the Go GC reclaims
		// when nothing else references it. (The KeyEntry's
		// valueRef was swapped to nil-or-new long ago at retire
		// time; the only remaining reference was this slice slot.)
		s.gcTotal.Add(1)
	}
	s.quarantine = kept
	s.quarantineLive.Store(int64(len(s.quarantine)))
}

// Stop closes the sweeper loop. Test-only — production has no shutdown
// path. Idempotent.
func (s *ValueStore) Stop() {
	if s == nil {
		return
	}
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
	}
}

// Stats returns a snapshot of the falsifier counters. Numbers are
// atomic and may drift between fields by a single call.
type ValueStoreStats struct {
	PutTotal       uint64
	RetireTotal    uint64
	GCTotal        uint64
	QuarantineLive int64
}

// Stats returns the current store counters.
func (s *ValueStore) Stats() ValueStoreStats {
	if s == nil {
		return ValueStoreStats{}
	}
	return ValueStoreStats{
		PutTotal:       s.putTotal.Load(),
		RetireTotal:    s.retireTotal.Load(),
		GCTotal:        s.gcTotal.Load(),
		QuarantineLive: s.quarantineLive.Load(),
	}
}

// ─────────────────────────────────────────────────────────────────────
// Default singleton.

var (
	defaultValueStore     *ValueStore
	defaultValueStoreOnce sync.Once
)

// DefaultValueStore returns the process-singleton ValueStore. Lazily
// constructed on first call.
func DefaultValueStore() *ValueStore {
	defaultValueStoreOnce.Do(func() {
		defaultValueStore = NewValueStore()
	})
	return defaultValueStore
}

// ResetDefaultValueStoreForTest discards the singleton + stops the
// sweeper. Test-only — production never touches this.
func ResetDefaultValueStoreForTest() {
	if defaultValueStore != nil {
		defaultValueStore.Stop()
	}
	defaultValueStore = NewValueStore()
}
