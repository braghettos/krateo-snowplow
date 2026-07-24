// f2_kubectl_truth_test.go — Ship 0.30.242 H.c-layered Phase 3 F2.
//
// kubectl-truth equivalence on EvaluateRBAC. Live-cluster opt-in
// gate (Mode B per F2 plan Choice 2). For each (subject, verb, gvr,
// ns) triple, compute snowplow's verdict AND kubectl-can-i's
// verdict; assert they agree across all 405 triples.
//
// SHIP-GATE NOT MERGE-GATE
//
// F2 is a SHIP-GATE that runs manually (or pre-deploy) against the
// live GKE cluster. It is NOT a CI MERGE-GATE — CI has no GKE access.
// The F4 lint covers the static-asserted invariant for CI; F2 is the
// empirical-against-cluster gate that runs once per ship.
//
// MECHANISM (F2 plan Choice 1.B — kubectl-dumped snapshot)
//
//   1. Dump cluster RBAC at one point in time via kubectl:
//        kubectl get clusterrolebindings -o json
//        kubectl get rolebindings -A -o json
//        kubectl get clusterroles -o json
//        kubectl get roles -A -o json
//   2. Parse into a snowplow *RBACSnapshot; publish via
//      PublishRBACSnapshotForTest + RebuildSubjectIndexesForTest.
//   3. Wire cache.SetGlobal with a minimal stub so EvaluateRBAC's
//      Global() check passes (snapshot reads go via rbacSnap.Load
//      directly).
//   4. For each (subject, verb, gvr, ns) triple in the curated 405:
//      run rbac.EvaluateRBAC AND kubectl auth can-i --as <subject>
//      --context=<f2GKEContext>
//   5. Assert verdicts agree.
//
// TARGET CONTEXT (L-F2-REBIND, 2026-07-24)
//
//   The target kubectl context is read from SNOWPLOW_GATE_CLUSTER_CONTEXT
//   (default the live west4-release context, f2DefaultClusterContext). The
//   old hardcoded gke_neon-481711_us-central1-a_cluster-1 cluster was torn
//   down 2026-06-14 — hardcoding it left this the only wire-truth RBAC gate
//   code-bound to a DEAD cluster (it refused any other context and could
//   never run). The env unbind re-activates it without a code edit.
//
// GUARDRAILS
//
//   - Every kubectl invocation includes --context=<f2GKEContext> explicitly
//     (feedback_kubectl_verify_gke_context).
//   - SNOWPLOW_GATE_USE_LIVE_CLUSTER=true env required to run.
//   - kubectl current-context MUST EQUAL f2GKEContext (the resolved target)
//     or the test refuses to probe.
//   - Capture timestamp + snowplow + portal helm revs documented in
//     test output so reproducibility audits have metadata.
//   - Snapshot capture failure → explicit FAIL + diagnostic (never
//     silent skip).
//
// DIVERGENCE TRIAGE (per F2 plan failure-mode section)
//
// Any verdict-pair mismatch surfaces with the divergence list. 3-bucket
// triage:
//   (a) snapshot-staleness race — snowplow snapshot captured before
//       kubectl-can-i; RBAC mutated in between. Recheck capture
//       timestamps.
//   (b) snowplow evaluator bug — real defect in EvaluateRBAC's logic
//       (rule-walk, candidate selection, sort, etc.).
//   (c) kubectl-can-i semantic gap — RBAC has SubjectAccessReview
//       features (e.g., resourceNames scopes, virtual groups,
//       impersonation) snowplow doesn't model.

package evaltest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/rbac"

	rbacv1 "k8s.io/api/rbac/v1"
)

// f2DefaultClusterContext is the fallback kubectl context when
// SNOWPLOW_GATE_CLUSTER_CONTEXT is unset — the live west4-release deploy
// target (memory reference_gke_cluster.md). The old hardcoded
// gke_neon-481711_us-central1-a_cluster-1 cluster was torn down 2026-06-14,
// which left this wire-truth RBAC gate code-bound to a DEAD cluster (the
// L-F2-REBIND blind-spot). The context is now read from the environment so
// the gate re-targets without a code edit.
const f2DefaultClusterContext = "gke_operations-dev-krateo-io_europe-west4_krateo-installer-release"

