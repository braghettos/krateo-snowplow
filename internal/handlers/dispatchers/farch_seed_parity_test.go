// farch_seed_parity_test.go — A4 seed parity falsifiers (definitive-cache-identity-
// architecture-2026-07-07.md §8.1 A4 + §6 F-ARCH-1). A4 is TEST-ONLY: the seed path
// (phase1_pip_seed.go:781) already routes through the shared effectiveKeyExtras (A1),
// so for a NON-declared widget the seed emits the identity-free shared cell that a
// post-contract (buildExtrasParam) browser request keys to — seed parity by
// construction (§1 "per-binding shared cells and seed parity BY CONSTRUCTION"). For a
// DECLARED widget the seed folds only the identity dimensions the cohort representative
// actually carries, and — proven by the arch trace on this branch — never creates a
// leakable mis-keyed shared cell (the two-dimension BindingUID × declared-identity-in-
// Extras key + DeclaredIdentity injecting only NON-EMPTY values makes every declared
// case SAFE). No prod change needed or made.
//
// F-ARCH-1 (the permanent #107 arm): portal-shaped request exactly as post-contract
// buildExtrasParam emits (dashboard: NO identity extras) vs a cell seeded by the REAL
// seed key fold → pre-hash ResolvedKeyInputs field-equality + L1 HIT. RED arm: re-add
// the unconditional identity extras (the shipped 1.6.5 / old-frontend defect) → the
// serve key diverges → MISS (reproduces the 0/N key-match divergence #107 documented).
//
// Declared-seed-safety pin: the three declared-widget cases the arch trace enumerated,
// driven through the REAL effectiveKeyExtras → ComputeKey (never hand-fed keys;
// feedback_key_parity_golden_real_inputs_prehash_diff), asserting the two-dimension
// property (Extras dimension, not just BindingUID; feedback_spoof_quarantine_needs_
// both_key_and_resolved_output_arms).
package dispatchers

import (
	"context"
	"reflect"
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/snowplow/internal/cache"
)

// portalDashboardRequest is the extras a post-contract buildExtrasParam
// (useWidgetQuery.ts:72-85 with SNOWPLOW_IDENTITY_INJECTION set) emits for a
// DASHBOARD widget: NO identity keys, NO route params → an EMPTY extras map. This
// is the exact wire shape F-ARCH-1 pins the seed against.
func portalDashboardRequest() map[string]any { return map[string]any{} }

// oldFrontendRequest is the PRE-contract buildExtrasParam shape (the shipped 1.6.5
// defect / flag-absent legacy): unconditional identity merges → username+groups on
// the wire. F-ARCH-1's RED arm uses this to reproduce the #107 seed-miss divergence.
func oldFrontendRequest() map[string]any {
	return map[string]any{"username": "cyberjoker", "groups": []any{"devs"}}
}

// TestFARCH1_SeedParity_NonDeclaredWidget_PortalShapedHIT — the permanent #107
// invariant. A NON-declared widget's cell seeded under the REAL seed key fold (the
// seed has NO request extras → effectiveKeyExtras(ctx, cr, nil)) is HIT by a
// post-contract portal request (empty extras) — pre-hash ResolvedKeyInputs
// field-equality + a real Put-under-seed / Get-under-serve HIT with zero resolver
// invocations. This is the warmth vehicle that fixes #107: seed cell == browser key.
func TestFARCH1_SeedParity_NonDeclaredWidget_PortalShapedHIT(t *testing.T) {
	enableWidgetContentL1(t)
	ctx := ctxWithIdentity() // cyberjoker / [devs] — the requesting principal
	const (
		g, v, r, ns, name = "widgets.templates.krateo.io", "v1beta1", "buttons", "demo-system", "btn-107"
		perPage, page     = 10, 1
	)
	// A NON-declared widget (no spec.identityContext), no inline extras — the ~99%
	// corpus shape. Its key folds NO identity on either side under the contract.
	cr := map[string]any{"spec": map[string]any{}}

	// SEED key: the REAL seed fold — effectiveKeyExtras(ctx, cr, nil) (no request).
	seedExtras := effectiveKeyExtras(ctx, cr, nil)
	seedKey, seedHandle, seedInputs := dispatchCacheLookupKey(ctx, "widgets",
		g, v, r, ns, name, perPage, page, seedExtras)
	if seedHandle == nil || seedKey == "" {
		t.Fatal("F-ARCH-1: expected a live per-cohort cache handle + key under the seed ctx")
	}
	body := []byte(`{"status":{"widgetData":{"label":"warm"}}}`)
	seedHandle.Put(seedKey, &cache.ResolvedEntry{RawJSON: body, Inputs: seedInputs})

	// SERVE key: the REAL dispatcher fold under a POST-CONTRACT portal request
	// (empty extras) — effectiveKeyExtras(ctx, cr, portalDashboardRequest()).
	serveExtras := effectiveKeyExtras(ctx, cr, portalDashboardRequest())
	serveKey, serveHandle, serveInputs := dispatchCacheLookupKey(ctx, "widgets",
		g, v, r, ns, name, perPage, page, serveExtras)
	if serveHandle == nil || serveKey == "" {
		t.Fatal("F-ARCH-1: expected a live per-cohort cache handle + key under the serve ctx")
	}

	// Pre-hash: both sides fold EMPTY effective extras (no identity, no request) for
	// a non-declared widget under the contract.
	if len(seedExtras) != 0 || len(serveExtras) != 0 {
		t.Fatalf("F-ARCH-1 pre-hash: a non-declared widget under the contract must fold EMPTY extras on BOTH sides; seed=%#v serve=%#v", seedExtras, serveExtras)
	}
	if !reflect.DeepEqual(seedInputs.Extras, serveInputs.Extras) {
		t.Fatalf("F-ARCH-1 pre-hash: seed vs serve ResolvedKeyInputs.Extras differ; seed=%#v serve=%#v", seedInputs.Extras, serveInputs.Extras)
	}
	if seedKey != serveKey {
		t.Fatalf("F-ARCH-1 INVARIANT: seed key %q != portal-shaped serve key %q — the seeded cell is not browser-reachable (the #107 seed-miss class)", seedKey, serveKey)
	}
	// L1 HIT with zero resolver invocations (the Get is a pure cache read).
	got, hit := serveHandle.Get(serveKey)
	if !hit {
		t.Fatal("F-ARCH-1: portal-shaped serve MISSED the seeded cell — #107 not fixed")
	}
	if string(got.RawJSON) != string(body) {
		t.Fatalf("F-ARCH-1: served the wrong body; got %q want %q", got.RawJSON, body)
	}
}

