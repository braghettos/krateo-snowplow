// rbac_cohorts.go — Ship PIP / 0.30.173.
//
// EnumerateRBACCohorts derives the canonical {Username,Groups} cohorts
// from the published RBAC snapshot's subject->binding indexes
// (CRBsByUser + RBsByUserByNS — rbac_snapshot.go:137-149). The cohort
// set drives the Phase-1 per-identity prewarm seed (Step 7.6 in
// phase1_walk.go): one seed per distinct cohort, dedup'ed by the union
// of binding-pointer-sets it matches.
//
// CANONICAL DEDUPE — TWO USERS ARE THE SAME COHORT IFF THEIR UNION OF
// BINDING-IDENTITY-SETS IS EQUAL. This is the architect's brief: an
// identity sees, in the RBAC evaluator, exactly the bindings whose
// Subjects.Kind=="User" + Subjects.Name==<username>. Two usernames that
// reach the SAME binding set yield byte-identical resolved output for
// every dispatcher key — they are one cohort.
//
// Ship 1 / 0.30.195 — binding identity is the binding's IMMUTABLE
// `metadata.uid` (empty-UID fallback = a stable content tuple), NOT the
// pointer address. The pre-0.30.195 pointer comparison drifted on every
// snapshot republish (~4.6/s), making the seed key diverge from the
// dispatch key; UID is stable across relist so the two agree across
// generations. The dedupe MEMBERSHIP is unchanged (UID-set == pointer-set
// membership); only the encoding moved from address to UID. Two cohorts
// whose binding *contents* happen to be equal but whose Subject.Name
// differs intentionally stay distinct — group membership flows through
// the matched binding set, not the name.
//
// GROUPS — the architect's brief defines a cohort as {Username, Groups}.
// The snapshot's CRBsByUser maps Subject.Kind=="User" Name. The user's
// Group membership is NOT carried in the snapshot itself — at request
// time it arrives on the request context via xcontext.UserInfo(ctx). For
// the Phase-1 seed we MUST install the user's groups so the seed's
// EvaluateRBAC verdict (gateGetEnvelope) matches the request-time
// verdict for the same identity. Without groups the seed would resolve
// under "user with NO groups" and a per-binding Group subject that the
// real request resolves against would diverge — a per-user mismatch
// landing as l1_hit:"miss" at first /call (HG-PIP.1 falsifier triggers).
//
// HOW WE DERIVE GROUPS — feedback_no_special_cases.md compliant. The
// only Group claim a user holds in this snowplow deployment flows
// through the snowplow JWT (jwtutil.UserInfo.Groups). The snapshot has
// no place to store per-user Group claims (k8s RBAC has no Group
// MEMBERSHIP, only Group SUBJECTS on bindings). The cohort therefore
// records Username but leaves Groups EMPTY at enumeration time; the
// per-seed ctx installs the cohort's Username via xcontext.WithUserInfo
// and the EvaluateRBAC evaluator sees user-kind bindings only. The
// remaining attack surface is a Group-only binding that admits an
// identity by group membership — a Group-only cohort. To cover those
// the enumerator ALSO emits ONE synthetic per-Group cohort
// {Username:"", Groups:[<group>]} per CRBsByGroup / RBsByGroupByNS key.
// At seed time the per-Group cohort installs Groups=<group> with empty
// Username, so EvaluateRBAC's Group-subject matcher fires exactly once
// per Group. A user that holds BOTH a user-binding AND a group-binding
// will hit two cohorts; the first /call resolves under the user-only
// identity (snowplow JWT carries both Username + Groups) and the
// seeded cohort that matches the request-time identity wins on Get.
//
// COHORT CAP — the architect's PM gate (#392, OQ-2) is FAIL-CLOSED at
// 50 cohorts. EnumerateRBACCohorts returns up to all canonical cohorts;
// the CALLER (runPIPSeed) checks len(cohorts) > 50 and FAIL-CLOSES
// (phase1Done stays false). This is the storage-bound guard: each
// cohort × (N_restactions+N_widgets) is an L1 entry, so 50 ×
// (~10+~25) ≈ 1750 entries is the upper bound on PIP-seeded L1 — well
// inside F1's resident-bytes ceiling.
//
// EMPTY SNAPSHOT — if rbacSnap.Load() returns nil (pre-readiness /
// cache=off), EnumerateRBACCohorts returns nil. The caller MUST treat
// nil as "no cohorts to seed" — not an error. This is the same
// degrade-to-deny posture EvaluateRBAC takes at evaluate.go:124 when
// the snapshot is not yet published.
//
// CONCURRENCY — lock-free. Snapshot() returns the published pointer
// atomically and the snapshot is immutable post-publish (§3 invariant
// 1, rbac_snapshot.go:36-51). EnumerateRBACCohorts iterates the
// already-built indexes; no mutation, no copy of typed RBAC objects.

package cache

import (
	"sort"

	rbacv1 "k8s.io/api/rbac/v1"
)

// Cohort is a canonical RBAC identity for the Phase-1 per-identity
// prewarm seed. Username is set for user-kind cohorts and empty for
// group-kind cohorts; Groups is the group membership the seed ctx
// installs via xcontext.WithUserInfo.
//
// Pointer-set equality (the architect's dedupe contract) compares the
// pointer-sets of the {ClusterRoleBinding, RoleBinding} the cohort
// matches in the published snapshot — see canonicalCohortKey.
type Cohort struct {
	Username string
	Groups   []string
}

