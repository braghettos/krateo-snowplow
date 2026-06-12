// schema_cache_test.go — Task #323 (#318-R2 Commit 2-B) falsifiers for the
// per-GVR compiled-CRD-schema memo (schema_cache.go).
//
// These are PLAIN unit tests (no build tag) so they run in the default gate.
// They mirror the dynamic-package Commit-1 shapes:
//   - TestCRDSchemaMemo_RecompileCountNtoOne pairs the RED baseline
//     (per-call compile fires N times, like TestPerCallClient_DownloadsEachCall)
//     with the GREEN memo (compile fires ONCE across N resolves, like
//     TestSharedSADiscoveryClient_BootSmoke...Reuse). The compile step is
//     counted via the compileCRDSchemaFn seam (extract.go).
//   - TestCRDSchemaMemo_InvalidateForcesRecompile pins that
//     InvalidateCRDSchemaMemo() (the bridge reset) forces the next resolve to
//     recompile (count +1) — the GREEN-side reset semantics.
//   - TestCRDSchemaMemo_ByteIdenticalMemoisedVsFresh is the never-change-output
//     gate: the memoised CRV validates a real document byte-identically to a
//     fresh-compiled CRV (same accept + same reject + same error string).
//
// NOTE (Task #312, resolved): TestExtractOpenAPISchemaFromCRD (extract_test.go)
// previously failed+SIGSEGV'd on a stale fixture ("schema OpenAPI v3 not found
// for version: v1") — its CRD stopped at openAPIV3Schema.type while the contract
// drills into properties.spec.properties.widgetData — and the panic poisoned the
// whole package binary, forcing a `-run` filter to score these tests. That
// fixture is now corrected and the nil-deref site is require-guarded, so the
// package runs GREEN as a FULL PACKAGE with no -run filter.

package schema

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtimeschema "k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

// gvrA / gvrB are two distinct composition GVRs used to key the memo. No
// special-case literal in production code — these are test fixtures only.
var (
	gvrA = runtimeschema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-2-2", Resource: "panelsa"}
	gvrB = runtimeschema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-2-2", Resource: "panelsb"}
)

