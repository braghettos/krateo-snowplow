package widgets

import (
	"context"
	"encoding/json"
	"testing"

	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets/resourcesrefstemplate"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets/widgetdatatemplate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"
)

// parseExtras mirrors internal/handlers/util.ParseExtras: in production the
// extras map is ALWAYS the result of json.Unmarshal of the ?extras= query
// param, so its values are JSON-native Go types (float64 for every number,
// string, bool, []any, map[string]any, nil) — never a Go int/int64. Tests
// MUST build extras this way so the fixture matches the real wire shape that
// flows through mergeExtras → ds → the gojq evaluator (gojq's encoder
// handles int/float64 but panics on a raw int64 — exactly the type a literal
// int64 fixture or maps.DeepCopyJSON(int) would produce, and a shape
// production never emits).
func parseExtras(t *testing.T, jsonExtras string) map[string]any {
	t.Helper()
	out := map[string]any{}
	require.NoError(t, json.Unmarshal([]byte(jsonExtras), &out))
	return out
}

// These are the step-2 (mergeExtras) unit tests for the extras→widgets
// parity feature. mergeExtras is the resolver's data-source merge that runs
// after injectSlice and before resolveWidgetData / resolveResourceRefs (both
// of which evaluate their jq against the SAME `ds`). They are plain unit
// tests (no integration build tag, no cluster) — exactly the shape of the
// sibling resolve_slice_injection_test.go, which unit-tests injectSlice + the
// widgetdatatemplate.Resolve jq path against an in-memory ds.

// TestMergeExtras_AddsKeysNonOverwriting is the core precedence falsifier:
// extras is the BASE; any key already in ds (an apiRef-RA result key or the
// injectSlice triple) WINS on collision. Mirrors the RESTAction precedence
// at restactions/api/resolve.go:228-230 (dict starts as the extras copy, API
// results overwrite).
func TestMergeExtras_AddsKeysNonOverwriting(t *testing.T) {
	ds := map[string]any{
		// pre-existing apiRef-RA result key — MUST win on collision.
		"shared": "from-apiRef",
		"list":   []any{map[string]any{"x": 1}},
	}
	// JSON-native extras (the production wire shape — numbers are float64).
	extras := parseExtras(t, `{"shared":"from-extras","tenant":"acme","limit":7}`)

	mergeExtras(ds, extras)

	assert.Equal(t, "from-apiRef", ds["shared"],
		"collision: the pre-existing apiRef-result key MUST win over extras (mirrors RESTAction precedence)")
	assert.Equal(t, "acme", ds["tenant"], "new extras key must be added to ds")
	assert.EqualValues(t, 7, ds["limit"], "new extras key must be added to ds")
	assert.Equal(t, []any{map[string]any{"x": 1}}, ds["list"], "unrelated ds key must be untouched")
}

// TestMergeExtras_DoesNotClobberInjectedSlice — the injectSlice triple is
// written into ds BEFORE mergeExtras runs (resolve.go), so an extras key
// named "slice" must NOT overwrite the resolver-owned pagination triple.
// Pagination is authoritative; a caller cannot override it via extras at the
// widget-template layer.
func TestMergeExtras_DoesNotClobberInjectedSlice(t *testing.T) {
	ds := map[string]any{"list": []any{}}
	injectSlice(ds, 50, 2) // ds["slice"] = {page:2, perPage:50, offset:50}

	extras := map[string]any{
		"slice": map[string]any{"page": 999, "perPage": 999, "offset": 999},
	}
	mergeExtras(ds, extras)

	got, ok := ds["slice"].(map[string]any)
	require.True(t, ok, "slice must remain a map[string]any")
	assert.Equal(t, 2, got["page"], "resolver-injected slice.page must survive the extras merge")
	assert.Equal(t, 50, got["perPage"], "resolver-injected slice.perPage must survive the extras merge")
	assert.Equal(t, 50, got["offset"], "resolver-injected slice.offset must survive the extras merge")
}

