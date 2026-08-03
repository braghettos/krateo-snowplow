// external_touched_sink_test.go — falsifiers for the external-touched sink
// (proposal 2026-06-22, external half, falsifier (f)).
//
// The sink is shared across a multi-stage resolve's iterator errgroup workers
// — converting it from a private copy to a shared reference is a concurrency
// change (feedback_shared_vs_copy_is_a_concurrency_change), so the load-bearing
// falsifier is the concurrent -race test below. Run with -race -count=1.

package cache

import (
	"context"
	"sync"
	"testing"
)

// TestExternalTouchedSink_NilSafe — a nil sink reads Count()==0 and Bump is a
// no-op (the request path with no sink installed). Proves the
// nil-receiver-safe contract every Put site relies on.
func TestExternalTouchedSink_NilSafe(t *testing.T) {
	var s *ExternalTouchedSink
	if got := s.Count(); got != 0 {
		t.Fatalf("nil sink Count() = %d, want 0", got)
	}
	s.Bump() // must not panic
	if got := s.Count(); got != 0 {
		t.Fatalf("nil sink Count() after Bump = %d, want 0", got)
	}
	// FromContext on a bare context returns nil (no sink installed).
	if got := ExternalTouchedSinkFromContext(context.Background()); got != nil {
		t.Fatalf("FromContext(bare ctx) = %v, want nil", got)
	}
}

// TestExternalTouchedSink_BumpCounts — a single bump flips Count()>0; the Put
// gate's exact predicate.
func TestExternalTouchedSink_BumpCounts(t *testing.T) {
	ctx, s := WithExternalTouchedSink(context.Background())
	if s.Count() != 0 {
		t.Fatalf("fresh sink Count() = %d, want 0", s.Count())
	}
	// The bump site reads the sink off ctx, exactly as resolve.go does.
	ExternalTouchedSinkFromContext(ctx).Bump()
	if s.Count() != 1 {
		t.Fatalf("after one bump Count() = %d, want 1", s.Count())
	}
	if ExternalTouchedSinkFromContext(ctx).Count() <= 0 {
		t.Fatalf("Put-gate predicate Count()>0 did not fire after a bump")
	}
}

// TestExternalTouchedSink_ConcurrentBump_Race — falsifier (f). The sink is
// bumped from N concurrent goroutines (the errgroup workers of a multi-stage
// external resolve). Run under -race: any unsynchronised access on the
// atomic.Int64 fails here. The final count must equal the number of bumps
// (atomicity), and the Put-gate predicate must be true.
func TestExternalTouchedSink_ConcurrentBump_Race(t *testing.T) {
	ctx, s := WithExternalTouchedSink(context.Background())
	const workers = 64
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			// Each worker reads the sink off the shared ctx and bumps it +
			// concurrently reads Count() (the Put-gate read), exercising both
			// the writer and reader paths under -race.
			sink := ExternalTouchedSinkFromContext(ctx)
			sink.Bump()
			_ = sink.Count()
		}()
	}
	wg.Wait()
	if got := s.Count(); got != workers {
		t.Fatalf("concurrent Count() = %d, want %d (lost a bump → not atomic)", got, workers)
	}
}