// TestFARCH1_RED_OldFrontendRequest_Misses — the F-ARCH-1 RED arm. Re-adding the
// unconditional identity extras to the SERVE request (the old-frontend / shipped
// 1.6.5 wire shape) makes the serve key fold {username,groups} while the seed key
// (identity-free) folds nothing → the keys DIVERGE → the seeded cell is MISSED. This
// documents exactly what the pre-contract wire shape did (the #107 0/N divergence):
// this arm is GREEN (asserts the MISS) precisely because the defect shape must miss.
func TestFARCH1_RED_OldFrontendRequest_Misses(t *testing.T) {
	enableWidgetContentL1(t)
	ctx := ctxWithIdentity()
	const (
		g, v, r, ns, name = "widgets.templates.krateo.io", "v1beta1", "buttons", "demo-system", "btn-107-red"
		perPage, page     = 10, 1
	)
	cr := map[string]any{"spec": map[string]any{}} // non-declared

	seedExtras := effectiveKeyExtras(ctx, cr, nil)
	seedKey, seedHandle, seedInputs := dispatchCacheLookupKey(ctx, "widgets",
		g, v, r, ns, name, perPage, page, seedExtras)
	if seedHandle == nil || seedKey == "" {
		t.Fatal("F-ARCH-1 RED: expected a live seed handle + key")
	}
	seedHandle.Put(seedKey, &cache.ResolvedEntry{RawJSON: []byte(`{"x":1}`), Inputs: seedInputs})

	// OLD-FRONTEND serve request: unconditional identity extras on the wire.
	serveExtras := effectiveKeyExtras(ctx, cr, oldFrontendRequest())
	serveKey, serveHandle, _ := dispatchCacheLookupKey(ctx, "widgets",
		g, v, r, ns, name, perPage, page, serveExtras)
	if serveHandle == nil || serveKey == "" {
		t.Fatal("F-ARCH-1 RED: expected a live serve handle + key")
	}

	// The old-frontend request folds identity extras (a non-declared widget passes
	// them through, passive compat) → the serve key MUST diverge from the seed key.
	if serveExtras["username"] != "cyberjoker" {
		t.Fatalf("F-ARCH-1 RED setup: the old-frontend request must fold identity extras (passive compat); got %#v", serveExtras)
	}
	if seedKey == serveKey {
		t.Fatalf("F-ARCH-1 RED: seed key == old-frontend serve key %q — the arm is not discriminating; the old wire shape MUST miss the identity-free seed cell (the #107 divergence)", serveKey)
	}
	if _, hit := serveHandle.Get(serveKey); hit {
		t.Fatal("F-ARCH-1 RED: old-frontend serve unexpectedly HIT the seed cell — the #107 seed-miss should reproduce here")
	}
}

// --- Declared-seed-safety pin (arch trace: SAFE across all 3 cases) ---

// declaredCR builds a widget CR declaring spec.identityContext=keys (no apiRef).
func declaredCR(keys ...string) map[string]any {
	anyKeys := make([]any, len(keys))
	for i, k := range keys {
		anyKeys[i] = k
	}
	return map[string]any{"spec": map[string]any{"identityContext": anyKeys}}
}

