package api

import (
	"context"
	"log/slog"
	"net/http"

	httpcall "github.com/krateoplatformops/plumbing/http/request"
	"github.com/krateoplatformops/plumbing/jqutil"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	jqsupport "github.com/krateoplatformops/snowplow/internal/support/jq"
)

// phase1IteratorCap bounds how many elements a `dependsOn.iterator` api
// stage expands to WHEN the resolve runs under a Phase-1
// (startup-warmup) context — cache.IsPhase1Resolution(ctx)==true.
//
// WHY: the compositions-panels / blueprints-panels RESTActions fan a
// stage-2 iterator over every namespace (~50 at production scale), one
// per-namespace `panels` LIST each. During the Phase-1 warmup walk the
// goal is only to DISCOVER + warm the `panels` informer — that informer
// is GVR-scoped, so resolving the iterated path for ONE namespace warms
// it exactly as well as resolving all 50. Expanding all 50 inside the
// 900s startupProbe budget is what crashloops the pod. The cap lets the
// warmup walk traverse the stage cheaply.
//
// >1 so a Phase-1 walk still exercises the iterator's templating with
// more than a degenerate single element; small because additional
// iterated elements discover no new informer. Mirrors the deliberate,
// shape-validated constant rationale of phase1PerGVRSampleLimit
// (dispatchers/phase1_walk.go).
//
// CRITICAL: the cap fires ONLY under a Phase-1 context. Every real
// `/call` leaves cache.IsPhase1Resolution false, so the iterator expands
// FULLY — behaviour byte-identical to pre-0.30.111.
const phase1IteratorCap = 3

func createRequestOptions(ctx context.Context, log *slog.Logger, in *templates.API, dict map[string]any) (all []httpcall.RequestOptions) {
	it := ""
	if in.DependsOn != nil {
		it = ptr.Deref(in.DependsOn.Iterator, "")
	}

	if len(it) == 0 {
		all = make([]httpcall.RequestOptions, 0, 1)
		el := createRequestOption(in, dict)
		all = append(all, el)
		return
	}

	all = []httpcall.RequestOptions{}

	// 0.30.111 Part 2 — Phase-1 iterator cap. Under a startup-warmup
	// resolution (cache.IsPhase1Resolution(ctx)==true) the iterator
	// fan-out is bounded to phase1IteratorCap elements: the warmup only
	// needs to DISCOVER the downstream GVR's informer, and that informer
	// is GVR-scoped, so one iterated element warms it as well as all N.
	// For every real `/call` the marker is absent, capActive stays
	// false, and the iterator expands FULLY — byte-identical to
	// pre-0.30.111.
	capActive := cache.IsPhase1Resolution(ctx)
	capped := 0

	action := func(sa any) error {
		if capActive && len(all) >= phase1IteratorCap {
			capped++
			return nil // skip — cap reached; keep draining the stream
		}
		el := createRequestOption(in, sa)
		all = append(all, el)
		return nil
	}

	err := jqutil.ForEach(ctx, jqutil.EvalOptions{Query: it, Unquote: true, Data: dict}, action)
	if err != nil {
		log.Error("unable to execute iterator", slog.String("query", it), slog.Any("err", err))
	}

	if capActive && capped > 0 {
		log.Info("phase1 iterator capped",
			slog.String("query", it),
			slog.Int("expanded", len(all)),
			slog.Int("skipped", capped),
			slog.Int("cap", phase1IteratorCap),
			slog.String("reason", "startup-warmup resolution — informer discovery needs only a bounded sample"),
		)
	}

	return all
}

func createRequestOption(in *templates.API, ds any) (out httpcall.RequestOptions) {
	out.ContinueOnError = ptr.Deref(in.ContinueOnError, false)
	out.ErrorKey = ptr.Deref(in.ErrorKey, "error")

	out.Path = evalJQ(in.Path, ds)
	out.Verb = ptr.To(ptr.Deref(in.Verb, http.MethodGet))

	if in.Payload != nil {
		out.Payload = ptr.To(evalJQ(*in.Payload, ds))
	}

	if in.Headers != nil {
		out.Headers = make([]string, 0, len(in.Headers))
		//copy(el.Headers, in.Headers)
		for _, h := range in.Headers {
			out.Headers = append(out.Headers, evalJQ(h, ds))
		}
	}

	return
}

func evalJQ(q string, ds any) string {
	q, ok := jqutil.MaybeQuery(q)
	if !ok {
		return q
	}

	out, err := jqutil.Eval(context.TODO(),
		jqutil.EvalOptions{
			Query:        q,
			Unquote:      true,
			Data:         ds,
			ModuleLoader: jqsupport.ModuleLoader(),
		})
	if err != nil {
		out = err.Error()
	}

	return out
}
