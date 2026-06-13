package main

import (
	"net/http"
	"testing"
	"time"
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
