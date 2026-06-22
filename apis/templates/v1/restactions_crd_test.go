package v1

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestGeneratedCRDKeepsResourcePolicyAnnotation is the falsifier guarding the
// `helm.sh/resource-policy: keep` annotation on the generated RESTActions CRD.
//
// Without `keep`, a `helm uninstall` deletes the RESTActions CRD and, with it,
// every RESTAction CR in the cluster — a data-loss risk at 50K scale. The
// annotation is emitted by the `+kubebuilder:metadata:annotations=...` marker on
// the RESTAction type (apis/templates/v1/restactions.go). controller-gen does not
// emit it on its own, so a controller-gen bump or an accidental marker removal
// could silently drop it during the next `make generate` / crds-sync. This test
// fails loudly if that ever happens.
func TestGeneratedCRDKeepsResourcePolicyAnnotation(t *testing.T) {
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

	const (
		key  = "helm.sh/resource-policy"
		want = "keep"
	)
	if got := crd.Metadata.Annotations[key]; got != want {
		t.Fatalf("generated CRD %s: metadata.annotations[%q] = %q, want %q "+
			"(the +kubebuilder:metadata:annotations marker on RESTAction must emit it; "+
			"without it helm uninstall deletes the CRD and all RESTAction CRs)",
			crdPath, key, got, want)
	}
}
