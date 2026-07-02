// nested_resolve_ingest.go — R (composition-resources loopback guard), the
// INGEST half. The emit half lives in the api resolver
// (resolve.go, the self-loopback bearer arm): it attaches
// X-Snowplow-Nested-Depth + X-Snowplow-Resolve-Ancestors to the outbound
// self-host /call. This file re-seeds those guards onto the inbound request ctx
// AT THE HTTP EDGE — but ONLY for a TRUSTED self-loopback — and then applies an
// HTTP-edge twin of the in-process #79 cycle-stop / depth-8 backstop
// (nested_call.go:114,169).
//
// WHY (RCA §0): the depth-8 counter (nested_call_depth.go) and the #79
// ancestor-set (nested_resolve_ancestor.go) are context.Value seams consumed
// only INSIDE the in-process ResolveNestedCall. They do NOT cross an HTTP hop.
// A composition-resources RA whose allCompositionResources managed set includes
// ITSELF dispatches a `/call?resource=restactions&name=<self>` loopback to
// snowplow's own self-host; that re-enters restActionHandler.ServeHTTP as a
// FRESH request carrying no depth / no ancestor set, so neither guard fires →
// unbounded HTTP self-recursion (the 3121-hop storm → context-canceled →
// decline-to-cache → never warms). R propagates the guards across the boundary.
//
// SECURITY (C1, the load-bearing line): the two headers are ADVISORY and
// SELF-TRUSTED ONLY. isTrustedSelfLoopback gates whether they are read into ctx
// at all. The trust check uses TOKEN PROVENANCE — the inbound bearer must EXACTLY
// equal snowplow's OWN authn-issued seed JWT (currentSeedLoopbackToken) — which
// is minted from snowplow's projected SA token (authn /serviceaccount/login,
// phase1_walk.go) and never leaves the pod except on a self-host loopback, so an
// EXTERNAL caller cannot present it. This is strictly stronger than matching an
// authn-issued USERNAME (which is authn-config-dependent / UNPINNED — memory
// project_prewarm_authn_loopback_identity_shift; a username match would need a
// hardcoded subject, violating feedback_no_special_cases). A req.Host==self
// check is belt-and-suspenders; the cryptographic token match is the primary
// gate. Any request failing the token match has BOTH headers IGNORED (never read
// into ctx) → it gets a NORMAL full resolve (C2 — a forged
// X-Snowplow-Nested-Depth:8 + spoofed ancestor set from a non-seed caller cannot
// force a premature raw-CR stop or suppress a legitimate resolve).
//
// BLAST-RADIUS BOUND: even a theoretical forge could only make snowplow return
// THIS ONE call's raw CR (resolve:false) or a bounded depth-stop — it cannot read
// cross-user data (the resolve still runs checkDispatchRBAC under the CALLER's
// identity, restactions.go:96, UNCHANGED by R) and cannot poison another user's
// L1 entry (per-user key). So the forge blast radius is self-DoS of the forger's
// own call — and the token gate makes even that unreachable for non-seed callers.
package dispatchers

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/snowplow/internal/cache"
)

// selfLoopbackHost is the URL_SELF host (Hostname, no port/scheme) parsed once at
// startup by SetSelfLoopbackHost (main.go). Empty when URL_SELF is unset — then
// the belt-and-suspenders host check is skipped and the token match alone gates
// (the token match is the primary and sufficient guard; the host check only ever
// tightens, never loosens). atomic so the startup Store and per-request Load are
// race-free without a mutex, mirroring api.selfHost.
var selfLoopbackHost atomic.Pointer[string]

// SetSelfLoopbackHost parses rawURL (URL_SELF) and installs its host for the
// ingest belt-and-suspenders check. An empty/unparseable rawURL clears it (host
// check disabled). Wired once at startup from main.go alongside
// api.SetSelfHost(URL_SELF), so both the emit-side and ingest-side self-host
// derive from the SAME URL_SELF value.
func SetSelfLoopbackHost(rawURL string) {
	if rawURL == "" {
		selfLoopbackHost.Store(nil)
		return
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		selfLoopbackHost.Store(nil)
		return
	}
	h := u.Hostname()
	selfLoopbackHost.Store(&h)
}

