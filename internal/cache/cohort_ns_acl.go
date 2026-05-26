// cohort_ns_acl.go — Ship 0.30.178 (A.2): per-cohort namespace ACL.
//
// PURPOSE
//   The Ship GMC (0.30.174) per-cohort gate memo populates its keptNames
//   set by running per-item filterListByRBAC over every item of every
//   pivot-served LIST. At admin scale (10K-50K compositions) that fan-out
//   walks every binding for every namespace touched. The walk is bounded
//   by EvaluateRBAC's typed-indexer reads — no I/O — but the cost still
//   dominates the first-call cold path for the same-cohort burst pattern
//   (the memo populates ONCE per (entry × cohort), so the per-item walk
//   only fires once per cohort, but THAT first call pays the full N-item
//   eval).
//
//   CohortNSACL pre-computes the verdict at the COHORT level: it walks
//   the cohort's matched binding-set ONCE, derives the namespace-set the
//   cohort is permitted to LIST in for the given gvr, and returns either:
//
//     - permitAll=true   — at least one ClusterRoleBinding grants list on
//                          the gvr cluster-wide; every namespace is in.
//     - permitAll=false, permittedNS={ns₁, ns₂, …} — only RoleBindings;
//                          the cohort can LIST in the namespaces that
//                          host a binding granting list on the gvr.
//
//   The memo populate step then short-circuits: permitAll keeps every
//   item; otherwise it filters by item.GetNamespace() membership in
//   permittedNS — a single map lookup per item, no EvaluateRBAC fan-out.
//
// CORRECTNESS — SAME ALGORITHM AS rulesPermit
//   The verdict logic mirrors rbac/evaluate.go:rulesPermit, restricted to
//   verb="list":
//     - rule.Verbs matches "list" (literal or "*")
//     - rule.APIGroups matches gvr.Group (literal or "*")
//     - rule.Resources matches gvr.Resource (literal or "*")
//     - rule.ResourceNames is EMPTY (a resourceNames-scoped rule never
//       grants a collection verb such as list — rbac/evaluate.go:500-507)
//   No subject re-check at this layer — the caller already filtered
//   bindings by cohort membership via collectCohortBindingPtrs, which is
//   the same pointer-set the rest of the cohort code uses (correctness
//   barrier preserved).
//
// SAFETY (binding contracts)
//   - feedback_l1_per_user_keyed_never_cohort.md — this helper computes
//     a per-cohort verdict on namespace membership, NOT a resolved-output
//     cache key. The L1 RESOLVED cache stays per-user-keyed.
//   - feedback_no_special_cases.md — the helper is uniform over every
//     GVR + every cohort. No hardcoded admin/cluster-admin/system:masters
//     fast-paths.
//   - feedback_shared_vs_copy_is_a_concurrency_change.md — reads the
//     immutable post-publish RBACSnapshot. No mutation, no shared
//     mutable state.
//   - feedback_check_k8s_clientgo_prior_art.md — rbac/evaluate.go is the
//     prior art; this helper REUSES rulesPermit's logic verbatim,
//     restricted to verb="list".
//
// FAIL-CLOSED
//   - snap=nil → permitAll=false, empty permittedNS (caller falls
//     through to the per-item gate, which itself fails closed without
//     a snapshot).
//   - No matched bindings → permitAll=false, empty permittedNS (cohort
//     can list in zero namespaces; every item dropped).
//   - A binding's roleRef misses in the snapshot → that binding is
//     ignored (matches roleRefPermits' RecordRBACSnapshotMiss + deny
//     posture; the missing target self-heals on the next rebuild).

package cache

