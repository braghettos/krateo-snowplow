package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/ptr"
	"github.com/krateoplatformops/snowplow/internal/cache"
	jqsupport "github.com/krateoplatformops/snowplow/internal/support/jq"
)

type jsonHandlerOptions struct {
	key    string
	out    map[string]any
	filter *string
}

// jsonHandler is the HTTP-body-shaped entry point — the form
// httpcall.Do's ResponseHandler contract requires. It io.ReadAll's the
// response body, json.Unmarshal's it, and hands the decoded value to
// jsonHandlerCore. Use this ONLY for a genuine HTTP-body stream
// (httpcall.Do); for an in-memory dispatch result use jsonHandlerBytes
// (raw []byte) or jsonHandlerValue (already-decoded value) — Ship
// 0.30.128 P-CORE-1: those skip the redundant io.ReadAll copy that every
// cache hit otherwise pays.
func jsonHandler(ctx context.Context, opts jsonHandlerOptions) func(io.ReadCloser) error {
	return func(in io.ReadCloser) error {
		dat, err := io.ReadAll(in)
		if err != nil {
			return err
		}
		return jsonHandlerBytesApply(ctx, opts, dat)
	}
}

// jsonHandlerBytes is the []byte-direct entry point — Ship 0.30.128
// P-CORE-1. For an in-memory dispatch result (informer-served bytes, an
// in-process nested /call's Status.Raw, the internal-rest-config
// dispatch) the bytes are ALREADY in memory; wrapping them in an
// io.ReadCloser only so jsonHandler can io.ReadAll them straight back
// out is a redundant full copy paid on every call. jsonHandlerBytes
// json.Unmarshal's the bytes directly and skips that copy.
func jsonHandlerBytes(ctx context.Context, opts jsonHandlerOptions) func([]byte) error {
	return func(dat []byte) error {
		return jsonHandlerBytesApply(ctx, opts, dat)
	}
}

// jsonHandlerValue is the decoded-value-direct entry point — Ship
// 0.30.128 P-CORE-2. For an apistage content-cache hit the gated
// envelope is already a structured value (map[string]any); marshalling
// it to []byte only for jsonHandler to unmarshal it right back is a
// redundant decode paid on every cache hit. jsonHandlerValue feeds the
// already-decoded value straight to jsonHandlerCore — no marshal, no
// unmarshal.
func jsonHandlerValue(ctx context.Context, opts jsonHandlerOptions) func(any) error {
	return func(decoded any) error {
		return jsonHandlerCore(ctx, opts, decoded)
	}
}

// jsonHandlerBytesApply json.Unmarshal's dat then runs jsonHandlerCore.
// The shared body behind jsonHandler (HTTP path) and jsonHandlerBytes
// (in-memory []byte path).
func jsonHandlerBytesApply(ctx context.Context, opts jsonHandlerOptions, dat []byte) error {
	var tmp any
	if err := json.Unmarshal(dat, &tmp); err != nil {
		return err
	}
	return jsonHandlerCore(ctx, opts, tmp)
}

