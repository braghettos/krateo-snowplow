// refilter_layering_test.go — Ship 0.30.235 permanent regression gate.
//
// Falsifier-first per feedback_falsifier_first_before_ship.md: pre-fix
// binary (dbbea37) FAILS (jsonHandlerOptions has no uaf field → compile
// error → no in-handler UAF path exists); post-fix binary PASSES (UAF
// runs on the raw envelope, projection narrows the permitted subset).
// Generic GVRs — no per-RA literals per feedback_no_special_cases.md.

package api

import (
	"encoding/json"
	"testing"

	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestRefilterLayering_UAFOnRawEnvelope is the permanent unit gate for
// Ship 0.30.235. It encodes the layering contract: a UAF NamespaceFrom
// expression evaluates against the K8s-canonical raw envelope shape
// (with .metadata), NOT the stage-filter-projected shape.
//
// Fixture (mirrors the live-cluster RA's projecting-filter spec):
//   - Stage filter projects items to {uid, name, ns} — DROPS .metadata.
//   - UAF NamespaceFrom = ".metadata.namespace" — needs .metadata.
//   - RBAC: namespace-scoped RB grants User "user1" list on
//     example.test/anyplural in ns "ns-a" only. User1 has
//     NO cluster-scope grant.
//   - Envelope: 2 items, one in ns-a (permitted) and one in ns-b
//     (denied).
//
// Asserted invariant: after the resolver-internal flow (jsonHandlerCore
// applies UAF before filter; no post-flow UAF call), dict["testStage"]
// has exactly 1 projected item with ns="ns-a".
//
// Pre-Ship-0.30.235 binary FAILS this test (compile or runtime — see
// header). Post-Ship-0.30.235 binary PASSES.
func TestRefilterLayering_UAFOnRawEnvelope(t *testing.T) {
	// RBAC fixture — User-kind RB scoped to ns-a, granting list on
	// example.test/anyplural. Generic GVR (no per-RA / per-
	// resource carve-out).
	role := &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{Name: "anyplural-lister", Namespace: "ns-a"},
		Rules: []rbacv1.PolicyRule{
			{
				Verbs:     []string{"list", "get", "watch"},
				APIGroups: []string{"example.test"},
				Resources: []string{"anyplural"},
			},
		},
	}
	binding := &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: "anyplural-lister-binding", Namespace: "ns-a"},
		Subjects: []rbacv1.Subject{
			{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: "user1"},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "anyplural-lister",
		},
	}
	newRefilterTestWatcher(t, role, binding)

	// Synthetic stage spec: filter projects items down to {uid, name,
	// ns} (no .metadata); UAF NamespaceFrom references .metadata.namespace.
	apiCall := &templates.API{
		Name:   "testStage",
		Filter: ptr.To("[.testStage.items[]? | {uid: .metadata.uid, name: .metadata.name, ns: .metadata.namespace}]"),
		UserAccessFilter: &templates.UserAccessFilterSpec{
			Verb:          "list",
			Group:         "example.test",
			Resource:      "anyplural",
			NamespaceFrom: ".metadata.namespace",
		},
	}

	// Raw envelope — the SA-cluster-scope LIST shape with 2 items.
	// Each item carries FULL .metadata (the shape UAF documents) and
	// is otherwise opaque (kind, apiVersion, status — irrelevant to
	// the layering test).
	envelope := map[string]any{
		"kind":       "AnyPluralList",
		"apiVersion": "example.test/v1",
		"items": []any{
			map[string]any{
				"kind":       "AnyPlural",
				"apiVersion": "example.test/v1",
				"metadata": map[string]any{
					"uid":       "uid-a",
					"name":      "item-a",
					"namespace": "ns-a",
				},
			},
			map[string]any{
				"kind":       "AnyPlural",
				"apiVersion": "example.test/v1",
				"metadata": map[string]any{
					"uid":       "uid-b",
					"name":      "item-b",
					"namespace": "ns-b",
				},
			},
		},
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	// Drive the resolver-internal flow through jsonHandlerCore. On the
	// post-fix binary the UAF spec wired into hOpts causes UAF to run
	// against the raw envelope BEFORE the stage filter; the projection
	// then operates on the already-narrowed envelope. On the pre-fix
	// binary `uaf`/`apiCallName` fields do not exist on
	// jsonHandlerOptions and this references is a compile error — the
	// captured falsifier (per T1).
	dict := make(map[string]any)
	hOpts := jsonHandlerOptions{
		key:         apiCall.Name,
		out:         dict,
		filter:      apiCall.Filter,
		uaf:         apiCall.UserAccessFilter,
		apiCallName: apiCall.Name,
	}

	ctx := ctxWithUser("user1")
	handler := jsonHandlerBytes(ctx, hOpts)
	if err := handler(raw); err != nil {
		t.Fatalf("jsonHandlerBytes: %v", err)
	}

	// Assertion: dict["testStage"] holds the projected slice with
	// EXACTLY 1 item — the ns-a one (user1's namespace-scoped grant).
	// The ns-b item must have been denied by UAF on the raw envelope
	// shape (where .metadata.namespace = "ns-b" → ns-b cluster does
	// not grant user1 → drop).
	got, ok := dict[apiCall.Name]
	if !ok {
		t.Fatalf("dict[%q] missing", apiCall.Name)
	}
	items, ok := got.([]any)
	if !ok {
		t.Fatalf("dict[%q] is not a slice; got %T (%v)", apiCall.Name, got, got)
	}
	if len(items) != 1 {
		t.Fatalf("layering bug: dict[%q] has %d items; want 1 (the ns-a item). full slice = %v",
			apiCall.Name, len(items), items)
	}
	first, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("kept item is not a map; got %T (%v)", items[0], items[0])
	}
	if first["ns"] != "ns-a" {
		t.Fatalf("kept item ns = %v; want ns-a (the user1-permitted namespace)", first["ns"])
	}
	if first["uid"] != "uid-a" {
		t.Fatalf("kept item uid = %v; want uid-a", first["uid"])
	}
	if first["name"] != "item-a" {
		t.Fatalf("kept item name = %v; want item-a", first["name"])
	}
}

