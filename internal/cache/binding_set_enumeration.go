// binding_set_enumeration.go — Ship A.3 / 0.30.179, refined 0.30.183.
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
// The predicate (ζ) `pruneUserKindSubjectZeta` (Ship 0.30.183) is
// generic over (subject name, matched-binding rules, handler GVR set)
// — no per-name hardcoding, no chart-tunable identity lists.
//
// CONTROL-PLANE USER-KIND COHORT PRUNING — Predicate (ζ), Ship 0.30.183
// (A.3-refine v3).
//
//   Kubernetes control-plane identities (kubelet, kube-controller-manager,
//   cluster-autoscaler, system:kube-scheduler, …) authenticate to the
//   apiserver as User-kind subjects via RBAC bindings, but NEVER issue
//   /call traffic against the snowplow dispatcher. Their cohorts are
//   structurally out of scope for the PIP prewarm seed — seeding them
//   wastes Phase 1 budget AND, at 0.30.181 production capture, drove the
//   binding-set class count from ~9 useful cohorts to 34 (29 control-
//   plane User cohorts + 5 surviving Group cohorts).
//
//   Predicate (ζ) implements RBAC PolicyRules INTERSECTION semantics —
//   the same predicate the upstream K8s authorizer uses to decide
//   whether a Subject can access a given (apiGroup, resource):
//
//     A User-kind subject is PRUNED iff the union of every matched
//     binding's RoleRef Rules has EMPTY intersection with the snowplow
//     handler GVR set (i.e. there exists no handler GVR (G,R) and no
//     rule r such that stringSliceMatches(r.APIGroups, G) AND
//     stringSliceMatches(r.Resources, R)).
//
//   Empty intersection ⇒ the subject CANNOT serve any /call against the
//   snowplow handler corpus, so a PIP seed for the cohort is guaranteed
//   to either deny or return empty content — both wasted seed work.
//
//   Wire-probe pre-flight (/tmp/snowplow-runs/0.30.183/before/wire-probe-
//   zeta.txt) confirmed that on the live 0.30.181 cluster, all 29
//   control-plane User-kind subjects' matched roles grant only
//   apiGroups ∈ {"", "certificates.k8s.io", "nodemanagement.gke.io",
//   "events.k8s.io", "rbac.authorization.k8s.io", ...} — disjoint from
//   the snowplow handler set {*.krateo.io}. (ζ) prunes them all.
//
//   `system:`-PREFIX FAST PATH. (ζ) retains the system:-prefix
//   short-circuit (predicate α′(a) lineage): subjects whose Subject.Name
//   carries the K8s reserved-name prefix `system:` skip the PolicyRule
//   walk and return PRUNE immediately. This is a pure-performance
//   optimisation — the system:-named subjects we have observed would
//   ALWAYS prune via the empty-intersection path, but the fast path
//   avoids 24 PolicyRule walks at enumeration time. The non-`system:`-
//   prefixed control-plane subjects (kubelet, kube-apiserver,
//   cluster-autoscaler, kubelet-bootstrap, kubelet-nodepool-bootstrap —
//   5 in the live corpus) take the full PolicyRule walk.
//
//   EMPTY-MATCHED-BINDINGS EDGE CASE — PRUNE (fail-closed). A User-kind
//   subject in userKeys with zero matched bindings can ONLY arrive via a
//   future indexer change that surfaces a subject without a paired
//   binding (today the snapshot indexer routes Subjects into the
//   per-name lists only through ADD, so a key in CRBsByUser ALWAYS has
//   at least one binding). Defensive prune: HG-178.5 reseed absorbs any
//   false-prune, no L1 cell is wasted on a subject that observably
//   carries no rules.
//
//   FAIL-CLOSED PRESERVED. (ζ) prunes only subjects that empirically
//   cannot read the snowplow corpus; every surviving cohort represents
//   a real user identity that issues /call traffic. Per-cohort seed
//   errors are real defects and propagate FAIL-CLOSED through
//   runPIPSeed (see internal/handlers/dispatchers/phase1_pip_seed.go,
//   Ship 0.30.183 reverts the 0.30.181 graceful-skip).
//
//   FUTURE TOPOLOGIES. A control-plane identity bound under a non-
//   `system:`-prefixed name AND granted a non-`system:` ClusterRole
//   whose Rules touch a `*.krateo.io` GVR would survive (ζ). That is
//   correct behaviour: the subject CAN access snowplow handlers, so
//   seeding its cohort is useful. If the seed then fails for unrelated
//   reasons (transport, snapshot-rebuild lag), FAIL-CLOSED surfaces
//   the defect — the loud-failure mode we want.
//
//   HANDLER GVR SET SCOPE. (ζ) compares rules against the
//   `*.krateo.io`-domain subset of `watcher.RegisteredGVRs()`. This
//   excludes the RBAC bootstrap informers (rbac.authorization.k8s.io/*)
//   and any non-handler informer wired for snapshot bookkeeping —
//   "snowplow handler GVRs" are by construction the krateo.io custom
//   resources the /call dispatcher routes through templates +
//   widgets + composition + core domains.
//
//   HANDLER-SET INVALIDATION. The handler GVR set is captured ONCE at
//   the start of each `EnumerateBindingSetClasses` call from the live
//   watcher. CRD lifecycle events that add/remove a handler GVR flip
//   `handlerGVRSetDirty` in rbac_snapshot.go; the Phase 1 PIP seed
//   driver checks the flag before each seed pass and re-enumerates on
//   the next iteration if it has flipped. The cost is one atomic store
//   per EnsureResourceType / RemoveResourceType call (free).
//
// PRUNE LOG. Every per-subject prune decision emits a `binding_set.prune`
// structured INFO log line with (subject_kind, name, matched_roles_count,
// matched_roles, reason). Customer-support can grep this to diagnose a
// missing-cohort report; absence of a log line for a missing cohort
// means the cohort was filtered earlier (snapshot indexer routed it
// elsewhere) or was never bound.

