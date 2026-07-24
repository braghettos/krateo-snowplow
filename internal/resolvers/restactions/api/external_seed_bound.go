// external_seed_bound.go — boot-readiness external-fetch wall-clock bound
// (#129-adjacent; origin: live west4-release 1.7.15 boot
// `readyz.backstop.fired reason=seed_incomplete elapsed_ms=480001`).
//
// ROOT CAUSE. The single external GET dispatch at resolve.go:1106-1107
// (httpFetchAllowingNonJSON) is bounded ONLY by the ctx deadline. On the
// boot-readiness paths that deadline is the 120s cohort timeout — no
// http.Client.Timeout is set, and util.NewRetryClient retries idempotent
// GETs with exponential backoff. So one degraded external endpoint (e.g. a
// ClickHouse obs-* data source) blocks up to ~120s per (external-widget ×
// cohort) unit, and N such units overrun pipGlobalTimeout=480s → the seed
// returns DeadlineExceeded → the readiness backstop fires.
//
// FIX. externalSeedFetchCtx wraps the fetch ctx with a per-fetch wall-clock
// deadline D, but ONLY on the boot readiness-critical paths (seed / discovery
// walk / content-prewarm pass), keyed on the EXISTING structural
// FallthroughScope constant — no host/name/resource literal
// (feedback_no_special_cases). Because the request is built with
// http.NewRequestWithContext and util.RetryClient.Do honors req.Context() at
// EVERY retry boundary (limiter wait, per-attempt clone, and the
// inter-attempt backoff sleeps), a context.WithTimeout truncates the WHOLE
// retry envelope — strictly stronger than a bare http.Client.Timeout, which
// would not bound the backoff sleeps between attempts.
//
// SCOPE ISOLATION. A /call (ScopeCallGeneric / ScopeCallRestactions / …) or a
// refresher resolve is NOT one of the gated scopes, so this returns the
// parent ctx + a no-op cancel and the external fetch on those paths stays
// byte-identical (no latency ceiling on a customer /call to a slow external
// widget). The internal informer-backed / apistage / internal-rest-config
// dispatch branches all return BEFORE resolve.go:1106, so their ctx is never
// wrapped — #130 first-nav internal warming is structurally untouched.
package api

import (
	"context"
	"time"

	"github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/snowplow/internal/cache"
)

// envSeedExternalFetchTimeoutMS is the string-typed installer knob
// (installer plumbing is string-only per
// project_installer_config_plumbing_is_string_only — env.Int coerces the
// string to an int in-runtime, no chart-boolean). Default 5000 (5s) means
// the bound is ON by default: a degraded external endpoint on the boot
// readiness path is cut at 5s, not ~120s. A value <= 0 disables the bound
// (restores the pre-fix unbounded behavior — reproduces the outage; NOT the
// shipped default). Toggle both ways per project_caching_is_provisional.
const envSeedExternalFetchTimeoutMS = "RESOLVER_SEED_EXTERNAL_FETCH_TIMEOUT_MS"

const defaultSeedExternalFetchTimeoutMS = 5000

// bootReadinessFetchScopes is the closed set of FallthroughScope paths on
// which the external-fetch wall-clock bound fires — every readiness-critical
// vector that resolves external widgets BEFORE MarkPhase1Done:
//
//   - ScopeBootPrewarmSeed  — per-cohort seed (withCohortSeedContext).
//   - ScopeBootPrewarmWalk  — discovery walk (withPhase1SAContext) AND the
//     content-prewarm pass (withContentPrewarmSAContext, which reuses the
//     walk scope — see phase1_content_prewarm.go). The content pass resolves
//     the obs-* apiRef data sources at PerPage:-1, so its resolves never
//     reach shouldServeRAFullList's paginated branch; the shared walk scope
//     therefore bounds its fetch without altering any other content-pass
//     behavior.
var bootReadinessFetchScopes = map[string]struct{}{
	cache.ScopeBootPrewarmSeed: {},
	cache.ScopeBootPrewarmWalk: {},
}

// externalSeedFetchCtx returns a ctx + cancel to wrap the external fetch at
// resolve.go:1106-1107. On a non-boot-readiness scope, or when the bound is
// disabled (D <= 0), it returns (ctx, no-op) so the fetch is byte-identical
// to the pre-fix path. On a boot-readiness scope with D > 0 it returns a
// context.WithTimeout(ctx, D) that truncates the whole retry envelope.
//
// The caller MUST `defer cancel()` in all cases (the no-op cancel is a real
// func so the call site is uniform).
func externalSeedFetchCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	ms := env.Int(envSeedExternalFetchTimeoutMS, defaultSeedExternalFetchTimeoutMS)
	if ms <= 0 {
		// Disabled — pre-fix unbounded behavior.
		return ctx, func() {}
	}

	fs := cache.FallthroughScope(ctx)
	if fs == nil {
		// No scope marker — /call-class handlers always stamp one, so a nil
		// scope here is a background/non-readiness path (or cache-off, where
		// WithFallthroughScope no-ops). Leave the fetch unbounded.
		return ctx, func() {}
	}
	if _, gated := bootReadinessFetchScopes[fs.Path]; !gated {
		// A /call or refresher scope — NOT readiness-critical. Byte-identical.
		return ctx, func() {}
	}

	return context.WithTimeout(ctx, time.Duration(ms)*time.Millisecond)
}

// ExternalSeedFetchBounded reports whether externalSeedFetchCtx would apply a
// wall-clock deadline to a fetch under ctx — i.e. whether ctx's
// FallthroughScope is one of the boot-readiness scopes AND the bound is
// enabled (D > 0). It is the SINGLE SOURCE OF TRUTH for the gating decision,
// exported so a marker-liveness test in the dispatchers package can drive the
// REAL production seed / walk / content-prewarm context builders (which live
// there) all the way into THIS predicate, rather than re-asserting a
// hand-copied scope-set that could drift
// (feedback_falsifier_must_drive_real_boundary_not_install_crossed_state).
//
// It derives its answer by actually invoking externalSeedFetchCtx and checking
// that the returned ctx is a DIFFERENT ctx value than the input — the bounded
// path returns a fresh context.WithTimeout child, the no-op path returns the
// parent ctx unchanged. This discriminates the helper's OWN wrapping and does
// NOT give a false positive when the parent already carries an (inherited)
// deadline, so the test can never pass against a helper that silently stopped
// wrapping.
func ExternalSeedFetchBounded(ctx context.Context) bool {
	fctx, cancel := externalSeedFetchCtx(ctx)
	defer cancel()
	return fctx != ctx
}
