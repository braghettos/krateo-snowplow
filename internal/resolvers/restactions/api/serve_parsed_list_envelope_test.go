// serve_parsed_list_envelope_test.go — Ship 0.30.242 H.c-layered Phase 3
// gap-fill from 2c.
//
// Unit tests for serveParsedListEnvelope (apistage.go:621), the
// Phase 2b replacement for the deleted gateListItemsWithMemo.
//
// CONTRACT (design §3.4 + apistage.go header comment)
//
// The cell holds items that THIS BindingUID's authorisation already
// permitted at populate time (the cell key folds BindingUID). The
// serve path renders the cell's pre-parsed items directly — NO
// per-item RBAC filtering, NO per-cohort gating. The returned
// envelope is shaped like the apiserver's LIST response:
//   {"apiVersion": <kind>, "kind": <kind>, "items": [...]any}
//
// served=false ONLY when parsed.items is nil (defensive guard against
// a malformed cell). The fail-closed branch falls through to apiserver.
//
// COVERAGE AXES
//
//   (1) Nil items input → (nil, false). Defensive guard.
//   (2) Empty (non-nil) items slice → (envelope, true) with items=[].
//       The cell is well-formed but the LIST happens to be empty —
//       served=true is correct (a valid empty list, not a malformed
//       cell). Apiserver behavior parity: an empty LIST returns 200
//       with items=[].
//   (3) Single item → envelope.items contains exactly the input's
//       .Object map.
//   (4) Multiple items → envelope.items contains every input's
//       .Object map in the same order (order preservation contract).
//   (5) Nil entry inside items slice → skipped (the nil-skip branch
//       at line 627). Defensive.
//   (6) apiVersion + kind round-trip on the envelope.
//   (7) NO per-item RBAC filtering — items with arbitrary content
//       (including content that would be denied under any cohort-
//       narrow RBAC policy) appear UNCHANGED in the served envelope.
//       This is the v4 invariant: RBAC narrowing happened at populate
//       time; serve is pure render.
//
// OUT OF SCOPE
//
//   - BindingUID-keyed cell write: F6 covers (raFullListServe → Put).
//   - Empty BindingUID / cache=off: F7 covers the invariant; this
//     function takes neither a BindingUID nor a cache handle.
//   - Per-item RBAC filtering INSIDE the function: by construction
//     the function takes no RBAC context, but axis (7) makes the
//     "no filter" claim empirical rather than structural.

package api

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// makeUnstructuredItem is a fixture helper for building items the
// production parseListEnvelope would emit.
func makeUnstructuredItem(name, ns string, extra map[string]any) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": "widgets.templates.krateo.io/v1beta1",
		"kind":       "Widget",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
		},
	}
	for k, v := range extra {
		obj[k] = v
	}
	u := &unstructured.Unstructured{Object: obj}
	u.SetCreationTimestamp(metav1.Now())
	return u
}

// ──────────────────────────────────────────────────────────────────────
// Axis 1 — Nil items input
// ──────────────────────────────────────────────────────────────────────

// TestServeParsedListEnvelope_NilItems_ReturnsFalse — the defensive
// fail-closed branch. A nil items slice indicates a malformed cell
// (populator bug or corrupted entry); the caller falls through to
// apiserver (same shape as gateContentEnvelope's RawJSON-unmarshal
// fallback).
func TestServeParsedListEnvelope_NilItems_ReturnsFalse(t *testing.T) {
	parsed := parsedListEnvelope{
		items:      nil,
		apiVersion: "widgets.templates.krateo.io/v1beta1",
		kind:       "WidgetList",
	}
	got, served := serveParsedListEnvelope(parsed)
	if served {
		t.Fatalf("nil items: expected served=false, got served=true")
	}
	if got != nil {
		t.Fatalf("nil items: expected envelope=nil, got %+v", got)
	}
}

// ──────────────────────────────────────────────────────────────────────
// Axis 2 — Empty (non-nil) items slice
// ──────────────────────────────────────────────────────────────────────

// TestServeParsedListEnvelope_EmptyItems_ServesValidEmptyList — a
// well-formed cell holding an empty list. Apiserver parity: this is
// NOT a malformed cell; it's a valid LIST response with zero items.
// served=true, envelope.items=[] (empty slice, not nil).
func TestServeParsedListEnvelope_EmptyItems_ServesValidEmptyList(t *testing.T) {
	parsed := parsedListEnvelope{
		items:      []*unstructured.Unstructured{},
		apiVersion: "widgets.templates.krateo.io/v1beta1",
		kind:       "WidgetList",
	}
	got, served := serveParsedListEnvelope(parsed)
	if !served {
		t.Fatalf("empty items: expected served=true (valid empty list), got served=false")
	}
	env, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("empty items: expected map[string]any envelope, got %T", got)
	}
	if env["apiVersion"] != "widgets.templates.krateo.io/v1beta1" {
		t.Fatalf("empty items: apiVersion mismatch: got %v", env["apiVersion"])
	}
	if env["kind"] != "WidgetList" {
		t.Fatalf("empty items: kind mismatch: got %v", env["kind"])
	}
	items, ok := env["items"].([]any)
	if !ok {
		t.Fatalf("empty items: expected items=[]any, got %T", env["items"])
	}
	if len(items) != 0 {
		t.Fatalf("empty items: expected len(items)=0, got %d", len(items))
	}
}

