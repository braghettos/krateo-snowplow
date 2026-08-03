package schema

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/davecgh/go-spew/spew"
	"github.com/stretchr/testify/assert"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	"sigs.k8s.io/yaml"
)

func TestValidate(t *testing.T) {

	validSchema := &apiextensions.CustomResourceValidation{
		OpenAPIV3Schema: &apiextensions.JSONSchemaProps{
			Type: "object",
			Properties: map[string]apiextensions.JSONSchemaProps{
				"name": {Type: "string"},
				"age":  {Type: "integer"},
			},
		},
	}

	validDocument := []byte(`{"name": "John", "age": 30}`)
	invalidDocument := []byte(`{"name": "John", "age": "thirty"}`)

	t.Run("valid document", func(t *testing.T) {
		var jsonObj map[string]any
		err := json.Unmarshal(validDocument, &jsonObj)
		assert.NoError(t, err)
		err = validateCustomResource(validSchema, jsonObj)
		assert.NoError(t, err)
	})

	t.Run("invalid document type mismatch", func(t *testing.T) {
		var jsonObj map[string]any
		err := json.Unmarshal(invalidDocument, &jsonObj)
		assert.NoError(t, err)
		err = validateCustomResource(validSchema, jsonObj)
		assert.Error(t, err)
	})
}

func TestValidateMissingAdditionalPropertiesOnObjectIssue(t *testing.T) {
	data, err := os.ReadFile("../../../../testdata/missing-additional-props/table.crd.yaml")
	assert.NoError(t, err)

	var crd map[string]any
	err = yaml.Unmarshal(data, &crd)
	assert.NoError(t, err)

	schema, err := extractOpenAPISchemaFromCRD(crd, "v1beta1")
	assert.NoError(t, err)

	doc, err := os.ReadFile("../../../../testdata/missing-additional-props/table.json")
	assert.NoError(t, err)

	var jsonObj map[string]any
	err = json.Unmarshal(doc, &jsonObj)
	assert.NoError(t, err)

	tmp, ok := jsonObj["spec"].(map[string]any)
	assert.True(t, ok, "spec should be map[string]any")

	tmp, ok = tmp["widgetData"].(map[string]any)
	assert.True(t, ok, "widgetData should be map[string]any")
	//spew.Dump(tmp)

	err = validateCustomResource(schema, tmp)
	assert.Error(t, err)
	spew.Dump(err)
}
