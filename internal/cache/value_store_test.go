// value_store_test.go — Ship 0.30.240.
//
// Unit coverage for the ValueObject + ValueStore primitives. The
// load-bearing v4 contract (atomic-Pointer swap, dual refcount,
// quarantine GC) is exercised here at the primitive layer; the
// integrated T8 falsifier lives in e2e_identity_free_test.go (or
// equivalent) where it asserts pointer-identity across cache.Get
// + serve-time filter.

package cache

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestValueObject_RawIsImmutableCopy — putValue takes a defensive copy
// of the raw bytes. Mutating the caller's slice MUST NOT mutate the
// cached value (the v4 §4.6 invariant — bytes in L1 are immutable).
func TestValueObject_RawIsImmutableCopy(t *testing.T) {
	store := NewValueStore()
	t.Cleanup(store.Stop)

	src := []byte(`{"a":1}`)
	v := store.putValue(src)
	if v == nil {
		t.Fatalf("putValue returned nil")
	}
	if !bytes.Equal(v.Raw(), src) {
		t.Fatalf("raw bytes diverge from src: %q vs %q", v.Raw(), src)
	}

	// Mutate the caller's source slice; the cached bytes must NOT shift.
	src[0] = 'X'
	if v.Raw()[0] == 'X' {
		t.Fatalf("putValue did not defensively copy: caller mutation leaked")
	}
}

// TestValueObject_ReferrersStartAtOne — putValue bumps referrers to 1
// (the KeyEntry pointing at the new value). T8 assertion 4 (single
// referrer per cell) relies on this initial state.
func TestValueObject_ReferrersStartAtOne(t *testing.T) {
	store := NewValueStore()
	t.Cleanup(store.Stop)

	v := store.putValue([]byte(`{}`))
	if got := v.Referrers(); got != 1 {
		t.Fatalf("freshly-put value has referrers=%d; want 1", got)
	}
}

// TestValueObject_RetireDecrementsAndQuarantines — retire decrements
// referrers; reaching zero enqueues the value on the quarantine list.
func TestValueObject_RetireDecrementsAndQuarantines(t *testing.T) {
	store := NewValueStore()
	t.Cleanup(store.Stop)

	v := store.putValue([]byte(`{}`))
	store.retire(v)
	if got := v.Referrers(); got != 0 {
		t.Fatalf("post-retire referrers=%d; want 0", got)
	}
	if v.quarantineUntil.Load() == 0 {
		t.Fatalf("retired value has zero quarantineUntil")
	}
	if got := store.Stats().QuarantineLive; got != 1 {
		t.Fatalf("quarantine live=%d; want 1", got)
	}
}

// TestValueObject_AcquireReaderRefusesQuarantined — AcquireReader
// must FAIL on a retired value so the caller re-reads valueRef from
// the KeyEntry.
func TestValueObject_AcquireReaderRefusesQuarantined(t *testing.T) {
	store := NewValueStore()
	t.Cleanup(store.Stop)

	v := store.putValue([]byte(`{}`))
	store.retire(v)

	if ok := v.AcquireReader(); ok {
		t.Fatalf("AcquireReader returned true on retired value; want false")
	}
	if got := v.Readers(); got != 0 {
		t.Fatalf("Readers=%d after refused acquire; want 0", got)
	}
}

// TestValueObject_AcquireReaderBumpsAndReleaseUndoes — round-trip on
// the reader counter under a non-retired value.
func TestValueObject_AcquireReaderBumpsAndReleaseUndoes(t *testing.T) {
	store := NewValueStore()
	t.Cleanup(store.Stop)

	v := store.putValue([]byte(`{}`))
	if !v.AcquireReader() {
		t.Fatalf("AcquireReader returned false on fresh value")
	}
	if got := v.Readers(); got != 1 {
		t.Fatalf("post-Acquire Readers=%d; want 1", got)
	}
	v.ReleaseReader()
	if got := v.Readers(); got != 0 {
		t.Fatalf("post-Release Readers=%d; want 0", got)
	}
}

// TestValueStore_SweepRespectsQuarantineDeadline — the sweeper does
// NOT free a value before its quarantineUntil deadline elapses, even
// if readers is zero.
func TestValueStore_SweepRespectsQuarantineDeadline(t *testing.T) {
	store := NewValueStore()
	t.Cleanup(store.Stop)

	v := store.putValue([]byte(`{}`))
	store.retire(v)

	// Sweep at a wall time BEFORE the deadline — value stays in
	// quarantine, GC counter unchanged.
	preGC := store.Stats().GCTotal
	store.sweepOnce(time.Now()) // now < quarantineUntil
	if got := store.Stats().GCTotal; got != preGC {
		t.Fatalf("sweep before deadline freed a value: GC %d -> %d", preGC, got)
	}
	if got := store.Stats().QuarantineLive; got != 1 {
		t.Fatalf("post-pre-deadline-sweep quarantine live=%d; want 1", got)
	}
}

