// rbac_cohort_gen.go — Ship GMC.1 / 0.30.175;
//   Ship 1 / 0.30.195 — UID-stable cohort identity (root-cause fix for
//   cohort-key drift).
//
// SHIP 1 / 0.30.195 — POINTER-ADDRESS → metadata.uid
//   The pre-0.30.195 identity hashed raw pointer ADDRESSES of the matched
//   bindings (`ptrAddr` → `fnv64aPointers`). The informer rebuilds those
//   pointers fresh on EVERY RBAC snapshot republish (~4.6/s churn,
//   `rebuildRBACSnapshot`), so the pointer-set hash drifted across
//   generations even when the LOGICAL binding membership was byte-
//   identical. A prewarm-seed key computed in one generation then
//   diverged from the dispatch key computed in a later generation, and
//   the prewarmed L1 cell was unreachable.
//   FIX: hash the binding's IMMUTABLE `metadata.uid` (the SORTED SET of
//   UIDs of the matched bindings) instead of the pointer address. UIDs
//   survive relist, so seed-time and dispatch-time hashes are byte-
//   identical across generations. The three identity consumers
//   (BindingSetHash, CohortRBACGen, canonicalCohortKey in
//   rbac_cohorts.go) all route through the UID-based identity helpers
//   (collectCohortBindingIDs / crbIdentity / rbIdentity / fnv64aIdentities)
//   so seed and dispatch never disagree. The membership SET is unchanged
//   (UID-set == pointer-set membership; only the encoding changes), so
//   the cohort is not widened — RBAC correctness is preserved and the
//   per-request content gate (apistage.go) is untouched.
//
// Per-cohort RBAC generation. Replaces the global cache.RBACGen() stamp
// the Ship GMC (0.30.174) gate memo used: any single RBAC mutation
// (subject add/remove on ANY binding) bumped the global counter, which
// invalidated EVERY cohort's gate memo — even cohorts whose own matched
// binding-pointer-set was unchanged. At admin scale (10K-50K bindings
// churning under controller traffic) the global stamp turned the GMC
// memo into a near-miss machine: admin's burst hit-rate degraded from
// the design's "≥9/10 on a 10-call burst" toward zero (HG-GMC.1 cold ≤2s
// regression caught at production scale).
//
// THE FIX
//   - Per-cohort generation: a cohort's `gen` bumps ONLY when the
//     pointer-set of (Cluster)RoleBindings that cohort matches changes.
//   - Mutations that don't touch a cohort's matched-binding-set leave
//     that cohort's gen untouched — gate memos stay valid; admin's burst
//     hit-rate stays ≥9/10.
//   - Cohorts whose matched-binding-set DID change still see their memo
//     invalidated at next call — correctness preserved.
//
// MECHANISM (architect-approved, ship without hash caching — Diego
// ratified 2026-05-22, sanity check via pprof per AC-GMC1.3):
//
//   1. Cohort key derived from (sorted(Groups), Username) — same shape
//      as `CohortKeyHash` so the GMC memo store and this generator
//      cohort-agree by identity.
//   2. Per-cohort state lives in a `sync.Map` keyed by cohort-key
//      string -> *cohortGenState. State has TWO atomics: the current
//      generation number + the last observed pointer-set hash.
//   3. On every call: load the LIVE snapshot, collect the union of
//      pointer-addresses of bindings this cohort matches, hash the
//      sorted pointer-address sequence via FNV-64a, compare to the
//      previously stored hash; if changed bump `gen` and store the
//      new hash. Return the current `gen`.
//   4. Lazy recompute (no informer-side push). The snapshot is published
//      atomically, so two callers observing the same snapshot compute
//      the same hash; their gen reads converge.
//
// CORRECTNESS — pointer-set semantics:
//   - A binding ADD where the new binding's Subjects matches the cohort
//     appears as a NEW pointer in the snapshot's CRBsByUser /
//     RBsByUserByNS / etc. lookup. The cohort's pointer-set gains an
//     element, hash differs, gen bumps. Cohorts not matching the new
//     binding keep an unchanged pointer-set, no bump.
//   - A binding DELETE removes a pointer from the cohort's set, hash
//     differs, gen bumps. Non-matching cohorts unaffected.
//   - A binding UPDATE (subject list edited) produces a NEW typed
//     pointer in the snapshot — every cohort that USED to match the
//     pre-edit pointer sees the pointer disappear; every cohort that
//     NOW matches the post-edit pointer sees a new pointer. Both sides
//     bump. Cohorts on the unchanged side of the edit see no change.
//   - Snapshot is immutable post-publish (rbac_snapshot.go:36-51), so
//     pointer addresses are stable for the lifetime of the published
//     snapshot. uintptr comparison is load-bearing — same contract as
//     EnumerateRBACCohorts' canonicalCohortKey (rbac_cohorts.go:209).
//
// CONCURRENCY:
//   - cohortGenMap is sync.Map — safe under concurrent Load/Store.
//   - Per-cohortGenState mutates via atomic.Uint64.Swap (hash) +
//     atomic.Uint64.Add (gen). The gen-bump is racy across goroutines
//     observing the SAME hash change — multiple callers may each Swap
//     and observe a different prior hash, leading to N gen-bumps for
//     1 logical pointer-set change. That's BENIGN: the contract is
//     "gen changes when pointer-set changes", not "gen changes by
//     exactly 1 per pointer-set change". The GMC memo only cares about
//     equality of stamp to current gen — N-bumps still invalidate stale
//     memos correctly.
//
// SHIP PARAMETERS — no special cases:
//   - feedback_no_special_cases.md compliant: cohort identity is
//     {Username, Groups} hashed by the same `CohortKeyHash`; no
//     hardcoded admin / system:masters branches.
//   - feedback_l1_per_user_keyed_never_cohort.md compliant: this
//     generator decides ONLY when to invalidate a per-cohort gate-memo
//     stamp. The L1 RESOLVED cache stays per-user-keyed.
//   - feedback_shared_vs_copy_is_a_concurrency_change.md compliant:
//     the published RBACSnapshot is read-only post-publish; we extract
//     uintptr values, never mutate the pointed-to objects.
//
// SANITY CHECK (AC-GMC1.3):
//   - Hash function MUST NOT appear in pprof top-10, OR if it does,
//     cumulative self-time MUST be <1%. Tester samples 30s CPU profile
//     under admin burst; if `fnv64aIdentities` or `collectCohortBindingIDs`
//     break that ceiling, follow-up ship adds hash caching keyed on
//     snapshot publish-seq.

