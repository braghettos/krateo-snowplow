package schema

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/dynamic"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	runtimeschema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

const (
	widgetDataKey = "widgetData"
)

// crdGVR is the hardcoded apiextensions.k8s.io/v1 CRD GroupVersionResource.
// Stable across all clusters; pinning here avoids both a discovery hop
// and a string-build allocation on every widget /call.
var crdGVR = runtimeschema.GroupVersionResource{
	Group:    "apiextensions.k8s.io",
	Version:  "v1",
	Resource: "customresourcedefinitions",
}

func ValidateObjectStatus(ctx context.Context, rc *rest.Config, obj map[string]any) error {
	gv := dynamic.GroupVersion(obj)
	gvk := gv.WithKind(dynamic.GetKind(obj))

	// Ship 2 (production-aim cleanup 2026-06-01) — resolve the
	// composition GVR through cache.GVRFor. The builtin scheme arm
	// covers apiextensions.k8s.io/v1 itself; the permanent plurals
	// store covers the composition's own CRD-backed GVK after one
	// discovery hop per process lifetime (already counted by
	// ReasonPluralsDiscoveryHop inside PluralFor). Replaces Ship D's
	// ReasonRestmapperResourceFor + the dynamic.ResourceFor cold-
	// restmapper build per /call (the helper was deleted in Ship 2).
	if cache.IsResolverGVRHit(gvk) {
		cache.RecordResolverPluralsHit(ctx, gvk.String())
	} else {
		cache.RecordResolverPluralsMiss(ctx, gvk.String())
	}
	gvr, err := cache.GVRFor(ctx, gvk, rc)
	if err != nil {
		return err
	}

	widgetData, ok, err := unstructured.NestedMap(obj, "status", widgetDataKey)
	if err != nil {
		return err
	}
	if !ok {
		name := dynamic.GetName(obj)
		return &apierrors.StatusError{
			ErrStatus: metav1.Status{
				Status: metav1.StatusFailure,
				Code:   http.StatusNotFound,
				Reason: metav1.StatusReasonNotFound,
				Details: &metav1.StatusDetails{
					Group: gvr.Group,
					Kind:  gvr.Resource,
					Name:  name,
				},
				Message: fmt.Sprintf("status.widgetData not found in %s %q", gvr.String(), name),
			}}
	}

	// Task #323 (#318-R2 Commit 2-B) — per-GVR compiled-schema memo. On a
	// hit, skip the CRD GET + extractOpenAPISchemaFromCRD +
	// buildValidationFromSchemaData recompute (TRACED 0.91% of the 0.30.258
	// drain profile — they are a pure function of (CRD bytes, version),
	// invariant per GVR across a window) and go straight to the per-object
	// validateCustomResource below. The memo is reset on any CRD lifecycle
	// event via the EXISTING 0.30.233 bridge (InvalidateCRDSchemaMemo wired
	// in main.go) AND fenced by a generation counter so an inflight miss-fill
	// (this call) cannot re-install a schema compiled from pre-reset bytes —
	// so a hit cannot serve a stale schema for a changed GVR even under a
	// concurrent CRD install (architect A1; schema_cache.go header).
	// Placed AFTER the status.widgetData-absent NotFound guard above so a
	// memo hit NEVER bypasses the fail-closed NotFound path (never-change-
	// output: a widgetData-absent object returns NotFound on hit AND miss).
	if crv, hit := lookupCRDSchema(gvr); hit {
		return validateCustomResource(crv, widgetData)
	}

	// MISS — snapshot the memo generation BEFORE the CRD GET below. If a CRD
	// lifecycle reset (InvalidateCRDSchemaMemo) lands during the GET+compile
	// window, the generation moves and storeCRDSchema drops the (potentially
	// stale-bytes) install, so the next call recompiles from fresh bytes
	// (architect A1 generation fence).
	gen := currentSchemaGen()

	// Ship 0.30.231 (2026-06-01) — inlined CRD GET. The deleted
	// internal/resolvers/crds.Get helper wrapped the same two-line
	// dynamic.NewClient + Get call below; inlining removes the
	// indirection. Earlier (Ship 2) attempted to also skip the
	// mapper build via dynamic.WithSkipMapper, but the contract was
	// unsafe — resourceInterfaceFor (client.go:146) calls
	// uc.mapper.RESTMapping unconditionally; the opts.GVR/opts.GVK
	// branch at line 138 only chooses the source GVK, it does not
	// skip the mapper. WithSkipMapper has been removed.
	//
	// Task #322 (#318-R2) Commit 1 — the per-call dynamic.NewClient
	// here built a FRESH memCacheClient + DeferredDiscoveryRESTMapper
	// every child GET, so the first GVR-only KindFor re-downloaded the
	// full API surface per call (TRACED 2.13% of the 0.30.258 drain
	// profile). SharedSADiscoveryClient returns a process-singleton
	// whose mapper is built ONCE and reused warm; the discovery
	// download is amortised to one boot download. rc here is ALWAYS the
	// SA rest.Config (opts.RC = widgets.go:228 r.saRC on the customer
	// /call path; phase1_walk_pagination_jobs.go:433/:579 saRC on the
	// drain) — the singleton's identity invariant. This is the cached-
	// mapper correction, NOT the WithSkipMapper revival: the mapper is
	// always non-nil and populated (cached_client.go header).
	//
	// FALLBACK — if the singleton errors (nil rc / startup race),
	// fall back to the per-call dynamic.NewClient so the path is never
	// worse than today (project_cache_off_is_transparent_fallback).
	cli, err := dynamic.SharedSADiscoveryClient(rc)
	if err != nil {
		// Fall back LOUDLY: a systemically broken singleton would
		// otherwise silently evaporate the caching win back to
		// per-call discovery downloads.
		slog.Warn("schema: SA discovery singleton unavailable; "+
			"falling back to per-call client",
			slog.String("error", err.Error()))
		cli, err = dynamic.NewClient(rc)
		if err != nil {
			return err
		}
	}
	crdObj, err := cli.Get(ctx, fmt.Sprintf("%s.%s", gvr.Resource, gvr.Group), dynamic.Options{
		GVR:       crdGVR,
		Namespace: "",
	})
	if err != nil {
		return err
	}
	var crd map[string]any
	if crdObj != nil {
		crd = crdObj.UnstructuredContent()
	} else {
		crd = map[string]any{}
	}

	// Miss — compile the CRV (the 0.91% extract + v1->internal build) and
	// memoise it per GVR. compileCRDSchemaFn is the package-private seam the
	// RED/GREEN falsifier counts (per-call recompile N -> 1 with the memo);
	// it is initialised once to extractOpenAPISchemaFromCRD and only
	// reassigned by test code (mirrors discoveryClientForConfigFn,
	// dynamic/cached_client.go:176).
	crv, err := compileCRDSchemaFn(crd, gvr.Version)
	if err != nil {
		return err
	}
	storeCRDSchema(gvr, crv, gen)

	return validateCustomResource(crv, widgetData)
}
