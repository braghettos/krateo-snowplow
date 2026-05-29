// jsoncopy.go — Ship C (resolver-path rebuild, 0.30.139).
//
// HISTORICAL — NO LONGER ON THE SERVE PATH (Ship 2a, 0.30.209). The
// `listEnvelopeValue` deep copy this helper was written to make cheaper
// has been REMOVED: Ship 2a serves a SHALLOW envelope aliasing the shared
// items, made safe by an allocator-aware `deleteEmpty` in the gojq fork
// (gojq/func.go). `CopyJSONValue`/`CopyJSONMap` have no serve-path caller
// anymore. They are retained as a tested, upstream-byte-equivalent
// deep-copy utility (TestCopyJSONValue_UpstreamEquivalence remains the
// gate) for any future isolation need; they are NOT a correctness
// dependency of the serve path. NOTE the original rationale below cited
// gojq's `normalizeNumbers` (gojq@v0.12.17) as a reason the copy "cannot
// be removed" — that is OBSOLETE: upstream removed normalizeNumbers in
// v0.12.18 (the fork is v0.12.19), and the remaining `deleteEmpty`
// in-place writer was the one this ship eliminated via copy-on-write.
//
// ---- Original Ship C rationale (preserved for provenance) ----
//
// **Ship C — bounded-headroom ship, projected 0.7–1.3 GB / 60-call,
// hard-gate ≥0.6 GB at the migrated site, small-win-or-revert posture
// per PM ratification 2026-05-20.** (AC-C.12 — design doc + commit
// message + this file header all carry the same provenance line so a
// future bisect sees the projection.)
//
// The cost being attacked. The live Ship B (0.30.138) alloc diff shows
// `plumbing/maps.deepCopyJSONValue` at **3.68 GB / 60-call** —
// **68 % of the residual delta** — and **100 % of that flow traces
// through `listEnvelopeValue` at apistage.go:213-228**, the per-call
// content-cache isolation copy. (Pod snowplow-6d87899496-9lkhv, image
// 0.30.138@sha256:a36072a7…, artifacts at
// /tmp/snowplow-runs/0.30.138/heap_alloc_pre_b.pprof +
// .../heap_alloc_post_b.pprof.) ~61 MB / `/call` in deep-copy alone.
//
// What this ship actually buys.
//
//   - **`plumbing/maps.deepCopyJSONValue` is already a monomorphic
//     type-switch** (TRACED at deepcopy.go:11-53 — the verdict's
//     "reflective→monomorphic" framing was wrong, the architect
//     self-corrected). Ship C's reduction is **bounded — projected
//     0.7–1.3 GB / 60-call, gated at ≥0.6 GB conservative**.
//
//   - **Win mechanism #1 — leaf-fast-path case ordering.** Upstream
//     tests `map[string]any` first, then `[]any`, then `[]map[string]any`,
//     then the leaf-scalar block at deepcopy.go:42. For a JSON tree
//     where ~80 % of visited nodes are leaves, that means each leaf
//     pays three failed type checks before the matching `case` fires.
//     Ship C puts the leaves first so the dominant case hits on
//     iteration one of the type-switch table. Bounded per-leaf savings
//     (~5–10 ns), multiplied by ~190K leaves per content-cache hit.
//
//   - **Win mechanism #2 — in-package call-frame elision.** Upstream's
//     `DeepCopyJSON` lives in `github.com/krateoplatformops/plumbing/maps`;
//     each recursive descent pays a cross-module call frame. Ship C's
//     `CopyJSONValue` lives in the same package as its caller
//     (`apistage.go`'s `listEnvelopeValue`), so Go's mid-stack inliner
//     can collapse the recursion more aggressively. INFERRED:
//     the larger of the two contributors.
//
//   - **NOT a reflect→type-switch upgrade**; not an algorithmic O(N)
//     reduction. Both copiers walk the tree top-down and `make()` fresh
//     containers at the same asymptotic shape.
//
// Byte-equivalence with upstream is a hard contract. The type-switch
// cases here MUST produce a `reflect.DeepEqual`-identical tree to
// `plumbing/maps.DeepCopyJSON` for every input the migrated call site
// can reach AND every input in the upstream type set —
// `TestCopyJSONValue_UpstreamEquivalence` is the gate (AC-C.4). The
// idiosyncratic upstream conversions (`int → int64`, `int32 → int64`,
// `float32 → float64`, `[]map[string]any → []any`) are preserved
// verbatim. The panic-on-unknown is preserved verbatim (matches
// apistage.go:209-210's contract — a misshaped input loudly aborts the
// request rather than serving a corrupted tree).
//
// Cache-toggle compliance (AC-C.11). `CACHE_ENABLED=false`: pure compute
// helper reachable only when `listEnvelopeValue` runs (gated upstream by
// the content cache + the apistage gate). Flipping CACHE_ENABLED=false
// bypasses the call. No cache-toggle change.
package api

import (
	"encoding/json"
	"fmt"
)

