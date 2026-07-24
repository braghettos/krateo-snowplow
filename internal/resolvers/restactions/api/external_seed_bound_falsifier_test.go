//go:build unit
// +build unit

// external_seed_bound_falsifier_test.go — fetch-level falsifiers for the
// boot-seed external-fetch wall-clock bound
// (docs/boot-seed-external-fetch-bound-design-2026-07-24.md).
//
// These drive the REAL production dispatch wrap — externalSeedFetchCtx +
// httpFetchAllowingNonJSON exactly as resolve.go:1106-1107 calls them —
// against an httptest.Server, so they exercise the real plumbing
// RetryClient/backoff envelope, not a stub of it.
//
// Arms:
//   - RED / discriminating (K>1×M>1): a production set MISSING one member
//     leaves that vector's fetch UNBOUNDED (~D+backoff); the full set bounds
//     every vector at ~D. Proves the scope set is COMPLETE + each member
//     load-bearing (feedback_falsifier_shape_must_discriminate). Mutates the
//     REAL production set (bootReadinessFetchScopes), not a shadow copy.
//   - Retry-envelope truncation: a 5xx-always stub returns in ~D, NOT
//     ~D+Σbackoff — proves ctx.WithTimeout truncates the WHOLE retry envelope
//     (limiter + per-attempt + backoff sleeps), strictly stronger than a bare
//     http.Client.Timeout.
//   - Scope isolation: a /call (ScopeCallGeneric) scope is NOT cut at D — a
//     slow external under a customer /call runs to completion, byte-identical.
//   - Default / toggle: env unset → 5000ms active; env=0 → disabled (pre-fix).
//
// No kubeconfig, no kind cluster (feedback_no_go_test_against_remote_kubeconfig).

package api

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	httpcall "github.com/krateoplatformops/plumbing/http/request"
	"github.com/krateoplatformops/snowplow/internal/cache"
)

