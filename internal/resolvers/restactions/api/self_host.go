// self_host.go — #57 (C-b): the configured SELF-LOOPBACK host the
// bearer-append's self-loopback arm compares a resolved endpoint against.
//
// A prewarm seed (or any caller) whose api-step is a literal `/call?...`
// loopback with the named `snowplow-endpoint` EndpointRef resolves to
// snowplow's OWN JWT-gated `/call` — so the seed's authn bearer MUST be
// appended even though a named EndpointRef is present (the original gate
// suppresses it for named endpoints, assuming they carry their own auth —
// true for a genuine external endpoint, false for the self-loopback). The
// self-host is the discriminator.
//
// HARD (C-b): the comparison is EXACT (scheme, host, port) field-equality
// against the PARSED URL_SELF — NEVER strings.Contains / suffix / prefix. An
// external endpoint whose host merely CONTAINS "snowplow"
// (snowplow-foo.example.com) must NOT match → the bearer stays off it. No
// resource/name/path literal — a uniform host predicate (feedback_no_special_cases).
package api

import (
	"net/url"
	"sync/atomic"

	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
)

// selfHostKey is the (scheme, host, port) tuple parsed once from URL_SELF.
// host is url.URL.Hostname() (no port); port is url.URL.Port() (the explicit
// port string, "" when absent). Compared field-by-field — exact equality.
type selfHostKey struct {
	scheme string
	host   string
	port   string
}

// selfHost holds the configured self-loopback host, parsed once at startup.
// atomic.Pointer so SetSelfHost (startup / tests) and the read in
// parsedHostEqualsSelf are race-free without a mutex. nil = unconfigured →
// the self-loopback arm never fires (pre-#57 behaviour; the bearer-append
// gate is then exactly the original EndpointRef==nil||ExportJWT).
var selfHost atomic.Pointer[selfHostKey]

// SetSelfHost parses rawURL (e.g. http://snowplow.krateo-system.svc.cluster.local:8081)
// and installs it as the self-loopback host. An empty or unparseable rawURL
// CLEARS it (self-loopback arm disabled). Wired once at startup from main.go
// (URL_SELF env) and by tests. Returns true when a non-empty host was set.
func SetSelfHost(rawURL string) bool {
	if rawURL == "" {
		selfHost.Store(nil)
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		selfHost.Store(nil)
		return false
	}
	selfHost.Store(&selfHostKey{scheme: u.Scheme, host: u.Hostname(), port: u.Port()})
	return true
}

// parsedHostEqualsSelf reports whether rawURL's (scheme, host, port) EXACTLY
// equals the configured self-host. False when self-host is unconfigured, when
// rawURL is unparseable, or on ANY field mismatch. Exact field-equality only
// — a substring/suffix near-miss (snowplow-foo.example.com) returns false.
func parsedHostEqualsSelf(rawURL string) bool {
	sh := selfHost.Load()
	if sh == nil || rawURL == "" {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return u.Scheme == sh.scheme && u.Hostname() == sh.host && u.Port() == sh.port
}

// bearerAppendForStage is the #57 (C-b) user-bearer-append DECISION for a
// non-UAF stage, extracted as a pure function so the gate wiring is unit-
// testable without the resolveRun/runStage harness (C-c arm-1 light coverage;
// the on-cluster post-deploy gate is the full functional proof). Callers gate
// it under `!uafActive && accessToken != ""` (resolve.go) — this only decides
// the WHICH-endpoint half.
//
// True (append the bearer) when EITHER:
//   - the original gate: no named EndpointRef, or ExportJWT==true (a named
//     endpoint is assumed to carry its own auth); OR
//   - the #57 self-loopback arm (ADDITIVE): the resolved endpoint host EXACTLY
//     equals the configured self-host (parsedHostEqualsSelf — exact
//     scheme+host+port, never a substring near-miss), i.e. the step loops back
//     at snowplow's OWN JWT-gated /call and needs the seed bearer despite its
//     named snowplow-endpoint ref.
// False for a genuine EXTERNAL named endpoint (incl. a host that merely
// CONTAINS "snowplow") — the bearer stays off it (the JWT-leak guard).
func bearerAppendForStage(apiCall *templates.API, ep endpoints.Endpoint) bool {
	if apiCall == nil {
		return false
	}
	return apiCall.EndpointRef == nil ||
		ptr.Deref(apiCall.ExportJWT, false) ||
		parsedHostEqualsSelf(ep.ServerURL)
}
