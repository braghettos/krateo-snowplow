// prewarm_iter_serial_test.go — Ship 0.30.125 (F2) falsifier for the
// context-scoped serial inner-call parallelism (OOM mitigation 2).
//
// F2's SA content-population pass uncaps the `dependsOn.iterator` (the
// full per-namespace fan-out, #159 OOM territory). To hold peak RSS down
// it marks its resolve context with cache.WithPrewarmIterSerial, and
// iterParallelism MUST return 1 for that context — running the uncapped
// fan-out serially. A real /call carries no marker and keeps the
// GOMAXPROCS/env width: the narrowing is context-scoped, NOT a
// process-wide RESOLVER_ITER_PARALLELISM=1 that would slow every /call.

package api

import (
	"context"
	"runtime"
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// TestFAL_PrewarmIterSerial_ForcesParallelismOne proves a resolve context
// marked cache.WithPrewarmIterSerial yields iterParallelism == 1.
func TestFAL_PrewarmIterSerial_ForcesParallelismOne(t *testing.T) {
	serialCtx := cache.WithPrewarmIterSerial(context.Background())
	if got := iterParallelism(serialCtx); got != 1 {
		t.Fatalf("FAL (OOM mitigation 2): iterParallelism under WithPrewarmIterSerial "+
			"= %d, want 1 — the uncapped content-pass fan-out MUST run serially", got)
	}
}

// TestFAL_PrewarmIterSerial_RealCallUnaffected proves a context WITHOUT
// the marker — every real /call — keeps the default GOMAXPROCS-derived
// width. The serial narrowing must not leak onto request traffic.
func TestFAL_PrewarmIterSerial_RealCallUnaffected(t *testing.T) {
	t.Setenv("RESOLVER_ITER_PARALLELISM", "") // no env override
	want := runtime.GOMAXPROCS(0)
	if want > 32 {
		want = 32
	}
	if want < 1 {
		want = 1
	}
	if got := iterParallelism(context.Background()); got != want {
		t.Fatalf("FAL: a real /call (no PrewarmIterSerial marker) must keep the "+
			"default parallelism %d; got %d — the serial marker leaked", want, got)
	}
}
