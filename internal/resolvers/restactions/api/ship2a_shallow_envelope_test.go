// ship2a_shallow_envelope_test.go — Ship 2a (0.30.209) gates.
//
// Ship 2a removes the per-serve deep copy at listEnvelopeValue
// (apistage.go) and serves a SHALLOW envelope whose items[] ALIAS the
// shared entry.Items[i].Object maps. That is only safe because the gojq
// fork's deleteEmpty (gojq/func.go) is now allocator-aware: it
// copy-on-writes any non-gojq-allocated node instead of writing it, so NO
// gojq path can mutate the shared input — even del/delpaths/|=/map_values.
//
// Gates in this file:
//
//   - TestShip2a_Shallow_DestructiveServe_Race (Gate 1 + Gate 3): N
//     goroutines concurrently build the SHALLOW envelope over ONE shared
//     item set and run DESTRUCTIVE filters (del / delpaths / |= /
//     map_values + a DEEP-SIBLING del) under -race. Must be CLEAN. After
//     the run, the shared item maps must be byte-identical to a pre-run
//     snapshot (proves gojq never wrote the input).
//
//   - TestShip2a_DeleteEmpty_CoW_NoInputMutation: a gojq-level unit test
//     that del/delpaths over a shared input never mutates it and yields
//     the same result as the same op over a private deep copy.
//
//   - TestShip2a_ByteIdentity_OldVsNew (Gate 2): for a valid-filter
//     corpus (read-only AND destructive) INCLUDING a json.Number leaf
//     (the v0.12.17->v0.12.19 normalize-removal delta), assert
//     {deep-copy input + gojq} and {shallow/shared input + gojq} produce
//     reflect.DeepEqual results.
//
// Run with -race. KUBECONFIG=/dev/null.

package api

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"sync"
	"testing"

	"github.com/itchyny/gojq"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ship2aItems builds n composition-shaped items with a nested
// metadata/spec/status tree — the per-item shape listEnvelopeValue serves.
func ship2aItems(n int) []*unstructured.Unstructured {
	items := make([]*unstructured.Unstructured, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "composition.krateo.io/v1",
			"kind":       "Composition",
			"metadata": map[string]any{
				"name":        "comp-" + strconv.Itoa(i),
				"namespace":   "ns-" + strconv.Itoa(i%8),
				"labels":      map[string]any{"app": "bench", "idx": strconv.Itoa(i)},
				"annotations": map[string]any{"krateo.io/x": "y"},
			},
			"spec": map[string]any{
				"values": map[string]any{"region": "eu", "tier": "gold"},
			},
			"status": map[string]any{
				"ready":      true,
				"conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
			},
		}})
	}
	return items
}

// ship2aDestructiveFilters force every gojq write path that reaches input.
// The pig wrapper is {key: <envelope>}; filters read `.result.items[]`.
var ship2aDestructiveFilters = []string{
	// del a nested leaf inside each item — deleteEmpty recurses item maps.
	`{items: [.result.items[] | del(.metadata.annotations)]}`,
	// DEEP-SIBLING del: delete a deep path; deleteEmpty must walk the
	// copy-on-write spine AND the aliased siblings (labels, spec, status).
	`{items: [.result.items[] | del(.spec.values.region)]}`,
	// update-assignment rewrites a nested field.
	`{items: [.result.items[] | (.metadata.labels.app) |= "X"]}`,
	// map_values over each item's metadata.
	`{items: [.result.items[] | .metadata |= map_values(.)]}`,
	// delpaths form with two paths.
	`{items: [.result.items[] | delpaths([["spec","values"],["status","ready"]])]}`,
	// pure read projection (control — no write).
	`{items: [.result.items[] | {n: .metadata.name}]}`,
}

// snapshotItems deep-copies the items' Object trees for a post-run
// byte-identity assertion against the shared input.
func snapshotItems(items []*unstructured.Unstructured) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, deepCopyAny(it.Object).(map[string]any))
	}
	return out
}

// runGojq parses+compiles+drains query over data (read-only consumption).
func runGojq(t *testing.T, query string, data any) any {
	t.Helper()
	code, err := gojq.Parse(query)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}
	c, err := gojq.Compile(code)
	if err != nil {
		t.Fatalf("compile %q: %v", query, err)
	}
	iter := c.RunWithContext(context.Background(), data)
	var first any
	count := 0
	for {
		v, more := iter.Next()
		if !more {
			break
		}
		if rerr, isErr := v.(error); isErr {
			t.Fatalf("runtime %q: %v", query, rerr)
		}
		if count == 0 {
			first = v
		}
		count++
	}
	return first
}

