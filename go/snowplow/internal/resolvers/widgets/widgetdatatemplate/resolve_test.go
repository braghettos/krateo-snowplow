package widgetdatatemplate_test

import (
	"context"
	"testing"

	templatesv1 "github.com/krateo-platformops/snowplow/apis/templates/v1"
	"github.com/krateo-platformops/snowplow/internal/resolvers/widgets/widgetdatatemplate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveWidgetDataTemplates(t *testing.T) {
	tests := []struct {
		name        string
		items       []templatesv1.WidgetDataTemplate
		dict        map[string]any
		want        []widgetdatatemplate.EvalResult
		expectedErr bool
	}{
		{
			name:  "No template",
			items: []templatesv1.WidgetDataTemplate{},
			dict:  map[string]any{},
			want:  []widgetdatatemplate.EvalResult{},
		},
		{
			name: "Valid template with full path",
			items: []templatesv1.WidgetDataTemplate{
				{
					ForPath:    "spec.widgetData.value",
					Expression: "${ .data.value }",
				},
			},
			dict: map[string]any{
				"data": map[string]any{
					"value": "test-value",
				},
			},
			want: []widgetdatatemplate.EvalResult{
				{Path: "spec.widgetData.value", Value: "test-value"},
			},
		},
		{
			name: "Valid template with relative path",
			items: []templatesv1.WidgetDataTemplate{
				{
					ForPath:    "value",
					Expression: "${ .data.value }",
				},
			},
			dict: map[string]any{
				"data": map[string]any{
					"value": "test-value-xxx",
				},
			},
			want: []widgetdatatemplate.EvalResult{
				{Path: "value", Value: "test-value-xxx"},
			},
		},
		{
			name: "Valid template with starting dot in the path",
			items: []templatesv1.WidgetDataTemplate{
				{
					ForPath:    ".value",
					Expression: "${ .data.value }",
				},
			},
			dict: map[string]any{
				"data": map[string]any{
					"value": "test-value-yyy",
				},
			},
			want: []widgetdatatemplate.EvalResult{
				{Path: "value", Value: "test-value-yyy"},
			},
		},
		{
			name: "Error during expression evaluation",
			items: []templatesv1.WidgetDataTemplate{
				{
					ForPath:    "value",
					Expression: "${ .invalid }",
				},
			},
			dict:        map[string]any{},
			expectedErr: false,
			want: []widgetdatatemplate.EvalResult{
				{Path: "value", Value: nil},
			},
		},
		{
			name: "Template with missing path",
			items: []templatesv1.WidgetDataTemplate{
				{
					Expression: ".data.value",
				},
			},
			dict: map[string]any{},
			want: []widgetdatatemplate.EvalResult{},
		},
		{
			name: "Template with missing expression",
			items: []templatesv1.WidgetDataTemplate{
				{
					ForPath: "value",
				},
			},
			dict: map[string]any{},
			want: []widgetdatatemplate.EvalResult{},
		},
		{
			name: "Invalid template (empty forPath)",
			items: []templatesv1.WidgetDataTemplate{
				{
					ForPath:    "",
					Expression: ".data.value",
				},
			},
			dict: map[string]any{},
			want: []widgetdatatemplate.EvalResult{},
		},
		{
			name: "Invalid template (empty expression)",
			items: []templatesv1.WidgetDataTemplate{
				{
					ForPath:    "value",
					Expression: "",
				},
			},
			dict: map[string]any{},
			want: []widgetdatatemplate.EvalResult{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := widgetdatatemplate.Resolve(context.Background(), widgetdatatemplate.ResolveOptions{
				Items:      tt.items,
				DataSource: tt.dict,
			})

			if tt.expectedErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			assert.Equal(t, tt.want, got)
		})
	}
}
