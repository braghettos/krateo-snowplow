// prewarm_engine_seed_order_test.go — #42: the FIRST-NAV-FIRST ordering
// falsifier for seedScopeYielding.
//
// ROOT CAUSE (docs/prewarm-first-nav-seed-trace-2026-07-03.md): the boot seed
// ran RESTActions FIRST, and a single high-fan-out RA (settings-krateo-status
// → apps/deployments = 10190 per-composition bindings at 50K) drowned the
// 8-minute boot budget, so the WIDGETS loop never ran → the dashboard's
// dashboard-flex widget cell (flexes GVR, single-digit targets) was never
// per-user-seeded → 750ms cold first-nav.
//
// THE FIX (prewarm_engine_boot.go): run the WIDGETS loop before the
// RESTActions loop (seedClassOrderFn), and process RESTActions in ascending
// len(targets) order. The discriminating property: when the budget deadline
// fires inside the expensive restaction fan-out, the cheap WIDGET cell is
// STILL seeded first.
//
// HARNESS SHAPE (feedback_falsifier_shape_must_discriminate): K>1 cohorts ×
// M>1 targets, ONE high-fan-out restaction (large target set whose per-target
// seed consumes the budget) + one dashboard-flex-class widget (small target
// set). The seed primitives (via the seams) record an ordered event log and
// consume wall-clock so the budget expires mid-fan-out. The budget is a real
// context deadline.
//
//   - GREEN arm  = production order [widgets, restactions]: the widget Put is
//     recorded BEFORE the context deadline fires.
//   - RED arm    = reverted order [restactions, widgets] (via the
//     seedClassOrderFn seam): the restaction fan-out eats the budget FIRST,
//     the loop aborts on ctx.Err() before the widgets loop runs, so the
//     widget Put is NEVER recorded. This is the RED arm that proves the
//     ordering — not incidental luck — is what warms the first-nav cell.
//
// SEAMS USED: enumeratePrewarmTargetsForGVRFn (per-GVR target sets),
// restActionTargetGVRFn (RA → its target GVR, no apiserver), seedOneWidgetFn /
// seedOneRestactionFn (per-target primitives that record + consume budget),
// seedClassOrderFn (the #42 order seam, flipped for the RED arm). Production
// ALWAYS binds seedClassOrderFn to [widgets, restactions].
//
// Pure unit: no cluster, no informer, no apiserver, -race clean. Does NOT
// touch ./internal/rbac/... (destructive TestMain).

package dispatchers

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/krateoplatformops/plumbing/endpoints"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// orderSeedGVRs — the widget GVR (cheap: few targets) and the high-fan-out
// restaction's TARGET GVR (expensive: many targets). Distinct so the seam
// enumerator returns different-sized target sets per class.
var (
	orderWidgetGVR = schema.GroupVersionResource{
		Group: "widgets.templates.krateo.io", Version: "v1beta1", Resource: "flexes",
	}
	orderFanoutGVR = schema.GroupVersionResource{
		Group: "apps", Version: "v1", Resource: "deployments",
	}
	// orderCheapRAGVR — a SECOND restaction's target GVR with a SMALL target
	// set (the cheap first-nav RA). Used by the C-SORT arm to give the
	// ascending-len(targets) sort two genuinely different-sized RA target sets
	// to order (the sort is a no-op with a single RA).
	orderCheapRAGVR = schema.GroupVersionResource{
		Group: "core.krateo.io", Version: "v1", Resource: "cheaps",
	}
)

// orderSeedEvent records one seed Put in call order.
type orderSeedEvent struct {
	class      string // "widget" | "restaction"
	label      string // ns/name
	beforeDone bool   // was the seed context still live (budget not yet expired) when this Put ran?
}

// orderSeedRecorder is the shared, mutex-guarded ordered event log the seam
// primitives append to. It also holds the outer budget context so each Put can
// record whether the budget was still live at the moment it ran.
type orderSeedRecorder struct {
	mu           sync.Mutex
	events       []orderSeedEvent
	budgetCtx    context.Context
	perTargetDur time.Duration // wall-clock each restaction target consumes
}

func (r *orderSeedRecorder) record(class, label string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, orderSeedEvent{
		class:      class,
		label:      label,
		beforeDone: r.budgetCtx.Err() == nil,
	})
}

