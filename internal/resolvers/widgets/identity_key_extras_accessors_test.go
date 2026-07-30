package widgets

import (
	"context"
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// identity_key_extras_accessors_test.go — M3 [SEC] for the three author-declared
// identity/key-extras accessors (widgets.go): GetIdentityContext,
// DeclaredIdentity, GetKeyExtras. All are pure / context-only ⇒ fully hermetic,
// no cluster.
//
// The load-bearing security property is the D1 enum foreclosure on
// GetIdentityContext: ONLY {username, groups} survive; anything else (a typo, a
// stale/forbidden displayName declaration) is DROPPED in code, so the server can
// never inject a key the JWT principal does not carry into the cache key OR the
// resolve input. GetKeyExtras is the request-extras twin with NO enum (any name
// is honored) — but with the SAME order-preserving + dedup + absence-tolerant
// contract.

// specWith wraps a spec.<field> value into the unstructured shape the accessors
// read (maps.NestedSlice(obj, "spec", <field>)).
func specWith(field string, val any) map[string]any {
	return map[string]any{"spec": map[string]any{field: val}}
}

// ---------------------------------------------------------------------------
// GetIdentityContext — enum-filtered, order-preserving, dedup, absence-tolerant.
// ---------------------------------------------------------------------------

func TestGetIdentityContext(t *testing.T) {
	tests := []struct {
		name string
		obj  map[string]any
		want []string
	}{
		{
			name: "SEC enum filter + order + dedup + non-string drop",
			// [username, displayName, groups, username(dup), 42] → [username, groups]
			obj:  specWith("identityContext", []any{"username", "displayName", "groups", "username", 42}),
			want: []string{"username", "groups"},
		},
		{
			name: "order preserved (groups before username)",
			obj:  specWith("identityContext", []any{"groups", "username"}),
			want: []string{"groups", "username"},
		},
		{
			name: "displayName ALONE is dropped ⇒ empty (D1 foreclosure)",
			obj:  specWith("identityContext", []any{"displayName"}),
			want: nil,
		},
		{
			name: "arbitrary out-of-enum typo dropped",
			obj:  specWith("identityContext", []any{"userName", "Groups", "user"}), // wrong case / typo
			want: nil,
		},
		{
			name: "absent spec.identityContext ⇒ nil",
			obj:  map[string]any{"spec": map[string]any{}},
			want: nil,
		},
		{
			name: "empty slice ⇒ nil",
			obj:  specWith("identityContext", []any{}),
			want: nil,
		},
		{
			name: "wrong type (string not slice) ⇒ nil (absence-tolerant)",
			obj:  specWith("identityContext", "username"),
			want: nil,
		},
		{
			name: "no spec at all ⇒ nil",
			obj:  map[string]any{},
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := GetIdentityContext(tc.obj)
			assert.Equal(t, tc.want, got)
		})
	}
}

// ---------------------------------------------------------------------------
// DeclaredIdentity — materialises the DECLARED enum keys from the ctx principal.
// ---------------------------------------------------------------------------

// ctxWithUser builds a context carrying the given authenticated principal, the
// way the authn middleware does in production (xcontext.WithUserInfo).
func ctxWithUser(username string, groups []string) context.Context {
	return xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: username, Groups: groups}))
}

