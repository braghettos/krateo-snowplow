//go:build unit
// +build unit

package apis_test

import (
	"testing"

	"github.com/krateo-platformops/snowplow/apis"
	templatesv1 "github.com/krateo-platformops/snowplow/apis/templates/v1"

	"k8s.io/apimachinery/pkg/runtime"
)

func TestAddToScheme(t *testing.T) {
	tests := []struct {
		name    string
		setup   func() *runtime.Scheme
		wantErr bool
	}{
		{
			name: "Valid scheme registration",
			setup: func() *runtime.Scheme {
				return runtime.NewScheme()
			},
			wantErr: false,
		},
		{
			name: "Nil scheme should return error",
			setup: func() *runtime.Scheme {
				return nil
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := tt.setup()
			err := apis.AddToScheme(scheme)
			if (err != nil) != tt.wantErr {
				t.Errorf("expected error: %v, got: %v", tt.wantErr, err)
			}

			if err == nil {
				// Verifica che il tipo sia stato registrato correttamente
				gvk := templatesv1.SchemeBuilder.GroupVersion.WithKind("RESTAction")
				obj, err := scheme.New(gvk)
				if err != nil {
					t.Errorf("expected Template to be registered, but got error: %v", err)
				}
				if obj == nil {
					t.Errorf("expected Template object, got nil")
				}
			}
		})
	}
}
