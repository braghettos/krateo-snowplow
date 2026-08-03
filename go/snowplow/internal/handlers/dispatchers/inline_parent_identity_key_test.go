// inline_parent_identity_key_test.go — L1 (coverage-audit) SECURITY:
// inlineParentIdentityForKey (helpers.go, A2 §4.3 F-ARCH-5 marker) — the
// KEY-side per-user marker that keeps an inline-embedding parent's cell per-USER
// so an embedded child's identity-varying `rendered` body cannot leak across
// users sharing one BindingUID.
//
// TestFARCH5_InlineParent_PerUserKey pins the WHOLE-effectiveKeyExtras outcome
// (alice-key != bob-key). This file pins the marker FUNCTION directly across the
// partial-identity shapes it must handle exactly, because the marker's precise
// map SHAPE is load-bearing:
//
//	identity-less ctx (no UserInfo)     → nil     (fail-safe: the request already
//	                                              fail-closes to the ""-BindingUID
//	                                              MISS path; the marker must not
//	                                              synthesize a {} key material)
//	username-only (no groups)           → {"username": u}                (no groups key)
//	groups-only (no username)           → {"groups": []any{...}}         (no username key)
//	both                                → {"username": u, "groups": []any{...}}
//	NON-inline widget (any identity)    → nil     (marker scoped to inline parents)
//
// AND every "groups" value MUST be JSON-native []any (NOT Go []string) — the
// shape-uniformity contract (feedback_identity_extras_must_be_json_native_slices):
// a []string that ever reached a resolve-input DeepCopyJSON would panic; the
// marker is key-only today but is built []any so it can never regress.
//
// RED arms (each expressed as a discriminating assertion):
//   - returns {} on identity-less  → a fold of {} into the key would make the
//     inline parent cell shared again (leak). Asserted nil, not empty.
//   - leaks marker to non-inline    → a non-inline widget getting a per-user
//     marker over-partitions the ~99% corpus. Asserted nil for non-inline.
//   - groups as []string            → asserted the concrete type is []any.

package dispatchers

import (
	"context"
	"testing"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/jwtutil"
)

// l1Ctx builds a ctx carrying the given identity. Empty username + nil groups
// still installs a (zero) UserInfo — for the true "identity-less" case use
// context.Background() (no WithUserInfo), which makes xcontext.UserInfo error.
func l1Ctx(username string, groups []string) context.Context {
	return xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: username, Groups: groups}))
}

func TestL1_InlineParentIdentityForKey_Shapes(t *testing.T) {
	inlineCR := widgetCRInlineParent() // hasInlineGETRef==true (farch_identity_contract_test.go)
	if !hasInlineGETRef(inlineCR) {
		t.Fatal("setup: widgetCRInlineParent must classify hasInlineGETRef==true")
	}

	t.Run("identity-less ctx → nil (RED: returns {})", func(t *testing.T) {
		got := inlineParentIdentityForKey(context.Background(), inlineCR)
		if got != nil {
			t.Fatalf("identity-less inline parent must return nil, got %#v — a {} return would fold empty identity into the key and re-share the inline cell (leak)", got)
		}
	})

	t.Run("username-only → {username} (no groups key)", func(t *testing.T) {
		got := inlineParentIdentityForKey(l1Ctx("alice", nil), inlineCR)
		if got == nil {
			t.Fatal("username-only inline parent must return a non-nil marker")
		}
		if got["username"] != "alice" {
			t.Fatalf("username marker = %#v, want username=alice", got)
		}
		if _, ok := got["groups"]; ok {
			t.Fatalf("no groups present → the marker must NOT carry a groups key; got %#v", got)
		}
	})

	t.Run("groups-only → {groups:[]any} (no username key)", func(t *testing.T) {
		got := inlineParentIdentityForKey(l1Ctx("", []string{"devs", "ops"}), inlineCR)
		if got == nil {
			t.Fatal("groups-only inline parent must return a non-nil marker")
		}
		if _, ok := got["username"]; ok {
			t.Fatalf("empty username → the marker must NOT carry a username key; got %#v", got)
		}
		g, ok := got["groups"]
		if !ok {
			t.Fatalf("groups-only marker must carry a groups key; got %#v", got)
		}
		// RED: groups as []string. The marker MUST be JSON-native []any.
		as, isStrings := g.([]string)
		if isStrings {
			t.Fatalf("RED (groups as []string): groups must be JSON-native []any, not []string %v — a []string reaching a resolve-input DeepCopyJSON panics", as)
		}
		anyG, isAny := g.([]any)
		if !isAny {
			t.Fatalf("groups must be []any; got %T (%#v)", g, g)
		}
		if len(anyG) != 2 || anyG[0] != "devs" || anyG[1] != "ops" {
			t.Fatalf("groups []any content = %#v, want [devs ops]", anyG)
		}
	})

	t.Run("both → {username, groups:[]any}", func(t *testing.T) {
		got := inlineParentIdentityForKey(l1Ctx("bob", []string{"team-a"}), inlineCR)
		if got["username"] != "bob" {
			t.Fatalf("marker username = %#v, want bob", got["username"])
		}
		g, ok := got["groups"].([]any)
		if !ok || len(g) != 1 || g[0] != "team-a" {
			t.Fatalf("marker groups = %#v, want []any{team-a}", got["groups"])
		}
	})

	t.Run("NON-inline widget → nil (RED: marker leaks to non-inline)", func(t *testing.T) {
		nonInline := map[string]any{"spec": map[string]any{}}
		if hasInlineGETRef(nonInline) {
			t.Fatal("setup: the non-inline CR must NOT classify hasInlineGETRef")
		}
		got := inlineParentIdentityForKey(l1Ctx("alice", []string{"devs"}), nonInline)
		if got != nil {
			t.Fatalf("RED (marker leaks to non-inline): a non-inline widget must return nil (no per-user marker), got %#v — else the ~99%% identity-free corpus over-partitions", got)
		}
	})
}
