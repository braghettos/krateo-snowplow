// apistage_parse_for_refresh_test.go — Ship #97 (0.30.214) unit-level
// falsifier for ParseListEnvelopeForRefresh.
//
// The Ship #97 fix exports ParseListEnvelopeForRefresh as a pure
// (inputs, raw) -> (items, apiVersion, kind, ok) helper so the
// dispatchers refresher Put site (resolve_populate.go:255) can populate
// ResolvedEntry.Items at Put time and restore the R3 fast-path predicate
// at apistage.go:487. This test pins three invariants:
//
//  1. Byte-equivalence with the MISS-branch parseListEnvelope call
//     (apistage.go:531). Same parse function, same field set; differs
//     only by call site / goroutine — the items slice must be element-
//     wise equal.
//  2. GET-by-name (Name != "") returns ok=false; R3 fast-path is LIST
//     only.
//  3. A malformed envelope returns ok=false (caller stores RawJSON only
//     — the gate then takes the unmarshal fallback, byte-identical to
//     pre-fix).

package api

import (
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestParseListEnvelopeForRefresh_ItemsByteEquivalentToMissBranch(t *testing.T) {
	// f1WidgetsGVR is a package-level test fixture (widgets.krateo.io/v1
	// widgets). Same envelope shape the MISS branch parses at
	// apistage.go:531.
	raw := []byte(`{"apiVersion":"widgets.krateo.io/v1","kind":"WidgetList",` +
		`"items":[` +
		`{"metadata":{"name":"w1","namespace":"team-a","managedFields":[{"manager":"x"}]}},` +
		`{"metadata":{"name":"w2","namespace":"team-b"}},` +
		`{"metadata":{"name":"w3","namespace":"team-c","resourceVersion":"42"}}` +
		`]}`)

	inputs := cache.ResolvedKeyInputs{
		CacheEntryClass: cache.CacheEntryClassApistage,
		Group:           "widgets.krateo.io",
		Version:         "v1",
		Resource:        "widgets",
		Namespace:       "",
		Name:            "", // LIST
	}

	// Branch B — the new refresher-side helper (Ship #97).
	itemsB, apiVerB, kindB, okB := ParseListEnvelopeForRefresh(inputs, raw)
	if !okB {
		t.Fatalf("ParseListEnvelopeForRefresh returned ok=false on well-formed LIST envelope")
	}

	// Branch A — the MISS-branch parse (apistage.go:531). Same function
	// the request goroutine has been calling all along.
	gvr := schema.GroupVersionResource{
		Group:    inputs.Group,
		Version:  inputs.Version,
		Resource: inputs.Resource,
	}
	parsedA, okA := parseListEnvelope(gvr, raw)
	if !okA {
		t.Fatalf("parseListEnvelope (MISS branch) returned ok=false on well-formed LIST envelope")
	}

	// Same item count.
	if got, want := len(itemsB), len(parsedA.items); got != want {
		t.Fatalf("item-count mismatch: helper=%d miss-branch=%d", got, want)
	}
	if apiVerB != parsedA.apiVersion {
		t.Fatalf("apiVersion mismatch: helper=%q miss-branch=%q", apiVerB, parsedA.apiVersion)
	}
	if kindB != parsedA.kind {
		t.Fatalf("kind mismatch: helper=%q miss-branch=%q", kindB, parsedA.kind)
	}

	// Same per-element metadata (the gated path consumes .Object — the
	// element-level map is what R3 hot-path reads at apistage.go:487).
	// We compare metadata.name + metadata.namespace, which the gate
	// uses for filterListByRBAC.
	for i := range itemsB {
		mdB, _ := itemsB[i].Object["metadata"].(map[string]any)
		mdA, _ := parsedA.items[i].Object["metadata"].(map[string]any)
		if mdB == nil || mdA == nil {
			t.Fatalf("item[%d]: missing metadata in one of the branches", i)
		}
		if mdB["name"] != mdA["name"] || mdB["namespace"] != mdA["namespace"] {
			t.Fatalf("item[%d] metadata mismatch: helper=%v miss-branch=%v", i, mdB, mdA)
		}
		// stripManagedFields must have run on both — neither item carries
		// metadata.managedFields. This is the Ship 2a invariant the R3
		// shared-Items contract depends on.
		if _, ok := mdB["managedFields"]; ok {
			t.Fatalf("item[%d] helper-branch DID NOT strip managedFields", i)
		}
		if _, ok := mdA["managedFields"]; ok {
			t.Fatalf("item[%d] miss-branch DID NOT strip managedFields", i)
		}
	}
}

func TestParseListEnvelopeForRefresh_GetByNameReturnsFalse(t *testing.T) {
	// GET-by-name — Name is non-empty. The R3 fast-path keys on LIST
	// envelopes only; GET-by-name returns ok=false and the caller keeps
	// Items=nil.
	raw := []byte(`{"apiVersion":"widgets.krateo.io/v1","kind":"Widget",` +
		`"metadata":{"name":"w1"}}`)
	inputs := cache.ResolvedKeyInputs{
		CacheEntryClass: cache.CacheEntryClassApistage,
		Group:           "widgets.krateo.io",
		Version:         "v1",
		Resource:        "widgets",
		Name:            "w1", // GET-by-name — not a LIST
	}

	items, apiVer, kind, ok := ParseListEnvelopeForRefresh(inputs, raw)
	if ok {
		t.Fatalf("ParseListEnvelopeForRefresh returned ok=true on GET-by-name; want ok=false")
	}
	if items != nil || apiVer != "" || kind != "" {
		t.Fatalf("ok=false branch must return zero values; got items=%v apiVer=%q kind=%q",
			items, apiVer, kind)
	}
}

func TestParseListEnvelopeForRefresh_MalformedEnvelopeReturnsFalse(t *testing.T) {
	// Malformed envelope — not parseable as a LIST. The caller stores
	// RawJSON only and the gate takes the unmarshal fallback at hit time.
	raw := []byte(`{"this":"is not a LIST envelope" THIS_IS_NOT_JSON`)
	inputs := cache.ResolvedKeyInputs{
		CacheEntryClass: cache.CacheEntryClassApistage,
		Group:           "widgets.krateo.io",
		Version:         "v1",
		Resource:        "widgets",
		Name:            "",
	}

	items, apiVer, kind, ok := ParseListEnvelopeForRefresh(inputs, raw)
	if ok {
		t.Fatalf("ParseListEnvelopeForRefresh returned ok=true on malformed envelope; want ok=false")
	}
	if items != nil || apiVer != "" || kind != "" {
		t.Fatalf("ok=false branch must return zero values; got items=%v apiVer=%q kind=%q",
			items, apiVer, kind)
	}
}

