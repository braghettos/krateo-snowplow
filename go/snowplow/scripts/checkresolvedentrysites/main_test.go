// main_test.go — the L-SCOPE-COMPLETENESS gate's own discrimination proof (the
// RED arm the analysis requires: a deliberately-unbalanced fixture the checker
// must FLAG, so "the gate discriminates" is itself tested, not assumed).
//
// It runs the checker (compiled fresh via `go run .`) against:
//   - testdata/unbalanced — the #118d-shape fixture: two OFFENDING
//     ResolvedEntry Put sites (one missing TTLOverride + no waiver, one with an
//     empty-reason waiver) alongside two well-formed ones. The checker MUST
//     exit non-zero and name EXACTLY the two offending lines.
//
// This is the falsifier-must-actually-run discipline applied to the gate
// itself: if the checker's discrimination logic regresses (e.g. stops flagging
// a missing field), this test goes RED.
package main

import (
	"os/exec"
	"strings"
	"testing"
)

// TestGate_FlagsUnbalancedFixture is the RED arm: the checker must FLAG the
// unbalanced fixture (non-zero exit) and name the two offending sites, while
// NOT naming either well-formed site.
func TestGate_FlagsUnbalancedFixture(t *testing.T) {
	out, err := exec.Command("go", "run", ".", "testdata/unbalanced").CombinedOutput()
	if err == nil {
		t.Fatalf("GATE DID NOT DISCRIMINATE: checker exited 0 on the unbalanced fixture — it must FLAG the offending Put sites.\n%s", out)
	}
	s := string(out)

	// The two OFFENDING sites (by line) must be named.
	mustContain := []string{
		"put_sites.go:41", // missedSite — no field, no waiver
		"put_sites.go:50", // emptyReasonSite — waiver with no reason
	}
	for _, m := range mustContain {
		if !strings.Contains(s, m) {
			t.Errorf("expected the checker to flag %s; it did not.\nOutput:\n%s", m, s)
		}
	}

	// The two WELL-FORMED sites must NOT be named. Assert their exact Put-line
	// prefixes are absent (stampedSite sets TTLOverride; waivedSite carries a
	// valid waiver on the line above).
	mustNotContain := []string{
		"put_sites.go:22", // stampedSite Put line (sets TTLOverride)
		"put_sites.go:32", // waivedSite Put line (valid waiver above)
	}
	for _, m := range mustNotContain {
		if strings.Contains(s, m) {
			t.Errorf("checker FALSE-flagged a well-formed site %s (over-broad).\nOutput:\n%s", m, s)
		}
	}

	// It must diagnose BOTH offending kinds distinctly.
	if !strings.Contains(s, "does not set TTLOverride") {
		t.Errorf("expected the missing-field diagnostic; not found.\nOutput:\n%s", s)
	}
	if !strings.Contains(s, "waiver with NO reason") {
		t.Errorf("expected the empty-waiver-reason diagnostic; not found.\nOutput:\n%s", s)
	}
}
