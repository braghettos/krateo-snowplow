//go:build unit
// +build unit

// handler_flatten_provenance_test.go — #74 Class 3 R1' falsifier pair.
//
// The accumulator in jsonHandlerCore (handler.go) splices a bare []any element
// into the per-key accumulator ONLY when it is FILTER-PRODUCED (opts.filter !=
// nil). A RAW-RESPONSE []any (opts.filter == nil) is appended as a SINGLE
// element, never spliced. This is the Class 3 fix: the iterator step
// allCompositionResources (NO filter) had one element resolve to ["v1", …] and
// the old wrapAsSlice flatten spliced the bare scalar "v1" into the parent
// list, which the downstream portal filter indexed as .metadata → "expected an
// object but got: string (\"v1\")".
//
// These drive the REAL jsonHandlerCore accumulator (not a hand-rolled copy) by
// seeding out[key] with a first element, then accumulating a second element
// whose value is a bare []any — exactly the multi-element iterator path.
package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/krateoplatformops/plumbing/ptr"
)

// accumulate runs jsonHandlerCore for a sequence of already-decoded element
// values under the given filter pointer, returning the final out[key]. This is
// the production accumulator path (handler.go) — the elements arrive one per
// iterator yield, accumulated into out[key].
func accumulate(t *testing.T, filter *string, key string, elements ...any) any {
	t.Helper()
	out := map[string]any{}
	opts := jsonHandlerOptions{
		key:    key,
		out:    out,
		dict:   map[string]any{},
		filter: filter,
	}
	for i, el := range elements {
		if err := jsonHandlerCore(context.Background(), opts, el); err != nil {
			t.Fatalf("jsonHandlerCore element %d: %v", i, err)
		}
	}
	return out[key]
}

// containsBareString reports whether v is a []any with a top-level bare-string
// member — the poisoned shape Class 3 produced.
func containsBareString(v any, s string) bool {
	arr, ok := v.([]any)
	if !ok {
		return false
	}
	for _, m := range arr {
		if str, isStr := m.(string); isStr && str == s {
			return true
		}
	}
	return false
}

// TestClass3R1_RawResponseArrayNotSpliced is the RED arm (cured by R1'). A
// no-filter iterator step (opts.filter == nil) whose second element resolves to
// a bare []any{"v1","apps/v1"} must NOT splice "v1" / "apps/v1" as top-level
// bare-string members of the accumulator — the array is appended as a SINGLE
// element. RED on the old wrapAsSlice flatten (the scalar WAS spliced); GREEN
// after R1'.
func TestClass3R1_RawResponseArrayNotSpliced(t *testing.T) {
	// First element: a real resource object (what allCompositionResources
	// resolves for a well-formed path). Second element: the poisoned bare
	// array (a resolve:true element resolving to a groupVersion-string list).
	obj := map[string]any{"metadata": map[string]any{"name": "cm-a"}}
	poison := []any{"v1", "apps/v1"}

	got := accumulate(t, nil /* opts.filter == nil → RAW response */, "allCompositionResources", obj, poison)

	if containsBareString(got, "v1") || containsBareString(got, "apps/v1") {
		j, _ := json.Marshal(got)
		t.Fatalf("Class 3 RED: a RAW-response []any was SPLICED — bare scalar leaked to the parent "+
			"(this poisons the downstream filter's .metadata index). got=%s", j)
	}
	// Positive: the array must still be PRESENT as a single element (not
	// dropped) — the no-filter element is preserved, just not flattened.
	arr, ok := got.([]any)
	if !ok || len(arr) != 2 {
		j, _ := json.Marshal(got)
		t.Fatalf("Class 3: expected 2 accumulated elements (obj + the array-as-single-element), got=%s", j)
	}
	if _, isArr := arr[1].([]any); !isArr {
		j, _ := json.Marshal(got)
		t.Fatalf("Class 3: the raw-response array must be a SINGLE nested element, got element[1]=%s", j)
	}
}

// TestClass3R1_FilterProducedArrayStillFlattens is the GREEN arm (regression
// guard). composition-schema's getCompositionDefinitionNamespace has a per-stage
// filter that CONSTRUCTS an array (map(.metadata.namespace)); the intended
// behaviour is element-wise concat into one flat namespace list. With
// opts.filter != nil and two elements ["ns-a"] / ["ns-b"], the accumulator must
// stay FLAT ["ns-a","ns-b"] — NOT nested [["ns-a"],["ns-b"]]. GREEN before AND
// after R1' (proves R1' did not break the load-bearing flatten).
func TestClass3R1_FilterProducedArrayStillFlattens(t *testing.T) {
	// The filter yields the per-element array as a SINGLE value (so tmp is a
	// []any at the accumulator, exercising the filter-produced splice branch).
	// This mirrors getCompositionDefinitionNamespace's `[ … | .metadata.namespace ]`
	// which constructs an array per iterator element.
	got := accumulate(t, ptr.To(".getCompositionDefinitionNamespace") /* filter != nil → array tmp */,
		"getCompositionDefinitionNamespace", []any{"ns-a"}, []any{"ns-b"})

	arr, ok := got.([]any)
	if !ok {
		j, _ := json.Marshal(got)
		t.Fatalf("GREEN: filter-produced arrays must accumulate to a []any, got=%s", j)
	}
	if len(arr) != 2 {
		j, _ := json.Marshal(got)
		t.Fatalf("GREEN: expected FLAT [\"ns-a\",\"ns-b\"] (filter-produced flatten preserved), got %d members: %s", len(arr), j)
	}
	for i, want := range []string{"ns-a", "ns-b"} {
		s, isStr := arr[i].(string)
		if !isStr || s != want {
			j, _ := json.Marshal(got)
			t.Fatalf("GREEN: filter-produced flatten broken — member[%d]=%v, want %q (flat). got=%s", i, arr[i], want, j)
		}
	}
}
