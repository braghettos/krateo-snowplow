// hash_extras_test.go — coverage-audit M13 + L2.
//
// HashExtras (resolved.go) is the SINGLE canonicalisation site shared by the
// L1 key (ComputeKey folds canonicaliseExtras) and the F4 SeedResolveMemo key.
// Its contract:
//
//   M13:
//     - HashExtras(nil) == HashExtras(map[string]any{}) == "e0"  (empty sentinel,
//       never the empty string, so an empty-extras key segment is unambiguous).
//     - order-INDEPENDENT: two maps with the same entries hash identically, and
//       a non-empty map never hashes to the "e0" sentinel.
//     - PARITY: order-equal Extras drive an EQUAL ComputeKey — HashExtras and the
//       key both route through canonicaliseExtras, so they cannot drift.
//
//   L2 (marshal-failure fallback):
//     - a value json.Marshal cannot encode (here: a non-JSON func value) still
//       makes the key VARY with content via the fmt.Sprintf fallback (never
//       collapses distinct content to one hash).
//     - nested-map recursion: canonicaliseExtras recurses into nested maps, so
//       a nested-map difference changes the hash (guards the recursion branch).
//
// RED arms:
//   M13: an order-SENSITIVE HashExtras (no key sort — the shape a fmt.Sprintf(map)
//     impl would exhibit) produces different hashes for the same content under
//     different key orderings → the shadow proves order-instability, so the GREEN
//     order-independence arm discriminates.
//   L2: a fallback that returns a CONSTANT on marshal error collapses distinct
//     un-marshalable content to one hash → the shadow proves the collapse.
package cache

import (
	"fmt"
	"testing"
)

// TestHashExtras_EmptySentinel is M13 part 1: nil and empty map both map to the
// fixed "e0" sentinel, and a non-empty map never does.
func TestHashExtras_EmptySentinel(t *testing.T) {
	if got := HashExtras(nil); got != "e0" {
		t.Fatalf("HashExtras(nil) = %q, want the empty sentinel %q", got, "e0")
	}
	if got := HashExtras(map[string]any{}); got != "e0" {
		t.Fatalf("HashExtras(empty) = %q, want the empty sentinel %q", got, "e0")
	}
	if HashExtras(nil) != HashExtras(map[string]any{}) {
		t.Fatalf("HashExtras(nil) must equal HashExtras(empty-map)")
	}
	if got := HashExtras(map[string]any{"k": "v"}); got == "e0" {
		t.Fatalf("HashExtras of a non-empty map must NOT be the empty sentinel %q", got)
	}
}

// TestHashExtras_OrderIndependent is M13 part 2: insertion order is irrelevant.
// We build the SAME logical map twice with different literal orderings and
// assert equal hashes.
//
// RED PROOF: hashExtrasOrderSensitive (a no-sort shadow) produces different
// hashes for the same content under different key orderings — proven in
// TestHashExtras_OrderSensitive_RedArm below.
func TestHashExtras_OrderIndependent(t *testing.T) {
	a := map[string]any{"alpha": "1", "beta": "2", "gamma": "3"}
	b := map[string]any{"gamma": "3", "alpha": "1", "beta": "2"}
	if HashExtras(a) != HashExtras(b) {
		t.Fatalf("HashExtras must be order-independent: %q vs %q", HashExtras(a), HashExtras(b))
	}
	// Run many times: even if the CORRECT impl were accidentally order-sensitive
	// it would show up across randomised iterations.
	for i := 0; i < 50; i++ {
		if HashExtras(a) != HashExtras(b) {
			t.Fatalf("HashExtras order-independence flaked on iteration %d", i)
		}
	}
}

// TestHashExtras_KeyParityWithComputeKey is M13 part 3: HashExtras and
// ComputeKey share canonicaliseExtras, so order-equal Extras drive an EQUAL
// ComputeKey. This is the "cannot drift" guard between the memo key and L1 key.
func TestHashExtras_KeyParityWithComputeKey(t *testing.T) {
	e1 := map[string]any{"x": "1", "y": float64(2), "z": true}
	e2 := map[string]any{"z": true, "y": float64(2), "x": "1"} // same entries, different order

	// HashExtras parity.
	if HashExtras(e1) != HashExtras(e2) {
		t.Fatalf("HashExtras parity: order-equal extras must hash equal")
	}

	// ComputeKey parity: identical inputs except Extras ordering → equal key.
	base := ResolvedKeyInputs{
		CacheEntryClass: CacheEntryClassWidgets,
		Group:           "g", Version: "v", Resource: "r",
		Namespace: "ns", Name: "n", BindingUID: "C:uid",
		PerPage: 5, Page: 1,
	}
	k1 := base
	k1.Extras = e1
	k2 := base
	k2.Extras = e2
	if ComputeKey(k1) != ComputeKey(k2) {
		t.Fatalf("ComputeKey parity: order-equal extras must produce equal keys")
	}
	// And a genuine extras difference MUST move both.
	e3 := map[string]any{"x": "1", "y": float64(2), "z": false} // z flipped
	if HashExtras(e1) == HashExtras(e3) {
		t.Fatalf("HashExtras must vary on an extras VALUE change")
	}
	k3 := base
	k3.Extras = e3
	if ComputeKey(k1) == ComputeKey(k3) {
		t.Fatalf("ComputeKey must vary on an extras VALUE change")
	}
}

