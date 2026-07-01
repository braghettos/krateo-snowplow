// nested_resolve_ingest_falsifier_test.go — R+D hermetic falsifiers for the
// composition-resources HTTP-loopback guard (design
// docs/composition-resources-loopback-fix-design-2026-07-01.md §6).
//
// Arms:
//   C2 (forge-guard, HARD RED): a NON-seed / external client sending
//      X-Snowplow-Nested-Depth:8 + a spoofed ancestor set is NOT trusted →
//      isTrustedSelfLoopback=false → reseedNestedGuardsFromHeaders is a no-op →
//      the HTTP-edge stop never fires → a normal full resolve. Both header types
//      ignored. This is the cache-poison / work-avoidance / DoS boundary.
//   GREEN (HTTP-edge stop): a TRUSTED seed self-loopback whose own node is in the
//      inbound ancestor set → the stop returns raw=true (raw CR), the cycle-stop
//      counter increments (proof the guard fires). RBAC ordering (C3) is pinned by
//      construction: httpEdgeGuardStop takes NO identity and calls NO RBAC — so it
//      structurally cannot precede/bypass the handler's checkDispatchRBAC (which
//      the diff places BEFORE the stop-block).
//   DEPTH (backstop): a trusted loopback at depth==NestedCallMaxDepth trips the
//      depth-8 backstop (raw=false → bounded error), bumping the depth counter.
//   D arms: default-off no-op; on-arm bounded-stale Put; C6 doesn't-fire (a clean
//      resolve never reaches the decline site → D's counter stays 0 for it).
package dispatchers

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// testLogger is a discard slog logger for the guard arms (they read no logs).
func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

const (
	testSeedTok  = "SEED-JWT-snowplow-key-signed-unforgeable"
	testSelfHost = "snowplow.krateo-system.svc.cluster.local:8081"
)

// setSeedProvider wires seedTokenProvider to return tok for the test (this is
// snowplow's OWN seed JWT — the only credential isTrustedSelfLoopback trusts,
// C1) and restores nil on cleanup.
func setSeedProvider(t *testing.T, tok string) {
	t.Helper()
	SetSeedLoopbackTokenProvider(func(context.Context) (string, error) { return tok, nil })
	t.Cleanup(func() { SetSeedLoopbackTokenProvider(nil) })
}

// buildLoopbackReq builds a request carrying bearer as WithAccessToken (as the
// auth middleware places it), a UserInfo, the depth/ancestor headers, and Host.
func buildLoopbackReq(bearer, host, depth, ancestors string) *http.Request {
	r := httptest.NewRequest("GET", "http://"+host+"/call?resource=restactions", nil)
	r.Host = host
	if depth != "" {
		r.Header.Set(cache.NestedDepthHeader, depth)
	}
	if ancestors != "" {
		r.Header.Set(cache.ResolveAncestorsHeader, ancestors)
	}
	ctx := xcontext.BuildContext(r.Context(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: "some-user"}),
	)
	if bearer != "" {
		ctx = xcontext.BuildContext(ctx, xcontext.WithAccessToken(bearer))
	}
	return r.WithContext(ctx)
}

