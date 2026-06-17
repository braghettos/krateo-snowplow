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
