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

