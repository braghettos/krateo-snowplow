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
// BINDING-POINTER-SETS IS EQUAL. This is the architect's brief: an
// identity sees, in the RBAC evaluator, exactly the bindings whose
// Subjects.Kind=="User" + Subjects.Name==<username>. Two usernames that
// reach the SAME binding set yield byte-identical resolved output for
// every dispatcher key — they are one cohort. Pointer comparison is
// load-bearing: the typed indexes share object identity per
// rbac_snapshot.go:108-112 (the values in CRBsByUser / RBsByUserByNS
// point into the SAME ClusterRoleBindings / RoleBindings slices held
// elsewhere in the snapshot). Two cohorts whose binding *contents*
// happen to be equal but whose Subject.Name differs intentionally stay
// distinct — group membership flows through the binding identity, not
// the name.
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
	"unsafe"

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

// canonicalCohortKey builds a deterministic key from the sorted pointer
// addresses of every binding in the cohort's union. Pointer comparison
// is the architect's dedupe contract — two cohorts whose binding-sets
// are pointer-equal MUST collapse into one. uintptr is stable for the
// lifetime of the snapshot (snapshot is immutable post-publish,
// rbac_snapshot.go:36-51).
//
// The key encodes ALL matched pointer addresses, sorted ascending, so
// {a,b} and {b,a} hash identically. An empty cohort (no CRBs, no RBs)
// yields the empty key — multiple "no bindings" identities would
// collapse, which is the architect's invariant (they all resolve to
// identical EvaluateRBAC verdicts and would produce byte-identical
// resolved output).
func canonicalCohortKey(crbs []*rbacv1.ClusterRoleBinding, rbs []*rbacv1.RoleBinding) string {
	// Capacity: 18 chars per uintptr (max uint64 hex) + 1 separator.
	capHint := (len(crbs) + len(rbs)) * 20
	if capHint == 0 {
		return ""
	}
	addrs := make([]uintptr, 0, len(crbs)+len(rbs))
	for _, p := range crbs {
		if p == nil {
			continue
		}
		addrs = append(addrs, uintptr(unsafe.Pointer(p)))
	}
	for _, p := range rbs {
		if p == nil {
			continue
		}
		addrs = append(addrs, uintptr(unsafe.Pointer(p)))
	}
	sort.Slice(addrs, func(i, j int) bool { return addrs[i] < addrs[j] })

	// Build the key as a fixed-base-16 encoding separated by '|'. We use
	// a manual builder rather than fmt.Sprintf to keep the hot path
	// allocation-cheap (this runs once per Phase-1 startup but staying
	// within the per-rebuild budget is courteous).
	buf := make([]byte, 0, capHint)
	const hex = "0123456789abcdef"
	for i, a := range addrs {
		if i > 0 {
			buf = append(buf, '|')
		}
		// uint64 -> hex, little-endian-friendly: print high nibble
		// first.
		for shift := 60; shift >= 0; shift -= 4 {
			b := hex[(a>>shift)&0xf]
			buf = append(buf, b)
		}
	}
	return string(buf)
}
