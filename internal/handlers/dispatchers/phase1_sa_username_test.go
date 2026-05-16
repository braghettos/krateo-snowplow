// phase1_sa_username_test.go — 0.30.108 falsifier for the Phase 1
// canonical-ServiceAccount-username derivation.
//
// THE BUG 0.30.108 FIXES (misdiagnosed across 0.30.105–107): Phase 1's
// resolution-context Username was the bare literal "snowplow-serviceaccount".
// rbac.EvaluateRBAC's ServiceAccount-kind subject matcher
// (parseServiceAccountUsername) requires the CANONICAL form
// `system:serviceaccount:<ns>:<name>`. A bare label has no
// `system:serviceaccount:` prefix → parse returns isSA=false → the
// ServiceAccount-kind subject on the snowplow SA's real ClusterRoleBinding
// (which grants `*/*` get/list/watch) can never match → EvaluateRBAC
// DENIES a fully-authorized SA. That is the navmenuitems rbac_dropped=3.
//
// THE FIX: phase1SAUsername decodes the canonical username from the SA
// token's JWT `sub` claim (the projected in-cluster token's `sub` IS
// exactly `system:serviceaccount:<ns>:<name>`). No ns/name Go literals.
//
// These tests assert: (1) a real-shaped SA JWT yields the canonical
// username verbatim; (2) the canonical form survives the same split
// rbac.parseServiceAccountUsername applies, so EvaluateRBAC's
// ServiceAccount-kind matcher would fire; (3) the pre-0.30.108 bare label
// is correctly rejected as non-canonical — a regression that reintroduces
// it fails here.

package dispatchers

import "testing"

// TestPhase1SAUsername_DecodesCanonicalFromJWT is the headline falsifier:
// the projected SA token's `sub` claim is the canonical username, and
// phase1SAUsername must return it verbatim.
func TestPhase1SAUsername_DecodesCanonicalFromJWT(t *testing.T) {
	// phase1RealSAToken (phase1_credential_real_test.go) is a real-shaped
	// SA JWT whose payload is {"sub":"system:serviceaccount:krateo-system:snowplow"}.
	const wantCanonical = "system:serviceaccount:krateo-system:snowplow"

	got, ok := phase1SAUsername(phase1RealSAToken)
	if !ok {
		t.Fatalf("phase1SAUsername(real SA token): ok=false — the canonical "+
			"username could not be decoded from the JWT `sub` claim")
	}
	if got != wantCanonical {
		t.Fatalf("phase1SAUsername: got %q; want the canonical JWT `sub` form %q",
			got, wantCanonical)
	}

	// The returned username MUST survive the same split the RBAC evaluator
	// applies (rbac.parseServiceAccountUsername) — otherwise EvaluateRBAC's
	// ServiceAccount-kind subject matcher cannot fire and the SA is denied.
	ns, name, isSA := splitCanonicalSAUsername(got)
	if !isSA {
		t.Fatalf("phase1SAUsername returned %q which the RBAC evaluator's "+
			"ServiceAccount matcher would NOT recognise — the 0.30.108 bug", got)
	}
	if ns == "" || name == "" {
		t.Fatalf("canonical username %q split to empty ns=%q name=%q", got, ns, name)
	}
}

// TestPhase1SAUsername_RejectsBareLabel is the regression guard for the
// 0.30.105–107 defect: a bare label with no `system:serviceaccount:`
// prefix is NOT a canonical username and must be rejected — it cannot be
// fed to the resolver context, because the RBAC evaluator would silently
// deny it.
func TestPhase1SAUsername_RejectsBareLabel(t *testing.T) {
	// "snowplow-serviceaccount" is not a JWT at all — ExtractUserInfo
	// fails — but the broader point is the same: any non-canonical string
	// must yield ok=false.
	if _, ok := phase1SAUsername("snowplow-serviceaccount"); ok {
		t.Fatalf("phase1SAUsername accepted the bare pre-0.30.108 label — a "+
			"regression: the RBAC evaluator would silently deny it")
	}
	if _, ok := phase1SAUsername(""); ok {
		t.Fatalf("phase1SAUsername accepted an empty token")
	}
}

// TestSplitCanonicalSAUsername mirrors rbac.parseServiceAccountUsername so
// the two stay in lockstep — phase1SAUsername relies on this split to
// VERIFY the JWT-decoded subject is the form EvaluateRBAC will match.
func TestSplitCanonicalSAUsername(t *testing.T) {
	cases := []struct {
		in       string
		wantNS   string
		wantName string
		wantOK   bool
	}{
		{"system:serviceaccount:krateo-system:snowplow", "krateo-system", "snowplow", true},
		{"system:serviceaccount:ns:name", "ns", "name", true},
		{"snowplow-serviceaccount", "", "", false},          // the 0.30.105–107 bug
		{"system:serviceaccount:", "", "", false},           // no ns/name
		{"system:serviceaccount:onlyns", "", "", false},     // no name segment
		{"system:serviceaccount::name", "", "", false},      // empty ns
		{"system:serviceaccount:ns:", "", "", false},        // empty name
		{"", "", "", false},
	}
	for _, tc := range cases {
		ns, name, ok := splitCanonicalSAUsername(tc.in)
		if ok != tc.wantOK || ns != tc.wantNS || name != tc.wantName {
			t.Errorf("splitCanonicalSAUsername(%q) = (%q,%q,%v); want (%q,%q,%v)",
				tc.in, ns, name, ok, tc.wantNS, tc.wantName, tc.wantOK)
		}
	}
}
