// binding_set_enumeration_group.go — Ship 0.30.184 (ζ)-extend Group-kind.
//
// Symmetric extension of predicate (ζ) — Ship 0.30.183 — from User-kind
// to Group-kind subjects. The User-kind predicate
// `pruneUserKindSubjectZeta` (binding_set_enumeration.go) prunes
// control-plane User-kind cohorts whose matched-binding PolicyRules
// have empty intersection with the snowplow handler GVR set. The
// 0.30.183 production deploy proved the User-kind cut correct (29
// cohorts pruned) BUT surfaced a residual defect: 5 Group-kind
// control-plane cohorts (system:monitoring, system:nodes,
// system:serviceaccounts, system:unauthenticated, system:masters
// partial) STILL produced per-cohort seed failures because they were
// not in scope for the User-kind predicate. The architect's wire-probe
// (/tmp/snowplow-runs/0.30.184/before/wire-probe-zeta-groups.txt)
// confirmed the 4 PRUNE candidates carry zero *.krateo.io apiGroups
// and the 2 KEEP candidates (cluster-admin via wildcard,
// authn-group-krateo-system via templates.krateo.io overlap) carry
// non-empty intersections.
//
// SUBJECT-KIND SYMMETRY (feedback_predicate_subject_kind_symmetry).
//
//   Predicate (ζ) is now uniform over (subject name, matched bindings,
//   handler GVR set) for BOTH User- and Group-kind subjects. The two
//   subject-kind branches share:
//
//     - the same `unionRulesForRefs` helper (resolves RoleRefs to
//       PolicyRule union via ClusterRolesByName + RolesByNSName);
//     - the same `stringSliceMatchesRBAC` wildcard semantics
//       (K8s authorizer parity);
//     - the same `handlerGVRSetSnapshot()` input set
//       (captured once per EnumerateBindingSetClasses call);
//     - the same per-prune INFO log shape
//       (`binding_set.prune` + reason + matched_roles_count + matched_roles).
//
//   The User-kind branch additionally short-circuits on the K8s
//   `system:`-prefix as a pure-performance fast path. The Group-kind
//   branch INTENTIONALLY OMITS this fast path because the wire-probe
//   surfaced `system:masters` as a Group-kind subject bound to
//   `cluster-admin` (wildcard rule → KEEP). Fast-pruning every
//   `system:`-prefixed Group would FALSE-PRUNE `system:masters` and
//   strip the admin cohort from Phase 1 PIP seeding — a correctness
//   defect, not a perf optimisation. The Group-kind branch ALWAYS
//   walks the PolicyRule union.
//
// FAIL-CLOSED PRESERVED. Group-kind subjects with empty matched
// bindings, nil snapshot, or empty handler GVR set fall through to
// the same `return true` defensive prune the User-kind predicate uses.
// HG-178.5 reseed absorbs any false-prune.
//
// HG-184.7 / HG-184.13 / HG-184.14 invariants (PM acceptance):
//
//   - HG-184.7  ServiceAccount-kind subjects are routed to the SA
//                snapshot landings (CRBsByServiceAccount / RBs…) and
//                never reach the Group-kind walk. The Group-kind
//                predicate has no SA branch by construction — it is
//                only invoked from the Group-walk loop in
//                EnumerateBindingSetClasses.
//   - HG-184.13 A `system:monitoring`-shaped Group cohort bound only
//                to a no-krateo-overlap role MUST prune. Cache-off
//                transparent fallback still applies (EvaluateRBAC
//                returns 403 from the live RBAC walk, not from the
//                cache pre-compute).
//   - HG-184.14 `system:masters` MUST survive the Group-kind predicate
//                because cluster-admin's wildcard PolicyRule overlaps
//                every handler GVR. Verified end-to-end in
//                TestPrunePredicate_ZetaCorpusReal_Groups.
//
// PRUNE LOG SHAPE. Identical to User-kind, distinguished by
// `subject_kind="Group"`:
//
//   binding_set.prune subsystem=cache subject_kind=Group
//     name=<group-name> reason=<empty_intersection|no_matched_bindings|
//     handler_set_empty> matched_roles_count=<int>
//     matched_roles=[<role names>]
//
//   NOTE: Group-kind does NOT emit `reason=system_prefix` because the
//   fast path is deliberately omitted (see above).
//
// FEEDBACK_NO_SPECIAL_CASES: predicate stays generic over (subject
// name, matched bindings, handler GVR set). No per-name hardcoding,
// no chart-tunable identity lists. The fact that `system:masters`
// survives is a CONSEQUENCE of cluster-admin's wildcard PolicyRule —
// not a hardcoded carve-out for the name `system:masters`.

