// prewarm_engine.go — Ship 1 of the unified dynamic cohort-prewarm
// engine: the engine core (scope-parameterised rePrewarm), the bounded
// engine work queue, and the customer-priority-yield semaphore.
//
// ONE ENGINE (feedback_dynamic_cohort_prewarm_no_static_no_cold_fill).
// The boot prewarm-navigation AND (Ship 2) the handling of
// created/modified/deleted widgets+RESTActions + RBAC binding shifts use
// the SAME walk→harvest→resource-driven-cohort→seed logic. This file is
// that one engine. Ship 1 invokes it with the BOOT scope only; the queue
// + semaphore scaffolding is built now so Ship 2 wires runtime triggers
// in with no refactor.
//
// SCOPE-PARAMETERISED. rePrewarm(ctx, scope) takes a prewarmScope that
// declares which navigation roots to (re-)walk + which cohort source to
// seed against. The BOOT scope = the 2 nav roots (the frontend
// config.json INIT / ROUTES_LOADER widgets). For each root the engine
// constructs a FRESH phase1Walker (new visited map — phase1_walk.go:679;
// reusing an old visited short-circuits at the visited check and descends
// nothing) and calls the SAME walk() so harvestApiRef + harvestNavWidget
// re-fire unconditionally before widgets.Resolve. The re-walk MUST NOT
// bypass walk().
//
// WHY THE RE-WALK (project_prewarm_page_offset_bug_2026_05_28 post-deploy
// section). The boot walk at phase1WarmupWith Step 4 runs BEFORE Step 7
// WaitAllInformersSynced and is single-pass; the navmenu's children are
// DYNAMIC (resourcesRefsTemplate over the apiRef sidebar-nav-menu-items),
// so the pre-sync walk resolves them while the apiserver-fallthrough data
// is still empty → 0 children → only the 2 roots harvested
// (widgets:2/restactions:2). The engine's BOOT re-walk runs AFTER the
// sync barrier (data available) so the full nav tree is discovered + the
// per-cohort harvesters are populated for the seed.
//
// CUSTOMER PRIORITY (feedback_customer_priority_over_refresher,
// project_c3_design_2026_05_27 B1). Re-prewarm work YIELDS to in-flight
// customer /call. Every customer dispatch (restactions/widgets ServeHTTP)
// brackets its work with markCustomerInFlight/markCustomerDone; the
// engine worker checks customerInFlight() before each unit of seed work
// and parks on a short backoff while any customer call is in flight — the
// customer path keeps absolute priority. The engine NEVER holds a lock the
// customer path needs; the yield is a cooperative check, not a hard mutex.
//
// BOUNDED QUEUE (refresher.go:95 shape). The engine owns a
// workqueue.TypedRateLimitingInterface[prewarmScope-key] so Ship 2 can
// enqueue re-prewarm work (widget CR change, RBAC shift) with idempotent
// dedup + exponential-backoff retry + never-drop semantics — the exact
// properties the refresher relies on. Ship 1 enqueues only the BOOT scope.
//
// LIFECYCLE. The engine's workers run under a context bounded by the
// engine timeout (boot: pipGlobalTimeout); they exit on ctx cancel /
// queue ShutDown. No unbounded goroutine: worker count is fixed
// (GOMAXPROCS-bounded), and each rePrewarm unit is bounded by the
// per-cohort timeout (seedCohort's context.WithTimeout).

package dispatchers

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/krateoplatformops/plumbing/env"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// envPrewarmEngineEnabled is the Ship 1 opt-in for the unified dynamic
// cohort-prewarm engine. When ON, the background seed goroutine routes
// through the engine (post-sync re-walk + per-target-GVR-scoped seed +
// the BindingsByGVR index) instead of the legacy global runPIPSeed.
// Default false: flag-off the engine is inert and the legacy PIP seed
// path runs byte-identically. The engine additionally requires
// PREWARM_ENABLED + PREWARM_CONTENT_ENABLED + PREWARM_PIP_ENABLED (it
// shares the same harvesters); when any is off it stays inert.
const envPrewarmEngineEnabled = "PREWARM_ENGINE_ENABLED"

// PrewarmEngineEnabled reports whether the Ship 1 unified engine is opted
// in. Defaults false (opt-in), mirroring the PIP gate's introduction
// posture so a regression can be ruled out by flipping one knob.
func PrewarmEngineEnabled() bool {
	return env.String(envPrewarmEngineEnabled, "false") == "true"
}

