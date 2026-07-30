// groups_hash_test.go — M9 [SEC-adj] direct-unit falsifier for
// internal/rbac/groups_hash.go's canonicalGroupsHash, the single source
// of truth for the snapshot-authz memo key's GroupsHash dimension.
//
// TestL2_B3_GroupsHashCollisionAndOrder (snapshot_authz_memo_test.go)
// already exercises the hasher THROUGH EvaluateRBAC's observable verdict.
// This file asserts the hasher's contract DIRECTLY at the unit boundary
// via the rbac.CanonicalGroupsHashForTest hook — so a hasher regression
// is caught even if a future memo-key change stopped routing groups
// through it, and so the three properties that the behavioural test
// cannot cheaply separate (empty-vs-[""] distinctness, input-slice
// purity) are pinned explicitly.
//
// Contract (groups_hash.go):
//   1. length-prefix framing => h(["a","bc"]) != h(["ab","c"]) (the
//      0.30.239 naive-concat alias is impossible).
//   2. order independence     => h(["x","y"]) == h(["y","x"]).
//   3. empty-set stability     => h(nil) == h([]) != h([""]).
//   4. input purity            => the caller's slice is NOT mutated
//      (the impl sorts a COPY, never in place).
//
// evaltest package only — never `go test ./internal/rbac/...` against the
// remote kubeconfig (feedback_no_go_test_against_remote_kubeconfig). This
// test spins nothing (no watcher, no kind) — it is a pure function probe.

package evaltest

import (
	"reflect"
	"testing"

	"github.com/krateoplatformops/snowplow/internal/rbac"
)

// TestM9_CanonicalGroupsHash_Collision asserts the length-prefix framing
// defeats the classic partition alias: ["a","bc"] and ["ab","c"] both
// naive-concat to "abc" but are DIFFERENT sets and MUST hash differently.
// RED arm: a hasher that concatenates element bytes without a length
// prefix (or with a fixed separator that can appear in element data)
// aliases these two and this assertion fails.
func TestM9_CanonicalGroupsHash_Collision(t *testing.T) {
	h1 := rbac.CanonicalGroupsHashForTest([]string{"a", "bc"})
	h2 := rbac.CanonicalGroupsHashForTest([]string{"ab", "c"})
	if h1 == h2 {
		t.Fatalf("M9 COLLISION: h([\"a\",\"bc\"])=%d == h([\"ab\",\"c\"])=%d — the length-prefix framing failed to disambiguate the partition (0.30.239 naive-concat alias)", h1, h2)
	}

	// A second, adversarial partition pair to make the RED wider: a naive
	// concat with ANY single-byte separator that also appears in the data
	// would still alias one of these.
	h3 := rbac.CanonicalGroupsHashForTest([]string{"x", "yz"})
	h4 := rbac.CanonicalGroupsHashForTest([]string{"xy", "z"})
	if h3 == h4 {
		t.Fatalf("M9 COLLISION: h([\"x\",\"yz\"])=%d == h([\"xy\",\"z\"])=%d — partition alias not defeated", h3, h4)
	}
}

// TestM9_CanonicalGroupsHash_OrderIndependent asserts the hash is a SET
// hash: reordering the same members yields the SAME value (the impl sorts
// a copy first). RED arm: a hasher that folds groups in caller order
// (no sort) produces different values for ["x","y"] vs ["y","x"].
func TestM9_CanonicalGroupsHash_OrderIndependent(t *testing.T) {
	cases := [][2][]string{
		{{"x", "y"}, {"y", "x"}},
		{{"a", "bc", "def"}, {"def", "a", "bc"}},
		{{"system:authenticated", "admins"}, {"admins", "system:authenticated"}},
	}
	for _, c := range cases {
		h1 := rbac.CanonicalGroupsHashForTest(c[0])
		h2 := rbac.CanonicalGroupsHashForTest(c[1])
		if h1 != h2 {
			t.Fatalf("M9 ORDER: h(%v)=%d != h(%v)=%d — hash is not order-independent (missing sort)", c[0], h1, c[1], h2)
		}
	}
}

// TestM9_CanonicalGroupsHash_EmptyVsSingletonEmptyString asserts the
// empty-set stability contract: nil and [] hash identically (the empty
// stream), and BOTH differ from [""] (a one-element set whose single
// member is the empty string — which writes a length prefix of 0 followed
// by zero element bytes). RED arm: an impl that early-returns the empty
// hash for len==0 but forgets the length prefix on non-empty elements
// would collide h(nil) with h([""]) (both would hash "nothing").
func TestM9_CanonicalGroupsHash_EmptyVsSingletonEmptyString(t *testing.T) {
	hNil := rbac.CanonicalGroupsHashForTest(nil)
	hEmpty := rbac.CanonicalGroupsHashForTest([]string{})
	hEmptyStr := rbac.CanonicalGroupsHashForTest([]string{""})

	if hNil != hEmpty {
		t.Fatalf("M9 EMPTY: h(nil)=%d != h([])=%d — nil and empty slice must hash identically", hNil, hEmpty)
	}
	if hNil == hEmptyStr {
		t.Fatalf("M9 EMPTY: h(nil)=%d == h([\"\"])=%d — the empty SET and the one-element set {\"\"} must hash differently (length-prefix distinguishes them)", hNil, hEmptyStr)
	}
}

// TestM9_CanonicalGroupsHash_InputNotMutated asserts caller-slice purity:
// canonicalGroupsHash sorts a COPY, so an UNSORTED caller slice must be
// unchanged after the call. RED arm: an impl that sorts opts.Groups
// in place mutates the caller's slice — which in prod would silently
// reorder the auth layer's group slice (and could corrupt a concurrent
// reader). We pass a deliberately out-of-order slice and assert both the
// element order AND that the hash still matched its sorted twin.
func TestM9_CanonicalGroupsHash_InputNotMutated(t *testing.T) {
	in := []string{"zebra", "alpha", "mid"}
	before := make([]string, len(in))
	copy(before, in)

	_ = rbac.CanonicalGroupsHashForTest(in)

	if !reflect.DeepEqual(in, before) {
		t.Fatalf("M9 PURITY: canonicalGroupsHash mutated its input slice: got %v, want unchanged %v (impl must sort a copy, never in place)", in, before)
	}

	// Cross-check the mutation would have been observable via the hash's
	// order independence: the unsorted input and its explicit sorted form
	// hash equal, so the ONLY way to detect an in-place sort is the slice
	// contents above.
	sorted := []string{"alpha", "mid", "zebra"}
	if rbac.CanonicalGroupsHashForTest(in) != rbac.CanonicalGroupsHashForTest(sorted) {
		t.Fatalf("M9 PURITY sanity: unsorted input and its sorted twin must hash equal")
	}
}
