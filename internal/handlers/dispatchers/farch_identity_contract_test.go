// farch_identity_contract_test.go — A2 contract-core falsifiers F-ARCH-2/3/5/6
// (definitive-cache-identity-architecture §6). All arms drive the REAL derivation
// (effectiveKeyExtras → cache.ComputeKey / widgets.DeclaredIdentity) over real CR
// shapes and assert PRE-HASH ResolvedKeyInputs field-equality on the identity
// dimension (feedback_key_parity_golden_real_inputs_prehash_diff), never a
// hand-fed digest. RED mutations are expressed as discriminating observables +
// captured to /tmp/a2/ (revert the prod fold → the discriminant collapses).
//
// GATE AUTHN-1: identity flows ONLY from xcontext.UserInfo (the JWT); these arms
// build the principal via WithUserInfo, never a store.

package dispatchers

import (
	"context"
	"os"
	"reflect"
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jwtutil"

	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ctxAs builds a request ctx carrying a specific authenticated principal (the
// only identity source under GATE AUTHN-1).
func ctxAsIdentity(username string, groups ...string) context.Context {
	return xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: username, Groups: groups}))
}

// widgetCRDeclaring builds a widget CR declaring spec.identityContext = keys
// (plus an apiRef.extras block so the CR is a realistic shape). Reuses the same
// unstructured shape widgetCRWithExtras produces.
func widgetCRDeclaring(keys ...string) map[string]any {
	anyKeys := make([]any, len(keys))
	for i, k := range keys {
		anyKeys[i] = k
	}
	return map[string]any{"spec": map[string]any{
		"identityContext": anyKeys,
		"apiRef":          map[string]any{"name": "some-ra", "namespace": "demo-system"},
	}}
}

// widgetCRDeclaringWithEcho builds a widget CR declaring spec.identityContext =
// keys AND a widgetDataTemplate that echoes .username into
// status.widgetData.echoedUser via jq. It has NO apiRef (so widgets.Resolve's
// apiRef fetch short-circuits with an empty dict — hermetic, no apiserver), and
// a spec.widgetData base map for the template to write into. Driving
// widgets.Resolve over this CR exercises the REAL resolve-input injection merge
// (resolve.go:64-73) → mergeExtras into the data source → widgetDataTemplate jq,
// so the rendered echoedUser IS the value that reached the resolve INPUT.
func widgetCRDeclaringWithEcho(keys ...string) map[string]any {
	anyKeys := make([]any, len(keys))
	for i, k := range keys {
		anyKeys[i] = k
	}
	return map[string]any{"spec": map[string]any{
		"identityContext": anyKeys,
		"widgetData":      map[string]any{},
		"widgetDataTemplate": []any{
			map[string]any{"forPath": ".echoedUser", "expression": "${ .username }"},
		},
	}}
}

// resolveRenderedWidgetData drives the REAL widgets.Resolve over a no-apiRef CR
// and returns status.widgetData — the RESOLVED OUTPUT / resolve INPUT the widget
// templates evaluated against. widgets.Resolve populates status.widgetData
// (resolve.go:141) BEFORE its final crdschema.ValidateObjectStatus step, which
// needs an apiserver and therefore returns a (benign, expected) error in this
// hermetic harness; the rendered widgetData is already set on opts.In.Object at
// that point, so we read it regardless of the tail validate error. This is a
// genuine drive of the injection merge + mergeExtras + widgetDataTemplate jq —
// NOT a key-side proxy.
func resolveRenderedWidgetData(t *testing.T, ctx context.Context, cr map[string]any, requestExtras map[string]any) map[string]any {
	t.Helper()
	in := &unstructured.Unstructured{Object: cr}
	obj, _ := widgets.Resolve(ctx, widgets.ResolveOptions{
		In:     in,
		Extras: requestExtras,
	})
	wd, _, err := unstructured.NestedMap(obj.Object, "status", "widgetData")
	if err != nil {
		t.Fatalf("resolveRenderedWidgetData: reading status.widgetData: %v", err)
	}
	return wd
}

// widgetCRInlineParent builds a CR with a static inline+GET resourcesRefs child
// (hasInlineGETRef==true) but NO identityContext of its own — the F-ARCH-5 shape.
func widgetCRInlineParent() map[string]any {
	return map[string]any{"spec": map[string]any{
		"resourcesRefs": map[string]any{"items": []any{
			map[string]any{
				"inline":     true,
				"verb":       "get",
				"resource":   "configmaps",
				"apiVersion": "v1",
				"name":       "child-cm",
				"namespace":  "demo-system",
			},
		}},
	}}
}