// TestMergeExtras_NilOrEmptyIsNoOp — the backward-compat guard. A widget
// resolved with no extras param (nil OR the non-nil empty map ParseExtras
// returns when ?extras is absent) must leave ds byte-identical.
func TestMergeExtras_NilOrEmptyIsNoOp(t *testing.T) {
	build := func() map[string]any {
		return map[string]any{
			"list": []any{map[string]any{"a": 1}},
			"meta": map[string]any{"k": "v"},
		}
	}

	// nil extras
	dsNil := build()
	mergeExtras(dsNil, nil)
	assert.Equal(t, build(), dsNil, "nil extras must be a no-op")

	// non-nil empty extras (what util.ParseExtras returns for absent ?extras)
	dsEmpty := build()
	mergeExtras(dsEmpty, map[string]any{})
	assert.Equal(t, build(), dsEmpty, "empty extras must be a no-op")
}

// TestMergeExtras_NilDsIsNoOp — defensive, symmetric with injectSlice.
func TestMergeExtras_NilDsIsNoOp(t *testing.T) {
	assert.NotPanics(t, func() {
		mergeExtras(nil, map[string]any{"k": "v"})
	})
}

// TestMergeExtras_DeepCopiesValues — ds must NOT alias the caller's extras
// map; mutating the merged value through the original extras map (or vice
// versa) must not bleed across. Proves the DeepCopyJSON isolation that keeps
// the per-call ds self-contained.
func TestMergeExtras_DeepCopiesValues(t *testing.T) {
	nested := map[string]any{"inner": "orig"}
	extras := map[string]any{"obj": nested}

	ds := map[string]any{}
	mergeExtras(ds, extras)

	// mutate the ORIGINAL extras' nested map after the merge.
	nested["inner"] = "mutated-after-merge"

	got, ok := ds["obj"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "orig", got["inner"],
		"ds must hold a DEEP COPY — a post-merge mutation of the source extras must not bleed into ds")
}

// --- step-2 through the TEMPLATE resolvers (the real consumers of ds) ---

// TestExtrasReferenceable_InWidgetDataTemplate — a widgetDataTemplate whose
// jq references an extras key resolves to the extras-supplied value.
// resolveWidgetData feeds the post-mergeExtras ds straight into
// widgetdatatemplate.Resolve, so this is the exact production eval path for
// step 2 over the widgetData path.
func TestExtrasReferenceable_InWidgetDataTemplate(t *testing.T) {
	// Simulate the resolver: an apiRef-less widget ⇒ ds starts empty, then
	// extras is merged in. JSON-native extras (count is float64 on the wire).
	ds := map[string]any{}
	mergeExtras(ds, parseExtras(t, `{"tenant":"acme","count":3}`))

	items := []templatesv1.WidgetDataTemplate{
		{ForPath: "data.tenant", Expression: "${ .tenant }"},
		{ForPath: "data.count", Expression: "${ .count }"},
	}
	evals, err := widgetdatatemplate.Resolve(context.Background(), widgetdatatemplate.ResolveOptions{
		Items:      items,
		DataSource: ds,
	})
	require.NoError(t, err)
	require.Len(t, evals, 2)

	byPath := map[string]any{}
	for _, e := range evals {
		byPath[e.Path] = e.Value
	}
	assert.Equal(t, "acme", byPath["data.tenant"],
		"widgetDataTemplate jq referencing an extras key must yield the extras value")
	// jqutil.InferType narrows a JSON integer; accept any integer width.
	assert.EqualValues(t, 3, toInt(t, byPath["data.count"]),
		"widgetDataTemplate jq referencing a numeric extras key must yield the numeric value")
}

