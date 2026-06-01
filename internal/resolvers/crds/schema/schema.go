package schema

import (
	"context"
	"fmt"
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

	// Ship 0.30.231 (2026-06-01) — inlined CRD GET. The deleted
	// internal/resolvers/crds.Get helper wrapped the same two-line
	// dynamic.NewClient + Get call below; inlining removes the
	// indirection. Earlier (Ship 2) attempted to also skip the
	// mapper build via dynamic.WithSkipMapper, but the contract was
	// unsafe — resourceInterfaceFor (client.go:146) calls
	// uc.mapper.RESTMapping unconditionally; the opts.GVR/opts.GVK
	// branch at line 138 only chooses the source GVK, it does not
	// skip the mapper. WithSkipMapper has been removed; the mapper
	// build is a dead allocation here but cannot panic.
	cli, err := dynamic.NewClient(rc)
	if err != nil {
		return err
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

	crv, err := extractOpenAPISchemaFromCRD(crd, gvr.Version)
	if err != nil {
		return err
	}

	return validateCustomResource(crv, widgetData)
}