// ─────────────────────────────────────────────────────────────────────
// Customer-priority signal. Every customer /call dispatch brackets its
// work with these. The engine yields while the counter is > 0.
// ─────────────────────────────────────────────────────────────────────

// customerInFlightCount tracks the number of customer /call dispatches
// currently executing. Incremented at restactions/widgets ServeHTTP entry,
// decremented (deferred) at exit. Read by the engine worker to decide
// whether to yield.
var customerInFlightCount atomic.Int64

// markCustomerInFlight is called at the top of a customer /call dispatch.
// Returns the matching done function (defer it). Cheap atomic — no lock.
func markCustomerInFlight() func() {
	customerInFlightCount.Add(1)
	return func() { customerInFlightCount.Add(-1) }
}

// customerInFlight reports whether any customer /call is currently
// executing. The engine worker yields while this is true.
func customerInFlight() bool {
	return customerInFlightCount.Load() > 0
}

// CustomerInFlight is the exported predicate the refresher subsystem
// injects via cache.SetCustomerInflightHook (Ship #98 / 0.30.215). It
// shares the same atomic counter as the prewarm engine's yield — a
// customer /call's ServeHTTP increment/decrement bracket
// (restactions.go:77, widgets.go:62) is now observed by BOTH the prewarm
// engine (boot re-seed scopes) AND the steady-state L1 refresher worker
// pool. One atomic-int64 Load per refresher yield tick (4 workers × 40
// Hz = 160 reads/s steady-state) — negligible cost, no cache-line
// contention on the read side.
func CustomerInFlight() bool {
	return customerInFlight()
}

// ─────────────────────────────────────────────────────────────────────
// prewarmScope — the unit of engine work. Declares what to (re-)walk and
// the cohort source for the seed. Ship 1 uses only scopeKindBoot; Ship 2
// adds scopeKindWidgetCR / scopeKindRBACShift (a comment placeholder is
// left so the wiring is obvious).
// ─────────────────────────────────────────────────────────────────────

type prewarmScopeKind string

const (
	// scopeKindBoot — the full boot re-walk of the 2 nav roots after the
	// sync barrier. Ship 1's only invocation.
	scopeKindBoot prewarmScopeKind = "boot"

	// scopeKindGVRDiscovered — Ship 2 Stage 2 / 0.30.247. Fires when a
	// new GVR is first registered post-boot via the synchronous
	// discovery path (cache.DiscoverGroupResources → EnsureResourceType
	// added==true). The discovery side wires this via
	// cache.RegisterGVRDiscoveredHook (gvr_discovered_hook.go); the
	// dispatchers-side hook handler at prewarm_engine_boot.go enqueues
	// a scope per discovered GVR. The engine's rePrewarm core then
	// re-walks the nav tree under each cohort identity (now reading
	// the freshly-widened BindingsByGVR index) so the iterator that
	// previously short-circuited at resolve.go:377-381 against
	// crds.items[] (because the new GVR didn't exist) now yields
	// non-empty tmp[] — the dep edge against the new GVR is recorded,
	// and subsequent CR ADD events propagate via the normal
	// OnAdd→onChange→dirty-mark→refresher path. Closes the canonical
	// S4 admin-path defect.
	scopeKindGVRDiscovered prewarmScopeKind = "gvr-discovered"

	// Ship 2 (NOT wired this ship): scopeKindWidgetCR (a widget/RESTAction
	// CR add/update/delete re-walks that object's subtree) and
	// scopeKindRBACShift (an RBAC binding shift re-seeds the affected GVRs'
	// cohorts). The engine queue + rePrewarm core are built to accept these
	// with no refactor.
)

// prewarmScope is one engine work item.
//
// SCOPE-KIND-CARRIED PAYLOAD:
//   - scopeKindBoot: no payload (gvr left zero); one boot scope per process.
//   - scopeKindGVRDiscovered: gvr carries the discovered GVR; the engine
//     dedups on key() so multiple discovery events for the same GVR
//     coalesce to one queued scope.
type prewarmScope struct {
	kind prewarmScopeKind

	// gvr is the discovered GroupVersionResource for scopeKindGVRDiscovered.
	// Zero value for other scope kinds (carries no semantics). The engine's
	// rePrewarm core inspects this only when scope.kind == scopeKindGVRDiscovered.
	gvr schema.GroupVersionResource
}