// TestShip2a_Shallow_DestructiveServe_Race is the PRIMARY gate (Gate 1) +
// the post-serve cache-integrity gate (Gate 3).
func TestShip2a_Shallow_DestructiveServe_Race(t *testing.T) {
	// ONE shared backing item set — every goroutine's shallow envelope
	// aliases these exact maps (the production cross-serve sharing).
	shared := ship2aItems(200)
	before := snapshotItems(shared)

	const goroutines = 32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				// SHALLOW envelope — items[] alias the shared maps, exactly
				// as listEnvelopeValue (Ship 2a) produces on a serve.
				env := listEnvelopeValue("composition.krateo.io/v1", "CompositionList", shared)
				// Wrap as the serve does: pig := {key: envelope}.
				pig := map[string]any{"result": env}
				q := ship2aDestructiveFilters[(g+i)%len(ship2aDestructiveFilters)]
				code, err := gojq.Parse(q)
				if err != nil {
					t.Errorf("parse %q: %v", q, err)
					return
				}
				c, err := gojq.Compile(code)
				if err != nil {
					t.Errorf("compile %q: %v", q, err)
					return
				}
				iter := c.RunWithContext(context.Background(), pig)
				for {
					v, more := iter.Next()
					if !more {
						break
					}
					if rerr, isErr := v.(error); isErr {
						t.Errorf("runtime %q: %v", q, rerr)
						return
					}
					// Read-only consumption — the serve marshals the result.
					_, _ = json.Marshal(v)
				}
			}
		}(g)
	}
	wg.Wait()

	// Gate 3 — the shared backing maps must be byte-identical to the
	// pre-run snapshot: gojq never wrote the input.
	after := snapshotItems(shared)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("shared input MUTATED by a destructive serve — Ship 2a is unsafe")
	}
}