package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"hash/fnv"
	"sort"
	"sync"
	"sync/atomic"

	rbacv1 "k8s.io/api/rbac/v1"
)

// cohortGenState is the per-cohort generation tracker. `gen` is the
// monotonically-non-decreasing counter the GMC memo stamps; `bindingsHash`
// records the FNV-64a hash of the cohort's sorted binding-pointer-set
// from the last call so the next call can compare equality.
type cohortGenState struct {
	gen          atomic.Uint64
	bindingsHash atomic.Uint64
}

// cohortGenMap is the process-wide registry of per-cohort generators.
// Keyed by the cohort identity string from `CohortKeyHash`. Values are
// `*cohortGenState`. sync.Map is used so the first-call install path
// (LoadOrStore) is lock-free for warm callers.
var cohortGenMap sync.Map // cohortKey (string) -> *cohortGenState

// CohortRBACGen returns the current generation for the (username, groups)
// cohort. The generation is bumped lazily: each call collects the cohort's
// matched-binding-pointer-set from the LIVE published RBAC snapshot, hashes
// the sorted pointer addresses, and compares to the previously stored hash.
// A change bumps `gen`. Unchanged sets leave `gen` untouched.
//
// Contract with the GMC gate memo (apistage_cohort_memo.go:142): callers
// stamp the memo with the value returned here and re-fetch on every
// memo-hit check. A stamp != live gen invalidates the memo and re-runs
// the per-item filter, which then re-stamps with the new gen.
//
// Snapshot=nil (pre-readiness / cache=off) returns the cohort's last
// observed gen (0 on first call). The GMC memo's hit-path will fail-closed
// at populate time anyway (filterListByRBAC returns identityOK=false
// when EvaluateRBAC degrades to deny without a snapshot).
func CohortRBACGen(username string, groups []string) uint64 {
	snap := rbacSnap.Load()
	// Ship 0.30.189 — normalise group-only identity to the sentinel so
	// CohortRBACGen partitions match the seed-time / dispatcher-time
	// hash partition. Real users with zero User-kind bindings collapse
	// to a single cohort gen stamp keyed by the sentinel; users with
	// User-kind bindings keep their own gen. CohortKeyHash is left
	// untouched downstream (memo partition is correctness-bound at
	// gen-bump, not key partition) — see project_0_30_189_design.
	username = normalizeIdentityForCohort(snap, username)

	key := CohortKeyHash(username, groups)

	stateAny, _ := cohortGenMap.LoadOrStore(key, &cohortGenState{})
	s := stateAny.(*cohortGenState)

	if snap == nil {
		// No published snapshot — return whatever gen we already have
		// (0 on first call). Don't attempt to compute a hash against a
		// missing snapshot; the GMC memo's populate-time fail-closed
		// covers correctness here.
		return s.gen.Load()
	}

	ids := collectCohortBindingIDs(snap, username, groups)
	h := fnv64aIdentities(ids)

	if s.bindingsHash.Swap(h) != h {
		s.gen.Add(1)
	}
	return s.gen.Load()
}

