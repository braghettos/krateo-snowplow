// admission_ceiling_test.go — the SHARED adaptive-headroom calc falsifiers
// (fold 2026-07-03, docs/prewarm-engine-implicit-on-cache-2026-07-03.md §3.2).
// Pins the pure calc + the seam + the transparent-when-unlimited posture that
// BOTH the seed bound and the nested bound rely on.
package cache_test

import (
	"math"
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// TestAdmissionCeiling_HeadroomMinusReserve pins ceiling = (limit − liveHeap) −
// limit/8 under injected deterministic seams.
func TestAdmissionCeiling_HeadroomMinusReserve(t *testing.T) {
	const limit = int64(8 * 1024 * 1024 * 1024) // 8 GiB
	const live = int64(1 * 1024 * 1024 * 1024)  // 1 GiB live heap
	restore := cache.SetAdmissionRuntimeSeamsForTest(
		func() int64 { return limit },
		func() int64 { return live },
	)
	t.Cleanup(restore)
	t.Cleanup(cache.ResetAdmissionRuntimeSeamsForTest)

	ceiling, unlimited := cache.AdmissionCeiling()
	if unlimited {
		t.Fatalf("expected bounded (limit set); got unlimited")
	}
	reserve := limit / 8
	want := (limit - live) - reserve
	if ceiling != want {
		t.Fatalf("ceiling = %d; want (limit-live)-limit/8 = %d", ceiling, want)
	}
}

// TestAdmissionCeiling_UnlimitedWhenGoMemLimitUnset pins the transparent posture:
// the runtime's unset default (math.MaxInt64) ⇒ unlimited==true, ceiling 0.
func TestAdmissionCeiling_UnlimitedWhenGoMemLimitUnset(t *testing.T) {
	restore := cache.SetAdmissionRuntimeSeamsForTest(
		func() int64 { return math.MaxInt64 },
		func() int64 { return 0 },
	)
	t.Cleanup(restore)
	t.Cleanup(cache.ResetAdmissionRuntimeSeamsForTest)

	ceiling, unlimited := cache.AdmissionCeiling()
	if !unlimited {
		t.Fatalf("GOMEMLIMIT unset (MaxInt64) must be unlimited (transparent); got ceiling=%d", ceiling)
	}
}

// TestAdmissionCeiling_NegativeHeadroomClampsToZero pins that when live heap
// already exceeds (limit − reserve), the ceiling clamps to 0 (never negative) —
// so a lone unit still admits via the caller's inFlightCount==0 escape, never
// via a negative ceiling arithmetic surprise.
func TestAdmissionCeiling_NegativeHeadroomClampsToZero(t *testing.T) {
	const limit = int64(2 * 1024 * 1024 * 1024)
	restore := cache.SetAdmissionRuntimeSeamsForTest(
		func() int64 { return limit },
		func() int64 { return limit }, // live == limit → headroom-reserve is negative
	)
	t.Cleanup(restore)
	t.Cleanup(cache.ResetAdmissionRuntimeSeamsForTest)

	ceiling, unlimited := cache.AdmissionCeiling()
	if unlimited {
		t.Fatalf("bounded limit must not report unlimited")
	}
	if ceiling != 0 {
		t.Fatalf("ceiling must clamp to 0 when live heap exhausts headroom; got %d", ceiling)
	}
}