// ──────────────────────────────────────────────────────────────────────
// Axis 3 — Single item
// ──────────────────────────────────────────────────────────────────────

// TestServeParsedListEnvelope_SingleItem_RoundTrip asserts the
// envelope's items slice contains exactly the input's .Object map.
func TestServeParsedListEnvelope_SingleItem_RoundTrip(t *testing.T) {
	item := makeUnstructuredItem("w-1", "ns-A", nil)
	parsed := parsedListEnvelope{
		items:      []*unstructured.Unstructured{item},
		apiVersion: "widgets.templates.krateo.io/v1beta1",
		kind:       "WidgetList",
	}
	got, served := serveParsedListEnvelope(parsed)
	if !served {
		t.Fatalf("single item: expected served=true, got served=false")
	}
	env := got.(map[string]any)
	items := env["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("single item: expected 1 item, got %d", len(items))
	}
	obj, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("single item: expected map[string]any item, got %T", items[0])
	}
	meta := obj["metadata"].(map[string]any)
	if meta["name"] != "w-1" {
		t.Fatalf("single item: name mismatch: got %v", meta["name"])
	}
	if meta["namespace"] != "ns-A" {
		t.Fatalf("single item: namespace mismatch: got %v", meta["namespace"])
	}
}

// ──────────────────────────────────────────────────────────────────────
// Axis 4 — Multiple items, order preserved
// ──────────────────────────────────────────────────────────────────────

