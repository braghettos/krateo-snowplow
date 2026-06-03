// scripts/lint/no_parallel_binding_derivation.go — Ship 0.30.242 H.c-layered
// Phase 3 F4 falsifier (go/ast lint).
//
// PURPOSE (design §4.3 + §13)
//
// The H.c-layered design positions internal/cache/match_subject.go as
// the SINGLE SOURCE OF TRUTH for BindingUID derivation from RBAC
// subject indexes. This lint enforces that contract: any production
// code (non-_test.go) that touches snap.CRBsBy* or snap.RBsBy* in a
// way that could project a BindingUID OUTSIDE the allowlist is a
// violation.
//
// FILE ALLOWLIST (production callers that have a legitimate reason
// to touch the snapshot indexes WITHOUT deriving a BindingUID):
//
//   internal/cache/rbac_snapshot.go — snapshot WRITER + writer diag
//     log lines (rebuildSubjectIndexes populates the maps; the log
//     lines emit len() counts). Identity-independent iteration.
//
//   internal/rbac/evaluate.go — selectCRBCandidates /
//     selectRBCandidates index pre-filter. The candidate fan-out
//     UNIONS bindings reachable by (Username, Groups, SA-kind) — it
//     does NOT produce a BindingUID; the BindingUID is produced
//     downstream by evaluateAgainstInformerFirstMatch via
//     cache.BindingUIDFromCRB / FromRB (the SOT) on the FIRST-MATCH
//     binding pointer.
//
// Everything else is a violation candidate. The lint reports
// file:line for each violation; exit 1 if any are found.
//
// DUAL-STATE PROOF (carries Phase 3 F4's falsifier discipline):
//
//   ✓ MUST PASS on current HEAD (74d5090) — no violations.
//   ✓ MUST FAIL on commit 9e4e3f8 (snowplow tag 0.30.235, the v3
//     baseline). v3 had cohort-derivation iterations in:
//       - internal/cache/binding_set_enumeration.go (deleted in 1d93d02)
//       - internal/cache/cohort_ns_acl.go (deleted in 1d93d02)
//       - internal/cache/rbac_cohorts.go (deleted in 1d93d02)
//       - internal/cache/rbac_cohort_gen.go (deleted in 2b commit a6ff9fd)
//     Each of these projected BindingUID-equivalent identities from snap
//     index iterations. Under the lint applied to 9e4e3f8's tree, these
//     4 files surface as violations — that's the FAIL side of the dual
//     state.
//
// USAGE
//
//   # PASS on clean tree (current HEAD):
//   go run scripts/lint/no_parallel_binding_derivation.go
//   # exit 0; no output
//
//   # FAIL on v3 baseline (snowplow tag 0.30.235, commit 9e4e3f8):
//   git worktree add /tmp/v3-baseline 9e4e3f8
//   go run scripts/lint/no_parallel_binding_derivation.go --root=/tmp/v3-baseline
//   # exit 1; violations list
//
// The lint is standalone (//go:build ignore so go build ./... skips
// it) — invoked via `go run`. Wire into CI via a Makefile target +
// Phase 4 pre-merge gate.

//go:build ignore

package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// fileAllowlist is the set of file paths (relative to project root)
// whose snap.CRBsBy* / snap.RBsBy* iterations are NOT BindingUID
// derivations. Every other file that touches these indexes in
// production code (non-_test.go) is a violation.
var fileAllowlist = map[string]struct{}{
	"internal/cache/rbac_snapshot.go": {},
	"internal/rbac/evaluate.go":       {},
}

// targetIdentifierPattern matches snapshot-index field selectors that
// project bindings keyed by subject identity:
//
//   CRBsByUser, CRBsByGroup, CRBsByServiceAccount, CRBsCatchAll
//   RBsByUserByNS, RBsByGroupByNS, RBsByServiceAccountByNS, RBsCatchAllByNS
//
// The CRBsCatchAll + RBsCatchAllByNS fields are unrecognised-Subject.Kind
// safety nets — touching them outside the allowlist is equally suspect
// (they too project a binding without going through the SOT).
var targetIdentifierPattern = regexp.MustCompile(`^(CRBsBy|RBsBy|CRBsCatchAll|RBsCatchAllByNS)`)

type violation struct {
	File string
	Line int
	Col  int
	Snip string
}