// f2ClusterContext returns the kubectl context every F2 kubectl/helm call
// passes explicitly (--context=…), read from SNOWPLOW_GATE_CLUSTER_CONTEXT
// (default f2DefaultClusterContext). It is a package-level var (not a const)
// so the whole file's --context=+f2GKEContext call sites keep compiling
// unchanged while the target becomes environment-driven; it is resolved once
// at package init from the env.
var f2GKEContext = func() string {
	if c := os.Getenv("SNOWPLOW_GATE_CLUSTER_CONTEXT"); c != "" {
		return c
	}
	return f2DefaultClusterContext
}()

// ──────────────────────────────────────────────────────────────────────
// F2 — kubectl-truth equivalence
// ──────────────────────────────────────────────────────────────────────

func TestF2_KubectlTruth_VerdictEquivalence(t *testing.T) {
	if os.Getenv("SNOWPLOW_GATE_USE_LIVE_CLUSTER") != "true" {
		t.Skip("F2 ship-gate skipped — set SNOWPLOW_GATE_USE_LIVE_CLUSTER=true to run against live GKE cluster")
	}

	// Context-verify hard rule per feedback_kubectl_verify_gke_context.
	ctxOut, err := exec.Command("kubectl", "config", "current-context").Output()
	if err != nil {
		t.Fatalf("F2: kubectl config current-context failed: %v", err)
	}
	ctxName := strings.TrimSpace(string(ctxOut))
	if ctxName != f2GKEContext {
		t.Fatalf("F2 REFUSED: kubectl current-context=%q != the gate target %q "+
			"(set SNOWPLOW_GATE_CLUSTER_CONTEXT to re-target) — per feedback_kubectl_verify_gke_context",
			ctxName, f2GKEContext)
	}

	// Capture cluster baseline metadata for reproducibility.
	captureStart := time.Now()
	helmRev := f2QueryHelmRev(t, "snowplow")
	portalRev := f2QueryHelmRev(t, "portal")
	t.Logf("F2 cluster baseline: snowplow=%s portal=%s context=%s capture_start=%s",
		helmRev, portalRev, ctxName, captureStart.Format(time.RFC3339))

	// (1) Dump RBAC corpus into a snapshot.
	snap, dumpStats, err := f2DumpClusterRBAC(t)
	if err != nil {
		t.Fatalf("F2 SNAPSHOT CAPTURE FAILED: %v — this is NOT a silent skip; F2 requires successful capture", err)
	}
	captureEnd := time.Now()
	t.Logf("F2 snapshot capture: CRBs=%d RBs=%d ClusterRoles=%d Roles=%d duration=%s",
		dumpStats.CRBs, dumpStats.RBs, dumpStats.ClusterRoles, dumpStats.Roles,
		captureEnd.Sub(captureStart).Round(time.Millisecond))

	// (2) Publish + rebuild indexes; wire a minimal Global so
	//     EvaluateRBAC's nil-check passes.
	if err := f2WireSnapshot(t, snap); err != nil {
		t.Fatalf("F2 SNAPSHOT WIRING FAILED: %v", err)
	}

	// (3) Build the 405 (subject, verb, gvr, ns) triples.
	triples := f2BuildTriples(t)
	t.Logf("F2 test surface: %d (subject, verb, gvr, ns) triples", len(triples))

	// (4) Evaluate each triple via both paths in parallel.
	evalStart := time.Now()
	results := f2EvaluateAll(t, triples, 8 /* concurrency */)
	t.Logf("F2 evaluation: %d triples, duration=%s",
		len(results), time.Since(evalStart).Round(time.Millisecond))

	// (5) Assertion.
	var divergences []f2Divergence
	for _, r := range results {
		if r.SnowplowAllowed != r.KubectlAllowed {
			divergences = append(divergences, f2Divergence{
				Triple:          r.Triple,
				SnowplowAllowed: r.SnowplowAllowed,
				KubectlAllowed:  r.KubectlAllowed,
				SnowplowBindingUID: r.SnowplowBindingUID,
				SnowplowErr:     r.SnowplowErr,
				KubectlErr:      r.KubectlErr,
			})
		}
	}

	if len(divergences) > 0 {
		t.Errorf("F2 VERDICT-EQUIVALENCE FAIL: %d of %d triples diverged between snowplow and kubectl-can-i",
			len(divergences), len(results))
		t.Errorf("3-BUCKET TRIAGE per F2 plan:")
		t.Errorf("  (a) snapshot-staleness race — RBAC mutated between snowplow capture and kubectl-can-i.")
		t.Errorf("      Recheck capture window (%s → %s, duration=%s).",
			captureStart.Format(time.RFC3339), time.Now().Format(time.RFC3339),
			time.Since(captureStart).Round(time.Millisecond))
		t.Errorf("  (b) snowplow evaluator bug — real defect in EvaluateRBAC's logic.")
		t.Errorf("  (c) kubectl-can-i semantic gap — RBAC has SAR features snowplow doesn't model.")
		t.Errorf("")
		t.Errorf("First %d divergences:", min(20, len(divergences)))
		for i, d := range divergences {
			if i >= 20 {
				break
			}
			t.Errorf("  %s", d.String())
		}
		t.Fatalf("F2 FAIL")
	}

	t.Logf("F2 PASS GATE: %d triples — 100%% verdict equivalence between snowplow and kubectl-can-i.", len(results))
}

