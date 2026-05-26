// binding_set_enumeration.go — Ship A.3 / 0.30.179.
//
// EnumerateBindingSetClasses derives the canonical {Username, Groups}
// COHORT set from the published RBAC snapshot via BINDING-SET dedupe at
// the cache-key layer (BindingSetHash) rather than the pointer-set dedupe
// the prior-art EnumerateRBACCohorts (rbac_cohorts.go) uses. Two cohorts
// whose BindingSetHash is equal collapse into ONE class; the Phase 1 PIP
// seed loop drives one re-resolve per class, populating the L1 cell that
// every member of the class hits at request time.
//
// THE ALGORITHMIC INSIGHT — `relevantGroups(userKey)`.
//
//   A naive enumeration would walk every (userKey, gs∈powerset(allGroups))
//   tuple. That is O(2^|allGroups|) and unbounded — admin-scale clusters
//   carry hundreds of groups across thousands of bindings.
//
//   THE INSIGHT: a user's effective L1 cell is determined ENTIRELY by the
//   pointer-set of RBAC bindings the snapshot indexes match for that
//   identity (CRBsByUser[u] ∪ CRBsByGroup[g for g in u.groups] ∪ namespace
//   variants). A group `g` is RELEVANT to user `u` IFF some binding has
//   `(u, g)` in its Subjects — i.e., g appears in a binding alongside u OR
//   alongside another userKey we are already enumerating. Groups never
//   co-occurring with u in any binding produce IDENTICAL pointer-sets
//   regardless of whether u carries them, so the enumeration may safely
//   restrict the powerset to relevantGroups(u).
//
//   Empirically (admin scale): |relevantGroups(u)| is typically ≤ 8.
//   Powerset 2^8 = 256. Across N users we enumerate N × 256 = ~12K tuples,
//   each O(K) where K = matched bindings — well inside the Phase 1 seed
//   budget. Without the bound, the full powerset would be 2^|allGroups|
//   ≈ 2^200+ — astronomical.
//
// SAFETY CAP.
//
//   If |relevantGroups(u)| > 20 for some user u, we fall back to a
//   SINGLE-GROUP enumeration for that user (one tuple per group). This
//   handles outlier topologies — a single user explicitly named in 20+
//   group-cross-bound CRBs — without burning the seed budget on 2^21+
//   combinations. The fallback is observable via the counter
//   `phase1.enum.powerset.skipped` (incremented per user falling back).
//
//   The 20-cap is an EMPIRICAL bound, NOT a hardcoded special-case
//   (feedback_no_special_cases): the cap is uniform over every user, the
//   counter surfaces the skip so the ops team can raise the cap or
//   diagnose the topology if real production data crosses it.
//
// PRUNING `system:authenticated`.
//
//   rbac/evaluate.go:559-564 grants `system:authenticated` IMPLICITLY to
//   every authenticated request. The snapshot's CRBsByGroup may carry a
//   `system:authenticated` key with bindings; we MUST NOT enumerate
//   powerset combinations *including* / *excluding* it as if it were a
//   discretionary group — every authenticated request carries it. The
//   correct model is to ALWAYS include it in `groups` when running the
//   per-tuple BindingSetHash computation. We thus prune it from the
//   powerset domain but inject it back into every tuple's groups at hash
//   time.
//
// HG-178.2 invariant: BindingSetHash matches CohortRBACGen's
// `fnv64aPointers(collectCohortBindingPtrs(...))` byte-for-byte — same
// helpers, same snapshot, same code path. By construction the L1 cell
// the seed populates is the SAME cell the request-time
// dispatchCacheLookupKey hashes for any cohort member.
//
// AC-178.8: re-enumeration fires on RBAC informer rebuild via existing
// `rbacRebuildDirty` — callers of EnumerateBindingSetClasses are
// responsible for re-running on snapshot publish. The Phase 1 PIP seed
// loop runs once per Phase 1; subsequent RBAC mutations land via the
// dirty-mark + dispatcher cache MISS + reseed lifecycle (HG-178.5).
//
// CONCURRENCY — lock-free. The snapshot is immutable post-publish;
// every read goes through `rbacSnap.Load()`. The internal dedup map
// uses `sync.Map.LoadOrStore` so multi-goroutine enumeration (if a
// future caller fans out) is safe; today the single Phase 1 caller
// serialises the call so contention is moot.
//
// FEEDBACK_CHECK_K8S_CLIENTGO_PRIOR_ART: client-go has no equivalent
// — RBAC cohort enumeration is a snowplow-specific concern (the K8s
// authorizer never enumerates cohorts; it evaluates per-request).
//
// FEEDBACK_NO_SPECIAL_CASES: no hardcoded admin / system:masters
// branches. The pruning of `system:authenticated` flows from the
// upstream evaluator's documented implicit-group rule (evaluate.go:559).
// The pruning of `system:serviceaccount:` flows from the same upstream
// convention — K8s' canonical ServiceAccount username pattern (see
// k8s.io/apiserver/pkg/authentication/serviceaccount.ServiceAccountUsernamePrefix).
//
// SERVICE-ACCOUNT COHORT PRUNING — Ship 0.30.182 (A.3-refine).
//
//   ServiceAccount identities are STRUCTURALLY OUT OF SCOPE for the PIP
//   prewarm seed: SAs are in-cluster Go callers, not authn-JWT routers,
//   so they never issue traffic against the /call dispatcher. Two
//   distinct entry-points carry SA subjects into the enumeration domain
//   and BOTH are pruned:
//
//     1. Subject.Kind == "ServiceAccount" appearing in a binding's
//        Subjects list. The Subject-walk (relevantGroups construction)
//        explicitly skips this Kind — see the switch arms in the CRB +
//        RB loops. The snapshot indexer routes SA-kind subjects to
//        CRBsByServiceAccount / RBsByServiceAccountByNS (not to
//        CRBsByUser / RBsByUserByNS), so this branch is primarily
//        defensive — future indexer changes that route SA-kind into
//        user-keyed maps would still see SA cohorts pruned here.
//
//     2. Subject.Kind == "User" with Subject.Name carrying the canonical
//        SA username prefix "system:serviceaccount:<ns>:<name>". This is
//        a K8s authn convention: a bearer-token-authenticated SA
//        arrives at the apiserver as a User with this prefixed name.
//        Bindings authored against this pattern land in CRBsByUser /
//        RBsByUserByNS, so the userKeys collection step prunes them by
//        prefix detection at collection time.
//
//   Pruning by the K8s-standard prefix is analogous to the existing
//   "system:authenticated" prune — both are upstream-defined identity
//   conventions, not snowplow special-cases. The constant
//   `serviceAccountUsernamePrefix` documents the source.
//
//   FAIL-CLOSED RESTORED. With SA cohorts pruned at enumeration, the
//   downstream Phase 1 PIP seed loop (phase1_pip_seed.go) treats any
//   per-cohort seed error as FAIL-CLOSED again — narrow SA cohorts can
//   no longer surface as expected-deny failures.