// slowOrFailingServer returns an httptest.Server whose handler either sleeps
// `bodyDelay` before a 200 (slow-but-eventually-OK) or always 5xx (retry
// amplifier). It records the number of inbound attempts so the retry-envelope
// arm can confirm >1 attempt actually occurred.
func slowOrFailingServer(t *testing.T, status int, bodyDelay time.Duration) (url string, attempts *int64) {
	t.Helper()
	var n int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&n, 1)
		if bodyDelay > 0 {
			select {
			case <-time.After(bodyDelay):
			case <-r.Context().Done():
				// The request ctx was cut (the wall-clock bound) — abandon.
				return
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &n
}

// fetchUnderScope stamps `scope` (if non-empty) on a cache-enabled ctx, wraps
// it with the PRODUCTION externalSeedFetchCtx exactly as resolve.go:1106 does,
// then runs the REAL httpFetchAllowingNonJSON against serverURL. Returns the
// wall-clock elapsed.
func fetchUnderScope(t *testing.T, scope, serverURL string) time.Duration {
	t.Helper()
	ctx := xcontext.BuildContext(t.Context())
	if scope != "" {
		ctx = cache.WithFallthroughScope(ctx, scope)
	}

	fctx, fcancel := externalSeedFetchCtx(ctx)
	defer fcancel()

	verb := http.MethodGet
	endpoint := endpoints.Endpoint{ServerURL: serverURL}
	call := httpcall.RequestOptions{
		RequestInfo: httpcall.RequestInfo{Verb: &verb},
		Endpoint:    &endpoint,
	}
	start := time.Now()
	_, _, _, _ = httpFetchAllowingNonJSON(fctx, call)
	return time.Since(start)
}

// TestBound_DiscriminatingRED_EachScopeMemberIsLoadBearing — for each of the
// two readiness scopes, a production set MISSING that member leaves ITS vector
// UNBOUNDED (fetch runs to the slow server's full delay), while the full set
// bounds it at ~D. K=2 scopes × M=2 set-configs (full vs missing-one).
func TestBound_DiscriminatingRED_EachScopeMemberIsLoadBearing(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	// Tight bound so "bounded" (~150ms) is unambiguously below the slow
	// server's 1200ms delay.
	t.Setenv(envSeedExternalFetchTimeoutMS, "150")
	const D = 150 * time.Millisecond
	const slow = 1200 * time.Millisecond

	url, _ := slowOrFailingServer(t, http.StatusOK, slow)

	// Save + restore the REAL production set (no shadow copy — drift-proof).
	orig := bootReadinessFetchScopes
	t.Cleanup(func() { bootReadinessFetchScopes = orig })

	for _, member := range []string{cache.ScopeBootPrewarmSeed, cache.ScopeBootPrewarmWalk} {
		member := member
		t.Run("full_set_bounds_"+member, func(t *testing.T) {
			bootReadinessFetchScopes = map[string]struct{}{
				cache.ScopeBootPrewarmSeed: {},
				cache.ScopeBootPrewarmWalk: {},
			}
			el := fetchUnderScope(t, member, url)
			if el >= slow {
				t.Fatalf("full set: %s fetch NOT bounded (elapsed %v ≥ slow %v)", member, el, slow)
			}
			if el > D+800*time.Millisecond {
				t.Fatalf("full set: %s fetch elapsed %v, expected ~D=%v (bounded)", member, el, D)
			}
		})
		t.Run("set_missing_"+member+"_leaves_it_UNBOUNDED_RED", func(t *testing.T) {
			// Drop exactly this member — the discriminating RED.
			bootReadinessFetchScopes = map[string]struct{}{
				cache.ScopeBootPrewarmSeed: {},
				cache.ScopeBootPrewarmWalk: {},
			}
			delete(bootReadinessFetchScopes, member)

			el := fetchUnderScope(t, member, url)
			if el < slow {
				t.Fatalf("RED FAILED TO DISCRIMINATE: %s fetch was bounded (elapsed %v < slow %v) even though its scope was DROPPED from the set — the bound does not actually key on this member, so the falsifier cannot prove the member is load-bearing.", member, el, slow)
			}
		})
	}
}

// TestBound_RetryEnvelopeTruncation — a 5xx-always endpoint (retry amplifier)
// under a boot-readiness scope returns in ~D, NOT ~D + Σbackoff. Proves the
// ctx.WithTimeout truncates the WHOLE retry envelope (including the
// inter-attempt backoff sleeps), which a bare http.Client.Timeout would not.
func TestBound_RetryEnvelopeTruncation(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv(envSeedExternalFetchTimeoutMS, "250")
	const D = 250 * time.Millisecond
	// Force multiple retry attempts with a real backoff so, absent the ctx
	// bound, the envelope would run far past D. base 200ms × several attempts.
	t.Setenv("CLIENT_MAX_RETRIES", "5")
	t.Setenv("CLIENT_BASE_BACKOFF", "200ms")
	t.Setenv("CLIENT_MAX_BACKOFF", "5s")

	url, attempts := slowOrFailingServer(t, http.StatusInternalServerError, 0)

	el := fetchUnderScope(t, cache.ScopeBootPrewarmSeed, url)

	// Bounded: must be well under the un-truncated envelope
	// (200+400+800+1600ms ≈ 3s of backoff alone).
	if el > 1500*time.Millisecond {
		t.Fatalf("RED: 5xx retry envelope NOT truncated — elapsed %v, expected ~D=%v. The ctx deadline is not cutting the inter-attempt backoff sleeps (a bare http.Client.Timeout would leave them running).", el, D)
	}
	// The retry loop must actually have engaged (>1 attempt) or the truncation
	// claim is vacuous.
	if n := atomic.LoadInt64(attempts); n < 2 {
		t.Fatalf("premise: only %d attempt(s) reached the 5xx server — the retry loop did not engage, so the retry-envelope-truncation claim is not exercised (raise CLIENT_MAX_RETRIES / lower backoff).", n)
	}
}

// TestBound_ScopeIsolation_CallGenericNotCut — a slow external widget under a
// /call scope (ScopeCallGeneric) is NOT cut at D: the customer /call runs to
// completion, byte-identical to the pre-fix path.
func TestBound_ScopeIsolation_CallGenericNotCut(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv(envSeedExternalFetchTimeoutMS, "150")
	const slow = 600 * time.Millisecond

	url, _ := slowOrFailingServer(t, http.StatusOK, slow)

	el := fetchUnderScope(t, cache.ScopeCallGeneric, url)
	if el < slow {
		t.Fatalf("RED: a /call (ScopeCallGeneric) external fetch was CUT at D (elapsed %v < slow %v) — the bound must NOT touch customer /call scopes; a slow external widget on /call must run to completion.", el, slow)
	}
}

// TestBound_DefaultAndToggle — env unset → 5000ms bound ACTIVE on a boot scope;
// env=0 → disabled (pre-fix unbounded). Also asserts the shipped default is ON,
// not off (a default-off flag would not fix the live backstop).
func TestBound_DefaultAndToggle(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	t.Run("env_unset_default_5000_bound_ON", func(t *testing.T) {
		// Explicitly ensure unset.
		t.Setenv(envSeedExternalFetchTimeoutMS, "")
		ctx := cache.WithFallthroughScope(xcontext.BuildContext(t.Context()), cache.ScopeBootPrewarmSeed)
		fctx, cancel := externalSeedFetchCtx(ctx)
		defer cancel()
		dl, ok := fctx.Deadline()
		if !ok {
			t.Fatal("RED: env unset must default to a 5000ms bound (ON), but the ctx carries no deadline — the shipped default is NOT bounded. A default-off flag does not fix the live backstop.")
		}
		// Deadline should be ~5s out (allow slack).
		if until := time.Until(dl); until <= 0 || until > 6*time.Second {
			t.Fatalf("default deadline is %v out, expected ~5s (defaultSeedExternalFetchTimeoutMS=%d)", until, defaultSeedExternalFetchTimeoutMS)
		}
	})

	t.Run("env_zero_disabled_prefix_behavior", func(t *testing.T) {
		t.Setenv(envSeedExternalFetchTimeoutMS, "0")
		ctx := cache.WithFallthroughScope(xcontext.BuildContext(t.Context()), cache.ScopeBootPrewarmSeed)
		fctx, cancel := externalSeedFetchCtx(ctx)
		defer cancel()
		if _, ok := fctx.Deadline(); ok {
			t.Fatal("RED: env=0 must DISABLE the bound (pre-fix unbounded behavior), but the ctx carries a deadline.")
		}
		if fctx != ctx {
			t.Fatal("RED: env=0 must return the parent ctx unchanged (byte-identical no-op).")
		}
	})
}

// TestBound_Internal130NonRegression — an internal (non-external) resolve never
// reaches resolve.go:1106, so its ctx is never handed to externalSeedFetchCtx.
// We assert the wrap decision itself is byte-identical flag-ON vs flag-OFF for a
// ctx with NO fallthrough scope (the shape a background/non-readiness caller
// carries): the helper is a no-op either way, so no latch / L1-key is touched.
func TestBound_Internal130NonRegression(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	noScope := xcontext.BuildContext(t.Context())

	for _, ms := range []string{"5000", "0"} {
		t.Setenv(envSeedExternalFetchTimeoutMS, ms)
		fctx, cancel := externalSeedFetchCtx(noScope)
		if fctx != noScope {
			cancel()
			t.Fatalf("RED (#130): a no-scope (internal / background) ctx was WRAPPED at MS=%s — the bound must no-op when there is no boot-readiness scope, so internal informer-backed first-nav warming is byte-identical.", ms)
		}
		if _, ok := fctx.Deadline(); ok {
			cancel()
			t.Fatalf("RED (#130): a no-scope ctx acquired a deadline at MS=%s — must stay unbounded.", ms)
		}
		cancel()
	}
}