func writeA2Artifact(t *testing.T, name, body string) {
	t.Helper()
	_ = os.MkdirAll("/tmp/a2", 0o755)
	_ = os.WriteFile("/tmp/a2/"+name, []byte(body), 0o644)
}

// keyFor computes the production key for a widget cell under a given identity ctx
// via the REAL derivation: effectiveKeyExtras → the per-cohort ResolvedKeyInputs
// → cache.ComputeKey. Identity enters ONLY via the extras fold (§2.1). BindingUID
// is left "" (no watcher wired), IDENTICAL for both users, so the ONLY key
// discriminant these arms probe is the identity-in-extras dimension — exactly the
// per-binding-≠-per-user crux (K=1 binding, M=2 users).
func keyFor(t *testing.T, ctx context.Context, cr map[string]any, request map[string]any) (string, map[string]any) {
	t.Helper()
	eff := effectiveKeyExtras(ctx, cr, request)
	inputs := cache.ResolvedKeyInputs{
		CacheEntryClass: "widgets",
		Group:           "widgets.templates.krateo.io",
		Version:         "v1beta1",
		Resource:        "buttons",
		Namespace:       "demo-system",
		Name:            "w1",
		// BindingUID "" — SAME for both users (the shared-binding equivalence class).
		PerPage: -1,
		Page:    -1,
		Extras:  eff,
	}
	return cache.ComputeKey(inputs), eff
}

// F-ARCH-2 — cross-user leak. A DECLARED widget under two users u1≠u2 in the SAME
// binding: the effective key extras carry each user's identity → the keys DIFFER →
// u2's serve after u1's Put is a MISS (never u1's bytes). RED mutation (drop the
// identity fold from effectiveKeyExtras) → both users fold identical extras → same
// key → u2 reads u1's cell (the leak). Pre-hash field-equality: assert the Extras
// maps differ on the identity keys, not just the digest.
func TestFARCH2_DeclaredWidget_CrossUserKeysDiffer(t *testing.T) {
	cr := widgetCRDeclaring("username", "groups")
	u1 := ctxAsIdentity("alice", "devs")
	u2 := ctxAsIdentity("bob", "devs") // SAME binding-shape (same groups), DIFFERENT user

	k1, e1 := keyFor(t, u1, cr, nil)
	k2, e2 := keyFor(t, u2, cr, nil)

	// Pre-hash: the identity dimension must differ (alice vs bob).
	if e1["username"] != "alice" || e2["username"] != "bob" {
		t.Fatalf("F-ARCH-2 pre-hash: effective extras must carry each user's server-derived username; e1=%#v e2=%#v", e1, e2)
	}
	if reflect.DeepEqual(e1, e2) {
		t.Fatalf("F-ARCH-2: a DECLARED widget's effective extras must DIFFER across users (identity folded); both=%#v — the RED mutation (drop the fold) makes them equal → cross-user leak", e1)
	}
	// Digest consequence: distinct keys → u2 is a MISS on u1's cell.
	if k1 == k2 {
		t.Fatalf("F-ARCH-2: declared-widget keys must differ across users (u1=%s u2=%s); identical key = u2 serves u1's per-user body (leak)", k1, k2)
	}
	writeA2Artifact(t, "farch2_crossuser.txt",
		"declared[username,groups]: alice-key != bob-key (identity folded); RED=drop fold → equal → leak.\ne1="+
			mapStr(e1)+"\ne2="+mapStr(e2))
}

