// handler_iterator_merge_test.go — Ship D.4.1 (0.30.145) hermetic
// regression tests for the iterator-merge nil-skip in
// jsonHandlerCore's `case []any:` branch (handler.go).
//
// The defect class this catches (re-diagnosed at design §2.4 of
// docs/ship-d4-1-predicate-redesign-2026-05-20.md): an iterator over
// a RESTAction stage's `apiCall.path` template can yield a per-
// iteration `tmp == nil` (Ship A's EvalValue returns
// (nil, true, nil) on a gojq `null` result; an apistage `served=
// false` empty-response arm also yields `tmp == nil`).
// wrapAsSlice(nil) returns []any{nil}; the pre-D.4.1
// `append(existingSlice, v...)` then puts a literal Go nil into the
// merged downstream slice, and any subsequent gojq filter probing
// `.apiVersion` on that nil trips "cannot iterate over: null" —
// the original panels-500 symptom.
//
// Coverage per AC-D4.1.5:
//
//   1. Single literal-nil element → dropped; counter += 1 (per-stage
//      label = opts.key, AC-D4.1.11).
//   2. Multiple literal-nil elements → all dropped; counter += N.
//   3. Healthy []any (no nils) → byte-identical to pre-D.4.1
//      (same length AND same element identities); counter does NOT
//      increment.
//   4. Mixed (some nil, some healthy) → only nils dropped; healthy
//      preserved in order; counter == number-dropped.
//   5. Per-stage label assertion: the counter call uses opts.key
//      as the `gvr`-position label, NOT a GroupVersionResource
//      string and NOT empty.
//   6. The Ship D.4 over-filter case: tmp is a NamespaceList
//      envelope; the merge appends the envelope as ONE element
//      under opts.out[opts.key]. The new guard ignores nested
//      .items — it only drops literal nils at the outer merge
//      level (pin against the D.4 false-positive class recurring).
//
// No build tag — runs with the default `go test ./...`.
package api