// TestExtrasReferenceable_InResourcesRefsTemplate is Diego's EXPLICIT case:
// a resourcesRefsTemplate whose jq references an extras key resolves
// correctly. resolveResourceRefs feeds the post-mergeExtras ds into
// resourcesrefstemplate.Resolve, so this is the exact production eval path
// for step 2 over the resourcesRefs path.
func TestExtrasReferenceable_InResourcesRefsTemplate(t *testing.T) {
	ds := map[string]any{}
	mergeExtras(ds, parseExtras(t, `{"targetNs":"team-a","targetName":"my-secret"}`))

	// Non-iterator template: each ${...} field is evaluated directly against
	// ds. The Name/Namespace reference extras keys.
	items := []templatesv1.ResourceRefTemplate{
		{
			Template: templatesv1.ResourceRef{
				ID:         "${ \"ref-\" + .targetName }",
				APIVersion: "v1",
				Resource:   "secrets",
				Name:       "${ .targetName }",
				Namespace:  "${ .targetNs }",
				Verb:       "get",
			},
		},
	}
	refs, err := resourcesrefstemplate.Resolve(context.Background(), items, ds)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, "my-secret", refs[0].Name,
		"resourcesRefsTemplate Name jq referencing an extras key must yield the extras value")
	assert.Equal(t, "team-a", refs[0].Namespace,
		"resourcesRefsTemplate Namespace jq referencing an extras key must yield the extras value")
	assert.Equal(t, "ref-my-secret", refs[0].ID,
		"resourcesRefsTemplate ID jq composing an extras key must resolve")
}

// TestExtrasReferenceable_InResourcesRefsTemplate_Iterator — extras drives an
// ITERATOR jq too (the fan-out shape), proving extras keys are visible to the
// iterator expression, not just the per-field templates.
func TestExtrasReferenceable_InResourcesRefsTemplate_Iterator(t *testing.T) {
	ds := map[string]any{}
	mergeExtras(ds, parseExtras(t, `{"names":["alpha","beta","gamma"],"ns":"team-b"}`))

	items := []templatesv1.ResourceRefTemplate{
		{
			Iterator: ptr.To("${ .names }"),
			Template: templatesv1.ResourceRef{
				// Inside the iterator, "." is each name string.
				Name:       "${ . }",
				Namespace:  "team-b", // literal — exercises the static path too
				Resource:   "configmaps",
				APIVersion: "v1",
				Verb:       "get",
			},
		},
	}
	refs, err := resourcesrefstemplate.Resolve(context.Background(), items, ds)
	require.NoError(t, err)
	require.Len(t, refs, 3, "iterator over an extras array must fan out one ref per element")
	gotNames := []string{refs[0].Name, refs[1].Name, refs[2].Name}
	assert.ElementsMatch(t, []string{"alpha", "beta", "gamma"}, gotNames,
		"each fanned-out ref Name must equal the corresponding extras array element")
}

// TestExtras_ApiRefResultWins_OverExtras_InTemplate — end-to-end precedence
// through the widgetDataTemplate path: when ds already carries an apiRef key,
// the template sees the apiRef value, NOT the colliding extras value.
func TestExtras_ApiRefResultWins_OverExtras_InTemplate(t *testing.T) {
	// ds simulates the apiRef-RA result already holding "tenant".
	ds := map[string]any{"tenant": "from-apiRef"}
	mergeExtras(ds, parseExtras(t, `{"tenant":"from-extras","extra":"only-in-extras"}`))

	items := []templatesv1.WidgetDataTemplate{
		{ForPath: "data.tenant", Expression: "${ .tenant }"},
		{ForPath: "data.extra", Expression: "${ .extra }"},
	}
	evals, err := widgetdatatemplate.Resolve(context.Background(), widgetdatatemplate.ResolveOptions{
		Items:      items,
		DataSource: ds,
	})
	require.NoError(t, err)
	byPath := map[string]any{}
	for _, e := range evals {
		byPath[e.Path] = e.Value
	}
	assert.Equal(t, "from-apiRef", byPath["data.tenant"],
		"the apiRef-result key MUST win over the colliding extras key at template eval")
	assert.Equal(t, "only-in-extras", byPath["data.extra"],
		"a non-colliding extras key is still referenceable")
}

