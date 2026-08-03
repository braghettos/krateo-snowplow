// uaf_portal_precondition_test.go — Ship 0.30.242 H.c-layered Phase 0
// mechanical pre-implementation gate (F8 falsifier).
//
// HARD PRE-MERGE INVARIANT: under H.c-layered the `apistage` L1 cell is
// RBAC-narrowed at populate time by the per-stage EvaluateRBAC first-match
// BindingUID. The cohort-gate-memo (apistage_cohort_memo.go) is DELETED
// in this ship. Cell correctness therefore depends on every fanout stage
// carrying a UAF declaration — without UAF, the stage executes under SA
// identity cluster-wide and the cell holds every namespace's items;
// a non-SA user reads the cell and sees admin's items. LEAK.
//
// This test loads the F5 customer-scenario portal RESTAction manifests
// for {compositions-list, compositions-panels, blueprints-panels} and
// asserts every fanout-eligible stage (one declaring a userAccessFilter)
// carries a valid UAF spec. FAIL means the portal hasn't yet rolled out
// tasks #73-78 to the F5 fixtures; MERGING H.c-layered now would leak.
// The test BLOCKS the ship at the CI surface.
//
// CAPTURE INSTRUCTIONS (Mode A — checked-in fixture, DEFAULT):
//
//	for fix in compositions-list compositions-panels blueprints-panels; do
//	  kubectl --context gke_neon-481711_us-central1-a_cluster-1 \
//	    get restactions.templates.krateo.io -A -o json \
//	  | jq --arg n "$fix" '.items[] | select(.metadata.name==$n)' \
//	  > internal/cache/testdata/uaf-portal-fixtures/$fix.json
//	done
//
// MODE B — live cluster (CI optional, SNOWPLOW_GATE_USE_LIVE_CLUSTER=true):
//
//	shells out to kubectl get restactions.templates.krateo.io -A -o json;
//	the active kubectl context MUST start with "gke_neon-481711_" per
//	feedback_kubectl_verify_gke_context (HARD rule); otherwise the test
//	REFUSES to probe and FAILs with an explicit context-mismatch diagnostic.
//
// PASS CRITERION: every fanout stage of every F5 fixture RA carries a
// userAccessFilter with non-empty verb AND EXACTLY ONE of resource or
// resourcesFrom. Group MAY be empty (core group). namespaceFrom is
// optional (defaults to .metadata.namespace per templates.UAF spec).
//
// FAIL CRITERION (BLOCKS MERGE):
//   - any F5 fixture missing from disk (Mode A) or live cluster (Mode B)
//   - any fixture having ZERO stages with a userAccessFilter declaration
//     (the RA is still in v3 single-cluster-wide-LIST shape; portal #74
//     has not yet rolled out)
//   - any UAF-declared stage missing required fields (verb, exactly-one
//     of resource/resourcesFrom)
//
// THIS TEST IS NOT IN THE LATENCY-MEASUREMENT PATH. Mode A reads checked-in
// JSON; Mode B is gated by env var and only used for ad-hoc dev probes.
// feedback_no_kubectl_in_measurement is preserved.

package cache

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// f5FixtureRAs — customer-scenario RAs covered by F5 (§22.6.1). If F5
// fixtures expand, this list MUST grow in lockstep.
var f5FixtureRAs = []string{
	"compositions-list",
	"compositions-panels",
	"blueprints-panels",
}

// uafFields — required keys in spec.api[].userAccessFilter for a fanout
// stage to be H.c-layered-compatible. Mirrors
// templates.UserAccessFilterSpec (apis/templates/v1/core.go:112-165).
type uafFields struct {
	Verb          string `json:"verb"`
	Group         string `json:"group"`
	Resource      string `json:"resource"`
	ResourcesFrom string `json:"resourcesFrom"`
	NamespaceFrom string `json:"namespaceFrom"`
}

// restActionWire — the minimum shape we parse out of the kubectl/fixture
// JSON. spec.api[] entries with a userAccessFilter participate in
// per-user fanout; those without it are non-fanout (e.g. UAF emit stages
// that list namespaces under SA identity); the gate only requires UAF
// on fanout-eligible stages.
type restActionWire struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		API []struct {
			Name             string     `json:"name"`
			UserAccessFilter *uafFields `json:"userAccessFilter,omitempty"`
		} `json:"api"`
	} `json:"spec"`
}

// loadFixture dispatches to Mode A (checked-in JSON) or Mode B (live
// kubectl), based on SNOWPLOW_GATE_USE_LIVE_CLUSTER.
func loadFixture(t *testing.T, name string) (restActionWire, bool) {
	t.Helper()
	if os.Getenv("SNOWPLOW_GATE_USE_LIVE_CLUSTER") == "true" {
		return loadFromLiveCluster(t, name)
	}
	return loadFromCheckedInFixture(t, name)
}

func loadFromCheckedInFixture(t *testing.T, name string) (restActionWire, bool) {
	t.Helper()
	path := filepath.Join("testdata", "uaf-portal-fixtures", name+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Logf("Phase 0 gate: fixture %s NOT FOUND (%v). Re-capture per test header; commit; re-run gate.", path, err)
		return restActionWire{}, false
	}
	var ra restActionWire
	if err := json.Unmarshal(data, &ra); err != nil {
		t.Logf("Phase 0 gate: fixture %s MALFORMED (%v)", path, err)
		return restActionWire{}, false
	}
	return ra, true
}

