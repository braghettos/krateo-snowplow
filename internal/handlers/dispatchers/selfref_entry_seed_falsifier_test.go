// selfref_entry_seed_falsifier_test.go — #83 Option A falsifiers.
//
// THE DEFECT (docs/c2-y5y7-composition-resources-fanout-trace-2026-07-01.md):
// the #79 ancestor-set cycle-stop only sees nodes registered ON DESCENT via
// ResolveNestedCall (nested_call.go Step 4/5 WithNestedResolveAncestor). The
// OUTERMOST node — the top-level RESTAction/Widget the request is dispatching —
// was NEVER registered, so a composition RA whose allCompositionResources
// managed set includes ITSELF recursed one FULL hop before the cycle detector
// could fire. That inner self-resolve ran its OWN top-level jq filter over an
// empty `.discovery` stage → gojq "cannot iterate over: null" (wrapped at
// restactions.go:70) → per-item stage error → decline-to-cache
// (restactions.go stageErrSink gate) → permanent cold re-fan-out (~1500× per
// /call, never converging).
//
// THE FIX (Option A): the dispatcher entry seeds the top-level node into the
// ancestor set BEFORE resolving, so the FIRST self-reentry is an immediate
// cycle-stop (1 hop, raw CR, no inner resolve, no null-iterate, clean parent
// Put → convergence).
//
// THE DISCRIMINATOR (feedback_falsifier_shape_must_discriminate): the seed is
// only effective if the node string the DISPATCHER seeds is BYTE-IDENTICAL to
// the node string the cycle-stop later membership-checks. If the two derivations
// drift, the seed writes a key the check never looks up → the cycle-stop still
// fires one hop too late → the defect returns silently. These arms pin that
// equality (SA1), pin that the dispatcher's exact seed value drives the
// cycle-stop end-to-end (SA2, RED-proven), and pin the concurrency contract the
// new entry-writer must not break (SA3, -race).
package dispatchers

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jwtutil"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
)

// TestA_SA1_NodeKeyParity_SeedMatchesCycleStop is the drift discriminator. The
// dispatcher entry (restactions.go / widgets.go) and the cycle-stop
// (nested_call.go) now BOTH derive the node string via nestedResolveNodeKey.
// This arm pins that the shared helper produces the canonical
// "<resource>/<namespace>/<name>" the ancestor set is keyed on, for the real
// fsa-y7 shape. RED arm: if a future edit re-inlines either site with a
// different separator/order, this equality fails.
func TestA_SA1_NodeKeyParity_SeedMatchesCycleStop(t *testing.T) {
	const resource, ns, name = "restactions", "demo-system", "fsa-y7-composition-resources"

	// The canonical form the ancestor set is keyed on.
	want := resource + "/" + ns + "/" + name

	// The dispatcher-entry seed derivation (Option A) and the cycle-stop-check
	// derivation (nested_call.go Step 3.5) are the SAME function call now — so
	// this also proves they cannot diverge by construction.
	seedKey := nestedResolveNodeKey(resource, ns, name)
	if seedKey != want {
		t.Fatalf("SA1: node key = %q, want %q — the ancestor-set key shape drifted "+
			"from '<resource>/<namespace>/<name>'; the dispatcher seed would then "+
			"write a key the cycle-stop never looks up (defect returns silently)", seedKey, want)
	}

	// Membership must hold when a ctx is seeded with that exact derived key.
	ctx := cache.WithNestedResolveAncestor(context.Background(), seedKey)
	if !cache.NestedResolveAncestorPresent(ctx, nestedResolveNodeKey(resource, ns, name)) {
		t.Fatalf("SA1: a ctx seeded with nestedResolveNodeKey(%q,%q,%q) does not report "+
			"the SAME node present — seed vs check derivations diverged", resource, ns, name)
	}
}