func TestDeclaredIdentity(t *testing.T) {
	t.Run("declared username+groups materialises both as JSON-native", func(t *testing.T) {
		obj := specWith("identityContext", []any{"username", "groups"})
		ctx := ctxWithUser("cyberjoker", []string{"devs", "ops"})

		got := DeclaredIdentity(ctx, obj)
		require.NotNil(t, got)
		assert.Equal(t, "cyberjoker", got["username"])
		// groups MUST be JSON-native []any (NOT []string) — else the downstream
		// DeepCopyJSON in the resolve/key fold panics.
		gg, ok := got["groups"].([]any)
		require.True(t, ok, "groups must be []any (JSON-native), got %T", got["groups"])
		assert.Equal(t, []any{"devs", "ops"}, gg)
	})

	t.Run("SEC: displayName declaration is foreclosed ⇒ nil (never injected)", func(t *testing.T) {
		obj := specWith("identityContext", []any{"displayName"})
		ctx := ctxWithUser("cyberjoker", []string{"devs"})
		assert.Nil(t, DeclaredIdentity(ctx, obj),
			"an out-of-enum declaration must yield no injection — the server never injects a key the principal lacks")
	})

	t.Run("no declaration ⇒ nil (prod-inert default)", func(t *testing.T) {
		obj := map[string]any{"spec": map[string]any{}}
		ctx := ctxWithUser("cyberjoker", []string{"devs"})
		assert.Nil(t, DeclaredIdentity(ctx, obj))
	})

	t.Run("declared but NO principal on ctx ⇒ nil (fail-safe)", func(t *testing.T) {
		obj := specWith("identityContext", []any{"username", "groups"})
		assert.Nil(t, DeclaredIdentity(context.Background(), obj),
			"absent UserInfo must inject nothing (fail-safe), never a spurious key")
	})

	t.Run("empty username is not a key", func(t *testing.T) {
		obj := specWith("identityContext", []any{"username", "groups"})
		ctx := ctxWithUser("", []string{"devs"})
		got := DeclaredIdentity(ctx, obj)
		require.NotNil(t, got, "groups still present ⇒ non-nil map")
		_, hasUser := got["username"]
		assert.False(t, hasUser, "an empty username must NOT be injected as a key")
		assert.Equal(t, []any{"devs"}, got["groups"])
	})

	t.Run("declared groups but principal has none ⇒ nil", func(t *testing.T) {
		obj := specWith("identityContext", []any{"groups"})
		ctx := ctxWithUser("cyberjoker", nil)
		assert.Nil(t, DeclaredIdentity(ctx, obj),
			"declared groups with an empty principal group set injects nothing (len(out)==0 ⇒ nil)")
	})

	t.Run("groups slice does NOT alias the ctx principal slice", func(t *testing.T) {
		src := []string{"devs", "ops"}
		obj := specWith("identityContext", []any{"groups"})
		ctx := ctxWithUser("cyberjoker", src)
		got := DeclaredIdentity(ctx, obj)
		gg := got["groups"].([]any)
		// Mutating the injected copy must not corrupt the source (fresh []any).
		gg[0] = "MUTATED"
		assert.Equal(t, "devs", src[0], "DeclaredIdentity must copy the groups, never alias the ctx slice")
	})
}

// ---------------------------------------------------------------------------
// GetKeyExtras — arbitrary names honored (NO enum), order-preserving, dedup,
// absence-tolerant.
// ---------------------------------------------------------------------------

func TestGetKeyExtras(t *testing.T) {
	tests := []struct {
		name string
		obj  map[string]any
		want []string
	}{
		{
			name: "arbitrary names honored, order preserved, dedup, non-string dropped",
			obj:  specWith("keyExtras", []any{"namespace", "name", "namespace", "custom-param", 7}),
			want: []string{"namespace", "name", "custom-param"},
		},
		{
			name: "NO enum filter — displayName-shaped name is honored (unlike identityContext)",
			obj:  specWith("keyExtras", []any{"displayName", "anything"}),
			want: []string{"displayName", "anything"},
		},
		{
			name: "absent ⇒ nil (fold-nothing default)",
			obj:  map[string]any{"spec": map[string]any{}},
			want: nil,
		},
		{
			name: "empty slice ⇒ nil",
			obj:  specWith("keyExtras", []any{}),
			want: nil,
		},
		{
			name: "wrong type ⇒ nil",
			obj:  specWith("keyExtras", map[string]any{"namespace": true}),
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := GetKeyExtras(tc.obj)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestGetKeyExtras_VsIdentityContext_EnumDivergence is the SEC discriminator
// between the two accessors on the SAME input: identityContext DROPS a
// displayName / typo (enum foreclosure), keyExtras KEEPS it (open-ended request
// key names). Encodes why they are two functions, not one.
func TestGetKeyExtras_VsIdentityContext_EnumDivergence(t *testing.T) {
	input := []any{"username", "displayName", "namespace"}

	idc := GetIdentityContext(specWith("identityContext", input))
	assert.Equal(t, []string{"username"}, idc,
		"identityContext MUST drop displayName + the request-only 'namespace' (enum = {username, groups})")

	ke := GetKeyExtras(specWith("keyExtras", input))
	assert.Equal(t, []string{"username", "displayName", "namespace"}, ke,
		"keyExtras MUST honor every declared name (no enum) — the divergence that requires two accessors")
}