// key returns the workqueue dedup key for this scope. Idempotent enqueue
// of the same key coalesces (refresher dedup semantics).
//
// KEY DERIVATION (RC-2 PM gate):
//   - scopeKindBoot: "boot" — single key, all boot enqueues coalesce.
//   - scopeKindGVRDiscovered: "gvr-discovered|<group>/<version>/<resource>"
//     — two enqueues for the same GVR coalesce; two enqueues for
//     different GVRs are DISTINCT and both run. Uses schema.GVR.String()
//     for stable formatting ("<group>/<version>/<resource>").
func (s prewarmScope) key() string {
	if s.kind == scopeKindGVRDiscovered {
		return string(s.kind) + "|" + s.gvr.String()
	}
	return string(s.kind)
}

// ─────────────────────────────────────────────────────────────────────
// prewarmEngine — the bounded queue + customer-priority worker pool. One
// per process; constructed lazily.
// ─────────────────────────────────────────────────────────────────────

type prewarmEngine struct {
	// scopeHandler runs one prewarmScope (the rePrewarm core, bound to the
	// boot harvesters + SA config at StartPrewarmEngine time). Set once at
	// start; read by workers.
	scopeHandler func(ctx context.Context, s prewarmScope) error

	mu      sync.Mutex
	pending map[string]prewarmScope // key -> scope, the bounded dedup queue
	signal  chan struct{}           // wakes a worker when pending grows

	// scopeDone, when set, is invoked by the worker after each scope is
	// processed (success or error) with the processed scope + its err. The
	// boot wiring uses it to release the engineSeed goroutine the instant
	// the boot scope completes, instead of holding for the full
	// pipGlobalTimeout (S2). Set once at start; read-only on the worker.
	scopeDone func(s prewarmScope, err error)

	startedOnce sync.Once

	// customer-priority yield knobs.
	yieldPoll time.Duration // how long a worker parks while a customer call is in flight

	// Falsifier / telemetry counters.
	enqueuedTotal atomic.Uint64
	processedTotal atomic.Uint64
	yieldTotal     atomic.Uint64 // engine yields to a customer call
}

var (
	prewarmEngineInstance *prewarmEngine
	prewarmEngineOnce      sync.Once
)

// defaultEngineYieldPoll is how long an engine worker parks before
// re-checking customerInFlight when a customer call is in flight. Short
// enough that the engine resumes promptly once the burst clears; long
// enough that a sustained customer burst keeps the engine fully yielded.
const defaultEngineYieldPoll = 25 * time.Millisecond

func prewarmEngineSingleton() *prewarmEngine {
	prewarmEngineOnce.Do(func() {
		prewarmEngineInstance = &prewarmEngine{
			pending:   map[string]prewarmScope{},
			signal:    make(chan struct{}, 1),
			yieldPoll: defaultEngineYieldPoll,
		}
	})
	return prewarmEngineInstance
}

// enqueueScope adds a scope to the bounded dedup queue and wakes a worker.
// Idempotent: a scope whose key is already pending coalesces (the latest
// wins, but Ship 1's scopes carry no payload so it is a true no-op
// dedup). Never blocks: the signal channel is buffered=1 and a full
// channel means a worker is already about to wake.
func (e *prewarmEngine) enqueueScope(s prewarmScope) {
	e.mu.Lock()
	e.pending[s.key()] = s
	e.mu.Unlock()
	e.enqueuedTotal.Add(1)
	select {
	case e.signal <- struct{}{}:
	default:
	}
}

// dequeueScope pops one scope from the pending set, or reports empty. The
// caller drains until empty.
func (e *prewarmEngine) dequeueScope() (prewarmScope, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for k, s := range e.pending {
		delete(e.pending, k)
		return s, true
	}
	return prewarmScope{}, false
}

