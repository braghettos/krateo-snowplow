// nested_resolve_ancestor_codec_test.go — R codec round-trip falsifier.
//
// The emit site (api resolver) serializes the ctx ancestor set with
// AncestorsHeaderValue; the ingest site (dispatcher handler) parses it back with
// ParseAncestorsHeader. These are the ONE shared codec (anti-drift, #66). This
// test pins:
//   - round-trip: serialize → parse restores the SAME set (order-independent);
//   - set semantics: duplicates collapse, wire order is irrelevant;
//   - comma-safety: the delimiter cannot over-split a node string (RFC-1123
//     labels + "/" never contain a comma);
//   - depth-8 bound: an 8-node set serializes well under any HTTP header limit;
//   - empty: no set → "" (emit omits the header).
package cache

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// seedSet builds a ctx carrying the given nodes as an ancestor set (copy-on-
// descend, exactly as the resolver descends).
func seedSet(nodes ...string) context.Context {
	ctx := context.Background()
	for _, n := range nodes {
		ctx = WithNestedResolveAncestor(ctx, n)
	}
	return ctx
}

// parseIntoSet applies ParseAncestorsHeader + WithNestedResolveAncestor to
// reconstruct the set membership the ingest side would build.
func parseIntoSet(header string) map[string]struct{} {
	ctx := context.Background()
	for _, n := range ParseAncestorsHeader(header) {
		ctx = WithNestedResolveAncestor(ctx, n)
	}
	set, _ := ctx.Value(ctxKeyNestedResolveAncestor).(map[string]struct{})
	if set == nil {
		return map[string]struct{}{}
	}
	return set
}

func TestAncestorsCodec_RoundTrip(t *testing.T) {
	a := "restactions/demo-system/fsa-y7-composition-resources"
	b := "widgets/other-system/fsa-y9-composition-status-panel"
	ctx := seedSet(a, b)

	header := AncestorsHeaderValue(ctx)
	got := parseIntoSet(header)

	if len(got) != 2 {
		t.Fatalf("round-trip set size = %d, want 2 (header=%q)", len(got), header)
	}
	for _, want := range []string{a, b} {
		if _, ok := got[want]; !ok {
			t.Fatalf("round-trip lost node %q (header=%q, got=%v)", want, header, got)
		}
	}
}

// TestAncestorsCodec_SetSemantics — wire ORDER is irrelevant and duplicate nodes
// collapse (the parse re-inserts into a fresh map). A hand-built reversed +
// duplicated header must reconstruct the SAME set.
func TestAncestorsCodec_SetSemantics(t *testing.T) {
	a := "restactions/ns-a/name-a"
	b := "restactions/ns-b/name-b"
	// Reversed order + a duplicate of a — the map must collapse it.
	header := strings.Join([]string{b, a, a}, ",")
	got := parseIntoSet(header)
	if len(got) != 2 {
		t.Fatalf("set semantics: size = %d, want 2 (dups must collapse, order irrelevant)", len(got))
	}
	if _, ok := got[a]; !ok {
		t.Fatalf("set semantics: missing %q", a)
	}
	if _, ok := got[b]; !ok {
		t.Fatalf("set semantics: missing %q", b)
	}
}

// TestAncestorsCodec_CommaSafety — a comma is outside the RFC-1123 label set and
// outside the node "/" separator, so it can never appear inside a real node. This
// arm feeds a node that (impossibly) contains a comma and asserts the codec does
// NOT silently over-split it into two fake nodes if it ever appeared — the design
// guard for the delimiter choice. Real nodes are comma-free so this is a
// belt-and-suspenders shape assertion: with a comma-free corpus the round-trip is
// exact; a single comma-bearing token WOULD split (proving the delimiter is the
// comma), which is why the node components are constrained to RFC-1123.
func TestAncestorsCodec_CommaSafety(t *testing.T) {
	// Real (comma-free) nodes round-trip exactly — the invariant we rely on.
	clean := "restactions/kube-system/a-b.c-1"
	if got := parseIntoSet(AncestorsHeaderValue(seedSet(clean))); len(got) != 1 {
		t.Fatalf("comma-safety: a clean RFC-1123 node must round-trip as ONE node, got %d", len(got))
	}
	if _, ok := parseIntoSet(AncestorsHeaderValue(seedSet(clean)))[clean]; !ok {
		t.Fatalf("comma-safety: clean node %q did not survive round-trip", clean)
	}
	// A comma-bearing token WOULD split — documents that the delimiter IS the
	// comma and therefore nodes MUST be comma-free (they are: RFC-1123 + "/").
	if got := ParseAncestorsHeader("has,comma"); len(got) != 2 {
		t.Fatalf("comma-safety: the delimiter is the comma (a comma-bearing token splits); "+
			"got %d tokens — if this changed, the delimiter drifted", len(got))
	}
}

// TestAncestorsCodec_Depth8Bound — an 8-node set (the depth-8 hard ceiling)
// serializes to a bounded header well under any HTTP header limit, and round-trips
// to all 8. Proves the header cannot grow unbounded even at the deepest legal path.
func TestAncestorsCodec_Depth8Bound(t *testing.T) {
	ctx := context.Background()
	for i := 0; i < NestedCallMaxDepth(); i++ {
		ctx = WithNestedResolveAncestor(ctx,
			fmt.Sprintf("restactions/composition-namespace-%d/fsa-y%d-composition-resources", i, i))
	}
	header := AncestorsHeaderValue(ctx)
	if got := parseIntoSet(header); len(got) != NestedCallMaxDepth() {
		t.Fatalf("depth-8 bound: round-trip size = %d, want %d", len(got), NestedCallMaxDepth())
	}
	// Sanity: the 8-node header is small (each node ~80B → ~640B), far under a
	// typical 8KiB header cap.
	if len(header) > 4096 {
		t.Fatalf("depth-8 bound: header unexpectedly large (%d bytes) — check node size assumption", len(header))
	}
}

func TestAncestorsCodec_Empty(t *testing.T) {
	if got := AncestorsHeaderValue(context.Background()); got != "" {
		t.Fatalf("empty set must serialize to \"\" (emit omits header), got %q", got)
	}
	if got := ParseAncestorsHeader(""); got != nil {
		t.Fatalf("empty header must parse to nil, got %v", got)
	}
	// Trailing/embedded empty tokens are dropped.
	if got := ParseAncestorsHeader(",,a,,"); len(got) != 1 || got[0] != "a" {
		t.Fatalf("empty-token drop failed: got %v", got)
	}
}