// TestExtras_NoExtras_TemplateUnchanged — backward-compat at the template
// layer: with no extras merged, a template referencing a key that only extras
// would have supplied evaluates to jq null (the pre-feature behaviour), and a
// template that does NOT reference extras is unaffected.
func TestExtras_NoExtras_TemplateUnchanged(t *testing.T) {
	dsBase := map[string]any{"list": []any{map[string]any{"a": 1}, map[string]any{"a": 2}}}

	// (a) no extras merged — a .tenant reference yields null (key absent).
	dsA := map[string]any{"list": dsBase["list"]}
	mergeExtras(dsA, nil)
	// (b) empty extras merged — must be identical to (a).
	dsB := map[string]any{"list": dsBase["list"]}
	mergeExtras(dsB, map[string]any{})

	items := []templatesv1.WidgetDataTemplate{
		{ForPath: "data.count", Expression: "${ .list | length }"},
		{ForPath: "data.tenant", Expression: "${ .tenant }"},
	}
	evalsA, err := widgetdatatemplate.Resolve(context.Background(), widgetdatatemplate.ResolveOptions{Items: items, DataSource: dsA})
	require.NoError(t, err)
	evalsB, err := widgetdatatemplate.Resolve(context.Background(), widgetdatatemplate.ResolveOptions{Items: items, DataSource: dsB})
	require.NoError(t, err)

	require.Equal(t, len(evalsA), len(evalsB))
	for i := range evalsA {
		assert.Equal(t, evalsA[i].Path, evalsB[i].Path)
		assert.Equal(t, evalsA[i].Value, evalsB[i].Value,
			"nil-extras and empty-extras must produce byte-identical template output")
	}
	// The non-extras template (count) is unaffected by the absence of extras.
	for _, e := range evalsA {
		if e.Path == "data.count" {
			assert.EqualValues(t, 2, toInt(t, e.Value), "a template that ignores extras is unchanged when extras is absent")
		}
	}
}

// ===========================================================================
// inline-extras design P — resolver-side helper + accessor falsifiers.
// These unit-test the NEW merge direction (mergeRequestWins: request wins) and
// the two accessors. The end-to-end per-surface + scope-isolation + input-only
// proofs live in extras_integration_test.go (real cluster).
// ===========================================================================

// TestMergeRequestWins_RequestOverridesInline is the per-surface precedence
// falsifier at the helper level (design §4, falsifier #2 core): a key declared
// BOTH inline AND via the request resolves to the REQUEST value. This is the
// load-bearing inversion of mergeExtras — the route is the more-specific
// intent and MUST win over an inline default.
func TestMergeRequestWins_RequestOverridesInline(t *testing.T) {
	inline := parseExtras(t, `{"tenant":"inline-default","onlyInline":"keep"}`)
	request := parseExtras(t, `{"tenant":"from-request","onlyRequest":"add"}`)

	eff := mergeRequestWins(inline, request)

	assert.Equal(t, "from-request", eff["tenant"],
		"collision: the REQUEST value MUST win over the inline default (per-surface precedence)")
	assert.Equal(t, "keep", eff["onlyInline"], "a non-colliding inline key must survive")
	assert.Equal(t, "add", eff["onlyRequest"], "a request-only key must be present")
}

// TestMergeRequestWins_EmptyInputs — backward-compat: both empty ⇒ a fresh
// empty map (an empty effective fold downstream is a no-op). nil inline +
// request ⇒ exactly the request; inline + nil request ⇒ exactly the inline.
func TestMergeRequestWins_EmptyInputs(t *testing.T) {
	assert.Equal(t, map[string]any{}, mergeRequestWins(nil, nil), "both nil ⇒ fresh empty map")
	assert.Equal(t, map[string]any{}, mergeRequestWins(map[string]any{}, map[string]any{}), "both empty ⇒ fresh empty map")

	req := parseExtras(t, `{"a":"1"}`)
	assert.Equal(t, map[string]any{"a": "1"}, mergeRequestWins(nil, req), "nil inline ⇒ exactly the request")

	inl := parseExtras(t, `{"b":"2"}`)
	assert.Equal(t, map[string]any{"b": "2"}, mergeRequestWins(inl, nil), "nil request ⇒ exactly the inline")
}

