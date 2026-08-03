// schema_cache_metrics_test.go — Task #326 unit tests for the
// snowplow_crd_schema_memo_* expvar wiring (schema_cache_metrics.go).
//
// Asserts, mirroring internal/cache/refresher_metrics_test.go idioms:
//   - registerCRDSchemaMemoMetrics is idempotent (second call is a no-op, does
//     NOT panic on duplicate expvar.Publish).
//   - PRESENCE: all four keys are published.
//   - MONOTONICITY: driving each underlying atomic (via the same memo
//     primitives the production path uses) makes the published expvar.Func
//     report the incremented value — read through expvar.Get, not the atomic
//     directly, so the full expvar surface is exercised.

package schema

import (
	"encoding/json"
	"expvar"
	"testing"
)

// metricKeys is the full set this ship publishes. Used by the presence test
// and as the source of truth for the per-counter monotonicity drivers below.
var metricKeys = []string{
	"snowplow_crd_schema_memo_hits_total",
	"snowplow_crd_schema_memo_misses_total",
	"snowplow_crd_schema_memo_stale_dropped_total",
	"snowplow_crd_schema_memo_invalidations_total",
}

// TestCRDSchemaMemoMetrics_Idempotent — calling the registration helper twice
// must NOT panic. expvar.Publish panics on a duplicate key; the sync.Once guard
// inside registerCRDSchemaMemoMetrics makes the helper safe to call repeatedly.
func TestCRDSchemaMemoMetrics_Idempotent(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("registerCRDSchemaMemoMetrics panicked on second call: %v", r)
		}
	}()
	registerCRDSchemaMemoMetrics()
	registerCRDSchemaMemoMetrics() // second call must be a no-op
}

// TestCRDSchemaMemoMetrics_AllKeysPublished — PRESENCE: every snowplow_crd_
// schema_memo_* key MUST be registered. A nil here means the publisher dropped
// a key.
func TestCRDSchemaMemoMetrics_AllKeysPublished(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	registerCRDSchemaMemoMetrics()
	for _, k := range metricKeys {
		if expvar.Get(k) == nil {
			t.Fatalf("%s not published", k)
		}
	}
}

// expvarUint reads a published uint64-returning expvar.Func by key and returns
// its rendered value. Fails loud if the key is missing.
func expvarUint(t *testing.T, key string) string {
	t.Helper()
	v := expvar.Get(key)
	if v == nil {
		t.Fatalf("%s not published", key)
	}
	return v.String()
}

// wantJSONUint renders n the way expvar.Func.String() does for a uint64 return
// (json.Marshal -> bare decimal), so the monotonicity asserts compare like for
// like.
func wantJSONUint(t *testing.T, n uint64) string {
	t.Helper()
	b, err := json.Marshal(n)
	if err != nil {
		t.Fatalf("json.Marshal(%d): %v", n, err)
	}
	return string(b)
}

// TestCRDSchemaMemoMetrics_PublishedAndReflectsAtomics — MONOTONICITY per
// counter. Drives each counter through the SAME memo primitives the production
// path uses (resolveMemo => miss+store then hit; InvalidateCRDSchemaMemo =>
// invalidation; a fenced store with a stale generation => stale_dropped), then
// reads the published Func to confirm it observes the +1. Each sub-assertion
// snapshots BEFORE (other tests in the same binary may have bumped the shared
// atomics) and checks the delta, so it is robust to test ordering.
func TestCRDSchemaMemoMetrics_PublishedAndReflectsAtomics(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	registerCRDSchemaMemoMetrics()
	resetCRDSchemaMemoForTest()
	t.Cleanup(resetCRDSchemaMemoForTest)

	// --- misses + hits: first resolve is a miss+store, second is a hit. ---
	beforeMiss := crdSchemaMemoStats().Misses
	beforeHit := crdSchemaMemoStats().Hits
	crd := tinyCRD(gvrA.Version)
	resolveMemo(t, gvrA, crd, gvrA.Version) // miss -> compile -> store
	resolveMemo(t, gvrA, crd, gvrA.Version) // hit

	afterMiss := crdSchemaMemoStats().Misses
	if afterMiss != beforeMiss+1 {
		t.Fatalf("misses snapshot: got %d want %d", afterMiss, beforeMiss+1)
	}
	if got, want := expvarUint(t, "snowplow_crd_schema_memo_misses_total"), wantJSONUint(t, afterMiss); got != want {
		t.Fatalf("snowplow_crd_schema_memo_misses_total expvar = %s, want %s", got, want)
	}

	afterHit := crdSchemaMemoStats().Hits
	if afterHit != beforeHit+1 {
		t.Fatalf("hits snapshot: got %d want %d", afterHit, beforeHit+1)
	}
	if got, want := expvarUint(t, "snowplow_crd_schema_memo_hits_total"), wantJSONUint(t, afterHit); got != want {
		t.Fatalf("snowplow_crd_schema_memo_hits_total expvar = %s, want %s", got, want)
	}

	// --- invalidations: InvalidateCRDSchemaMemo bumps the reset counter. ---
	beforeInval := crdSchemaMemoStats().Resets
	InvalidateCRDSchemaMemo()
	afterInval := crdSchemaMemoStats().Resets
	if afterInval != beforeInval+1 {
		t.Fatalf("invalidations snapshot: got %d want %d", afterInval, beforeInval+1)
	}
	if got, want := expvarUint(t, "snowplow_crd_schema_memo_invalidations_total"), wantJSONUint(t, afterInval); got != want {
		t.Fatalf("snowplow_crd_schema_memo_invalidations_total expvar = %s, want %s", got, want)
	}

	// --- stale_dropped: a fenced store whose snapshot generation is stale
	//     (we bump the generation via InvalidateCRDSchemaMemo AFTER snapshotting)
	//     must be dropped, bumping the stale-drop counter. ---
	beforeStale := crdSchemaMemoStats().StaleDropped
	staleGen := currentSchemaGen()        // snapshot
	InvalidateCRDSchemaMemo()             // moves the generation past staleGen
	storeCRDSchema(gvrB, mustCompile(t, tinyCRD(gvrB.Version), gvrB.Version), staleGen)
	afterStale := crdSchemaMemoStats().StaleDropped
	if afterStale != beforeStale+1 {
		t.Fatalf("stale_dropped snapshot: got %d want %d", afterStale, beforeStale+1)
	}
	if got, want := expvarUint(t, "snowplow_crd_schema_memo_stale_dropped_total"), wantJSONUint(t, afterStale); got != want {
		t.Fatalf("snowplow_crd_schema_memo_stale_dropped_total expvar = %s, want %s", got, want)
	}
}
