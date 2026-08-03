// boot_resume_attempt_test.go — #135 F4b Lever B C3: the ctx-marker unit.
//
// WithBootResumeAttempt / BootResumeAttemptFromContext carry the boot scope's
// workqueue requeue count (NumRequeues) from processScope onto the boot scope
// ctx so rePrewarmBootScoped can distinguish pass 0 (attempt==0 → WALK) from an
// F.4 deadline-cut resume (attempt>0 → REUSE). Mirrors the WithSeedResolveMemo /
// WithSeedDeclinedExternalSet ctx-marker units in this package:
//   - round-trip (set N → read N),
//   - Disabled() → ctx unchanged → read 0 (inert, the cache-off posture),
//   - absent / nil ctx → 0 default (the safe "pass 0 — WALK" default: any path
//     that never installed the marker re-walks).

package cache

import (
	"context"
	"testing"
)

func TestBootResumeAttempt_RoundTripAndDefaults(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true") // not Disabled() → the install is live

	// ── round-trip: set N → read N (a few representative attempts incl. 0).
	for _, n := range []int{0, 1, 5, 42} {
		ctx := WithBootResumeAttempt(context.Background(), n)
		if got := BootResumeAttemptFromContext(ctx); got != n {
			t.Fatalf("round-trip: WithBootResumeAttempt(%d) → read %d; want %d", n, got, n)
		}
	}

	// ── absent marker on a live base ctx → 0 (the safe "pass 0 — WALK" default).
	if got := BootResumeAttemptFromContext(context.Background()); got != 0 {
		t.Fatalf("absent marker must read 0 (pass-0/WALK default); got %d", got)
	}

	// ── nil ctx → 0 (BootResumeAttemptFromContext is nil-safe).
	//nolint:staticcheck // SA1012: intentionally passing a nil ctx to prove nil-safety.
	if got := BootResumeAttemptFromContext(nil); got != 0 {
		t.Fatalf("nil ctx must read 0; got %d", got)
	}
	// WithBootResumeAttempt on a nil ctx must not panic and must return nil.
	//nolint:staticcheck // SA1012: intentionally passing a nil ctx to prove nil-safety.
	if ctx := WithBootResumeAttempt(nil, 3); ctx != nil {
		t.Fatalf("WithBootResumeAttempt(nil, 3) must return the nil ctx unchanged; got non-nil")
	}
}

// TestBootResumeAttempt_DisabledInert — Disabled() (CACHE_ENABLED=false) →
// WithBootResumeAttempt returns the ctx UNCHANGED, so no marker is installed and
// a read is the 0 default (byte-identical to pre-Lever-B). A cache-off process
// never runs the prewarm engine, so the marker is never consulted there.
func TestBootResumeAttempt_DisabledInert(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false")
	if !Disabled() {
		t.Fatal("premise broken: CACHE_ENABLED=false must make Disabled() true")
	}
	base := context.Background()
	ctx := WithBootResumeAttempt(base, 7)
	if ctx != base {
		t.Fatal("Disabled(): WithBootResumeAttempt must return the ctx UNCHANGED (inert install)")
	}
	if got := BootResumeAttemptFromContext(ctx); got != 0 {
		t.Fatalf("Disabled(): the marker must be inert → read 0; got %d", got)
	}
}
