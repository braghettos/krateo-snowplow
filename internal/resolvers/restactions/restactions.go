package restactions

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jqutil"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/resolvers/restactions/api"
	jqsupport "github.com/krateoplatformops/snowplow/internal/support/jq"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
)

const (
	annotationKeyLastAppliedConfiguration = "kubectl.kubernetes.io/last-applied-configuration"
	annotationKeyVerboseAPI               = "krateo.io/verbose"
)

type ResolveOptions struct {
	In      *templates.RESTAction
	SArc    *rest.Config
	AuthnNS string
	PerPage int
	Page    int
	Extras  map[string]any
}

func Resolve(ctx context.Context, opts ResolveOptions) (*templates.RESTAction, error) {
	dict := api.Resolve(ctx, api.ResolveOptions{
		RC:      opts.SArc,
		AuthnNS: opts.AuthnNS,
		Verbose: isVerbose(opts.In),
		Items:   opts.In.Spec.API,
		PerPage: opts.PerPage,
		Page:    opts.Page,
		Extras:  opts.Extras,
		// Ship E (0.30.116): the owning RESTAction's identity, folded
		// into the per-api-stage L1 key so a stage id is scoped to its
		// RESTAction. opts.In is the typed RESTAction CR.
		RESTActionNamespace: opts.In.GetNamespace(),
		RESTActionName:      opts.In.GetName(),
	})
	if dict == nil {
		dict = map[string]any{}
	}

	log := xcontext.Logger(ctx)
	log.Debug("resolved api", slog.Any("dict", dict))

	var raw []byte
	if opts.In.Spec.Filter != nil {
		q := ptr.Deref(opts.In.Spec.Filter, "")
		// [panel500-instr] site=14 — top-level filter eval PRE-TRIP
		// snapshot. v2 design §2: captures the dict shape just before
		// jqutil.Eval runs, so when site=15 fires on the failing
		// filter, we have the immediately-preceding snapshot of
		// every dict key's type+length flag + the filter expression's
		// first 200 chars. Sorted deterministic ordering for grep
		// greppability. Fires on EVERY RESTAction resolve with a
		// top-level filter (not just the failing ones — tester
		// correlates by trace ID).
		dictSummary := make([]string, 0, len(dict))
		for k, v := range dict {
			flag := "non-nil"
			if v == nil {
				flag = "NIL"
			} else if arr, ok := v.([]any); ok {
				flag = fmt.Sprintf("[]any len=%d", len(arr))
			} else if m, ok := v.(map[string]any); ok {
				flag = fmt.Sprintf("map keys=%d", len(m))
			}
			dictSummary = append(dictSummary, fmt.Sprintf("%s=%s", k, flag))
		}
		sort.Strings(dictSummary)
		filterLen := len(q)
		filterHead := q
		if filterLen > 200 {
			filterHead = q[:200]
		}
		slog.Info("[panel500-instr] site=14 tag=top_level_filter_eval",
			slog.String("restaction_name", opts.In.GetName()),
			slog.String("restaction_namespace", opts.In.GetNamespace()),
			slog.String("dict_summary", strings.Join(dictSummary, ", ")),
			slog.Int("filter_len", filterLen),
			slog.String("filter_head", filterHead),
		)
		s, err := jqutil.Eval(context.TODO(), jqutil.EvalOptions{
			Query: q, Data: dict,
			ModuleLoader: jqsupport.ModuleLoader(),
		})
		if err != nil {
			// [panel500-instr] site=15 — filter eval FAILURE catch
			// (THE SMOKING GUN). At the EXACT moment the
			// `cannot iterate over: null` filter error fires, dumps
			// every element of dict["allCompositionResources"]
			// showing elem_is_map / elem_type / apiVersion_type /
			// apiVersion_value (+ kind dimensional pair). The
			// fail-trace element's idx + elem_type + apiVersion_type
			// are the load-bearing log lines that decide D.4.2's
			// fix site.
			//
			// PM tightening #2 (cap N=5 + truncation marker): if
			// dict["allCompositionResources"] has 1000+ items and only
			// one is malformed, logging all 1000 per request is
			// operationally noisy. The first 5 are sufficient to
			// discriminate the source; the truncation marker
			// preserves the total count for analysis. We log up to
			// 5 elements then emit one summary line.
			if arr, ok := dict["allCompositionResources"].([]any); ok {
				const maxLog = 5
				logged := 0
				for i, elem := range arr {
					if logged >= maxLog {
						break
					}
					elemMap, isMap := elem.(map[string]any)
					var avRaw, kRaw any
					if isMap {
						avRaw = elemMap["apiVersion"]
						kRaw = elemMap["kind"]
					}
					avStr, _ := avRaw.(string)
					kStr, _ := kRaw.(string)
					slog.Info("[panel500-instr] site=15 tag=filter_error_dict_dump",
						slog.String("restaction_name", opts.In.GetName()),
						slog.Int("idx", i),
						slog.Bool("elem_is_map", isMap),
						slog.String("elem_type", fmt.Sprintf("%T", elem)),
						slog.String("apiVersion_type", fmt.Sprintf("%T", avRaw)),
						slog.String("apiVersion_value", avStr),
						slog.String("kind_type", fmt.Sprintf("%T", kRaw)),
						slog.String("kind_value", kStr),
					)
					logged++
				}
				if len(arr) > maxLog {
					slog.Info("[panel500-instr] site=15 tag=truncated",
						slog.String("restaction_name", opts.In.GetName()),
						slog.Int("total_malformed", len(arr)),
						slog.Int("logged_first_n", maxLog),
						slog.String("hint", "PM cap N=5; remaining elements not logged this request"),
					)
				}
			}
			return opts.In, fmt.Errorf("unable to resolve filter: %w", err)
		}

		raw = []byte(s)
	} else {
		var err error
		raw, err = json.Marshal(dict)
		if err != nil {
			return opts.In, err
		}
	}

	opts.In.Status = &runtime.RawExtension{
		Raw: raw,
	}

	if opts.In.Annotations != nil {
		delete(opts.In.Annotations, annotationKeyLastAppliedConfiguration)
	}
	if opts.In.ManagedFields != nil {
		opts.In.ManagedFields = nil
	}

	return opts.In, nil
}

// IsVerbose returns true if the object has the AnnotationKeyConnectorVerbose
// annotation set to `true`.
func isVerbose(o metav1.Object) bool {
	return o.GetAnnotations()[annotationKeyVerboseAPI] == "true"
}
