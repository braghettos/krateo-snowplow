package schema

import (
	"fmt"

	"github.com/krateoplatformops/plumbing/maps"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// compileCRDSchemaFn is the package-private seam over the compile step
// (extractOpenAPISchemaFromCRD + buildValidationFromSchemaData) that the
// Task #323 RED/GREEN falsifier counts to prove per-call recompiles drop from
// N to 1 once the per-GVR memo (schema_cache.go) is engaged. Production code
// path is unchanged: the variable is initialised once to
// extractOpenAPISchemaFromCRD and is only reassigned by test code. Mirrors
// discoveryClientForConfigFn (internal/dynamic/cached_client.go:176) and
// inClusterConfigFn (internal/dynamic/sa_client.go).
var compileCRDSchemaFn = extractOpenAPISchemaFromCRD

func extractOpenAPISchemaFromCRD(crd map[string]any, version string) (*apiextensions.CustomResourceValidation, error) {
	versions, found, err := unstructured.NestedSlice(crd, "spec", "versions")
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("no versions found in CRD")
	}

	for _, v := range versions {
		versionMap, ok := v.(map[string]any)
		if !ok {
			continue
		}

		if name, found := versionMap["name"].(string); !found || name != version {
			continue
		}

		schemaData, exists, err := maps.NestedMap(versionMap,
			"schema", "openAPIV3Schema",
			"properties", "spec", "properties", widgetDataKey)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("schema OpenAPI v3 not found for version: %s", version)
		}

		return buildValidationFromSchemaData(schemaData)
	}

	return nil, fmt.Errorf("version [%s] not found in CRD schema", version)
}

/*
func dumpSchemaRecursively(s *apiextensions.JSONSchemaProps, prefix string) {
	if s == nil {
		fmt.Printf("%s<nil>\n", prefix)
		return
	}
	fmt.Printf("%sType=%v Properties=[", prefix, s.Type)
	for k := range s.Properties {
		fmt.Printf("%s%s ", k, "")
	}
	fmt.Printf("]\n")

	for name, prop := range s.Properties {
		dumpSchemaRecursively(&prop, prefix+"  "+name+".")
	}
	if s.Items != nil && s.Items.Schema != nil {
		dumpSchemaRecursively(s.Items.Schema, prefix+"Items.")
	}
}
*/