// BindingSetHash — Ship A.3 / 0.30.179 — returns the FNV-64a hash of
// the cohort's matched RBAC binding-pointer-set. Used by the L1 cache
// key (dispatchCacheLookupKey via ResolvedKeyInputs.BindingSetHash) to
// fold per-user identity into a per-COHORT cell: two users whose
// binding-set is pointer-equal share an L1 entry.
//
// MECHANISM
//   - Load the live published snapshot via rbacSnap.Load().
//   - Inject the implicit `system:authenticated` group for any non-empty
//     username. The evaluator (evaluate.go:333-338, evaluate.go:559-564)
//     adds CRBsByGroup["system:authenticated"] for every authenticated
//     request REGARDLESS of whether the request's UserInfo.Groups
//     contains it. BindingSetHash MUST mirror that behaviour or the
//     seed-time hash (which enumerates the implicit group) would
//     diverge from the request-time hash (where the JWT may omit it).
//   - Collect the cohort's matched binding-identity set via
//     collectCohortBindingIDs (Ship 1 / 0.30.195 — metadata.uid, not
//     pointer address).
//   - Hash the union identity-set via fnv64aIdentities (FNV-64a;
//     SET semantics, order-independent).
//
// EMPTY SNAPSHOT — returns 0 when no snapshot has been published
// (degrade-to-deny posture; the cache-key caller sees zero identity-
// fold and the entry collapses with every other zero-snapshot caller
// for the same GVR/ns/name). The serve-time RBAC gate fails closed at
// EvaluateRBAC anyway so a wrong-identity hit is impossible — but the
// cell would not be reusable across cohorts, which is the same shape
// as the pre-A.3 per-user keying.
//
// CORRECTNESS — pointer-set semantics carry over from CohortRBACGen
// (rbac_cohort_gen.go:118-153): two cohorts whose binding-sets are
// pointer-equal hash to the same value, regardless of group ordering
// or username string differences. A binding ADD / DELETE / UPDATE
// touching this cohort's matched set produces a different pointer-set
// and thus a different hash — the next call from this cohort lands on
// a fresh L1 cell.
//
// HG-178.5 falsifier: a binding mutation against an enumerated cohort
// MUST shift this hash for that cohort; the next /call MISSes and the
// reseed populates the new cell.
//
// CONCURRENCY — lock-free. rbacSnap.Load is atomic; the snapshot is
// immutable post-publish (rbac_snapshot.go:36-51); collectCohortBindingIDs
// + fnv64aIdentities are pure functions.
func BindingSetHash(username string, groups []string) uint64 {
	snap := rbacSnap.Load()
	if snap == nil {
		return 0
	}
	// Ship 0.30.189 — normalise group-only identity to the sentinel
	// BEFORE injecting system:authenticated + collecting pointers. The
	// PIP seed enumerator emits the sentinel for the group-only cohort
	// (binding_set_enumeration.go:319); the dispatcher's request-time
	// call arrives with the real username (e.g. "cyberjoker"). Both
	// callers reach this line; both normalise to the same sentinel
	// when the user has zero User-kind bindings → the hashed pointer-
	// set is identical → the L1 cell the seed populated is the cell
	// the dispatcher reads. Users WITH User-kind bindings (admin)
	// pass through unchanged; their own per-user cohort is preserved.
	username = normalizeIdentityForCohort(snap, username)

	// Inject implicit system:authenticated for authenticated requests —
	// mirrors evaluate.go's behaviour. We MUST do this here (not just at
	// seed enumeration time) so request-time + seed-time hashes match
	// byte-for-byte regardless of whether the JWT carried the implicit
	// group in its Groups claim.
	//
	// After 0.30.189 normalisation `username != ""` now also covers the
	// sentinel, which is correct: the group-only cohort IS an
	// authenticated identity (real authenticated users with zero User-
	// kind bindings) and so MUST carry system:authenticated through the
	// hash.
	effective := groups
	if username != "" {
		hasAuth := false
		for _, g := range groups {
			if g == systemAuthenticatedGroup {
				hasAuth = true
				break
			}
		}
		if !hasAuth {
			effective = make([]string, 0, len(groups)+1)
			effective = append(effective, groups...)
			effective = append(effective, systemAuthenticatedGroup)
		}
	}
	ids := collectCohortBindingIDs(snap, username, effective)
	return fnv64aIdentities(ids)
}

