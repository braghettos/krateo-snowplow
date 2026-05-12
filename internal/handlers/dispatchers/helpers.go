package dispatchers

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/http/response"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/handlers/util"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"github.com/krateoplatformops/snowplow/internal/rbac"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func fetchObject(req *http.Request) (got objects.Result) {
	log := xcontext.Logger(req.Context())

	gvr, err := util.ParseGVR(req)
	if err != nil {
		got.Err = response.New(http.StatusBadRequest, err)
		return
	}
	log.Debug("GVR from request query parameters", slog.Any("gvr", gvr))

	nsn, err := util.ParseNamespacedName(req)
	if err != nil {
		got.Err = response.New(http.StatusBadRequest, err)
		return
	}
	log.Debug("Name and Namespace from request query parameters", slog.Any("nsn", nsn))

	return objects.Get(req.Context(), templatesv1.ObjectReference{
		Reference: templatesv1.Reference{
			Name: nsn.Name, Namespace: nsn.Namespace,
		},
		APIVersion: gvr.GroupVersion().String(),
		Resource:   gvr.Resource,
	})
}

func paginationInfo(log *slog.Logger, req *http.Request) (perPage, page int) {
	perPage, page = -1, -1

	if val := req.URL.Query().Get("perPage"); val != "" {
		var err error
		perPage, err = strconv.Atoi(val)
		if err != nil {
			log.Error("unable convert perPage parameter to int",
				slog.Any("err", err))
		}
	}

	if val := req.URL.Query().Get("page"); val != "" {
		var err error
		page, err = strconv.Atoi(val)
		if err != nil {
			log.Error("unable convert page parameter to int",
				slog.Any("err", err))
		}
	}

	if perPage > 0 && page <= 0 {
		page = 1
	}

	return
}

// checkDispatchRBAC is the cache=on permission gate (Revision 2
// binding, Tag 0.30.4). Returns true iff the user identified by ctx is
// permitted to GET the dispatched CR in namespace.
//
// The check runs against the *dispatch target* (RestAction or Widget
// CR) — the same object the cache=off fetchObject branch hits the
// apiserver for. In cache=on mode fetchObject does not enforce RBAC
// for that GET, so the gate must run explicitly here.
//
// Callers MUST only invoke this in cache=on mode (`!cache.Disabled()`).
// In cache=off mode the gate is implicit in fetchObject's per-user
// apiserver call.
func checkDispatchRBAC(ctx context.Context, gvr schema.GroupVersionResource, namespace string) bool {
	log := xcontext.Logger(ctx)

	ui, err := xcontext.UserInfo(ctx)
	if err != nil {
		log.Error("checkDispatchRBAC: unable to extract UserInfo",
			slog.Any("err", err),
		)
		return false
	}

	allowed, evalErr := rbac.EvaluateRBAC(ctx, rbac.EvaluateOptions{
		Username:  ui.Username,
		Groups:    ui.Groups,
		Verb:      "get",
		Group:     gvr.Group,
		Resource:  gvr.Resource,
		Namespace: namespace,
	})
	if evalErr != nil {
		log.Error("checkDispatchRBAC: EvaluateRBAC error",
			slog.String("user", ui.Username),
			slog.String("gvr", gvr.String()),
			slog.String("namespace", namespace),
			slog.Any("err", evalErr),
		)
		return false
	}
	return allowed
}
