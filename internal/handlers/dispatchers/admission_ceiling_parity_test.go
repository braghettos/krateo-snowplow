// admission_ceiling_parity_test.go — the #64 parallel-copy DRIFT-GUARD
// (fold 2026-07-03, coordinator condition 1). The two ceiling arithmetics —
// cache.AdmissionCeiling() (the SHARED primitive the SEED bound uses) and
// api.admissionCeiling() (the nested-resolve bound's own inline copy, kept
// byte-identical to base) — are identical TODAY but have NO compile-time link
// (the nested bound was deliberately NOT rewired to call the shared primitive,
// the C1 choice). This test is the MECHANICAL backstop: inject the SAME
// (GOMEMLIMIT, liveHeap) into BOTH via their test seams and assert byte-identical
// (ceiling, unlimited) across a table. If one copy's reserve fraction / formula
// ever drifts, this goes RED.
//
// Lives in package dispatchers because it is the one package that imports BOTH
// cache and api (api imports cache, so a cache-side test cannot import api —
// cycle). Uses api's exported test-only shims (admission_parity_shim.go).
package dispatchers

import (
	"math"
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
	restapi "github.com/krateoplatformops/snowplow/internal/resolvers/restactions/api"
)

func TestAdmissionCeiling_ParityAcrossBothCopies(t *testing.T) {
	const gib = int64(1024 * 1024 * 1024)

	cases := []struct {
		name  string
		limit int64
		live  int64
		// wantCeiling / wantUnlimited are the EXPECTED shared result; the test
		// asserts BOTH copies equal each other AND equal this expectation, so a
		// drift in EITHER copy is caught (not just a divergence between them).
		wantCeiling   int64
		wantUnlimited bool
	}{
		{
			name: "unlimited (GOMEMLIMIT unset → MaxInt64)",
			limit: math.MaxInt64, live: 0,
			wantCeiling: 0, wantUnlimited: true,
		},
		{
			name: "normal 8Gi limit, 4Gi live → (8-4)Gi - 8Gi/8 = 3Gi",
			limit: 8 * gib, live: 4 * gib,
			wantCeiling: (8*gib - 4*gib) - (8*gib)/8, wantUnlimited: false,
		},
		{
			name: "negative headroom clamps to 0 (live == limit)",
			limit: 2 * gib, live: 2 * gib,
			wantCeiling: 0, wantUnlimited: false,
		},
		{
			name: "limit <= 0 treated as unlimited",
			limit: 0, live: 0,
			wantCeiling: 0, wantUnlimited: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limit, live := tc.limit, tc.live
			limitFn := func() int64 { return limit }
			liveFn := func() int64 { return live }

			// Inject the SAME seams into BOTH copies.
			restoreCache := cache.SetAdmissionRuntimeSeamsForTest(limitFn, liveFn)
			t.Cleanup(restoreCache)
			t.Cleanup(cache.ResetAdmissionRuntimeSeamsForTest)
			restapi.SetRuntimeSeamsForTest(limitFn, liveFn)
			t.Cleanup(restapi.ResetNestedResolveBoundForTest)

			cacheCeil, cacheUnlim := cache.AdmissionCeiling()
			apiCeil, apiUnlim := restapi.AdmissionCeilingForTest()

			// PARITY: the two hand-copied arithmetics must agree.
			if cacheCeil != apiCeil || cacheUnlim != apiUnlim {
				t.Fatalf("#64 DRIFT: cache.AdmissionCeiling()=(%d,%v) != api.admissionCeiling()=(%d,%v) "+
					"for (limit=%d, live=%d) — the two parallel ceiling copies have DRIFTED",
					cacheCeil, cacheUnlim, apiCeil, apiUnlim, limit, live)
			}
			// GROUND TRUTH: and both must equal the expected value (catches a
			// drift that happens to move BOTH copies the same wrong way).
			if cacheCeil != tc.wantCeiling || cacheUnlim != tc.wantUnlimited {
				t.Fatalf("ceiling = (%d,%v); want (%d,%v) for (limit=%d, live=%d)",
					cacheCeil, cacheUnlim, tc.wantCeiling, tc.wantUnlimited, limit, live)
			}
		})
	}
}