// CohortKeyHash hashes (sorted(groups), username) into a stable cohort
// identity string. Exported so the GMC memo (api/apistage_cohort_memo.go)
// and this per-cohort generator can compute the SAME key for the SAME
// identity — the two layers cohort-agree by construction.
//
// Hash: SHA-256 truncated to 16 hex chars (8 bytes). Same shape and
// algorithm the 0.30.174 GMC memo used pre-move (the move into cache/
// preserves byte-identical keys across the GMC and CohortRBACGen layers).
//
// Empty username + empty groups yields a deterministic key — anonymous
// requests cohort together. Callers that need to bypass the memo on a
// missing identity do so BEFORE calling this (GMC memo checks ui at
// apistage_cohort_memo.go:131).
func CohortKeyHash(username string, groups []string) string {
	sortedGroups := append([]string(nil), groups...)
	sort.Strings(sortedGroups)

	h := sha256.New()
	// Version prefix so a future key-shape change rotates the cohort
	// space cleanly across rolling restarts.
	h.Write([]byte("cohort-v1"))
	h.Write([]byte{0})
	h.Write([]byte(username))
	h.Write([]byte{0})
	for _, g := range sortedGroups {
		h.Write([]byte(g))
		h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:8])
}

// collectCohortBindingIDs gathers the union of STABLE binding identities
// of every (Cluster)RoleBinding this cohort matches in the published
// snapshot. The union is:
//
//   CRBsByUser[username] ∪
//     ∪_g CRBsByGroup[g] ∪
//     ∪_ns RBsByUserByNS[ns][username] ∪
//     ∪_ns ∪_g RBsByGroupByNS[ns][g]
//
// Returns a slice of stable identity strings — sortable and hashable as a
// SET. Identity is the binding's `metadata.uid`, which is IMMUTABLE across
// relist / snapshot republish (Ship 1 / 0.30.195). This is the root-cause
// fix for cohort-key drift: the pre-0.30.195 code hashed raw pointer
// ADDRESSES (`ptrAddr`), which the informer rebuilds fresh on every RBAC
// snapshot republish (~4.6/s churn). The pointer-set hash therefore
// drifted across generations even when the logical binding membership was
// byte-identical, so a prewarm-seed key diverged from the dispatch key and
// the prewarmed cell was unreachable. Hashing the UID makes the seed-time
// and dispatch-time hashes byte-identical ACROSS generations.
//
// EMPTY-UID FALLBACK (review-required, CLAIM 5):
//   Synthetic / hand-built bindings (e.g. fakes, some test fixtures, and
//   theoretically a control-plane object before the apiserver stamps a
//   UID) can have an empty `metadata.uid`. We MUST NOT collapse all
//   empty-UID bindings to a single shared zero-bucket — that would make
//   two DISTINCT empty-UID bindings hash-collide into one cohort identity,
//   which is an RBAC over-collapse (cross-binding leak). Instead, for a
//   binding with empty UID we fall back to a stable per-binding identifier
//   built from its content tuple: a Kind tag + namespace + name +
//   roleRef(apiGroup/kind/name). This tuple is stable across republishes
//   (it is derived from the binding's own immutable spec fields, not its
//   address) and distinguishes two empty-UID bindings whose name or
//   roleRef differs. Two empty-UID bindings that are genuinely identical
//   in every tuple field DO collapse — but that is correct: they are
//   indistinguishable to the RBAC evaluator and produce the same verdict.
//
// SA-kind bindings are NOT collected — the GMC gate memo runs on the
// resolver path where Identity is User+Groups (jwtutil.UserInfo). A user
// holding both a user-binding and a group-binding union both; SA
// identity is a separate code path (and a separate cohort).
func collectCohortBindingIDs(
	snap *RBACSnapshot, username string, groups []string,
) []string {
	if snap == nil {
		return nil
	}

	// Estimate: typical admin matches a handful of CRBs + a handful per
	// namespace of RBs. Allocate generously; the slice is throwaway.
	out := make([]string, 0, 32)

	// User-kind cluster-wide bindings.
	if username != "" {
		for _, p := range snap.CRBsByUser[username] {
			if p == nil {
				continue
			}
			out = append(out, crbIdentity(p))
		}
	}

	// Group-kind cluster-wide bindings.
	for _, g := range groups {
		if g == "" {
			continue
		}
		for _, p := range snap.CRBsByGroup[g] {
			if p == nil {
				continue
			}
			out = append(out, crbIdentity(p))
		}
	}

	// User-kind per-namespace RoleBindings — iterate every namespace
	// the snapshot tracks.
	if username != "" {
		for _, inner := range snap.RBsByUserByNS {
			for _, p := range inner[username] {
				if p == nil {
					continue
				}
				out = append(out, rbIdentity(p))
			}
		}
	}

	// Group-kind per-namespace RoleBindings.
	for _, g := range groups {
		if g == "" {
			continue
		}
		for _, inner := range snap.RBsByGroupByNS {
			for _, p := range inner[g] {
				if p == nil {
					continue
				}
				out = append(out, rbIdentity(p))
			}
		}
	}

	return out
}

