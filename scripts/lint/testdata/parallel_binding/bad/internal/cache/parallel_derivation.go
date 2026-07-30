// scripts/lint/testdata/parallel_binding/bad/internal/cache/parallel_derivation.go
// — Ship H6 lint-gate BAD fixture (FAIL side of the dual-state proof for
// scripts/lint/no_parallel_binding_derivation.go).
//
// PURPOSE: this file reproduces the v3-baseline defect class the lint
// must catch — a BindingUID-equivalent derivation that iterates a
// snapshot subject index (snap.CRBsByUser) in a NON-allowlisted
// production file, projecting an identity->binding mapping OUTSIDE the
// single source of truth (internal/cache/match_subject.go). This is the
// exact shape of the deleted cohort_* files (binding_set_enumeration.go,
// cohort_ns_acl.go, rbac_cohorts.go) called out in the lint's dual-state
// comment.
//
// Its relative path from the fixture root is
// "internal/cache/parallel_derivation.go" — NOT in the fileAllowlist —
// so the lint MUST flag it (exit 1).
//
// The lint's BAD arm invokes:
//
//   go run scripts/lint/no_parallel_binding_derivation.go \
//     --root=$(pwd)/scripts/lint/testdata/parallel_binding/bad
//   # exit 1; violation reported for the snap.CRBsByUser touch below.
//
// `//go:build ignore` keeps `go build ./...` + `go vet ./...` away.

//go:build ignore

package cache

type snapshot struct {
	CRBsByUser      map[string][]int
	RBsByUserByNS   map[string]map[string][]int
	CRBsCatchAll    []int
	RBsCatchAllByNS map[string][]int
}

// deriveParallelBindingUID is the VIOLATION: it iterates snap.CRBsByUser
// to project a per-user binding identity outside the SOT. The lint flags
// the snap.CRBsByUser selector.
func deriveParallelBindingUID(snap *snapshot, user string) []int {
	// THE LINT MUST FLAG THIS snap.CRBsByUser touch.
	return snap.CRBsByUser[user]
}