// TestMergeRequestWins_DeepCopiesBothInputs — the effective map must NOT alias
// EITHER source map. Mutating a nested value through the original inline OR
// request map after the merge must not bleed into the effective map. This is
// the shared-vs-copy isolation that keeps the per-call effective map self-
// contained (feedback_shared_vs_copy_is_a_concurrency_change).
func TestMergeRequestWins_DeepCopiesBothInputs(t *testing.T) {
	inlineNested := map[string]any{"k": "inline-orig"}
	requestNested := map[string]any{"k": "request-orig"}
	inline := map[string]any{"i": inlineNested}
	request := map[string]any{"r": requestNested}

	eff := mergeRequestWins(inline, request)

	inlineNested["k"] = "inline-MUTATED"
	requestNested["k"] = "request-MUTATED"

	gi, _ := eff["i"].(map[string]any)
	gr, _ := eff["r"].(map[string]any)
	require.NotNil(t, gi)
	require.NotNil(t, gr)
	assert.Equal(t, "inline-orig", gi["k"], "effective map must hold a DEEP COPY of the inline source")
	assert.Equal(t, "request-orig", gr["k"], "effective map must hold a DEEP COPY of the request source")
}

// TestMergeRequestWins_Race — falsifier #9 (merge-helper half). The dispatcher
// + seed call the merge helpers concurrently across in-flight /calls reading
// the SAME shared CR-derived maps. Run mergeRequestWins concurrently over
// SHARED source maps under `-race`; the sources must never be mutated and
// every result must be equivalent. A data race here = the shared-vs-copy
// hazard the design's #9 guards.
func TestMergeRequestWins_Race(t *testing.T) {
	sharedInline := parseExtras(t, `{"tenant":"acme","nested":{"k":"v"}}`)
	sharedRequest := parseExtras(t, `{"region":"eu"}`)

	const goroutines = 32
	done := make(chan struct{}, goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			eff := mergeRequestWins(sharedInline, sharedRequest)
			// Mutate THIS goroutine's result — must not bleed into the shared sources.
			eff["scratch"] = "x"
			if n, ok := eff["nested"].(map[string]any); ok {
				n["k"] = "mutated-locally"
			}
		}()
	}
	for i := 0; i < goroutines; i++ {
		<-done
	}
	// Sources untouched after all concurrent merges + local mutations.
	assert.Equal(t, "acme", sharedInline["tenant"], "shared inline source must be untouched by concurrent merges")
	nested, _ := sharedInline["nested"].(map[string]any)
	require.NotNil(t, nested)
	assert.Equal(t, "v", nested["k"], "shared inline nested value must be untouched (deep copy isolated it)")
	assert.Equal(t, "eu", sharedRequest["region"], "shared request source must be untouched")
}

// TestGetApiRefExtras_ReadsSubKey_AbsentReturnsEmpty — the accessor reads the
// `extras` SUB-KEY off spec.apiRef directly (NOT through GetApiRef's
// ObjectReference unmarshal). Present ⇒ the map; absent apiRef OR absent
// extras OR a typed-miss ⇒ {} (backward-compat no-op). Also proves it does NOT
// alias the CR (deep copy).
func TestGetApiRefExtras_ReadsSubKey_AbsentReturnsEmpty(t *testing.T) {
	// present
	obj := map[string]any{"spec": map[string]any{"apiRef": map[string]any{
		"name": "ra", "namespace": "ns", "extras": map[string]any{"tenant": "acme"},
	}}}
	got := GetApiRefExtras(obj)
	assert.Equal(t, "acme", got["tenant"], "must read spec.apiRef.extras")
	got["tenant"] = "mutated" // must not bleed into the CR
	reread := GetApiRefExtras(obj)
	assert.Equal(t, "acme", reread["tenant"], "accessor must return a deep copy — no aliasing of the CR")

	// absent extras sub-key
	assert.Equal(t, map[string]any{}, GetApiRefExtras(map[string]any{
		"spec": map[string]any{"apiRef": map[string]any{"name": "ra"}}}),
		"absent extras sub-key ⇒ {}")
	// absent apiRef
	assert.Equal(t, map[string]any{}, GetApiRefExtras(map[string]any{"spec": map[string]any{}}),
		"absent apiRef ⇒ {}")
	// typed-miss (extras is not a map)
	assert.Equal(t, map[string]any{}, GetApiRefExtras(map[string]any{
		"spec": map[string]any{"apiRef": map[string]any{"extras": "not-a-map"}}}),
		"typed-miss ⇒ {}")
}