// tinyCRD is the smallest CRD shape extractOpenAPISchemaFromCRD accepts: a
// versions[] entry whose schema.openAPIV3Schema.properties.spec.properties has
// a widgetData node. Mirrors the working extract path (validate_test.go's CRD
// fixture is the larger real one used by the byte-identity test below).
func tinyCRD(version string) map[string]any {
	return map[string]any{
		"spec": map[string]any{
			"versions": []any{
				map[string]any{
					"name": version,
					"schema": map[string]any{
						"openAPIV3Schema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"spec": map[string]any{
									"type": "object",
									"properties": map[string]any{
										widgetDataKey: map[string]any{
											"type": "object",
											"properties": map[string]any{
												"title": map[string]any{"type": "string"},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// resolveMemo mimics ValidateObjectStatus's memo branch (schema.go) WITHOUT
// the apiserver-bound CRD GET / cache.GVRFor: on a memo hit it returns the
// cached CRV; on a miss it compiles via the SAME compileCRDSchemaFn seam the
// production path uses and stores it. This is the unit under test for the
// recompile-count discrimination (the seam counts compiles identically on the
// production path). Returns the CRV so callers can assert identity.
func resolveMemo(t *testing.T, gvr runtimeschema.GroupVersionResource, crd map[string]any, version string) *apiextensions.CustomResourceValidation {
	t.Helper()
	if crv, hit := lookupCRDSchema(gvr); hit {
		return crv
	}
	// Snapshot the generation BEFORE compile, exactly as ValidateObjectStatus
	// does before its CRD GET (architect A1 fence).
	gen := currentSchemaGen()
	crv, err := compileCRDSchemaFn(crd, version)
	if err != nil {
		t.Fatalf("compileCRDSchemaFn(%v) errored: %v", gvr, err)
	}
	storeCRDSchema(gvr, crv, gen)
	return crv
}

// TestCRDSchemaMemo_RecompileCountNtoOne is the headline RED/GREEN falsifier:
// per-call recompile count N -> 1 once the memo is engaged.
//
//   - RED baseline (no memo): a fresh compile every resolve fires the seam N
//     times — exactly what ValidateObjectStatus did before this ship
//     (extractOpenAPISchemaFromCRD on every child GET, schema.go:126 pre-fix).
//   - GREEN (memo): N resolves for the same GVR compile EXACTLY ONCE; the rest
//     are memo hits.
func TestCRDSchemaMemo_RecompileCountNtoOne(t *testing.T) {
	resetCRDSchemaMemoForTest()
	t.Cleanup(resetCRDSchemaMemoForTest)

	const N = 8

	// Count compiles via the seam (mirrors fakeCachedDiscovery.groupsCalls).
	var compiles int
	orig := compileCRDSchemaFn
	compileCRDSchemaFn = func(crd map[string]any, version string) (*apiextensions.CustomResourceValidation, error) {
		compiles++
		return orig(crd, version)
	}
	t.Cleanup(func() { compileCRDSchemaFn = orig })

	crd := tinyCRD("v1-2-2")

	// --- RED baseline: NO memo (compile every iteration) ---------------------
	// Reproduce the pre-fix per-call cost: bypass the memo and compile
	// directly N times. The seam fires N times.
	compiles = 0
	for i := 0; i < N; i++ {
		if _, err := compileCRDSchemaFn(crd, "v1-2-2"); err != nil {
			t.Fatalf("RED iteration %d compile errored: %v", i, err)
		}
	}
	t.Logf("RED baseline (per-call compile, no memo): compileCRDSchemaFn called %d times across %d resolves. GREEN (memo) = 1.", compiles, N)
	if compiles != N {
		t.Fatalf("RED baseline expected N=%d compiles (one per resolve); got %d", N, compiles)
	}

	// --- GREEN: WITH memo (compile once) -------------------------------------
	resetCRDSchemaMemoForTest()
	compiles = 0
	var first *apiextensions.CustomResourceValidation
	for i := 0; i < N; i++ {
		crv := resolveMemo(t, gvrA, crd, "v1-2-2")
		if crv == nil {
			t.Fatalf("GREEN iteration %d returned nil CRV", i)
		}
		if first == nil {
			first = crv
			continue
		}
		// Pointer identity: every hit returns the SAME cached CRV.
		if crv != first {
			t.Fatalf("GREEN iteration %d returned a DIFFERENT CRV pointer than the first "+
				"resolve (%p vs %p) — the memo is not sharing the compiled value", i, crv, first)
		}
	}
	if compiles != 1 {
		t.Fatalf("GREEN expected EXACTLY 1 compile across %d resolves on the warm memo; got %d "+
			"(the per-GVR memo is being recompiled per call — memo broken)", N, compiles)
	}

	// Counters reflect 1 miss + (N-1) hits for gvrA.
	s := crdSchemaMemoStats()
	if s.Misses != 1 {
		t.Errorf("memo Misses=%d want 1 (the first resolve)", s.Misses)
	}
	if s.Hits != uint64(N-1) {
		t.Errorf("memo Hits=%d want %d (resolves 2..N)", s.Hits, N-1)
	}
}

// TestCRDSchemaMemo_PerGVRKeying pins that DISTINCT GVRs are memoised
// independently — a hit for gvrA must not serve gvrB's (different) schema.
// Compiles fire once per distinct GVR, not once globally.
func TestCRDSchemaMemo_PerGVRKeying(t *testing.T) {
	resetCRDSchemaMemoForTest()
	t.Cleanup(resetCRDSchemaMemoForTest)

	var compiles int
	orig := compileCRDSchemaFn
	compileCRDSchemaFn = func(crd map[string]any, version string) (*apiextensions.CustomResourceValidation, error) {
		compiles++
		return orig(crd, version)
	}
	t.Cleanup(func() { compileCRDSchemaFn = orig })

	crd := tinyCRD("v1-2-2")

	crvA1 := resolveMemo(t, gvrA, crd, "v1-2-2")
	crvB1 := resolveMemo(t, gvrB, crd, "v1-2-2")
	crvA2 := resolveMemo(t, gvrA, crd, "v1-2-2")
	crvB2 := resolveMemo(t, gvrB, crd, "v1-2-2")

	if compiles != 2 {
		t.Fatalf("expected 2 compiles (one per distinct GVR); got %d — keying is wrong "+
			"(either over-shared across GVRs or not memoised at all)", compiles)
	}
	if crvA1 != crvA2 {
		t.Errorf("gvrA resolves returned distinct CRV pointers — per-GVR memo not reused")
	}
	if crvB1 != crvB2 {
		t.Errorf("gvrB resolves returned distinct CRV pointers — per-GVR memo not reused")
	}
	if crvA1 == crvB1 {
		t.Errorf("gvrA and gvrB returned the SAME CRV pointer — the memo is collapsing " +
			"distinct GVR keys (cross-GVR contamination)")
	}
}

// TestCRDSchemaMemo_InvalidateForcesRecompile pins the bridge-reset semantics:
// after InvalidateCRDSchemaMemo() the next resolve recompiles (count +1), then
// re-warms. Mirrors the InvalidateSADiscovery() arm of the Commit-1 boot-smoke.
func TestCRDSchemaMemo_InvalidateForcesRecompile(t *testing.T) {
	resetCRDSchemaMemoForTest()
	t.Cleanup(resetCRDSchemaMemoForTest)

	var compiles int
	orig := compileCRDSchemaFn
	compileCRDSchemaFn = func(crd map[string]any, version string) (*apiextensions.CustomResourceValidation, error) {
		compiles++
		return orig(crd, version)
	}
	t.Cleanup(func() { compileCRDSchemaFn = orig })

	crd := tinyCRD("v1-2-2")

	// Warm: 1 compile, then hits.
	for i := 0; i < 5; i++ {
		resolveMemo(t, gvrA, crd, "v1-2-2")
	}
	if compiles != 1 {
		t.Fatalf("after warm: compiles=%d want 1", compiles)
	}

	// Invalidate (the bridge reset) — the next resolve MUST recompile.
	InvalidateCRDSchemaMemo()
	resolveMemo(t, gvrA, crd, "v1-2-2")
	if compiles != 2 {
		t.Fatalf("after InvalidateCRDSchemaMemo() + one resolve: compiles=%d want 2 — "+
			"the reset did not force a recompile (invalidation broken)", compiles)
	}

	// Re-warm: stays at 2.
	for i := 0; i < 5; i++ {
		resolveMemo(t, gvrA, crd, "v1-2-2")
	}
	if compiles != 2 {
		t.Fatalf("after re-warm: compiles=%d want 2 (post-reset rebuild should be reused)", compiles)
	}

	s := crdSchemaMemoStats()
	if s.Resets != 1 {
		t.Errorf("memo Resets=%d want 1", s.Resets)
	}
}

// TestInvalidateCRDSchemaMemo_BeforeStore_NoPanic pins that resetting before
// any entry exists is a soft no-op (the bridge may fire on a CRD event before
// any ValidateObjectStatus has populated the memo). Mirrors
// TestInvalidateSADiscovery_BeforeBuild_NoPanic.
func TestInvalidateCRDSchemaMemo_BeforeStore_NoPanic(t *testing.T) {
	resetCRDSchemaMemoForTest()
	t.Cleanup(resetCRDSchemaMemoForTest)
	InvalidateCRDSchemaMemo() // must not panic with an empty memo
	if s := crdSchemaMemoStats(); s.Resets != 1 {
		t.Errorf("Resets=%d want 1 after one invalidate on an empty memo", s.Resets)
	}
}

// TestValidateObjectStatus_MemoHit_StillFailsClosedOnAbsentWidgetData pins the
// ORDERING invariant the memo MUST preserve: a memo hit must NEVER bypass the
// status.widgetData-absent NotFound guard. We drive the REAL ValidateObjectStatus
// with a BUILT-IN GVK object (so cache.GVRFor resolves offline via
// builtinGVKToGVR — no apiserver) that has NO status.widgetData, AND we
// pre-warm the memo for that GVR. Pre-fix-ordering (memo branch BEFORE the
// !ok guard) would validate a nil widgetData against the cached CRV; correct
// ordering returns a NotFound StatusError on hit exactly as on miss.
//
// This is the guard that catches the memo-branch-misplacement regression
// (a status-only check would not have).
func TestValidateObjectStatus_MemoHit_StillFailsClosedOnAbsentWidgetData(t *testing.T) {
	resetCRDSchemaMemoForTest()
	t.Cleanup(resetCRDSchemaMemoForTest)

	// A built-in GVK — ConfigMap — resolves through builtinGVKToGVR with no
	// network. The object deliberately has NO status.widgetData.
	obj := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": "no-widgetdata", "namespace": "default"},
		// no "status" / no "widgetData"
	}
	gvk := runtimeschema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	gvr, err := cacheGVRForOffline(t, gvk)
	if err != nil {
		t.Skipf("ConfigMap GVK did not resolve offline (builtin map shape changed): %v — "+
			"ordering invariant still covered by code review", err)
	}

	// Pre-WARM the memo for this GVR with any valid CRV — a memo HIT is now
	// guaranteed if the lookup runs.
	storeCRDSchema(gvr, mustCompile(t, tinyCRD("v1-2-2"), "v1-2-2"), currentSchemaGen())

	// The REAL ValidateObjectStatus. rc is unused on the NotFound path (the
	// guard fires before any CRD GET), so a dummy rc is fine.
	verr := ValidateObjectStatus(context.Background(), nil, obj)
	if verr == nil {
		t.Fatalf("ValidateObjectStatus returned nil for a status.widgetData-absent object " +
			"with a WARM memo — the memo hit BYPASSED the NotFound fail-closed guard " +
			"(ORDERING regression: the memo branch must come AFTER the !ok guard)")
	}
	// Must be the NotFound StatusError, not a validation error against nil.
	se, ok := verr.(*apierrors.StatusError)
	if !ok {
		t.Fatalf("ValidateObjectStatus returned %T (%v), want *apierrors.StatusError NotFound — "+
			"a non-StatusError means the memo hit ran validateCustomResource against a nil "+
			"widgetData instead of failing closed", verr, verr)
	}
	if se.ErrStatus.Reason != metav1.StatusReasonNotFound {
		t.Fatalf("ValidateObjectStatus StatusError reason=%q, want NotFound", se.ErrStatus.Reason)
	}
}

// cacheGVRForOffline resolves a built-in GVK to its GVR without an apiserver,
// via the production cache.GVRFor (builtinGVKToGVR arm). Returns the error if
// the GVK is not built-in (so the test skips rather than hitting the network).
func cacheGVRForOffline(t *testing.T, gvk runtimeschema.GroupVersionKind) (runtimeschema.GroupVersionResource, error) {
	t.Helper()
	return cache.GVRFor(context.Background(), gvk, nil)
}

// TestCRDSchemaMemo_ByteIdenticalMemoisedVsFresh is the never-change-output
// gate (feedback_cache_must_not_constrain_jq / feedback_validate_content_not_just_status):
// validateCustomResource against a MEMOISED CRV produces byte-identical results
// (same accept, same reject, same error string) as against a FRESH-compiled CRV
// for the same (CRD, object). Uses the real testdata CRD the working
// validate_test.go exercises, so the full extract+build path runs.
func TestCRDSchemaMemo_ByteIdenticalMemoisedVsFresh(t *testing.T) {
	resetCRDSchemaMemoForTest()
	t.Cleanup(resetCRDSchemaMemoForTest)

	data, err := os.ReadFile("../../../../testdata/missing-additional-props/table.crd.yaml")
	if err != nil {
		t.Fatalf("read CRD fixture: %v", err)
	}
	var crd map[string]any
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("unmarshal CRD fixture: %v", err)
	}
	const version = "v1beta1"

	// FRESH: compile directly (the pre-memo path).
	freshCRV, err := extractOpenAPISchemaFromCRD(crd, version)
	if err != nil {
		t.Fatalf("fresh extract errored: %v", err)
	}

	// MEMOISED: compile once via the memo, then read back the cached CRV.
	gvr := runtimeschema.GroupVersionResource{Group: "widgets.templates.krateo.io", Version: version, Resource: "tables"}
	storeCRDSchema(gvr, mustCompile(t, crd, version), currentSchemaGen())
	memoCRV, hit := lookupCRDSchema(gvr)
	if !hit || memoCRV == nil {
		t.Fatalf("memoised CRV not found after store")
	}

	// Load the real object's widgetData (same path validate_test.go uses).
	doc, err := os.ReadFile("../../../../testdata/missing-additional-props/table.json")
	if err != nil {
		t.Fatalf("read object fixture: %v", err)
	}
	var jsonObj map[string]any
	if err := json.Unmarshal(doc, &jsonObj); err != nil {
		t.Fatalf("unmarshal object fixture: %v", err)
	}
	spec, _ := jsonObj["spec"].(map[string]any)
	if spec == nil {
		t.Fatalf("fixture object has no spec map")
	}
	widgetData, _ := spec[widgetDataKey].(map[string]any)
	if widgetData == nil {
		t.Fatalf("fixture object has no spec.widgetData map")
	}

	// Validate the REAL widgetData against both CRVs. The fixture is the
	// missing-additional-props case the working test asserts ERRORS — so both
	// must error, with the SAME error SET.
	//
	// NOTE — the validation outcome is compared as a SORTED SET of the
	// individual sub-errors, NOT the raw aggregate string. k8s'
	// validation.ValidateCustomResource (kube-openapi) collects "forbidden
	// property" errors by ranging a Go MAP, so the ORDER of the comma-joined
	// aggregate string is non-deterministic ACROSS RUNS — two FRESH compiles
	// of the SAME CRV also produce different aggregate-string orderings (see
	// the freshVsFresh self-baseline below). The never-change-output contract
	// is "same validation SET", which a sorted-set compare captures exactly
	// and which a raw-string compare would falsely flag as a regression.
	freshErr := validateCustomResource(freshCRV, widgetData)
	memoErr := validateCustomResource(memoCRV, widgetData)
	if (freshErr == nil) != (memoErr == nil) {
		t.Fatalf("validity DIVERGED: fresh err=%v, memo err=%v — the memoised CRV "+
			"changed the validation outcome (never-change-output VIOLATED)", freshErr, memoErr)
	}
	if freshErr != nil && memoErr != nil {
		freshSet := sortedErrParts(freshErr)
		memoSet := sortedErrParts(memoErr)
		if !equalStringSlices(freshSet, memoSet) {
			t.Fatalf("error SETS DIVERGED (order-independent compare):\n  fresh: %v\n  memo:  %v\n"+
				"the memoised CRV produced a DIFFERENT validation error set "+
				"(never-change-output VIOLATED)", freshSet, memoSet)
		}
		// Self-baseline: prove the SET is stable while the RAW STRING is not —
		// i.e. document why we compare sets. Two fresh compiles must agree on
		// the SET; they may disagree on the raw string.
		freshErr2 := validateCustomResource(mustCompile(t, crd, version), widgetData)
		if !equalStringSlices(sortedErrParts(freshErr2), freshSet) {
			t.Fatalf("fresh-vs-fresh error SETS diverged — the sorted-set invariant " +
				"itself is unstable; the byte-identity gate is mis-modelled")
		}
	}

	// Cross-check the EMPTY doc too: against THIS (missing-additional-props)
	// fixture CRV, {} actually REJECTS ([columns: Required value, data:
	// Required value] — the CRD's spec.widgetData requires columns+data), so
	// this is a both-REJECT parity check (error/non-error parity must match),
	// NOT a nil-nil positive identity. The deterministic POSITIVE path (a
	// valid object -> nil error against both CRVs) is pinned separately by
	// TestCRDSchemaMemo_PositivePathIdentity_NilBothWays (PM C1) — the empty
	// doc cannot serve that role on this fixture.
	empty := map[string]any{}
	fv := validateCustomResource(freshCRV, empty)
	mv := validateCustomResource(memoCRV, empty)
	if (fv == nil) != (mv == nil) {
		t.Fatalf("empty-doc validity PARITY DIVERGED: fresh=%v memo=%v", fv, mv)
	}
}

// TestCRDSchemaMemo_PositivePathIdentity_NilBothWays is the PM-C1 deterministic
// POSITIVE-path never-change-output gate: a genuinely VALID object validates to
// a nil error against BOTH the fresh-compiled and the memoised CRV. Unlike the
// error path (sorted-SET compare, because k8s aggregates errors via a Go map in
// non-deterministic order), the accept path is a single nil — fully
// deterministic, no map-order ambiguity. This is the cleaner half of
// feedback_cache_must_not_constrain_jq / feedback_validate_content_not_just_status:
// the cache must not constrain VALID output.
//
// Fixture: tinyCRD's widgetData schema is {type:object,
// properties:{title:{type:string}}} with additionalProperties forced false by
// enforceStrictObjects, so {"title":"hello"} is valid (verified: nil error;
// an unknown field rejects, so the schema is non-vacuous).
func TestCRDSchemaMemo_PositivePathIdentity_NilBothWays(t *testing.T) {
	resetCRDSchemaMemoForTest()
	t.Cleanup(resetCRDSchemaMemoForTest)

	crd := tinyCRD("v1-2-2")
	const version = "v1-2-2"

	// FRESH compile (the pre-memo path).
	freshCRV, err := extractOpenAPISchemaFromCRD(crd, version)
	if err != nil {
		t.Fatalf("fresh extract errored: %v", err)
	}

	// MEMOISED: compile once via the memo, read back the cached CRV.
	gvr := runtimeschema.GroupVersionResource{Group: "composition.krateo.io", Version: version, Resource: "positivepanels"}
	storeCRDSchema(gvr, mustCompile(t, crd, version), currentSchemaGen())
	memoCRV, hit := lookupCRDSchema(gvr)
	if !hit || memoCRV == nil {
		t.Fatalf("memoised CRV not found after store")
	}

	// A genuinely VALID widgetData — must validate to nil against BOTH.
	valid := map[string]any{"title": "hello"}
	if fe := validateCustomResource(freshCRV, valid); fe != nil {
		t.Fatalf("fresh CRV REJECTED a valid object (%v) — fixture is not actually valid; "+
			"the positive-path gate is mis-built", fe)
	}
	if me := validateCustomResource(memoCRV, valid); me != nil {
		t.Fatalf("memoised CRV REJECTED a valid object (%v) while fresh ACCEPTED it — the "+
			"memo CHANGED the accept-path outcome (never-change-output VIOLATED on the "+
			"positive path)", me)
	}
}

// mustCompile is a tiny helper for the byte-identity test — compiles via the
// real (un-seamed) extractOpenAPISchemaFromCRD.
func mustCompile(t *testing.T, crd map[string]any, version string) *apiextensions.CustomResourceValidation {
	t.Helper()
	crv, err := extractOpenAPISchemaFromCRD(crd, version)
	if err != nil {
		t.Fatalf("mustCompile errored: %v", err)
	}
	return crv
}

// sortedErrParts splits a validation aggregate error into its comma-separated
// sub-errors and sorts them, so two error values carrying the SAME SET of
// sub-errors in DIFFERENT order compare equal. This is the order-independent
// "same validation outcome" representation (see the gate's NOTE).
//
// k8s' errs.ToAggregate().Error() wraps a multi-error as "[part0, part1, ...,
// partN]". The surrounding "[" / "]" must be trimmed BEFORE splitting,
// otherwise the trailing "]" sticks to whichever sub-error landed LAST in the
// (map-order-dependent, non-deterministic) UNSORTED order — making two
// otherwise-identical sets differ by a stray bracket on one element. Trimming
// the brackets first yields a truly stable set (verified: two fresh compiles
// then match element-for-element).
func sortedErrParts(err error) []string {
	if err == nil {
		return nil
	}
	s := strings.TrimSpace(err.Error())
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	parts := strings.Split(s, ", ")
	sort.Strings(parts)
	return parts
}

// equalStringSlices reports whether two string slices are element-wise equal.
func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
