// refresher_stage_error_falsifier_test.go — Ship 0.30.120 pre-flight
// falsifier for the two-layer L1-poison fix.
//
// THE DEFECT (pre-existing since 0.30.113): the SA-transport L1
// refresher re-resolves the whole compositions-list RESTAction with no
// per-user JWT. Its allNamespacesAndCrds stage has exportJwt:true — a
// nested snowplow /call loopback needing the user's bearer token. The
// background refresher has none, so the loopback 401s; the stage's
// continueOnError swallows the 401 as a structurally-valid empty result;
// the refresher Puts a ~1.9 KB empty envelope over the user's correct
// ~26 MB entry. Uniform under-serve.
//
// COMMIT 1 of Ship 0.30.120 (this file's current state — the captured
// pre-flight artifact): the sink seam (cache.WithStageErrorSink) and the
// counters exist, but the error-aware Put-gate is NOT yet wired. Test 1
// therefore asserts the CURRENT BUGGY behaviour — the poisoned empty
// result OVERWRITES the good entry — and PASSES, proving the poison is
// real. COMMIT 2 lands the fix (the Put-gate + layer-(a) exportJwt skip)
// and INVERTS Test 1's assertion + adds Test 3. The inversion diff is
// the falsifier proof.

package dispatchers

import (
	"context"
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// goodEntry is the non-trivial "correct" L1 payload the refresher must
// not clobber. emptyEntry is the under-served result the poisoned
// re-resolve produces.
var (
	stageErrGoodEntry  = []byte(`{"list":[{"uid":"real"}]}`)
	stageErrEmptyEntry = []byte(`{"list":[]}`)
)

// --- Test 1 — poison reproducer / falsifier --------------------------------

// TestRefresher_StageErrorDoesNotOverwriteGoodEntry seeds L1 with a good
// entry, then runs resolveAndPopulateL1 with a stub that emulates the
// poisoned re-resolve: it bumps the stage-error sink (the api resolver
// does exactly this when it writes dict[call.ErrorKey] on a swallowed
// continueOnError'd inner-call failure) and returns an empty result.
//
// COMMIT 1 (pre-fix, captured falsifier artifact): the assertion is the
// CURRENT BUGGY behaviour — the empty result OVERWRITES the good entry.
// This test PASSES on the un-fixed code, proving the poison is real.
// COMMIT 2 inverts the assertion to the fixed behaviour (good entry
// kept).
func TestRefresher_StageErrorDoesNotOverwriteGoodEntry(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	cache.ResetResolvedCacheForTest()
	t.Cleanup(cache.ResetResolvedCacheForTest)

	c := cache.ResolvedCache()
	inputs := cache.ResolvedKeyInputs{
		CacheEntryClass: "restactions",
		Group:           "templates.krateo.io",
		Version:         "v1",
		Resource:        "restactions",
		Namespace:       "krateo-system",
		Name:            "compositions-list",
		Username:        "cyberjoker",
		Groups:          []string{"devs"},
	}
	key := cache.ComputeKey(inputs)
	c.Put(key, &cache.ResolvedEntry{RawJSON: stageErrGoodEntry, Inputs: &inputs})

	// Emulate the poisoned re-resolve: touch the stage-error sink that
	// resolveAndPopulateL1 installed on ctx (this is exactly what the api
	// resolver does at each dict[call.ErrorKey] write), then return the
	// under-served empty result.
	restore := setResolveOnceForTest(func(ctx context.Context, _ cache.ResolvedKeyInputs) ([]byte, error) {
		if sink := cache.StageErrorSinkFromContext(ctx); sink != nil {
			sink.Add(1)
		}
		return stageErrEmptyEntry, nil
	})
	t.Cleanup(restore)

	if err := resolveAndPopulateL1(context.Background(), inputs, nil, nil); err != nil {
		t.Fatalf("resolveAndPopulateL1 error: %v", err)
	}

	got, ok := c.Get(key)
	if !ok {
		t.Fatalf("entry vanished — expected it to still be present")
	}
	// COMMIT 1 (pre-fix): the un-fixed refresher Puts the empty result
	// over the good entry — this is the BUG. Asserting it here makes the
	// captured artifact PASS, proving the poison is real and reproducible.
	if string(got.RawJSON) != string(stageErrEmptyEntry) {
		t.Fatalf("pre-fix artifact: L1 entry = %q; want the EMPTY result %q "+
			"(commit-1 documents the BUG — the refresher overwrites the good "+
			"entry with the poisoned empty re-resolve)",
			got.RawJSON, stageErrEmptyEntry)
	}
}

// --- Test 2 — discriminator / no false positive ----------------------------

// TestRefresher_LegitimateEmptyIsStored proves that an empty result with
// NO stage error is stored normally — modelling a user who legitimately
// has zero compositions. This holds both pre- and post-fix: the gate
// (commit 2) keys on stage-error PRESENCE, never on result emptiness.
func TestRefresher_LegitimateEmptyIsStored(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	cache.ResetResolvedCacheForTest()
	t.Cleanup(cache.ResetResolvedCacheForTest)

	c := cache.ResolvedCache()
	inputs := cache.ResolvedKeyInputs{
		CacheEntryClass: "restactions",
		Resource:        "restactions",
		Namespace:       "krateo-system",
		Name:            "compositions-list",
		Username:        "loner",
	}
	key := cache.ComputeKey(inputs)
	c.Put(key, &cache.ResolvedEntry{RawJSON: stageErrGoodEntry, Inputs: &inputs})

	// Empty result, NO sink touch — a legitimate "0 compositions" outcome.
	restore := setResolveOnceForTest(func(_ context.Context, _ cache.ResolvedKeyInputs) ([]byte, error) {
		return stageErrEmptyEntry, nil
	})
	t.Cleanup(restore)

	if err := resolveAndPopulateL1(context.Background(), inputs, nil, nil); err != nil {
		t.Fatalf("resolveAndPopulateL1 error: %v", err)
	}

	got, ok := c.Get(key)
	if !ok {
		t.Fatalf("entry vanished — expected the legit empty result stored")
	}
	if string(got.RawJSON) != string(stageErrEmptyEntry) {
		t.Fatalf("legit empty NOT stored: L1 entry = %q; want %q "+
			"(no stage error => the gate must NOT fire; a legit empty IS stored)",
			got.RawJSON, stageErrEmptyEntry)
	}
}