import (
	"context"
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// nilSkipCounterFor returns the current value of the
// ReasonResolverNilMerge cell with the given stage-name (opts.key)
// label. The Ship D scope must be active on ctx for a counter
// increment to be recorded (Ship D contract — no scope = no
// counter; see cache.FallthroughScope short-circuit).
func nilSkipCounterFor(scope, stage string) uint64 {
	return cache.FallthroughCount(scope, stage, cache.ReasonResolverNilMerge)
}

// scopedCtx wraps ctx with the Ship D FallthroughScope so the
// guard's RecordApiserverFallthrough call actually increments the
// counter. Use a /call-class scope to mirror the production code
// path (the resolver runs inside a request handler middleware-
// stamped scope).
func scopedCtx(ctx context.Context) context.Context {
	return cache.WithFallthroughScope(ctx, cache.ScopeCallRestactions)
}

// runIteratorMerge drives jsonHandlerCore through the `case []any:`
// merge branch with an existing slice already under
// opts.out[opts.key] and a per-iteration tmp value. The harness
// resets the fallthrough counters and returns the resulting merged
// slice + the counter delta.
func runIteratorMerge(t *testing.T, stageName string, existing []any, tmp any) (merged []any, counterDelta uint64) {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")
	cache.ResetFallthroughCountersForTest()

	out := map[string]any{stageName: existing}
	opts := jsonHandlerOptions{key: stageName, out: out, filter: nil}
	ctx := scopedCtx(context.Background())

	before := nilSkipCounterFor(cache.ScopeCallRestactions, stageName)
	if err := jsonHandlerCore(ctx, opts, tmp); err != nil {
		t.Fatalf("jsonHandlerCore err = %v; want nil", err)
	}
	after := nilSkipCounterFor(cache.ScopeCallRestactions, stageName)

	got, ok := out[stageName].([]any)
	if !ok {
		t.Fatalf("out[%q] type = %T; want []any", stageName, out[stageName])
	}
	return got, after - before
}

// healthyMap returns a small populated map[string]any with the
// given `id` field — used as a "real merged item" payload for the
// merge tests.
func healthyMap(id string) map[string]any {
	return map[string]any{"id": id}
}

// ─────────────────────────────────────────────────────────────────────
// AC-D4.1.5 #1 — single literal-nil element dropped
// ─────────────────────────────────────────────────────────────────────

func TestJsonHandlerCore_IteratorMerge_DropsLiteralNil(t *testing.T) {
	stage := "allCompositionResources"
	healthy1 := healthyMap("h1")
	existing := []any{healthy1}

	// tmp == nil exercises wrapAsSlice's default branch:
	// wrapAsSlice(nil) returns []any{nil} — exactly the failing
	// case the design §2.4 traces.
	merged, delta := runIteratorMerge(t, stage, existing, nil)

	if len(merged) != 1 {
		t.Fatalf("merged length = %d; want 1 (nil dropped, healthy preserved)", len(merged))
	}
	if got := merged[0]; got == nil {
		t.Errorf("merged[0] is nil; want the healthy element preserved")
	}
	if delta != 1 {
		t.Errorf("counter delta = %d; want 1 (one literal-nil dropped)", delta)
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-D4.1.5 #2 — multiple literal-nil elements dropped (multi-yield)
// ─────────────────────────────────────────────────────────────────────

func TestJsonHandlerCore_IteratorMerge_DropsMultipleNils(t *testing.T) {
	stage := "allCompositionResources"
	h1 := healthyMap("h1")
	existing := []any{h1}

	// tmp is itself a []any whose elements alternate nils with
	// healthy values — simulates a multi-yield filter that
	// returned nils mixed with concrete items. The "predicate
	// inside the wrapAsSlice loop" placement (PM-explicit) is
	// what makes this case work: a shortcut `if tmp == nil`
	// BEFORE wrapAsSlice would not see these per-element nils.
	h2 := healthyMap("h2")
	h3 := healthyMap("h3")
	tmp := []any{nil, h2, nil, h3, nil}

	merged, delta := runIteratorMerge(t, stage, existing, tmp)

	if len(merged) != 3 {
		t.Fatalf("merged length = %d; want 3 (existing h1 + h2 + h3, three nils dropped)", len(merged))
	}
	// Healthy elements preserved in order.
	if merged[0] != any(h1) {
		t.Errorf("merged[0] != h1: got %v", merged[0])
	}
	if merged[1] != any(h2) {
		t.Errorf("merged[1] != h2: got %v", merged[1])
	}
	if merged[2] != any(h3) {
		t.Errorf("merged[2] != h3: got %v", merged[2])
	}
	if delta != 3 {
		t.Errorf("counter delta = %d; want 3 (three literal-nils dropped)", delta)
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-D4.1.5 #3 — healthy-only byte-identity canary
// ─────────────────────────────────────────────────────────────────────

// TestJsonHandlerCore_IteratorMerge_HealthyOnlyByteIdentical is the
// pre-D.4.1 byte-identity canary. With tmp containing only healthy
// elements and no nils, the merged slice's length AND element
// identities must be unchanged (no over-zealous filtering, no
// element substitution). Counter MUST NOT increment.
//
// This is also the AC-D4.1.12 element-identity pin: any regression
// that incorrectly drops healthy items fails this row immediately.
func TestJsonHandlerCore_IteratorMerge_HealthyOnlyByteIdentical(t *testing.T) {
	stage := "allCompositionResources"
	h1 := healthyMap("h1")
	h2 := healthyMap("h2")
	h3 := healthyMap("h3")
	existing := []any{h1}
	tmp := []any{h2, h3}

	merged, delta := runIteratorMerge(t, stage, existing, tmp)

	if len(merged) != 3 {
		t.Fatalf("merged length = %d; want 3 (existing + 2 healthy from tmp)", len(merged))
	}
	// Element identities preserved.
	if merged[0] != any(h1) {
		t.Errorf("merged[0] != h1: got %v", merged[0])
	}
	if merged[1] != any(h2) {
		t.Errorf("merged[1] != h2: got %v", merged[1])
	}
	if merged[2] != any(h3) {
		t.Errorf("merged[2] != h3: got %v", merged[2])
	}
	if delta != 0 {
		t.Errorf("counter delta = %d; want 0 (no nils → no increments)", delta)
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-D4.1.5 #4 — mixed: only nils dropped; healthy preserved
// ─────────────────────────────────────────────────────────────────────

func TestJsonHandlerCore_IteratorMerge_MixedNilsAndHealthy(t *testing.T) {
	stage := "compositionsList"
	h1 := healthyMap("alpha")
	h2 := healthyMap("beta")
	existing := []any{h1}
	// 1 nil + 1 healthy interleaved; predicate INSIDE wrapAsSlice
	// loop drops nils, keeps healthy.
	tmp := []any{nil, h2}

	merged, delta := runIteratorMerge(t, stage, existing, tmp)

	if len(merged) != 2 {
		t.Fatalf("merged length = %d; want 2 (existing + 1 healthy from tmp; 1 nil dropped)", len(merged))
	}
	if merged[0] != any(h1) || merged[1] != any(h2) {
		t.Errorf("merged element identities lost: %v", merged)
	}
	if delta != 1 {
		t.Errorf("counter delta = %d; want 1 (one nil dropped)", delta)
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-D4.1.11 — per-stage label assertion
// ─────────────────────────────────────────────────────────────────────

// TestJsonHandlerCore_IteratorMerge_PerStageLabel pins the counter's
// `gvr`-position label semantic: per AC-D4.1.11 the value MUST be
// opts.key (the STAGE NAME from jsonHandlerCore), NOT a
// GroupVersionResource string and NOT empty.
//
// Mechanism: drive two distinct stage names, drop one nil from each,
// and assert each stage's per-cell counter is exactly 1. If the
// implementation used an empty label, both increments would
// collapse into a single `"" | resolver-nil-merge` cell with
// count=2; if the implementation used a GVR string, the stage-
// keyed cells would stay at 0.
func TestJsonHandlerCore_IteratorMerge_PerStageLabel(t *testing.T) {
	stageA, stageB := "allCompositionResources", "getComposition"
	// Fresh counter state; runIteratorMerge resets, but we drive
	// two stages in sequence — only reset once at the start.
	t.Setenv("CACHE_ENABLED", "true")
	cache.ResetFallthroughCountersForTest()

	// Drive stage A with one nil.
	outA := map[string]any{stageA: []any{}}
	optsA := jsonHandlerOptions{key: stageA, out: outA, filter: nil}
	if err := jsonHandlerCore(scopedCtx(context.Background()), optsA, nil); err != nil {
		t.Fatalf("stageA jsonHandlerCore: %v", err)
	}
	// Drive stage B with one nil.
	outB := map[string]any{stageB: []any{}}
	optsB := jsonHandlerOptions{key: stageB, out: outB, filter: nil}
	if err := jsonHandlerCore(scopedCtx(context.Background()), optsB, nil); err != nil {
		t.Fatalf("stageB jsonHandlerCore: %v", err)
	}

	if got := nilSkipCounterFor(cache.ScopeCallRestactions, stageA); got != 1 {
		t.Errorf("counter[stageA=%q] = %d; want 1", stageA, got)
	}
	if got := nilSkipCounterFor(cache.ScopeCallRestactions, stageB); got != 1 {
		t.Errorf("counter[stageB=%q] = %d; want 1", stageB, got)
	}
	// The empty-label cell MUST be zero — confirms the
	// implementation did NOT use "" as the gvr-position label
	// (which would have collapsed both increments).
	if got := nilSkipCounterFor(cache.ScopeCallRestactions, ""); got != 0 {
		t.Errorf("counter[empty stage] = %d; want 0 (implementation must use opts.key, not empty)", got)
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-D4.1.5 #6 — Ship D.4 over-filter case NOT regressed
// ─────────────────────────────────────────────────────────────────────

// TestJsonHandlerCore_IteratorMerge_NamespacesListItemMerge pins the
// behaviour that the D.4.1 guard does NOT regress the original-D.4
// false-positive case: when tmp is a `NamespaceList` envelope (or
// any other LIST envelope whose items lack per-item TypeMeta by
// k8s convention), the iterator-merge appends the envelope as ONE
// element under opts.out[opts.key] — the new guard ignores nested
// .items (it only drops literal nils at the OUTER merge level).
//
// The envelope itself is NOT a literal nil; the guard's predicate
// fires only for `x == nil`. So the namespace envelope (with its
// items[] of TypeMeta-elided objects) is preserved verbatim. This
// is the structural reason Ship D.4.1 cannot regress the D.4
// over-filter class.
func TestJsonHandlerCore_IteratorMerge_NamespacesListItemMerge(t *testing.T) {
	stage := "namespaces"
	// A realistic NamespaceList envelope shape — apiserver elides
	// per-item TypeMeta for core groups (§1.1 of the design).
	envelope := map[string]any{
		"apiVersion": "v1",
		"kind":       "NamespaceList",
		"items": []any{
			map[string]any{"metadata": map[string]any{"name": "krateo-system"}},
			map[string]any{"metadata": map[string]any{"name": "default"}},
		},
	}
	existing := []any{}
	merged, delta := runIteratorMerge(t, stage, existing, envelope)

	if len(merged) != 1 {
		t.Fatalf("merged length = %d; want 1 (envelope is ONE element, not its items)", len(merged))
	}
	gotEnv, ok := merged[0].(map[string]any)
	if !ok {
		t.Fatalf("merged[0] type = %T; want map[string]any (the envelope)", merged[0])
	}
	if gotEnv["kind"] != "NamespaceList" {
		t.Errorf("merged[0].kind = %v; want NamespaceList (envelope preserved verbatim)", gotEnv["kind"])
	}
	if delta != 0 {
		t.Errorf("counter delta = %d; want 0 (envelope is not nil — no D.4-style over-filter)", delta)
	}
}
