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

// TestA1_DeclaredIdentityForKey_InertInA1 — pins that the A2 injection slot is
// INERT in A1: declaredIdentityForKey returns nil regardless of ctx/CR, so
// effectiveKeyExtras adds nothing beyond the union. If A2 wiring lands, THIS test
// is the one that must be deliberately updated — a guard that A1 shipped with the
// slot provably off (and that a stray early-wire is caught).
func TestA1_DeclaredIdentityForKey_InertInA1(t *testing.T) {
	ctx := ctxWithIdentity()
	// Even a CR that (in A2) would declare identityContext must yield nil in A1 —
	// the accessor does not exist yet, so any CR shape returns nil.
	cr := map[string]any{"spec": map[string]any{
		"identityContext": []any{"username", "groups"},
		"apiRef":          map[string]any{"extras": map[string]any{"k": "v"}},
	}}
	if got := declaredIdentityForKey(ctx, cr); got != nil {
		t.Fatalf("A1: declaredIdentityForKey must be INERT (nil) until A2 wires it; got %#v", got)
	}
	// And effectiveKeyExtras over that CR equals the pure union (no identity keys).
	want := keyExtrasFor(cr, nil)
	got := effectiveKeyExtras(ctx, cr, nil)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("A1: with the injection inert, effectiveKeyExtras must equal the pure union\n  got  = %#v\n  want = %#v", got, want)
	}
}