// TestShip2a_DeleteEmpty_CoW_NoInputMutation isolates the deleteEmpty CoW
// fix: del/delpaths over a SHARED input must (a) not mutate it and (b)
// yield the same result as the same op over a private deep copy.
func TestShip2a_DeleteEmpty_CoW_NoInputMutation(t *testing.T) {
	cases := []struct {
		name  string
		query string
	}{
		{"del shallow leaf", `del(.a)`},
		{"del deep leaf", `del(.b.c)`},
		{"del deep with siblings", `del(.b.c.d)`},
		{"delpaths multi", `delpaths([["a"],["b","c","d"]])`},
		{"del array element", `del(.list[1])`},
		{"map_values update", `.b |= map_values(.)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mk := func() map[string]any {
				return map[string]any{
					"a": "AAA",
					"b": map[string]any{
						"c": map[string]any{"d": 1.0, "e": "keep"},
						"f": "sibling",
					},
					"list": []any{"x", "y", "z"},
				}
			}
			shared := mk()
			snap := deepCopyAny(shared)

			// Result over the SHARED input.
			gotShared := runGojq(t, tc.query, shared)

			// Shared input must be UNCHANGED.
			if !reflect.DeepEqual(shared, snap) {
				t.Fatalf("%q mutated shared input: got %#v want %#v", tc.query, shared, snap)
			}

			// Result over a PRIVATE deep copy — must DeepEqual the shared run.
			priv := deepCopyAny(mk())
			gotPriv := runGojq(t, tc.query, priv)
			if !reflect.DeepEqual(gotShared, gotPriv) {
				t.Errorf("%q result diverges shared-vs-private:\n shared=%#v\n  priv=%#v",
					tc.query, gotShared, gotPriv)
			}
		})
	}
}

// TestShip2a_ByteIdentity_OldVsNew is Gate 2. For a valid-filter corpus
// (read-only AND destructive) including a json.Number leaf (the
// normalize-removal delta), assert that running gojq over a DEEP COPY of
// the input (the 0.30.130-0.30.208 isolation model) and over the SHALLOW
// shared input (Ship 2a) produce reflect.DeepEqual results.
func TestShip2a_ByteIdentity_OldVsNew(t *testing.T) {
	// Build the envelope value as listEnvelopeValue does (shallow), then
	// for the "old" arm deep-copy it via CopyJSONValue (the retired
	// isolation copier) — both arms feed identically-shaped pigs.
	items := ship2aItems(25)

	// Inject a json.Number leaf into one item to cover the v0.12.17->
	// v0.12.19 normalize-removal delta: pre-v0.12.18 gojq normalized
	// json.Number to float64/int; v0.12.19 leaves it as json.Number. The
	// production decoder (parseListEnvelope) never emits json.Number
	// (plain json.Unmarshal -> float64), so this is a synthetic worst
	// case proving the delta is understood and bounded.
	items[0].Object["spec"].(map[string]any)["replicas"] = json.Number("3")

	corpus := []string{
		`.result.items | length`,
		`[.result.items[] | .metadata.name]`,
		`{items: [.result.items[] | {n: .metadata.name, ns: .metadata.namespace}]}`,
		`[.result.items[] | del(.metadata.annotations)]`,
		`[.result.items[] | del(.spec.values.region)]`,
		`[.result.items[] | delpaths([["spec","values"],["status","ready"]])]`,
		`[.result.items[] | (.metadata.labels.app) |= "X"]`,
		`[.result.items[] | .metadata |= map_values(.)]`,
		`.result.items[0].spec.replicas`, // the json.Number leaf
	}

	for _, q := range corpus {
		t.Run(q, func(t *testing.T) {
			// OLD arm: deep-copy isolation (the pre-Ship-2a model).
			oldEnv := listEnvelopeValue("composition.krateo.io/v1", "CompositionList", items)
			oldPig := map[string]any{"result": CopyJSONMap(oldEnv)}
			oldRes := runGojq(t, q, oldPig)

			// NEW arm: shallow shared envelope (Ship 2a).
			newEnv := listEnvelopeValue("composition.krateo.io/v1", "CompositionList", items)
			newPig := map[string]any{"result": newEnv}
			newRes := runGojq(t, q, newPig)

			// Compare at the value level AND at the marshalled-bytes level
			// (the widget-prop boundary).
			if !reflect.DeepEqual(oldRes, newRes) {
				t.Errorf("value diverges old-vs-new for %q:\n old=%#v\n new=%#v", q, oldRes, newRes)
			}
			ob, _ := json.Marshal(oldRes)
			nb, _ := json.Marshal(newRes)
			if string(ob) != string(nb) {
				t.Errorf("marshalled bytes diverge for %q:\n old=%s\n new=%s", q, ob, nb)
			}
		})
	}
}

// oldRecursiveManagedFieldsStrip models the deleted resolve.go
// removeManagedFields per-serve walk: it deletes a "managedFields" key
// from every map in the tree. Used by the GET byte-identity gate to prove
// the Ship 2a load-time strip (gateGetEnvelope) yields the SAME served
// wire shape the old per-serve walk produced.
func oldRecursiveManagedFieldsStrip(v any) {
	switch t := v.(type) {
	case map[string]any:
		delete(t, "managedFields")
		for _, sub := range t {
			oldRecursiveManagedFieldsStrip(sub)
		}
	case []any:
		for _, sub := range t {
			oldRecursiveManagedFieldsStrip(sub)
		}
	}
}

// hasManagedFieldsKey reports whether metadata.managedFields survives.
func hasManagedFieldsKey(obj map[string]any) bool {
	md, _ := obj["metadata"].(map[string]any)
	if md == nil {
		return false
	}
	_, ok := md["managedFields"]
	return ok
}

// TestShip2a_GetByName_ManagedFields_ByteIdentity is the Gate 2
// GET-by-name case: a managedFields-bearing single-object GET response
// served via gateGetEnvelope (Ship 2a strips at load) must be byte-
// identical to the old model (serve the object, then per-serve
// removeManagedFields walk). Proves dropping the per-serve walk does not
// change the served wire shape for a managedFields-bearing GET.
func TestShip2a_GetByName_ManagedFields_ByteIdentity(t *testing.T) {
	raw := []byte(`{
		"apiVersion": "v1",
		"kind": "ConfigMap",
		"metadata": {
			"name": "cm-1",
			"namespace": "team-a",
			"managedFields": [
				{"manager":"kubectl","operation":"Apply","apiVersion":"v1","time":"2026-05-29T00:00:00Z"}
			]
		},
		"data": {"key": "value"}
	}`)

	// OLD model: decode, then the per-serve recursive strip.
	var oldObj map[string]any
	if err := json.Unmarshal(raw, &oldObj); err != nil {
		t.Fatalf("unmarshal old: %v", err)
	}
	oldRecursiveManagedFieldsStrip(oldObj)
	oldBytes, _ := json.Marshal(oldObj)

	// NEW model: gateGetEnvelope strips managedFields at load via the
	// production stripManagedFields (the exact call gateGetEnvelope makes).
	var newObj map[string]any
	if err := json.Unmarshal(raw, &newObj); err != nil {
		t.Fatalf("unmarshal new: %v", err)
	}
	stripManagedFields(newObj)
	newBytes, _ := json.Marshal(newObj)

	if string(oldBytes) != string(newBytes) {
		t.Fatalf("GET-by-name served wire shape diverges old-vs-new:\n old=%s\n new=%s",
			oldBytes, newBytes)
	}
	if hasManagedFieldsKey(newObj) {
		t.Fatalf("Ship 2a GET strip left managedFields in served object: %s", newBytes)
	}
}
