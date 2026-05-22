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

	// Ship D.2 (0.30.143) — F-3 cache lookup FIRST. The Secrets
	// snapshot covers AUTHN_NAMESPACE `<user>-clientconfig` Secrets
	// in-process; on a hit we skip the apiserver round-trip
	// entirely. On any soft miss (cache=off, namespace mismatch,
	// pre-sync, Secret absent, snapshot nil pre-readiness) the
	// function returns served=false and we fall through to the
	// upstream plumbing call below — the pre-D.2 behavior preserved
	// exactly (AC-D2.4). The Ship D RecordApiserverFallthrough call
	// is BELOW the cache-hit return so the F-3 counter measures
	// only the fallback path; the F-3 falsifier (134/60s → ≤5/60s)
	// reads that drop.
	if ep, served, ferr := FromInformerSecret(ctx, ref.Namespace, ref.Name); served {
		// Hard error path: Secret present but malformed
		// (server-url missing). Upstream FromSecret would have
		// returned the same error verbatim — we propagate it.
		if ferr != nil {
			return ep, ferr
		}
		// AC-D2.7 — the isInternal+!env.TestMode ServerURL override
		// applies UNIFORMLY on both the cache-hit and the upstream-
		// fallback path. Kept here (not factored into
		// FromInformerSecret) per PM-explicit `feedback_no_special_cases`:
		// the override is the resolver's contextual decision (the
		// stage is internal-driven), not a Secret-cache concern.
		if isInternal && !env.TestMode() {
			ep.ServerURL = "https://kubernetes.default.svc"
		}
		return ep, nil
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
	// Ship 0.30.165 — normalize CA bytes to the single-base64-encoded
	// PEM shape that plumbing's transport expects (see endpoints_ca.go).
	// Live `<user>-clientconfig` Secrets store double-base64 PEM; without
	// this normalization plumbing silently fails to populate RootCAs and
	// requests die with `x509: certificate signed by unknown authority`.
	ep.CertificateAuthorityData = string(normalizeCAData([]byte(ep.CertificateAuthorityData)))
	if isInternal && !env.TestMode() {
		ep.ServerURL = "https://kubernetes.default.svc"
	}

	return ep, nil
}
