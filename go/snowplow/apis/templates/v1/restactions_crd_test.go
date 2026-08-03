package v1

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestGeneratedCRDOmitsResourcePolicyAnnotation is the falsifier locking in the
// teardown-completeness decision (chart #31): the generated RESTActions CRD must
// NOT carry the `helm.sh/resource-policy: keep` annotation.
//
// Rationale (#31): the RESTActions CRD has a single Helm owner — the dedicated
// crds-subchart — and no parent chart templates it. Upgrades are unaffected
// because Helm only prunes resources that LEAVE the chart, and the CRD never
// leaves it. Re-adding `keep` would silently orphan the component CRD on a
// `helm uninstall` of the crds-subchart release (teardown never reaches a clean
// slate). A controller-gen marker (#9/#39) previously re-emitted `keep`,
// defeating #31; that marker was reverted. This test fails loudly if `keep`
// ever reappears on the generated CRD.
func TestGeneratedCRDOmitsResourcePolicyAnnotation(t *testing.T) {
	// crds/ lives at the repo root; this test file is apis/templates/v1.
	crdPath := filepath.Join("..", "..", "..", "crds", "templates.krateo.io_restactions.yaml")

	raw, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("read generated CRD %s: %v", crdPath, err)
	}

	var crd struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("unmarshal generated CRD: %v", err)
	}

	const key = "helm.sh/resource-policy"
	if got, present := crd.Metadata.Annotations[key]; present {
		t.Fatalf("generated CRD %s: metadata.annotations[%q] = %q, want it ABSENT "+
			"(per chart #31 teardown-completeness: the crds-subchart is the CRD's single Helm "+
			"owner, upgrades are unaffected because the CRD never leaves the chart, and `keep` "+
			"would orphan the CRD on helm uninstall; the +kubebuilder:metadata:annotations "+
			"marker on RESTAction must NOT re-emit it)",
			crdPath, key, got)
	}
}
