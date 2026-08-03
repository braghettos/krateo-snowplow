package handlers

import (
	"log/slog"
	"net/http"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func Dispatcher(handlers map[string]http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		fn := func(wri http.ResponseWriter, req *http.Request) {
			if req.Method != http.MethodGet {
				next.ServeHTTP(wri, req)
				return
			}

			log := xcontext.Logger(req.Context())

			api := req.URL.Query().Get("apiVersion")
			if len(api) == 0 {
				log.Warn("missing 'apiVersion' query parameter")
			}

			res := req.URL.Query().Get("resource")
			if len(res) == 0 {
				log.Warn("missing 'resource' query parameter")
			}

			gv, err := schema.ParseGroupVersion(api)
			if err != nil {
				log.Error("unable to create schema.GroupVersion",
					slog.String("api", api), slog.Any("err", err))
			}
			gvr := gv.WithResource(res)

			key := gv.Group
			// Hack caused by new Widgets handlers
			if res == "restactions" {
				key = "restactions." + gv.Group
			}

			h, ok := handlers[key]
			if !ok {
				log.Warn("handler not found", slog.String("gvr", gvr.String()))
				next.ServeHTTP(wri, req)
			} else {
				log.Debug("handler found, forwarding request", slog.String("gvr", gvr.String()))
				h.ServeHTTP(wri, req)
			}
		}

		return http.HandlerFunc(fn)
	}
}
