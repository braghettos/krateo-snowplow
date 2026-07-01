// nested_resolve_ancestor.go — #79: the nested-resolve ANCESTOR-SET seam, the
// cycle-precise companion to the depth counter (nested_call_depth.go).
//
// WHY (distinct from depth): the depth cap (nestedCallMaxDepth=8) is a coarse
// backstop — it terminates a cycle, but only after 8 wasted hops, and it CANNOT
// distinguish a genuine self-reference (A resolve:true-refs A) from a legitimate
// deep-but-acyclic chain. A self-referential RA (fsa-y2-composition-resources's
// allCompositionResources self-element) recursed to depth 8 EVERY cold /call —
// the 1.5.20 cold-path amplifier: ~250 resolver invocations + 8× the envelope
// materialisation per self-ref, per tree. The ancestor set stops the recursion
// at the FIRST self-reentry (the node is already on the current root→node path),
// returning the raw CR (resolve:false semantics), leaving the depth cap as the
// backstop for a non-cyclic pathological chain.
//
// ANCESTOR SET ≠ GLOBAL VISITED SET (the crux, RC-1): the set is the nodes on
// the CURRENT root→here path ONLY. A legitimate DIAMOND P→{C1,C2}→L resolves L
// on BOTH branches (L is not an ancestor of itself on either branch) — a global
// visited set would wrongly stop the second L. So the set is scoped to the
// descent path.
//
// IMMUTABLE COPY-ON-DESCEND (the concurrency contract, RC-4 — nested fan-out is
// a CONCURRENT errgroup): WithNestedResolveAncestor returns a NEW map that is a
// copy of the parent set plus the one new node. It NEVER mutates the parent's
// map. Sibling branches therefore each get their own independent extended copy
// and never write a shared map — no data race, no cross-branch contamination
// (feedback_shared_vs_copy_is_a_concurrency_change: converting a private copy to
// a shared reference is a concurrency change; here we deliberately keep each
// descent's set private via copy-on-descend). The stored map is treated as
// read-only after construction.
//
// Mirrors WithNestedCallDepth: a distinct unexported empty-struct key so
// external packages cannot collide via a raw string key.

package cache

import "context"

// ctxKeyNestedResolveAncestorType is the typed empty-struct context key.
type ctxKeyNestedResolveAncestorType struct{}

var ctxKeyNestedResolveAncestor = ctxKeyNestedResolveAncestorType{}

// WithNestedResolveAncestor returns a child context whose ancestor set is the
// parent set (read off ctx) PLUS node. The returned set is a fresh copy — the
// parent's set is never mutated (copy-on-descend), so concurrent sibling
// descents each hold an independent set and never race a shared map.
//
// node is the cycle identity of a resolve target — canonically
// "<resource>/<namespace>/<name>" (the caller builds it; the exact string is
// opaque to this seam, it only needs set-membership). An empty ctx is returned
// unchanged.
func WithNestedResolveAncestor(ctx context.Context, node string) context.Context {
	if ctx == nil {
		return ctx
	}
	parent, _ := ctx.Value(ctxKeyNestedResolveAncestor).(map[string]struct{})
	// Copy-on-descend: a new map sized parent+1, never a write to `parent`.
	next := make(map[string]struct{}, len(parent)+1)
	for k := range parent {
		next[k] = struct{}{}
	}
	next[node] = struct{}{}
	return context.WithValue(ctx, ctxKeyNestedResolveAncestor, next)
}

// NestedResolveAncestorPresent reports whether node is already on the current
// root→here descent path (i.e. resolving it again would be a cycle). Returns
// false when no ancestor set is attached (the outermost resolve).
func NestedResolveAncestorPresent(ctx context.Context, node string) bool {
	if ctx == nil {
		return false
	}
	set, _ := ctx.Value(ctxKeyNestedResolveAncestor).(map[string]struct{})
	if set == nil {
		return false
	}
	_, present := set[node]
	return present
}