// ──────────────────────────────────────────────────────────────────────
// Cluster RBAC dump + snapshot construction
// ──────────────────────────────────────────────────────────────────────

type f2DumpStats struct {
	CRBs, RBs, ClusterRoles, Roles int
}

func f2QueryHelmRev(t *testing.T, release string) string {
	t.Helper()
	out, err := exec.Command("helm",
		"--kube-context="+f2GKEContext,
		"-n", "krateo-system",
		"history", release,
		"--max", "1",
		"-o", "json",
	).Output()
	if err != nil {
		return "<helm-history-err>"
	}
	var hist []struct {
		Revision int    `json:"revision"`
		Chart    string `json:"chart"`
	}
	if err := json.Unmarshal(out, &hist); err != nil || len(hist) == 0 {
		return "<parse-err>"
	}
	return fmt.Sprintf("rev=%d chart=%s", hist[len(hist)-1].Revision, hist[len(hist)-1].Chart)
}

// f2DumpClusterRBAC runs 4 kubectl get -o json calls against the live
// cluster and assembles a *cache.RBACSnapshot. Returns stats + the
// snapshot. All kubectl calls pass --context=gke_neon-481711_*
// explicitly.
func f2DumpClusterRBAC(t *testing.T) (*cache.RBACSnapshot, f2DumpStats, error) {
	t.Helper()
	snap := &cache.RBACSnapshot{
		RoleBindingsByNS:   map[string][]*rbacv1.RoleBinding{},
		ClusterRolesByName: map[string]*rbacv1.ClusterRole{},
		RolesByNSName:      map[string]*rbacv1.Role{},
	}
	stats := f2DumpStats{}

	// ClusterRoleBindings
	if out, err := f2KubectlGet("clusterrolebindings"); err != nil {
		return nil, stats, fmt.Errorf("kubectl get clusterrolebindings: %w", err)
	} else {
		var list rbacv1.ClusterRoleBindingList
		if err := json.Unmarshal(out, &list); err != nil {
			return nil, stats, fmt.Errorf("parse clusterrolebindings: %w", err)
		}
		snap.ClusterRoleBindings = make([]*rbacv1.ClusterRoleBinding, len(list.Items))
		for i := range list.Items {
			snap.ClusterRoleBindings[i] = &list.Items[i]
		}
		stats.CRBs = len(list.Items)
	}

	// RoleBindings (-A across all namespaces)
	if out, err := f2KubectlGetAllNS("rolebindings"); err != nil {
		return nil, stats, fmt.Errorf("kubectl get rolebindings -A: %w", err)
	} else {
		var list rbacv1.RoleBindingList
		if err := json.Unmarshal(out, &list); err != nil {
			return nil, stats, fmt.Errorf("parse rolebindings: %w", err)
		}
		for i := range list.Items {
			rb := &list.Items[i]
			snap.RoleBindingsByNS[rb.Namespace] = append(snap.RoleBindingsByNS[rb.Namespace], rb)
			stats.RBs++
		}
	}

	// ClusterRoles
	if out, err := f2KubectlGet("clusterroles"); err != nil {
		return nil, stats, fmt.Errorf("kubectl get clusterroles: %w", err)
	} else {
		var list rbacv1.ClusterRoleList
		if err := json.Unmarshal(out, &list); err != nil {
			return nil, stats, fmt.Errorf("parse clusterroles: %w", err)
		}
		for i := range list.Items {
			cr := &list.Items[i]
			snap.ClusterRolesByName[cr.Name] = cr
		}
		stats.ClusterRoles = len(list.Items)
	}

	// Roles (-A)
	if out, err := f2KubectlGetAllNS("roles"); err != nil {
		return nil, stats, fmt.Errorf("kubectl get roles -A: %w", err)
	} else {
		var list rbacv1.RoleList
		if err := json.Unmarshal(out, &list); err != nil {
			return nil, stats, fmt.Errorf("parse roles: %w", err)
		}
		for i := range list.Items {
			r := &list.Items[i]
			snap.RolesByNSName[r.Namespace+"/"+r.Name] = r
			stats.Roles++
		}
	}

	return snap, stats, nil
}

