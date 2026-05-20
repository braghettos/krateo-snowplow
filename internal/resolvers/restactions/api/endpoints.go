package api

import (
	"context"
	"fmt"
	"log/slog"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/plumbing/kubeutil"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"k8s.io/client-go/rest"
)

type endpointReferenceMapper struct {
	authnNS  string
	username string
	rc       *rest.Config
}

func (m *endpointReferenceMapper) resolveOne(ctx context.Context, ref *templates.Reference) (endpoints.Endpoint, error) {
	isInternal := false
	if ref == nil {
		// 0.30.102 Tag B: when the request is driven by an internal /
		// startup path (Phase 1's SA-credentialed resolution walk) the
		// context carries an explicit internal-dispatch endpoint via
		// cache.WithInternalEndpoint. There is no `<user>-clientconfig`
		// Secret for the synthetic SA identity, so the per-user lookup
		// below would fail; consult the context-carried endpoint first.
		// Ordinary per-user requests never set it — they fall through
		// to the unchanged clientconfig path. General mechanism, not a
		// per-resource carve-out (feedback_no_special_cases.md).
		if v, ok := cache.InternalEndpointFromContext(ctx); ok {
			if ep, epOK := v.(*endpoints.Endpoint); epOK && ep != nil {
				return *ep, nil
			}
			// The context carried an internal endpoint but it is not a
			// usable *endpoints.Endpoint (wrong shape, or a nil pointer).
			// Fall through to the per-user clientconfig path — but WARN:
			// an internal driver wired the wrong type and its dispatches
			// will silently take the per-user path, which has no
			// `<sa>-clientconfig` Secret and so will fail. Loud so a
			// future caller passing the wrong shape is diagnosable.
			xcontext.Logger(ctx).Warn("resolveOne: internal endpoint present but not a usable *endpoints.Endpoint; falling through to per-user clientconfig",
				slog.String("subsystem", "cache"),
				slog.String("got_type", fmt.Sprintf("%T", v)),
				slog.String("hint", "an internal driver (e.g. Phase 1) must pass *endpoints.Endpoint to cache.WithInternalEndpoint"),
			)
		}
		ref = &templates.Reference{
			Namespace: m.authnNS,
			Name:      fmt.Sprintf("%s-clientconfig", kubeutil.MakeDNS1123Compatible(m.username)),
		}
		isInternal = true
	}

	// Ship D (0.30.141) — F-3: endpoints.FromSecret issues a per-user
	// clientconfig-Secret GET (apiserver) per non-UAF stage per /call —
	// architect's largest snowplow-attributable apiserver traffic
	// source on the request path. Record BEFORE the plumbing call so a
	// panicking secret-GET still increments the counter (AC-D.3). The
	// gvr label is left empty: the Secret being read is fixed per user,
	// not per resolver target, and synthesizing a placeholder would
	// inflate cardinality without diagnostic value.
	cache.RecordApiserverFallthrough(ctx, cache.ReasonSecretGet, "")
	ep, err := endpoints.FromSecret(ctx, m.rc, ref.Name, ref.Namespace)
	if err != nil {
		return ep, err
	}
	if isInternal && !env.TestMode() {
		ep.ServerURL = "https://kubernetes.default.svc"
	}

	return ep, nil
}
