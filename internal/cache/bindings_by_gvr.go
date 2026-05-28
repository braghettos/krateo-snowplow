// bindings_by_gvr.go — Ship 1 of the unified dynamic cohort-prewarm
// engine: the incremental BindingsByGVR reverse index.
//
// PURPOSE. The boot prewarm seed (phase1_pip_seed.go) must scope the
// cohort set PER NAVIGATED GVR — the ~3-6 subjects that can actually
// read each GVR — instead of the global GVR-agnostic powerset (~34
// cohorts dominated by control-plane SAs that never navigate). This
// index is that scoping substrate: for a navigated GVR it returns the
// subjects whose RBAC roleRef rules grant get/list on it (plus the
// cluster-admin `*/*` wildcard subjects).
//
// CRITICAL — LIST not KEY (project_resource_driven_cohort_design_2026_05_28).
// This index changes only WHICH SUBJECTS the seed enumerates per GVR. It
// does NOT change the cohort KEY: the per-cohort L1 cell stays keyed by
// `BindingSetHash` over the subject's FULL matched binding-set (NOT
// GVR-scoped) — a GVR-scoped key would collapse subjects whose g-verdict
// matches but whose ns-visibility differs (RBAC leak). EnumerateResourceCohorts
// returns Cohort values that route through the EXISTING BindingSetHash /
// sentinel / system:authenticated machinery unchanged.
//
// AUTHZ BOUNDARY UNTOUCHED. This index is SEED-TARGETING only. The
// per-request authz boundary remains EvaluateRBAC (evaluate.go:210) over
// the wholesale snapshot (rebuildRBACSnapshot / rebuildSubjectIndexes).
// List over-inclusion = wasted seed (benign); under-inclusion = per-user
// fallback resolve (benign). Eventual consistency is fine.
//
// INCREMENTAL MAINTENANCE (Gate-2, gate2bench). The wholesale rebuild of
// this index per ~4.6/s snapshot republish was measured at ~265 ms/build
// → ~122% CPU and is REJECTED. The index is therefore:
//   - built ONCE after WaitAllInformersSynced over RegisteredGVRs()
//     (BuildBindingsByGVRIndex);
//   - maintained INCREMENTALLY via delta hooks on the SAME RBAC informer
//     events that drive scheduleRBACRebuild (OnBindingAdd / OnBindingUpdate
//     (old,new) / OnBindingDelete) — each event resolves ONE binding's
//     roleRef rules + (un)enrols its subjectKeys into the per-GVR buckets.
//   The Gate-2 incremental bench measured the per-binding-event delta at
//   ~5.8 µs → 0.0027% CPU at 4.6/s (set-backed buckets make the DELETE
//   side O(navigatedGVRs), not O(bucket-size)).
//
// SET-BACKED BUCKETS (Gate-2 lesson). Buckets are map[subjectKey]struct{}
// — NOT slices. A slice bucket made an admin-wildcard binding delete an
// O(390K) linear scan (774 µs/event in the first cut). Set buckets make
// the delete O(1) per touched bucket.
//
// CONCURRENCY. The index is guarded by its own RWMutex (bindingsIndexMu)
// — it is mutated by the RBAC informer processor goroutine(s) (via the
// delta hooks, which run on the same processor goroutine as
// scheduleRBACRebuild — handler bodies must stay non-blocking, and the
// delta is a few map ops) and read by EnumerateResourceCohorts on the
// boot/engine path. We take the write lock for deltas and the read lock
// for enumeration. The matcher (roleRefGrantsGetList) reads the published
// immutable snapshot's roles, so role-rule lookups are lock-free against
// the snapshot.
//
// FEEDBACK_NO_SPECIAL_CASES. No GVR / resource / user literals. The
// navigated-GVR set is supplied by the caller (RegisteredGVRs() from the
// walk); subject matching flows from the rule wildcards; the wildcard
// bucket is the generic `*/*` rule, not a hardcoded cluster-admin name.
//
// FEEDBACK_CHECK_K8S_CLIENTGO_PRIOR_ART. client-go has no per-GVR
// subject reverse index — the authorizer evaluates one request at a
// time. This is a snowplow seed-targeting concern.

package cache