// TestGetResourcesRefsExtras_ReadsSiblingBlock_AbsentReturnsEmpty — the
// accessor reads the SIBLING block spec.resourcesRefsTemplateExtras (NOT a
// sub-key of the resourcesRefsTemplate slice). Present ⇒ the map; absent ⇒ {}.
func TestGetResourcesRefsExtras_ReadsSiblingBlock_AbsentReturnsEmpty(t *testing.T) {
	obj := map[string]any{"spec": map[string]any{
		"resourcesRefsTemplate":       []any{map[string]any{"iterator": "${ .x }"}},
		"resourcesRefsTemplateExtras": map[string]any{"targetNs": "team-a"},
	}}
	got := GetResourcesRefsExtras(obj)
	assert.Equal(t, "team-a", got["targetNs"], "must read the sibling spec.resourcesRefsTemplateExtras block")
	got["targetNs"] = "mutated"
	assert.Equal(t, "team-a", GetResourcesRefsExtras(obj)["targetNs"], "accessor must return a deep copy")

	// absent block ⇒ {}
	assert.Equal(t, map[string]any{}, GetResourcesRefsExtras(map[string]any{
		"spec": map[string]any{"resourcesRefsTemplate": []any{}}}),
		"absent resourcesRefsTemplateExtras ⇒ {}")
}

// TestInlineExtras_ApiRefInlineReferenceable_InTemplate — falsifier #1a/#2a
// CORE at the helper level: the apiRef-effective map (inline folded under
// request) is what reaches the apiRef RA dict + transitively `ds`. With NO
// request, the inline value is referenceable (#1a); with a colliding request
// key, the request value wins (#2a). Exercised through the real template eval
// (the same path ds feeds in production).
func TestInlineExtras_ApiRefInlineReferenceable_InTemplate(t *testing.T) {
	// #1a — inline only (request empty): inline value reaches the template.
	effInlineOnly := mergeRequestWins(parseExtras(t, `{"tenant":"inline-acme"}`), map[string]any{})
	dsA := map[string]any{}
	mergeExtras(dsA, effInlineOnly) // ds seeded from the apiRef-effective map (apiRef-less ⇒ no result to win)
	evA := evalTenant(t, dsA)
	assert.Equal(t, "inline-acme", evA, "#1a: apiRef-inline value (no request) must be referenceable in the template")

	// #2a — request overrides inline at the apiRef surface.
	effOverride := mergeRequestWins(parseExtras(t, `{"tenant":"inline-acme"}`), parseExtras(t, `{"tenant":"req-globex"}`))
	dsB := map[string]any{}
	mergeExtras(dsB, effOverride)
	evB := evalTenant(t, dsB)
	assert.Equal(t, "req-globex", evB, "#2a: the REQUEST value MUST win over the apiRef-inline default")
}