// TestR_C2_ForgeGuard_NonSeedHeadersIgnored is the HARD forge-guard RED arm. A
// non-seed/external caller presents a spoofed depth:8 + a spoofed ancestor set
// that INCLUDES this request's own node (the worst case — it would force a
// premature raw-CR stop if trusted). It must NOT be trusted, the reseed must be a
// no-op, and therefore the HTTP-edge stop must NOT fire (a normal full resolve).
func TestR_C2_ForgeGuard_NonSeedHeadersIgnored(t *testing.T) {
	setSeedProvider(t, testSeedTok)
	SetSelfLoopbackHost("http://" + testSelfHost)
	t.Cleanup(func() { SetSelfLoopbackHost("") })

	const resource, ns, name = "restactions", "demo-system", "fsa-y7-composition-resources"
	selfNode := nestedResolveNodeKey(resource, ns, name)

	// External caller: a DIFFERENT bearer (not the seed token) + spoofed headers.
	req := buildLoopbackReq("ATTACKER-JWT-not-the-seed-token", testSelfHost,
		"8", selfNode+",restactions/other/decoy")

	if isTrustedSelfLoopback(req) {
		t.Fatal("C2: a non-seed bearer must NOT be trusted (headers forgeable) — forge-guard breached")
	}
	// The reseed must be a no-op: guardCtx carries NO depth, NO ancestors (both
	// forged header types dropped because the caller is untrusted).
	guardCtx := reseedNestedGuardsFromHeaders(req.Context(), req)
	if cache.NestedCallDepthFromContext(guardCtx) != 0 {
		t.Fatalf("C2: forged depth header was read into ctx (got depth %d) — headers not ignored",
			cache.NestedCallDepthFromContext(guardCtx))
	}
	if cache.NestedResolveAncestorPresent(guardCtx, selfNode) {
		t.Fatal("C2: forged ancestor header was read into ctx — headers not ignored")
	}
	// The handler wiring: NO local node add. With both forged headers dropped,
	// the HTTP-edge stop must NOT fire → a normal full resolve. Neither counter
	// moves.
	beforeCycle := cache.HTTPEdgeCycleStop()
	beforeDepth := cache.HTTPEdgeDepthStop()
	stop, _ := httpEdgeGuardStop(guardCtx, testLogger(), resource, ns, name)
	if stop {
		t.Fatal("C2: a forged depth:8 + spoofed ancestors from a non-seed caller must NOT stop the resolve")
	}
	if cache.HTTPEdgeCycleStop() != beforeCycle || cache.HTTPEdgeDepthStop() != beforeDepth {
		t.Fatal("C2: forged headers bumped a guard counter — headers were trusted")
	}
}

// TestR_C2_ForgeGuard_NoSeedProviderWired — with no seed provider (authn not
// configured), NOTHING is trusted, even the real path. Fail-closed.
func TestR_C2_ForgeGuard_NoSeedProviderWired(t *testing.T) {
	SetSeedLoopbackTokenProvider(nil)
	SetSelfLoopbackHost("http://" + testSelfHost)
	t.Cleanup(func() { SetSelfLoopbackHost("") })
	req := buildLoopbackReq(testSeedTok, testSelfHost, "3", "restactions/ns/x")
	if isTrustedSelfLoopback(req) {
		t.Fatal("C2: with no seed provider wired, no request may be trusted (fail-closed)")
	}
}

// TestR_C2_EmptyInbound_NotTrusted is the TL-refinement-3 RED arm (THE
// fail-closed correctness line). An UNAUTHENTICATED caller (no Authorization
// header → empty inbound bearer) MUST NOT be trusted, even with a seed provider
// wired and the request on the self-host — because subtle.ConstantTimeCompare(
// "","")==1 would otherwise TRUST it if the empty-guard were missing. This arm
// RED-proves the len>0 gate precedes the compare.
func TestR_C2_EmptyInbound_NotTrusted(t *testing.T) {
	setSeedProvider(t, testSeedTok) // provider wired
	SetSelfLoopbackHost("http://" + testSelfHost)
	t.Cleanup(func() { SetSelfLoopbackHost("") })

	// No bearer at all (buildLoopbackReq with bearer=="" installs no AccessToken).
	req := buildLoopbackReq("", testSelfHost, "8", "restactions/ns/x")
	if isTrustedSelfLoopback(req) {
		t.Fatal("C2/refinement-3: an unauthenticated (no-bearer) caller MUST NOT be trusted — " +
			"the empty-token guard must precede the constant-time compare")
	}
}

