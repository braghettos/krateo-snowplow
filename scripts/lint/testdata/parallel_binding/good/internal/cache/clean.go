// scripts/lint/testdata/parallel_binding/good/internal/cache/clean.go —
// Ship H6 lint-gate GOOD fixture (PASS side, no-target-pattern path).
//
// PURPOSE: a NON-allowlisted production-shaped file that simply does not
// touch any snap.CRBsBy* / snap.RBsBy* / snap.CRBsCatchAll /
// snap.RBsCatchAllByNS index. It exercises the lint's other PASS path:
// a file with no target pattern is never a violation, allowlist or not.
// Confirms the GOOD root stays clean even with a non-allowlisted file
// present.
//
// `//go:build ignore` keeps `go build ./...` + `go vet ./...` away.

//go:build ignore

package cache

// routeThroughSOT is the sanctioned derivation path: it calls the single
// source of truth (BindingUIDFromCRB) rather than iterating a snapshot
// subject index. No snap.CRBsBy* access at all — nothing for the lint to
// flag.
func routeThroughSOT(crb any) string {
	return bindingUIDFromCRB(crb)
}

func bindingUIDFromCRB(crb any) string {
	_ = crb
	return "uid"
}
