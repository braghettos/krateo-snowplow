// scripts/lint/testdata/parallel_binding/good/internal/cache/rbac_snapshot.go
// — Ship H6 lint-gate GOOD fixture (PASS side of the dual-state proof for
// scripts/lint/no_parallel_binding_derivation.go).
//
// PURPOSE: this file lives at the allowlisted relative path
// "internal/cache/rbac_snapshot.go" (see the lint's fileAllowlist at
// no_parallel_binding_derivation.go:81-84). It touches snap.CRBsByUser
// exactly the way the real snapshot WRITER does — identity-independent
// len()-style iteration — which is legitimate BECAUSE it is allowlisted.
// The lint MUST NOT flag it, so this GOOD root exits 0.
//
// Directory layout note: the lint's --root is the PROJECT ROOT and it
// walks <root>/internal. The fixture therefore nests the file under
// internal/cache/ so that filepath.Rel(root, path) == the allowlisted
// key. A sibling clean.go (no snap.CRBsBy* at all) exercises the other
// PASS path (no target pattern present).
//
// The lint's GOOD arm invokes:
//
//   go run scripts/lint/no_parallel_binding_derivation.go \
//     --root=$(pwd)/scripts/lint/testdata/parallel_binding/good
//   # exit 0; no violations
//
// `//go:build ignore` keeps `go build ./...` + `go vet ./...` away.

//go:build ignore

package cache

// snapshot mimics the fields the lint's targetIdentifierPattern matches.
type snapshot struct {
	CRBsByUser map[string][]int
}

// countBindings is the identity-independent iteration the WRITER file is
// allowed to do. It projects NO BindingUID — it only counts. Because
// this file is allowlisted, the lint tolerates the snap.CRBsByUser touch.
func countBindings(snap *snapshot) int {
	n := 0
	for range snap.CRBsByUser {
		n++
	}
	return n
}
