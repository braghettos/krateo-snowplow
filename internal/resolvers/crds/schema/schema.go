package schema

import (
	"context"
	"fmt"
	"net/http"

	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/dynamic"
	"github.com/krateoplatformops/snowplow/internal/resolvers/crds"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
)

const (
	widgetDataKey = "widgetData"
)

func ValidateObjectStatus(ctx context.Context, rc *rest.Config, obj map[string]any) error {
	gv := dynamic.GroupVersion(obj)
	gvk := gv.WithKind(dynamic.GetKind(obj))
	// Ship D (0.30.141) — F-4: dynamic.ResourceFor builds a fresh
	// discovery client + cold restmapper per widget /call. Record
	// BEFORE the upstream construction (AC-D.3).
	cache.RecordApiserverFallthrough(ctx, cache.ReasonRestmapperResourceFor, gvk.String())
	gvr, err := dynamic.ResourceFor(rc, gvk)
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

	crd, err := crds.Get(ctx, crds.GetOptions{
		RC:      rc,
		Name:    fmt.Sprintf("%s.%s", gvr.Resource, gvr.Group),
		Version: gvr.Version,
	})
	if err != nil {
		return err
	}

	crv, err := extractOpenAPISchemaFromCRD(crd, gvr.Version)
	if err != nil {
		return err
	}

	return validateCustomResource(crv, widgetData)
}