// F-ARCH-3 — declared/undeclared boundary + spoof quarantine.
//
//	(a) UNDECLARED + old-frontend request (client identity extras) → folded as-is
//	    (passive compat, per-user, no strip).
//	(b) UNDECLARED + new-frontend request (no extras) → per-binding shared key ==
//	    the seed key (empty extras).
//	(c) DECLARED + a request trying to SPOOF extras.username=someone-else → server
//	    injection WINS: the effective extras carry the JWT's OWN username, not the
//	    spoofed value. RED mutation (injection loses precedence) → the spoofed value
//	    survives → arm fails.
func TestFARCH3_Boundary_And_SpoofQuarantine(t *testing.T) {
	ctx := ctxAsIdentity("alice", "devs")

	// (a) undeclared + client identity extras → folded (passive compat).
	undeclared := map[string]any{"spec": map[string]any{}}
	_, aExtras := keyFor(t, ctx, undeclared, map[string]any{"username": "client-supplied"})
	if aExtras["username"] != "client-supplied" {
		t.Fatalf("F-ARCH-3(a): an UNDECLARED widget must fold client-supplied extras verbatim (passive compat); got %#v", aExtras)
	}

	// (b) undeclared + no request extras → empty effective extras (== seed key).
	_, bExtras := keyFor(t, ctx, undeclared, nil)
	if len(bExtras) != 0 {
		t.Fatalf("F-ARCH-3(b): an UNDECLARED widget with no request extras must have EMPTY effective extras (per-binding == seed key); got %#v", bExtras)
	}

	// (c) declared[username] + a SPOOF attempt in the request → injection wins
	// ON THE KEY SIDE (the effective key extras carry alice, not the spoof).
	declared := widgetCRDeclaring("username")
	_, cExtras := keyFor(t, ctx, declared, map[string]any{"username": "SOMEONE-ELSE"})
	if cExtras["username"] != "alice" {
		t.Fatalf("F-ARCH-3(c) SPOOF QUARANTINE (key side): server injection must WIN over a client-supplied extras.username (JWT=alice); got %#v — the RED mutation (injection loses precedence) leaves the spoofed value here", cExtras)
	}

	// (c2) SPOOF QUARANTINE on the RESOLVED OUTPUT (the second conjunct §4.4.6(b)
	// requires and (c) alone was missing). Drive the REAL widgets.Resolve with the
	// SAME spoof: a request carrying extras.username="SOMEONE-ELSE", a JWT ctx whose
	// username is alice, a CR declaring identityContext:[username]. The rendered
	// output (status.widgetData.echoedUser, echoing .username through the resolve
	// path's widgetDataTemplate jq) MUST carry alice — proving the INJECTION also
	// wins in the resolve INPUT, not just in the key. This is the §4.4.6(b)-
	// impossible state made observable: it goes RED under the exact mutation arch
	// found (reverse the precedence in resolve.go:64-73's injection merge so the
	// client-supplied value wins), which the KEY-only (c) arm cannot see.
	echoCR := widgetCRDeclaringWithEcho("username")
	rendered := resolveRenderedWidgetData(t, ctx, echoCR, map[string]any{"username": "SOMEONE-ELSE"})
	if got := rendered["echoedUser"]; got != "alice" {
		t.Fatalf("F-ARCH-3(c2) SPOOF QUARANTINE (resolved output): the resolve INPUT must carry the JWT username (alice), NOT the client spoof; status.widgetData.echoedUser=%#v — the RED mutation (resolve.go injection loses precedence) leaves SOMEONE-ELSE in the rendered body", got)
	}
	if got := rendered["echoedUser"]; got == "SOMEONE-ELSE" {
		t.Fatalf("F-ARCH-3(c2): the SPOOFED value must NOT survive into the rendered body; got %#v", got)
	}

	writeA2Artifact(t, "farch3_boundary_spoof.txt",
		"(a) undeclared folds client extras=client-supplied; (b) undeclared+no-extras empty; "+
			"(c) declared[username] + spoof SOMEONE-ELSE → injection wins on KEY → alice; "+
			"(c2) SAME spoof driven through REAL widgets.Resolve → rendered echoedUser=alice (resolve-input injection wins, NOT SOMEONE-ELSE). "+
			"RED=reverse resolve.go injection precedence → (c2) rendered=SOMEONE-ELSE (key-only (c) unaffected).")
}

// F-ARCH-5 — inline-variance union (arch ruling option (d)). An inline-embedding
// parent (hasInlineGETRef) that declares NO identityContext of its own is keyed
// per-USER (full-identity marker) so an embedded child's identity-varying rendered
// body cannot leak across users sharing one binding. Two users → keys DIFFER. RED
// mutation (drop inlineParentIdentityForKey) → same key → u2 served u1's embedded
// child body.
func TestFARCH5_InlineParent_PerUserKey(t *testing.T) {
	parent := widgetCRInlineParent() // hasInlineGETRef==true, NO identityContext
	u1 := ctxAsIdentity("alice", "devs")
	u2 := ctxAsIdentity("bob", "devs") // same binding-shape

	// Sanity: the parent declares NO identity of its own — without the inline
	// marker this would be per-binding (the leak). declaredIdentityForKey is nil.
	if di := declaredIdentityForKey(u1, parent); di != nil {
		t.Fatalf("F-ARCH-5 setup: the inline parent must declare NO identityContext of its own; got %#v", di)
	}

	k1, e1 := keyFor(t, u1, parent, nil)
	k2, e2 := keyFor(t, u2, parent, nil)

	if e1["username"] != "alice" || e2["username"] != "bob" {
		t.Fatalf("F-ARCH-5 pre-hash: an inline parent's effective extras must carry the FULL per-user identity marker; e1=%#v e2=%#v", e1, e2)
	}
	if reflect.DeepEqual(e1, e2) || k1 == k2 {
		t.Fatalf("F-ARCH-5: an inline-embedding parent must be keyed PER-USER (full-identity marker); e1=%#v e2=%#v k1=%s k2=%s — the RED mutation (drop inlineParentIdentityForKey) makes them equal → embedded child body leaks cross-user", e1, e2, k1, k2)
	}

	// BOUNDARY: a NON-inline undeclared widget stays per-binding (no marker) —
	// proves the marker is scoped to inline parents, not blanket per-user.
	nonInline := map[string]any{"spec": map[string]any{}}
	_, en1 := keyFor(t, u1, nonInline, nil)
	_, en2 := keyFor(t, u2, nonInline, nil)
	if !reflect.DeepEqual(en1, en2) {
		t.Fatalf("F-ARCH-5 BOUNDARY: a NON-inline undeclared widget must stay per-binding (empty extras, shared); en1=%#v en2=%#v — the inline marker must not leak into non-inline widgets", en1, en2)
	}
	writeA2Artifact(t, "farch5_inline_peruser.txt",
		"inline parent (no own identityContext): alice-key != bob-key (full-identity marker); "+
			"non-inline undeclared: shared key (marker scoped to inline). RED=drop marker → inline keys equal → child leak.")
}