func loadFromLiveCluster(t *testing.T, name string) (restActionWire, bool) {
	t.Helper()
	ctxOut, err := exec.Command("kubectl", "config", "current-context").Output()
	if err != nil {
		t.Logf("Phase 0 gate: kubectl config current-context failed (%v)", err)
		return restActionWire{}, false
	}
	ctx := strings.TrimSpace(string(ctxOut))
	if !strings.HasPrefix(ctx, "gke_neon-481711_") {
		t.Logf("Phase 0 gate: kubectl context %q is NOT gke_neon-481711_*; per feedback_kubectl_verify_gke_context the gate REFUSES to probe", ctx)
		return restActionWire{}, false
	}
	out, err := exec.Command("kubectl", "get", "restactions.templates.krateo.io",
		"-A", "-o", "json").Output()
	if err != nil {
		t.Logf("Phase 0 gate: kubectl get restactions failed (%v)", err)
		return restActionWire{}, false
	}
	var list struct {
		Items []restActionWire `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		t.Logf("Phase 0 gate: kubectl output malformed (%v)", err)
		return restActionWire{}, false
	}
	for _, ra := range list.Items {
		if ra.Metadata.Name == name {
			return ra, true
		}
	}
	t.Logf("Phase 0 gate: RA %q NOT FOUND in live cluster", name)
	return restActionWire{}, false
}

// TestUAFPortalPrecondition is the F8 mechanical gate.
//
// PASS = every F5 fixture loaded AND every fanout-eligible stage's
// userAccessFilter has a valid shape.
//
// FAIL = any fixture missing OR any fixture has zero UAF-declared stages
// (still v3 shape) OR any UAF-declared stage has invalid fields.
//
// A FAIL means portal #74 has not yet rolled out per-stage UAF
// declarations to the F5 fixtures; merging H.c-layered now would delete
// the cohort-gate-memo while leaving stages SA-identified and cluster-
// wide-scoped — leaking cross-cohort items. The gate BLOCKS the merge.
func TestUAFPortalPrecondition(t *testing.T) {
	type failure struct {
		ra, stage, reason string
	}
	var failures []failure
	var loaded int

	for _, name := range f5FixtureRAs {
		ra, ok := loadFixture(t, name)
		if !ok {
			failures = append(failures, failure{name, "<missing>", "fixture not loaded — see capture instructions in test header"})
			continue
		}
		loaded++
		fanoutCount := 0
		for _, stage := range ra.Spec.API {
			if stage.UserAccessFilter == nil {
				// Non-fanout stage. Allowed (e.g., UAF emit stages or
				// non-resource preludes that list namespaces under SA).
				continue
			}
			fanoutCount++
			uaf := stage.UserAccessFilter
			if uaf.Verb == "" {
				failures = append(failures, failure{name, stage.Name, "userAccessFilter.verb is empty"})
			}
			hasResource := uaf.Resource != ""
			hasResourcesFrom := uaf.ResourcesFrom != ""
			if hasResource == hasResourcesFrom {
				// templates.UserAccessFilterSpec CEL invariant:
				// EXACTLY ONE of resource OR resourcesFrom must be set
				// (apis/templates/v1/core.go:111). Both empty OR both
				// present is invalid.
				failures = append(failures, failure{name, stage.Name,
					"userAccessFilter must specify EXACTLY ONE of resource or resourcesFrom"})
			}
			// Group MAY be empty (core group); no required-presence check.
			// NamespaceFrom is optional (defaults to .metadata.namespace);
			// no check.
		}
		if fanoutCount == 0 {
			// The RA has NO UAF-declared stages — it is still in v3
			// single-cluster-wide-LIST shape. H.c-layered cannot rely
			// on per-stage RBAC narrowing for this RA.
			failures = append(failures, failure{name, "<all stages>", "RA has ZERO stages with userAccessFilter — portal #74 has NOT rolled out per-stage UAF declarations for this fixture; H.c-layered MUST NOT merge while this fixture is in v3 shape"})
		}
	}

	if len(failures) > 0 {
		var b strings.Builder
		b.WriteString("Phase 0 UAF portal precondition FAILED — Ship 0.30.242 H.c-layered MUST NOT MERGE.\n")
		b.WriteString(fmt.Sprintf("Loaded %d of %d F5 fixtures.\n", loaded, len(f5FixtureRAs)))
		b.WriteString("Failures:\n")
		for _, f := range failures {
			b.WriteString("  RA=")
			b.WriteString(f.ra)
			b.WriteString(" stage=")
			b.WriteString(f.stage)
			b.WriteString(" reason=")
			b.WriteString(f.reason)
			b.WriteString("\n")
		}
		b.WriteString("\nNext steps:\n")
		b.WriteString("  1. Wait for portal tasks #73-78 (especially #74) to roll out the\n")
		b.WriteString("     per-stage UAF declarations to {compositions-list, compositions-panels,\n")
		b.WriteString("     blueprints-panels} in the krateo helm release.\n")
		b.WriteString("  2. Re-capture the fixtures per the test header's kubectl/jq command.\n")
		b.WriteString("  3. Commit the fresh fixtures alongside this ship's diff.\n")
		b.WriteString("  4. Re-run: go test ./internal/cache/ -run TestUAFPortalPrecondition\n")
		b.WriteString("  5. ONLY proceed to Phase 2 implementation after this gate PASSES.\n")
		t.Fatal(b.String())
	}
}
