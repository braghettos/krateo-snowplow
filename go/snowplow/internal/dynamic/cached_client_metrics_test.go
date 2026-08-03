// cached_client_metrics_test.go — Task #326 unit tests for the
// snowplow_sa_discovery_* expvar wiring (cached_client_metrics.go).
//
// Asserts, mirroring internal/cache/refresher_metrics_test.go idioms:
//   - registerSADiscoveryMetrics is idempotent (second call is a no-op, does
//     NOT panic on duplicate expvar.Publish).
//   - PRESENCE: all three keys are published.
//   - MONOTONICITY: driving each counter through the production primitives
//     (SharedSADiscoveryClient build => builds; InvalidateSADiscovery on a live
//     mapper => invalidations; SharedSADiscoveryClient(nil) => fallbacks) makes
//     the published expvar.Func report the +1 — read through expvar.Get, not
//     the atomic directly, so the full expvar surface is exercised.

package dynamic

import (
	"encoding/json"
	"expvar"
	"testing"

	"k8s.io/client-go/rest"
)

var saDiscoveryMetricKeys = []string{
	"snowplow_sa_discovery_builds_total",
	"snowplow_sa_discovery_invalidations_total",
	"snowplow_sa_discovery_fallbacks_total",
}

// TestSADiscoveryMetrics_Idempotent — calling the registration helper twice
// must NOT panic. expvar.Publish panics on a duplicate key; the sync.Once guard
// inside registerSADiscoveryMetrics makes the helper safe to call repeatedly.
func TestSADiscoveryMetrics_Idempotent(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("registerSADiscoveryMetrics panicked on second call: %v", r)
		}
	}()
	registerSADiscoveryMetrics()
	registerSADiscoveryMetrics() // second call must be a no-op
}

// TestSADiscoveryMetrics_AllKeysPublished — PRESENCE: every
// snowplow_sa_discovery_* key MUST be registered.
func TestSADiscoveryMetrics_AllKeysPublished(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	registerSADiscoveryMetrics()
	for _, k := range saDiscoveryMetricKeys {
		if expvar.Get(k) == nil {
			t.Fatalf("%s not published", k)
		}
	}
}

// saExpvarUint reads a published uint64-returning expvar.Func by key.
func saExpvarUint(t *testing.T, key string) string {
	t.Helper()
	v := expvar.Get(key)
	if v == nil {
		t.Fatalf("%s not published", key)
	}
	return v.String()
}

// saWantJSONUint renders n the way expvar.Func.String() does for a uint64.
func saWantJSONUint(t *testing.T, n uint64) string {
	t.Helper()
	b, err := json.Marshal(n)
	if err != nil {
		t.Fatalf("json.Marshal(%d): %v", n, err)
	}
	return string(b)
}

// TestSADiscoveryMetrics_PublishedAndReflectsAtomics — MONOTONICITY per
// counter. installFakeCachedDiscovery resets the singleton + counters (via
// resetSADiscoveryForTest) so the baseline is a clean zero, then each counter
// is driven through the production primitive and read back via the published
// expvar.Func.
func TestSADiscoveryMetrics_PublishedAndReflectsAtomics(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	registerSADiscoveryMetrics()
	fake := &fakeCachedDiscovery{}
	installFakeCachedDiscovery(t, fake) // resets singleton + counters before/after

	// --- builds: a successful SharedSADiscoveryClient build bumps it once. ---
	beforeBuilds := SADiscoveryStatsSnapshot().Builds
	rc := &rest.Config{Host: "https://metrics.invalid"}
	if _, err := SharedSADiscoveryClient(rc); err != nil {
		t.Fatalf("SharedSADiscoveryClient errored: %v", err)
	}
	// A second call on the warm singleton must NOT rebuild (no extra build).
	if _, err := SharedSADiscoveryClient(rc); err != nil {
		t.Fatalf("SharedSADiscoveryClient (warm) errored: %v", err)
	}
	afterBuilds := SADiscoveryStatsSnapshot().Builds
	if afterBuilds != beforeBuilds+1 {
		t.Fatalf("builds snapshot: got %d want %d (one build per warm singleton)", afterBuilds, beforeBuilds+1)
	}
	if got, want := saExpvarUint(t, "snowplow_sa_discovery_builds_total"), saWantJSONUint(t, afterBuilds); got != want {
		t.Fatalf("snowplow_sa_discovery_builds_total expvar = %s, want %s", got, want)
	}

	// --- invalidations: InvalidateSADiscovery on the live mapper bumps it. ---
	beforeInval := SADiscoveryStatsSnapshot().Invalidations
	InvalidateSADiscovery()
	afterInval := SADiscoveryStatsSnapshot().Invalidations
	if afterInval != beforeInval+1 {
		t.Fatalf("invalidations snapshot: got %d want %d", afterInval, beforeInval+1)
	}
	if got, want := saExpvarUint(t, "snowplow_sa_discovery_invalidations_total"), saWantJSONUint(t, afterInval); got != want {
		t.Fatalf("snowplow_sa_discovery_invalidations_total expvar = %s, want %s", got, want)
	}

	// --- fallbacks: SharedSADiscoveryClient(nil) errors => caller falls back. ---
	beforeFallbacks := SADiscoveryStatsSnapshot().Fallbacks
	if _, err := SharedSADiscoveryClient(nil); err == nil {
		t.Fatalf("SharedSADiscoveryClient(nil) returned no error — expected the fallback path")
	}
	afterFallbacks := SADiscoveryStatsSnapshot().Fallbacks
	if afterFallbacks != beforeFallbacks+1 {
		t.Fatalf("fallbacks snapshot: got %d want %d", afterFallbacks, beforeFallbacks+1)
	}
	if got, want := saExpvarUint(t, "snowplow_sa_discovery_fallbacks_total"), saWantJSONUint(t, afterFallbacks); got != want {
		t.Fatalf("snowplow_sa_discovery_fallbacks_total expvar = %s, want %s", got, want)
	}
}