// widgetPutBeforeBudget reports whether a widget Put was recorded while the
// budget was still live.
func (r *orderSeedRecorder) widgetPutBeforeBudget() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.class == "widget" && e.beforeDone {
			return true
		}
	}
	return false
}

func (r *orderSeedRecorder) counts() (widgets, restactions int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		switch e.class {
		case "widget":
			widgets++
		case "restaction":
			restactions++
		}
	}
	return
}

// firstIndex returns the index of the FIRST recorded Put with the given
// class+label (ns/name), or -1 if none. Used by C-SORT to assert relative
// ordering (cheap RA Put recorded before expensive RA Put).
func (r *orderSeedRecorder) firstIndex(class, label string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, e := range r.events {
		if e.class == class && e.label == label {
			return i
		}
	}
	return -1
}

// putBeforeBudget reports whether a Put with class+label was recorded while
// the budget was still live.
func (r *orderSeedRecorder) putBeforeBudget(class, label string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.class == class && e.label == label && e.beforeDone {
			return true
		}
	}
	return false
}

// installOrderSeams wires all four production seams for one ordering run.
//
//   - enumeratePrewarmTargetsForGVRFn returns gvrCount[gvr] targets for the
//     GVR (widget GVR + one distinct target GVR per RA); unknown GVRs → nil.
//   - restActionTargetGVRFn maps each RA ref (by ns/name) to its target GVR
//     via raGVR, so the restaction loop's ascending-len(targets) SORT sees
//     genuinely different-sized target sets per RA (the C-SORT discriminator).
//   - seedOneWidgetFn / seedOneRestactionFn record the Put (class+label) and
//     consume rec.perTargetDur of wall-clock so the budget can expire
//     mid-sequence.
//
// No new seam: this reuses the exact seams the production code exposes.
func installOrderSeams(t *testing.T, rec *orderSeedRecorder,
	gvrCount map[schema.GroupVersionResource]int,
	raGVR map[string]schema.GroupVersionResource) {
	t.Helper()

	prevEnum := enumeratePrewarmTargetsForGVRFn
	enumeratePrewarmTargetsForGVRFn = func(gvr schema.GroupVersionResource, verb string) []cache.PrewarmTarget {
		return makeGVRTargets(gvr, gvrCount[gvr])
	}
	t.Cleanup(func() { enumeratePrewarmTargetsForGVRFn = prevEnum })

	prevTargetGVR := restActionTargetGVRFn
	restActionTargetGVRFn = func(_ context.Context, ref templatesv1.ObjectReference) (schema.GroupVersionResource, bool) {
		gvr, ok := raGVR[ref.Namespace+"/"+ref.Name]
		return gvr, ok
	}
	t.Cleanup(func() { restActionTargetGVRFn = prevTargetGVR })

	prevWidgetSeed := seedOneWidgetFn
	seedOneWidgetFn = func(_ context.Context, e navWidgetEntry, _ string) error {
		rec.record("widget", e.W.GetNamespace()+"/"+e.W.GetName())
		return nil
	}
	t.Cleanup(func() { seedOneWidgetFn = prevWidgetSeed })

	prevRASeed := seedOneRestactionFn
	seedOneRestactionFn = func(_ context.Context, _ string, ref templatesv1.ObjectReference, _ string) error {
		// Each target consumes wall-clock so the budget can expire mid-sequence
		// (the class-order RED) or mid-fan-out (the sort RED).
		time.Sleep(rec.perTargetDur)
		rec.record("restaction", ref.Namespace+"/"+ref.Name)
		return nil
	}
	t.Cleanup(func() { seedOneRestactionFn = prevRASeed })
}

// makeGVRTargets builds n distinct per-binding targets for a GVR.
func makeGVRTargets(gvr schema.GroupVersionResource, n int) []cache.PrewarmTarget {
	out := make([]cache.PrewarmTarget, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, cache.PrewarmTarget{
			BindingUID: gvr.Resource + "-uid-" + strconv.Itoa(i),
			Subject:    cache.SubjectIdentity{Username: gvr.Resource + "-user-" + strconv.Itoa(i)},
			GVR:        gvr,
			Verb:       "list",
		})
	}
	return out
}