// TestServeParsedListEnvelope_MultipleItems_OrderPreserved asserts
// items appear in the served envelope in the SAME ORDER as in the
// input parsed envelope. Order matters because the apistage stage's
// downstream jq filter may rely on stable ordering.
func TestServeParsedListEnvelope_MultipleItems_OrderPreserved(t *testing.T) {
	in := []*unstructured.Unstructured{
		makeUnstructuredItem("w-alpha", "ns-A", nil),
		makeUnstructuredItem("w-beta", "ns-B", nil),
		makeUnstructuredItem("w-gamma", "ns-A", nil),
		makeUnstructuredItem("w-delta", "ns-C", nil),
	}
	parsed := parsedListEnvelope{
		items:      in,
		apiVersion: "widgets.templates.krateo.io/v1beta1",
		kind:       "WidgetList",
	}
	got, served := serveParsedListEnvelope(parsed)
	if !served {
		t.Fatalf("multiple items: expected served=true")
	}
	items := got.(map[string]any)["items"].([]any)
	if len(items) != len(in) {
		t.Fatalf("multiple items: expected %d items, got %d", len(in), len(items))
	}
	for i, item := range items {
		gotName := item.(map[string]any)["metadata"].(map[string]any)["name"]
		wantName, _ := in[i].Object["metadata"].(map[string]any)["name"].(string)
		if gotName != wantName {
			t.Fatalf("multiple items: order broken at index %d: got name=%v, want %q", i, gotName, wantName)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────
// Axis 5 — Nil entry inside items slice
// ──────────────────────────────────────────────────────────────────────

// TestServeParsedListEnvelope_NilEntries_Skipped asserts that nil
// pointers inside the items slice (defensive — should never happen
// in practice but the code has an `if it != nil` guard) are SKIPPED
// silently, not propagated as nil entries in the served envelope.
func TestServeParsedListEnvelope_NilEntries_Skipped(t *testing.T) {
	in := []*unstructured.Unstructured{
		makeUnstructuredItem("w-real-1", "ns-A", nil),
		nil,
		makeUnstructuredItem("w-real-2", "ns-B", nil),
		nil,
		makeUnstructuredItem("w-real-3", "ns-C", nil),
	}
	parsed := parsedListEnvelope{
		items:      in,
		apiVersion: "widgets.templates.krateo.io/v1beta1",
		kind:       "WidgetList",
	}
	got, served := serveParsedListEnvelope(parsed)
	if !served {
		t.Fatalf("nil entries: expected served=true")
	}
	items := got.(map[string]any)["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("nil entries: expected 3 real items (nil entries skipped), got %d", len(items))
	}
	wantNames := []string{"w-real-1", "w-real-2", "w-real-3"}
	for i, item := range items {
		gotName := item.(map[string]any)["metadata"].(map[string]any)["name"]
		if gotName != wantNames[i] {
			t.Fatalf("nil entries: at index %d got %v, want %s", i, gotName, wantNames[i])
		}
	}
}

// ──────────────────────────────────────────────────────────────────────
// Axis 6 — apiVersion + kind round-trip
// ──────────────────────────────────────────────────────────────────────

// TestServeParsedListEnvelope_APIVersionKind_RoundTrip cycles through
// several GVK shapes (different groups, versions, kinds) and asserts
// the served envelope echoes them verbatim.
func TestServeParsedListEnvelope_APIVersionKind_RoundTrip(t *testing.T) {
	cases := []struct {
		apiVersion string
		kind       string
	}{
		{"widgets.templates.krateo.io/v1beta1", "WidgetList"},
		{"composition.krateo.io/v1-2-2", "GithubScaffoldingWithCompositionPageList"},
		{"templates.krateo.io/v1", "RESTActionList"},
		{"v1", "NamespaceList"},
		{"", ""}, // edge: empty GVK (defensive — should not panic)
	}
	for _, tc := range cases {
		t.Run(tc.apiVersion+"/"+tc.kind, func(t *testing.T) {
			parsed := parsedListEnvelope{
				items:      []*unstructured.Unstructured{makeUnstructuredItem("w", "ns", nil)},
				apiVersion: tc.apiVersion,
				kind:       tc.kind,
			}
			got, served := serveParsedListEnvelope(parsed)
			if !served {
				t.Fatalf("served=false")
			}
			env := got.(map[string]any)
			if env["apiVersion"] != tc.apiVersion {
				t.Fatalf("apiVersion round-trip: got %v, want %q", env["apiVersion"], tc.apiVersion)
			}
			if env["kind"] != tc.kind {
				t.Fatalf("kind round-trip: got %v, want %q", env["kind"], tc.kind)
			}
		})
	}
}

// ──────────────────────────────────────────────────────────────────────
// Axis 7 — NO per-item RBAC filtering (v4 invariant)
// ──────────────────────────────────────────────────────────────────────

// TestServeParsedListEnvelope_NoPerItemRBACFiltering asserts the
// function does NOT filter items by ANY content-based predicate.
// Items with arbitrary content (including content that would be
// denied under any conceivable cohort-narrow RBAC policy) appear
// UNCHANGED in the served envelope.
//
// This is the v4 invariant ratified at 2b (per F1 deletion decision):
// RBAC narrowing happens at populate time (the cell key folds the
// BindingUID), not at serve time. The deleted gateListItemsWithMemo
// USED to filter at serve time via the cohort-gate-memo; H.c-layered
// removes that filter entirely.
//
// The test passes items spanning multiple "would-be-denied"
// namespaces (the kind of mix that previously would have been
// filtered by a cohort-gate-memo's keptNames). All MUST appear in
// the served envelope.
func TestServeParsedListEnvelope_NoPerItemRBACFiltering(t *testing.T) {
	// Items in multiple namespaces — what a cohort-gate-memo would
	// have narrowed to "permitted namespaces" only at serve time.
	in := []*unstructured.Unstructured{
		makeUnstructuredItem("w-broad", "krateo-system", nil),       // would-be-permitted
		makeUnstructuredItem("w-narrow-a", "team-a", nil),             // would-be-denied
		makeUnstructuredItem("w-narrow-b", "team-b", nil),             // would-be-denied
		makeUnstructuredItem("w-narrow-c", "team-c", nil),             // would-be-denied
		makeUnstructuredItem("w-narrow-d", "team-d", nil),             // would-be-denied
		makeUnstructuredItem("w-sensitive", "kube-system", nil),       // would-be-denied
	}
	parsed := parsedListEnvelope{
		items:      in,
		apiVersion: "widgets.templates.krateo.io/v1beta1",
		kind:       "WidgetList",
	}
	got, served := serveParsedListEnvelope(parsed)
	if !served {
		t.Fatalf("no-filter: expected served=true")
	}
	items := got.(map[string]any)["items"].([]any)
	if len(items) != len(in) {
		t.Fatalf("v4 INVARIANT BROKEN: serveParsedListEnvelope filtered items (got %d, expected all %d). "+
			"This function MUST NOT apply per-item RBAC filtering — that happened at populate time "+
			"(the cell key folds the BindingUID). Serve is pure render under H.c-layered (design §3.4).",
			len(items), len(in))
	}
	// Verify each "would-be-denied" item is present.
	gotNames := map[string]bool{}
	for _, it := range items {
		name := it.(map[string]any)["metadata"].(map[string]any)["name"].(string)
		gotNames[name] = true
	}
	wantNames := []string{"w-broad", "w-narrow-a", "w-narrow-b", "w-narrow-c", "w-narrow-d", "w-sensitive"}
	for _, n := range wantNames {
		if !gotNames[n] {
			t.Fatalf("v4 INVARIANT BROKEN: item %q missing from served envelope — serve filtered it out", n)
		}
	}
}