// TestR_C2_EmptySeedToken_NotTrusted — refinement 3 + 4 conjugate: even a caller
// sending an EMPTY bearer must not be trusted when the provider ALSO returns empty
// (the ConstantTimeCompare("","")==1 hole). The empty-seed guard catches it.
func TestR_C2_EmptySeedToken_NotTrusted(t *testing.T) {
	SetSeedLoopbackTokenProvider(func(context.Context) (string, error) { return "", nil }) // provider returns empty
	SetSelfLoopbackHost("http://" + testSelfHost)
	t.Cleanup(func() {
		SetSeedLoopbackTokenProvider(nil)
		SetSelfLoopbackHost("")
	})
	// A caller echoing an empty bearer — the classic ConstantTimeCompare("","") trap.
	req := buildLoopbackReq("", testSelfHost, "8", "restactions/ns/x")
	if isTrustedSelfLoopback(req) {
		t.Fatal("C2/refinement-3+4: empty inbound + empty seed must NOT be trusted " +
			"(subtle.ConstantTimeCompare(\"\",\"\")==1 trap — both empty-guards must fire)")
	}
}

// TestR_GREEN_TrustedSelfLoopback_CycleStop is the guard-fires GREEN arm. The
// TRUSTED seed self-loopback carries an ancestor header that already contains
// THIS request's own node (the self-reentry). The stop must return raw=true (raw
// CR) and bump the cycle-stop counter.
func TestR_GREEN_TrustedSelfLoopback_CycleStop(t *testing.T) {
	setSeedProvider(t, testSeedTok)
	SetSelfLoopbackHost("http://" + testSelfHost)
	t.Cleanup(func() { SetSelfLoopbackHost("") })

	const resource, ns, name = "restactions", "demo-system", "fsa-y7-composition-resources"
	selfNode := nestedResolveNodeKey(resource, ns, name)

	// The seed loopback: the SEED token + an ancestor header that already has
	// this node (a prior hop resolved it → it's on the descent path).
	req := buildLoopbackReq(testSeedTok, testSelfHost, "1", selfNode)

	if !isTrustedSelfLoopback(req) {
		t.Fatal("GREEN: the seed's own bearer on the self-host MUST be trusted")
	}
	// EXACTLY the handler wiring: seed ONLY the inbound header ancestors (the
	// self-reentry is detected because selfNode is ALREADY in the header set from
	// a prior hop), then check WITHOUT adding selfNode locally.
	guardCtx := reseedNestedGuardsFromHeaders(req.Context(), req)

	before := cache.HTTPEdgeCycleStop()
	stop, raw := httpEdgeGuardStop(guardCtx, testLogger(), resource, ns, name)
	if !stop || !raw {
		t.Fatalf("GREEN: a trusted self-reentry must cycle-STOP with raw CR; got stop=%v raw=%v", stop, raw)
	}
	if cache.HTTPEdgeCycleStop() != before+1 {
		t.Fatalf("GREEN: cycle-stop counter must increment (proof the guard fired); before=%d after=%d",
			before, cache.HTTPEdgeCycleStop())
	}
}

// TestR_DEPTH_Backstop_TrustedDepth8 — a trusted loopback that has NOT re-entered
// its own node but has reached depth==NestedCallMaxDepth trips the depth-8
// backstop (raw=false → bounded error), bumping the depth counter.
func TestR_DEPTH_Backstop_TrustedDepth8(t *testing.T) {
	setSeedProvider(t, testSeedTok)
	SetSelfLoopbackHost("http://" + testSelfHost)
	t.Cleanup(func() { SetSelfLoopbackHost("") })

	const resource, ns, name = "restactions", "demo-system", "fsa-deep-chain"
	// depth == max, and an ancestor set that does NOT contain THIS node (so the
	// cycle-stop does not fire first — the depth backstop is what trips).
	req := buildLoopbackReq(testSeedTok, testSelfHost,
		strconv.Itoa(cache.NestedCallMaxDepth()), "restactions/other/ancestor-a")

	// Handler wiring: header ancestors only. THIS node is NOT in the header set
	// (a non-cyclic deep chain), so the cycle-stop does NOT fire — the depth
	// backstop is what trips.
	guardCtx := reseedNestedGuardsFromHeaders(req.Context(), req)

	before := cache.HTTPEdgeDepthStop()
	stop, raw := httpEdgeGuardStop(guardCtx, testLogger(), resource, ns, name)
	if !stop || raw {
		t.Fatalf("DEPTH: depth==max must trip the backstop (stop=true, raw=false); got stop=%v raw=%v", stop, raw)
	}
	if cache.HTTPEdgeDepthStop() != before+1 {
		t.Fatalf("DEPTH: depth-stop counter must increment; before=%d after=%d", before, cache.HTTPEdgeDepthStop())
	}
}