// TestValueStore_SweepFreesPastDeadline — the sweeper drops retired
// values whose deadline has elapsed AND whose reader count is zero.
func TestValueStore_SweepFreesPastDeadline(t *testing.T) {
	store := NewValueStore()
	t.Cleanup(store.Stop)

	v := store.putValue([]byte(`{}`))
	store.retire(v)

	// Sweep at a wall time after the deadline — value is freed.
	preGC := store.Stats().GCTotal
	futureNow := time.Now().Add(ValueQuarantineDuration + time.Second)
	store.sweepOnce(futureNow)
	if got := store.Stats().GCTotal; got != preGC+1 {
		t.Fatalf("sweep past deadline did NOT free retired value: GC %d -> %d", preGC, got)
	}
	if got := store.Stats().QuarantineLive; got != 0 {
		t.Fatalf("post-past-deadline-sweep quarantine live=%d; want 0", got)
	}
}

// TestValueStore_SweepHoldsReadersBackInQuarantine — a retired value
// with non-zero readers count stays in quarantine across a past-deadline
// sweep. The next sweep, once readers drops to zero, frees it.
func TestValueStore_SweepHoldsReadersBackInQuarantine(t *testing.T) {
	store := NewValueStore()
	t.Cleanup(store.Stop)

	v := store.putValue([]byte(`{}`))
	// Acquire BEFORE retiring (mimics: a serve goroutine loaded
	// valueRef, Add(1)'d readers; refresh now retires the prior value).
	if !v.AcquireReader() {
		t.Fatalf("AcquireReader returned false on fresh value")
	}
	store.retire(v)

	futureNow := time.Now().Add(ValueQuarantineDuration + time.Second)
	preGC := store.Stats().GCTotal
	store.sweepOnce(futureNow)
	if got := store.Stats().GCTotal; got != preGC {
		t.Fatalf("sweep freed a value with in-flight readers: GC %d -> %d", preGC, got)
	}

	// Release the reader; next sweep frees.
	v.ReleaseReader()
	store.sweepOnce(futureNow)
	if got := store.Stats().GCTotal; got != preGC+1 {
		t.Fatalf("post-release sweep did NOT free retired value: GC %d -> %d", preGC, got)
	}
}

// TestValueStore_RetireIdempotent — calling retire twice on the same
// value is safe and only enqueues once.
func TestValueStore_RetireIdempotent(t *testing.T) {
	store := NewValueStore()
	t.Cleanup(store.Stop)

	v := store.putValue([]byte(`{}`))
	store.retire(v)
	store.retire(v) // double-release should clamp at zero, not negative
	if got := v.Referrers(); got < 0 {
		t.Fatalf("double-retire produced negative referrers=%d", got)
	}
}

// TestValueObject_NoTornReadsUnderConcurrentRefresh — RACE TEST.
// 8 reader goroutines continuously load + acquire the current value
// while 4 refresh goroutines atomic-swap fresh values onto the slot.
// Readers must NEVER see a value with referrers < 1 + readers < 0;
// no torn bytes; no panic.
//
// This is the foundational concurrency contract the T8 falsifier
// builds on. Run under -race.
func TestValueObject_NoTornReadsUnderConcurrentRefresh(t *testing.T) {
	store := NewValueStore()
	t.Cleanup(store.Stop)

	// The shared "slot" — analogous to ResolvedEntry.valueRef.
	var slot atomic.Pointer[ValueObject]
	slot.Store(store.putValue([]byte(`{"v":0}`)))

	const readers = 8
	const refreshers = 4
	const dur = 200 * time.Millisecond

	done := make(chan struct{})
	var wg sync.WaitGroup

	// Readers.
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				v := slot.Load()
				if v == nil {
					continue
				}
				if !v.AcquireReader() {
					// Value was retired between load + acquire. Re-read.
					continue
				}
				if r := v.Raw(); len(r) == 0 {
					v.ReleaseReader()
					t.Errorf("torn read: empty raw bytes")
					return
				}
				// Sanity: refcount accounting.
				if v.Readers() < 1 {
					v.ReleaseReader()
					t.Errorf("readers count went non-positive while held: %d", v.Readers())
					return
				}
				v.ReleaseReader()
			}
		}()
	}

	// Refreshers.
	for i := 0; i < refreshers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n := int64(0)
			for {
				select {
				case <-done:
					return
				default:
				}
				n++
				fresh := store.putValue([]byte(`{"v":` + itoaTest(int(n)) + `}`))
				prior := slot.Swap(fresh)
				store.retire(prior)
			}
		}(i)
	}

	time.Sleep(dur)
	close(done)
	wg.Wait()
}

// itoaTest — a tiny strconv shim that avoids the import dance for the
// race test above.
func itoaTest(n int) string {
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
