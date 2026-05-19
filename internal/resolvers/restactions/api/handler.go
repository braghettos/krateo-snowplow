package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jqutil"
	"github.com/krateoplatformops/plumbing/ptr"
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
		s, err := jqutil.Eval(context.TODO(), jqutil.EvalOptions{
			Query: q, Data: pig,
			ModuleLoader: jqsupport.ModuleLoader(),
		})
		if err != nil {
			log.Error("unable to evaluate JQ filter",
				slog.String("filter", q), slog.Any("error", err))
		} else {
			if err := json.Unmarshal([]byte(s), &tmp); err != nil {
				return err
			}
		}
	}

	got, ok := opts.out[opts.key]
	if !ok {
		opts.out[opts.key] = tmp
		return nil
	}

	switch existingSlice := got.(type) {
	case []any:
		if v := wrapAsSlice(tmp); len(v) > 0 {
			opts.out[opts.key] = append(existingSlice, v...)
		}
	default:
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
		return []any{v}
	}
}
