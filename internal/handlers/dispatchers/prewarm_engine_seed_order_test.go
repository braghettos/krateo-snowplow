// prewarm_engine_seed_order_test.go — #42 FIX-E: the rank-major, class-
// interleaved, first-nav-ordered seed falsifier.
//
// ── FALSIFIER-SET MIGRATION (2026-07-04, TL-approved delete-with-migration) ──
// FIX-E replaced the class-major seedScopeYielding (whole-widgets-class then
// whole-restactions-class, dispatched by the seedClassOrderFn seam) with a
// UNIFIED rank-major, class-INTERLEAVED, first-nav-ordered loop (per identity
// rank r → widgets(r) in NavOrder → restactions(r)). The seedClassOrderFn seam
// was removed. THREE falsifiers that guarded the superseded model were DELETED;
// their properties are re-covered as follows (PM property-coverage map):
//
//   (a) TestSeedScopeYielding_FirstNavFirst_OrderingFalsifier
//       GUARDED: "widgets seed before restactions (restactions not starved by
//       widget work)". SUPERSEDED by per-rank interleave — restactions now seed
//       WITHIN each rank right after that rank's widgets, not after ALL widget
//       ranks. The underlying #42 property ("restactions must not be starved")
//       is now guarded by THIS file's rank-major assert (a lower-rank seed
//       before rank-1 completes → RED) + the revert-interleave mutation.
//   (b) TestSeedScopeYielding_CheapCohortFirst_SortFalsifier
//       GUARDED: "restactions in ascending len(targets) order (cheap RA before
//       the fan-out tail)". RETAINED — the RA within-rank ascending-len tiebreak
//       survives FIX-E; asserted EXPLICITLY here (property (b) block).
//   (c) TestSeedScopeYielding_CheapCohortFirst_WidgetsSortFalsifier
//       GUARDED: "cheap/critical widgets not starved by a whale (A2 widget
//       len-sort)". SUPERSEDED by NavOrder (count≠cost, proven twice — A2 seeded
//       a cheap late-nav widget before the dashboard's own widgets). The "cheap/
//       critical widget seeds first" property is now guarded by first-nav
//       (NavOrder) order + the sequence assert here.
//
// Condition 3: the E fixture derives NavOrder via the REAL harvest-stamping
// (newNavWidgetHarvester + harvestNavWidget in walk order), NOT hand-assigned —
// so it fails if the harvest-order stamping breaks, not only the seed loop.
//
// Hermetic, -race, seams only (no cluster). TestFixD_IdentityRankMajorSeedOrder
// (rank-major skeleton) stays GREEN unmodified in its own file.

package dispatchers

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// ── recorder: ordered (class,label,identity) seed events ────────────────────
type eSeedEvent struct {
	class    string // "widget" | "restaction"
	label    string // ns/name
	identity string // "user:<u>" | "group:<g>" | "anon"
}

type eSeedRecorder struct {
	mu     sync.Mutex
	events []eSeedEvent
}

func (r *eSeedRecorder) record(class, label, identity string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, eSeedEvent{class: class, label: label, identity: identity})
}

func (r *eSeedRecorder) snapshot() []eSeedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]eSeedEvent(nil), r.events...)
}

