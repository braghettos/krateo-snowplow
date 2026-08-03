// scripts/lint/gate_test.go — H6 discrimination proof for the two
// orphaned-then-wired AST lint gates.
//
// WHY THIS FILE EXISTS (H6): the lint programs in this directory are
// //go:build ignore standalone `package main` programs, so `go build
// ./...` / `go vet ./...` skip them AND `go list ./scripts/lint/...`
// matches no package — the normal `go test ./...` coverage run never
// exercised the gate at all. This test is a NORMAL (non-ignore) test
// file, so `go test ./scripts/lint/...` now builds a test binary here
// and drives each lint program via `go run` over the good/bad fixtures.
// That makes the gate's discrimination itself a tested property, per
// the falsifier-must-actually-run discipline.
//
// EACH lint gets both arms:
//   - GOOD fixture  -> the lint MUST exit 0 (no false positives).
//   - BAD  fixture  -> the lint MUST exit non-zero AND name the
//     offending site (the RED arm: if the lint stops discriminating,
//     this goes RED).
//
// The BAD arm is the transient-neuter equivalent: the fixture IS the
// neutered/regressed state the lint must reject. Verified by hand
// (see the ship report) and re-verified on every coverage run.
//
// package main matches the sibling //go:build ignore lint programs'
// package declaration; because those are excluded by the build tag, the
// test binary is built from this file alone — no `func main` required.
package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// lintProgram is one //go:build ignore lint under scripts/lint/,
// resolved relative to this test's working directory (which `go test`
// sets to the package dir, i.e. scripts/lint/).
type lintCase struct {
	name string // subtest name
	// prog is the lint source file, relative to scripts/lint/.
	prog string
	// goodRoot / badRoot are --root values relative to scripts/lint/.
	goodRoot string
	badRoot  string
	// mustNameInBad is a substring the BAD-arm output MUST contain (the
	// offending site) — proves the lint pinpointed the violation, not
	// merely exited non-zero for an unrelated reason.
	mustNameInBad string
}

func lintCases() []lintCase {
	return []lintCase{
		{
			name:          "no_parallel_binding_derivation",
			prog:          "no_parallel_binding_derivation.go",
			goodRoot:      filepath.Join("testdata", "parallel_binding", "good"),
			badRoot:       filepath.Join("testdata", "parallel_binding", "bad"),
			mustNameInBad: "parallel_derivation.go",
		},
		{
			name:          "no_unchecked_unstructured_assert",
			prog:          "no_unchecked_unstructured_assert.go",
			goodRoot:      filepath.Join("testdata", "unchecked_assert", "good"),
			badRoot:       filepath.Join("testdata", "unchecked_assert", "bad"),
			mustNameInBad: "regression_unchecked_assert.go",
		},
	}
}

// runLint invokes `go run <prog> --root=<root>` from the scripts/lint/
// working dir and returns combined output + whether it exited zero.
func runLint(t *testing.T, prog, root string) (string, bool) {
	t.Helper()
	cmd := exec.Command("go", "run", prog, "--root="+root)
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

// TestLintGate_GoodFixturePasses is the PASS arm: each lint MUST exit 0
// on its GOOD fixture (no false positives). A regression that made the
// lint over-broad would red this.
func TestLintGate_GoodFixturePasses(t *testing.T) {
	for _, lc := range lintCases() {
		lc := lc
		t.Run(lc.name, func(t *testing.T) {
			out, ok := runLint(t, lc.prog, lc.goodRoot)
			if !ok {
				t.Fatalf("GATE FALSE-POSITIVE: %s exited non-zero on the GOOD fixture %s; it must PASS.\nOutput:\n%s",
					lc.prog, lc.goodRoot, out)
			}
		})
	}
}

// TestLintGate_BadFixtureFails is the RED arm: each lint MUST exit
// non-zero on its BAD fixture AND name the offending site. If the lint's
// discrimination logic (or its wiring) regresses so the BAD fixture
// passes, this test goes RED — exactly the H6 property being enforced.
func TestLintGate_BadFixtureFails(t *testing.T) {
	for _, lc := range lintCases() {
		lc := lc
		t.Run(lc.name, func(t *testing.T) {
			out, ok := runLint(t, lc.prog, lc.badRoot)
			if ok {
				t.Fatalf("GATE DID NOT DISCRIMINATE: %s exited 0 on the BAD fixture %s; it MUST FAIL.\nOutput:\n%s",
					lc.prog, lc.badRoot, out)
			}
			if !strings.Contains(out, lc.mustNameInBad) {
				t.Fatalf("%s failed the BAD fixture but did not name the offending site %q (wrong-reason failure?).\nOutput:\n%s",
					lc.prog, lc.mustNameInBad, out)
			}
		})
	}
}