// jsonHandlerCore is the post-decode logic: wrap the decoded value under
// opts.key, apply the optional stage filter, and merge into opts.out.
// Byte-identical behaviour to the pre-0.30.128 jsonHandler from the
// decoded value onward (AC-128.4) — P-CORE-1/2 only change WHEN/WHERE
// the decode happens (once at entry-load, not once per cache-hit), never
// the merge/filter result.
func jsonHandlerCore(ctx context.Context, opts jsonHandlerOptions, tmp any) error {
	log := xcontext.Logger(ctx)

	pig := map[string]any{
		opts.key: tmp,
	}
	if si, ok := opts.out["slice"]; ok {
		pig["slice"] = si
	}

	if opts.filter != nil {
		q := ptr.Deref(opts.filter, "")
		log.Debug("found local filter on api result", slog.String("filter", q))
		// Ship A (0.30.137): EvalValue returns gojq's result value
		// directly — no jqutil encode-to-string + json.Unmarshal-back
		// round-trip (design §3.4.2). Behaviour byte-identical per the
		// §3.4.2 truth table.
		v, ok, err := EvalValue(context.TODO(), q, pig, jqsupport.ModuleLoader())
		switch {
		case errors.Is(err, ErrMultiYield):
			// Current: multi-yield -> invalid concatenated JSON ->
			// json.Unmarshal errors -> return err -> stage fails.
			return err
		case err != nil:
			// Parse/compile/runtime gojq-error. Current: log.Error, tmp
			// unchanged, stage continues. (jqutil.Eval err branch.)
			log.Error("unable to evaluate JQ filter",
				slog.String("filter", q), slog.Any("error", err))
		case !ok:
			// Zero-yield (jq `empty`). Current: jqutil.Eval returns "",
			// json.Unmarshal("") errors -> return err -> stage fails.
			return fmt.Errorf("jq filter %q yielded no value", q)
		default:
			// Single value. Current: json.Unmarshal(s) -> tmp. Ship A:
			// tmp = v directly (the §3.1-3.3 equivalence proof).
			tmp = v
			// [panel500-instr] site=4 — Ship A EvalValue default-arm
			// assignment. Architect §2.4: empirical question "does
			// the per-iteration stage filter ever produce v == nil?
			// If yes, Ship A's EvalValue returning (nil, true, nil)
			// is the proximate nil source for tmp." Fires AFTER tmp
			// is set so the value-side check matches the assigned
			// nil precisely.
			slog.Info("[panel500-instr] site=4 tag=evalvalue_default",
				slog.String("stage_key", opts.key),
				slog.String("filter", q),
				slog.Bool("v_is_nil", v == nil),
				slog.String("v_type", fmt.Sprintf("%T", v)),
			)
		}
	}

	got, ok := opts.out[opts.key]
	if !ok {
		// [panel500-instr] site=1 — first-iteration write to dict.
		// Architect's design §2.1: empirical question "does a
		// first-iteration write of tmp == nil ever happen for stage
		// {allCompositionResources, getComposition}?" A tmp_is_nil=true
		// first write means D.4.1's case []any: guard cannot catch
		// this defect — the literal nil lands directly in dict before
		// any later iteration enters either type-switch arm.
		slog.Info("[panel500-instr] site=1 tag=first_iteration_write",
			slog.String("stage_key", opts.key),
			slog.Bool("tmp_is_nil", tmp == nil),
			slog.String("tmp_type", fmt.Sprintf("%T", tmp)),
		)
		// [panel500-instr] site=11 — PM-added (dict-state-after-write
		// canary, distinct from site 1's source-side capture). Same
		// code location, complementary field shape: includes the
		// explicit is_first_iteration flag so the burst-log
		// cross-product can identify the (stage, first-iter) tuple
		// without inferring it from absence of `got`.
		slog.Info("[panel500-instr] site=11 tag=first-iteration-dict-write",
			slog.String("stage", opts.key),
			slog.String("tmp_type", fmt.Sprintf("%T", tmp)),
			slog.Bool("tmp_is_nil", tmp == nil),
			slog.Bool("is_first_iteration", true),
		)
		opts.out[opts.key] = tmp
		return nil
	}

	switch existingSlice := got.(type) {
	case []any:
		// [panel500-instr] site=3 — D.4.1 case []any: arm entry
		// (control). Architect §2.3: tester report says the
		// resolver-nil-merge counter never fired on the burst →
		// expect this log to NOT fire either. Confirms D.4.1 was on
		// the wrong arm. If site 3 DOES fire during burst, D.4.1's
		// site was reached but the counter wiring failed; if it
		// does NOT fire, the defect path stayed in the default: arm
		// (site 2).
		slog.Info("[panel500-instr] site=3 tag=case_anyslice_arm",
			slog.String("stage_key", opts.key),
			slog.Int("existing_len", len(existingSlice)),
			slog.String("tmp_type", fmt.Sprintf("%T", tmp)),
			slog.Bool("tmp_is_nil", tmp == nil),
		)
		// Ship D.4.1 (0.30.145) — iterator-merge nil-skip.
		//
		// The defect: an iterator over a RESTAction stage's
		// `apiCall.path` template can yield a per-iteration `tmp`
		// that is a literal Go `nil` — either because Ship A's
		// `EvalValue` returned `(nil, true, nil)` for a gojq `null`
		// result, or because the apistage cache hit a `served=false`
		// empty-response arm. `wrapAsSlice(nil)` returns
		// `[]any{nil}` — a single-element slice containing a
		// literal Go nil. The append-into-merged-slice would
		// otherwise put that nil into the merged downstream slice
		// under `opts.out[opts.key]`, and any subsequent gojq filter
		// probing `.apiVersion` (or any field) on that nil element
		// trips "cannot iterate over: null" — the original panels-500
		// symptom (re-diagnosed at design doc §2.4).
		//
		// The predicate runs INSIDE the wrapAsSlice loop — NOT
		// before it. A "shortcut `if tmp == nil`" before
		// wrapAsSlice would miss the case where `tmp` is itself a
		// `[]any` whose elements include a nil (multi-yield filter
		// returning nils interleaved with healthy values). The
		// actual failure mode is at the slice-element level after
		// wrapAsSlice, so the predicate operates there.
		//
		// FILTER-IN-PLACE — no apiserver fall-through. The merge
		// just doesn't append the nil; healthy elements in the same
		// iteration are merged unchanged. The per-iteration source
		// (apistage cache entry / nested-call result) is the
		// resolver's transient state, NOT a cache layer — there is
		// nothing to evict (per
		// feedback_l1_invalidation_delete_only: DELETE is the only
		// verb that self-evicts; nothing here mutates a cache).
		//
		// Counter (AC-D4.1.11 — PM-explicit per-stage label): the
		// `gvr` argument carries `opts.key` (the STAGE NAME from
		// jsonHandlerCore at :122-123), NOT a GroupVersionResource
		// string. Operators get a per-RESTAction-stage breakdown
		// at `/debug/vars`'s
		// `snowplow_apiserver_fallthrough_cells["call-*|<stageName>|resolver-nil-merge"]`
		// — the diagnostic value-add per the design's §3.4 note 5.
		if v := wrapAsSlice(tmp); len(v) > 0 {
			kept := make([]any, 0, len(v))
			for _, x := range v {
				if x == nil {
					cache.RecordApiserverFallthrough(ctx, cache.ReasonResolverNilMerge, opts.key)
					continue
				}
				kept = append(kept, x)
			}
			if len(kept) > 0 {
				opts.out[opts.key] = append(existingSlice, kept...)
			}
		}
	default:
		// [panel500-instr] site=2 — type-switch default: arm
		// reassignment. Architect §2.2: empirical question "does the
		// burst land in this arm? got_is_nil=true means a prior
		// site=1 nil-write happened and we're now constructing
		// []any{nil, tmp} — the literal-nil-embedded slice the
		// downstream filter trips on." D.4.1 NEVER touched this arm
		// — its guard sat only in the case []any: arm above. Log
		// fires per-iteration on the failing path with both got and
		// tmp types + nil flags.
		slog.Info("[panel500-instr] site=2 tag=default_arm_merge",
			slog.String("stage_key", opts.key),
			slog.String("got_type", fmt.Sprintf("%T", got)),
			slog.Bool("got_is_nil", got == nil),
			slog.String("tmp_type", fmt.Sprintf("%T", tmp)),
			slog.Bool("tmp_is_nil", tmp == nil),
		)
		opts.out[opts.key] = []any{got, tmp}

		switch v := tmp.(type) {
		case []any:
			all := []any{got}
			all = append(all, v...)
			opts.out[opts.key] = all
		default:
			opts.out[opts.key] = []any{got, v}
		}
	}

	return nil
}

func wrapAsSlice(value any) []any {
	switch v := value.(type) {
	case []any:
		return v
	default:
		// [panel500-instr] site=9 — wrapAsSlice nil case. Architect
		// §2.9: empirical question "is wrapAsSlice(nil) ever called?
		// If yes and site 3 doesn't fire, then wrapAsSlice(nil) is
		// being called from a different caller chain than the D.4.1
		// case []any: arm." Confirms the structural mechanism — the
		// helper that turns a literal nil into []any{nil}.
		if v == nil {
			slog.Info("[panel500-instr] site=9 tag=wrap_as_slice_nil")
		}
		return []any{v}
	}
}