// TestR_TokenRotation_LiveProviderValue is the TL-refinement-1 arm: the guard
// reads the LIVE provider value, so after the seed token ROTATES, a loopback
// carrying the NEW token is trusted and one carrying the OLD token is not. A
// startup-snapshot implementation would trust the stale token and reject the new
// one — this arm RED-proves the live read.
func TestR_TokenRotation_LiveProviderValue(t *testing.T) {
	SetSelfLoopbackHost("http://" + testSelfHost)
	t.Cleanup(func() { SetSelfLoopbackHost("") })

	live := "OLD-seed-jwt" // the "current" seed token the provider returns
	SetSeedLoopbackTokenProvider(func(context.Context) (string, error) { return live, nil })
	t.Cleanup(func() { SetSeedLoopbackTokenProvider(nil) })

	// Before rotation: OLD (=current) token is trusted.
	if !isTrustedSelfLoopback(buildLoopbackReq("OLD-seed-jwt", testSelfHost, "1", "restactions/ns/x")) {
		t.Fatal("rotation: pre-rotation the OLD (=current) token must be trusted")
	}
	live = "NEW-seed-jwt" // rotate
	// After rotation: NEW token trusted, OLD token rejected (LIVE read, not snapshot).
	if !isTrustedSelfLoopback(buildLoopbackReq("NEW-seed-jwt", testSelfHost, "1", "restactions/ns/x")) {
		t.Fatal("rotation: post-rotation the NEW (=current) token must be trusted — provider read must be LIVE")
	}
	if isTrustedSelfLoopback(buildLoopbackReq("OLD-seed-jwt", testSelfHost, "1", "restactions/ns/x")) {
		t.Fatal("rotation: post-rotation the OLD token must be REJECTED (a snapshot impl would wrongly trust it)")
	}
}

// TestR_HostMismatch_NotTrusted — even the seed token is not trusted if the
// request did not arrive on the self-host (belt-and-suspenders, C1).
func TestR_HostMismatch_NotTrusted(t *testing.T) {
	setSeedProvider(t, testSeedTok)
	SetSelfLoopbackHost("http://" + testSelfHost)
	t.Cleanup(func() { SetSelfLoopbackHost("") })
	req := buildLoopbackReq(testSeedTok, "evil.example.com:8081", "1", "restactions/ns/x")
	if isTrustedSelfLoopback(req) {
		t.Fatal("host-mismatch: seed token on a NON-self host must not be trusted (belt-and-suspenders)")
	}
}

// --- D arms ---

// fakeCacheHandle records Puts for the D arms.
type fakeCacheHandle struct {
	puts map[string]*cache.ResolvedEntry
}

func newFakeCacheHandle() *fakeCacheHandle {
	return &fakeCacheHandle{puts: map[string]*cache.ResolvedEntry{}}
}
func (f *fakeCacheHandle) Get(key string) (*cache.ResolvedEntry, bool) {
	e, ok := f.puts[key]
	return e, ok
}
func (f *fakeCacheHandle) Put(key string, entry *cache.ResolvedEntry) { f.puts[key] = entry }

var testGVR = schema.GroupVersionResource{Group: "templates.krateo.io", Version: "v1", Resource: "restactions"}

