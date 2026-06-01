package widgets

import (
	"context"
	"log/slog"
	"net/http"
	"reflect"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/maps"
	v1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	crdschema "github.com/krateoplatformops/snowplow/internal/resolvers/crds/schema"

	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets/apiref"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets/resourcesrefs"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets/resourcesrefstemplate"

	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets/widgetdatatemplate"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
)

type Widget = unstructured.Unstructured

type ResolveOptions struct {
	In      *Widget
	RC      *rest.Config
	AuthnNS string
	PerPage int
	Page    int
	Extras  map[string]any
}

func Resolve(ctx context.Context, opts ResolveOptions) (*Widget, error) {
	log := xcontext.Logger(ctx).With(loggerAttr(opts.In.Object))

	// Ship 0.30.193 Checkpoint 1 — widget-resolve phase wall-clocks.
	// Captured per-phase so seedOneWidget's deferred log can attribute
	// total widget-resolve cost across apiref / widgetData / resrefs /
	// validate. The sink is checked once to skip overhead on the
	// production /call path (no sink installed → no log emitted by
	// caller); phase locals are still computed (4 × time.Now → ~120ns)
	// to keep the code path uniform per feedback_no_special_cases.
	pipSink := cache.PIPStageTimingSinkFrom(ctx)
	phaseApirefMs := int64(0)
	phaseWidgetDataMs := int64(0)
	phaseResRefsMs := int64(0)
	phaseValidateMs := int64(0)
	defer func() {
		// Only emit the structured phase-timing log when a sink is
		// wired (PIP seed path). Production /call has no sink → no log
		// → zero additional log overhead.
		if pipSink == nil {
			return
		}
		log.Info("phase1.seed.widget.phase.timing",
			slog.String("subsystem", "cache"),
			slog.Int64("apiref_ms", phaseApirefMs),
			slog.Int64("widget_data_ms", phaseWidgetDataMs),
			slog.Int64("resrefs_ms", phaseResRefsMs),
			slog.Int64("validate_ms", phaseValidateMs),
		)
	}()

	apirefStart := time.Now()
	ds, err := resolveApiRef(ctx, opts)
	phaseApirefMs = time.Since(apirefStart).Milliseconds()
	if err != nil {
		log.Error("unable to resolve api reference", slog.Any("err", err))
		maps.SetNestedField(opts.In.Object, err.Error(), "status", "error")
		return opts.In, err
	}

	// Ship H7 (0.30.161): re-inject the pagination triple into the
	// widget data source. The api-resolver constructed `dict["slice"]`
	// at internal/resolvers/restactions/api/resolve.go:211-215, but the
	// RESTAction's spec.Filter projection (e.g. compositions-list emits
	// `{list: (.allCompositions // [])}`) strips `.slice` from the dict
	// before it arrives here as `ds`. The widget jq expects `.slice`
	// (see e.g. table.dashboard-compositions-panel-row-table.yaml:32 —
	// `if .slice then $sorted[0 : (.slice.page * .slice.perPage)] else …`)
	// and silently falls through to "return all rows" when absent —
	// materialising the unbounded list at the resolver layer.
	injectSlice(ds, opts.PerPage, opts.Page)

	widgetDataStart := time.Now()
	widgetData, err := resolveWidgetData(ctx, opts.In, ds)
	phaseWidgetDataMs = time.Since(widgetDataStart).Milliseconds()
	if err != nil {
		log.Error("unable to resolve widget data", slog.Any("err", err))
		maps.SetNestedField(opts.In.Object, err.Error(), "status", "error")
		return opts.In, err
	}

	err = maps.SetNestedField(opts.In.Object, widgetData, "status", widgetDataKey)
	if err != nil {
		log.Error("unable to set status as unstructured.NestedMap",
			slog.Any("err", err))
		return opts.In, err
	}

	resRefsStart := time.Now()
	resourcesRefsResults, err := resolveResourceRefs(ctx, opts.In, ds)
	phaseResRefsMs = time.Since(resRefsStart).Milliseconds()
	if err != nil {
		maps.SetNestedField(opts.In.Object, err.Error(), "status", "error")
		return opts.In, err
	}

	if tot := len(resourcesRefsResults); tot > 0 {
		tmp, err := maps.StructSliceToMapSlice(resourcesRefsResults)
		if err != nil {
			return opts.In, err
		}

		pig := map[string]any{
			"items": tmp,
		}
		if opts.PerPage > 0 && opts.Page > 0 {
			hasNext := (tot >= opts.PerPage)
			page := opts.Page
			if hasNext {
				page = page + 1
			}
			pig["slice"] = map[string]any{
				"perPage":  opts.PerPage,
				"page":     page,
				"continue": hasNext,
			}
		}

		err = maps.SetNestedField(opts.In.Object, pig, "status", resourcesRefsKey)
		if err != nil {
			return opts.In, err
		}
	}

	validateStart := time.Now()
	// Ship 2 (production-aim cleanup 2026-06-01) — pass opts.RC in
	// BOTH paths. The previous TestMode-gated branch passed nil to
	// ValidateObjectStatus on the production path, forcing the
	// validator to recover the rc via rest.InClusterConfig() —
	// useless work AND a latent defect for non-in-cluster
	// deployments (the seed path runs with a context-injected
	// internal-dispatch rc, NOT in-cluster). The call site KNOWS
	// the rc; the helper shouldn't recover it.
	err = crdschema.ValidateObjectStatus(ctx, opts.RC, opts.In.Object)
	phaseValidateMs = time.Since(validateStart).Milliseconds()
	if err != nil {
		maps.SetNestedField(opts.In.Object, err.Error(), "status", "error")
		return opts.In, &apierrors.StatusError{
			ErrStatus: metav1.Status{
				Status:  metav1.StatusFailure,
				Code:    http.StatusBadRequest,
				Reason:  metav1.StatusReasonBadRequest,
				Message: err.Error(),
			}}
	}

	return opts.In, nil
}