// EnumerateRBACCohorts returns the canonical Cohort list derived from
// the current RBAC snapshot. Two identities collapse into ONE cohort
// iff their union of matched (ClusterRoleBinding, RoleBinding)
// pointer-sets is equal. Returns nil when no snapshot has been
// published (degrade-to-deny posture; caller treats nil as "no cohorts
// to seed").
//
// The returned slice is sorted by Username then Groups[0] for stable
// log output and deterministic cohort ordering across pod restarts.
func EnumerateRBACCohorts() []Cohort {
	snap := rbacSnap.Load()
	if snap == nil {
		return nil
	}

	// Group cohorts by their canonical binding-pointer-set key. The
	// VALUE is the cohort representative; the KEY is the canonical
	// string built from sorted pointer addresses of every matched
	// binding (CRB + RB across all namespaces). Two identities whose
	// binding-sets are pointer-equal land on the same key.
	byKey := map[string]Cohort{}

	// User-kind cohorts — drained from CRBsByUser and RBsByUserByNS.
	// Collect the per-username binding-pointer-set by walking BOTH the
	// cluster-wide CRBs and EVERY namespace's RoleBindings keyed on the
	// same Subject.Name.
	usernames := map[string]struct{}{}
	for u := range snap.CRBsByUser {
		usernames[u] = struct{}{}
	}
	for _, inner := range snap.RBsByUserByNS {
		for u := range inner {
			usernames[u] = struct{}{}
		}
	}
	for u := range usernames {
		crbs := snap.CRBsByUser[u]
		var rbs []*rbacv1.RoleBinding
		for _, inner := range snap.RBsByUserByNS {
			rbs = append(rbs, inner[u]...)
		}
		key := canonicalCohortKey(crbs, rbs)
		if _, seen := byKey[key]; seen {
			// First-write-wins: the architect's dedupe contract treats
			// canonically-equal cohorts as ONE. Subsequent identities
			// matching the same binding-set are dropped.
			continue
		}
		byKey[key] = Cohort{Username: u}
	}

	// Group-kind cohorts — drained from CRBsByGroup and RBsByGroupByNS.
	// A group-binding admits any identity holding that group claim; the
	// seed cohort installs Groups=[<group>] with empty Username so
	// EvaluateRBAC's Group-subject matcher fires.
	groups := map[string]struct{}{}
	for g := range snap.CRBsByGroup {
		groups[g] = struct{}{}
	}
	for _, inner := range snap.RBsByGroupByNS {
		for g := range inner {
			groups[g] = struct{}{}
		}
	}
	for g := range groups {
		crbs := snap.CRBsByGroup[g]
		var rbs []*rbacv1.RoleBinding
		for _, inner := range snap.RBsByGroupByNS {
			rbs = append(rbs, inner[g]...)
		}
		key := canonicalCohortKey(crbs, rbs)
		if _, seen := byKey[key]; seen {
			continue
		}
		byKey[key] = Cohort{Groups: []string{g}}
	}

	// Materialise to a slice; sort deterministically so logs and seed
	// ordering are stable across pod restarts.
	out := make([]Cohort, 0, len(byKey))
	for _, c := range byKey {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Username != out[j].Username {
			return out[i].Username < out[j].Username
		}
		// Tiebreak on Groups[0] (group-kind cohorts) — both empty
		// usernames means group-kind; sort by group name.
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

// canonicalCohortKey builds a deterministic key from the sorted STABLE
// identities of every binding in the cohort's union. Ship 1 / 0.30.195
// switched the identity from the binding's pointer ADDRESS to its
// immutable `metadata.uid` (with an empty-UID content-tuple fallback) —
// the SAME identity the L1-cache-key consumers BindingSetHash /
// CohortRBACGen use (collectCohortBindingIDs / crbIdentity / rbIdentity).
// Routing all three consumers through the same identity is what keeps the
// seed-time key (this enumerator) byte-identical to the dispatch-time key
// ACROSS snapshot republishes; the pre-0.30.195 pointer addresses drifted
// every rebuild (~4.6/s) and made the prewarmed cell unreachable.
//
// The key encodes ALL matched identities, sorted ascending, so {a,b} and
// {b,a} hash identically. An empty cohort (no CRBs, no RBs) yields the
// empty key — multiple "no bindings" identities collapse, which is the
// architect's invariant (they all resolve to identical EvaluateRBAC
// verdicts and would produce byte-identical resolved output).
//
// EMPTY-UID FALLBACK — crbIdentity / rbIdentity fall back to a stable
// per-binding content tuple (Kind tag + ns/name + roleRef) when
// metadata.uid is empty, so two distinct empty-UID bindings stay distinct
// rather than collapsing into a shared zero-bucket. See
// collectCohortBindingIDs (rbac_cohort_gen.go) for the full CLAIM-5 note.
func canonicalCohortKey(crbs []*rbacv1.ClusterRoleBinding, rbs []*rbacv1.RoleBinding) string {
	if len(crbs)+len(rbs) == 0 {
		return ""
	}
	ids := make([]string, 0, len(crbs)+len(rbs))
	for _, p := range crbs {
		if p == nil {
			continue
		}
		ids = append(ids, crbIdentity(p))
	}
	for _, p := range rbs {
		if p == nil {
			continue
		}
		ids = append(ids, rbIdentity(p))
	}
	if len(ids) == 0 {
		return ""
	}
	sort.Strings(ids)

	// Build the key by joining the sorted identities with a 0x1e record
	// separator (distinct from the 0x1f field separator the identity
	// tuples use internally) so a single concatenation can never alias a
	// different set partition.
	buf := make([]byte, 0, len(ids)*40)
	for i, id := range ids {
		if i > 0 {
			buf = append(buf, 0x1e)
		}
		buf = append(buf, id...)
	}
	return string(buf)
}