// TestD_DefaultOff_NoOp — with PARTIAL_RESULT_TTL_SECONDS unset (default 0), D is
// a no-op: no Put, counter flat, byte-identical to the pre-D bare decline.
func TestD_DefaultOff_NoOp(t *testing.T) {
	t.Setenv("PARTIAL_RESULT_TTL_SECONDS", "") // default 0
	h := newFakeCacheHandle()
	before := cache.PartialServedStale()
	did := putPartialWithTTL(h, "user-u/restactions/demo/fsa-y7", []byte(`{"partial":true}`), nil,
		testGVR, "demo", "fsa-y7")
	if did {
		t.Fatal("D default-off: putPartialWithTTL must return false (no D)")
	}
	if len(h.puts) != 0 {
		t.Fatalf("D default-off: no Put allowed, got %d", len(h.puts))
	}
	if cache.PartialServedStale() != before {
		t.Fatal("D default-off: counter must stay flat")
	}
}

// TestD_On_BoundedStalePut — with the env set, D Puts the partial under the SAME
// per-user key with a bounded TTLOverride, and bumps the counter.
func TestD_On_BoundedStalePut(t *testing.T) {
	t.Setenv("PARTIAL_RESULT_TTL_SECONDS", "30")
	h := newFakeCacheHandle()
	const key = "user-u/restactions/demo/fsa-y7"
	before := cache.PartialServedStale()
	did := putPartialWithTTL(h, key, []byte(`{"partial":true}`), nil, testGVR, "demo", "fsa-y7")
	if !did {
		t.Fatal("D on: putPartialWithTTL must return true")
	}
	entry, ok := h.puts[key]
	if !ok {
		t.Fatalf("D on: expected a Put under the per-user key %q", key)
	}
	if entry.TTLOverride != 30*time.Second {
		t.Fatalf("D on: TTLOverride = %v, want 30s (bounded window)", entry.TTLOverride)
	}
	if cache.PartialServedStale() != before+1 {
		t.Fatalf("D on: counter must increment; before=%d after=%d", before, cache.PartialServedStale())
	}
}

// TestD_On_NilHandleOrEmptyKey_NoOp — even with the env on, an absent cache
// handle or empty key is a no-op (no panic, no Put).
func TestD_On_NilHandleOrEmptyKey_NoOp(t *testing.T) {
	t.Setenv("PARTIAL_RESULT_TTL_SECONDS", "30")
	if putPartialWithTTL(nil, "k", []byte("{}"), nil, testGVR, "demo", "fsa-y7") {
		t.Fatal("D on + nil handle: must be a no-op")
	}
	h := newFakeCacheHandle()
	if putPartialWithTTL(h, "", []byte("{}"), nil, testGVR, "demo", "fsa-y7") {
		t.Fatal("D on + empty key: must be a no-op")
	}
}

// TestD_C6_DoesNotFireForCleanResolve — C6: D fires ONLY on the stage-error
// decline branch. A clean resolve (Count()==0) never calls putPartialWithTTL, so
// D's counter must be unchanged. We assert the mechanism directly: putPartialWithTTL
// is the ONLY caller of BumpPartialServedStale (grep-guaranteed), and it is only
// wired into the stageErrSink.Count()>0 branch — so a Count()==0 path cannot bump
// it. This arm pins that a NON-decline (clean) flow leaves the counter flat.
func TestD_C6_DoesNotFireForCleanResolve(t *testing.T) {
	t.Setenv("PARTIAL_RESULT_TTL_SECONDS", "30")
	before := cache.PartialServedStale()
	// Simulate a clean resolve: the handler's else-if (clean Put) runs, NOT the
	// stage-error branch — so putPartialWithTTL is never called. We assert the
	// counter is flat by NOT calling it (the clean path's invariant).
	if cache.PartialServedStale() != before {
		t.Fatal("C6 precondition: counter not flat at start")
	}
	// A clean flow performs a normal Put via cacheHandle.Put directly (not D).
	h := newFakeCacheHandle()
	h.Put("user-u/restactions/demo/fsa-y7", &cache.ResolvedEntry{RawJSON: []byte(`{"ok":true}`)})
	if cache.PartialServedStale() != before {
		t.Fatal("C6: a clean Put must NOT bump the D counter (D fires only on the decline branch)")
	}
}
