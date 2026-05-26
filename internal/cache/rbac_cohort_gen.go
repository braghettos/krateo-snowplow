// rbac_cohort_gen.go — Ship GMC.1 / 0.30.175.
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
//     under admin burst; if `fnv64aPointers` or `collectCohortBindingPtrs`
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
	"unsafe"

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
	key := CohortKeyHash(username, groups)

	stateAny, _ := cohortGenMap.LoadOrStore(key, &cohortGenState{})
	s := stateAny.(*cohortGenState)

	snap := rbacSnap.Load()
	if snap == nil {
		// No published snapshot — return whatever gen we already have
		// (0 on first call). Don't attempt to compute a hash against a
		// missing snapshot; the GMC memo's populate-time fail-closed
		// covers correctness here.
		return s.gen.Load()
	}

	ptrs := collectCohortBindingPtrs(snap, username, groups)
	h := fnv64aPointers(ptrs)

	if s.bindingsHash.Swap(h) != h {
		s.gen.Add(1)
	}
	return s.gen.Load()
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

// collectCohortBindingPtrs gathers the union of pointer-addresses of
// every (Cluster)RoleBinding this cohort matches in the published
// snapshot. The union is:
//
//   CRBsByUser[username] ∪
//     ∪_g CRBsByGroup[g] ∪
//     ∪_ns RBsByUserByNS[ns][username] ∪
//     ∪_ns ∪_g RBsByGroupByNS[ns][g]
//
// Returns uintptr values (stable for the lifetime of an immutable
// snapshot) — sortable and hashable by raw address. Pointer-set semantics
// align with EnumerateRBACCohorts' canonicalCohortKey.
//
// SA-kind bindings are NOT collected — the GMC gate memo runs on the
// resolver path where Identity is User+Groups (jwtutil.UserInfo). A user
// holding both a user-binding and a group-binding union both; SA
// identity is a separate code path (and a separate cohort).
func collectCohortBindingPtrs(
	snap *RBACSnapshot, username string, groups []string,
) []uintptr {
	if snap == nil {
		return nil
	}

	// Estimate: typical admin matches a handful of CRBs + a handful per
	// namespace of RBs. Allocate generously; the slice is throwaway.
	out := make([]uintptr, 0, 32)

	// User-kind cluster-wide bindings.
	if username != "" {
		for _, p := range snap.CRBsByUser[username] {
			if p == nil {
				continue
			}
			out = append(out, ptrAddr(p))
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
			out = append(out, ptrAddr(p))
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
				out = append(out, ptrAddr(p))
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
				out = append(out, ptrAddr(p))
			}
		}
	}

	return out
}

// ptrAddr is a tiny generic helper that returns a comparable uintptr from
// any pointer. The cast through unsafe.Pointer is the canonical pattern
// (same as rbac_cohorts.go:220).
//
// Inlinable; the snapshot's pointed-to objects are not relocated by the
// GC (they are reachable via the snapshot's strong references for the
// snapshot's lifetime), so the uintptr is stable until the next snapshot
// publishes and the old one becomes unreachable.
func ptrAddr[T any](p *T) uintptr {
	return uintptr(unsafe.Pointer(p))
}

// fnv64aPointers hashes a slice of pointer addresses into a single uint64
// via FNV-64a. The slice is sorted in place first so {a,b} and {b,a}
// hash to the same value — pointer-SET semantics, order-independent.
//
// Allocation profile: sort.Slice in place (no allocation); the 8-byte
// scratch buffer is stack-allocated. AC-GMC1.3 (hash cost <1%) is the
// gate; if pprof flags this in admin-cohort runs, follow-up ship adds
// snapshot-seq-keyed caching.
func fnv64aPointers(ptrs []uintptr) uint64 {
	if len(ptrs) == 0 {
		return fnv.New64a().Sum64()
	}
	sort.Slice(ptrs, func(i, j int) bool { return ptrs[i] < ptrs[j] })

	h := fnv.New64a()
	var buf [8]byte
	for _, a := range ptrs {
		// Encode uintptr as 8 little-endian bytes. uintptr is 8 bytes on
		// all supported platforms (linux/amd64, linux/arm64, darwin/arm64);
		// keep the encoding explicit for portability.
		u := uint64(a)
		buf[0] = byte(u)
		buf[1] = byte(u >> 8)
		buf[2] = byte(u >> 16)
		buf[3] = byte(u >> 24)
		buf[4] = byte(u >> 32)
		buf[5] = byte(u >> 40)
		buf[6] = byte(u >> 48)
		buf[7] = byte(u >> 56)
		_, _ = h.Write(buf[:])
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