// CopyJSONValue returns a deep copy of v that shares NO mutable state
// with v. Monomorphic — a direct type-switch over the documented JSON
// value-tree types, no reflect, no recursive plumbing.DeepCopyJSON.
//
// Accepted types — exactly the union `plumbing/maps.deepCopyJSONValue`
// accepts at upstream deepcopy.go:11-52:
//
//	nil, bool, int, int32, int64, float32, float64, string, json.Number,
//	[]any, map[string]any, []map[string]any
//
// Integer / floating-point conversions are preserved verbatim from
// upstream (`int → int64`, `int32 → int64`, `float32 → float64`) so a
// future input-source change (a custom decoder, Decoder.UseNumber(), an
// `[]int` cast that ever lands here) cannot silently diverge from
// upstream's byte-shape. The conversions are unreachable on today's
// `encoding/json.Unmarshal`-sourced inputs (which only produce
// `nil, bool, float64, string, []any, map[string]any`) — they exist for
// upstream parity, not for any current code path.
//
// Aliasing. Scalar branches return `x` as-is — scalars are immutable
// (string headers, int/float value types) and aliasing is safe.
// Map/slice branches `make()` fresh containers and recurse. No element
// of the returned tree shares storage with `v`.
//
// Panic-on-unknown. A non-JSON value (a struct, a chan, a typed-int
// other than int/int32/int64) panics with `fmt.Errorf("cannot deep copy
// %T", v)` — the verbatim upstream message at deepcopy.go:51. This is
// intentional: it preserves the `apistage.go:209-210` "panic on unknown"
// contract and keeps ops' vocabulary unchanged. A silent quiet-skip or a
// loud-WARN-and-pass-through would let a corrupted tree reach the
// resolver — the falsifier (§8.3 in the design) rejects that posture.
//
// Case ordering — leaves first. The case order below is the §2.2 win
// mechanism #1: `nil` and the immutable-scalar passthrough block hit
// first because they dominate the leaf node count (~80 % of nodes in a
// typical JSON tree by visit count). Container cases (`map[string]any`,
// `[]any`, `[]map[string]any`) come last. Ordering does not change
// correctness — a `case` matches on type, not position — but it makes
// the dominant code path the first table entry the Go type-switch
// compiler emits, with cache-locality benefits the architect projects
// at ~5–10 ns/leaf.
func CopyJSONValue(v any) any {
	switch x := v.(type) {

	// ---- Leaf fast path (the win mechanism #1) -------------------

	case nil:
		// Untyped nil — falls through here at the outer scope. Upstream
		// returns it at deepcopy.go:42 (folded into the scalar block).
		return nil

	case bool, int64, float64, string, json.Number:
		// True passthrough scalars — upstream deepcopy.go:42-43 returns
		// them as-is. Scalars are immutable (string is an immutable
		// header; the numeric types are value types); aliasing is safe.
		return x

	// ---- Upstream-parity numeric conversions ---------------------
	//
	// These three branches widen `int`/`int32` to `int64` and `float32`
	// to `float64`, mirroring upstream deepcopy.go:44-49 verbatim. They
	// are unreachable on today's `json.Unmarshal`-sourced inputs (which
	// produce only `nil, bool, float64, string, []any, map[string]any`)
	// — but we mirror upstream's widening anyway because (a)
	// byte-equivalence with upstream is a contract, and (b) a future
	// input-source change (a custom decoder, an `[]int` cast routed
	// through here) would silently lose precision if we no-oped them.
	// `TestCopyJSONValue_UpstreamEquivalence` exercises all three rows.

	case int:
		// Upstream deepcopy.go:44-45 — `case int: return int64(x)`.
		return int64(x)

	case int32:
		// Upstream deepcopy.go:46-47 — `case int32: return int64(x)`.
		return int64(x)

	case float32:
		// Upstream deepcopy.go:48-49 — `case float32: return float64(x)`.
		return float64(x)

	// ---- Containers ----------------------------------------------

	case map[string]any:
		if x == nil {
			// Typed nil — an `any` that contains a type `map[string]any`
			// with a value of nil. Upstream deepcopy.go:14-17 returns
			// the typed nil verbatim; we do the same.
			return x
		}
		clone := make(map[string]any, len(x))
		for k, sub := range x {
			clone[k] = CopyJSONValue(sub)
		}
		return clone

	case []any:
		if x == nil {
			// Typed nil — same posture as map. Upstream deepcopy.go:24-27.
			return x
		}
		clone := make([]any, len(x))
		for i, sub := range x {
			clone[i] = CopyJSONValue(sub)
		}
		return clone

	case []map[string]any:
		if x == nil {
			// Typed nil — same posture. Upstream deepcopy.go:34-36.
			return x
		}
		// Upstream deepcopy.go:37-41 promotes `[]map[string]any` to
		// `[]any` — the result's element type is `any`, NOT
		// `map[string]any`. We do the same; downstream consumers
		// (including a future caller that probes types via
		// `reflect.TypeOf`) expect that element-type promotion.
		// `TestCopyJSONValue_UpstreamEquivalence` asserts the result
		// type is `[]any` via reflect.
		clone := make([]any, len(x))
		for i, sub := range x {
			clone[i] = CopyJSONValue(sub)
		}
		return clone

	default:
		// Verbatim upstream deepcopy.go:50-52 panic message — preserves
		// the `apistage.go:209-210` contract and ops' vocabulary.
		panic(fmt.Errorf("cannot deep copy %T", v))
	}
}

// CopyJSONMap is the typed convenience analogue of
// `plumbing/maps.DeepCopyJSON` (upstream deepcopy.go:58-60): asserts the
// result to `map[string]any` so the migrated caller
// (`apistage.go:227 listEnvelopeValue`) does not pay a runtime
// type-assert at the call site and the call site stays a one-line drop-in
// replacement for `maps.DeepCopyJSON`.
//
// The assertion is safe by construction: `CopyJSONValue` of a
// `map[string]any` is the `case map[string]any` branch above, which
// returns a `map[string]any` — and the typed-nil branch returns the
// same typed-nil `map[string]any(nil)`. Both successfully type-assert
// to `map[string]any`.
func CopyJSONMap(m map[string]any) map[string]any {
	return CopyJSONValue(m).(map[string]any)
}