package cache

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	rbacv1 "k8s.io/api/rbac/v1"
)

// systemAuthenticatedGroup is the canonical implicit-group name the
// upstream evaluator (evaluate.go:559-564) grants every authenticated
// request. Pruned from the powerset domain and INJECTED back into every
// tuple's groups at BindingSetHash time.
const systemAuthenticatedGroup = "system:authenticated"

// serviceAccountUsernamePrefix is the K8s-standard prefix that authn
// emits for SA bearer-token requests. Defined upstream as
// k8s.io/apiserver/pkg/authentication/serviceaccount.ServiceAccountUsernamePrefix
// = "system:serviceaccount:". Any User-kind identity whose name starts
// with this prefix represents an SA — pruned at enumeration time
// because SAs do not issue /call traffic (see file header).
const serviceAccountUsernamePrefix = "system:serviceaccount:"

// isServiceAccountUsername reports whether `name` is the canonical K8s
// SA username pattern "system:serviceaccount:<ns>:<name>". Returns
// false on empty input. Generic over name content — no per-name
// hardcoding (feedback_no_special_cases).
func isServiceAccountUsername(name string) bool {
	return strings.HasPrefix(name, serviceAccountUsernamePrefix)
}

// bindingSetPowersetCap is the empirical safety cap on
// |relevantGroups(userKey)|. A user whose relevant-groups count exceeds
// this falls back to single-group enumeration (one tuple per group) and
// bumps phase1EnumPowersetSkippedTotal. Documented in the file header.
const bindingSetPowersetCap = 20

