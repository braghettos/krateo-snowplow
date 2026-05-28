// bindingsbygvr_bench_test.go — Gate 2 (DIAGNOSTIC, local-only) CPU
// profile of the proposed BindingsByGVR reverse-index build over the
// live RBAC topology.
//
// SANDBOX — this is its OWN package (gate2bench) with NO TestMain. It
// does NOT import or touch internal/rbac's destructive TestMain (which
// DELETES the RESTAction CRD). It reads a READ-ONLY JSON dump of the
// live cluster RBAC topology produced by:
//
//	kubectl get clusterrolebindings,clusterroles,rolebindings,roles -A -o json
//
// pointed at by the GATE2_RBAC_DUMP env var. The dump is parsed into
// typed rbacv1 objects, the proposed index is built, and the per-build
// CPU time is measured.
//
// PURPOSE — decide design question 2: would rebuilding the proposed
// BindingsByGVR reverse index on EVERY rbac-snapshot republish (~4.6/s
// observed) add an acceptable steady-state CPU cost?
//
//	PASS  (<1% CPU) → rebuild per republish is safe.
//	FAIL  (>=1% CPU) → must be incremental (only-changed-bindings) with
//	                   lazy reseed.
//
// roleRefGrantsGetList MIRRORS the existing wildcard matching in
// internal/rbac/evaluate.go:446-526 (rulesPermit / stringSliceMatches /
// resourceNameMatches). A binding is enrolled under a {apiGroup,
// resource} GVR iff its roleRef's rules grant get OR list on that GVR;
// a `*/*` rule lands the binding in the WildcardBindings bucket.
//
// Run locally with:
//
//	GATE2_RBAC_DUMP=/tmp/gate2-rbac/rbac_all.json \
//	  go test ./internal/rbac/gate2bench/ -run x -bench BindingsByGVR \
//	  -benchtime 10x -cpuprofile /tmp/gate2-rbac/cpu.prof -v

package gate2bench

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
)

// ─────────────────────────────────────────────────────────────────────
// Matcher — MIRRORS internal/rbac/evaluate.go:446-526. Kept verbatim so
// the index-enrolment predicate is bit-faithful to the live evaluator.
// ─────────────────────────────────────────────────────────────────────

// stringSliceMatches — verbatim from evaluate.go:519-526.
func stringSliceMatches(allowed []string, want string) bool {
	for _, a := range allowed {
		if a == "*" || a == want {
			return true
		}
	}
	return false
}

// gvrKey is a {apiGroup, resource} pair — the index key.
type gvrKey struct {
	group    string
	resource string
}

// roleRefGrantsGetList reports whether any rule in `rules` grants `get`
// OR `list` on the given {group, resource} GVR — mirroring rulesPermit's
// verb/group/resource wildcard semantics (evaluate.go:446-463). The
// resourceNames-scope check is mirrored too: a resourceNames-scoped rule
// can never grant `list` (a collection verb), and for `get` it would
// need the object name — which the index build does not have — so a
// resourceNames-scoped rule does NOT enrol the binding under the GVR for
// LIST navigation (the navigation path is LIST-shaped). We therefore
// treat a non-empty ResourceNames rule as NOT granting list (faithful to
// resourceNameMatches for the collection verb `list`).
//
// Returns true iff a non-resourceNames-scoped rule grants get OR list on
// the given GVR (wildcard-aware via stringSliceMatches). The `*/*`
// wildcard case is handled separately by rulesAreWildcard (the binding
// lands in the WildcardBindings bucket once, not per-GVR).
func roleRefGrantsGetList(rules []rbacv1.PolicyRule, key gvrKey) bool {
	for _, rule := range rules {
		// Verb: need get OR list. Mirror stringSliceMatches per-verb.
		grantsGet := stringSliceMatches(rule.Verbs, "get")
		grantsList := stringSliceMatches(rule.Verbs, "list")
		if !grantsGet && !grantsList {
			continue
		}
		// resourceNames-scoped rules can't grant list (collection verb)
		// and we have no object name for get → do not enrol for nav LIST.
		if len(rule.ResourceNames) > 0 {
			continue
		}
		if !stringSliceMatches(rule.APIGroups, key.group) {
			continue
		}
		if !stringSliceMatches(rule.Resources, key.resource) {
			continue
		}
		return true
	}
	return false
}