// makeOrderWidget / makeOrderRA build the sole cheap widget and the sole
// high-fan-out RA ref for the run.
func makeOrderWidget(ns, name string) navWidgetEntry {
	w := &unstructured.Unstructured{}
	w.SetNamespace(ns)
	w.SetName(name)
	w.SetGroupVersionKind(schema.GroupVersionKind{
		Group: orderWidgetGVR.Group, Version: orderWidgetGVR.Version, Kind: "Flex",
	})
	return navWidgetEntry{W: w, GVR: orderWidgetGVR}
}

func makeOrderRA(ns, name string) templatesv1.ObjectReference {
	return templatesv1.ObjectReference{
		Reference:  templatesv1.Reference{Name: name, Namespace: ns},
		APIVersion: restActionGVR.Group + "/" + restActionGVR.Version,
		Resource:   restActionGVR.Resource,
	}
}

// quietLogging silences the seed's info logging for a run; we assert on the
// recorder, not logs.
func quietLogging(t *testing.T) {
	t.Helper()
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })
}

// runOrderArm runs seedScopeYielding once under a budget context for the
// CLASS-order falsifier: one cheap widget (few cohorts) + one high-fan-out RA
// (many targets whose per-target sleep is sized so the budget expires well
// inside the fan-out).
func runOrderArm(t *testing.T, order []seedClass, budget time.Duration, few, many int, perTarget time.Duration) *orderSeedRecorder {
	t.Helper()

	quietLogging(t)
	// customerInFlight must be 0 so engineYieldCheckpoint is a no-op.
	zeroCustomerInFlight()

	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	rec := &orderSeedRecorder{budgetCtx: ctx, perTargetDur: perTarget}
	ra := makeOrderRA("krateo-system", "settings-krateo-status")
	installOrderSeams(t, rec,
		map[schema.GroupVersionResource]int{orderWidgetGVR: few, orderFanoutGVR: many},
		map[string]schema.GroupVersionResource{ra.Namespace + "/" + ra.Name: orderFanoutGVR})

	prevOrder := seedClassOrderFn
	seedClassOrderFn = func() []seedClass { return order }
	t.Cleanup(func() { seedClassOrderFn = prevOrder })

	widgets := []navWidgetEntry{makeOrderWidget("krateo-system", "dashboard-flex")}
	ras := []templatesv1.ObjectReference{ra}

	// seedScopeYielding returns ctx.Err() when the budget cuts it off — that
	// is EXPECTED in the RED arm; both arms are non-fatal here.
	_ = seedScopeYielding(ctx, ras, widgets, endpoints.Endpoint{}, nil, "test-authn-ns")
	return rec
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// ── THE FALSIFIER ────────────────────────────────────────────────────────
//
// Both arms share the SAME inputs (K=2 cohorts on the cheap widget, M=200
// targets on the fan-out RA), the SAME budget, and the SAME per-target sleep
// sized so the fan-out alone (200 × 2ms = 400ms) FAR overruns the 60ms budget.
// The ONLY difference is the class order the seam dictates.

func TestSeedScopeYielding_FirstNavFirst_OrderingFalsifier(t *testing.T) {
	engineLatchTestMu.Lock() // shares customerInFlightCount + package seams
	defer engineLatchTestMu.Unlock()

	const (
		fewWidgetTargets  = 2                     // K>1 cohorts on the widget class
		manyFanoutTargets = 200                   // M>1 high fan-out (the 10190-analogue)
		perTargetSleep    = 2 * time.Millisecond  // 200×2ms = 400ms fan-out >> budget
		budget            = 60 * time.Millisecond // expires deep inside the fan-out
	)

	t.Run("GREEN_widgets_first_seeds_widget_before_budget", func(t *testing.T) {
		rec := runOrderArm(t,
			[]seedClass{seedClassWidgets, seedClassRestactions},
			budget, fewWidgetTargets, manyFanoutTargets, perTargetSleep)

		w, _ := rec.counts()
		if w == 0 {
			t.Fatalf("widgets-first: NO widget Put recorded at all (want %d); events=%+v", fewWidgetTargets, rec.events)
		}
		if !rec.widgetPutBeforeBudget() {
			t.Fatalf("widgets-first: widget Put was NOT recorded before the budget expired — "+
				"the fix must seed the first-nav widget cell before any high-fan-out tail; events=%+v", rec.events)
		}
		t.Logf("GREEN: widget Put recorded before budget (widgets=%d); ordering fix warms first-nav cell", w)
	})

	t.Run("RED_restactions_first_starves_widget", func(t *testing.T) {
		rec := runOrderArm(t,
			[]seedClass{seedClassRestactions, seedClassWidgets}, // reverted order
			budget, fewWidgetTargets, manyFanoutTargets, perTargetSleep)

		// The restaction fan-out must have started (it eats the budget) and the
		// widget Put must NOT have been recorded before the budget expired — the
		// pre-fix 750ms cold-first-nav condition.
		_, ra := rec.counts()
		if ra == 0 {
			t.Fatalf("restactions-first: expected the fan-out to run (and eat the budget) but 0 restaction Puts recorded; events=%+v", rec.events)
		}
		if rec.widgetPutBeforeBudget() {
			t.Fatalf("RED arm FAILED TO GO RED: a widget Put was recorded before the budget expired even with "+
				"restactions-first — the harness does not discriminate ordering (fan-out sleep too small / budget too large); events=%+v", rec.events)
		}
		t.Logf("RED: restactions-first ate the budget (restactionPuts=%d), widget cell NOT warmed before budget — matches pre-fix cold first-nav", ra)
	})
}

// ── C-SORT: the within-restaction ascending-len(targets) SORT falsifier ────
//
// The class-order falsifier above proves widgets-first, but with a SINGLE
// restaction the ascending-len(targets) sort orders nothing — an inverted or
// removed comparator would go undetected (the #46 masking pattern). C-SORT
// gives the sort ≥2 restactions with genuinely different-sized target sets
// (cheap=few, expensive=many) and a budget that fits the cheap RA's targets
// but NOT the expensive one's, then asserts the CHEAP RA is seeded FIRST and
// lands before the budget — REGARDLESS of the input ref order.
//
// The discriminator (no comparator seam needed): run the SAME inputs in BOTH
// ref orders — ascending [cheap, expensive] and descending [expensive, cheap]
// — and require the SAME outcome (cheap-first, cheap-before-budget). Only a
// correct ascending sort satisfies BOTH arms: a removed sort would follow
// input order (descending arm → expensive first → cheap starved), and an
// inverted (descending) comparator would starve the cheap RA in BOTH. The
// masked-arm guard below proves the descending-INPUT arm genuinely depends on
// the sort (it fails the cheap-first assertion when we simulate an unsorted
// iteration over the same input).

// runSortArm runs seedScopeYielding with widgets DISABLED (class order =
// [restactions] only) and the given RA ref order, under a budget that fits the
// cheap RA (fewTargets) but not the expensive RA (manyTargets). Returns the
// recorder plus the ns/name labels of the cheap and expensive RAs.
func runSortArm(t *testing.T, refOrder []templatesv1.ObjectReference,
	few, many int, perTarget time.Duration, budget time.Duration) *orderSeedRecorder {
	t.Helper()

	quietLogging(t)
	zeroCustomerInFlight()

	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	rec := &orderSeedRecorder{budgetCtx: ctx, perTargetDur: perTarget}
	installOrderSeams(t, rec,
		map[schema.GroupVersionResource]int{orderCheapRAGVR: few, orderFanoutGVR: many},
		map[string]schema.GroupVersionResource{
			"krateo-system/dashboard-data":         orderCheapRAGVR, // cheap first-nav RA
			"krateo-system/settings-krateo-status": orderFanoutGVR,  // expensive tail
		})

	// widgets DISABLED — isolate the restaction sort.
	prevOrder := seedClassOrderFn
	seedClassOrderFn = func() []seedClass { return []seedClass{seedClassRestactions} }
	t.Cleanup(func() { seedClassOrderFn = prevOrder })

	_ = seedScopeYielding(ctx, refOrder, nil, endpoints.Endpoint{}, nil, "test-authn-ns")
	return rec
}

func TestSeedScopeYielding_CheapCohortFirst_SortFalsifier(t *testing.T) {
	engineLatchTestMu.Lock()
	defer engineLatchTestMu.Unlock()

	const (
		fewCheapTargets   = 3                     // cheap RA — fits the budget
		manyFanoutTargets = 200                   // expensive RA — overruns the budget
		perTargetSleep    = 2 * time.Millisecond  // per-target wall-clock
		budget            = 60 * time.Millisecond // fits 3×2ms=6ms cheap, not 200×2ms=400ms expensive
	)
	cheap := makeOrderRA("krateo-system", "dashboard-data")
	expensive := makeOrderRA("krateo-system", "settings-krateo-status")
	cheapLabel := cheap.Namespace + "/" + cheap.Name
	expensiveLabel := expensive.Namespace + "/" + expensive.Name

	// assertCheapFirst is the shared GREEN assertion: the cheap RA must be
	// seeded FIRST and land before the budget; the expensive RA is the one
	// starved (0 or truncated Puts after the budget).
	assertCheapFirst := func(t *testing.T, rec *orderSeedRecorder, arm string) {
		t.Helper()
		ci := rec.firstIndex("restaction", cheapLabel)
		ei := rec.firstIndex("restaction", expensiveLabel)
		if ci < 0 {
			t.Fatalf("%s: cheap RA %q never seeded (want first); events=%+v", arm, cheapLabel, rec.events)
		}
		if !rec.putBeforeBudget("restaction", cheapLabel) {
			t.Fatalf("%s: cheap RA %q Put did NOT land before the budget — the ascending sort must seed the "+
				"cheap first-nav RA before the expensive tail; events=%+v", arm, cheapLabel, rec.events)
		}
		if ei >= 0 && ei < ci {
			t.Fatalf("%s: expensive RA %q (idx %d) was seeded BEFORE cheap RA %q (idx %d) — ascending "+
				"len(targets) sort violated; events=%+v", arm, expensiveLabel, ei, cheapLabel, ci, rec.events)
		}
		t.Logf("%s: cheap RA seeded first (idx %d, before budget); expensive tail starved (firstIdx %d) — ascending sort load-bearing", arm, ci, ei)
	}

	// GREEN 1 — input already ascending [cheap, expensive]. Trivially the sort
	// keeps cheap first.
	t.Run("GREEN_input_ascending", func(t *testing.T) {
		rec := runSortArm(t, []templatesv1.ObjectReference{cheap, expensive},
			fewCheapTargets, manyFanoutTargets, perTargetSleep, budget)
		assertCheapFirst(t, rec, "ascending-input")
	})

	// GREEN 2 — input DESCENDING [expensive, cheap]. This is the arm the sort
	// actually earns: without the ascending sort, the expensive RA would run
	// first (input order) and starve the cheap RA. Same asserted outcome as
	// GREEN 1 proves the outcome depends on the SORT, not the input order.
	t.Run("GREEN_input_descending_sort_reorders", func(t *testing.T) {
		rec := runSortArm(t, []templatesv1.ObjectReference{expensive, cheap},
			fewCheapTargets, manyFanoutTargets, perTargetSleep, budget)
		assertCheapFirst(t, rec, "descending-input")
	})

	// MASKED-ARM GUARD — prove the descending-input arm genuinely discriminates
	// the sort: simulate an UNSORTED iteration over the SAME descending input
	// (expensive first) under the SAME budget, and assert it would STARVE the
	// cheap RA (cheap Put NOT before budget). If this "no-sort" simulation
	// still landed cheap-before-budget, the GREEN_descending arm would be
	// non-discriminating (budget too large / fan-out too small). This mirrors
	// the class-falsifier's RED-arm guard.
	t.Run("MASKED_no_sort_would_starve_cheap", func(t *testing.T) {
		quietLogging(t)
		zeroCustomerInFlight()

		ctx, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()
		rec := &orderSeedRecorder{budgetCtx: ctx, perTargetDur: perTargetSleep}

		// Directly replay the per-target primitive in UNSORTED descending input
		// order (expensive's manyTargets first, then cheap's fewTargets) — the
		// exact sequence the loop would run if the ascending sort were removed.
		replay := func(ref templatesv1.ObjectReference, n int) {
			for i := 0; i < n; i++ {
				if ctx.Err() != nil {
					return
				}
				time.Sleep(rec.perTargetDur)
				rec.record("restaction", ref.Namespace+"/"+ref.Name)
			}
		}
		replay(expensive, manyFanoutTargets) // unsorted: expensive first
		replay(cheap, fewCheapTargets)       // cheap starved behind the tail

		if rec.putBeforeBudget("restaction", cheapLabel) {
			t.Fatalf("MASKED guard failed: an UNSORTED descending replay still landed the cheap RA before the "+
				"budget — the GREEN_descending arm does NOT discriminate the sort (budget too large / fan-out too "+
				"small); events=%+v", rec.events)
		}
		t.Logf("MASKED: unsorted descending replay STARVES cheap RA (as expected) — so GREEN_descending's cheap-first result is EARNED by the ascending sort")
	})
}