func f2KubectlGet(resource string) ([]byte, error) {
	cmd := exec.Command("kubectl",
		"--context="+f2GKEContext,
		"get", resource, "-o", "json",
	)
	return cmd.Output()
}

func f2KubectlGetAllNS(resource string) ([]byte, error) {
	cmd := exec.Command("kubectl",
		"--context="+f2GKEContext,
		"get", resource, "-A", "-o", "json",
	)
	return cmd.Output()
}

// f2WireSnapshot publishes the kubectl-dumped snapshot + rebuilds
// indexes + wires a minimal cache.Global so EvaluateRBAC's nil-check
// passes.
//
// MECHANISM: EvaluateRBAC reads the snapshot via rbacSnap.Load (not
// via the watcher's typed body — watcher.go:Snapshot is just
// `_ = rw; return rbacSnap.Load()`), so a sentinel ResourceWatcher
// suffices for the Global()-nil-check. We use newTestWatcher
// (evaluate_test.go pattern) to construct an empty dynamic-fake
// watcher + wire SetGlobal, then STAMP OVER its snapshot with the
// kubectl-dumped one.
func f2WireSnapshot(t *testing.T, snap *cache.RBACSnapshot) error {
	t.Helper()
	// (1) Wire a sentinel watcher so cache.Global() returns non-nil.
	//     newTestWatcher seeds an empty dynamic-fake + waits for
	//     cache sync + cache.SetGlobal(rw) + registers t.Cleanup for
	//     teardown. The fake's snapshot is empty; we overwrite it
	//     immediately below.
	newTestWatcher(t)

	// (2) Stamp PublishSeq monotonically (mimics the Phase 2a
	//     writer-order fix); rebuild subject indexes; publish the
	//     kubectl-dumped snapshot, overwriting the fake's empty one.
	snap.PublishSeq = uint64(time.Now().UnixNano())
	cache.RebuildSubjectIndexesForTest(snap)
	cache.PublishRBACSnapshotForTest(snap)
	t.Cleanup(func() { cache.PublishRBACSnapshotForTest(nil) })
	return nil
}

// ──────────────────────────────────────────────────────────────────────
// Triple construction (F2 plan Choice 3 — 3 × 5 × 27 = 405)
// ──────────────────────────────────────────────────────────────────────

type f2Subject struct {
	Username string
	Groups   []string
	Label    string // short label for error messages
}

type f2Triple struct {
	Subject f2Subject
	Verb    string
	Group   string
	Resource string
	Namespace string
}

func (t f2Triple) String() string {
	return fmt.Sprintf("[%s] %s/%s in ns=%q verb=%s",
		t.Subject.Label, t.Group, t.Resource, t.Namespace, t.Verb)
}