package cache

import (
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// systemAuthenticatedGroup is the canonical implicit-group name the
// upstream evaluator (evaluate.go:559-564) grants every authenticated
// request. Pruned from the powerset domain and INJECTED back into every
// tuple's groups at BindingSetHash time.
const systemAuthenticatedGroup = "system:authenticated"

// systemPrefix is the K8s reserved-name convention. Subject names that
// carry this prefix denote control-plane / framework identities (see
// SIG-Auth reserved-names guidance). Predicate (ζ) Ship 0.30.183 uses
// it as a pure-performance fast path (skip the PolicyRule walk for
// subjects whose name starts with `system:`; (ζ) would always reach
// PRUNE for them via empty-intersection anyway, but the fast path saves
// 24+ PolicyRule walks at enumeration time on the live GKE corpus).
const systemPrefix = "system:"

// krateoDomainSuffix is the snowplow handler GVR domain marker. The
// snowplow /call dispatcher routes ONLY krateo.io custom resources
// (templates.krateo.io, widgets.templates.krateo.io, composition.
// krateo.io, core.krateo.io). The handler GVR set used by predicate
// (ζ) is the `*.krateo.io`-domain subset of `RegisteredGVRs()`.
const krateoDomainSuffix = "krateo.io"

// roleRefKey identifies a unique (kind, namespace, name) tuple for a
// matched binding's RoleRef. CRB roleRefs always carry namespace="";
// RB roleRefs carry the RB's namespace when ref.Kind == "Role" and ""
// when ref.Kind == "ClusterRole". The tuple is the lookup key for
// PolicyRule resolution against the snapshot's ClusterRolesByName +
// RolesByNSName indexes.
type roleRefKey struct {
	Kind      string // "ClusterRole" or "Role"
	Namespace string // RB namespace for Kind=Role, "" otherwise
	Name      string
}

// pruneUserKindSubjectZeta reports whether a User-kind subject's
// matched bindings grant access to ANY snowplow handler GVR (predicate
// (ζ), Ship 0.30.183). Empty intersection ⇒ PRUNE.
//
//   - `system:`-prefix fast path: names starting with the K8s reserved
//     prefix prune without a PolicyRule walk (every observed subject
//     with this prefix prunes via the empty-intersection path anyway;
//     the fast path is pure performance).
//   - Subjects with ZERO matched bindings prune (fail-closed defensive
//     branch; today's indexer cannot surface such subjects, HG-178.5
//     reseed absorbs any future false-prune).
//   - For every other subject, walk the union of every matched
//     binding's RoleRef Rules and return KEEP iff some rule's
//     (APIGroups, Resources) overlaps some handler GVR. Empty
//     intersection ⇒ PRUNE.
//
// `refs` is the deduplicated set of (kind, namespace, name) RoleRef
// tuples matched for the subject (CRBs + per-namespace RBs). `snap`
// resolves the tuples to PolicyRule slices via ClusterRolesByName +
// RolesByNSName. `handlerGVRSet` is captured ONCE at enumeration start
// and passed as an immutable slice — no per-call lookup race.
//
// Wildcard semantics: stringSliceMatches mirrors the K8s authorizer
// (rbac/evaluate.go:519-526): "*" in rule.APIGroups OR rule.Resources
// matches everything. A cyberjoker-style role with APIGroups=["*"]
// Resources=["*"] therefore overlaps every handler GVR and survives
// regardless of the handler set.
//
// Per `feedback_no_special_cases`: predicate is uniform over every
// User-kind subject — no per-name hardcoding, no chart-tunable
// identity lists. Per `feedback_check_k8s_clientgo_prior_art`:
// PolicyRule intersection is the K8s authorizer's own semantics — we
// invoke the same `stringSliceMatches` helper (rbac/evaluate.go).
func pruneUserKindSubjectZeta(name string, refs []roleRefKey, snap *RBACSnapshot, handlerGVRSet []schema.GroupResource) bool {
	if strings.HasPrefix(name, systemPrefix) {
		return true // fast path: system:-prefix subjects always prune
	}
	if len(refs) == 0 {
		// Defensive fail-closed: a subject in userKeys with no matched
		// RoleRefs cannot serve any handler GVR. Today's indexer
		// guarantees at least one ref for every userKey, so this
		// branch is unreachable in production; a future indexer
		// change that surfaces an orphan would prune here and the
		// HG-178.5 reseed lifecycle would absorb the false-prune.
		return true
	}
	if snap == nil || len(handlerGVRSet) == 0 {
		// Defensive: handler set empty means no /call could ever match
		// — prune (fail-closed). Snapshot nil should never happen
		// because the caller already gated on rbacSnap.Load() != nil.
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

// stringSliceMatchesRBAC implements the K8s RBAC wildcard rule:
// "*" matches every value; otherwise an exact match is required.
// Byte-identical to rbac/evaluate.go:519-526 — duplicated here so the
// cache package does not import internal/rbac (which itself imports
// internal/cache, creating a cycle).
//
// Per `feedback_check_k8s_clientgo_prior_art`: the canonical RBAC
// wildcard semantics live in upstream Kubernetes
// (k8s.io/apiserver/pkg/registry/rbac/validation); we reimplement the
// 5-line helper locally rather than vendor the apiserver package.
func stringSliceMatchesRBAC(allowed []string, want string) bool {
	for _, a := range allowed {
		if a == "*" || a == want {
			return true
		}
	}
	return false
}

// collectMatchedRoleRefsForUser returns the deduplicated set of
// (kind, namespace, name) RoleRef tuples for every binding (CRB + RB
// across namespaces) whose Subjects include a User-kind entry with the
// given name. The result feeds `unionRulesForRefs` to resolve into a
// PolicyRule union.
//
// CRB roleRefs always carry Kind="ClusterRole" Namespace=""; RB
// roleRefs carry the RB's namespace when ref.Kind=="Role" and "" when
// ref.Kind=="ClusterRole" (a cluster-role can be referenced from any
// scope, the upstream RBAC authorizer permits it).
//
// Returns a nil slice when the subject is bound by no binding (the
// defensive empty-bindings branch in `pruneUserKindSubjectZeta`). The
// returned slice is sorted for stable log output.
func collectMatchedRoleRefsForUser(snap *RBACSnapshot, name string) []roleRefKey {
	if snap == nil {
		return nil
	}
	seen := map[roleRefKey]struct{}{}
	for _, crb := range snap.CRBsByUser[name] {
		if crb == nil {
			continue
		}
		// CRBs reference ClusterRoles only (rbac/evaluate.go:409-417).
		// The roleRefPermits switch arms a CRB ref against
		// ClusterRolesByName; Role-kind in a CRB is invalid per K8s.
		seen[roleRefKey{Kind: "ClusterRole", Namespace: "", Name: crb.RoleRef.Name}] = struct{}{}
	}
	for ns, inner := range snap.RBsByUserByNS {
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
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// unionRulesForRefs resolves each roleRefKey to its PolicyRule slice
// via the snapshot's ClusterRolesByName / RolesByNSName indexes and
// returns the union. A missed lookup is silently skipped — the same
// fail-closed posture rbac/evaluate.go:407-434 uses (a missed lookup
// is observability-only via RecordRBACSnapshotMiss; predicate (ζ)
// does NOT call RecordRBACSnapshotMiss because the enumerator runs
// off the hot path and a miss here is benign — the predicate treats
// the missing rules as empty, which only ever moves a subject toward
// PRUNE, never toward KEEP).
//
// The returned slice may contain duplicate PolicyRule values when two
// distinct roles share an identical rule; that is acceptable because
// predicate (ζ) only checks for the EXISTENCE of an overlapping rule
// — duplicates do not change the answer.
func unionRulesForRefs(snap *RBACSnapshot, refs []roleRefKey) []rbacv1.PolicyRule {
	if snap == nil || len(refs) == 0 {
		return nil
	}
	var out []rbacv1.PolicyRule
	for _, ref := range refs {
		switch ref.Kind {
		case "ClusterRole":
			cr, ok := snap.ClusterRolesByName[ref.Name]
			if !ok || cr == nil {
				continue
			}
			out = append(out, cr.Rules...)
		case "Role":
			r, ok := snap.RolesByNSName[ref.Namespace+"/"+ref.Name]
			if !ok || r == nil {
				continue
			}
			out = append(out, r.Rules...)
		}
	}
	return out
}

// handlerGVRSetSnapshot returns the `*.krateo.io`-domain subset of the
// live watcher's RegisteredGVRs as a deduplicated []GroupResource
// (version stripped — RBAC PolicyRules carry no version field). The
// returned slice is the predicate (ζ) input set; one call per
// `EnumerateBindingSetClasses` invocation. Returns nil when the
// watcher is unwired (cache=off / pre-readiness).
//
// The dedupe is necessary because two GVR versions of the same
// (group, resource) collapse to one GroupResource — RBAC rules don't
// distinguish them.
func handlerGVRSetSnapshot() []schema.GroupResource {
	// Reset the dirty flag FIRST. We are about to capture the current
	// set; any subsequent EnsureResourceType / RemoveResourceType
	// re-flips the bit so the next enumeration picks up the change.
	// Order matters: if we reset AFTER capturing and a lifecycle event
	// fires in between, the bit would be cleared and the change lost.
	handlerGVRSetDirty.Store(false)

	rw := Global()
	if rw == nil {
		handlerGVRCount.Store(0)
		return nil
	}
	gvrs := rw.RegisteredGVRs()
	if len(gvrs) == 0 {
		handlerGVRCount.Store(0)
		return nil
	}
	seen := map[schema.GroupResource]struct{}{}
	for _, gvr := range gvrs {
		if !strings.HasSuffix(gvr.Group, krateoDomainSuffix) {
			continue
		}
		seen[schema.GroupResource{Group: gvr.Group, Resource: gvr.Resource}] = struct{}{}
	}
	if len(seen) == 0 {
		handlerGVRCount.Store(0)
		return nil
	}
	out := make([]schema.GroupResource, 0, len(seen))
	for gr := range seen {
		out = append(out, gr)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].Resource < out[j].Resource
	})
	// Surface the count via expvar so post-ship operators can dump the
	// set authoritatively (HG-183.11). Always-stored, monotonically
	// updated; pre-readiness snapshots store 0 (the snapshot is nil
	// at that point but we still want the gauge live).
	handlerGVRCount.Store(int64(len(out)))
	return out
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
// Ship 0.30.183 (A.3-refine v3): User-kind subjects whose matched-
// binding rules have empty intersection with the snowplow handler GVR
// set are PRUNED at the userKeys collection step via predicate (ζ)
// `pruneUserKindSubjectZeta`.
//
// Ship 0.30.184 (ζ)-extend Group-kind: symmetric Group-kind predicate
// `pruneGroupKindSubjectZeta` (binding_set_enumeration_group.go) applies
// the same PolicyRule-intersection prune to Group-kind subjects at the
// groupKeys collection step. The Group-kind branch INTENTIONALLY OMITS
// the `system:`-prefix fast path so `system:masters` bound to
// cluster-admin (wildcard rule) survives as the admin cohort.
//
// The returned slice is sorted by (Username, Groups[0]) for stable
// log output and deterministic cohort ordering across pod restarts.
func EnumerateBindingSetClasses() []Cohort {
	snap := rbacSnap.Load()
	if snap == nil {
		return nil
	}

	log := slog.Default()

	// Capture the snowplow handler GVR set ONCE at enumeration start.
	// Passed as an immutable slice into every predicate (ζ) invocation
	// — no per-subject re-read, no race against EnsureResourceType /
	// RemoveResourceType mid-enumeration. The dirty flag drives
	// re-enumeration on the NEXT seed pass if the handler set changes
	// after this snapshot.
	handlerGVRSet := handlerGVRSetSnapshot()

	// Step 1 — userKeys = union of CRBsByUser ∪ ∪_ns RBsByUserByNS,
	// pruned by predicate (ζ) `pruneUserKindSubjectZeta`. The predicate
	// fires ONLY for User-kind subjects (Group-kind cohorts always
	// survive); per-prune INFO log lets customer-support grep
	// `binding_set.prune` to diagnose missing-cohort reports.
	candidateUsers := map[string]struct{}{}
	for u := range snap.CRBsByUser {
		candidateUsers[u] = struct{}{}
	}
	for _, inner := range snap.RBsByUserByNS {
		for u := range inner {
			candidateUsers[u] = struct{}{}
		}
	}
	userKeys := map[string]struct{}{}
	for u := range candidateUsers {
		refs := collectMatchedRoleRefsForUser(snap, u)
		pruned := pruneUserKindSubjectZeta(u, refs, snap, handlerGVRSet)
		if !pruned {
			userKeys[u] = struct{}{}
			continue
		}
		// PRUNED — emit the INFO log line. Reason is `system_prefix`
		// when the fast path fired; otherwise `empty_intersection`.
		reason := "empty_intersection"
		if strings.HasPrefix(u, systemPrefix) {
			reason = "system_prefix"
		} else if len(refs) == 0 {
			reason = "no_matched_bindings"
		} else if len(handlerGVRSet) == 0 {
			reason = "handler_set_empty"
		}
		roleNames := roleRefNamesForLog(refs)
		log.Info("binding_set.prune",
			slog.String("subsystem", "cache"),
			slog.String("subject_kind", "User"),
			slog.String("name", u),
			slog.String("reason", reason),
			slog.Int("matched_roles_count", len(refs)),
			slog.Any("matched_roles", roleNames),
		)
	}

	// Step 2 — groupKeys = union of CRBsByGroup ∪ ∪_ns RBsByGroupByNS,
	// pruning systemAuthenticatedGroup (implicit per evaluate.go:559)
	// AND predicate (ζ) Ship 0.30.184 (symmetric extension to User-kind):
	// Group-kind subjects whose matched-binding rules have empty
	// intersection with the snowplow handler GVR set are PRUNED. The
	// Group-kind predicate has NO `system:`-prefix fast path — it would
	// FALSE-PRUNE `system:masters` bound to cluster-admin (wildcard rule
	// → KEEP). See binding_set_enumeration_group.go for design rationale.
	candidateGroups := map[string]struct{}{}
	for g := range snap.CRBsByGroup {
		if g == systemAuthenticatedGroup || g == "" {
			continue
		}
		candidateGroups[g] = struct{}{}
	}
	for _, inner := range snap.RBsByGroupByNS {
		for g := range inner {
			if g == systemAuthenticatedGroup || g == "" {
				continue
			}
			candidateGroups[g] = struct{}{}
		}
	}
	groupKeys := map[string]struct{}{}
	for g := range candidateGroups {
		refs := collectMatchedRoleRefsForGroup(snap, g)
		pruned := pruneGroupKindSubjectZeta(g, refs, snap, handlerGVRSet)
		if !pruned {
			groupKeys[g] = struct{}{}
			continue
		}
		// PRUNED — emit the INFO log line with subject_kind="Group".
		// Reason classification mirrors the User-kind branch (sans
		// `system_prefix`, which the Group-kind predicate omits).
		reason := groupKindPruneReason(refs, handlerGVRSet)
		roleNames := roleRefNamesForLog(refs)
		log.Info("binding_set.prune",
			slog.String("subsystem", "cache"),
			slog.String("subject_kind", "Group"),
			slog.String("name", g),
			slog.String("reason", reason),
			slog.Int("matched_roles_count", len(refs)),
			slog.Any("matched_roles", roleNames),
		)
	}

	// Step 3 — build relevantGroups(u): the set of group names that
	// CO-OCCUR with u in some binding's Subjects. Walk every CRB +
	// every RB once; for each binding whose Subjects include a User
	// matching some u in userKeys, add every Group in the same Subjects
	// to relevantGroups[u]. ServiceAccount-kind subjects route to
	// CRBsByServiceAccount / RBsByServiceAccountByNS via the snapshot
	// indexer (not to CRBsByUser), so they never appear in userKeys and
	// the User-kind switch arm below transparently ignores them.
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

// roleRefNamesForLog projects a []roleRefKey down to the de-duplicated
// sorted list of role NAMES for the `binding_set.prune` INFO log's
// `matched_roles` field. Pure projection — used only for log output, no
// load-bearing semantics.
func roleRefNamesForLog(refs []roleRefKey) []string {
	if len(refs) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	for _, r := range refs {
		seen[r.Name] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
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
