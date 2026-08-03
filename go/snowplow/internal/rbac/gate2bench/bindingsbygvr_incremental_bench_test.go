// bindingsbygvr_incremental_bench_test.go — Gate 2 STEP-0 INCREMENTAL
// delta CPU probe for the proposed BindingsByGVR reverse-index.
//
// SANDBOX — same own-package (gate2bench) as bindingsbygvr_bench_test.go;
// NO TestMain, never touches internal/rbac's destructive TestMain. Reads
// the READ-ONLY live RBAC topology dump pointed at by GATE2_RBAC_DUMP
// (produced by `kubectl get clusterrolebindings,clusterroles,
// rolebindings,roles -A -o json`). Reuses the topology loader + the
// matcher (roleRefGrantsGetList / rulesAreWildcard) from the wholesale
// bench file in this package — those are bit-faithful to
// internal/rbac/evaluate.go:446-526.
//
// WHY THIS FILE — the wholesale per-republish rebuild was already
// measured (BenchmarkBindingsByGVRBuild / TestBindingsByGVRGate) at
// ~119.97% CPU @ 4.6/s; that path is REJECTED. Ship 1's index is built
// ONCE then maintained INCREMENTALLY via delta hooks on the
// rbacSnapshotEventHandlers (AddFunc/UpdateFunc/DeleteFunc). This file
// measures the INCREMENTAL delta cost — the only cost the live process
// actually pays once Ship 1 lands:
//
//   1. PER-BINDING-EVENT delta — the bulk of the ~4.6/s churn is binding
//      add/remove/update (composition-install RBAC growth,
//      project_composition_install_rbac_scale). For ONE binding:
//      resolve its roleRef rules once + (un)enrol its subjects into the
//      per-GVR buckets. An UPDATE is a DELETE-of-old + ADD-of-new (the
//      informer hands old,new). We measure the ADD+DELETE pair (the
//      UPDATE worst case) per event.
//
//   2. WORST-CASE role-rule change — a ClusterRole/Role rule edit
//      invalidates EVERY binding whose roleRef points at that role. The
//      maintenance must re-route all of them. We build a reverse
//      role->bindings map, find the MOST-referenced role, and re-route
//      every binding referencing it. This is the rare-but-expensive
//      event; we measure its one-shot cost and fold it in at a
//      realistic LOW rate.
//
// HEADLINE — steady-state CPU% = perBindingEvent×bindingRate +
// roleReroute×roleRate, ×100. PASS = <1%. If the role-reroute worst
// case pushes the total >=1%, STOP and report so the design adds
// debounce/coalescing BEFORE the index is built.
//
// Run locally:
//
//	GATE2_RBAC_DUMP=/tmp/gate2-rbac/rbac_all.json \
//	  go test ./internal/rbac/gate2bench/ -run Incremental -v

package gate2bench

import (
	"math/rand"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
)

// ─────────────────────────────────────────────────────────────────────
// Event-rate model. Binding churn dominates the observed ~4.6/s snapshot
// republish rate; role-rule edits are RARE (operators rarely edit a
// shared ClusterRole's verbs at runtime — RBAC role definitions are
// install-time artifacts). We attribute the bulk of 4.6/s to binding
// events and a small residual to role-rule edits.
// ─────────────────────────────────────────────────────────────────────

const (
	// bindingEventRatePerSec — the per-binding add/update/delete rate.
	// Conservative: attribute the WHOLE observed 4.6/s republish rate to
	// binding events (the actual role-rule edit rate is much lower, so
	// this over-counts the cheap path — a conservative PASS bias).
	bindingEventRatePerSec = 4.6

	// roleRuleEditRatePerSec — the per-role rule-change rate. Roles are
	// install-time artifacts; a runtime rule edit is rare. We model it at
	// 0.1/s (one role-rule edit every 10s) which is already generous for
	// a steady-state cluster, then ALSO report the worst case folded in
	// at the full 4.6/s as a stress sanity bound.
	roleRuleEditRatePerSec = 0.1
)

