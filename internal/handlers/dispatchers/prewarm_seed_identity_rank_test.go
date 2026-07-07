// prewarm_seed_identity_rank_test.go — #42 FIX-D: identity-rank-major boot seed
// order (design §A5 FIX-D).
//
// FIX-D ranks IDENTITIES by their dedup collapsed-binding count DESCENDING and
// seeds RANK-MAJOR across ALL widgets: pass 1 = every widget × the rank-1
// identity (Group/devs ≈ the whole user population), pass 2 = rank-2, …. So the
// 95%-mix cohort's dashboard cells are ALL warm within the first pass regardless
// of a heavy-widget tail — count≠cost (A3: the A2 count-sort put the cheap
// WIDGET first, but per-identity cost dominates, so warming the top cohort
// across every widget first is the load-bearing order).
//
// D-1: the rank metric is CollapsedBindings from the dedup (cache.PrewarmTarget),
//   carried on seedTarget — NO static list / name literal.
// D-2: 3 identities with counts [442,5,1] → pass-1 seeds ALL widgets under the
//   442-identity BEFORE any 5- or 1-identity work; mutation (rank sort removed →
//   insertion/identity-key order) does NOT hold that property → RED.
// D-3: pure ordering — the (widget×identity) seed SET is unchanged (same
//   dispatch count + same identity set as a widget-major loop).
//
// Hermetic: reuses the order-test seams (enumeratePrewarmTargetsForGVRFn,
// seedOneWidgetFn, seedClassOrderFn=[widgets]); no cluster, -race clean.

package dispatchers

import (
	"context"
	"sync"
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// rankSeedEvent records one widget seed in call order, with the seeding
// identity (read from the cohort ctx the seam is invoked under).
type rankSeedEvent struct {
	widget   string
	identity string // "user:<u>" or "group:<g,...>"
}

type rankRecorder struct {
	mu     sync.Mutex
	events []rankSeedEvent
}

func (r *rankRecorder) record(widget, identity string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, rankSeedEvent{widget: widget, identity: identity})
}

// identityLabelFromCtx renders the seeding identity from the cohort ctx the
// seam runs under (withCohortSeedContext installs WithUserInfo). Mirrors the
// dedup/rank key domain (Username, else group set).
func identityLabelFromCtx(ctx context.Context) string {
	ui, err := xcontext.UserInfo(ctx)
	if err != nil {
		return "anon"
	}
	if ui.Username != "" {
		return "user:" + ui.Username
	}
	if len(ui.Groups) > 0 {
		return "group:" + ui.Groups[0]
	}
	return "anon"
}

// rankIdentityTargets — 3 identities with distinct collapsed-binding counts.
// devs=442 (rank 1), ops=5 (rank 2), installer SA=1 (rank 3).
func rankIdentityTargets() []cache.PrewarmTarget {
	return []cache.PrewarmTarget{
		{BindingUID: "C:devs", Subject: cache.SubjectIdentity{Groups: []string{"devs"}}, Verb: "list", CollapsedBindings: 442},
		{BindingUID: "C:ops", Subject: cache.SubjectIdentity{Groups: []string{"ops"}}, Verb: "list", CollapsedBindings: 5},
		{BindingUID: "C:sa", Subject: cache.SubjectIdentity{Username: "system:serviceaccount:krateo-system:installer"}, Verb: "list", CollapsedBindings: 1},
	}
}

func rankWidget(name string, gvr schema.GroupVersionResource) navWidgetEntry {
	w := &unstructured.Unstructured{}
	w.SetNamespace("krateo-system")
	w.SetName(name)
	w.SetGroupVersionKind(schema.GroupVersionKind{Group: gvr.Group, Version: gvr.Version, Kind: "W"})
	return navWidgetEntry{W: w, GVR: gvr}
}