import (
	"sort"
	"sync"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// groupResource is the {apiGroup, resource} index key — a navigated GVR
// reduced to the pair RBAC rules match on (RBAC rules carry no version).
type groupResource struct {
	Group    string
	Resource string
}

// grFromGVR reduces a GVR to its {group, resource} index key.
func grFromGVR(gvr schema.GroupVersionResource) groupResource {
	return groupResource{Group: gvr.Group, Resource: gvr.Resource}
}

// subjectKey is one RBAC subject, canonicalised into the form the cohort
// machinery consumes. Kind is the rbac/v1 subject kind ("User", "Group",
// "ServiceAccount"); Name is the subject name; Namespace is set only for
// ServiceAccount subjects (the SA's namespace). subjectKey is comparable
// so it is a valid map key for the set-backed buckets.
type subjectKey struct {
	Kind      string
	Name      string
	Namespace string
}

// bindingID is the stable per-binding identity the buckets are keyed on
// for incremental delete. A binding's subjects all share its bindingID;
// the bucket value is the set of bindingIDs whose roleRef grants the GVR,
// each carrying the binding's subjects so EnumerateResourceCohorts can
// project them. Identity is the binding's metadata.uid when present, else
// the kind+ns+name content tuple (stable across relist) — same shape as
// crbIdentity/rbIdentity in rbac_cohort_gen.go.
type bindingID string

// bindingEntry is the per-binding value stored in a GVR bucket: the
// binding's identity + the subjects it grants. The subjects are copied by
// value (small Subject list) so the bucket is self-contained and does not
// pin the snapshot's typed objects.
type bindingEntry struct {
	id       bindingID
	subjects []subjectKey
}

// bindingsByGVRIndex is the incremental reverse index.
//
//   - byGVR[gr] = set of bindingEntries whose roleRef grants get/list on
//     {group,resource} gr (exact-match, non-resourceNames-scoped).
//   - wildcard  = set of bindingEntries whose roleRef carries a `*/*`
//     get/list rule (cluster-admin). NOT expanded across every GVR — the
//     per-GVR cohort = byGVR[gr] ∪ wildcard.
//   - byRole[roleKey] = set of bindingIDs referencing that role — used by
//     the role-rule-change re-route path to find every binding to
//     re-resolve when a ClusterRole/Role's rules change.
//   - entries[id] = the bindingEntry for an id — lets the delta hooks
//     unrol a binding without re-deriving its subjects, and the role
//     re-route look up an id's entry.
type bindingsByGVRIndex struct {
	mu       sync.RWMutex
	byGVR    map[groupResource]map[bindingID]struct{}
	wildcard map[bindingID]struct{}
	byRole   map[string]map[bindingID]struct{}
	entries  map[bindingID]bindingEntry

	// navigated is the set of GVRs the index buckets are maintained for.
	// Set at BuildBindingsByGVRIndex time; the delta hooks enrol only into
	// these buckets. A GVR discovered later (post-build CRD) widens this
	// via AddNavigatedGVR.
	navigated map[groupResource]struct{}

	built bool
}

// bindingsIndexSingleton is the process-wide index. Lazily constructed.
var (
	bindingsIndexInstance *bindingsByGVRIndex
	bindingsIndexOnce      sync.Once
)

func bindingsByGVRSingleton() *bindingsByGVRIndex {
	bindingsIndexOnce.Do(func() {
		bindingsIndexInstance = &bindingsByGVRIndex{
			byGVR:     map[groupResource]map[bindingID]struct{}{},
			wildcard:  map[bindingID]struct{}{},
			byRole:    map[string]map[bindingID]struct{}{},
			entries:   map[bindingID]bindingEntry{},
			navigated: map[groupResource]struct{}{},
		}
	})
	return bindingsIndexInstance
}

// roleRefKey renders the byRole map key for a binding's roleRef in the
// binding's namespace ("" for a CRB). Mirrors roleRefPermits' resolution
// domain: ClusterRole is cluster-scoped; Role is ns-scoped.
func roleRefKey(namespace string, ref rbacv1.RoleRef) string {
	switch ref.Kind {
	case "ClusterRole":
		return "C/" + ref.Name
	case "Role":
		return "R/" + namespace + "/" + ref.Name
	default:
		return ""
	}
}

// crbBindingID / rbBindingID compute the stable binding identity — reuse
// the same UID-or-content-tuple identity the cohort hash uses so the two
// never disagree on which logical binding an id refers to.
func crbBindingID(p *rbacv1.ClusterRoleBinding) bindingID {
	return bindingID(crbIdentity(p))
}

func rbBindingID(p *rbacv1.RoleBinding) bindingID {
	return bindingID(rbIdentity(p))
}

// subjectsFromRBAC projects a binding's rbac/v1 Subjects into subjectKeys.
// Only User / Group / ServiceAccount kinds are projected (the cohort
// machinery consumes User+Groups; SA subjects are carried so a future SA
// cohort path can use them — collectCohortBindingIDs already ignores SA).
func subjectsFromRBAC(subjects []rbacv1.Subject) []subjectKey {
	if len(subjects) == 0 {
		return nil
	}
	out := make([]subjectKey, 0, len(subjects))
	for _, s := range subjects {
		switch s.Kind {
		case rbacv1.UserKind, rbacv1.GroupKind:
			out = append(out, subjectKey{Kind: s.Kind, Name: s.Name})
		case rbacv1.ServiceAccountKind:
			out = append(out, subjectKey{Kind: s.Kind, Name: s.Name, Namespace: s.Namespace})
		}
	}
	return out
}

// The RBAC wildcard matcher rbacStringSliceMatches is defined in
// cohort_ns_acl.go (a package-local copy of rbac/evaluate.go's
// stringSliceMatches — cache/ cannot import rbac/ without a cycle). Reused
// here so the index enrolment predicate stays bit-faithful to the live
// evaluator.

// rulesGrantGetList reports whether any rule in `rules` grants get OR list
// on the given {group,resource} — mirroring rulesPermit's wildcard
// semantics (evaluate.go:446-463). resourceNames-scoped rules never grant
// the collection verb `list` and we have no object name for `get`, so they
// do NOT enrol a binding for nav LIST (faithful to resourceNameMatches for
// the collection verb). The `*/*` case is handled by rulesGrantWildcard.
func rulesGrantGetList(rules []rbacv1.PolicyRule, gr groupResource) bool {
	for _, rule := range rules {
		if len(rule.ResourceNames) > 0 {
			continue
		}
		if !rbacStringSliceMatches(rule.Verbs, "get") && !rbacStringSliceMatches(rule.Verbs, "list") {
			continue
		}
		if !rbacStringSliceMatches(rule.APIGroups, gr.Group) {
			continue
		}
		if !rbacStringSliceMatches(rule.Resources, gr.Resource) {
			continue
		}
		return true
	}
	return false
}

// rulesGrantWildcard reports whether the rule set contains a `*/*`
// get/list rule (independent of any specific GVR) — populates the
// wildcard bucket ONCE per binding instead of per GVR.
func rulesGrantWildcard(rules []rbacv1.PolicyRule) bool {
	for _, rule := range rules {
		if len(rule.ResourceNames) > 0 {
			continue
		}
		if (rbacStringSliceMatches(rule.Verbs, "get") || rbacStringSliceMatches(rule.Verbs, "list")) &&
			sliceHasStar(rule.APIGroups) && sliceHasStar(rule.Resources) {
			return true
		}
	}
	return false
}

func sliceHasStar(s []string) bool {
	for _, v := range s {
		if v == "*" {
			return true
		}
	}
	return false
}

// rulesForRoleRef resolves a binding's roleRef to its rule set against the
// published snapshot — mirrors roleRefPermits (evaluate.go:407-434). The
// snapshot is immutable post-publish so this read is lock-free. namespace
// is "" for a CRB; for a RoleBinding it is the binding's namespace.
func rulesForRoleRef(snap *RBACSnapshot, namespace string, ref rbacv1.RoleRef) ([]rbacv1.PolicyRule, bool) {
	if snap == nil {
		return nil, false
	}
	switch ref.Kind {
	case "ClusterRole":
		cr, ok := snap.ClusterRolesByName[ref.Name]
		if !ok {
			return nil, false
		}
		return cr.Rules, true
	case "Role":
		if namespace == "" {
			return nil, false
		}
		r, ok := snap.RolesByNSName[namespace+"/"+ref.Name]
		if !ok {
			return nil, false
		}
		return r.Rules, true
	default:
		return nil, false
	}
}

// ─────────────────────────────────────────────────────────────────────
// Delta primitives (WRITE-locked). enrol/unrol take a fully-resolved
// bindingEntry + rules so the hooks resolve the snapshot ONCE per event.
// ─────────────────────────────────────────────────────────────────────

// enrolLocked enrols a binding into the per-GVR buckets / wildcard bucket
// + the byRole map. Caller holds idx.mu (write). rules is the binding's
// resolved roleRef rules; rk is its roleRefKey ("" if unresolvable).
func (idx *bindingsByGVRIndex) enrolLocked(entry bindingEntry, rules []rbacv1.PolicyRule, rk string) {
	idx.entries[entry.id] = entry
	if rk != "" {
		set := idx.byRole[rk]
		if set == nil {
			set = map[bindingID]struct{}{}
			idx.byRole[rk] = set
		}
		set[entry.id] = struct{}{}
	}
	if rulesGrantWildcard(rules) {
		idx.wildcard[entry.id] = struct{}{}
		return
	}
	for gr := range idx.navigated {
		if rulesGrantGetList(rules, gr) {
			set := idx.byGVR[gr]
			if set == nil {
				set = map[bindingID]struct{}{}
				idx.byGVR[gr] = set
			}
			set[entry.id] = struct{}{}
		}
	}
}

// unrolLocked removes a binding id from every bucket it could be in. Caller
// holds idx.mu (write). With set-backed buckets each delete is O(1); we
// touch at most |navigated| buckets + wildcard + byRole. rk is the
// binding's roleRefKey (for the targeted byRole delete); when "" we skip
// the byRole delete (the binding never enrolled there).
func (idx *bindingsByGVRIndex) unrolLocked(id bindingID, rk string) {
	delete(idx.wildcard, id)
	for _, set := range idx.byGVR {
		delete(set, id)
	}
	if rk != "" {
		if set := idx.byRole[rk]; set != nil {
			delete(set, id)
		}
	}
	delete(idx.entries, id)
}

// ─────────────────────────────────────────────────────────────────────
// Build (ONCE). BuildBindingsByGVRIndex builds the full index from the
// published snapshot over the given navigated GVR set. Called after
// WaitAllInformersSynced over RegisteredGVRs(). Idempotent: a rebuild
// resets and re-enrols (used only at boot / explicit re-build; the
// steady-state path is the delta hooks).
// ─────────────────────────────────────────────────────────────────────

// BuildBindingsByGVRIndex builds the index ONCE over navigatedGVRs against
// the current published RBAC snapshot. Safe to call with a nil/empty
// navigated set (the index stays empty until AddNavigatedGVR widens it).
// Returns the number of bindings enrolled (CRB + RB resolved).
func BuildBindingsByGVRIndex(navigatedGVRs []schema.GroupVersionResource) int {
	idx := bindingsByGVRSingleton()
	snap := rbacSnap.Load()

	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Reset to a clean state (idempotent rebuild).
	idx.byGVR = map[groupResource]map[bindingID]struct{}{}
	idx.wildcard = map[bindingID]struct{}{}
	idx.byRole = map[string]map[bindingID]struct{}{}
	idx.entries = map[bindingID]bindingEntry{}
	idx.navigated = map[groupResource]struct{}{}
	for _, gvr := range navigatedGVRs {
		idx.navigated[grFromGVR(gvr)] = struct{}{}
	}

	if snap == nil {
		idx.built = true
		return 0
	}

	enrolled := 0
	for _, crb := range snap.ClusterRoleBindings {
		if crb == nil {
			continue
		}
		rules, ok := rulesForRoleRef(snap, "", crb.RoleRef)
		if !ok {
			continue
		}
		entry := bindingEntry{id: crbBindingID(crb), subjects: subjectsFromRBAC(crb.Subjects)}
		idx.enrolLocked(entry, rules, roleRefKey("", crb.RoleRef))
		enrolled++
	}
	for ns, rbs := range snap.RoleBindingsByNS {
		for _, rb := range rbs {
			if rb == nil {
				continue
			}
			rules, ok := rulesForRoleRef(snap, ns, rb.RoleRef)
			if !ok {
				continue
			}
			entry := bindingEntry{id: rbBindingID(rb), subjects: subjectsFromRBAC(rb.Subjects)}
			idx.enrolLocked(entry, rules, roleRefKey(ns, rb.RoleRef))
			enrolled++
		}
	}
	idx.built = true
	return enrolled
}

// BindingsByGVRIndexBuilt reports whether the index has been built at
// least once. The engine's cohort source falls back to the global
// enumeration when this is false (pre-build / cache-off).
func BindingsByGVRIndexBuilt() bool {
	idx := bindingsByGVRSingleton()
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.built
}

// AddNavigatedGVR widens the index's navigated set by one GVR and enrols
// every already-known binding that grants get/list on it (a post-build
// CRD discovery widens the navigated surface). Cheap: walks idx.entries
// once, resolving each binding's rules against the snapshot. Idempotent —
// a GVR already in navigated is a no-op.
func AddNavigatedGVR(gvr schema.GroupVersionResource) {
	idx := bindingsByGVRSingleton()
	gr := grFromGVR(gvr)

	idx.mu.Lock()
	defer idx.mu.Unlock()
	if _, ok := idx.navigated[gr]; ok {
		return
	}
	idx.navigated[gr] = struct{}{}

	snap := rbacSnap.Load()
	if snap == nil {
		return
	}
	// Enrol every known non-wildcard binding that grants this GVR. Wildcard
	// bindings already cover every GVR (the per-GVR cohort unions wildcard),
	// so they need no per-GVR enrolment.
	for id, entry := range idx.entries {
		if _, isWild := idx.wildcard[id]; isWild {
			continue
		}
		// Re-resolve the binding's rules. We need the binding's roleRef;
		// it is not stored on the entry, so we re-derive via the byRole
		// reverse map — find the role this id is under and resolve it.
		rules, ok := idx.rulesForEntryLocked(snap, id)
		if !ok {
			continue
		}
		if rulesGrantGetList(rules, gr) {
			set := idx.byGVR[gr]
			if set == nil {
				set = map[bindingID]struct{}{}
				idx.byGVR[gr] = set
			}
			set[entry.id] = struct{}{}
		}
	}
}

// rulesForEntryLocked resolves the rules for a binding id by finding which
// role it is enrolled under in byRole and resolving that role against the
// snapshot. Caller holds idx.mu. Returns (nil,false) if the id is not in
// any byRole bucket (a binding whose roleRef was unresolvable at enrol).
func (idx *bindingsByGVRIndex) rulesForEntryLocked(snap *RBACSnapshot, id bindingID) ([]rbacv1.PolicyRule, bool) {
	for rk, set := range idx.byRole {
		if _, ok := set[id]; !ok {
			continue
		}
		ns, ref := parseRoleRefKey(rk)
		return rulesForRoleRef(snap, ns, ref)
	}
	return nil, false
}

// parseRoleRefKey is the inverse of roleRefKey: "C/<name>" → (ns="",
// ClusterRole ref); "R/<ns>/<name>" → (ns, Role ref). Used by
// rulesForEntryLocked to re-resolve a known binding's rules.
func parseRoleRefKey(rk string) (string, rbacv1.RoleRef) {
	if len(rk) > 2 && rk[:2] == "C/" {
		return "", rbacv1.RoleRef{Kind: "ClusterRole", Name: rk[2:]}
	}
	if len(rk) > 2 && rk[:2] == "R/" {
		rest := rk[2:]
		// rest = "<ns>/<name>" — split on the first "/".
		for i := 0; i < len(rest); i++ {
			if rest[i] == '/' {
				return rest[:i], rbacv1.RoleRef{Kind: "Role", Name: rest[i+1:]}
			}
		}
	}
	return "", rbacv1.RoleRef{}
}

// ─────────────────────────────────────────────────────────────────────
// EnumerateResourceCohorts — the seed's cohort source per GVR.
// ─────────────────────────────────────────────────────────────────────

// EnumerateResourceCohorts returns the Cohort set whose RBAC binding-sets
// grant get/list on gvr (the per-GVR bucket ∪ the cluster-admin wildcard
// bucket). Each User subject → a User cohort; each Group subject → a
// group-only cohort (Username=groupOnlyCohortSentinel, Groups=[group]) —
// matching what EnumerateBindingSetClasses emits so the seed-time and
// request-time BindingSetHash converge.
//
// The cohort LIST is GVR-scoped; the cohort KEY (BindingSetHash) is NOT —
// it stays over the subject's FULL matched binding-set (computed by
// withCohortSeedContext → BindingSetHash at seed time and by the
// dispatcher at request time). This function only narrows the subjects to
// seed; correctness flows through the unchanged hash + the per-request
// EvaluateRBAC gate.
//
// Returns nil when the index is not built (the caller falls back to the
// global EnumerateBindingSetClasses). Deterministic order (sorted by
// Username then Groups[0]) for stable seed ordering across restarts.
func EnumerateResourceCohorts(gvr schema.GroupVersionResource) []Cohort {
	idx := bindingsByGVRSingleton()
	gr := grFromGVR(gvr)

	idx.mu.RLock()
	if !idx.built {
		idx.mu.RUnlock()
		return nil
	}
	// Collect the matching binding ids: per-GVR bucket ∪ wildcard.
	ids := make(map[bindingID]struct{}, 16)
	if set := idx.byGVR[gr]; set != nil {
		for id := range set {
			ids[id] = struct{}{}
		}
	}
	for id := range idx.wildcard {
		ids[id] = struct{}{}
	}
	// Project the matched bindings' subjects into User / Group cohort sets.
	// Dedupe by the canonical username so two bindings naming the same user
	// yield ONE cohort (the hash will union their binding-sets anyway).
	userSubjects := map[string]struct{}{}
	groupSubjects := map[string]struct{}{}
	for id := range ids {
		entry, ok := idx.entries[id]
		if !ok {
			continue
		}
		for _, s := range entry.subjects {
			switch s.Kind {
			case rbacv1.UserKind:
				if s.Name != "" {
					userSubjects[s.Name] = struct{}{}
				}
			case rbacv1.GroupKind:
				// Prune system:authenticated — it is the implicit group
				// every authenticated request carries (evaluate.go:559);
				// BindingSetHash injects it. Seeding a cohort whose ONLY
				// distinguishing group is system:authenticated would seed
				// the universal authenticated cell, which is the
				// group-only sentinel cohort emitted below per real group.
				if s.Name != "" && s.Name != systemAuthenticatedGroup {
					groupSubjects[s.Name] = struct{}{}
				}
			}
		}
	}
	idx.mu.RUnlock()

	var out []Cohort
	for u := range userSubjects {
		out = append(out, Cohort{Username: u})
	}
	for g := range groupSubjects {
		// Group-only cohort: sentinel username + the single group — the
		// SAME shape EnumerateBindingSetClasses emits (binding_set_enumeration.go),
		// so BindingSetHash(sentinel,[g]) at seed time == the dispatcher's
		// hash for a real group-only user holding g.
		out = append(out, Cohort{Username: groupOnlyCohortSentinel, Groups: []string{g}})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Username != out[j].Username {
			return out[i].Username < out[j].Username
		}
		gi, gj := "", ""
		if len(out[i].Groups) > 0 {
			gi = out[i].Groups[0]
		}
		if len(out[j].Groups) > 0 {
			gj = out[j].Groups[0]
		}
		return gi < gj
	})
	return out
}

// ResetBindingsByGVRIndexForTest clears the singleton's state. Production
// MUST NOT call this. Tests (including cross-package dispatcher tests) use
// it to observe a clean build.
func ResetBindingsByGVRIndexForTest() {
	idx := bindingsByGVRSingleton()
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.byGVR = map[groupResource]map[bindingID]struct{}{}
	idx.wildcard = map[bindingID]struct{}{}
	idx.byRole = map[string]map[bindingID]struct{}{}
	idx.entries = map[bindingID]bindingEntry{}
	idx.navigated = map[groupResource]struct{}{}
	idx.built = false
}
