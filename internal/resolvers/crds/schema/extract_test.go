package schema

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractOpenAPISchemaFromCRD(t *testing.T) {
	// Task #312 fixture correction. extractOpenAPISchemaFromCRD's contract
	// (extract.go:40-48) extracts the schema node at the nested path
	// spec.versions[name==version].schema.openAPIV3Schema.properties.spec.
	// properties.widgetData — NOT openAPIV3Schema itself. The old fixture
	// stopped at openAPIV3Schema.type, so the lookup missed and the function
	// returned (nil, "schema OpenAPI v3 not found for version: v1"); the
	// non-fatal assert.NotNil then let the test deref a nil result on the
	// next line and SIGSEGV, poisoning the whole package run. This fixture
	// supplies the widgetData node the contract drills into (the value at
	// that path is what becomes the result's OpenAPIV3Schema).
	validCRD := map[string]any{
		"spec": map[string]any{
			"versions": []any{
				map[string]any{
					"name": "v1",
					"schema": map[string]any{
						"openAPIV3Schema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"spec": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"widgetData": map[string]any{
											"type": "object",
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	t.Run("valid schema extraction", func(t *testing.T) {
		result, err := extractOpenAPISchemaFromCRD(validCRD, "v1")
		// require (not assert) so a future contract drift FAILS CLEANLY
		// here instead of dereferencing a nil result on the next line and
		// SIGSEGV-ing the whole package (Task #312 — a panicking test
		// poisons every package run).
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.OpenAPIV3Schema)
		assert.Equal(t, "object", result.OpenAPIV3Schema.Type)
	})

	t.Run("missing version in CRD", func(t *testing.T) {
		_, err := extractOpenAPISchemaFromCRD(validCRD, "v2")
		assert.Error(t, err)
		assert.Equal(t, "version [v2] not found in CRD schema", err.Error())
	})

	t.Run("missing versions key", func(t *testing.T) {
		invalidCRD := map[string]any{
			"spec": map[string]any{},
		}
		_, err := extractOpenAPISchemaFromCRD(invalidCRD, "v1")
		assert.Error(t, err)
		assert.Equal(t, "no versions found in CRD", err.Error())
	})

	t.Run("invalid schema format", func(t *testing.T) {
		invalidSchemaCRD := map[string]any{
			"spec": map[string]any{
				"versions": []any{
					map[string]any{
						"name":   "v1",
						"schema": "invalid-format",
					},
				},
			},
		}
		_, err := extractOpenAPISchemaFromCRD(invalidSchemaCRD, "v1")
		assert.Error(t, err)
	})
}