func f2Subjects() []f2Subject {
	return []f2Subject{
		// admin: broad-RBAC via system:masters + admins groups.
		{Username: "admin", Groups: []string{"admins", "system:masters", "system:authenticated"}, Label: "admin"},
		// cyberjoker: narrow-RBAC via devs group (cluster has devs Group bindings).
		{Username: "cyberjoker", Groups: []string{"devs", "system:authenticated"}, Label: "cyberjoker"},
		// anonymous: deny path (no group memberships that grant anything substantive).
		{Username: "system:anonymous", Groups: []string{"system:unauthenticated"}, Label: "anonymous"},
	}
}

// f2GVRs returns the 5 representative GVRs. Per the F2 plan, prefer
// "diversity of authorization shapes" over arbitrary name variety.
// CRITICAL: kubectl-can-i validates GVR EXISTENCE before checking
// RBAC (the apiserver's authorizer chain). A non-existent GVR
// returns kubectl=deny REGARDLESS of RBAC state. So every GVR in
// this set MUST be a real, registered cluster resource — verified
// via `kubectl api-resources` at test-fixture-build time.
//
// Snowplow's RBAC evaluator does NOT validate GVR existence (RBAC
// is just one of the apiserver's authorizer chain links; resource-
// existence checking is a separate concern); we mirror the cluster's
// chain semantics here by only testing GVRs that actually exist.
//
// Real Krateo resources from the cluster (kubectl api-resources):
//   - widgets.templates.krateo.io/panels (widget collection)
//   - templates.krateo.io/restactions
//   - composition.krateo.io/compositions (varies by CRD; the
//     compositions GROUP exists, ResourceWatcher discovers it via
//     CRDs at the storedVersions[0])
//   - core/namespaces (cluster-wide)
//   - core/configmaps (per-namespace)
func f2GVRs() []struct{ Group, Resource string } {
	return []struct{ Group, Resource string }{
		{"widgets.templates.krateo.io", "panels"},
		{"templates.krateo.io", "restactions"},
		{"composition.krateo.io", "githubscaffoldingwithcompositionpages"},
		{"", "namespaces"},
		{"", "configmaps"},
	}
}

// f2Namespaces returns 27 namespaces curated for diversity of
// authorization shapes (per Diego's ACK comment).
func f2Namespaces(t *testing.T) []string {
	t.Helper()
	// Required namespaces (deterministic):
	required := []string{
		"krateo-system",  // cyberjoker scoped grants land here
		"kube-system",    // admin-only
		"kube-public",    // admin-only
		"default",        // most users have access
		"demo-system",    // narrow user RBAC pattern (per cluster baseline)
	}

	// 22 additional namespaces from a deterministic sort of the
	// cluster's namespace list (skips those already in `required`).
	out, err := exec.Command("kubectl",
		"--context="+f2GKEContext,
		"get", "ns", "-o", "name",
	).Output()
	if err != nil {
		t.Logf("F2 namespace list fetch failed (%v) — using required-only set", err)
		return required
	}
	var clusterNS []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		// "namespace/foo" → "foo"
		n := strings.TrimPrefix(line, "namespace/")
		clusterNS = append(clusterNS, n)
	}
	sort.Strings(clusterNS)

	have := map[string]bool{}
	for _, n := range required {
		have[n] = true
	}
	want := append([]string(nil), required...)
	for _, n := range clusterNS {
		if len(want) >= 27 {
			break
		}
		if have[n] {
			continue
		}
		want = append(want, n)
		have[n] = true
	}
	return want
}

func f2BuildTriples(t *testing.T) []f2Triple {
	t.Helper()
	subjects := f2Subjects()
	gvrs := f2GVRs()
	namespaces := f2Namespaces(t)
	out := make([]f2Triple, 0, len(subjects)*len(gvrs)*len(namespaces))
	for _, s := range subjects {
		for _, gv := range gvrs {
			for _, ns := range namespaces {
				// Pick a verb that's most semantically interesting:
				// - For namespaces resource use "get" with the ns
				//   name as the resource name's stand-in (kubectl
				//   handles this via --resource flag, not --name).
				// - For all others use "list".
				verb := "list"
				if gv.Resource == "namespaces" {
					verb = "get"
				}
				out = append(out, f2Triple{
					Subject:   s,
					Verb:      verb,
					Group:     gv.Group,
					Resource:  gv.Resource,
					Namespace: ns,
				})
			}
		}
	}
	return out
}