// TestA_SA2_EntrySeedDrivesCycleStop is the end-to-end RED arm. It replicates
// EXACTLY what the dispatcher entry does — seed the top-level node with
// WithNestedResolveAncestor(nestedResolveNodeKey(GVR.Resource, ns, name)) — then
// drives the REAL ResolveNestedCall for that same node (the first self-reentry).
// With the seed present the reentry MUST cycle-stop (raw CR, no recursion, no
// depth error). This proves the dispatcher's seed VALUE is the one the cycle-stop
// consumes — not merely that the cycle-stop works when hand-seeded (that is
// RC-2's job).
//
// RED: remove the seed line (or seed a drifted key) → ResolveNestedCall recurses
// into restactions.Resolve, resolving the array-status projection into .status
// (the exact pre-A behaviour that then null-iterated on descent). The raw-vs-
// resolved status discriminates.
func TestA_SA2_EntrySeedDrivesCycleStop(t *testing.T) {
	const ns, name = "demo-system", "fsa-y7-composition-resources"
	newNestedCallWatcherWithInner(t, ns, name,
		nestedInnerRESTActionArrayStatus(ns, name),
		nestedCallRoleBinding(ns, "authorized-user"))

	base := xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: "authorized-user"}),
	)
	// EXACTLY the dispatcher-entry seed (Option A): the node is derived from the
	// fetched GVR.Resource + ns + name via the shared helper. nestedCallInnerGVR
	// .Resource == "restactions" is the GVR objects.Get fills for the inner CR,
	// matching the dispatcher's got.GVR.Resource.
	ctx := cache.WithNestedResolveAncestor(base,
		nestedResolveNodeKey(nestedCallInnerGVR.Resource, ns, name))

	ref := templates.ObjectReference{
		Reference:  templates.Reference{Name: name, Namespace: ns},
		Resource:   nestedCallInnerGVR.Resource,
		APIVersion: nestedCallInnerGVR.Group + "/" + nestedCallInnerGVR.Version,
	}
	raw, err := ResolveNestedCall(ctx, ref, 0, 0, nil)
	if err != nil {
		// A "depth limit exceeded" here would mean the entry-seed did NOT match
		// on the first reentry and the resolve fell through to the depth-8
		// backstop (the pre-A behaviour) — call that out explicitly.
		if strings.Contains(err.Error(), "depth limit exceeded") {
			t.Fatalf("SA2: entry-seed must stop at the FIRST reentry, NOT recurse to the depth-8 backstop; got %v", err)
		}
		t.Fatalf("SA2: entry-seeded self-reentry must cycle-STOP cleanly (raw CR), got error: %v", err)
	}
	var envelope map[string]any
	if uerr := json.Unmarshal(raw, &envelope); uerr != nil {
		t.Fatalf("SA2: cycle-stop result is not a JSON object: %v\n got %s", uerr, raw)
	}
	// The RAW CR must survive (spec.filter literal intact, no resolved array in
	// status) — proving the inner restactions.Resolve did NOT run. This is the
	// exact property that eliminates the null-iterate: the inner top-level filter
	// never evaluates.
	specBytes, _ := json.Marshal(envelope["spec"])
	if !strings.Contains(string(specBytes), "team-a") {
		t.Fatalf("SA2: expected the RAW CR (spec.filter literal intact); got spec=%s", specBytes)
	}
	statusBytes, _ := json.Marshal(envelope["status"])
	if strings.Contains(string(statusBytes), "team-a") {
		t.Fatalf("SA2: status carries the RESOLVED projection — the entry seed did NOT drive "+
			"the cycle-stop (the inner resolve ran, which would then null-iterate on descent); "+
			"status=%s", statusBytes)
	}
}

// TestA_SA3_ConcurrentEntrySeedsAreIndependent is the -race concurrency arm.
// Option A adds a NEW writer of the ancestor set at the request entry; the set
// rides on ctx into the CONCURRENT errgroup nested fan-out. Two independent
// top-level requests (different self-nodes) seeding + membership-checking
// concurrently must never race a shared map and must never see each other's node
// (copy-on-descend keeps each descent's set private —
// feedback_shared_vs_copy_is_a_concurrency_change). RED under `-race` if the
// entry seed ever mutated a shared parent map instead of copy-on-descend.
func TestA_SA3_ConcurrentEntrySeedsAreIndependent(t *testing.T) {
	const nsA, nameA = "demo-system", "fsa-y7-composition-resources"
	const nsB, nameB = "other-system", "fsa-y9-composition-resources"
	keyA := nestedResolveNodeKey("restactions", nsA, nameA)
	keyB := nestedResolveNodeKey("restactions", nsB, nameB)

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			ctx := cache.WithNestedResolveAncestor(context.Background(), keyA)
			if !cache.NestedResolveAncestorPresent(ctx, keyA) {
				t.Errorf("SA3: request-A ctx must see its OWN seeded node A")
			}
			if cache.NestedResolveAncestorPresent(ctx, keyB) {
				t.Errorf("SA3: request-A ctx must NOT see request-B's node (cross-request bleed)")
			}
		}()
		go func() {
			defer wg.Done()
			ctx := cache.WithNestedResolveAncestor(context.Background(), keyB)
			if !cache.NestedResolveAncestorPresent(ctx, keyB) {
				t.Errorf("SA3: request-B ctx must see its OWN seeded node B")
			}
			if cache.NestedResolveAncestorPresent(ctx, keyA) {
				t.Errorf("SA3: request-B ctx must NOT see request-A's node (cross-request bleed)")
			}
		}()
	}
	wg.Wait()
}