// ─────────────────────────────────────────────────────────────────────
// Incremental index — the same shape as proposedIndex (reuses gvrKey,
// subjectRef, navigatedGVRs, the matcher) but with the maintenance
// operations the live delta hooks would perform: enrolBinding (ADD),
// unrolBinding (DELETE), and a reverse role->bindings map for the
// role-reroute path.
//
// bindingRef is a stable per-binding key the buckets store + the unrol
// path removes by. It mirrors subjectRef (kind/ns/name) which is unique
// per binding.
// ─────────────────────────────────────────────────────────────────────

// incIndex is the incrementally-maintained reverse index. Buckets are
// SETS (map[subjectRef]struct{}) — NOT slices — so the DELETE-side delta
// (unrolBinding) is O(1) per touched bucket rather than O(bucket-size).
// This mirrors the data structure Ship 1's real index uses: a slice
// bucket would make a single admin-wildcard binding delete an O(390K)
// linear scan, which is the cost the slice-based first cut surfaced. The
// live AddFunc/UpdateFunc/DeleteFunc hooks must be O(navigatedGVRs), not
// O(total-enrolments), or the delete path itself becomes the regression.
//
// The reverse role->bindings map is also set-backed for the same reason.
type incIndex struct {
	bindingsByGVR    map[gvrKey]map[subjectRef]struct{}
	wildcardBindings map[subjectRef]struct{}

	// roleRefKey -> set of bindings referencing it. Used by the
	// role-rule-change path to find every binding to re-route. Key is
	// "C/<name>" for a ClusterRole, "R/<ns>/<name>" for a namespaced
	// Role (matches rulesForRoleRef's resolution domain).
	bindingsByRole map[string]map[subjectRef]struct{}
}

// roleRefKeyFor renders the reverse-map key for a binding's roleRef in
// the binding's namespace ("" for a CRB). Mirrors rulesForRoleRef's
// resolution: ClusterRole is cluster-scoped; Role is ns-scoped.
func roleRefKeyFor(namespace string, ref rbacv1.RoleRef) string {
	switch ref.Kind {
	case "ClusterRole":
		return "C/" + ref.Name
	case "Role":
		return "R/" + namespace + "/" + ref.Name
	default:
		return ""
	}
}

// enrolBinding performs the ADD-side delta for ONE binding: resolve its
// roleRef rules once, then enrol its subjectRef into every matching
// per-GVR bucket (or the wildcard bucket) + the reverse role map. This
// is exactly the work the live AddFunc hook performs for one event.
func (t *topology) enrolBinding(idx *incIndex, namespace string, ref rbacv1.RoleRef, sref subjectRef) {
	rules, ok := t.rulesForRoleRef(namespace, ref)
	if !ok {
		return
	}
	if rk := roleRefKeyFor(namespace, ref); rk != "" {
		set := idx.bindingsByRole[rk]
		if set == nil {
			set = map[subjectRef]struct{}{}
			idx.bindingsByRole[rk] = set
		}
		set[sref] = struct{}{}
	}
	if rulesAreWildcard(rules) {
		idx.wildcardBindings[sref] = struct{}{}
		return
	}
	for _, key := range navigatedGVRs {
		if roleRefGrantsGetList(rules, key) {
			set := idx.bindingsByGVR[key]
			if set == nil {
				set = map[subjectRef]struct{}{}
				idx.bindingsByGVR[key] = set
			}
			set[sref] = struct{}{}
		}
	}
}

// unrolBinding performs the DELETE-side delta for ONE binding: remove its
// subjectRef from every per-GVR bucket + the wildcard bucket + the
// reverse role map. This is the live DeleteFunc hook for one event. With
// SET-backed buckets the removal is O(1) per touched bucket; a binding
// touches at most |navigatedGVRs| buckets (and exactly one wildcard slot
// when wildcard) — so the whole delete is O(navigatedGVRs).
func (t *topology) unrolBinding(idx *incIndex, namespace string, ref rbacv1.RoleRef, sref subjectRef) {
	rules, ok := t.rulesForRoleRef(namespace, ref)
	if !ok {
		// roleRef gone — defensive: in the live hook the DeleteFunc still
		// has the old object so the roleRef is known; nothing to do here.
		return
	}
	if rk := roleRefKeyFor(namespace, ref); rk != "" {
		if set := idx.bindingsByRole[rk]; set != nil {
			delete(set, sref)
		}
	}
	if rulesAreWildcard(rules) {
		delete(idx.wildcardBindings, sref)
		return
	}
	for _, key := range navigatedGVRs {
		if roleRefGrantsGetList(rules, key) {
			if set := idx.bindingsByGVR[key]; set != nil {
				delete(set, sref)
			}
		}
	}
}