// phase1EnumPowersetSkippedTotal counts users whose relevantGroups exceed
// bindingSetPowersetCap and thus fell back to single-group enumeration.
// Exposed via the expvar pipeline in phase1_pip_metrics.go (the
// dispatcher package) as `snowplow_phase1_enum_powerset_skipped`.
var phase1EnumPowersetSkippedTotal atomic.Uint64

// phase1EnumBindingsetClassesTotal records the size of the most recent
// enumeration result. Exposed as
// `snowplow_phase1_bindingset_classes_total`.
var phase1EnumBindingsetClassesTotal atomic.Uint64

// Phase1EnumPowersetSkippedTotal returns the cumulative count of users
// whose |relevantGroups| > bindingSetPowersetCap and thus fell back to
// single-group enumeration. Read-only accessor for the metrics layer.
func Phase1EnumPowersetSkippedTotal() uint64 {
	return phase1EnumPowersetSkippedTotal.Load()
}

// Phase1EnumBindingsetClassesTotal returns the size of the most recent
// EnumerateBindingSetClasses result. Read-only accessor for the metrics
// layer.
func Phase1EnumBindingsetClassesTotal() uint64 {
	return phase1EnumBindingsetClassesTotal.Load()
}

// EnumerateBindingSetClasses returns the canonical Cohort list derived
// from the published RBAC snapshot via BindingSetHash dedupe. Two
// identities whose binding-pointer-set hashes equal collapse into ONE
// cohort. Returns nil when no snapshot has been published.
//
// The returned slice is sorted by (Username, Groups[0]) for stable
// log output and deterministic cohort ordering across pod restarts.
func EnumerateBindingSetClasses() []Cohort {
	snap := rbacSnap.Load()
	if snap == nil {
		return nil
	}

	// Step 1 — userKeys = union of CRBsByUser ∪ ∪_ns RBsByUserByNS,
	// pruning SA-style canonical usernames (system:serviceaccount:<ns>:<name>).
	// See file header §SERVICE-ACCOUNT COHORT PRUNING for rationale: SAs
	// don't issue /call traffic, so their cohorts produce no first-paint
	// L1 hit and FAIL-CLOSED on narrow-SA seed denials would be spurious.
	// Pruning by the K8s-standard prefix is generic — no per-name
	// hardcoding (feedback_no_special_cases).
	userKeys := map[string]struct{}{}
	for u := range snap.CRBsByUser {
		if isServiceAccountUsername(u) {
			continue
		}
		userKeys[u] = struct{}{}
	}
	for _, inner := range snap.RBsByUserByNS {
		for u := range inner {
			if isServiceAccountUsername(u) {
				continue
			}
			userKeys[u] = struct{}{}
		}
	}

	// Step 2 — groupKeys = union of CRBsByGroup ∪ ∪_ns RBsByGroupByNS,
	// pruning systemAuthenticatedGroup (implicit per evaluate.go:559).
	groupKeys := map[string]struct{}{}
	for g := range snap.CRBsByGroup {
		if g == systemAuthenticatedGroup || g == "" {
			continue
		}
		groupKeys[g] = struct{}{}
	}
	for _, inner := range snap.RBsByGroupByNS {
		for g := range inner {
			if g == systemAuthenticatedGroup || g == "" {
				continue
			}
			groupKeys[g] = struct{}{}
		}
	}

	// Step 3 — build relevantGroups(u): the set of group names that
	// CO-OCCUR with u in some binding's Subjects. Walk every CRB +
	// every RB once; for each binding whose Subjects include a User
	// matching some u in userKeys, add every Group in the same Subjects
	// to relevantGroups[u]. ServiceAccount + catch-all kinds are
	// ignored (this enumerator is for User/Group cohorts only).
	relevantGroups := map[string]map[string]struct{}{}
	addRelevant := func(u, g string) {
		if g == systemAuthenticatedGroup || g == "" {
			return
		}
		inner, ok := relevantGroups[u]
		if !ok {
			inner = map[string]struct{}{}
			relevantGroups[u] = inner
		}
		inner[g] = struct{}{}
	}

	// ClusterRoleBindings.
	for _, crb := range snap.ClusterRoleBindings {
		if crb == nil {
			continue
		}
		// Collect users + groups present in this binding's Subjects.
		// ServiceAccount-kind subjects are skipped: SAs don't issue /call
		// traffic, so they produce no enumerated cohorts (see file header
		// §SERVICE-ACCOUNT COHORT PRUNING). User-kind subjects whose name
		// carries the K8s-standard SA prefix are pruned from userKeys at
		// collection time — the userKeys lookup below excludes them too.
		var users, groups []string
		for _, s := range crb.Subjects {
			switch s.Kind {
			case rbacv1.UserKind:
				if _, ok := userKeys[s.Name]; ok {
					users = append(users, s.Name)
				}
			case rbacv1.GroupKind:
				if _, ok := groupKeys[s.Name]; ok {
					groups = append(groups, s.Name)
				}
			case rbacv1.ServiceAccountKind:
				// SAs are structurally out of scope for /call prewarm.
				continue
			}
		}
		for _, u := range users {
			for _, g := range groups {
				addRelevant(u, g)
			}
		}
	}

	// RoleBindings (namespaced).
	for _, byNS := range snap.RoleBindingsByNS {
		for _, rb := range byNS {
			if rb == nil {
				continue
			}
			var users, groups []string
			for _, s := range rb.Subjects {
				switch s.Kind {
				case rbacv1.UserKind:
					if _, ok := userKeys[s.Name]; ok {
						users = append(users, s.Name)
					}
				case rbacv1.GroupKind:
					if _, ok := groupKeys[s.Name]; ok {
						groups = append(groups, s.Name)
					}
				case rbacv1.ServiceAccountKind:
					// SAs are structurally out of scope for /call prewarm.
					continue
				}
			}
			for _, u := range users {
				for _, g := range groups {
					addRelevant(u, g)
				}
			}
		}
	}

	// Step 4 — enumerate (userKey, gs) tuples. For each user (PLUS the
	// empty-user "" which represents Group-only cohorts), generate the
	// powerset over relevantGroups[u]. Apply the safety cap.
	//
	// `byHash` dedupes by BindingSetHash via sync.Map.LoadOrStore. The
	// VALUE is the cohort representative we choose to materialise. We
	// re-validate sort order at materialisation, so the choice of
	// representative within a class is irrelevant to determinism.
	var byHash sync.Map // uint64 -> Cohort

	enumerateUser := func(u string) {
		var rel []string
		if inner, ok := relevantGroups[u]; ok {
			rel = make([]string, 0, len(inner))
			for g := range inner {
				rel = append(rel, g)
			}
			sort.Strings(rel)
		}

		if len(rel) > bindingSetPowersetCap {
			// SAFETY CAP — fall back to single-group enumeration.
			phase1EnumPowersetSkippedTotal.Add(1)
			// Single-group tuples: (u, {g}) for each g in rel — plus
			// (u, {}) covering "user-only" bindings.
			storeTuple(&byHash, u, nil)
			for _, g := range rel {
				storeTuple(&byHash, u, []string{g})
			}
			return
		}

		// Full powerset over rel. 2^len(rel) iterations; len ≤ cap.
		n := len(rel)
		// 1<<n is bounded by 2^cap so safe under cap.
		total := 1 << n
		for mask := 0; mask < total; mask++ {
			var gs []string
			for i := 0; i < n; i++ {
				if mask&(1<<i) != 0 {
					gs = append(gs, rel[i])
				}
			}
			storeTuple(&byHash, u, gs)
		}
	}

	// Enumerate every user-key, then the empty-user (Group-only cohorts:
	// the snapshot's group-keys without a paired user identity).
	for u := range userKeys {
		enumerateUser(u)
	}
	// Empty-user: cover identities that arrive with no User-kind binding
	// match but match some Group binding. We enumerate per individual
	// group (each group on its own is a distinct cohort representative);
	// this is enough because EvaluateRBAC's Group-subject matcher fires
	// per-group, so a multi-group anonymous request resolves through the
	// per-group bindings independently.
	for g := range groupKeys {
		storeTuple(&byHash, "", []string{g})
	}

	// Step 5 — materialise + sort by (Username, Groups[0]).
	var out []Cohort
	byHash.Range(func(_, v any) bool {
		out = append(out, v.(Cohort))
		return true
	})
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

	phase1EnumBindingsetClassesTotal.Store(uint64(len(out)))
	return out
}