// ──────────────────────────────────────────────────────────────────────
// Dual-evaluator (snowplow + kubectl-can-i)
// ──────────────────────────────────────────────────────────────────────

type f2Result struct {
	Triple             f2Triple
	SnowplowAllowed    bool
	SnowplowBindingUID string
	SnowplowErr        error
	KubectlAllowed     bool
	KubectlErr         error
}

type f2Divergence struct {
	Triple             f2Triple
	SnowplowAllowed    bool
	SnowplowBindingUID string
	SnowplowErr        error
	KubectlAllowed     bool
	KubectlErr         error
}

func (d f2Divergence) String() string {
	snowplowSig := "deny"
	if d.SnowplowAllowed {
		snowplowSig = "allow uid=" + d.SnowplowBindingUID
	}
	if d.SnowplowErr != nil {
		snowplowSig = "ERR: " + d.SnowplowErr.Error()
	}
	kubectlSig := "deny"
	if d.KubectlAllowed {
		kubectlSig = "allow"
	}
	if d.KubectlErr != nil {
		kubectlSig = "ERR: " + d.KubectlErr.Error()
	}
	return fmt.Sprintf("%s | snowplow=%s | kubectl=%s",
		d.Triple.String(), snowplowSig, kubectlSig)
}

func f2EvaluateAll(t *testing.T, triples []f2Triple, concurrency int) []f2Result {
	t.Helper()
	results := make([]f2Result, len(triples))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, tr := range triples {
		i, tr := i, tr
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = f2EvaluateOne(tr)
		}()
	}
	wg.Wait()
	return results
}

func f2EvaluateOne(tr f2Triple) f2Result {
	r := f2Result{Triple: tr}

	// Snowplow path.
	allowed, uid, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
		Username:  tr.Subject.Username,
		Groups:    tr.Subject.Groups,
		Verb:      tr.Verb,
		Group:     tr.Group,
		Resource:  tr.Resource,
		Namespace: tr.Namespace,
	})
	r.SnowplowAllowed = allowed
	r.SnowplowBindingUID = uid
	r.SnowplowErr = err

	// kubectl-can-i path.
	r.KubectlAllowed, r.KubectlErr = f2KubectlCanI(tr)
	return r
}

func f2KubectlCanI(tr f2Triple) (bool, error) {
	args := []string{
		"--context=" + f2GKEContext,
		"auth", "can-i",
		tr.Verb,
		f2ResourceArg(tr),
		"--as=" + tr.Subject.Username,
	}
	// Pass groups via --as-group (kubectl 1.30+).
	for _, g := range tr.Subject.Groups {
		args = append(args, "--as-group="+g)
	}
	if tr.Namespace != "" {
		args = append(args, "-n", tr.Namespace)
	}
	cmd := exec.Command("kubectl", args...)
	out, err := cmd.Output()
	trimmed := strings.TrimSpace(string(out))
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// kubectl auth can-i exits 1 on deny — that's a verdict, not an error.
			if trimmed == "no" || strings.HasPrefix(trimmed, "no") {
				return false, nil
			}
			// Other non-zero exit with non-"no" output — surface as error.
			return false, fmt.Errorf("kubectl exit=%d stdout=%q stderr=%q", exitErr.ExitCode(), trimmed, string(exitErr.Stderr))
		}
		return false, err
	}
	return strings.HasPrefix(trimmed, "yes"), nil
}

// f2ResourceArg builds the resource argument for `kubectl auth can-i`:
// "<resource>.<group>" (kubectl handles the dotted form) or just
// "<resource>" for the core group.
func f2ResourceArg(tr f2Triple) string {
	if tr.Group == "" {
		return tr.Resource
	}
	return tr.Resource + "." + tr.Group
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
