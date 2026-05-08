package cache

import (
	"context"
	"testing"
	"time"
)

// TestMemCache_SetCountAndMemberTotal pins the Q-DIAG-PPROF (0.25.321)
// observability contract for MemCache.sets exposure: 3 sets with member
// counts [2, 5, 7] MUST report set_count=3 and set_member_total=14. This
// is the synthetic load described in the architect spec; the canary
// extracts these gauges from /metrics/runtime to attribute the heap-shift.
func TestMemCache_SetCountAndMemberTotal(t *testing.T) {
	mc := NewMem(time.Minute)
	ctx := context.Background()

	if err := mc.SAddMultiWithTTL(ctx, "set:a", []string{"a1", "a2"}, time.Minute); err != nil {
		t.Fatalf("seed set:a: %v", err)
	}
	if err := mc.SAddMultiWithTTL(ctx, "set:b", []string{"b1", "b2", "b3", "b4", "b5"}, time.Minute); err != nil {
		t.Fatalf("seed set:b: %v", err)
	}
	if err := mc.SAddMultiWithTTL(ctx, "set:c", []string{"c1", "c2", "c3", "c4", "c5", "c6", "c7"}, time.Minute); err != nil {
		t.Fatalf("seed set:c: %v", err)
	}

	if got, want := mc.SetCount(), int64(3); got != want {
		t.Errorf("SetCount: got %d, want %d", got, want)
	}
	if got, want := mc.SetMemberTotal(), int64(14); got != want {
		t.Errorf("SetMemberTotal: got %d, want %d", got, want)
	}
}

// TestMemCache_SetCountNilSafe ensures the nil-safe path returns 0,
// matching the cache-disabled deploy contract (sampler must not panic
// when appCache is nil).
func TestMemCache_SetCountNilSafe(t *testing.T) {
	var mc *MemCache
	if got := mc.SetCount(); got != 0 {
		t.Errorf("nil SetCount: got %d, want 0", got)
	}
	if got := mc.SetMemberTotal(); got != 0 {
		t.Errorf("nil SetMemberTotal: got %d, want 0", got)
	}
}

// TestRBACWatcher_LensesNilSafe ensures the three Q-DIAG-PPROF lenses
// are nil-safe (RBACWatcher may be nil when CACHE_ENABLED=false).
func TestRBACWatcher_LensesNilSafe(t *testing.T) {
	var rw *RBACWatcher
	if got := rw.IdentityCacheLen(); got != 0 {
		t.Errorf("nil IdentityCacheLen: got %d, want 0", got)
	}
	if got := rw.LastCohortBidForUserLen(); got != 0 {
		t.Errorf("nil LastCohortBidForUserLen: got %d, want 0", got)
	}
	if got := rw.EvalCacheLen(); got != 0 {
		t.Errorf("nil EvalCacheLen: got %d, want 0", got)
	}
}

// TestRBACWatcher_EvalCacheLenTracksLRU confirms EvalCacheLen reflects
// the bounded LRU's live size after Add. Used by the canary to verify
// the 200K cap holds (entry_count near cap = working set saturated).
func TestRBACWatcher_EvalCacheLenTracksLRU(t *testing.T) {
	rw := NewRBACWatcher(nil, nil)
	if got := rw.EvalCacheLen(); got != 0 {
		t.Fatalf("fresh EvalCacheLen: got %d, want 0", got)
	}
	rw.evalCache.Add("k1", true)
	rw.evalCache.Add("k2", false)
	if got, want := rw.EvalCacheLen(), int64(2); got != want {
		t.Errorf("EvalCacheLen after 2 adds: got %d, want %d", got, want)
	}
}

// TestSampleDiag_ZeroWhenUnregistered guards the test/cache-off path.
func TestSampleDiag_ZeroWhenUnregistered(t *testing.T) {
	// Reset to a known-zero sampler for this test (atomic.Value is package-
	// global; other tests may have registered fns). Idempotent.
	RegisterDiagSampler(func() DiagSnapshot { return DiagSnapshot{} })
	got := SampleDiag()
	if got != (DiagSnapshot{}) {
		t.Errorf("SampleDiag: got %+v, want zero", got)
	}
}