// crbIdentity returns the stable cohort-identity string for a
// ClusterRoleBinding: its `metadata.uid` when present, else the empty-UID
// fallback tuple (see collectCohortBindingIDs CLAIM-5 note). The "C:"
// Kind tag namespaces ClusterRoleBinding identities away from RoleBinding
// identities so a CRB and an RB can never alias on an identical tuple.
func crbIdentity(p *rbacv1.ClusterRoleBinding) string {
	if uid := string(p.UID); uid != "" {
		return "u:" + uid
	}
	// Empty-UID fallback: stable per-binding tuple (no namespace for
	// cluster-scoped CRBs). roleRef is part of the binding's effective
	// grant, so two CRBs with the same name but different roleRef stay
	// distinct.
	return "C:" + p.Name +
		"\x1f" + p.RoleRef.APIGroup + "/" + p.RoleRef.Kind + "/" + p.RoleRef.Name
}

// rbIdentity returns the stable cohort-identity string for a RoleBinding:
// its `metadata.uid` when present, else the empty-UID fallback tuple. The
// "R:" Kind tag + namespace keep RoleBinding identities distinct from
// ClusterRoleBindings and from same-name RoleBindings in other namespaces.
func rbIdentity(p *rbacv1.RoleBinding) string {
	if uid := string(p.UID); uid != "" {
		return "u:" + uid
	}
	return "R:" + p.Namespace + "/" + p.Name +
		"\x1f" + p.RoleRef.APIGroup + "/" + p.RoleRef.Kind + "/" + p.RoleRef.Name
}

// fnv64aIdentities hashes a slice of stable binding-identity strings into
// a single uint64 via FNV-64a. The slice is sorted first so {a,b} and
// {b,a} hash to the same value — SET semantics, order-independent. A
// 0-byte separator is written between identities so a single
// concatenation can never alias a different multi-set partition (e.g.
// {"ab","c"} vs {"a","bc"}).
//
// Allocation profile: sort.Strings in place (no allocation beyond the
// sort's internal handling); the FNV hasher is a stack-friendly struct.
// AC-GMC1.3 (hash cost <1%) carries over from the pointer-set hash; if
// pprof flags this in admin-cohort runs, a follow-up ship caches the
// result keyed on snapshot publish-seq.
func fnv64aIdentities(ids []string) uint64 {
	if len(ids) == 0 {
		return fnv.New64a().Sum64()
	}
	sort.Strings(ids)

	h := fnv.New64a()
	for _, id := range ids {
		_, _ = h.Write([]byte(id))
		// Field separator — guards against concatenation aliasing across
		// different set partitions.
		_, _ = h.Write([]byte{0})
	}
	return h.Sum64()
}

// resetCohortGenMapForTest clears the package-level cohort generator
// state. Used by tests that need a clean slate between runs (e.g. to
// observe the gen=0 → gen=1 transition without inheriting a previous
// test's bump). Production code MUST NOT call this — it would
// invalidate every live gate-memo stamp at once.
func resetCohortGenMapForTest() {
	cohortGenMap.Range(func(k, _ any) bool {
		cohortGenMap.Delete(k)
		return true
	})
}

// Compile-time sanity: rbacv1 import must be referenced so the file
// compiles standalone if someone strips the helper functions. The
// reference is satisfied by the function signatures below; this line
// is a no-op marker.
var _ = (*rbacv1.ClusterRoleBinding)(nil)