// seedCtxRealUser / seedCtxGroupOnly model the cohort representative the seed ctx
// carries (withCohortSeedContext installs WithUserInfo from pickRepresentativeFrom-
// Subjects): a real single user, or a group-only representative (empty username).
func seedCtxRealUser(username string, groups ...string) context.Context {
	return xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: username, Groups: groups}))
}
func seedCtxGroupOnly(groups ...string) context.Context {
	return xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: "", Groups: groups})) // group-only rep
}

// TestFARCH_A4_DeclaredSeedSafety pins the arch trace's three cases through the REAL
// effectiveKeyExtras → ComputeKey. It proves the seed never creates a leakable
// mis-keyed shared cell for a declared widget:
//
//	Case 1: representative = real user → declared[username] folds THAT user's name;
//	        another real user folds a DIFFERENT name → distinct Extras → distinct key.
//	Case 2: representative = group-only + declared[username] → DeclaredIdentity returns
//	        nil (empty username not injected) → seed folds EMPTY extras; a real member's
//	        request folds {username:<name>} → distinct key → the seed cell is an
//	        UNREACHABLE orphan (no leak, benign warmth-miss).
//	Case 3: representative = group-only + declared[groups] → seed folds {groups:[devs]};
//	        a real member also folds {groups:[devs]} → SAME extras → SAME key → intended
//	        per-group-shared HIT.
func TestFARCH_A4_DeclaredSeedSafety(t *testing.T) {
	// Case 1 — real-user representative, declared[username]: per-user distinct keys.
	t.Run("case1_real_user_distinct", func(t *testing.T) {
		cr := declaredCR("username")
		seedExtras := effectiveKeyExtras(seedCtxRealUser("admin"), cr, nil)
		otherExtras := effectiveKeyExtras(seedCtxRealUser("bob"), cr, nil)
		if seedExtras["username"] != "admin" {
			t.Fatalf("case1: seed under real user must fold that username; got %#v", seedExtras)
		}
		if reflect.DeepEqual(seedExtras, otherExtras) {
			t.Fatalf("case1: a declared[username] cell must differ across users (admin vs bob); both=%#v — the seed cell is per-user, never cross-user reachable", seedExtras)
		}
	})

	// Case 2 — group-only representative, declared[username]: seed folds EMPTY extras
	// (no username to inject) → unreachable orphan vs any real member's request.
	t.Run("case2_group_only_username_orphan", func(t *testing.T) {
		cr := declaredCR("username")
		seedExtras := effectiveKeyExtras(seedCtxGroupOnly("devs"), cr, nil)
		if len(seedExtras) != 0 {
			t.Fatalf("case2: a group-only representative (empty username) declaring [username] must inject NOTHING → EMPTY seed extras (DeclaredIdentity returns nil on empty username); got %#v", seedExtras)
		}
		// A real devs member folds their own username → distinct extras → the seed
		// orphan is UNREACHABLE (no real principal can re-derive the empty-extras key).
		realExtras := effectiveKeyExtras(seedCtxRealUser("carol", "devs"), cr, nil)
		if realExtras["username"] != "carol" {
			t.Fatalf("case2: a real devs member declaring [username] must fold their own name; got %#v", realExtras)
		}
		if reflect.DeepEqual(seedExtras, realExtras) {
			t.Fatalf("case2 LEAK GUARD: the empty-extras seed cell must be UNREACHABLE by a real member (whose extras carry a username); seed=%#v real=%#v — equality would mean a real request keys onto the group-only orphan (leak)", seedExtras, realExtras)
		}
	})

	// Case 3 — group-only representative, declared[groups]: seed == real member →
	// intended per-group-shared HIT (body scoped to the group, not a user).
	t.Run("case3_group_only_groups_shared_hit", func(t *testing.T) {
		cr := declaredCR("groups")
		seedExtras := effectiveKeyExtras(seedCtxGroupOnly("devs"), cr, nil)
		// groups is JSON-native []any (the A2 fix — deep-copy-safe for the
		// resolve-input path; key-parity byte-identical to the prior []string).
		if !reflect.DeepEqual(seedExtras["groups"], []any{"devs"}) {
			t.Fatalf("case3: a group-only representative declaring [groups] must fold {groups:[devs]}; got %#v", seedExtras)
		}
		realExtras := effectiveKeyExtras(seedCtxRealUser("carol", "devs"), cr, nil)
		// carol declares [groups] only → username NOT folded; groups match the seed.
		if !reflect.DeepEqual(seedExtras, realExtras) {
			t.Fatalf("case3: a declared[groups] cell must be per-group-shared — seed (group-only rep) and a real member's extras must be IDENTICAL; seed=%#v real=%#v", seedExtras, realExtras)
		}
	})
}
