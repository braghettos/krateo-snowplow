// debug_refreshes_structural_test.go — L9 [SEC-adj]: the /debug/refreshes body
// is AGGREGATE-ONLY by construction.
//
// debugRefreshesBody (debug_refreshes.go) deliberately exposes only process-wide
// scalar counters + the live subscriber count. It must NEVER grow a per-key or
// per-identity ENUMERATION field (e.g. a []string ArmedKeys or a
// map[string]uint64 PerUserDelivered) — which keys/identities are armed is a
// cross-user signal (it reveals which widgets/resources a connected user is
// watching, #61 C-3). A structural guard is the right shape here: any future
// slice/map field on the body is a leak-surface regression, and it should fail
// at type level in CI, not in a security review months later.
//
// This test reflects over debugRefreshesBody and asserts EVERY exported field is
// a scalar (no slice, no map, no nested struct/array). The RED arm proves the
// check is discriminating: the SAME reflective assertion applied to a shadow
// struct carrying a []string ArmedKeys field FAILS.

package handlers

import (
	"reflect"
	"testing"
)

// scalarKinds is the allow-list of reflect.Kinds a leak-safe aggregate body may
// use. Anything outside it (Slice, Map, Struct, Array, Ptr, Interface) is a
// potential per-key/per-identity enumeration surface and is rejected.
var scalarKinds = map[reflect.Kind]bool{
	reflect.Bool:    true,
	reflect.Int:     true,
	reflect.Int8:    true,
	reflect.Int16:   true,
	reflect.Int32:   true,
	reflect.Int64:   true,
	reflect.Uint:    true,
	reflect.Uint8:   true,
	reflect.Uint16:  true,
	reflect.Uint32:  true,
	reflect.Uint64:  true,
	reflect.Float32: true,
	reflect.Float64: true,
	reflect.String:  true,
}

// nonScalarFields returns the names of any exported fields on t whose kind is
// NOT in scalarKinds — i.e. the enumeration/aggregate-shape violations.
func nonScalarFields(t reflect.Type) []string {
	var bad []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // unexported — not part of the serialized surface
		}
		if !scalarKinds[f.Type.Kind()] {
			bad = append(bad, f.Name+" ("+f.Type.Kind().String()+")")
		}
	}
	return bad
}

// TestDebugRefreshesBody_AggregateOnly — every field of debugRefreshesBody is a
// scalar; no slice/map/struct field (which would be a per-key/per-identity
// enumeration surface, #61 C-3).
func TestDebugRefreshesBody_AggregateOnly(t *testing.T) {
	rt := reflect.TypeOf(debugRefreshesBody{})
	if rt.Kind() != reflect.Struct {
		t.Fatalf("debugRefreshesBody must be a struct; got %s", rt.Kind())
	}
	if bad := nonScalarFields(rt); len(bad) > 0 {
		t.Fatalf("L9: /debug/refreshes body must be AGGREGATE-ONLY (all scalar fields); "+
			"found non-scalar field(s) %v — a slice/map here leaks which keys/identities are "+
			"armed (cross-user signal, #61 C-3)", bad)
	}
}

// TestDebugRefreshesBody_RED_SliceFieldIsCaught is the RED proof: a shadow body
// type identical to debugRefreshesBody EXCEPT for an added `[]string ArmedKeys`
// per-key enumeration field MUST be flagged by the same reflective check. If it
// weren't, the L9 guard would tolerate exactly the leak it exists to prevent.
func TestDebugRefreshesBody_RED_SliceFieldIsCaught(t *testing.T) {
	type wrongBody struct {
		SSEEnabled  bool     `json:"sseEnabled"`
		Subscribers int      `json:"subscribers"`
		Published   uint64   `json:"published"`
		Delivered   uint64   `json:"delivered"`
		Dropped     uint64   `json:"dropped"`
		Coalesced   uint64   `json:"coalesced"`
		ArmedKeys   []string `json:"armedKeys"` // per-key enumeration — the leak
	}
	bad := nonScalarFields(reflect.TypeOf(wrongBody{}))
	if len(bad) != 1 || bad[0] != "ArmedKeys (slice)" {
		t.Fatalf("RED: the structural check must flag exactly the []string ArmedKeys field; got %v", bad)
	}

	// Also prove a map[string]uint64 per-identity field is caught.
	type wrongBodyMap struct {
		Published        uint64            `json:"published"`
		PerUserDelivered map[string]uint64 `json:"perUserDelivered"` // per-identity — the leak
	}
	badMap := nonScalarFields(reflect.TypeOf(wrongBodyMap{}))
	if len(badMap) != 1 || badMap[0] != "PerUserDelivered (map)" {
		t.Fatalf("RED: the structural check must flag the map per-identity field; got %v", badMap)
	}
}