import (
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// CohortNSACL returns the cohort's LIST verdict on the given gvr,
// resolved at the cohort level rather than per-item.
//
// Returns:
//
//   - permitAll == true:  at least one ClusterRoleBinding the cohort
//     matches has a bound role with a rule granting `list` on the gvr
//     cluster-wide. Every namespace is permitted; `permittedNS` is nil.
//
//   - permitAll == false, len(permittedNS) > 0: no cluster-wide grant,
//     but at least one RoleBinding the cohort matches grants `list` on
//     the gvr in its own namespace. permittedNS is the set of those
//     namespaces — the cohort can list items whose metadata.namespace
//     is in the set.
//
//   - permitAll == false, len(permittedNS) == 0: the cohort cannot list
//     the gvr in any namespace. (Equivalent to "every item denied".)
//
// The caller (apistage_cohort_memo.go at memo MISS) consumes the tuple
// to short-circuit per-item EvaluateRBAC:
//
//	if permitAll:
//	    keep every item; cache encoded envelope bytes (zero-bound)
//	else:
//	    keep items whose item.GetNamespace() ∈ permittedNS
//
// snap is the live RBACSnapshot (rw.Snapshot() at the call site). A nil
// snap returns (false, nil) — caller falls back to the per-item gate,
// which fails closed without a snapshot.
//
// The function is read-only against the snapshot; safe for concurrent
// callers per the snapshot's immutability contract (rbac_snapshot.go:36-51).
func CohortNSACL(
	snap *RBACSnapshot,
	username string,
	groups []string,
	gvr schema.GroupVersionResource,
) (permitAll bool, permittedNS map[string]struct{}) {
	if snap == nil {
		return false, nil
	}

	// Step 1 — gather the cohort's matched cluster-wide CRBs. If any of
	// them grants list on the gvr cluster-wide, permitAll=true wins
	// outright (a cluster-wide list grant subsumes every per-namespace
	// landing).
	clusterBindings := collectCohortClusterBindings(snap, username, groups)
	for _, crb := range clusterBindings {
		if crb == nil {
			continue
		}
		if bindingGrantsListOnGVR(snap, crb.RoleRef, gvr) {
			return true, nil
		}
	}

	// Step 2 — no cluster-wide grant. Walk the cohort's matched per-NS
	// RoleBindings and add each binding's namespace to permittedNS when
	// the binding's role grants list on the gvr.
	//
	// We allocate permittedNS lazily so a no-RoleBinding cohort returns
	// a nil map (zero allocation) — the caller's nil-safe map lookup
	// (`_, ok := permittedNS[ns]`) returns ok=false for every ns, which
	// is the correct "deny every item" outcome.
	nsBindings := collectCohortNamespaceBindings(snap, username, groups)
	for ns, rbs := range nsBindings {
		for _, rb := range rbs {
			if rb == nil {
				continue
			}
			if bindingGrantsListInNS(snap, ns, rb.RoleRef, gvr) {
				if permittedNS == nil {
					permittedNS = make(map[string]struct{}, len(nsBindings))
				}
				permittedNS[ns] = struct{}{}
				// Once a namespace is permitted, additional bindings in
				// the same namespace add nothing — move on to the next
				// namespace. The break is a small constant-factor win;
				// correctness is unaffected if the loop continues.
				break
			}
		}
	}

	return false, permittedNS
}

// collectCohortClusterBindings returns the union of ClusterRoleBindings
// the (username, groups) cohort matches. The set is a pointer SUPERSET
// of the post-anySubjectMatches set — same correctness barrier as
// selectCRBCandidates in rbac/evaluate.go:302-349. Pointer-dedup.
//
// SA-kind / catch-all are NOT collected here — the GMC memo's caller is
// always a User+Groups identity (jwtutil.UserInfo on the resolver path,
// apistage_cohort_memo.go:127). collectCohortBindingPtrs (rbac_cohort_gen.go:205-268)
// makes the same choice. Future SA cohort support is additive (one more
// snap landing + one more matcher branch).
func collectCohortClusterBindings(
	snap *RBACSnapshot, username string, groups []string,
) []*rbacv1.ClusterRoleBinding {
	if snap == nil {
		return nil
	}

	seen := make(map[*rbacv1.ClusterRoleBinding]struct{})
	var out []*rbacv1.ClusterRoleBinding
	add := func(crbs []*rbacv1.ClusterRoleBinding) {
		for _, c := range crbs {
			if c == nil {
				continue
			}
			if _, ok := seen[c]; ok {
				continue
			}
			seen[c] = struct{}{}
			out = append(out, c)
		}
	}

	if username != "" {
		add(snap.CRBsByUser[username])
		// system:authenticated — implicit group for every authenticated
		// request (mirrors evaluate.go's anySubjectMatches branch).
		add(snap.CRBsByGroup["system:authenticated"])
	}
	for _, g := range groups {
		if g == "" {
			continue
		}
		add(snap.CRBsByGroup[g])
	}
	return out
}

// collectCohortNamespaceBindings returns the cohort's matched
// RoleBindings keyed by namespace. Same pointer-set semantics as
// collectCohortClusterBindings, scoped per-namespace.
//
// The inner slice for each namespace is the union of:
//
//	RBsByUserByNS[ns][username] ∪
//	RBsByGroupByNS[ns][g] for each g ∈ groups ∪
//	RBsByGroupByNS[ns]["system:authenticated"] (when username != "")
//
// Returns an empty map when no namespace has a matching binding (the
// caller's range-over-nil is zero iterations).
func collectCohortNamespaceBindings(
	snap *RBACSnapshot, username string, groups []string,
) map[string][]*rbacv1.RoleBinding {
	if snap == nil {
		return nil
	}

	out := make(map[string][]*rbacv1.RoleBinding)
	addNS := func(ns string, rbs []*rbacv1.RoleBinding) {
		if len(rbs) == 0 {
			return
		}
		// Pointer-dedup within the namespace's slice. A binding that
		// matches BOTH the cohort's user landing AND a group landing
		// appears in two index slices; dedup keeps the post-lookup
		// linear walk over a single canonical pointer per binding.
		existing := out[ns]
		seen := make(map[*rbacv1.RoleBinding]struct{}, len(existing))
		for _, rb := range existing {
			seen[rb] = struct{}{}
		}
		for _, rb := range rbs {
			if rb == nil {
				continue
			}
			if _, ok := seen[rb]; ok {
				continue
			}
			seen[rb] = struct{}{}
			existing = append(existing, rb)
		}
		out[ns] = existing
	}

	// User-kind RBs per namespace.
	if username != "" {
		for ns, inner := range snap.RBsByUserByNS {
			addNS(ns, inner[username])
		}
	}

	// Group-kind RBs per namespace — iterate every group's landing in
	// every namespace.
	for _, g := range groups {
		if g == "" {
			continue
		}
		for ns, inner := range snap.RBsByGroupByNS {
			addNS(ns, inner[g])
		}
	}

	// system:authenticated implicit group per namespace (only for
	// authenticated identities — mirrors evaluate.go anySubjectMatches).
	if username != "" {
		for ns, inner := range snap.RBsByGroupByNS {
			addNS(ns, inner["system:authenticated"])
		}
	}

	return out
}

// bindingGrantsListOnGVR reports whether the bound Role/ClusterRole has
// at least one PolicyRule granting `list` on the (gvr.Group, gvr.Resource)
// pair. namespace="" is used to resolve a Role roleRef inside a
// ClusterRoleBinding (which is invalid per Kubernetes — the helper
// returns false in that case, mirroring roleRefPermits).
//
// Logic mirrors rbac/evaluate.go:rulesPermit restricted to verb="list":
//   - rule.Verbs matches "list" (literal or "*")
//   - rule.APIGroups matches gvr.Group (literal or "*")
//   - rule.Resources matches gvr.Resource (literal or "*")
//   - rule.ResourceNames is EMPTY (resourceNames-scoped rules never
//     grant collection verbs — rbac/evaluate.go:500-515)
//
// A roleRef whose target is absent from the snapshot is treated as
// "no grant" — the snapshot rebuild lag self-heals; the caller's
// per-item gate is the eventual-consistency safety net.
func bindingGrantsListOnGVR(
	snap *RBACSnapshot, ref rbacv1.RoleRef, gvr schema.GroupVersionResource,
) bool {
	if snap == nil {
		return false
	}
	switch ref.Kind {
	case "ClusterRole":
		cr, ok := snap.ClusterRolesByName[ref.Name]
		if !ok {
			// Same fail-closed posture as roleRefPermits. We deliberately
			// do NOT call RecordRBACSnapshotMiss here — the metric counts
			// per-EvaluateRBAC misses, and a per-cohort ACL walk would
			// inflate the ratio without informing operators of an actual
			// hot-path miss. The caller's per-item gate is the
			// authoritative miss-counter.
			return false
		}
		return rulesGrantListOnGVR(cr.Rules, gvr)
	case "Role":
		// Role roleRefs in a ClusterRoleBinding are invalid per K8s — the
		// CRB caller passes ref from a CRB.RoleRef which is always
		// ClusterRole in well-formed clusters. RoleBindings can carry
		// kind=Role, but those are resolved by the caller passing the
		// RoleBinding's own namespace; we don't have it here. Conservative:
		// return false — the per-item gate covers any path the ACL
		// short-circuit misses.
		//
		// This is correctness-load-bearing only if a permit-loss bug
		// would manifest: but the per-item gate runs AFTER permitAll=false
		// path (filtered by permittedNS membership) AND for !permitAll the
		// caller falls through to the original filterListByRBAC anyway. So
		// returning false here at worst expands the per-item filter
		// candidate set — never produces a wrong permit.
		return false
	default:
		return false
	}
}

// bindingGrantsListInNS is the namespaced sibling of
// bindingGrantsListOnGVR: it resolves a RoleBinding's roleRef (which
// can be either Role-in-ns or ClusterRole-cluster-wide) against the
// snapshot and checks for a list grant on the gvr.
//
// Called by CohortNSACL's RoleBinding loop; the binding's own namespace
// is passed in so a kind=Role ref resolves correctly.
func bindingGrantsListInNS(
	snap *RBACSnapshot, namespace string, ref rbacv1.RoleRef, gvr schema.GroupVersionResource,
) bool {
	if snap == nil {
		return false
	}
	switch ref.Kind {
	case "ClusterRole":
		cr, ok := snap.ClusterRolesByName[ref.Name]
		if !ok {
			return false
		}
		return rulesGrantListOnGVR(cr.Rules, gvr)
	case "Role":
		if namespace == "" {
			return false
		}
		r, ok := snap.RolesByNSName[namespace+"/"+ref.Name]
		if !ok {
			return false
		}
		return rulesGrantListOnGVR(r.Rules, gvr)
	default:
		return false
	}
}

// rulesGrantListOnGVR walks rules looking for one that permits
// list on the given gvr. Mirrors rbac/evaluate.go:rulesPermit, fixed to
// verb="list" with a resourceNames-empty check (a resourceNames-scoped
// rule cannot grant a collection verb).
func rulesGrantListOnGVR(rules []rbacv1.PolicyRule, gvr schema.GroupVersionResource) bool {
	for _, rule := range rules {
		if len(rule.ResourceNames) != 0 {
			// resourceNames-scoped → never grants list (rbac/evaluate.go:500-507).
			continue
		}
		if !rbacStringSliceMatches(rule.Verbs, "list") {
			continue
		}
		if !rbacStringSliceMatches(rule.APIGroups, gvr.Group) {
			continue
		}
		if !rbacStringSliceMatches(rule.Resources, gvr.Resource) {
			continue
		}
		return true
	}
	return false
}

// rbacStringSliceMatches is the RBAC wildcard rule: "*" matches every
// value; otherwise exact match. Mirrors rbac/evaluate.go:stringSliceMatches
// — duplicated here so cache/ doesn't import rbac/ (rbac/ already imports
// cache/, so an inverse import would create a cycle).
func rbacStringSliceMatches(allowed []string, want string) bool {
	for _, a := range allowed {
		if a == "*" || a == want {
			return true
		}
	}
	return false
}