// requestArrivedOnSelfHost reports whether req arrived on the configured
// self-host. Belt-and-suspenders ONLY (C1: the token match is primary). Returns
// true when no self-host is configured — it must never TIGHTEN beyond the token
// gate when unconfigured (the token match still stands as the sole gate). req.Host
// may carry a port; we compare the hostname component only.
func requestArrivedOnSelfHost(req *http.Request) bool {
	sh := selfLoopbackHost.Load()
	if sh == nil {
		return true // unconfigured — defer entirely to the token match
	}
	host := req.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h // req.Host carried a port — compare the hostname component
	}
	return host == *sh
}

// isTrustedSelfLoopback is the C1 forge-guard. TRUE iff the inbound bearer
// EXACTLY equals snowplow's own current authn-issued seed JWT (token provenance)
// AND (belt-and-suspenders) the request arrived on the self-host. Only the seed
// possesses snowplow's SA-minted authn JWT → unforgeable by an external caller.
// For ANY request that fails EITHER condition the depth/ancestor headers are
// ignored (the caller re-seeds NOTHING → a normal full resolve).
//
// TL refinement 1 — LIVE provider value: currentSeedLoopbackToken reads the LIVE
// seedTokenProvider (authn.Client.Token), NOT a startup snapshot, so a ROTATED
// seed token still matches. The provider caches the JWT until authn.Client's
// refreshSkew (60s) before expiry, so an in-flight loopback and this read return
// the SAME cached string — no rotation gap in practice; the depth-8 backstop
// covers any theoretical edge miss during re-exchange.
//
// TL refinement 2 — CONSTANT-TIME compare: the bearer is a secret at a trust
// boundary, so the equality uses subtle.ConstantTimeCompare (no early-exit
// length/byte-position timing oracle a remote attacker could use to recover the
// token byte-by-byte). ConstantTimeCompare returns 1 on equal, 0 otherwise, and
// is itself length-safe (returns 0 for unequal lengths without leaking where).
//
// TL refinement 3 — EMPTY-TOKEN GUARD *BEFORE* the compare (THE fail-closed
// correctness line): subtle.ConstantTimeCompare([]byte(""), []byte("")) returns 1
// — so if either side could be empty and reached the compare, an UNAUTHENTICATED
// caller (no Authorization header → inbound "") would be TRUSTED when the seed
// token is also empty (provider unwired/errored). Both len>0 checks below are
// therefore HARD gates that MUST precede the compare. RED-armed
// (TestR_C2_EmptyInbound_NotTrusted / TestR_C2_ForgeGuard_NoSeedProviderWired).
//
// TL refinement 4 — SAFE-DEGRADE fail-closed: no seed token / provider unwired or
// erroring (currentSeedLoopbackToken → have=false) ⇒ NOTHING is trusted ⇒ headers
// ignored. Consistent by construction: in that same state the #57 bearer-append
// would not fire either (no authn token on the seed ctx), so there is no
// authenticated self-loopback to guard — the guard being off cannot let a real
// loopback storm through because a real loopback cannot exist without the token.
//
// CO-LOCATION (arch-enforced): the EMIT side appends the SAME token this ingest
// trusts. Emit rides the #57 bearer-append arm (resolve.go, parsedHostEqualsSelf
// gate); that bearer is xcontext.AccessToken(r.ctx) — which on the seed path was
// installed by installSeedLoopbackToken (phase1_walk.go) from seedTokenProvider.
// This ingest's currentSeedLoopbackToken reads the SAME seedTokenProvider. So the
// token emit appends == the token ingest trusts, BY SHARED SOURCE — they cannot
// desync (no parallel token derivation).
func isTrustedSelfLoopback(req *http.Request) bool {
	// Refinement 3: empty inbound → NOT trusted (an unauthenticated caller must
	// never reach the constant-time compare).
	inbound, ok := xcontext.AccessToken(req.Context())
	if !ok || len(inbound) == 0 {
		return false
	}
	// Refinement 1+4: LIVE provider read; unwired/errored/empty → NOT trusted.
	seedTok, have := currentSeedLoopbackToken(req.Context())
	if !have || len(seedTok) == 0 {
		return false // no seed token wired → nothing to trust against (fail-closed)
	}
	// Refinement 2: constant-time compare, reached ONLY with both sides non-empty.
	if subtle.ConstantTimeCompare([]byte(inbound), []byte(seedTok)) != 1 {
		return false // not snowplow's own seed loopback — headers ignored (C2)
	}
	// Belt-and-suspenders secondary (token-provenance is the cryptographic primary).
	return requestArrivedOnSelfHost(req)
}