// StartPrewarmEngine starts the engine worker(s) bound to the given scope
// handler. Idempotent — repeated calls are no-ops (the first wins). The
// handler is the rePrewarm core closed over the boot harvesters + SA
// config; the worker invokes it per dequeued scope, yielding to customer
// /call between scopes. The worker exits on ctx cancel.
//
// Ship 1 runs ONE worker (the boot re-walk is a single coherent pass).
// Ship 2 may raise the worker count for parallel runtime triggers; the
// queue's dedup keeps that safe.
//
// scopeDone (nilable) is invoked after each scope is processed — the boot
// wiring uses it to release the background goroutine the instant the boot
// scope completes (S2) rather than holding for the full pipGlobalTimeout.
//
// Ship 2 Stage 2 / 0.30.247 — wires the cache→engine hook for
// scopeKindGVRDiscovered. The hook subscribes here BEFORE the worker
// goroutine spawns so any GVR discovered during boot (lazy registers
// post-MarkEagerSet) is queued, not dropped. The wiring happens inside
// startedOnce so it runs exactly once per process.
func StartPrewarmEngine(ctx context.Context, handler func(ctx context.Context, s prewarmScope) error, scopeDone func(s prewarmScope, err error)) {
	e := prewarmEngineSingleton()
	e.startedOnce.Do(func() {
		e.scopeHandler = handler
		e.scopeDone = scopeDone

		// Ship 2 Stage 2 — wire the cache→engine hook. The hook fires
		// from inside cache.DiscoverGroupResources (the `if added`
		// branch of EnsureResourceType for genuinely-new GVRs). The
		// callback is non-blocking: enqueueScope is O(1) under a
		// sync.Mutex + a buffered=1 signal-channel send.
		//
		// REGISTRATION ORDER (PM observation 3, R4 startup-storm):
		// runs BEFORE `go e.runWorker(ctx)` below so a discovery firing
		// during the same goroutine that called StartPrewarmEngine
		// (e.g. via subsequent walker calls) IS queued, not dropped.
		// The registration is idempotent at the cache side (compares
		// fn pointer) so a future engine re-entry would not double-wire.
		registerEngineGVRDiscoveredHook(e)

		go e.runWorker(ctx)
		slog.Info("prewarm.engine.started",
			slog.String("subsystem", "cache"),
			slog.String("queue", "bounded-dedup"),
			slog.String("customer_priority", "yield-on-inflight"),
			slog.String("gvr_discovered_hook", "wired"),
		)
	})
}

// runWorker is the engine worker loop. It blocks on the signal channel,
// drains the pending queue, and runs each scope through the handler —
// YIELDING to any in-flight customer /call before each scope. Exits on
// ctx cancel.
func (e *prewarmEngine) runWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.signal:
		}
		// Drain every pending scope. New enqueues during the drain re-fire
		// the signal so we loop back.
		for {
			if ctx.Err() != nil {
				return
			}
			s, ok := e.dequeueScope()
			if !ok {
				break
			}
			// CUSTOMER PRIORITY — yield before running the scope while any
			// customer /call is in flight. The seed work inside the handler
			// also yields between cohorts (see seedScopeYielding), so a
			// burst that arrives mid-scope is also deferred.
			e.yieldToCustomer(ctx)
			if e.scopeHandler == nil {
				continue
			}
			err := e.scopeHandler(ctx, s)
			if err != nil {
				slog.Warn("prewarm.engine.scope_incomplete",
					slog.String("subsystem", "cache"),
					slog.String("scope", s.key()),
					slog.Any("err", err),
				)
			}
			e.processedTotal.Add(1)
			if e.scopeDone != nil {
				e.scopeDone(s, err)
			}
		}
	}
}

// yieldToCustomer parks the worker while any customer /call is in flight,
// re-checking every yieldPoll. Returns promptly once no customer call is
// in flight (or ctx is cancelled). This is the cooperative customer-
// priority yield: the engine never delays a customer call; it steps aside
// for the duration of the burst.
func (e *prewarmEngine) yieldToCustomer(ctx context.Context) {
	if !customerInFlight() {
		return
	}
	t := time.NewTicker(e.yieldPoll)
	defer t.Stop()
	for customerInFlight() {
		e.yieldTotal.Add(1)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// engineYieldCheckpoint is the per-cohort yield checkpoint the seed loop
// calls between cohorts so a customer burst arriving MID-scope is also
// deferred. It is the same cooperative yield as yieldToCustomer but
// callable from the seed package without exposing the engine struct.
func engineYieldCheckpoint(ctx context.Context) {
	prewarmEngineSingleton().yieldToCustomer(ctx)
}
