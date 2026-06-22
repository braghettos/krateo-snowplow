// nested_call_seam.go — the in-process RA/widget resolver seam.
//
// HISTORY: introduced at Ship 0.30.123 (#155) for the /call-loopback stage.
// The /call-loopback DISPATCH BRANCH + its RESOLVER_INPROCESS_NESTED_CALL
// kill-switch were RETIRED in the 2026-06-22 unified ship (the corpus audit
// confirmed zero live loopback paths). The SEAM itself SURVIVES: it is now
// the in-process resolver behind the DIRECT-APISERVER-PATH + `resolve: true`
// mechanism (resolve_inprocess.go's maybeResolveInProcess). The flag is gone
// — the resolve substitution is gated by the per-step `resolve` property
// (default true), not a process-wide env switch.
//
// THE IMPORT-CYCLE CONSTRAINT: this package (internal/resolvers/restactions/
// api) cannot import `restactions`/`widgets`/`dispatchers` — they import this
// package, so a back-import is a cycle. The actual resolution (objects.Get ->
// checkDispatchRBAC -> branch on GVR -> restactions.Resolve / widgets.Resolve)
// therefore lives in internal/handlers/dispatchers/nested_call.go, which CAN
// import everything. This file declares only the SEAM: a function-typed
// package var the dispatchers package fills at startup via
// RegisterNestedCallResolver. Mirrors the resolveOnceFn seam pattern
// (dispatchers/resolve_populate.go).
//
// When an api-step's `path` is a direct apiserver path to a RESTAction/Widget
// CR with resolve:true, the resolver's inner-call worker (resolve.go) — after
// the CR is fetched from the cacheable internal path — invokes
// nestedCallResolver IN-PROCESS, carrying the WithUserInfo identity already on
// ctx and the OUTER L1 key (for transitive dep propagation). This lets a
// JWT-less / SA-credentialed resolve complete a referenced-CR resolve that a
// per-user HTTP edge could not.

package api

import (
	"context"

	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
)

// NestedCallResolverFunc resolves a /call-loopback stage IN-PROCESS. ref
// is the ObjectReference decoded from the stage's /call?resource=...&
// apiVersion=... path; perPage/page/extras are the pagination + extras
// the stage carried. It returns the referenced RESTAction's resolved
// Status.Raw — byte-identical to what an HTTP /call would have returned
// as its response body — or an error.
//
// The implementation (dispatchers.ResolveNestedCall) MUST run the
// checkDispatchRBAC gate before resolving — the in-process path bypasses
// the HTTP edge, and with it the per-user apiserver RBAC enforcement, so
// the explicit gate is the single load-bearing correctness line.
type NestedCallResolverFunc func(
	ctx context.Context,
	ref templatesv1.ObjectReference,
	perPage, page int,
	extras map[string]any,
) (statusRaw []byte, err error)

// nestedCallResolver is the seam. nil until RegisterNestedCallResolver
// wires it at startup — a nil resolver is the SECOND structural fallback
// (alongside the env flag): the loopback branch is skipped and the /call
// stage takes the HTTP path. Production never reassigns it after the
// single startup wiring; tests swap it via the _test.go shim.
var nestedCallResolver NestedCallResolverFunc

// RegisterNestedCallResolver wires the in-process nested-/call resolver.
// Called once at startup from main.go —
// api.RegisterNestedCallResolver(dispatchers.ResolveNestedCall) —
// alongside the cache.RegisterRefreshFunc wiring. Idempotent in shape; a
// later call replaces the earlier wiring (used by tests).
func RegisterNestedCallResolver(fn NestedCallResolverFunc) {
	nestedCallResolver = fn
}