// buildIncIndex builds the full incremental index ONCE (the boot build —
// NOT the measured cost; the measured cost is the per-event delta over a
// warm index). Reuses enrolBinding so the boot build and the delta path
// share the exact same enrolment code.
func (t *topology) buildIncIndex() *incIndex {
	idx := &incIndex{
		bindingsByGVR:    make(map[gvrKey]map[subjectRef]struct{}, len(navigatedGVRs)),
		wildcardBindings: make(map[subjectRef]struct{}, 256),
		bindingsByRole:   make(map[string]map[subjectRef]struct{}, 1024),
	}
	for _, crb := range t.clusterRoleBindings {
		t.enrolBinding(idx, "", crb.RoleRef, subjectRef{kind: "ClusterRoleBinding", name: crb.Name})
	}
	for _, rb := range t.roleBindings {
		t.enrolBinding(idx, rb.Namespace, rb.RoleRef, subjectRef{kind: "RoleBinding", ns: rb.Namespace, name: rb.Name})
	}
	return idx
}

// ─────────────────────────────────────────────────────────────────────
// TestBindingsByGVRIncrementalGate — the STEP-0 verdict.
// ─────────────────────────────────────────────────────────────────────

func TestBindingsByGVRIncrementalGate(t *testing.T) {
	top := loadTopology(t)
	idx := top.buildIncIndex()

	// Sanity: the index must be non-trivial (a silent no-enrol matcher
	// would make the per-event number a lie).
	var enrolled int
	for _, v := range idx.bindingsByGVR {
		enrolled += len(v)
	}
	t.Logf("incremental index: GVR-buckets=%d wildcard=%d per-GVR-enrolments=%d roles-referenced=%d",
		len(idx.bindingsByGVR), len(idx.wildcardBindings), enrolled, len(idx.bindingsByRole))

	// ── (1) PER-BINDING-EVENT delta — UPDATE worst case = unrol-old +
	// enrol-new. We sample a fixed set of bindings deterministically (seed
	// fixed) and, for each, perform unrol+enrol on a COPY index so the
	// index stays consistent. We measure over many iterations to amortise
	// timer noise.
	type bindingSample struct {
		namespace string
		ref       rbacv1.RoleRef
		sref      subjectRef
	}
	var samples []bindingSample
	for _, crb := range top.clusterRoleBindings {
		samples = append(samples, bindingSample{"", crb.RoleRef, subjectRef{kind: "ClusterRoleBinding", name: crb.Name}})
	}
	for _, rb := range top.roleBindings {
		samples = append(samples, bindingSample{rb.Namespace, rb.RoleRef, subjectRef{kind: "RoleBinding", ns: rb.Namespace, name: rb.Name}})
	}
	if len(samples) == 0 {
		t.Fatal("no bindings in topology — cannot measure per-event delta")
	}

	rng := rand.New(rand.NewSource(42))
	const bindingIters = 5000
	// Warm: do a few un/enrol pairs so map growth + branch prediction are
	// steady.
	for i := 0; i < 50; i++ {
		s := samples[rng.Intn(len(samples))]
		top.unrolBinding(idx, s.namespace, s.ref, s.sref)
		top.enrolBinding(idx, s.namespace, s.ref, s.sref)
	}
	start := time.Now()
	for i := 0; i < bindingIters; i++ {
		s := samples[rng.Intn(len(samples))]
		// UPDATE = unrol(old) + enrol(new). For a pure ADD or DELETE the
		// cost is half this; modelling the UPDATE pair is the conservative
		// per-event worst case.
		top.unrolBinding(idx, s.namespace, s.ref, s.sref)
		top.enrolBinding(idx, s.namespace, s.ref, s.sref)
	}
	perBindingEvent := time.Since(start) / bindingIters

	// ── (2) WORST-CASE role-rule change — re-route EVERY binding
	// referencing the MOST-referenced role. A role-rule edit changes the
	// roleRef's rules, so every referencing binding's bucket membership
	// may change: the maintenance unrols every referencing binding under
	// the OLD rules then re-enrols under the NEW rules. We measure the
	// unrol+enrol of the full referencing set for the heaviest role.
	var heaviestRole string
	var heaviestCount int
	for rk, set := range idx.bindingsByRole {
		if len(set) > heaviestCount {
			heaviestCount = len(set)
			heaviestRole = rk
		}
	}
	// Build the (namespace, ref) for each referencing binding so the
	// re-route can call unrol/enrol with the correct resolution domain.
	// We reconstruct from the heaviest role's referencing bindings by
	// matching subjectRef back to the topology binding (the subjectRef is
	// unique per binding: kind+ns+name). For perf we precompute a lookup.
	refBySubject := map[subjectRef]bindingSample{}
	for _, s := range samples {
		refBySubject[s.sref] = s
	}
	reroute := func() {
		set := idx.bindingsByRole[heaviestRole]
		// Snapshot the set keys — unrol mutates the set in place, which
		// would corrupt the range.
		work := make([]subjectRef, 0, len(set))
		for sref := range set {
			work = append(work, sref)
		}
		for _, sref := range work {
			s := refBySubject[sref]
			top.unrolBinding(idx, s.namespace, s.ref, s.sref)
			top.enrolBinding(idx, s.namespace, s.ref, s.sref)
		}
	}
	// Warm one re-route, then measure a few (the heaviest role's set is
	// large so a handful of iterations is enough to amortise).
	reroute()
	const rerouteIters = 20
	startR := time.Now()
	for i := 0; i < rerouteIters; i++ {
		reroute()
	}
	perRoleReroute := time.Since(startR) / rerouteIters

	// ── Steady-state CPU% derivation.
	bindingCPU := perBindingEvent.Seconds() * bindingEventRatePerSec
	roleCPU := perRoleReroute.Seconds() * roleRuleEditRatePerSec
	totalCPU := (bindingCPU + roleCPU) * 100

	// Stress sanity: fold the worst-case role-reroute in at the FULL
	// 4.6/s rate (every republish were a heaviest-role rule edit — a
	// physically-impossible upper bound) to show the headroom.
	roleStressCPU := perRoleReroute.Seconds() * bindingEventRatePerSec
	stressTotalCPU := (bindingCPU + roleStressCPU) * 100

	t.Logf("Gate-2 INCREMENTAL result:")
	t.Logf("  bindings: CRB=%d RB=%d (total=%d)",
		len(top.clusterRoleBindings), len(top.roleBindings),
		len(top.clusterRoleBindings)+len(top.roleBindings))
	t.Logf("  navigated GVRs: %d", len(navigatedGVRs))
	t.Logf("  (1) per-binding-event delta (UPDATE = unrol+enrol): %v", perBindingEvent)
	t.Logf("      @ %.1f/s -> %.4f%% CPU", bindingEventRatePerSec, bindingCPU*100)
	t.Logf("  (2) worst-case role-reroute: role=%q referencing-bindings=%d", heaviestRole, heaviestCount)
	t.Logf("      per-reroute: %v", perRoleReroute)
	t.Logf("      @ %.2f/s (realistic role-edit rate) -> %.4f%% CPU", roleRuleEditRatePerSec, roleCPU*100)
	t.Logf("  STEADY-STATE total: %.4f%% CPU", totalCPU)
	t.Logf("  STRESS bound (role-reroute folded at FULL %.1f/s): %.4f%% CPU", bindingEventRatePerSec, stressTotalCPU)
	if totalCPU < 1.0 {
		t.Logf("  VERDICT: PASS (<1%%) — incremental delta maintenance is safe")
	} else {
		t.Logf("  VERDICT: FAIL (>=1%%) — design needs debounce/coalescing on the role-reroute path")
	}
}