// TestInlineExtras_3_ApiRefResultWinsOverInline — falsifier #3. The apiRef RA
// stage output MUST win over an apiRef.extras default on the SAME key (guards
// against accidentally flipping mergeExtras's direction). Models the production
// chain: api.Resolve seeds its dict from the apiRef-EFFECTIVE map
// (mergeRequestWins(apiRefInline, request)) and the API result then OVERWRITES
// the dict on collision (restactions/api/resolve.go:228-230) — so the result
// becomes `ds`, where it dominates. Here we model the post-api ds (carrying the
// result value) and confirm the inline default never displaces it.
func TestInlineExtras_3_ApiRefResultWinsOverInline(t *testing.T) {
	// apiRef-effective dict seeded for the fetch (inline default + request).
	apiRefEff := mergeRequestWins(parseExtras(t, `{"tenant":"inline-default"}`), map[string]any{})
	assert.Equal(t, "inline-default", apiRefEff["tenant"], "pre-fetch dict carries the inline default")

	// The apiRef RA result overwrites `tenant` on collision (api.Resolve dict
	// precedence) → ds holds the RESULT value. Model the resulting ds:
	ds := map[string]any{"tenant": "from-apiRef-result"}
	// Request extras then merge into ds non-overwriting (resolve.go:103) — does
	// not displace the result either.
	mergeExtras(ds, map[string]any{})

	got := evalTenant(t, ds)
	assert.Equal(t, "from-apiRef-result", got,
		"#3: the apiRef-RESULT MUST win over the apiRef.extras inline default at template eval (mergeExtras direction must NOT be flipped)")
}

// TestInlineExtras_2b_RequestOverridesRrtInline — falsifier #2b at the resolver
// level (resourcesRefsTemplate surface). The request extras are folded into ds
// at resolve.go:103 (mergeExtras(ds, opts.Extras)) BEFORE the §4.2 rrt-inline
// fold (mergeExtras(ds, rrtInline)). Since mergeExtras is non-overwriting
// (ds-wins) and the request value is ALREADY in ds, the rrt-inline value on the
// same key does NOT displace it → REQUEST wins over rrt-inline. Models that
// exact two-step ds mutation order and confirms the request value survives.
func TestInlineExtras_2b_RequestOverridesRrtInline(t *testing.T) {
	ds := map[string]any{}
	// Step 1 (resolve.go:103) — request extras into ds.
	mergeExtras(ds, parseExtras(t, `{"targetNs":"req-team"}`))
	// Step 2 (§4.2) — rrt-inline into ds, non-overwriting (ds/request wins).
	mergeExtras(ds, parseExtras(t, `{"targetNs":"rrt-inline-team","onlyRrt":"keep"}`))

	// The resourcesRefsTemplate jq sees the REQUEST value on the colliding key,
	// and the rrt-only key still fills.
	items := []templatesv1.ResourceRefTemplate{
		{Template: templatesv1.ResourceRef{
			ID: "${ .onlyRrt }", APIVersion: "v1", Resource: "namespaces",
			Namespace: "${ .targetNs }", Verb: "get",
		}},
	}
	refs, err := resourcesrefstemplate.Resolve(context.Background(), items, ds)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, "req-team", refs[0].Namespace,
		"#2b: the REQUEST value MUST win over the rrt-inline default on a colliding key")
	assert.Equal(t, "keep", refs[0].ID, "a non-colliding rrt-inline key is still referenceable")
}

// evalTenant runs a widgetDataTemplate `${ .tenant }` against ds and returns
// the resolved value (the real template eval path).
func evalTenant(t *testing.T, ds map[string]any) any {
	t.Helper()
	evals, err := widgetdatatemplate.Resolve(context.Background(), widgetdatatemplate.ResolveOptions{
		Items:      []templatesv1.WidgetDataTemplate{{ForPath: "data.tenant", Expression: "${ .tenant }"}},
		DataSource: ds,
	})
	require.NoError(t, err)
	require.Len(t, evals, 1)
	return evals[0].Value
}

// toInt narrows jqutil.InferType's integer result (int/int32/int64/float64)
// to int for width-agnostic numeric assertions.
func toInt(t *testing.T, v any) int {
	t.Helper()
	switch x := v.(type) {
	case int:
		return x
	case int32:
		return int(x)
	case int64:
		return int(x)
	case float64:
		return int(x)
	default:
		t.Fatalf("expected a numeric value, got %T (%v)", v, v)
		return 0
	}
}
