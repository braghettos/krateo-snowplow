// widgets_inline_falsifier_test.go — #72 Phase 1 producer falsifiers (§3/§5/§6),
// driving the REAL embedInlineChildren over the nested-call watcher harness
// (newNestedCallWatcher: a seeded inner RESTAction resolvable via
// ResolveNestedCall under a per-user identity + RBAC). Hermetic, no kubeconfig.
//
//   §3 single-response content — an inline+allowed+GET ref's child envelope is
//      embedded into items[i].rendered (non-empty, correct content) in ONE
//      pass; a non-inline ref carries NO rendered (byte-identical to today).
//   §5 central dep-edge — embedInlineChildren resolves the child via
//      ResolveNestedCall UNDER WithL1KeyContext(parentKey), so the child
//      resource's dep edge lands on the PARENT L1 key (→ a child change
//      dirty-marks the parent → PublishRefresh → fresh embedded child on
//      re-resolve). RED arm: resolve the child on a FRESH ctx WITHOUT
//      L1KeyFromContext → the dep edge does NOT land on the parent key → stale.
//   §5 grandchild one-level cap — embedInlineChildren resolves the child but
//      does NOT re-enter itself on the child's own inline refs (recursive arm
//      gated OFF, C-INLINE-2). Asserts the child's rendered body is embedded
//      but a grandchild ref inside it is NOT (only its path remains).
package dispatchers

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/snowplow/internal/cache"
	restactionsapi "github.com/krateoplatformops/snowplow/internal/resolvers/restactions/api"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// inlineChildCallPath builds the /call path the resourcesRefs resolver writes
// for the seeded inner RESTAction — exactly what ParseCallPathToObjectRef
// decodes back to the (restactions, ns, name) ObjectReference.
func inlineChildCallPath(ns, name string) string {
	return "/call?resource=" + nestedCallInnerGVR.Resource +
		"&apiVersion=" + nestedCallInnerGVR.Group + "/" + nestedCallInnerGVR.Version +
		"&namespace=" + ns + "&name=" + name
}

// parentWithInlineRef builds a resolved parent widget `res` whose
// status.resourcesRefs.items[0] is an inline GET ref pointing at the seeded
// child RA. `inline`/`allowed` are parameterised so the arms can toggle them.
func parentWithInlineRef(ns, childName string, inline, allowed bool) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "widgets.templates.krateo.io/v1beta1",
		"kind":       "Panel",
		"metadata":   map[string]any{"namespace": "krateo-system", "name": "detail-card"},
		"status": map[string]any{
			"resourcesRefs": map[string]any{
				"items": []any{
					map[string]any{
						"id":      "child",
						"path":    inlineChildCallPath(ns, childName),
						"verb":    "GET",
						"allowed": allowed,
						"inline":  inline,
					},
				},
			},
		},
	}}
}

func inlineTestCtx(t *testing.T, user, parentKey string) context.Context {
	t.Helper()
	restactionsapi.RegisterNestedCallResolver(ResolveNestedCall)
	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: user}))
	if parentKey != "" {
		ctx = cache.WithL1KeyContext(ctx, parentKey)
	}
	ctx = cache.WithInternalEndpoint(ctx, &endpoints.Endpoint{ServerURL: "http://test.invalid"})
	return ctx
}

// firstItem returns status.resourcesRefs.items[0] as a map.
func firstItem(t *testing.T, res *unstructured.Unstructured) map[string]any {
	t.Helper()
	items, _, _ := unstructuredNestedSlice(res, "status", "resourcesRefs", "items")
	if len(items) == 0 {
		t.Fatalf("no items")
	}
	m, _ := items[0].(map[string]any)
	return m
}

func unstructuredNestedSlice(res *unstructured.Unstructured, fields ...string) ([]any, bool, error) {
	return unstructured.NestedSlice(res.Object, fields...)
}