// eIdentityLabel renders the seeding identity from the cohort ctx the seam runs
// under (withCohortSeedContext installs WithUserInfo). Mirrors the rank key
// domain (Username, else first group).
func eIdentityLabel(ctx context.Context) string {
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

// quietLoggingE silences seed info logging for a run (assert on the recorder).
func quietLoggingE(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(eDiscard{}, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

type eDiscard struct{}

func (eDiscard) Write(p []byte) (int, error) { return len(p), nil }

// ── E fixture GVRs ──────────────────────────────────────────────────────────
// Each widget has its OWN GVR so the enumerator seam can give it a distinct
// identity set; the two RA target GVRs get distinct-sized sets for the RA
// ascending-len tiebreak assert (property (b)).
func eWidgetGVR(res string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "widgets.templates.krateo.io", Version: "v1beta1", Resource: res}
}

var (
	eCheapRAGVR  = schema.GroupVersionResource{Group: "core.krateo.io", Version: "v1", Resource: "cheaps"}
	eFanoutRAGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
)

type eID struct {
	name      string
	group     bool
	collapsed int
}

func (e eID) subject() cache.SubjectIdentity {
	if e.group {
		return cache.SubjectIdentity{Groups: []string{e.name}}
	}
	return cache.SubjectIdentity{Username: e.name}
}

// eIdentityTargets builds one PrewarmTarget per identity for a GVR; the
// CollapsedBindings count drives the rank.
func eIdentityTargets(gvr schema.GroupVersionResource, ids ...eID) []cache.PrewarmTarget {
	out := make([]cache.PrewarmTarget, 0, len(ids))
	for _, id := range ids {
		out = append(out, cache.PrewarmTarget{
			BindingUID:        "uid-" + id.name,
			Subject:           id.subject(),
			GVR:               gvr,
			Verb:              "list",
			CollapsedBindings: id.collapsed,
		})
	}
	return out
}

// eHarvestWidget stamps a widget through the REAL harvester (condition 3) so
// NavOrder comes from harvest sequence, not a hand-assigned value.
func eHarvestWidget(h *navWidgetHarvester, name string, gvr schema.GroupVersionResource) {
	w := &unstructured.Unstructured{}
	w.SetNamespace("krateo-system")
	w.SetName(name)
	w.SetGroupVersionKind(schema.GroupVersionKind{Group: gvr.Group, Version: gvr.Version, Kind: "W"})
	h.harvestNavWidget(w, gvr, -1, 1, -1, -1)
}

func eRA(name string) templatesv1.ObjectReference {
	return templatesv1.ObjectReference{
		Reference:  templatesv1.Reference{Name: name, Namespace: "krateo-system"},
		APIVersion: restActionGVR.Group + "/" + restActionGVR.Version,
		Resource:   restActionGVR.Resource,
	}
}

// TestFixE_RankMajorClassInterleaveFirstNavOrder — the #42 FIX-E falsifier.
//
// Fixture: 2 identity ranks (devs collapsed=442 = rank 1; ops collapsed=5 =
// rank 2), 3 widgets harvested in a deliberate NAV ORDER (dashboard-flex first,
// then obs-panel, then settings-card), each widget carrying BOTH identities; 2
// restactions (cheap 1-target, fanout 3-target) each carrying devs.
func TestFixE_RankMajorClassInterleaveFirstNavOrder(t *testing.T) {
	engineLatchTestMu.Lock()
	defer engineLatchTestMu.Unlock()
	zeroCustomerInFlight()
	quietLoggingE(t)

	devs := eID{name: "devs", group: true, collapsed: 442}
	ops := eID{name: "ops", group: true, collapsed: 5}

	// REAL harvest-stamping (condition 3): NavOrder = harvest sequence.
	h := newNavWidgetHarvester()
	dashGVR, obsGVR, setGVR := eWidgetGVR("flexes"), eWidgetGVR("obs"), eWidgetGVR("cards")
	eHarvestWidget(h, "dashboard-flex", dashGVR) // NavOrder 0 — first-nav
	eHarvestWidget(h, "obs-panel", obsGVR)       // NavOrder 1
	eHarvestWidget(h, "settings-card", setGVR)   // NavOrder 2
	widgets := h.snapshot()
	if len(widgets) != 3 {
		t.Fatalf("harvest setup: want 3 widgets, got %d", len(widgets))
	}

	cheapRA, fanoutRA := eRA("cheap-ra"), eRA("fanout-ra")
	ras := []templatesv1.ObjectReference{fanoutRA, cheapRA} // hostile input order (fanout first)

	rec := &eSeedRecorder{}

	prevEnum := enumeratePrewarmTargetsForGVRFn
	enumeratePrewarmTargetsForGVRFn = func(gvr schema.GroupVersionResource, _ string) []cache.PrewarmTarget {
		switch gvr {
		case dashGVR, obsGVR, setGVR:
			return eIdentityTargets(gvr, devs, ops)
		case eCheapRAGVR:
			return eIdentityTargets(eCheapRAGVR, devs) // 1 target
		case eFanoutRAGVR:
			return eIdentityTargets(eFanoutRAGVR, devs, ops, eID{name: "filler", collapsed: 1}) // 3 targets
		}
		return nil
	}
	t.Cleanup(func() { enumeratePrewarmTargetsForGVRFn = prevEnum })

	prevTGVR := restActionTargetGVRFn
	restActionTargetGVRFn = func(_ context.Context, ref templatesv1.ObjectReference) (schema.GroupVersionResource, bool) {
		if ref.Name == "cheap-ra" {
			return eCheapRAGVR, true
		}
		return eFanoutRAGVR, true
	}
	t.Cleanup(func() { restActionTargetGVRFn = prevTGVR })

	prevW := seedOneWidgetFn
	seedOneWidgetFn = func(ctx context.Context, e navWidgetEntry, _ string) error {
		rec.record("widget", e.W.GetName(), eIdentityLabel(ctx))
		return nil
	}
	t.Cleanup(func() { seedOneWidgetFn = prevW })

	prevR := seedOneRestactionFn
	seedOneRestactionFn = func(ctx context.Context, _ string, ref templatesv1.ObjectReference, _ string) error {
		rec.record("restaction", ref.Name, eIdentityLabel(ctx))
		return nil
	}
	t.Cleanup(func() { seedOneRestactionFn = prevR })

	if err := seedScopeYielding(context.Background(), ras, widgets, endpoints.Endpoint{}, nil, "authn-ns"); err != nil {
		t.Fatalf("seedScopeYielding returned %v; want nil", err)
	}

	assertFixESequence(t, rec.snapshot())
}

// assertFixESequence verifies rank-major / class-interleave / nav-order /
// RA-tiebreak on the recorded event sequence.
func assertFixESequence(t *testing.T, ev []eSeedEvent) {
	t.Helper()

	// (a) RANK-MAJOR + INTERLEAVE: last rank-1 (devs) event < first rank-2 (ops)
	// event, across BOTH classes.
	lastDevs, firstOps := -1, len(ev)
	for i, e := range ev {
		switch e.identity {
		case "group:devs":
			lastDevs = i
		case "group:ops":
			if i < firstOps {
				firstOps = i
			}
		}
	}
	if lastDevs < 0 || firstOps == len(ev) {
		t.Fatalf("FIX-E: expected both devs (rank1) and ops (rank2) seeds; events=%+v", ev)
	}
	if lastDevs >= firstOps {
		t.Fatalf("FIX-E rank-major VIOLATED: a rank-2 (ops) seed at idx %d ran BEFORE the last rank-1 (devs) seed at idx %d — "+
			"a lower rank must not interleave before rank-1 completes; events=%+v", firstOps, lastDevs, ev)
	}

	// Within rank-1 (devs): (c) widgets in NavOrder; widgets-before-restactions
	// (per-rank + FIX-F segment shape); (b) RAs ascending-len tiebreak.
	var r1widgets, r1ras []string
	for _, e := range ev {
		if e.identity != "group:devs" {
			continue
		}
		if e.class == "widget" {
			if len(r1ras) > 0 {
				t.Fatalf("FIX-E: rank-1 widget %q seeded AFTER a rank-1 restaction — per-rank shape is widgets-then-restactions; events=%+v", e.label, ev)
			}
			r1widgets = append(r1widgets, e.label)
		} else {
			r1ras = append(r1ras, e.label)
		}
	}
	// (c) NavOrder: dashboard-flex(0) → obs-panel(1) → settings-card(2).
	if got, want := strings.Join(r1widgets, ","), "dashboard-flex,obs-panel,settings-card"; got != want {
		t.Fatalf("FIX-E property (c) NavOrder VIOLATED: rank-1 widget order = %q; want first-nav %q "+
			"(harvest-stamped NavOrder, NOT A2 count-sort)", got, want)
	}
	// (b) RA ascending-len tiebreak: cheap-ra (1 target) before fanout-ra (3).
	seenCheap := false
	for _, ra := range r1ras {
		if ra == "fanout-ra" && !seenCheap {
			t.Fatalf("FIX-E property (b) RA ascending-len tiebreak VIOLATED: fanout-ra (3 targets) seeded before cheap-ra (1 target); rank-1 RA order=%v", r1ras)
		}
		if ra == "cheap-ra" {
			seenCheap = true
		}
	}
	if !seenCheap {
		t.Fatalf("FIX-E: cheap-ra was not seeded under rank-1; RA order=%v events=%+v", r1ras, ev)
	}
}
