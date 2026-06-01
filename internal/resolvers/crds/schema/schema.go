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

func ValidateObjectStatus(ctx context.Context, rc *rest.Config, obj map[string]any) error {
	gv := dynamic.GroupVersion(obj)
	gvk := gv.WithKind(dynamic.GetKind(obj))
	// Ship 2 / 0.30.226 — plurals-resolver hot-path swap. Replaces
	// per-/call cold restmapper construction (Ship D F-4) with the
	// permanent plurals store (Ship 1 / 0.30.225). NO fallback arm
	// to apiserver discovery on miss: a miss is either a coverage
	// gap (counted as plurals-miss) or a genuinely-unresolvable GVK.
	gvr, err := cache.GVRFor(ctx, gvk, rc)
	if err != nil {
		cache.RecordResolverPluralsMiss(ctx, gvk.String())
		return err
	}
	cache.RecordResolverPluralsHit(ctx, gvk.String())

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

	// Ship 2 / 0.30.226 — inlined direct apiserver GET on the CRD,
	// replacing the deleted internal/resolvers/crds.Get helper. The
	// CRD fetch is a one-shot call per ValidateObjectStatus and the
	// CRD GVR is fixed; previously this routed through the crds
	// package which also fired ReasonCRDGet (removed alongside this
	// swap because it always tagged the apiextensions GVR which
	// dominated the call-widgets cell unhelpfully).
	cli, err := dynamic.NewClient(rc)
	if err != nil {
		return err
	}
	got, err := cli.Get(ctx, fmt.Sprintf("%s.%s", gvr.Resource, gvr.Group), dynamic.Options{
		GVR: runtimeschema.GroupVersionResource{
			Group:    "apiextensions.k8s.io",
			Version:  "v1",
			Resource: "customresourcedefinitions",
		},
		Namespace: "",
	})
	if err != nil {
		return err
	}
	var crd map[string]any
	if got != nil {
		crd = got.UnstructuredContent()
	} else {
		crd = map[string]any{}
	}

	crv, err := extractOpenAPISchemaFromCRD(crd, gvr.Version)
	if err != nil {
		return err
	}

	return validateCustomResource(crv, widgetData)
}
