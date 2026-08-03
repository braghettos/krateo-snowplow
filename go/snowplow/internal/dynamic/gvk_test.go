package dynamic

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestGroupVersion(t *testing.T) {
	tests := []struct {
		name       string
		obj        map[string]any
		expectedGV schema.GroupVersion
	}{
		{
			name: "valid apiVersion with group and version",
			obj: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
			},
			expectedGV: schema.GroupVersion{
				Group:   "apps",
				Version: "v1",
			},
		},
		{
			name: "core API type",
			obj: map[string]any{
				"apiVersion": "v1",
			},
			expectedGV: schema.GroupVersion{
				Group:   "",
				Version: "v1",
			},
		},
		{
			name: "custom resource definition",
			obj: map[string]interface{}{
				"apiVersion": "monitoring.coreos.com/v1",
			},
			expectedGV: schema.GroupVersion{
				Group:   "monitoring.coreos.com",
				Version: "v1",
			},
		},
		{
			name: "apiextensions type",
			obj: map[string]interface{}{
				"apiVersion": "apiextensions.k8s.io/v1",
			},
			expectedGV: schema.GroupVersion{
				Group:   "apiextensions.k8s.io",
				Version: "v1",
			},
		},
		{
			name: "invalid apiVersion format",
			obj: map[string]interface{}{
				"apiVersion": "apps/v1/something",
			},
			expectedGV: schema.GroupVersion{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.obj == nil {
				defer func() {
					if r := recover(); r == nil {
						t.Errorf("expected panic for nil object, but none occurred")
					}
				}()
			}

			got := GroupVersion(tt.obj)

			if !reflect.DeepEqual(got, tt.expectedGV) {
				t.Errorf("GVK() = %v, want %v", got, tt.expectedGV)
			}
		})
	}
}