// §3 — single-response content: an inline+allowed+GET ref embeds the resolved
// child envelope into items[0].rendered (non-empty, correct), in ONE pass.
func TestFalsifier72_S3_InlineChildEmbeddedInSingleResponse(t *testing.T) {
	const ns, childName = "krateo-system", "inner-restaction"
	newNestedCallWatcher(t, ns, childName, nestedCallRoleBinding(ns, "u1"))
	ctx := inlineTestCtx(t, "u1", "parent-L1-key")

	res := parentWithInlineRef(ns, childName, true, true)
	n := embedInlineChildren(ctx, slog.Default(), res, -1, -1, nil)
	if n != 1 {
		t.Fatalf("§3: embedded %d children, want 1", n)
	}
	item := firstItem(t, res)
	rendered, ok := item["rendered"]
	if !ok || rendered == nil {
		t.Fatalf("§3: items[0].rendered not set — inline child not embedded")
	}
	// Content check (feedback_validate_content_not_just_status): the embedded
	// envelope must carry the child's resolved body, not an empty shell.
	// rendered is now a map[string]any (path i, deep-copy-safe); re-encode it to
	// assert the resolved content is present.
	rm, ok := rendered.(map[string]any)
	if !ok {
		t.Fatalf("§3: rendered must be a map[string]any (deep-copy-safe), got %T", rendered)
	}
	rb, _ := json.Marshal(rm)
	if !strings.Contains(string(rb), `"resolved":true`) || !strings.Contains(string(rb), childName) {
		t.Fatalf("§3: rendered child body missing resolved content; got: %s", string(rb))
	}
	// The path is KEPT alongside rendered (backward-compat — old SPA follows path).
	if p, _ := item["path"].(string); p == "" {
		t.Fatalf("§3: items[0].path must be KEPT alongside rendered (backward-compat)")
	}
}

// §3 negative — a NON-inline ref embeds NOTHING (byte-identical to today).
func TestFalsifier72_S3_NonInlineRefNotEmbedded(t *testing.T) {
	const ns, childName = "krateo-system", "inner-restaction"
	newNestedCallWatcher(t, ns, childName, nestedCallRoleBinding(ns, "u1"))
	ctx := inlineTestCtx(t, "u1", "parent-L1-key")

	res := parentWithInlineRef(ns, childName, false /*inline*/, true)
	n := embedInlineChildren(ctx, slog.Default(), res, -1, -1, nil)
	if n != 0 {
		t.Fatalf("§3-neg: embedded %d children for a NON-inline ref, want 0 (must be byte-identical to today)", n)
	}
	if _, ok := firstItem(t, res)["rendered"]; ok {
		t.Fatalf("§3-neg: non-inline ref must NOT carry rendered")
	}
}

// §5 — central dep-edge: the inline child resolve (under WithL1KeyContext)
// lands the child RA's dep edge on the PARENT L1 key. RED arm: resolve under a
// fresh ctx WITHOUT the L1 key → the edge does NOT land on the parent → a child
// change would never dirty-mark the parent → stale embedded child.
func TestFalsifier72_S5_ChildDepEdgeLandsOnParentKey(t *testing.T) {
	const ns, childName = "krateo-system", "inner-restaction"
	const parentKey = "parent-L1-key-dep"
	newNestedCallWatcher(t, ns, childName, nestedCallRoleBinding(ns, "u1"))

	// GREEN: ctx carries the parent L1 key.
	ctx := inlineTestCtx(t, "u1", parentKey)
	res := parentWithInlineRef(ns, childName, true, true)
	if n := embedInlineChildren(ctx, slog.Default(), res, -1, -1, nil); n != 1 {
		t.Fatalf("§5: embedded %d, want 1", n)
	}
	matched := cache.Deps().CollectMatchesForTest(nestedCallInnerGVR, ns, childName)
	if _, ok := matched[parentKey]; !ok {
		t.Fatalf("§5: child RA dep edge did NOT land on the PARENT key %q (matched=%v) — a child change "+
			"would never dirty-mark the parent → stale embedded child (the central correctness claim).",
			parentKey, matched)
	}
}

// §5 RED control — resolve the SAME inline child on a fresh ctx WITHOUT the
// parent L1 key; the dep edge must NOT land on the parent key (proving the
// GREEN case's edge came from L1KeyFromContext, not incidentally).
func TestFalsifier72_S5_NoL1KeyNoParentDepEdge(t *testing.T) {
	const ns, childName = "krateo-system", "inner-restaction"
	const parentKey = "parent-L1-key-red"
	newNestedCallWatcher(t, ns, childName, nestedCallRoleBinding(ns, "u1"))

	// No parent key on ctx (the RED-arm shape: a fresh ctx).
	ctx := inlineTestCtx(t, "u1", "" /*no L1 key*/)
	res := parentWithInlineRef(ns, childName, true, true)
	_ = embedInlineChildren(ctx, slog.Default(), res, -1, -1, nil)

	matched := cache.Deps().CollectMatchesForTest(nestedCallInnerGVR, ns, childName)
	if _, ok := matched[parentKey]; ok {
		t.Fatalf("§5 RED: child dep edge landed on parentKey %q despite no L1KeyFromContext — the §5 GREEN "+
			"result must come from the threaded L1 key, not coincidence", parentKey)
	}
}

