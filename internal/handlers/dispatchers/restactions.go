package dispatchers

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/plumbing/http/response"
	"github.com/krateoplatformops/snowplow/apis"
	v1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/handlers/util"
	"github.com/krateoplatformops/snowplow/internal/resolvers/restactions"
	"k8s.io/apimachinery/pkg/runtime"
)

func RESTAction() http.Handler {
	return &restActionHandler{
		authnNS: env.String("AUTHN_NAMESPACE", ""),
		verbose: env.True("DEBUG"),
	}
}

type restActionHandler struct {
	authnNS string
	verbose bool
}

var _ http.Handler = (*restActionHandler)(nil)

func (r *restActionHandler) ServeHTTP(wri http.ResponseWriter, req *http.Request) {
	log := xcontext.Logger(req.Context())

	start := time.Now()

	extras, err := util.ParseExtras(req)
	if err != nil {
		response.BadRequest(wri, err)
		return
	}

	got := fetchObject(req)
	if got.Err != nil {
		response.Encode(wri, got.Err)
		return
	}

	// Revision 2 binding (0.30.4): in cache=on mode every RestAction
	// dispatch is gated by EvaluateRBAC against the CR being dispatched.
	// Cache=off skips this gate — fetchObject already runs per-user
	// against apiserver, which enforces RBAC inline.
	if !cache.Disabled() {
		if !checkDispatchRBAC(req.Context(), got.GVR, got.Unstructured.GetNamespace()) {
			log.Warn("RESTAction dispatch denied by EvaluateRBAC",
				slog.String("name", got.Unstructured.GetName()),
				slog.String("namespace", got.Unstructured.GetNamespace()),
				slog.String("gvr", got.GVR.String()),
			)
			response.Encode(wri, response.New(http.StatusForbidden,
				fmt.Errorf("forbidden: cannot get %s in namespace %q",
					got.GVR.Resource, got.Unstructured.GetNamespace())))
			return
		}
	}

	scheme := runtime.NewScheme()
	if err := apis.AddToScheme(scheme); err != nil {
		log.Error("unable to add apis to scheme",
			slog.Any("err", err))
		response.InternalError(wri, err)
		return
	}

	var cr v1.RESTAction
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(got.Unstructured.Object, &cr)
	if err != nil {
		log.Error("unable to convert unstructured to typed rest action",
			slog.String("name", got.Unstructured.GetName()),
			slog.String("namespace", got.Unstructured.GetNamespace()),
			slog.Any("err", err))
		response.InternalError(wri, err)
		return
	}

	perPage, page := paginationInfo(log, req)

	ctx := xcontext.BuildContext(req.Context())
	res, err := restactions.Resolve(ctx, restactions.ResolveOptions{
		In:      &cr,
		AuthnNS: r.authnNS,
		PerPage: perPage,
		Page:    page,
		Extras:  extras,
	})
	if err != nil {
		log.Error("unable to resolve rest action",
			slog.String("name", cr.GetName()),
			slog.String("namespace", cr.GetNamespace()),
			slog.Any("err", err))
		response.InternalError(wri, err)
		return
	}

	log.Info("RESTAction successfully resolved",
		slog.String("name", cr.Name),
		slog.String("namespace", cr.Namespace),
		slog.String("duration", util.ETA(start)),
	)

	wri.Header().Set("Content-Type", "application/json")
	wri.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(wri)
	enc.SetIndent("", "  ")
	enc.Encode(res)
}
