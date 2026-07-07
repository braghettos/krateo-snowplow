// effective_key_extras_test.go — A1 byte-identical golden for the
// effectiveKeyExtras extraction (docs/definitive-cache-identity-architecture-2026-07-07.md
// §8.1-A1). A1 is a pure behavior-preserving refactor: effectiveKeyExtras must
// return output BYTE-IDENTICAL to the pre-A1 inline fold
// unionForKey(GetApiRefExtras(cr), GetResourcesRefsExtras(cr), request) at every
// one of the four routed sites, because the A2 identity-injection slot
// (declaredIdentityForKey) is INERT in A1 (returns nil). This is the "nothing
// moved" acceptance gate — no observable behavior is introduced.
//
// keyExtrasFor (extras_cache_key_test.go) IS the pre-A1 inline fold, so asserting
// effectiveKeyExtras == keyExtrasFor over representative corpus shapes is the
// direct byte-identical proof. The transitive coverage (Falsifier64/67,
// InlineExtras seed-parity, DedupKeyParity) exercises the four sites end-to-end;
// this arm pins the extraction's equivalence at the unit boundary + guards A2
// against silently changing A1 behavior (the injection must stay inert here).

package dispatchers

import (
	"reflect"
	"testing"
)

// TestA1_EffectiveKeyExtras_ByteIdenticalToInlineFold — the A1 nothing-moved
// golden across the corpus-representative extras shapes: no-extras, apiRef-inline
// only, rrt-inline only, both inline maps, request-extras only, and inline+request
// with a COLLISION (request must win — the pre-A1 precedence). At each shape the
// shared helper must deep-equal the pre-A1 inline fold.
func TestA1_EffectiveKeyExtras_ByteIdenticalToInlineFold(t *testing.T) {
	ctx := ctxWithIdentity()

	cases := []struct {
		name       string
		apiRefJSON string
		rrtJSON    string
		request    map[string]any
	}{
		{name: "no-extras (backward-compat empty fold)"},
		{name: "apiRef-inline only", apiRefJSON: `{"tenant":"acme"}`},
		{name: "rrt-inline only", rrtJSON: `{"region":"eu"}`},
		{name: "both inline maps", apiRefJSON: `{"tenant":"acme"}`, rrtJSON: `{"region":"eu"}`},
		{name: "request-extras only", request: map[string]any{"page": "detail"}},
		{
			name:       "inline + request, request wins on collision",
			apiRefJSON: `{"tenant":"acme","shared":"inline"}`,
			rrtJSON:    `{"region":"eu"}`,
			request:    map[string]any{"shared": "request", "q": "x"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cr := widgetCRWithExtras(t, tc.apiRefJSON, tc.rrtJSON)

			want := keyExtrasFor(cr, tc.request)           // the pre-A1 inline fold
			got := effectiveKeyExtras(ctx, cr, tc.request) // the A1 shared helper

			if !reflect.DeepEqual(got, want) {
				t.Fatalf("A1 NOT byte-identical: effectiveKeyExtras diverged from the pre-A1 unionForKey fold\n  got  = %#v\n  want = %#v", got, want)
			}
		})
	}
}

// TestA2_DeclaredIdentityForKey_WiresInjection — the A1 inert-slot guard,
// deliberately FLIPPED for A2 (the stray-early-wire guard becomes the wiring
// proof — per the definitive-arch A2 brief, converted not deleted). In A1 this
// asserted declaredIdentityForKey returns nil for a CR declaring identityContext;
// A2 WIRES the injection, so a declared CR under an identity-carrying ctx now
// materialises the declared subset of the principal, and effectiveKeyExtras folds
// it into the key (which the pure pre-A2 union does NOT). The inert half survives
// as the undeclared control (still nil → still byte-identical to pre-A2).
func TestA2_DeclaredIdentityForKey_WiresInjection(t *testing.T) {
	ctx := ctxWithIdentity() // cyberjoker / [devs]

	// DECLARED: spec.identityContext:[username,groups] → injection now materialises
	// {username: cyberjoker, groups: [devs]} from the ctx JWT (A2 wired).
	declaredCR := map[string]any{"spec": map[string]any{
		"identityContext": []any{"username", "groups"},
		"apiRef":          map[string]any{"extras": map[string]any{"k": "v"}},
	}}
	got := declaredIdentityForKey(ctx, declaredCR)
	// groups is JSON-native []any (NOT []string) — the A2 fix: identity extras
	// must be deep-copy-safe for the resolve-input path (mergeRequestWins →
	// plumbing DeepCopyJSON panics on []string). Key-parity byte-identical
	// (json.Marshal treats []string and []any the same), proven by the A1
	// byte-identical goldens above.
	want := map[string]any{"username": "cyberjoker", "groups": []any{"devs"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("A2: declaredIdentityForKey must materialise the declared identity subset from ctx; got %#v want %#v", got, want)
	}
	// effectiveKeyExtras folds the identity into the key (differs from the pure union).
	eff := effectiveKeyExtras(ctx, declaredCR, nil)
	if eff["username"] != "cyberjoker" {
		t.Fatalf("A2: effectiveKeyExtras must fold the declared username into the key; got %#v", eff)
	}
	if pure := keyExtrasFor(declaredCR, nil); reflect.DeepEqual(eff, pure) {
		t.Fatalf("A2: a DECLARED widget's key must DIFFER from the pure pre-A2 union (identity folded); both = %#v", eff)
	}

	// UNDECLARED control: no identityContext → still nil → key byte-identical to
	// the pure union (the prod-inert acceptance for the ~99% corpus survives).
	undeclaredCR := map[string]any{"spec": map[string]any{
		"apiRef": map[string]any{"extras": map[string]any{"k": "v"}},
	}}
	if got := declaredIdentityForKey(ctx, undeclaredCR); got != nil {
		t.Fatalf("A2: an UNDECLARED widget must inject nothing (nil); got %#v", got)
	}
	if eff, pure := effectiveKeyExtras(ctx, undeclaredCR, nil), keyExtrasFor(undeclaredCR, nil); !reflect.DeepEqual(eff, pure) {
		t.Fatalf("A2: an UNDECLARED widget's key must equal the pure union (prod-inert)\n  got  = %#v\n  want = %#v", eff, pure)
	}
}