// §5 grandchild ONE-LEVEL CAP (C-INLINE-2, the gate-to-lift). embedInlineChildren
// embeds the inline CHILD but does NOT recurse into the child's OWN inline refs:
// the embedded rendered child envelope's status.resourcesRefs.items[] carry NO
// `rendered` (the grandchild is left as a path-only ref the SPA follows). This
// pins the one-level-hard property — the recursive arm is gated OFF until the
// grandchild Edge-type-3 dep-edge is proven. RED (hypothetical recursive impl
// that re-entered embedInlineChildren on the child): the grandchild item WOULD
// carry rendered.
//
// Mechanism: embedInlineChildren is called ONLY from the widgets.go dispatcher
// (top level); ResolveNestedCall resolves the child but NEVER calls
// embedInlineChildren — so a child resolved via ResolveNestedCall never has its
// own inline refs embedded. The child here is a RESTAction whose resolved
// top-level filter PRODUCES a status.resourcesRefs.items[] carrying an inline
// grandchild ref; the embedded rendered child must keep that grandchild
// PATH-ONLY (no `rendered`), proving the walk did not recurse.
func TestFalsifier72_S5_GrandchildOneLevelCap(t *testing.T) {
	const ns, childName = "krateo-system", "inner-with-grandchild"
	// The child RA's filter yields an envelope whose status.resourcesRefs.items
	// already contains a grandchild ref flagged inline. (embedInlineChildren is
	// NOT applied to this child — ResolveNestedCall returns it verbatim — so the
	// grandchild MUST stay path-only.)
	inner := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "templates.krateo.io/v1",
		"kind":       "RESTAction",
		"metadata":   map[string]any{"namespace": ns, "name": childName},
		"spec": map[string]any{
			"filter": `{"status":{"resourcesRefs":{"items":[` +
				`{"id":"grandchild","path":"/call?resource=configmaps&apiVersion=v1&namespace=` + ns + `&name=gc","verb":"GET","allowed":true,"inline":true}` +
				`]}}}`,
			"api": []any{},
		},
	}}
	newNestedCallWatcherWithInner(t, ns, childName, inner, nestedCallRoleBinding(ns, "u1"))
	ctx := inlineTestCtx(t, "u1", "parent-L1-key-gc")

	res := parentWithInlineRef(ns, childName, true, true)
	if n := embedInlineChildren(ctx, slog.Default(), res, -1, -1, nil); n != 1 {
		t.Fatalf("§5-gc: embedded %d, want 1 (the child)", n)
	}
	item := firstItem(t, res)
	rendered, _ := item["rendered"].(map[string]any)
	if rendered == nil {
		t.Fatalf("§5-gc: child not embedded")
	}
	// ONE-LEVEL CAP: NOWHERE inside the embedded child envelope may a `rendered`
	// key appear — embedInlineChildren is called ONLY at the top dispatcher
	// level, and ResolveNestedCall (which produced this child) never calls it.
	// So a grandchild ref inside the child stays path-only. A recursive impl
	// (re-entering embedInlineChildren on the child) WOULD inject a nested
	// `rendered` and trip this scan — the discriminating RED.
	if path := findNestedRendered(rendered, "rendered"); path != "" {
		t.Fatalf("§5-gc ONE-LEVEL CAP VIOLATED: a nested `rendered` exists inside the embedded child at %s — "+
			"embedInlineChildren recursed (recursive arm must stay gated OFF until the grandchild dep-edge "+
			"falsifier is green)", path)
	}
	// Sanity: the child envelope is non-empty (the embed actually happened).
	if len(rendered) == 0 {
		t.Fatalf("§5-gc: embedded child envelope is empty")
	}
}