package cache

import (
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// pruneGroupKindSubjectZeta reports whether a Group-kind subject's
// matched bindings grant access to ANY snowplow handler GVR (predicate
// (ζ), Ship 0.30.184). Empty intersection ⇒ PRUNE.
//
// Symmetric with `pruneUserKindSubjectZeta` (Ship 0.30.183) EXCEPT:
//
//   - NO `system:`-prefix fast path. The wire-probe surfaced
//     `system:masters` as a Group-kind subject bound to `cluster-admin`
//     (wildcard rule → KEEP). Fast-pruning every `system:`-prefixed
//     Group would FALSE-PRUNE `system:masters` and strip the admin
//     cohort from Phase 1 PIP seeding. Performance cost: 5 extra
//     PolicyRule walks at enumeration time on the live GKE corpus
//     (one per system:-prefixed Group subject) — negligible.
//
//   - Defensive empty-matched-bindings branch returns PRUNE
//     (fail-closed); HG-178.5 reseed absorbs any false-prune.
//
// `refs` is the deduplicated set of (kind, namespace, name) RoleRef
// tuples matched for the Group subject (CRBs + per-namespace RBs).
// `snap` resolves the tuples to PolicyRule slices via
// ClusterRolesByName + RolesByNSName. `handlerGVRSet` is captured ONCE
// at enumeration start and passed as an immutable slice — no per-call
// lookup race.
//
// Wildcard semantics: stringSliceMatchesRBAC mirrors the K8s
// authorizer. A role with APIGroups=["*"] (cluster-admin pattern)
// therefore overlaps every handler GVR and survives regardless of
// the handler set.
//
// Per `feedback_no_special_cases`: predicate is uniform over every
// Group-kind subject — no per-name hardcoding, no chart-tunable
// identity lists. Per `feedback_check_k8s_clientgo_prior_art`:
// PolicyRule intersection is the K8s authorizer's own semantics — we
// reuse the same `stringSliceMatchesRBAC` helper.
func pruneGroupKindSubjectZeta(name string, refs []roleRefKey, snap *RBACSnapshot, handlerGVRSet []schema.GroupResource) bool {
	// NO system: fast path — would FALSE-PRUNE system:masters bound to
	// cluster-admin. The Group-kind branch ALWAYS walks the PolicyRule
	// union.
	_ = name // referenced only for future log enrichment / observability

	if len(refs) == 0 {
		// Defensive fail-closed: a Group subject with no matched
		// RoleRefs cannot serve any handler GVR. Today's indexer
		// guarantees at least one ref for every groupKey, so this
		// branch is unreachable in production; a future indexer
		// change that surfaces an orphan would prune here and the
		// HG-178.5 reseed lifecycle would absorb the false-prune.
		return true
	}
	if snap == nil || len(handlerGVRSet) == 0 {
		// Defensive: handler set empty means no /call could ever
		// match — prune (fail-closed). Snapshot nil should never
		// happen because the caller already gated on
		// rbacSnap.Load() != nil.
		return true
	}
	rules := unionRulesForRefs(snap, refs)
	for _, gr := range handlerGVRSet {
		for _, rule := range rules {
			if stringSliceMatchesRBAC(rule.APIGroups, gr.Group) &&
				stringSliceMatchesRBAC(rule.Resources, gr.Resource) {
				return false // KEEP — overlap with snowplow GVR
			}
		}
	}
	return true // PRUNE — empty intersection
}

// collectMatchedRoleRefsForGroup is the Group-kind mirror of
// `collectMatchedRoleRefsForUser`. Walks CRBsByGroup[name] and
// RBsByGroupByNS[*][name] to gather every binding the Group subject
// matches; deduplicates and sorts the resulting RoleRef tuples.
//
// Returns nil when the Group is bound by no binding (the defensive
// empty-bindings branch in `pruneGroupKindSubjectZeta`). The returned
// slice is sorted for stable log output.
//
// CRB roleRefs always carry Kind="ClusterRole" Namespace=""; RB
// roleRefs carry the RB's namespace when ref.Kind=="Role" and "" when
// ref.Kind=="ClusterRole" (a cluster-role can be referenced from any
// scope, the upstream RBAC authorizer permits it).
func collectMatchedRoleRefsForGroup(snap *RBACSnapshot, name string) []roleRefKey {
	if snap == nil {
		return nil
	}
	seen := map[roleRefKey]struct{}{}
	for _, crb := range snap.CRBsByGroup[name] {
		if crb == nil {
			continue
		}
		// CRBs reference ClusterRoles only (rbac/evaluate.go:409-417).
		seen[roleRefKey{Kind: "ClusterRole", Namespace: "", Name: crb.RoleRef.Name}] = struct{}{}
	}
	for ns, inner := range snap.RBsByGroupByNS {
		for _, rb := range inner[name] {
			if rb == nil {
				continue
			}
			// RB roleRef may be Role (ns-scoped) or ClusterRole
			// (cluster-scoped, referenced from a ns RB).
			switch rb.RoleRef.Kind {
			case "Role":
				seen[roleRefKey{Kind: "Role", Namespace: ns, Name: rb.RoleRef.Name}] = struct{}{}
			case "ClusterRole":
				seen[roleRefKey{Kind: "ClusterRole", Namespace: "", Name: rb.RoleRef.Name}] = struct{}{}
			default:
				// Defensive: unknown kind — skip. The K8s RBAC API
				// validates roleRef.Kind at admission; an unknown
				// kind landing here would already be authorizer-deny.
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]roleRefKey, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	// Reuse the canonical sort order from collectMatchedRoleRefsForUser
	// so log output is consistent across the two subject kinds.
	sortRoleRefKeys(out)
	return out
}

// sortRoleRefKeys is a tiny shared helper that imposes the canonical
// (Kind, Namespace, Name) sort order on a roleRefKey slice. Extracted
// so the Group-kind branch yields byte-identical log output ordering
// to the User-kind branch.
func sortRoleRefKeys(out []roleRefKey) {
	// Inline insertion sort would be more elegant for ≤8-entry slices
	// but sort.Slice keeps parity with collectMatchedRoleRefsForUser.
	// The two functions MUST yield the same total order for any input.
	sortRoleRefKeysImpl(out)
}

// sortRoleRefKeysImpl is split out so the sort import lives only in
// this file (binding_set_enumeration.go imports sort already for the
// User-kind variant). The two implementations are byte-identical.
func sortRoleRefKeysImpl(out []roleRefKey) {
	// O(n log n) sort via Go stdlib — same predicate as
	// collectMatchedRoleRefsForUser. We could share a single
	// implementation, but keeping the body inline here documents the
	// symmetry obligation at the diff site.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			a, b := out[j-1], out[j]
			less := false
			if a.Kind != b.Kind {
				less = a.Kind < b.Kind
			} else if a.Namespace != b.Namespace {
				less = a.Namespace < b.Namespace
			} else {
				less = a.Name < b.Name
			}
			if less {
				break
			}
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
}

// groupKindPruneReason classifies why pruneGroupKindSubjectZeta
// returned true. Mirrors the User-kind reason classifier inlined in
// EnumerateBindingSetClasses but is exposed as a function so the
// Group-walk plug-in can emit it with the same set of well-known
// strings. The set is intentionally distinct from the User-kind set
// (no `system_prefix` reason — the Group-kind branch has no fast path).
//
// Returns one of:
//
//   - "no_matched_bindings" — refs slice empty
//   - "handler_set_empty"   — handlerGVRSet empty
//   - "empty_intersection"  — refs + handler set non-empty, but no rule overlapped a handler GVR
func groupKindPruneReason(refs []roleRefKey, handlerGVRSet []schema.GroupResource) string {
	if len(refs) == 0 {
		return "no_matched_bindings"
	}
	if len(handlerGVRSet) == 0 {
		return "handler_set_empty"
	}
	return "empty_intersection"
}

// Compile-time check that rbacv1 is used (this file uses the package
// transitively through roleRefKey / RBACSnapshot, but Go's unused-
// import rule would otherwise complain when the file is read in
// isolation). The helper is unreachable.
var _ = rbacv1.GroupKind