func resolveApiRef(ctx context.Context, opts ResolveOptions) (map[string]any, error) {
	apiRef, err := GetApiRef(opts.In.Object)
	if err != nil {
		return nil, err
	}

	return apiref.Resolve(ctx, apiref.ResolveOptions{
		RC:      opts.RC,
		ApiRef:  apiRef,
		AuthnNS: opts.AuthnNS,
		PerPage: opts.PerPage,
		Page:    opts.Page,
	})
}

func resolveWidgetData(ctx context.Context, obj *Widget, ds map[string]any) (map[string]any, error) {
	log := xcontext.Logger(ctx)

	src := GetWidgetData(obj.Object)

	// LOAD-BEARING INVARIANT (task #69 / arch-rev-70) — error-direction
	// symmetry with the cache routing predicate. A widgetDataTemplate read
	// error here MUST fail-soft to the STATIC-ONLY widgetData (`src`, no
	// aggregate built). The cache predicate
	// (dispatchers.isRBACSensitiveApiRefWidget) reads the SAME accessor
	// (GetWidgetDataTemplate) and, on the same error, DE-CLASSIFIES the
	// widget → routes it to the identity-free widgetContent cell. These two
	// sites MUST stay symmetric: if a read error here ever started building
	// the cross-namespace aggregate while the predicate still de-classified,
	// the SA-maximal aggregate would land in the shared identity-free cell —
	// reopening the cross-user leak task #69 closed. Keep both fail-soft.
	wdt, err := GetWidgetDataTemplate(obj.Object)
	if err != nil {
		log.Warn("unable to get widgetDataTemplate", slog.Any("err", err))
		return src, nil
	}

	evals, err := widgetdatatemplate.Resolve(ctx, widgetdatatemplate.ResolveOptions{
		Items:      wdt,
		DataSource: ds,
	})
	if err != nil {
		log.Error("unable to resolve widgetDataTemplate", slog.Any("err", err))
		return src, err
	}
	log.Debug("widgetDataTemplate JQ evaluation results", slog.Any("evals", evals))

	for _, el := range evals {
		fields := maps.ParsePath(el.Path)
		if len(fields) == 0 {
			continue
		}

		log.Debug("widgetDataTemplate setting nested value",
			slog.Any("fields", fields),
			slog.String("path", el.Path),
			slog.Any("value", el.Value),
			slog.Any("type", reflect.TypeOf(el.Value)),
		)

		err = maps.SetNestedValue(src, fields, el.Value)
		if err != nil {
			log.Error("unable to set nested value",
				slog.Any("fields", fields),
				slog.Any("value", el.Value),
				slog.Any("valueType", reflect.TypeOf(el.Value)),
				slog.Any("err", err))
			return src, err
		}
	}

	return src, nil
}

func resolveResourceRefs(ctx context.Context, obj *Widget, ds map[string]any) ([]v1.ResourceRefResult, error) {
	log := xcontext.Logger(ctx)

	all := []v1.ResourceRef{}

	resrefs, err := GetResourcesRefs(obj.Object)
	if err != nil {
		log.Warn("unable to get resources references", slog.Any("err", err))
	} else {
		all = append(all, resrefs...)
	}

	resrefstpl, err := GetResourcesRefsTemplate(obj.Object)
	if err != nil {
		log.Warn("unable to get resource references template", slog.Any("err", err))
	}
	if len(resrefstpl) > 0 {
		resrefsExtra, err := resourcesrefstemplate.Resolve(ctx, resrefstpl, ds)
		if err != nil {
			return nil, err
		} else {
			all = append(all, resrefsExtra...)
		}
	}

	return resourcesrefs.Resolve(ctx, all)
}

// injectSlice writes `ds["slice"] = {page, perPage, offset}` IFF
// the request carried pagination (`perPage > 0 && page > 0`) AND
// `ds` does not already contain a `slice` entry.
//
// Mechanism-uniform per feedback_no_special_cases: no widget-name
// table, no GVR allowlist. The shape mirrors the triple computed at
// internal/resolvers/restactions/api/resolve.go:211-215; this hop
// merely propagates that triple through the RA-filter projection
// (which would otherwise strip it — see the function's call site
// for the TRACED defect chain).
//
// Branch A per design §3: pre-existing `slice` is preserved so an
// RA author who deliberately emits `.slice` via spec.Filter wins.
func injectSlice(ds map[string]any, perPage, page int) {
	if ds == nil {
		return
	}
	if perPage <= 0 || page <= 0 {
		return
	}
	if _, present := ds["slice"]; present {
		return
	}
	ds["slice"] = map[string]any{
		"page":    page,
		"perPage": perPage,
		"offset":  (page - 1) * perPage,
	}
}