// storeTuple computes the BindingSetHash for (username, groups) and
// dedupes the cohort into byHash. BindingSetHash itself injects the
// implicit `system:authenticated` group for any non-empty username
// (mirrors evaluate.go), so we MUST NOT inject it again here — we just
// surface it in the stored Cohort.Groups so the seed's withCohortSeedContext
// installs an explicit Groups that includes it.
//
// `groups` is the powerset-or-single-group tuple from the enumeration
// step; we record the cohort with that tuple + the implicit-auth group
// for non-empty usernames so the seed ctx's WithUserInfo carries
// everything BindingSetHash would inject — keeping the seed-time and
// request-time tuples literal-equal as well as hash-equal.
//
// An empty username + empty groups (anonymous + no relevant groups) is
// a valid cohort tuple — anonymous bindings only.
func storeTuple(byHash *sync.Map, username string, groups []string) {
	// Compute the hash via BindingSetHash (which handles implicit-auth
	// injection internally). This guarantees AC-178.2 byte-equality with
	// the request-time hash.
	hash := BindingSetHash(username, groups)
	if hash == 0 {
		// Snapshot disappeared between Load and BindingSetHash — skip.
		return
	}

	// Build the cohort's stored Groups: include the original groups +
	// implicit-auth for authenticated requests, sorted deterministically.
	var stored []string
	if username != "" {
		hasAuth := false
		for _, g := range groups {
			if g == systemAuthenticatedGroup {
				hasAuth = true
				break
			}
		}
		if hasAuth {
			stored = append([]string(nil), groups...)
		} else {
			stored = make([]string, 0, len(groups)+1)
			stored = append(stored, groups...)
			stored = append(stored, systemAuthenticatedGroup)
		}
	} else {
		stored = append([]string(nil), groups...)
	}
	sort.Strings(stored)

	// LoadOrStore — first-write-wins. Cohort identities that hash to the
	// same BindingSetHash are members of the same equivalence class; we
	// keep the first one encountered as the representative.
	cohort := Cohort{Username: username, Groups: stored}
	byHash.LoadOrStore(hash, cohort)
}

// ResetBindingSetEnumerationCountersForTest clears the package-level
// counters so tests can observe a clean delta. Production code MUST NOT
// call this.
func ResetBindingSetEnumerationCountersForTest() {
	phase1EnumPowersetSkippedTotal.Store(0)
	phase1EnumBindingsetClassesTotal.Store(0)
}