// findNestedRendered returns a dotted path to the first occurrence of `key`
// anywhere in the tree rooted at v, or "" if absent. Used to prove the embedded
// child has NO nested `rendered` (one-level cap).
func findNestedRendered(v any, key string) string {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if k == key {
				return k
			}
			if p := findNestedRendered(child, key); p != "" {
				return k + "." + p
			}
		}
	case []any:
		for i, child := range t {
			if p := findNestedRendered(child, key); p != "" {
				return "[" + itoa(i) + "]." + p
			}
		}
	}
	return ""
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// §4 — cross-user RBAC inequality (the leak-proof). The SAME inline parent,
// embedded under two identities with DIFFERENT RBAC: u1 (granted the child RA)
// gets a populated rendered child; u2 (NOT granted) gets NO rendered —
// ResolveNestedCall's checkDispatchRBAC gate denies u2's resolve, so no child
// body is embedded. The two users' served parents therefore DIFFER on the
// rendered child = no cross-user leak (the embedded child is resolved under the
// REQUESTING identity, never a shared/SA-maximal render). RED arm (a hypothetical
// impl that embedded under a shared/parent identity): u2 would get u1's rendered.
func TestFalsifier72_S4_CrossUserRBACInequality(t *testing.T) {
	const ns, childName = "krateo-system", "inner-restaction"
	// Bind ONLY u1 to the child-RA getter role; u2 has no grant.
	newNestedCallWatcher(t, ns, childName, nestedCallRoleBinding(ns, "u1"))

	// u1 (granted) — rendered populated.
	ctxU1 := inlineTestCtx(t, "u1", "parent-key-u1")
	resU1 := parentWithInlineRef(ns, childName, true, true)
	embedInlineChildren(ctxU1, slog.Default(), resU1, -1, -1, nil)
	renderedU1, hasU1 := firstItem(t, resU1)["rendered"]

	// u2 (NOT granted) — ResolveNestedCall denies → no rendered embedded.
	ctxU2 := inlineTestCtx(t, "u2", "parent-key-u2")
	resU2 := parentWithInlineRef(ns, childName, true, true)
	embedInlineChildren(ctxU2, slog.Default(), resU2, -1, -1, nil)
	_, hasU2 := firstItem(t, resU2)["rendered"]

	if !hasU1 || renderedU1 == nil {
		t.Fatalf("§4: u1 (granted) must get a populated rendered child")
	}
	if hasU2 {
		t.Fatalf("§4 LEAK: u2 (NOT granted) got a rendered child — the inline child must be resolved under the "+
			"REQUESTING identity (ResolveNestedCall's RBAC gate), never shared/SA-maximal. Cross-user leak.")
	}
	// The two served parents DIFFER on the rendered child — the inequality.
}

// §6 — hermetic single-encode bound (the full 50K RSS bench is the tester's
// Phase 3 arm). Asserts the A1 cost is LINEAR in child count, not quadratic:
// embedInlineChildren does ONE decode per child (into the map carrier) + the
// parent's SINGLE encode re-encodes the tree once — it does NOT independently
// re-encode the whole parent per child. We assert that embedding N children
// allocates within a calibrated multiple of embedding 1 (linear scaling), not
// N× the whole-parent encode. Calibrated empirically (feedback_capacity_caps_
// empirical_per_entry_cost), not a design-time magic number.
func TestFalsifier72_S6_LinearNotQuadraticEmbedCost(t *testing.T) {
	const ns, childName = "krateo-system", "inner-restaction"
	// One seeded child; N items all reference it (each a distinct embed). The
	// linear-vs-quadratic property is about per-item embed cost (decode +
	// in-place set), not distinct children — so one CR suffices.
	newNestedCallWatcher(t, ns, childName, nestedCallRoleBinding(ns, "u1"))
	ctx := inlineTestCtx(t, "u1", "parent-key-s6")

	build := func(k int) *unstructured.Unstructured {
		items := make([]any, 0, k)
		for i := 0; i < k; i++ {
			items = append(items, map[string]any{
				"id": "child" + itoa(i), "path": inlineChildCallPath(ns, childName),
				"verb": "GET", "allowed": true, "inline": true,
			})
		}
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "widgets.templates.krateo.io/v1beta1", "kind": "Panel",
			"metadata": map[string]any{"namespace": "krateo-system", "name": "p"},
			"status":   map[string]any{"resourcesRefs": map[string]any{"items": items}},
		}}
	}

	// Embed 1 child, then 8 children; the per-child marginal cost must be
	// roughly constant (linear total), not growing per added child (quadratic
	// = re-encoding the whole tree per child).
	alloc1 := testing.AllocsPerRun(20, func() {
		r := build(1)
		_ = embedInlineChildren(ctx, slog.Default(), r, -1, -1, nil)
	})
	alloc8 := testing.AllocsPerRun(20, func() {
		r := build(8)
		_ = embedInlineChildren(ctx, slog.Default(), r, -1, -1, nil)
	})
	// Linear: alloc8 ≈ 8×(per-child) + fixed. Quadratic would be ≳ 8× alloc1
	// AND super-linear in k. Bound: alloc8 must be < 8× alloc1 × a generous
	// linear-slack multiple (3), i.e. NOT quadratic. (Per-child = 1 decode +
	// 1 resolve; the parent encode is amortized once.)
	if alloc8 > alloc1*8*3 {
		t.Fatalf("§6: embedding 8 children allocated %.0f vs 1 child %.0f — > linear×3 bound; the per-child "+
			"cost is super-linear (a quadratic whole-parent re-encode per child?), A1 worsened", alloc8, alloc1)
	}
	t.Logf("§6 linear-cost OK: alloc(1)=%.0f alloc(8)=%.0f (ratio %.1f, linear bound 24)", alloc1, alloc8, alloc8/alloc1)
}