// F-ARCH-6 — cache-off transparency, driven through the REAL widgets.Resolve.
// The A2 identity injection is part of the widget INPUT contract, not a cache
// feature: it runs in widgets.Resolve whether or not the cache is on, so a
// declared widget's rendered output for a given JWT is identical cache-on vs
// cache-off. This arm DRIVES the resolve path in BOTH modes (CACHE_ENABLED=true
// cold vs CACHE_ENABLED=false) over the SAME declared CR + JWT ctx and asserts
// the RESOLVED OUTPUT is byte-identical AND carries the injected JWT identity in
// BOTH modes — no more twice-call-DeepEqual-with-self. (widgets.Resolve consults
// no cache handle, so byte-identity is the substantive property: the injection
// is transparent to the cache toggle.)
func TestFARCH6_CacheOffTransparency_ResolvedOutputIdentical(t *testing.T) {
	echoCR := widgetCRDeclaringWithEcho("username")
	ctx := ctxAsIdentity("alice", "devs")

	// Cache ON (cold): drive the real resolve path with CACHE_ENABLED truthy.
	t.Setenv("CACHE_ENABLED", "true")
	if cache.Disabled() {
		t.Fatalf("F-ARCH-6 setup: CACHE_ENABLED=true must report cache enabled")
	}
	cacheOn := resolveRenderedWidgetData(t, ctx, widgetCRDeclaringWithEcho("username"), nil)

	// Cache OFF: drive the SAME resolve path with CACHE_ENABLED=false semantics.
	t.Setenv("CACHE_ENABLED", "false")
	if !cache.Disabled() {
		t.Fatalf("F-ARCH-6 setup: CACHE_ENABLED=false must report cache disabled")
	}
	cacheOff := resolveRenderedWidgetData(t, ctx, echoCR, nil)

	// The injected JWT identity must materialise in the resolve INPUT in BOTH modes.
	if cacheOn["echoedUser"] != "alice" {
		t.Fatalf("F-ARCH-6 (cache ON): the declared JWT identity must reach the resolve input (echoedUser=alice); got %#v", cacheOn)
	}
	if cacheOff["echoedUser"] != "alice" {
		t.Fatalf("F-ARCH-6 (cache OFF): the declared JWT identity must reach the resolve input (echoedUser=alice); got %#v", cacheOff)
	}
	// And the resolved output must be byte-identical across the cache toggle —
	// the injection is part of the INPUT contract, invariant to cache state.
	if !reflect.DeepEqual(cacheOn, cacheOff) {
		t.Fatalf("F-ARCH-6: declared-widget resolved output must be byte-identical cache-on vs cache-off for the same JWT; on=%#v off=%#v", cacheOn, cacheOff)
	}
	writeA2Artifact(t, "farch6_cacheoff.txt",
		"declared[username] echoing .username → status.widgetData.echoedUser: driven through REAL widgets.Resolve under CACHE_ENABLED=true (cold) AND CACHE_ENABLED=false — both render echoedUser=alice (JWT identity in the resolve input) AND are byte-identical. Injection is an INPUT-contract property, transparent to the cache toggle.")
}

// mapStr renders a map for artifact transcripts deterministically-enough.
func mapStr(m map[string]any) string {
	b := ""
	for k, v := range m {
		b += k + "=" + valStr(v) + " "
	}
	return b
}

func valStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []string:
		s := "["
		for i, e := range t {
			if i > 0 {
				s += ","
			}
			s += e
		}
		return s + "]"
	default:
		return "?"
	}
}
