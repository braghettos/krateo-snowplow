// informer_only_reads_test.go — #101 falsifier for cache.WithInformerOnlyReads.
//
// THE DEFECT (docs/refreshes-arming-endpoint-storm-trace-2026-07-04.md). The
// GET /refreshes arming path runs on a RefreshAuth-shaped ctx: UserInfo present,
// UserConfig DELIBERATELY absent. subscriptionKeyExtras calls objects.Get per
// coord; on an informer GET-miss the routed branch fell through to
// getFromAPIServer, which died INSTANTLY at the UserConfig read with a noisy
// "unable to get user endpoint" ERROR (378/nav storm) and the coord was
// fail-closed-skipped anyway.
//
// THE FIX: cache.WithInformerOnlyReads makes objects.Get return a NotFound-shaped
// Err on fallthrough WITHOUT calling getFromAPIServer — the skip stays, the log
// noise is gone. Serve set UNCHANGED (the fallthrough never succeeded here).
//
// Hermetic: reuses the newServeWatcher fixture (real fake dynamic client + synced
// informer + serveAdminRBACSeed) + a RefreshAuth-shaped ctx (ctxWithUser — UserInfo,
// no UserConfig). NO cluster, NO apiserver. The mutation (drop the marker) proves
// the noisy ERROR reappears (RED). -race clean.

package objects

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/snowplow/internal/cache"
)

// endpointErrFragment is the exact log message getFromAPIServer emits on the
// UserConfig read failure (get.go:178) — the storm line the fix silences.
const endpointErrFragment = "unable to get user endpoint"

// refreshAuthShapedCtx returns the ctx the /refreshes arming path runs under:
// UserInfo present (RefreshAuth attaches it), UserConfig DELIBERATELY absent
// (the route needs no <user>-clientconfig lookup), plus a log-capturing logger
// so the falsifier can assert the endpoint-error line is/ isn't emitted.
func refreshAuthShapedCtx(informerOnly bool) (context.Context, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	// LevelDebug so the fix's replacement Debug line is ALSO captured (we assert
	// the ERROR fragment is absent, and can see the quiet-suppression line).
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := xcontext.BuildContext(ctxWithUser(serveAdminUser), xcontext.WithLogger(logger))
	if informerOnly {
		ctx = cache.WithInformerOnlyReads(ctx)
	}
	return ctx, buf
}

// TestGet_InformerOnlyReads_GetMiss_QuietNotFound is the #101 GREEN arm: an
// informer GET-miss on a RefreshAuth-shaped (endpoint-less) ctx marked
// WithInformerOnlyReads returns a NotFound-shaped Err WITHOUT the noisy
// endpoint-error line and WITHOUT touching the apiserver fallthrough counter.
func TestGet_InformerOnlyReads_GetMiss_QuietNotFound(t *testing.T) {
	resetServeCounters()
	// GVR registered + synced, but the requested object is ABSENT → GET-miss →
	// routed-branch fallthrough (the storm coord).
	newServeWatcher(t, newServeTestObject("default", "alpha", "m"))

	ctx, logBuf := refreshAuthShapedCtx(true)
	res := Get(ctx, serveTestRef("default", "missing"))

	// (1) NotFound-shaped Err (the caller's fail-closed skip keys on Err != nil).
	if res.Err == nil {
		t.Fatalf("informer-only GET-miss: expected a NotFound-shaped Err; got nil (object served?)")
	}
	if res.Err.Code != 404 {
		t.Fatalf("informer-only GET-miss: expected 404-coded Err; got %d", res.Err.Code)
	}
	if res.Unstructured != nil {
		t.Fatalf("informer-only GET-miss: expected no object; got one")
	}
	// (2) GVR preserved for telemetry.
	if res.GVR != serveTestGVR {
		t.Fatalf("informer-only GET-miss: GVR want %v; got %v", serveTestGVR, res.GVR)
	}
	// (3) The noisy endpoint-error line is ABSENT (the whole point).
	if got := logBuf.String(); strings.Contains(got, endpointErrFragment) {
		t.Fatalf("informer-only GET-miss: expected ZERO %q log lines; got:\n%s", endpointErrFragment, got)
	}
	// (4) The apiserver fallthrough was NOT taken (no getFromAPIServer, so the
	// counter it bumps stays 0 — the read never reached the apiserver arm).
	if s := ObjectsGetStatsSnapshot(); s.ApiserverFallthrough != 0 {
		t.Fatalf("informer-only GET-miss: ApiserverFallthrough want 0 (suppressed); got %d", s.ApiserverFallthrough)
	}
}

// TestGet_InformerOnlyReads_Mutation_ErrorLineReappears is the #101 RED arm
// (mutation = drop the marker). The SAME fixture + endpoint-less ctx WITHOUT
// WithInformerOnlyReads falls through to getFromAPIServer and emits the noisy
// "unable to get user endpoint" ERROR — proving the GREEN arm's silence is
// caused by the marker, not by the fixture.
func TestGet_InformerOnlyReads_Mutation_ErrorLineReappears(t *testing.T) {
	resetServeCounters()
	newServeWatcher(t, newServeTestObject("default", "alpha", "m"))

	ctx, logBuf := refreshAuthShapedCtx(false) // MUTATION: marker dropped
	res := Get(ctx, serveTestRef("default", "missing"))

	// Without the marker the fallthrough runs: getFromAPIServer dies at the
	// UserConfig read → 401-shaped Err AND the noisy endpoint-error line.
	if res.Err == nil {
		t.Fatalf("mutation (no marker): expected the endpoint-read Err; got nil")
	}
	if got := logBuf.String(); !strings.Contains(got, endpointErrFragment) {
		t.Fatalf("mutation (no marker) NOT RED: expected the %q ERROR line to reappear on fallthrough; got:\n%s",
			endpointErrFragment, got)
	}
	// And the apiserver fallthrough counter DID move (the doomed arm ran).
	if s := ObjectsGetStatsSnapshot(); s.ApiserverFallthrough != 1 {
		t.Fatalf("mutation (no marker): ApiserverFallthrough want 1 (fallthrough taken); got %d", s.ApiserverFallthrough)
	}
}

// TestGet_InformerOnlyReads_HitStillServes proves the marker short-circuits ONLY
// the fallthrough: a genuine informer HIT still serves (serve set unchanged).
func TestGet_InformerOnlyReads_HitStillServes(t *testing.T) {
	resetServeCounters()
	rw := newServeWatcher(t, newServeTestObject("default", "alpha", "m"))
	waitForServeObject(t, rw, serveTestGVR, "default", "alpha")

	ctx, logBuf := refreshAuthShapedCtx(true)
	res := Get(ctx, serveTestRef("default", "alpha"))

	if res.Err != nil {
		t.Fatalf("informer-only HIT: expected the object served; got Err %v", res.Err)
	}
	if res.Unstructured == nil {
		t.Fatalf("informer-only HIT: expected a served object; got nil")
	}
	if s := ObjectsGetStatsSnapshot(); s.InformerServed != 1 {
		t.Fatalf("informer-only HIT: InformerServed want 1; got %d", s.InformerServed)
	}
	if got := logBuf.String(); strings.Contains(got, endpointErrFragment) {
		t.Fatalf("informer-only HIT: unexpected endpoint-error line on a served hit:\n%s", got)
	}
}