// TestUAFRefilter_ResourcesFromMultiStage_ResolvesAgainstDictNotPig is
// Ship K / 0.30.245's permanent regression gate.
//
// THE BUG (preserved on the H.c-layered branch from 0.30.235)
//
// `applyUserAccessFilterOnPig` called
// `resolveUAFResources(ctx, log, uaf, pig)`. `pig` is the per-stage
// {opts.key: rawEnvelope} scope — it never contains UPSTREAM stage
// keys. The compositions-list RA's allCompositions stage has
// `resourcesFrom: '[ (.crds // [])[] | .plural ]'` which references
// `.crds` populated by the PRIOR `crds` stage. With pig-scope
// evaluation:
//
//   pig = {allCompositions: <envelope>}   // no .crds key
//   .crds // []  → []
//   resourcesFrom → []                    // empty resource set
//   resolveUAFResources fails-closed      // → drops every item
//
// The pre-Ship-K piechart count for compositions-list was always 0 on
// the live cluster, regardless of the user's RBAC grants. This test
// captures that contract violation and asserts the fix.
//
// THE FIX (Ship K / 0.30.245)
//
// jsonHandlerOptions gains a `dict` field carrying the resolver's
// accumulated stage-output dict. applyUserAccessFilterOnPig accepts
// `dict` and passes it to resolveUAFResources instead of `pig`. With
// dict-scope evaluation:
//
//   dict = {crds: [...], allCompositions: <envelope>}
//   .crds // []  → [{plural: "compositions"}, ...]
//   resourcesFrom → ["compositions", ...]   // non-empty
//   refilter proceeds per item               // RBAC verdict per ns
//
// DUAL-STATE PROOF
//
// On the pre-Ship-K binary this test FAILS (resourcesFrom resolves to
// `[]` → fail-closed → 0 items kept). On the post-Ship-K binary it
// PASSES (resourcesFrom resolves to `["anyplural"]` → refilter runs
// per item → ns-a item kept).
//
// FIXTURE SHAPE (mirrors the live compositions-list RA structure)
//
//   - Stage 1 ("crds"): returns [{plural: "anyplural"}] — a synthetic
//     CRD-list shape. Resolver writes this to dict["crds"] before the
//     next stage runs.
//   - Stage 2 ("allItems"): the UAF stage. Its `userAccessFilter` has
//     ResourcesFrom = "[ (.crds // [])[] | .plural ]" — references the
//     UPSTREAM `crds` stage's output. NamespaceFrom = ".metadata.namespace"
//     so the per-item refilter checks per-namespace RBAC.
//   - Raw envelope at stage 2: 2 items in ns-a and ns-b.
//   - RBAC: user1 has Role "anyplural-lister" in ns-a only (same as
//     the existing layering test fixture).
//
// Asserted invariant: dict["allItems"] holds exactly 1 item — the ns-a
// one. Pre-Ship-K: 0 items (resourcesFrom returned [] → all dropped).
func TestUAFRefilter_ResourcesFromMultiStage_ResolvesAgainstDictNotPig(t *testing.T) {
	// Same RBAC fixture as TestRefilterLayering_UAFOnRawEnvelope —
	// User-kind RB scoped to ns-a, granting list on example.test/anyplural.
	role := &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{Name: "anyplural-lister", Namespace: "ns-a"},
		Rules: []rbacv1.PolicyRule{
			{
				Verbs:     []string{"list", "get", "watch"},
				APIGroups: []string{"example.test"},
				Resources: []string{"anyplural"},
			},
		},
	}
	binding := &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: "anyplural-lister-binding", Namespace: "ns-a"},
		Subjects: []rbacv1.Subject{
			{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: "user1"},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "anyplural-lister",
		},
	}
	newRefilterTestWatcher(t, role, binding)

	// Construct the dict with an UPSTREAM stage output (`crds`) already
	// populated. This mimics the resolver's state at the moment the
	// next stage's jsonHandlerCore runs — prior stages have completed
	// + written to dict; the current stage is about to write under its
	// own key.
	dict := map[string]any{
		"crds": []any{
			map[string]any{"plural": "anyplural"},
		},
	}

	// Synthetic stage 2 spec — references `.crds` via resourcesFrom.
	// This is the SHAPE the live compositions-list RA's allCompositions
	// stage carries: a multi-stage `resourcesFrom` referencing an
	// upstream stage's output. Group is static; Resource is unset
	// because ResourcesFrom takes over per CRD XOR rule.
	apiCall := &templates.API{
		Name: "allItems",
		// Stage filter projects to {uid, name, ns} — drops .metadata.
		// (Layering test pattern carried forward; the resourcesFrom fix
		// composes with the layering fix.)
		Filter: ptr.To("[.allItems.items[]? | {uid: .metadata.uid, name: .metadata.name, ns: .metadata.namespace}]"),
		UserAccessFilter: &templates.UserAccessFilterSpec{
			Verb:           "list",
			Group:          "example.test",
			ResourcesFrom:  "[ (.crds // [])[] | .plural ]",
			NamespaceFrom:  ".metadata.namespace",
		},
	}

	// Raw envelope at stage 2 — 2 items in ns-a + ns-b.
	envelope := map[string]any{
		"kind":       "AnyPluralList",
		"apiVersion": "example.test/v1",
		"items": []any{
			map[string]any{
				"kind":       "AnyPlural",
				"apiVersion": "example.test/v1",
				"metadata": map[string]any{
					"uid":       "uid-a",
					"name":      "item-a",
					"namespace": "ns-a",
				},
			},
			map[string]any{
				"kind":       "AnyPlural",
				"apiVersion": "example.test/v1",
				"metadata": map[string]any{
					"uid":       "uid-b",
					"name":      "item-b",
					"namespace": "ns-b",
				},
			},
		},
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	// Drive jsonHandlerCore via jsonHandlerBytes — the same surface
	// the resolver uses. CRITICAL: hOpts.dict MUST be set to the
	// upstream-stage-populated dict for Ship K's fix to apply.
	hOpts := jsonHandlerOptions{
		key:         apiCall.Name,
		out:         dict,
		dict:        dict,
		filter:      apiCall.Filter,
		uaf:         apiCall.UserAccessFilter,
		apiCallName: apiCall.Name,
	}

	ctx := ctxWithUser("user1")
	handler := jsonHandlerBytes(ctx, hOpts)
	if err := handler(raw); err != nil {
		t.Fatalf("jsonHandlerBytes: %v", err)
	}

	// Assertion: dict["allItems"] has EXACTLY 1 item — the ns-a one.
	//
	// Pre-Ship-K (resolveUAFResources called with pig instead of dict):
	//   pig = {allItems: <envelope>}   // no .crds key
	//   .crds // []  → []
	//   resourcesFrom → []
	//   refilter fail-closes → setRefilteredEmpty(pig, "allItems")
	//   pig["allItems"] = {"items": []}
	//   filter runs over empty items → dict["allItems"] = []   (0 items)
	//   ASSERTION FAILS: len(items) == 0, want 1.
	//
	// Post-Ship-K (resolveUAFResources called with dict):
	//   dict = {crds: [{plural: "anyplural"}], allItems: <envelope>}
	//   .crds // []  → [{plural: "anyplural"}]
	//   resourcesFrom → ["anyplural"]
	//   refilter walks items per RBAC verdict:
	//     ns-a: user1 has Role "anyplural-lister" in ns-a → KEEP
	//     ns-b: no role/binding for user1 in ns-b → DROP
	//   pig["allItems"]["items"] = [{ns-a item}]
	//   filter projects → dict["allItems"] = [{uid: "uid-a", name: "item-a", ns: "ns-a"}]
	//   ASSERTION PASSES: len(items) == 1, ns="ns-a".
	got, ok := dict[apiCall.Name]
	if !ok {
		t.Fatalf("dict[%q] missing", apiCall.Name)
	}
	items, ok := got.([]any)
	if !ok {
		t.Fatalf("dict[%q] is not a slice; got %T (%v)", apiCall.Name, got, got)
	}
	if len(items) != 1 {
		t.Fatalf("Ship K REGRESSION: dict[%q] has %d items; want 1 (the ns-a item). "+
			"Pre-Ship-K bug: resolveUAFResources was called with pig (per-stage scope, "+
			"no .crds key) → resourcesFrom returned [] → fail-closed dropped every item. "+
			"Post-Ship-K: dict carries upstream stage outputs; resourcesFrom resolves to "+
			"[anyplural] → per-item RBAC verdict → ns-a kept. full slice = %v",
			apiCall.Name, len(items), items)
	}
	first, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("kept item is not a map; got %T (%v)", items[0], items[0])
	}
	if first["ns"] != "ns-a" {
		t.Fatalf("kept item ns = %v; want ns-a", first["ns"])
	}
	if first["uid"] != "uid-a" {
		t.Fatalf("kept item uid = %v; want uid-a", first["uid"])
	}
	if first["name"] != "item-a" {
		t.Fatalf("kept item name = %v; want item-a", first["name"])
	}
}