func main() {
	var (
		root    = flag.String("root", "", "project root; defaults to walking up from cwd until go.mod is found")
		verbose = flag.Bool("v", false, "verbose: log every file walked + decisions")
	)
	flag.Parse()

	if *root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "lint: cwd: %v\n", err)
			os.Exit(2)
		}
		*root = findProjectRoot(cwd)
		if *root == "" {
			fmt.Fprintf(os.Stderr, "lint: no go.mod found walking up from %s\n", cwd)
			os.Exit(2)
		}
	}

	internalDir := filepath.Join(*root, "internal")
	if _, err := os.Stat(internalDir); err != nil {
		fmt.Fprintf(os.Stderr, "lint: %s: %v\n", internalDir, err)
		os.Exit(2)
	}

	var violations []violation

	err := filepath.Walk(internalDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			if *verbose {
				fmt.Fprintf(os.Stderr, "lint: skip test file %s\n", path)
			}
			return nil
		}
		rel, relErr := filepath.Rel(*root, path)
		if relErr != nil {
			return relErr
		}
		if _, ok := fileAllowlist[rel]; ok {
			if *verbose {
				fmt.Fprintf(os.Stderr, "lint: skip allowlisted %s\n", rel)
			}
			return nil
		}

		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if parseErr != nil {
			fmt.Fprintf(os.Stderr, "lint: parse %s: %v\n", path, parseErr)
			os.Exit(2)
		}

		fileViolations := scanFile(fset, f, rel)
		violations = append(violations, fileViolations...)
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "lint: walk: %v\n", err)
		os.Exit(2)
	}

	if len(violations) == 0 {
		if *verbose {
			fmt.Fprintln(os.Stderr, "lint: PASS — no parallel BindingUID derivations found")
		}
		os.Exit(0)
	}

	sort.Slice(violations, func(i, j int) bool {
		if violations[i].File != violations[j].File {
			return violations[i].File < violations[j].File
		}
		return violations[i].Line < violations[j].Line
	})

	fmt.Fprintln(os.Stderr, "lint: FAIL — found parallel BindingUID derivation outside the SOT (internal/cache/match_subject.go).")
	fmt.Fprintln(os.Stderr, "lint: violations:")
	for _, v := range violations {
		fmt.Fprintf(os.Stderr, "  %s:%d:%d: snap.%s\n", v.File, v.Line, v.Col, v.Snip)
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "lint: FIX — route the derivation through cache.BindingUIDFromCRB /")
	fmt.Fprintln(os.Stderr, "lint:        BindingUIDFromRB (the SOT in internal/cache/match_subject.go),")
	fmt.Fprintln(os.Stderr, "lint:        OR add the file to fileAllowlist with a justification comment")
	fmt.Fprintln(os.Stderr, "lint:        (writer / candidate-fan-out / identity-independent iteration).")
	os.Exit(1)
}

// scanFile walks the file's AST looking for SelectorExpr nodes of the
// form X.Y where:
//
//   X is an Ident named "snap" (the canonical snapshot variable name
//     used throughout internal/cache and internal/rbac), AND
//   Y matches targetIdentifierPattern.
//
// We deliberately use the IDENT NAME "snap" rather than full type
// resolution because:
//   - Type resolution requires the full type-checker, an order-of-
//     magnitude more setup + dependencies.
//   - "snap" is the conventional name across both packages (TRACED in
//     rbac_snapshot.go, evaluate.go, every deleted cohort_* file).
//     Any rename to a different identifier would be flagged by
//     code-review independent of this lint.
//   - The cost of a false negative (a future caller using a different
//     variable name) is bounded — code review + Phase 3 F3 / F6 / F7
//     would surface the correctness defect.
//   - The cost of a false positive (a caller deliberately naming an
//     unrelated variable "snap") is acceptable — rename it.
func scanFile(fset *token.FileSet, f *ast.File, relPath string) []violation {
	var out []violation
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if ident.Name != "snap" {
			return true
		}
		if !targetIdentifierPattern.MatchString(sel.Sel.Name) {
			return true
		}
		pos := fset.Position(sel.Pos())
		out = append(out, violation{
			File: relPath,
			Line: pos.Line,
			Col:  pos.Column,
			Snip: sel.Sel.Name,
		})
		return true
	})
	return out
}

// findProjectRoot walks up from the given dir until it finds a
// go.mod; returns "" if none found at any ancestor.
func findProjectRoot(dir string) string {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
