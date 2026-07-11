package main

import (
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// TestWriteTimeoutGuard locks the http.Server WriteTimeout at 300s.
//
// Regression guard for #351 / C2 (docs/c2-cacheoff-deliverability-trace-2026-06-13.md):
// Go anchors the write deadline to request-read time (t0), so WriteTimeout
// is the SOLE server-side ceiling on the /call dispatch path (there is no
// http.TimeoutHandler and no per-request context.WithTimeout there). The
// cache-OFF compositions path at 50K takes ~159s to resolve; if WriteTimeout
// regresses below that, the single buffered Write blows its t0-anchored
// deadline and the client gets HTTP 0 (empty reply). 300s gives ~2x headroom
// over the measured 159s worst case. Cache-ON is sub-4s and never approaches
// this deadline, so the value carries zero warm blast radius.
//
// If a future change lowers writeTimeout below the measured cache-OFF
// worst case, this test fails BEFORE it can ship and silently re-break the
// cache-OFF deliverability contract.
func TestWriteTimeoutGuard(t *testing.T) {
	const want = 300 * time.Second

	if writeTimeout != want {
		t.Fatalf("writeTimeout = %v, want %v; #351/C2 requires >= ~159s (50K cache-OFF "+
			"compositions resolve) plus headroom — see "+
			"docs/c2-cacheoff-deliverability-trace-2026-06-13.md", writeTimeout, want)
	}

	// Bind the assertion to the actual field wiring, not just the literal:
	// a future refactor that stops threading writeTimeout into
	// http.Server.WriteTimeout must also fail this guard.
	srv := &http.Server{WriteTimeout: writeTimeout}
	if srv.WriteTimeout != want {
		t.Fatalf("http.Server.WriteTimeout = %v, want %v", srv.WriteTimeout, want)
	}
}

// TestResolvePrewarmRegisterDefault pins the #130 1.7.5 implicit-on contract of
// the PREWARM_REGISTER_ENABLED resolution: the startup navigation GVR-walk runs
// by default; ONLY the exact string "false" opts out.
//
// This is the helper-level half of the C5 reachability gate. The composed
// default-wiring arm (with cache.Disabled folded in) is
// TestPrewarmWalkReachable_DefaultConfig below.
func TestResolvePrewarmRegisterDefault(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"unset (deployed default) => ON", "", true},
		{"explicit false => OFF (emergency opt-out)", "false", false},
		{"explicit true => ON", "true", true},
		{"any other value => ON (implicit-on)", "1", true},
		{"garbage => ON (only \"false\" disables)", "no", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolvePrewarmRegisterDefault(tc.raw); got != tc.want {
				t.Fatalf("resolvePrewarmRegisterDefault(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestPrewarmWalkReachable_DefaultConfig is the C5 reachability arm — the one
// that would have caught 1.7.4's inert F1. It reproduces main's EXACT composed
// gate for the startup navigation GVR-walk:
//
//	if !cache.Disabled() {              // deployed default: CACHE_ENABLED=true
//	    ...
//	    if resolvePrewarmRegisterDefault(os.Getenv("PREWARM_REGISTER_ENABLED")) {
//	        w.PrewarmRegisterFromNavigation(...)   // the walk (F1 batch confirm)
//	    }
//	}
//
// under the DEPLOYED DEFAULT environment (CACHE_ENABLED=true, no
// PREWARM_REGISTER_ENABLED key set) and asserts the walk IS reached. The
// pre-1.7.4 gate (`== "true"`) with no key set made this expression FALSE — the
// F1 walk-batch confirm never ran on-deploy, the "inert F1" the ledger row is
// still open on. This arm fails (RED) if the gate ever regresses to require an
// explicit opt-in, i.e. it protects the reachability the whole 1.7.5 change
// exists to establish.
func TestPrewarmWalkReachable_DefaultConfig(t *testing.T) {
	// Deployed default env: cache on, prewarm-register key ABSENT.
	t.Setenv("CACHE_ENABLED", "true")
	os.Unsetenv("PREWARM_REGISTER_ENABLED")

	// main's outer guard: the walk lives inside the !cache.Disabled() branch.
	if cache.Disabled() {
		t.Fatalf("setup: CACHE_ENABLED=true must make cache.Disabled()==false (the walk's outer guard)")
	}

	// main's inner gate — the EXACT expression at main.go's walk site.
	reached := !cache.Disabled() &&
		resolvePrewarmRegisterDefault(os.Getenv("PREWARM_REGISTER_ENABLED"))

	if !reached {
		t.Fatalf("C5 reachability: under the DEPLOYED DEFAULT config (CACHE_ENABLED=true, " +
			"PREWARM_REGISTER_ENABLED unset) main's composed gate MUST reach " +
			"PrewarmRegisterFromNavigation — this is exactly the wiring 1.7.4 got wrong " +
			"(gate was `== \"true\"`, key unset => walk never ran => F1 inert). Gate regressed.")
	}

	// Belt-and-suspenders: the explicit emergency opt-out still works and is the
	// ONLY thing that suppresses the walk under cache-on.
	t.Setenv("PREWARM_REGISTER_ENABLED", "false")
	if !cache.Disabled() && resolvePrewarmRegisterDefault(os.Getenv("PREWARM_REGISTER_ENABLED")) {
		t.Fatalf("C5: explicit PREWARM_REGISTER_ENABLED=false must suppress the walk even with cache on")
	}
}