// reseedNestedGuardsFromHeaders re-installs the depth counter + ancestor set from
// the inbound X-Snowplow-* headers onto ctx — but ONLY when isTrustedSelfLoopback.
// Returns ctx unchanged for an untrusted request (headers ignored). The parse
// uses the SAME cache-package codec the emit site serialized with (anti-drift).
//
// Placement contract: the caller invokes this AFTER xcontext.BuildContext and
// AFTER the #83 Option-A WithNestedResolveAncestor self-seed, so the seeded
// ancestor set is {this node} ∪ {the header's ancestors} — exactly the current
// descent path the emit side serialized ({...ancestors, this-node's-parent}).
func reseedNestedGuardsFromHeaders(ctx context.Context, req *http.Request) context.Context {
	if !isTrustedSelfLoopback(req) {
		return ctx
	}
	if v := req.Header.Get(cache.NestedDepthHeader); v != "" {
		if d, err := strconv.Atoi(v); err == nil && d >= 0 {
			ctx = cache.WithNestedCallDepth(ctx, d)
		}
	}
	for _, node := range cache.ParseAncestorsHeader(req.Header.Get(cache.ResolveAncestorsHeader)) {
		ctx = cache.WithNestedResolveAncestor(ctx, node)
	}
	return ctx
}

// httpEdgeGuardStop is the HTTP-edge twin of the in-process cycle-stop /
// depth-8 backstop (nested_call.go:114,169). It runs AFTER fetchObject +
// checkDispatchRBAC (C3 — the stop is NEVER an RBAC bypass; it fires only on an
// authorized, real CR) and BEFORE the L1 lookup. It reads the ctx guards that
// reseedNestedGuardsFromHeaders installed (so an UNTRUSTED request has no guards
// → this always returns stop=false for it → normal resolve, C2).
//
//   - If node (this CR's resource/namespace/name — the SAME shared derivation
//     nestedResolveNodeKey builds) is already on the current descent path
//     (NestedResolveAncestorPresent, seeded from the header + the #83 self-seed),
//     this is a SELF-REFERENCE over the HTTP boundary: return stop=true, raw=true
//     → the handler serves the RAW CR (resolve:false semantics), NOT a recurse.
//     This is the primary, cost-optimal stop (fires at the first self-reentry).
//   - Else if the inbound depth has reached NestedCallMaxDepth, the depth-8
//     BACKSTOP trips: stop=true, raw=false → the handler returns a bounded
//     508-class error (a non-cyclic pathologically deep chain).
//   - Else stop=false → resolve normally.
func httpEdgeGuardStop(ctx context.Context, log *slog.Logger, resource, namespace, name string) (stop, raw bool) {
	node := nestedResolveNodeKey(resource, namespace, name)
	if cache.NestedResolveAncestorPresent(ctx, node) {
		cache.BumpHTTPEdgeCycleStop()
		log.Debug("http-edge nested resolve cycle-stop: node already an ancestor — returning raw CR",
			slog.String("node", node),
			slog.Int("depth", cache.NestedCallDepthFromContext(ctx)),
		)
		return true, true
	}
	if cache.NestedCallDepthFromContext(ctx) >= cache.NestedCallMaxDepth() {
		cache.BumpHTTPEdgeDepthStop()
		log.Warn("http-edge nested resolve depth-limit backstop: refusing to recurse further",
			slog.String("node", node),
			slog.Int("depth", cache.NestedCallDepthFromContext(ctx)),
			slog.Int("max", cache.NestedCallMaxDepth()),
		)
		return true, false
	}
	return false, false
}