func TestFixD_IdentityRankMajorSeedOrder(t *testing.T) {
	engineLatchTestMu.Lock()
	defer engineLatchTestMu.Unlock()
	zeroCustomerInFlight()

	// 3 widgets, each carrying the SAME 3 identities (counts 442/5/1).
	gvrA := schema.GroupVersionResource{Group: "widgets.templates.krateo.io", Version: "v1beta1", Resource: "wa"}
	gvrB := schema.GroupVersionResource{Group: "widgets.templates.krateo.io", Version: "v1beta1", Resource: "wb"}
	gvrC := schema.GroupVersionResource{Group: "widgets.templates.krateo.io", Version: "v1beta1", Resource: "wc"}
	widgets := []navWidgetEntry{rankWidget("wa", gvrA), rankWidget("wb", gvrB), rankWidget("wc", gvrC)}

	rec := &rankRecorder{}

	prevEnum := enumeratePrewarmTargetsForGVRFn
	enumeratePrewarmTargetsForGVRFn = func(gvr schema.GroupVersionResource, _ string) []cache.PrewarmTarget {
		out := rankIdentityTargets()
		for i := range out {
			out[i].GVR = gvr
		}
		return out
	}
	t.Cleanup(func() { enumeratePrewarmTargetsForGVRFn = prevEnum })

	prevSeed := seedOneWidgetFn
	seedOneWidgetFn = func(ctx context.Context, e navWidgetEntry, _ string, _ bool) error {
		rec.record(e.W.GetName(), identityLabelFromCtx(ctx))
		return nil
	}
	t.Cleanup(func() { seedOneWidgetFn = prevSeed })

	// #42 FIX-E: the seedClassOrderFn=[widgets] seam-set (formerly here) was
	// removed — the seam is deleted; widgets-only isolation is carried by the
	// nil restactions arg below (always was; the seam-set was redundant).
	// Assertions unchanged. See prewarm_engine_seed_order_test.go migration map.
	if err := seedScopeYielding(context.Background(), nil, widgets, endpoints.Endpoint{}, nil, "authn-ns", false, false); err != nil {
		t.Fatalf("seedScopeYielding returned %v; want nil", err)
	}

	// D-3 pure ordering: the seed SET is unchanged — 3 widgets × 3 identities =
	// 9 dispatches, each identity present per widget.
	if len(rec.events) != 9 {
		t.Fatalf("D-3: expected 9 (widget×identity) seeds, got %d: %+v", len(rec.events), rec.events)
	}

	// D-2 rank-major: EVERY rank-1 (group:devs, count 442) seed must precede
	// EVERY rank-2 (group:ops) and rank-3 (SA) seed. Find the last index of a
	// devs seed and the first index of any non-devs seed.
	lastDevs, firstNonDevs := -1, len(rec.events)
	devsCount := 0
	for i, e := range rec.events {
		if e.identity == "group:devs" {
			lastDevs = i
			devsCount++
		} else if i < firstNonDevs {
			firstNonDevs = i
		}
	}
	if devsCount != 3 {
		t.Fatalf("D-2: expected the rank-1 devs identity seeded on all 3 widgets, got %d; events=%+v", devsCount, rec.events)
	}
	if lastDevs >= firstNonDevs {
		t.Fatalf("D-2: identity-rank-major VIOLATED — a non-rank-1 seed (idx %d) ran BEFORE the last rank-1 devs seed (idx %d). "+
			"pass-1 must seed ALL widgets under the 442-identity before any lower rank; events=%+v", firstNonDevs, lastDevs, rec.events)
	}

	// Rank-2 (ops) must precede rank-3 (SA) too (full descending rank order).
	lastOps, firstSA := -1, len(rec.events)
	for i, e := range rec.events {
		if e.identity == "group:ops" {
			lastOps = i
		} else if e.identity == "user:system:serviceaccount:krateo-system:installer" && i < firstSA {
			firstSA = i
		}
	}
	if lastOps >= firstSA {
		t.Fatalf("D-2: rank-2 ops (last idx %d) must precede rank-3 SA (first idx %d); events=%+v", lastOps, firstSA, rec.events)
	}
}
