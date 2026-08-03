// nested_resolve_ancestor_test.go — #79 RC-1 / RC-4 seam falsifiers. The
// ancestor-set is an IMMUTABLE copy-on-descend path set, NOT a global visited
// set. RC-1 proves the diamond distinction; RC-4 (-race) proves concurrent
// sibling descents never race a shared map.
package cache

import (
	"context"
	"sync"
	"testing"
)

// TestRC1_AncestorSetIsPathNotGlobalVisited is the CRUX (RC-1). A diamond
// P→{C1,C2}→L: L is reachable on BOTH branches but is NOT an ancestor of itself
// on either branch, so it must NOT be reported present when descending the
// second branch. A global visited set would (wrongly) mark L present after the
// first branch. The ancestor set (path-scoped, copy-on-descend) does not.
func TestRC1_AncestorSetIsPathNotGlobalVisited(t *testing.T) {
	root := context.Background()

	// Branch 1: P → C1 → (about to resolve L).
	p := WithNestedResolveAncestor(root, "P")
	c1 := WithNestedResolveAncestor(p, "C1")
	if NestedResolveAncestorPresent(c1, "L") {
		t.Fatal("RC-1: L must NOT be an ancestor on branch 1 before it is descended")
	}
	// Descend L on branch 1.
	c1L := WithNestedResolveAncestor(c1, "L")
	if !NestedResolveAncestorPresent(c1L, "L") {
		t.Fatal("RC-1: L must be its own ancestor once descended (self-ref would stop here)")
	}

	// Branch 2: P → C2 → L. Crucially, descending from `p` again (NOT from the
	// branch-1 context) — L was on branch 1's path but is NOT on branch 2's.
	c2 := WithNestedResolveAncestor(p, "C2")
	if NestedResolveAncestorPresent(c2, "L") {
		t.Fatal("RC-1 CRUX: L is on branch-1's path but NOT branch-2's — a diamond must " +
			"resolve L on BOTH branches (ancestor set != global visited set)")
	}
	// And C1 (a sibling) must not leak into branch 2.
	if NestedResolveAncestorPresent(c2, "C1") {
		t.Fatal("RC-1: sibling C1 must NOT be an ancestor of branch-2 (path-scoped)")
	}
}

// TestRC1_ParentSetNotMutatedByDescent — copy-on-descend: extending a child
// context must NOT add the node to the PARENT's set (else siblings would see it).
func TestRC1_ParentSetNotMutatedByDescent(t *testing.T) {
	p := WithNestedResolveAncestor(context.Background(), "P")
	_ = WithNestedResolveAncestor(p, "child-A")
	if NestedResolveAncestorPresent(p, "child-A") {
		t.Fatal("RC-1: descending child-A wrongly mutated the parent set (not copy-on-descend)")
	}
}

// TestRC4_ConcurrentDescentsNoRace is the HARD -race arm (RC-4). Many
// goroutines each descend their OWN child node from a shared parent context
// concurrently. If WithNestedResolveAncestor mutated the parent's map instead of
// copying, the concurrent map writes would data-race (go test -race fails) and
// siblings would contaminate each other. Each sibling must see ONLY its own node
// + the parent's, never another sibling's.
func TestRC4_ConcurrentDescentsNoRace(t *testing.T) {
	parent := WithNestedResolveAncestor(context.Background(), "P")
	const N = 64
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mine := nodeName(i)
			child := WithNestedResolveAncestor(parent, mine)
			if !NestedResolveAncestorPresent(child, mine) {
				errs[i] = errRC4("own node missing", mine)
				return
			}
			if !NestedResolveAncestorPresent(child, "P") {
				errs[i] = errRC4("parent node missing", mine)
				return
			}
			// A DIFFERENT sibling's node must NOT be present (no shared-map
			// contamination).
			other := nodeName((i + 1) % N)
			if other != mine && NestedResolveAncestorPresent(child, other) {
				errs[i] = errRC4("sibling node leaked in", other)
			}
		}(i)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			t.Fatalf("RC-4: %v", e)
		}
	}
}

func nodeName(i int) string { return "sib-" + string(rune('A'+i%26)) + string(rune('0'+i/26)) }

type rc4Err struct{ what, node string }

func (e rc4Err) Error() string { return e.what + " (node " + e.node + ")" }
func errRC4(what, node string) error { return rc4Err{what, node} }