func containsStar(s []string) bool {
	for _, v := range s {
		if v == "*" {
			return true
		}
	}
	return false
}

// rulesAreWildcard reports whether the rule set contains a `*/*` get/list
// rule (independent of any specific GVR) — used to populate the
// WildcardBindings bucket ONCE per binding rather than per GVR.
func rulesAreWildcard(rules []rbacv1.PolicyRule) bool {
	for _, rule := range rules {
		if len(rule.ResourceNames) > 0 {
			continue
		}
		if (stringSliceMatches(rule.Verbs, "get") || stringSliceMatches(rule.Verbs, "list")) &&
			containsStar(rule.APIGroups) && containsStar(rule.Resources) {
			return true
		}
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────
// Topology — typed snapshot built once from the live dump.
// ─────────────────────────────────────────────────────────────────────

type topology struct {
	clusterRoleBindings []*rbacv1.ClusterRoleBinding
	roleBindings        []*rbacv1.RoleBinding
	clusterRolesByName  map[string]*rbacv1.ClusterRole
	rolesByNSName       map[string]*rbacv1.Role
}

// subjectRef is the index value — one (binding-kind, ns, name) tuple per
// enrolled binding. The proposed index stores subjects-of-bindings; we
// materialise the binding identity (the subjects are reachable from it),
// matching the snapshot's pointer-slice shape (no struct duplication).
type subjectRef struct {
	kind string // "ClusterRoleBinding" | "RoleBinding"
	ns   string // "" for CRB
	name string
}

// metaListDump mirrors the `kubectl get ... -o json` envelope: a single
// List with a heterogeneous items[] (CRB/CR/RB/Role discriminated by
// .kind).
type metaListDump struct {
	Items []json.RawMessage `json:"items"`
}

type kindProbe struct {
	Kind string `json:"kind"`
}

// loadTopology parses the live dump into typed objects. Parsed ONCE
// (sync.Once in the benchmark) so the per-iteration cost is purely the
// index build, not JSON parsing.
func loadTopology(tb testing.TB) *topology {
	tb.Helper()
	path := os.Getenv("GATE2_RBAC_DUMP")
	if path == "" {
		tb.Skip("GATE2_RBAC_DUMP not set — point it at the kubectl RBAC dump JSON")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		tb.Fatalf("read dump %q: %v", path, err)
	}
	var dump metaListDump
	if err := json.Unmarshal(raw, &dump); err != nil {
		tb.Fatalf("unmarshal dump envelope: %v", err)
	}

	top := &topology{
		clusterRolesByName: map[string]*rbacv1.ClusterRole{},
		rolesByNSName:      map[string]*rbacv1.Role{},
	}
	for _, item := range dump.Items {
		var kp kindProbe
		if err := json.Unmarshal(item, &kp); err != nil {
			continue
		}
		switch kp.Kind {
		case "ClusterRoleBinding":
			var o rbacv1.ClusterRoleBinding
			if json.Unmarshal(item, &o) == nil {
				top.clusterRoleBindings = append(top.clusterRoleBindings, &o)
			}
		case "RoleBinding":
			var o rbacv1.RoleBinding
			if json.Unmarshal(item, &o) == nil {
				top.roleBindings = append(top.roleBindings, &o)
			}
		case "ClusterRole":
			var o rbacv1.ClusterRole
			if json.Unmarshal(item, &o) == nil {
				top.clusterRolesByName[o.Name] = &o
			}
		case "Role":
			var o rbacv1.Role
			if json.Unmarshal(item, &o) == nil {
				top.rolesByNSName[o.Namespace+"/"+o.Name] = &o
			}
		}
	}
	tb.Logf("topology loaded: CRB=%d RB=%d CR=%d Role=%d",
		len(top.clusterRoleBindings), len(top.roleBindings),
		len(top.clusterRolesByName), len(top.rolesByNSName))
	return top
}

// rulesForRoleRef resolves a binding's roleRef to its rule set — mirrors
// roleRefPermits (evaluate.go:407-434). namespace is "" for a CRB; for a
// RoleBinding it is the binding's namespace (needed to resolve kind=Role).
func (t *topology) rulesForRoleRef(namespace string, ref rbacv1.RoleRef) ([]rbacv1.PolicyRule, bool) {
	switch ref.Kind {
	case "ClusterRole":
		cr, ok := t.clusterRolesByName[ref.Name]
		if !ok {
			return nil, false
		}
		return cr.Rules, true
	case "Role":
		if namespace == "" {
			return nil, false
		}
		r, ok := t.rolesByNSName[namespace+"/"+ref.Name]
		if !ok {
			return nil, false
		}
		return r.Rules, true
	default:
		return nil, false
	}
}

// navigatedGVRs is the ~50-GVR navigated set the index would be built
// for. Approximated from the production navigation surface (compositions
// / panels / navmenuitems / routes / pages / datagrids + the common
// krateo + core GVRs). The benchmark builds the full index across ALL of
// these per iteration (the proposed per-republish rebuild cost).
var navigatedGVRs = []gvrKey{
	{"composition.krateo.io", "compositions"},
	{"widgets.templates.krateo.io", "panels"},
	{"widgets.templates.krateo.io", "navmenus"},
	{"widgets.templates.krateo.io", "navmenuitems"},
	{"widgets.templates.krateo.io", "routes"},
	{"widgets.templates.krateo.io", "routesloaders"},
	{"widgets.templates.krateo.io", "pages"},
	{"widgets.templates.krateo.io", "datagrids"},
	{"widgets.templates.krateo.io", "tables"},
	{"widgets.templates.krateo.io", "rows"},
	{"widgets.templates.krateo.io", "columns"},
	{"widgets.templates.krateo.io", "buttons"},
	{"widgets.templates.krateo.io", "flowcharts"},
	{"widgets.templates.krateo.io", "linecharts"},
	{"widgets.templates.krateo.io", "barcharts"},
	{"widgets.templates.krateo.io", "piecharts"},
	{"widgets.templates.krateo.io", "markdowns"},
	{"widgets.templates.krateo.io", "yamlviewers"},
	{"widgets.templates.krateo.io", "paragraphs"},
	{"widgets.templates.krateo.io", "panelloaders"},
	{"templates.krateo.io", "restactions"},
	{"core.krateo.io", "compositiondefinitions"},
	{"core.krateo.io", "compositiondynamics"},
	{"", "configmaps"},
	{"", "secrets"},
	{"", "namespaces"},
	{"", "events"},
	{"", "pods"},
	{"", "services"},
	{"", "serviceaccounts"},
	{"apps", "deployments"},
	{"apps", "statefulsets"},
	{"apps", "replicasets"},
	{"apps", "daemonsets"},
	{"argoproj.io", "applications"},
	{"argoproj.io", "appprojects"},
	{"apiextensions.k8s.io", "customresourcedefinitions"},
	{"rbac.authorization.k8s.io", "roles"},
	{"rbac.authorization.k8s.io", "rolebindings"},
	{"rbac.authorization.k8s.io", "clusterroles"},
	{"rbac.authorization.k8s.io", "clusterrolebindings"},
	{"batch", "jobs"},
	{"batch", "cronjobs"},
	{"networking.k8s.io", "ingresses"},
	{"snapshot.storage.k8s.io", "volumesnapshots"},
	{"krateo.io", "tokens"},
	{"finops.krateo.io", "focusconfigs"},
	{"git.krateo.io", "repoes"},
	{"resourcetrees.krateo.io", "compositionreferences"},
	{"core.krateo.io", "snapshots"},
}

// proposedIndex is the reverse index the design proposes.
type proposedIndex struct {
	bindingsByGVR    map[gvrKey][]subjectRef
	wildcardBindings []subjectRef
}

// buildIndex builds the FULL BindingsByGVR + WildcardBindings index over
// the topology for the navigatedGVRs set — the per-republish rebuild
// whose CPU cost we measure. This is the function the benchmark times.
//
// Mechanism (mirrors the snapshot rebuild loop shape):
//   - For every binding (CRB + RB), resolve its roleRef rules ONCE.
//   - If the rules contain a */* get/list rule → enrol once in
//     wildcardBindings (the binding grants every navigated GVR).
//   - Else, for each navigated GVR, test roleRefGrantsGetList and enrol
//     under that GVR's bucket on a hit.
func (t *topology) buildIndex() *proposedIndex {
	idx := &proposedIndex{
		bindingsByGVR:    make(map[gvrKey][]subjectRef, len(navigatedGVRs)),
		wildcardBindings: make([]subjectRef, 0, 256),
	}

	enrol := func(rules []rbacv1.PolicyRule, ref subjectRef) {
		if rulesAreWildcard(rules) {
			idx.wildcardBindings = append(idx.wildcardBindings, ref)
			return
		}
		for _, key := range navigatedGVRs {
			if roleRefGrantsGetList(rules, key) {
				idx.bindingsByGVR[key] = append(idx.bindingsByGVR[key], ref)
			}
		}
	}

	for _, crb := range t.clusterRoleBindings {
		rules, ok := t.rulesForRoleRef("", crb.RoleRef)
		if !ok {
			continue
		}
		enrol(rules, subjectRef{kind: "ClusterRoleBinding", name: crb.Name})
	}
	for _, rb := range t.roleBindings {
		rules, ok := t.rulesForRoleRef(rb.Namespace, rb.RoleRef)
		if !ok {
			continue
		}
		enrol(rules, subjectRef{kind: "RoleBinding", ns: rb.Namespace, name: rb.Name})
	}
	return idx
}

// ─────────────────────────────────────────────────────────────────────
// Benchmark + the headline CPU% derivation.
// ─────────────────────────────────────────────────────────────────────

// republishRatePerSec is the OBSERVED rbac-snapshot republish rate the
// proposed index would be rebuilt at (project task: ~4.6/s, from
// internal/cache/rbac_snapshot.go's dirty-coalesced rebuild under the
// live controller-install churn).
const republishRatePerSec = 4.6

func BenchmarkBindingsByGVRBuild(b *testing.B) {
	top := loadTopology(b)

	// Sanity: confirm the index is non-trivial (catches a matcher that
	// silently enrols nothing — which would make the CPU number a lie).
	probe := top.buildIndex()
	b.Logf("index built: GVR-buckets=%d wildcard-bindings=%d",
		len(probe.bindingsByGVR), len(probe.wildcardBindings))
	var totalEnrolled int
	for _, v := range probe.bindingsByGVR {
		totalEnrolled += len(v)
	}
	b.Logf("total per-GVR enrolments=%d", totalEnrolled)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = top.buildIndex()
	}
}

// TestBindingsByGVRGate runs the build a fixed number of times, computes
// the per-build wall+CPU time, and prints the headline steady-state CPU%
// at the republish rate — the Gate-2 verdict. Single-threaded build, so
// wall ≈ CPU for this CPU-bound loop. Run with -v.
func TestBindingsByGVRGate(t *testing.T) {
	top := loadTopology(t)

	const iters = 20
	// Warm one build (allocations, map growth) so the timed loop is
	// steady-state.
	_ = top.buildIndex()

	start := time.Now()
	var lastBuckets, lastWild, lastEnrol int
	for i := 0; i < iters; i++ {
		idx := top.buildIndex()
		lastBuckets = len(idx.bindingsByGVR)
		lastWild = len(idx.wildcardBindings)
		lastEnrol = 0
		for _, v := range idx.bindingsByGVR {
			lastEnrol += len(v)
		}
	}
	elapsed := time.Since(start)
	perBuild := elapsed / iters
	// Steady-state CPU% = per-build seconds × rebuilds-per-second × 100.
	cpuFraction := perBuild.Seconds() * republishRatePerSec
	cpuPct := cpuFraction * 100

	t.Logf("Gate-2 result:")
	t.Logf("  bindings: CRB=%d RB=%d (total=%d)",
		len(top.clusterRoleBindings), len(top.roleBindings),
		len(top.clusterRoleBindings)+len(top.roleBindings))
	t.Logf("  navigated GVRs: %d", len(navigatedGVRs))
	t.Logf("  index: GVR-buckets=%d wildcard-bindings=%d per-GVR-enrolments=%d",
		lastBuckets, lastWild, lastEnrol)
	t.Logf("  per-full-index-build: %v", perBuild)
	t.Logf("  republish rate: %.1f/s", republishRatePerSec)
	t.Logf("  steady-state CPU added if rebuilt per republish: %.4f%%", cpuPct)
	if cpuPct < 1.0 {
		t.Logf("  VERDICT: PASS (<1%%) — per-republish rebuild is safe")
	} else {
		t.Logf("  VERDICT: FAIL (>=1%%) — must be incremental (only-changed-bindings) + lazy reseed")
	}
}