// TestHashExtras_NestedMapRecursion is L2 part 1: canonicaliseExtras recurses
// into nested maps, so a difference deep in a nested map changes the hash AND is
// order-independent at the nested level.
func TestHashExtras_NestedMapRecursion(t *testing.T) {
	a := map[string]any{
		"outer": map[string]any{"p": "1", "q": "2"},
		"top":   "T",
	}
	// Same nested content, different nested insertion order → equal.
	aReordered := map[string]any{
		"top":   "T",
		"outer": map[string]any{"q": "2", "p": "1"},
	}
	if HashExtras(a) != HashExtras(aReordered) {
		t.Fatalf("nested-map canonicalisation must be order-independent")
	}
	// A change DEEP in the nested map must move the hash — proves recursion
	// actually visits the nested value rather than hashing an opaque address.
	b := map[string]any{
		"outer": map[string]any{"p": "1", "q": "CHANGED"},
		"top":   "T",
	}
	if HashExtras(a) == HashExtras(b) {
		t.Fatalf("a nested-map value change must change the hash (recursion not applied)")
	}
}

// TestHashExtras_MarshalFailureVariesWithContent is L2 part 2: an un-marshalable
// value (a func) forces canonicaliseExtras to error; the fmt.Sprintf fallback
// must still make DISTINCT un-marshalable content hash DIFFERENTLY.
//
// RED PROOF: hashExtrasConstantOnError (returns a fixed sentinel on marshal
// error) collapses the two distinct payloads — proven in
// TestHashExtras_ConstantOnError_RedArm below.
func TestHashExtras_MarshalFailureVariesWithContent(t *testing.T) {
	// Two maps that both FAIL json.Marshal (func values) but carry different
	// discriminating scalar content alongside the un-marshalable value.
	m1 := map[string]any{"tag": "one", "bad": func() {}}
	m2 := map[string]any{"tag": "two", "bad": func() {}}

	h1 := HashExtras(m1)
	h2 := HashExtras(m2)
	if h1 == "e0" || h2 == "e0" {
		t.Fatalf("marshal-failure fallback must not collapse to the empty sentinel")
	}
	if h1 == h2 {
		t.Fatalf("marshal-failure fallback must vary with content: %q == %q "+
			"(a constant-on-error impl would collide distinct un-marshalable extras)", h1, h2)
	}
}

// --- RED-arm proofs ---------------------------------------------------------

// hashExtrasOrderSensitive is the M13 WRONG impl: it concatenates entries in the
// GIVEN key order (no sort), so the same logical content hashed under two
// different key orderings produces two different hashes. This is the defect a
// fmt.Sprintf(map)-style impl would exhibit (map iteration order is not sorted);
// we model it DETERMINISTICALLY via an explicit key-order slice so the RED proof
// never flakes on Go's randomised — but not guaranteed-divergent — map order.
func hashExtrasOrderSensitive(keysInOrder []string, m map[string]any) string {
	if len(m) == 0 {
		return "e0"
	}
	var b []byte
	for _, k := range keysInOrder {
		b = append(b, []byte(fmt.Sprintf("%s=%v;", k, m[k]))...)
	}
	return string(b)
}

// TestHashExtras_OrderSensitive_RedArm proves the M13 order-independence arm
// discriminates: the order-sensitive shadow produces DIFFERENT hashes for the
// same logical map under two different key orderings — the exact behavior the
// GREEN order-independence arm forbids. Deterministic (no reliance on runtime
// map-order divergence).
func TestHashExtras_OrderSensitive_RedArm(t *testing.T) {
	m := map[string]any{"alpha": "1", "beta": "2", "gamma": "3"}
	forward := hashExtrasOrderSensitive([]string{"alpha", "beta", "gamma"}, m)
	reverse := hashExtrasOrderSensitive([]string{"gamma", "beta", "alpha"}, m)
	if forward == reverse {
		t.Fatalf("RED-arm sanity: the order-sensitive wrong impl SHOULD produce different " +
			"hashes for the same content under different key orderings; the M13 " +
			"order-independence arm is not discriminating otherwise")
	}
	// The correct HashExtras produces the SAME hash regardless of insertion
	// order (asserted in TestHashExtras_OrderIndependent) — so the GREEN arm
	// genuinely discriminates against this shadow.
}

// hashExtrasConstantOnError is the L2 WRONG impl: on marshal error it returns a
// fixed constant, collapsing all un-marshalable content to one hash.
func hashExtrasConstantOnError(m map[string]any) string {
	if len(m) == 0 {
		return "e0"
	}
	if _, err := canonicaliseExtras(m); err != nil {
		return "MARSHAL_ERROR" // the defect: content-blind constant
	}
	return HashExtras(m)
}

// TestHashExtras_ConstantOnError_RedArm proves the L2 GREEN arm discriminates:
// the constant-on-error shadow COLLIDES the two distinct un-marshalable payloads
// the GREEN arm asserts DISTINCT.
func TestHashExtras_ConstantOnError_RedArm(t *testing.T) {
	m1 := map[string]any{"tag": "one", "bad": func() {}}
	m2 := map[string]any{"tag": "two", "bad": func() {}}
	if hashExtrasConstantOnError(m1) != hashExtrasConstantOnError(m2) {
		t.Fatalf("RED-arm sanity: the constant-on-error wrong impl SHOULD collide distinct " +
			"un-marshalable extras; the L2 test is not discriminating otherwise")
	}
}
